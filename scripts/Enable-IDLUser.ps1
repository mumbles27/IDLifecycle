<#
.SYNOPSIS
    Enables AD user accounts for IDLifecycle (optionally adds groups and moves to another OU).

.DESCRIPTION
    Reads ONE JSON request from stdin and writes ONE JSON response to stdout.

    Request:
      {"server":"", "idAttribute":"employeeID", "items":[
         {"changeId":"c1","externalId":"E1001","objectGuid":"<guid>","moveTo":"OU=Staff,DC=...","addGroups":["GG-AllStaff"],
          "removeGroups":["GG-Disabled"]} ]}

    Per item the script finds the account by objectGuid (all items in one bulk query), VERIFIES that its
    idAttribute equals externalId (otherwise the item fails and nothing is touched), validates that every
    addGroups entry (sAMAccountName or DN) and the moveTo OU exist (otherwise the item fails BEFORE any change),
    then enables the account if needed, adds the missing group memberships, removes it from the removeGroups it is a
    member of (e.g. the "Disabled Users" group it joined when it was disabled) and moves it if needed. Adding to a
    privileged group (adminCount=1 or a protected RID) or an ambiguous one fails the item before any change; a
    removeGroups entry that is privileged or ambiguous is KEPT (WARN log, 'Skipped' report row, "kept ... group(s)" note)
    and an unknown one is ignored with a WARN - never a failure.
    Optional "set" / "clear" (Set-IDLUser semantics) are applied before the account is enabled. "expect" / "force" and the
    "before" record work as in Set-IDLUser (before: enabled, dn, attributes, groupsAdded, groupsRemoved).
    Idempotent: nothing to do => status "skipped". One failing item never stops the others.

    Response: {"ok":true,"dryRun":false,"results":[{"changeId","externalId","status":"applied|would|skipped|failed",
    "message","objectGuid","dn","sam","upn","error","before"}],"summary":{"total","applied","would","skipped","failed"}}

.PARAMETER DryRun
    Do every lookup and log/report everything as WOULD: statements, but make NO changes. Successful items
    come back with status "would".

.EXAMPLE
    '{"idAttribute":"employeeID","items":[{"changeId":"c1","externalId":"E1001","objectGuid":"11111111-1111-1111-1111-111111111111"}]}' |
        pwsh -NoProfile -File .\Enable-IDLUser.ps1 -DryRun

.EXAMPLE
    Get-Content .\enable-request.json -Raw | pwsh -NoProfile -File .\Enable-IDLUser.ps1 | ConvertFrom-Json

.NOTES
    Requires PowerShell 7+, the ActiveDirectory module, and delegated rights to enable/move users and change
    group membership. Exit codes: 0 ok, 1 some items failed, 2 invalid input, 3 module missing, 4 cannot reach AD,
    5 unexpected error. Logs and the CSV report (Enable-IDLUser_Report_<ts>.csv) are written to $PSScriptRoot\Logs.
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
    Start-IDLRun -ScriptName 'Enable-IDLUser' -ScriptRoot $PSScriptRoot -IsAction -DryRun:$DryRun -Parameters @{ DryRun = [bool]$DryRun }

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
    $needMemberOf = $false
    $allGroups = [System.Collections.Generic.List[string]]::new()
    $expectNeeds = Get-IDLExpectNeeds -Items $items
    $allGroups.AddRange($expectNeeds.Groups)
    $extraProperties = @($expectNeeds.Properties)
    foreach ($item in $items){
        $extraProperties += Get-IDLAttributeRequestNames -Item $item

        $groups = Get-IDLStringList -Value (Get-IDLField -Object $item -Name 'addGroups')
        if ($groups.Count -gt 0){
            $needMemberOf = $true
            $allGroups.AddRange($groups)
        }

        $groups = Get-IDLStringList -Value (Get-IDLField -Object $item -Name 'removeGroups')
        if ($groups.Count -gt 0){
            $needMemberOf = $true
            $allGroups.AddRange($groups)
        }
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
                Add-IDLReportRow -Action 'EnableUser' -Result 'Failed' -Identity $externalId -Details $target.Status -ErrorText $target.Message
                $results.Add($result)
                Register-IDLResult -Result $result
                continue
            }

            $identity = [string]$user.SamAccountName
            $userDn = [string]$user.DistinguishedName
            Write-IDLLog "Processing $changeId ($externalId) -> $userDn"

            $moveTo = ConvertTo-IDLString -Value (Get-IDLField -Object $item -Name 'moveTo')
            $moveTo = (($null -eq $moveTo) ? ('') : ($moveTo.Trim()))

            # Validate everything up front so a bad group/OU fails the item before any change is made, and refuse when
            # the account is no longer as the request expects (undo).
            Assert-IDLExpect -Item $item -User $user -GroupMap $groupMap -IdAttribute $idAttribute
            $attributeRequest = Get-IDLAttributeRequest -Item $item -Blocked $blocked -ScriptName 'Enable-IDLUser'

            # Throws (item fails, nothing changed) for ambiguous or privileged groups.
            $addInfo = Get-IDLGroupDns -Identities (Get-IDLStringList -Value (Get-IDLField -Object $item -Name 'addGroups')) -GroupMap $groupMap -Adding
            if ($addInfo.Missing.Count -gt 0){
                throw "Group(s) not found: $($addInfo.Missing -join ', ')"
            }

            $addDns = $addInfo.Dns

            # Removals never block the enable: privileged or ambiguous groups are kept, unknown ones ignored.
            $removeInfo = Get-IDLGroupDns -Identities (Get-IDLStringList -Value (Get-IDLField -Object $item -Name 'removeGroups')) -GroupMap $groupMap -SkipPrivileged

            if ($moveTo -ne ''){
                if (-not (Test-IDLDnShape -Value $moveTo)){
                    throw "moveTo is not a distinguished name: $moveTo"
                }

                if (-not (Test-IDLContainerExists -Dn $moveTo -Server $server)){
                    throw "Target OU does not exist: $moveTo"
                }
            }

            $before = New-IDLBefore -User $user
            $guid = [guid]$user.ObjectGUID

            # Attributes first: the account is enabled with its values already restored.
            $changes = Update-IDLUserAttributes -User $user -Desired $attributeRequest.Desired -ClearNames $attributeRequest.Clear -Server $server -DryRun:$DryRun -Done $done -Before $before.attributes

            if (-not [bool]$user.Enabled){
                $changes++
                if ($DryRun){
                    Write-IDLLog "  WOULD: enable $identity"
                    Add-IDLReportRow -Action 'EnableAccount' -Result 'Would' -Identity $identity -ObjectIdOrDN $userDn
                    $done.Add('would enable')
                }
                else{
                    Enable-ADAccount -Identity $guid @serverParams
                    Write-IDLLog "  Enabled $identity"
                    Add-IDLReportRow -Action 'EnableAccount' -Result 'Applied' -Identity $identity -ObjectIdOrDN $userDn
                    $done.Add('enabled')
                }
            }
            else{
                Write-IDLLog "  $identity is already enabled"
            }

            if ($addDns.Count -gt 0){
                $changes += Update-IDLGroupMembership -User $user -AddDns $addDns -Server $server -DryRun:$DryRun -Done $done -Added $before.groupsAdded
            }

            foreach ($missingGroup in $removeInfo.Missing){
                Write-IDLLog "  Group '$missingGroup' not found; nothing to remove" 'WARN'
            }

            $notes = [System.Collections.Generic.List[string]]::new()
            foreach ($skip in $removeInfo.Skipped){
                $why = (($skip.Kind -eq 'privileged') ? ("privileged group ($($skip.Reason)); membership kept") : ("ambiguous group ($($skip.Reason)); membership kept"))
                Write-IDLLog "  Group '$($skip.Identity)' is $why" 'WARN'
                Add-IDLReportRow -Action 'RemoveGroupMember' -Result 'Skipped' -Identity $identity -ObjectIdOrDN $userDn -Details "$($skip.Identity): $why"
                $notes.Add("kept $($skip.Kind) group $($skip.Identity)")
            }

            if ($removeInfo.Dns.Count -gt 0){
                $changes += Update-IDLGroupMembership -User $user -RemoveDns $removeInfo.Dns -Server $server -DryRun:$DryRun -Done $done -Removed $before.groupsRemoved
            }

            $finalDn = $userDn
            if ($moveTo -ne ''){
                $newDn = Move-IDLUser -User $user -TargetOu $moveTo -Server $server -DryRun:$DryRun -Done $done
                if ($null -ne $newDn){
                    $changes++
                    $finalDn = $newDn
                }
            }

            $userFields = Get-IDLUserFields -User $user -DnOverride $finalDn
            if ($changes -eq 0){
                $result = New-IDLResult -ChangeId $changeId -ExternalId $externalId -Status skipped -Message ((@('nothing to do (already enabled)') + $notes) -join '; ') @userFields
                Add-IDLReportRow -Action 'EnableUser' -Result 'Skipped' -Identity $identity -ObjectIdOrDN $userDn -Details 'already in desired state'
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
            Add-IDLReportRow -Action 'EnableUser' -Result 'Failed' -Identity $externalId -ObjectIdOrDN ([string]$userFields['Dn']) -Details $message -ErrorText $errorText
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
