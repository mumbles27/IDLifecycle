<#
.SYNOPSIS
    Disables AD user accounts for IDLifecycle (optionally strips groups, stamps a description, moves to a disabled OU).

.DESCRIPTION
    Reads ONE JSON request from stdin and writes ONE JSON response to stdout.

    Request:
      {"server":"", "idAttribute":"employeeID", "items":[
         {"changeId":"c1","externalId":"E1001","objectGuid":"<guid>","moveTo":"OU=Disabled,DC=...",
          "removeGroups":["GG-AllStaff"],"addGroups":["GG-Disabled"],"description":"Disabled by IDLifecycle 2026-10-16"} ]}

    Per item the script finds the account by objectGuid (all items in one bulk query), VERIFIES that its
    idAttribute equals externalId (otherwise the item fails and nothing is touched), then:
      1. disables the account (if it is enabled) - ALWAYS first, so nothing below can leave it enabled,
      2. validates moveTo (shape and existence; a bad one fails the item as "failed after: disabled; ..."),
      3. sets the description (if given and different),
      4. removes it from the listed groups it is a member of (sAMAccountName or DN; unknown groups are warned and ignored;
         privileged and ambiguous groups are KEPT with a WARN log, a 'Skipped' report row and a
         "kept privileged group(s): ..." note in the message - never a failure),
      5. adds it to the addGroups it is not yet a member of (e.g. a "Disabled Users" group). Never a failure either:
         a privileged group (adminCount=1 or a protected RID) is refused, an ambiguous or unknown one ignored - each with a
         WARN log, a 'Skipped' report row and a "not added: ..." note in the message,
      6. moves it to moveTo (if given and it is not already there).
    Optional "set" / "clear" (Set-IDLUser semantics, LDAP names) are applied after step 3. Before step 1 the item fails
    with nothing changed when "expect" does not match ("drift: ...", see Set-IDLUser; "force": true skips it) or when
    set/clear is invalid. Applied and would results carry "before": {enabled, dn, attributes (incl. description when it
    changed), groupsAdded, groupsRemoved}.
    It is idempotent: an already-disabled account still gets steps 2-6; if nothing needed doing the status is "skipped".
    One failing item never stops the others.

    Response: {"ok":true,"dryRun":false,"results":[{"changeId","externalId","status":"applied|would|skipped|failed",
    "message","objectGuid","dn","sam","upn","error","before"}],"summary":{"total","applied","would","skipped","failed"}}

.PARAMETER DryRun
    Do every lookup and log/report everything as WOULD: statements, but make NO changes. Successful items
    come back with status "would".

.EXAMPLE
    '{"idAttribute":"employeeID","items":[{"changeId":"c1","externalId":"E1001","objectGuid":"11111111-1111-1111-1111-111111111111"}]}' |
        pwsh -NoProfile -File .\Disable-IDLUser.ps1 -DryRun

.EXAMPLE
    Get-Content .\disable-request.json -Raw | pwsh -NoProfile -File .\Disable-IDLUser.ps1 | ConvertFrom-Json

.NOTES
    Requires PowerShell 7+, the ActiveDirectory module, and delegated rights to disable/move users and change
    group membership. Exit codes: 0 ok, 1 some items failed, 2 invalid input, 3 module missing, 4 cannot reach AD,
    5 unexpected error. Logs and the CSV report (Disable-IDLUser_Report_<ts>.csv) are written to $PSScriptRoot\Logs.
#>

#Requires -Version 7.0

[CmdletBinding()]
param(
    [switch]$DryRun
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

Import-Module (Join-Path $PSScriptRoot 'IDLifecycle.Common.psm1') -Force

$exitCode = 5
$response = $null

try{
    Start-IDLRun -ScriptName 'Disable-IDLUser' -ScriptRoot $PSScriptRoot -IsAction -DryRun:$DryRun -Parameters @{ DryRun = [bool]$DryRun }

    # ---- Phase: read and validate the request ----
    $request = Read-IDLRequest
    $common = Get-IDLRequestCommon -Request $request -RequireItems
    $server = $common.Server
    $idAttribute = $common.IdAttribute
    $items = $common.Items
    Add-IDLCounter -Name 'Total' -By $items.Count
    Write-IDLLog ("Request: server='{0}' idAttribute={1} items={2}" -f $server, $idAttribute, $items.Count)

    $blocked = @(Get-IDLProtectedAttributes) + @('cn', 'name', $idAttribute.ToLowerInvariant())

    # ---- Phase: dependencies and connection ----
    try{
        Import-Module ActiveDirectory -Global -ErrorAction Stop
        if (-not (Get-Module -Name ActiveDirectory)){
            throw 'ActiveDirectory module did not load.'
        }
    }
    catch{
        throw (New-IDLException -Kind Dependency -Message "ActiveDirectory module could not be loaded: $($_.Exception.Message)" -InnerException $_.Exception)
    }

    Write-IDLLog "ActiveDirectory module loaded (version $((Get-Module ActiveDirectory).Version))"
    Test-IDLAdConnection -Server $server
    $serverParams = Get-IDLServerParams -Server $server

    # ---- Phase: bulk resolution ----
    $phase = Start-IDLPhase -Name 'Resolve accounts and groups'
    $needDescription = $false
    $needMemberOf = $false
    $allGroups = [System.Collections.Generic.List[string]]::new()
    $expectNeeds = Get-IDLExpectNeeds -Items $items
    $allGroups.AddRange($expectNeeds.Groups)
    $extraProperties = @($expectNeeds.Properties)
    foreach ($item in $items){
        $extraProperties += Get-IDLAttributeRequestNames -Item $item

        if (-not [string]::IsNullOrWhiteSpace((ConvertTo-IDLString -Value (Get-IDLField -Object $item -Name 'description')))){
            $needDescription = $true
        }

        $groups = Get-IDLStringList -Value (Get-IDLField -Object $item -Name 'removeGroups')
        if ($groups.Count -gt 0){
            $needMemberOf = $true
            $allGroups.AddRange($groups)
        }

        $groups = Get-IDLStringList -Value (Get-IDLField -Object $item -Name 'addGroups')
        if ($groups.Count -gt 0){
            $needMemberOf = $true
            $allGroups.AddRange($groups)
        }
    }

    if ($needDescription){
        $extraProperties += 'description'
    }

    if ($needMemberOf){
        $extraProperties += 'memberOf'
    }

    $targets = Resolve-IDLTargetUsers -Items $items -IdAttribute $idAttribute -Server $server -ExtraProperties $extraProperties
    $groupMap = Resolve-IDLGroups -Identities $allGroups.ToArray() -Server $server
    Stop-IDLPhase -Phase $phase

    # ---- Phase: process items ----
    $phase = Start-IDLPhase -Name 'Process items'
    $results = [System.Collections.Generic.List[object]]::new()

    for ($i = 0; $i -lt $items.Count; $i++){
        $item = $items[$i]
        $changeId = ConvertTo-IDLString -Value (Get-IDLField -Object $item -Name 'changeId')
        $externalId = ConvertTo-IDLString -Value (Get-IDLField -Object $item -Name 'externalId')
        $target = $targets[$i]
        $user = $target.User
        $done = [System.Collections.Generic.List[string]]::new()
        $result = $null

        try{
            if ($target.Status -ne 'Found'){
                $userFields = Get-IDLUserFields -User $user
                $result = New-IDLResult -ChangeId $changeId -ExternalId $externalId -Status failed -Message $target.Message -ErrorText $target.Message @userFields
                Add-IDLReportRow -Action 'DisableUser' -Result 'Failed' -Identity $externalId -Details $target.Status -ErrorText $target.Message
                $results.Add($result)
                Register-IDLResult -Result $result
                continue
            }

            $identity = [string]$user.SamAccountName
            $userDn = [string]$user.DistinguishedName
            Write-IDLLog "Processing $changeId ($externalId) -> $userDn"

            $description = ConvertTo-IDLString -Value (Get-IDLField -Object $item -Name 'description')
            $moveTo = ConvertTo-IDLString -Value (Get-IDLField -Object $item -Name 'moveTo')
            $moveTo = (($null -eq $moveTo) ? ('') : ($moveTo.Trim()))
            # Before any change: the account must still be as the request expects (undo), and set/clear must be
            # valid - these are the only checks that can stop the disable.
            Assert-IDLExpect -Item $item -User $user -GroupMap $groupMap -IdAttribute $idAttribute
            $attributeRequest = Get-IDLAttributeRequest -Item $item -Blocked $blocked -ScriptName 'Disable-IDLUser'
            if (
                (-not [string]::IsNullOrWhiteSpace($description)) -and
                (@(@($attributeRequest.Desired.Keys) + @($attributeRequest.Clear) | Where-Object { $_ -ieq 'description' }).Count -gt 0)
            ){
                throw "'description' is given both as a field and in set/clear"
            }

            $before = New-IDLBefore -User $user
            $changes = 0
            $guid = [guid]$user.ObjectGUID

            # Step 1: disable FIRST.
            # Nothing else in the request (bad moveTo, privileged or ambiguous groups) may
            # leave a terminated account enabled; later failures end the item as "failed after: disabled; ...".
            if ([bool]$user.Enabled){
                $changes++
                if ($DryRun){
                    Write-IDLLog "  WOULD: disable $identity"
                    Add-IDLReportRow -Action 'DisableAccount' -Result 'Would' -Identity $identity -ObjectIdOrDN $userDn
                    $done.Add('would disable')
                }
                else{
                    Disable-ADAccount -Identity $guid @serverParams
                    Write-IDLLog "  Disabled $identity"
                    Add-IDLReportRow -Action 'DisableAccount' -Result 'Applied' -Identity $identity -ObjectIdOrDN $userDn
                    $done.Add('disabled')
                }
            }
            else{
                Write-IDLLog "  $identity is already disabled"
            }

            # Validate the destination before the remaining changes.
            if ($moveTo -ne ''){
                if (-not (Test-IDLDnShape -Value $moveTo)){
                    throw "moveTo is not a distinguished name: $moveTo"
                }

                if (-not (Test-IDLContainerExists -Dn $moveTo -Server $server)){
                    throw "Target OU does not exist: $moveTo"
                }
            }

            # Privileged and ambiguous groups are kept (never removed, never a failure): the disable must not be blocked.
            $removeInfo = Get-IDLGroupDns -Identities (Get-IDLStringList -Value (Get-IDLField -Object $item -Name 'removeGroups')) -GroupMap $groupMap -SkipPrivileged
            foreach ($missingGroup in $removeInfo.Missing){
                Write-IDLLog "  Group '$missingGroup' not found; nothing to remove" 'WARN'
            }

            foreach ($skip in $removeInfo.Skipped){
                $why = (($skip.Kind -eq 'privileged') ? ("privileged group ($($skip.Reason)); membership kept") : ("ambiguous group ($($skip.Reason)); membership kept"))
                Write-IDLLog "  Group '$($skip.Identity)' is $why" 'WARN'
                Add-IDLReportRow -Action 'RemoveGroupMember' -Result 'Skipped' -Identity $identity -ObjectIdOrDN $userDn -Details "$($skip.Identity): $why"
            }

            $removeDns = $removeInfo.Dns

            if (
                (-not [string]::IsNullOrWhiteSpace($description)) -and
                ((Get-IDLAdValue -Object $user -Name 'description') -cne $description)
            ){
                $changes++
                if ($DryRun){
                    $before.attributes['description'] = Get-IDLAdValue -Object $user -Name 'description'
                    Write-IDLLog "  WOULD: set description on $identity"
                    Add-IDLReportRow -Action 'SetDescription' -Result 'Would' -Identity $identity -ObjectIdOrDN $userDn -Details $description
                    $done.Add('would set description')
                }
                else{
                    Set-ADUser -Identity $guid -Replace @{ description = $description } @serverParams
                    $before.attributes['description'] = Get-IDLAdValue -Object $user -Name 'description'
                    Write-IDLLog "  Set description on $identity"
                    Add-IDLReportRow -Action 'SetDescription' -Result 'Applied' -Identity $identity -ObjectIdOrDN $userDn -Details $description
                    $done.Add('set description')
                }
            }

            $changes += Update-IDLUserAttributes -User $user -Desired $attributeRequest.Desired -ClearNames $attributeRequest.Clear -Server $server -DryRun:$DryRun -Done $done -Before $before.attributes

            if ($removeDns.Count -gt 0){
                $changes += Update-IDLGroupMembership -User $user -RemoveDns $removeDns -Server $server -DryRun:$DryRun -Done $done -Removed $before.groupsRemoved
            }

            # Groups a disabled account joins (e.g. "Disabled Users"). Like the removals this never blocks the disable:
            # privileged groups are refused, ambiguous and unknown ones ignored - all reported.
            $notAdded = [System.Collections.Generic.List[string]]::new()
            $addInfo = Get-IDLGroupDns -Identities (Get-IDLStringList -Value (Get-IDLField -Object $item -Name 'addGroups')) -GroupMap $groupMap -SkipPrivileged
            foreach ($missingGroup in $addInfo.Missing){
                Write-IDLLog "  Group '$missingGroup' not found; $identity is not added to it" 'WARN'
                Add-IDLReportRow -Action 'AddGroupMember' -Result 'Skipped' -Identity $identity -ObjectIdOrDN $userDn -Details "$($missingGroup): not found"
                $notAdded.Add("$missingGroup (not found)")
            }

            foreach ($skip in $addInfo.Skipped){
                $why = (($skip.Kind -eq 'privileged') ? ("a protected administrative group ($($skip.Reason)); refusing to add the account") : ("ambiguous ($($skip.Reason)); not added"))
                Write-IDLLog "  Group '$($skip.Identity)' is $why" 'WARN'
                Add-IDLReportRow -Action 'AddGroupMember' -Result 'Skipped' -Identity $identity -ObjectIdOrDN $userDn -Details "$($skip.Identity): $why"
                $notAdded.Add("$($skip.Identity) ($($skip.Kind))")
            }

            if ($addInfo.Dns.Count -gt 0){
                $changes += Update-IDLGroupMembership -User $user -AddDns $addInfo.Dns -Server $server -DryRun:$DryRun -Done $done -Added $before.groupsAdded
            }

            $finalDn = $userDn
            if ($moveTo -ne ''){
                $newDn = Move-IDLUser -User $user -TargetOu $moveTo -Server $server -DryRun:$DryRun -Done $done
                if ($null -ne $newDn){
                    $changes++
                    $finalDn = $newDn
                }
            }

            # Tell the operator about memberships that were deliberately left in place.
            $notes = [System.Collections.Generic.List[string]]::new()
            $keptPrivileged = @($removeInfo.Skipped | Where-Object { $_.Kind -eq 'privileged' } | ForEach-Object { $_.Identity })
            $keptAmbiguous = @($removeInfo.Skipped | Where-Object { $_.Kind -eq 'ambiguous' } | ForEach-Object { $_.Identity })
            if ($keptPrivileged.Count -gt 0){
                $notes.Add('kept privileged group(s): ' + ($keptPrivileged -join ', '))
            }

            if ($keptAmbiguous.Count -gt 0){
                $notes.Add('kept ambiguous group(s): ' + ($keptAmbiguous -join ', '))
            }

            if ($notAdded.Count -gt 0){
                $notes.Add('not added: ' + ($notAdded -join ', '))
            }

            $userFields = Get-IDLUserFields -User $user -DnOverride $finalDn
            if ($changes -eq 0){
                $message = (@('nothing to do (already disabled)') + $notes) -join '; '
                $result = New-IDLResult -ChangeId $changeId -ExternalId $externalId -Status skipped -Message $message @userFields
                Add-IDLReportRow -Action 'DisableUser' -Result 'Skipped' -Identity $identity -ObjectIdOrDN $userDn -Details 'already in desired state'
            }
            elseif ($DryRun){
                $result = New-IDLResult -ChangeId $changeId -ExternalId $externalId -Status would -Message ((@($done) + $notes) -join '; ') -Before $before @userFields
            }
            else{
                $result = New-IDLResult -ChangeId $changeId -ExternalId $externalId -Status applied -Message ((@($done) + $notes) -join '; ') -Before $before @userFields
            }
        }
        catch{
            $errorText = Protect-IDLText -Text $_.Exception.Message
            Write-IDLLog "  Item $changeId failed: $errorText" 'ERROR'
            Write-IDLLog "  Stack: $($_.ScriptStackTrace)" 'DEBUG'
            $userFields = Get-IDLUserFields -User $user
            $message = (($done.Count -gt 0) ? ('failed after: ' + ($done -join '; ')) : ('failed'))
            $result = New-IDLResult -ChangeId $changeId -ExternalId $externalId -Status failed -Message $message -ErrorText $errorText @userFields
            Add-IDLReportRow -Action 'DisableUser' -Result 'Failed' -Identity $externalId -ObjectIdOrDN ([string]$userFields['Dn']) -Details $message -ErrorText $errorText
        }

        $results.Add($result)
        Register-IDLResult -Result $result
    }

    Stop-IDLPhase -Phase $phase

    $response = New-IDLActionResponse -Results $results.ToArray() -DryRun:$DryRun
    $exitCode = (($response.summary.failed -gt 0) ? (1) : (0))
}
catch{
    $exitCode = Get-IDLExitCode -Exception $_.Exception
    $message = Protect-IDLText -Text $_.Exception.Message
    Write-IDLLog "FATAL: $message" 'ERROR'
    Write-IDLLog "Stack: $($_.ScriptStackTrace)" 'DEBUG'
    $response = New-IDLErrorResponse -Message $message
}
finally{
    Stop-IDLRun -ExitCode $exitCode
}

Write-IDLResponse -Response $response
exit $exitCode
