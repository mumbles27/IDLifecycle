<#
.SYNOPSIS
    Updates attributes, group memberships and OU placement of existing AD users for IDLifecycle.

.DESCRIPTION
    Reads ONE JSON request from stdin and writes ONE JSON response to stdout.

    Request:
      {"server":"", "idAttribute":"employeeID", "items":[
         {"changeId":"c1","externalId":"E1001","objectGuid":"<guid>","set":{"title":"New"},"clear":["telephoneNumber"],
          "addGroups":["GG-AllStaff"],"removeGroups":["CN=Old,OU=Groups,DC=..."],"moveTo":"OU=Sales,DC=..."} ]}

    Per item the script finds the account by objectGuid (all items in one bulk query), VERIFIES that its
    idAttribute equals externalId (otherwise the item fails and nothing is touched), validates the request
    (attribute names, protected attributes, addGroups resolvable, moveTo OU exists) BEFORE any change, then:
      1. applies only the attribute values that actually differ, in ONE Set-ADUser call using LDAP names with
         -Replace (set) and -Clear (clear);
      2. adds / removes group memberships that are not already in place (groups may be sAMAccountName or DN);
      3. moves the account when moveTo is non-empty and it is not already there.
    Items where everything already matches are "skipped". One failing item never stops the others.
    The ID attribute, cn/name, memberOf, userAccountControl, password attributes etc. cannot be changed here.

    Undo support: an optional "expect" object ({enabled, dn, attributes:{name: value|null}, memberOf, notMemberOf}) is
    checked before any change; a mismatch fails the item with error "drift: <each difference>" and nothing is changed
    ("force": true skips the check, with a WARN). Applied and would results carry "before": {enabled, dn, attributes (the
    old value of every attribute the item changed, null = was empty), groupsAdded, groupsRemoved (DNs actually added /
    removed, not no-ops)} - what the engine needs to build the inverse.

    Response: {"ok":true,"dryRun":false,"results":[{"changeId","externalId","status":"applied|would|skipped|failed",
    "message","objectGuid","dn","sam","upn","error","before"}],"summary":{"total","applied","would","skipped","failed"}}

.PARAMETER DryRun
    Do every lookup and log/report everything as WOULD: statements, but make NO changes. Successful items
    come back with status "would".

.EXAMPLE
    '{"idAttribute":"employeeID","items":[{"changeId":"c1","externalId":"E1001","objectGuid":"11111111-1111-1111-1111-111111111111","set":{"title":"Analyst"}}]}' |
        pwsh -NoProfile -File .\Set-IDLUser.ps1 -DryRun

.EXAMPLE
    Get-Content .\set-request.json -Raw | pwsh -NoProfile -File .\Set-IDLUser.ps1 | ConvertFrom-Json

.NOTES
    Requires PowerShell 7+, the ActiveDirectory module, and delegated rights to write the attributes, change
    group membership and move users. Exit codes: 0 ok, 1 some items failed, 2 invalid input, 3 module missing,
    4 cannot reach AD, 5 unexpected error. Logs and the CSV report (Set-IDLUser_Report_<ts>.csv) are written to
    $PSScriptRoot\Logs. Attribute VALUES are not logged (only names) because they can contain personal data.
    An empty/null value in "set" is rejected - send the attribute name in "clear" instead.
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
    Start-IDLRun -ScriptName 'Set-IDLUser' -ScriptRoot $PSScriptRoot -IsAction -DryRun:$DryRun -Parameters @{ DryRun = [bool]$DryRun }

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

    # ---- Phase: bulk resolution (only the properties we need) ----
    $phase = Start-IDLPhase -Name 'Resolve accounts and groups'
    $extraProperties = [System.Collections.Generic.List[string]]::new()
    $allGroups = [System.Collections.Generic.List[string]]::new()
    $expectNeeds = Get-IDLExpectNeeds -Items $items
    $extraProperties.AddRange($expectNeeds.Properties)
    $allGroups.AddRange($expectNeeds.Groups)
    foreach ($item in $items){
        $extraProperties.AddRange((Get-IDLAttributeRequestNames -Item $item))

        $addGroups = Get-IDLStringList -Value (Get-IDLField -Object $item -Name 'addGroups')
        $removeGroups = Get-IDLStringList -Value (Get-IDLField -Object $item -Name 'removeGroups')
        if (($addGroups.Count + $removeGroups.Count) -gt 0){
            $extraProperties.Add('memberOf')
            $allGroups.AddRange($addGroups)
            $allGroups.AddRange($removeGroups)
        }
    }

    $targets = Resolve-IDLTargetUsers -Items $items -IdAttribute $idAttribute -Server $server -ExtraProperties ([string[]]@($extraProperties | Select-Object -Unique))
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
                Add-IDLReportRow -Action 'SetUser' -Result 'Failed' -Identity $externalId -Details $target.Status -ErrorText $target.Message
                $results.Add($result)
                Register-IDLResult -Result $result
                continue
            }

            $identity = [string]$user.SamAccountName
            $userDn = [string]$user.DistinguishedName
            Write-IDLLog "Processing $changeId ($externalId) -> $userDn"

            # ---- validate the item completely before any change ----
            $attributeRequest = Get-IDLAttributeRequest -Item $item -Blocked $blocked -ScriptName 'Set-IDLUser'

            # Throws (item fails, nothing changed) for ambiguous or privileged groups.
            $addInfo = Get-IDLGroupDns -Identities (Get-IDLStringList -Value (Get-IDLField -Object $item -Name 'addGroups')) -GroupMap $groupMap -Adding
            $removeInfo = Get-IDLGroupDns -Identities (Get-IDLStringList -Value (Get-IDLField -Object $item -Name 'removeGroups')) -GroupMap $groupMap
            if ($addInfo.Missing.Count -gt 0){
                throw "Group(s) not found: $($addInfo.Missing -join ', ')"
            }

            foreach ($missingGroup in $removeInfo.Missing){
                Write-IDLLog "  Group '$missingGroup' not found; nothing to remove" 'WARN'
            }

            $addDns = $addInfo.Dns
            $removeDns = $removeInfo.Dns

            $moveTo = ConvertTo-IDLString -Value (Get-IDLField -Object $item -Name 'moveTo')
            $moveTo = (($null -eq $moveTo) ? ('') : ($moveTo.Trim()))
            if ($moveTo -ne ''){
                if (-not (Test-IDLDnShape -Value $moveTo)){
                    throw "moveTo is not a distinguished name: $moveTo"
                }

                if (-not (Test-IDLContainerExists -Dn $moveTo -Server $server)){
                    throw "Target OU does not exist: $moveTo"
                }
            }

            # Nothing has changed yet: refuse when the account is no longer as the request expects (undo).
            Assert-IDLExpect -Item $item -User $user -GroupMap $groupMap -IdAttribute $idAttribute

            $before = New-IDLBefore -User $user
            $changes = Update-IDLUserAttributes -User $user -Desired $attributeRequest.Desired -ClearNames $attributeRequest.Clear -Server $server -DryRun:$DryRun -Done $done -Before $before.attributes

            if (($addDns.Count + $removeDns.Count) -gt 0){
                $changes += Update-IDLGroupMembership -User $user -AddDns $addDns -RemoveDns $removeDns -Server $server -DryRun:$DryRun -Done $done -Added $before.groupsAdded -Removed $before.groupsRemoved
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
                $result = New-IDLResult -ChangeId $changeId -ExternalId $externalId -Status skipped -Message 'no changes needed' @userFields
                Add-IDLReportRow -Action 'SetUser' -Result 'Skipped' -Identity $identity -ObjectIdOrDN $userDn -Details 'already in desired state'
            }
            elseif ($DryRun){
                $result = New-IDLResult -ChangeId $changeId -ExternalId $externalId -Status would -Message ($done -join '; ') -Before $before @userFields
            }
            else{
                $result = New-IDLResult -ChangeId $changeId -ExternalId $externalId -Status applied -Message ($done -join '; ') -Before $before @userFields
            }
        }
        catch{
            $errorText = Protect-IDLText -Text $_.Exception.Message
            Write-IDLLog "  Item $changeId failed: $errorText" 'ERROR'
            Write-IDLLog "  Stack: $($_.ScriptStackTrace)" 'DEBUG'
            $userFields = Get-IDLUserFields -User $user
            $message = (($done.Count -gt 0) ? ('failed after: ' + ($done -join '; ')) : ('failed'))
            $result = New-IDLResult -ChangeId $changeId -ExternalId $externalId -Status failed -Message $message -ErrorText $errorText @userFields
            Add-IDLReportRow -Action 'SetUser' -Result 'Failed' -Identity $externalId -ObjectIdOrDN ([string]$userFields['Dn']) -Details $message -ErrorText $errorText
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
