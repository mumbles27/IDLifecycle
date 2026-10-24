<#
.SYNOPSIS
    Deletes DISABLED AD user accounts for IDLifecycle (refuses to delete enabled accounts or non-users).

.DESCRIPTION
    Reads ONE JSON request from stdin and writes ONE JSON response to stdout.

    Request:
      {"server":"", "idAttribute":"employeeID", "items":[ {"changeId":"c1","externalId":"E1001","objectGuid":"<guid>"} ]}

    Safety rails per item:
      * the account is found by objectGuid (all items in one bulk query) and its idAttribute MUST equal externalId,
        otherwise the item fails and nothing is deleted;
      * the object must be a user (a computer or anything else is refused);
      * the account must be DISABLED - an enabled account is refused ("account is enabled");
      * if the account has child objects (e.g. CN=ExchangeActiveSyncDevices), none of them may be a user, computer,
        group or inetOrgPerson; otherwise the item fails. The account is deleted with Remove-ADObject -Recursive
        (Remove-ADUser cannot delete a non-leaf object), so leaf device/container children go with it;
      * an account that is already gone is "skipped" (idempotent).
    One failing item never stops the others.

    Response: {"ok":true,"dryRun":false,"results":[{"changeId","externalId","status":"applied|would|skipped|failed",
    "message","objectGuid","dn","sam","upn","error"}],"summary":{"total","applied","would","skipped","failed"}}

.PARAMETER DryRun
    Do every lookup and safety check and log/report WOULD: statements, but delete nothing. Successful items
    come back with status "would".

.EXAMPLE
    '{"idAttribute":"employeeID","items":[{"changeId":"c1","externalId":"E1001","objectGuid":"11111111-1111-1111-1111-111111111111"}]}' |
        pwsh -NoProfile -File .\Remove-IDLUser.ps1 -DryRun

.EXAMPLE
    Get-Content .\remove-request.json -Raw | pwsh -NoProfile -File .\Remove-IDLUser.ps1 | ConvertFrom-Json

.NOTES
    Requires PowerShell 7+, the ActiveDirectory module and delegated delete rights on user objects (Delete subtree,
    because accounts with child objects are deleted recursively).
    Exit codes: 0 ok, 1 some items failed, 2 invalid input, 3 module missing, 4 cannot reach AD, 5 unexpected error.
    Logs and the CSV report (Remove-IDLUser_Report_<ts>.csv) are written to $PSScriptRoot\Logs. DELETION IS PERMANENT
    unless the AD Recycle Bin is enabled - use -DryRun first.
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
    Start-IDLRun -ScriptName 'Remove-IDLUser' -ScriptRoot $PSScriptRoot -IsAction -DryRun:$DryRun -Parameters @{ DryRun = [bool]$DryRun }

    # ---- Phase: read and validate the request ----
    $request = Read-IDLRequest
    $common = Get-IDLRequestCommon -Request $request -RequireItems
    $server = $common.Server
    $idAttribute = $common.IdAttribute
    $items = $common.Items
    Add-IDLCounter -Name 'Total' -By $items.Count
    Write-IDLLog ("Request: server='{0}' idAttribute={1} items={2}" -f $server, $idAttribute, $items.Count)

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

    # Object classes that must never be deleted as a side effect of deleting a user (recursive delete).
    $principalClasses = @('user', 'computer', 'group', 'inetOrgPerson')

    # ---- Phase: bulk resolution ----
    $phase = Start-IDLPhase -Name 'Resolve accounts'
    $targets = Resolve-IDLTargetUsers -Items $items -IdAttribute $idAttribute -Server $server
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
        $result = $null

        try{
            switch ($target.Status){
                'NotFound' {
                    # A computer (or other object) can hide from the user-only lookup; make sure we are not
                    # reporting "gone" for something that still exists and is simply not a user.
                    $guidText = ConvertTo-IDLString -Value (Get-IDLField -Object $item -Name 'objectGuid')
                    if (-not [string]::IsNullOrWhiteSpace($guidText)){
                        $other = $null
                        try{
                            $other = Get-ADObject -Identity ([guid]$guidText.Trim()) @serverParams
                        }
                        catch{
                            if (-not (Test-IDLAdNotFound -Exception $_.Exception)){
                                throw
                            }
                        }

                        if ($null -ne $other){
                            throw "object $($other.DistinguishedName) is not a user (objectClass $($other.ObjectClass)); refusing to delete"
                        }
                    }

                    $result = New-IDLResult -ChangeId $changeId -ExternalId $externalId -Status skipped -Message 'already gone (account not found)'
                    Add-IDLReportRow -Action 'RemoveUser' -Result 'Skipped' -Identity $externalId -Details $target.Message
                }
                'Found' {
                    $identity = [string]$user.SamAccountName
                    $userDn = [string]$user.DistinguishedName
                    $userFields = Get-IDLUserFields -User $user
                    Write-IDLLog "Processing $changeId ($externalId) -> $userDn"

                    if ([string]$user.ObjectClass -ine 'user'){
                        throw "object is not a user (objectClass $($user.ObjectClass)); refusing to delete"
                    }

                    if ([bool]$user.Enabled){
                        throw 'account is enabled; refusing to delete (disable it first)'
                    }

                    # Child objects (e.g. CN=ExchangeActiveSyncDevices) make the user a non-leaf, which Remove-ADUser
                    # cannot delete. Device/container children may go with it; a security principal below it may not.
                    $children = @(Get-ADObject -LDAPFilter '(objectClass=*)' -SearchBase $userDn -SearchScope Subtree @serverParams | Where-Object { [string]$_.DistinguishedName -ine $userDn })
                    Write-IDLLog "  $identity has $($children.Count) child object(s)"
                    $principals = @($children | Where-Object { [string]$_.ObjectClass -in $principalClasses })
                    if ($principals.Count -gt 0){
                        $names = @($principals | ForEach-Object { '{0} ({1})' -f $_.DistinguishedName, $_.ObjectClass }) -join '; '
                        throw "account has child security principal(s): $names; refusing to delete"
                    }

                    $childNote = (($children.Count -gt 0) ? (" with $($children.Count) child object(s)") : (''))
                    if ($DryRun){
                        Write-IDLLog "  WOULD: delete $userDn$childNote"
                        Add-IDLReportRow -Action 'RemoveUser' -Result 'Would' -Identity $identity -ObjectIdOrDN $userDn -Details $childNote.Trim()
                        $result = New-IDLResult -ChangeId $changeId -ExternalId $externalId -Status would -Message ('would delete account' + $childNote) @userFields
                    }
                    else{
                        # Remove-ADObject -Recursive also removes the leaf children; all checks above have passed.
                        Remove-ADObject -Identity ([guid]$user.ObjectGUID) -Recursive -Confirm:$false @serverParams
                        Write-IDLLog "  Deleted $userDn$childNote"
                        Add-IDLReportRow -Action 'RemoveUser' -Result 'Applied' -Identity $identity -ObjectIdOrDN $userDn
                        $result = New-IDLResult -ChangeId $changeId -ExternalId $externalId -Status applied -Message ('account deleted' + $childNote) @userFields
                    }
                }
                default {
                    $userFields = Get-IDLUserFields -User $user
                    $result = New-IDLResult -ChangeId $changeId -ExternalId $externalId -Status failed -Message $target.Message -ErrorText $target.Message @userFields
                    Add-IDLReportRow -Action 'RemoveUser' -Result 'Failed' -Identity $externalId -Details $target.Status -ErrorText $target.Message
                }
            }
        }
        catch{
            $errorText = Protect-IDLText -Text $_.Exception.Message
            Write-IDLLog "  Item $changeId failed: $errorText" 'ERROR'
            Write-IDLLog "  Stack: $($_.ScriptStackTrace)" 'DEBUG'
            $userFields = Get-IDLUserFields -User $user
            $result = New-IDLResult -ChangeId $changeId -ExternalId $externalId -Status failed -Message 'failed' -ErrorText $errorText @userFields
            Add-IDLReportRow -Action 'RemoveUser' -Result 'Failed' -Identity $externalId -ObjectIdOrDN ([string]$userFields['Dn']) -Details 'failed' -ErrorText $errorText
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
