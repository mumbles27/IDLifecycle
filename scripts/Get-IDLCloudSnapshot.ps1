<#
.SYNOPSIS
    Reads the Microsoft Entra side IDLifecycle plans against: the synced users and the direct user members of the
    cloud groups that roles and lifecycle groups name.

.DESCRIPTION
    Report-only script (it changes nothing, so there is no -DryRun). Reads ONE JSON request document from
    stdin and writes ONE JSON response document to stdout.

    Request:
      {"cloud":{...see IDLifecycle.Cloud.psm1...}, "groups":["<objectId>", ...], "match":"sid|immutable_id|upn",
       "includeUsers":true}

    groups: up to 1000 object ids. match (default sid) tells the service how it pairs AD accounts with Entra users;
    every user carries all three keys, so the script only validates and logs it. includeUsers (default true): read
    every user that is synchronised from AD (onPremisesSyncEnabled eq true).

    Response:
      {"ok":true,
       "users":[{"id","upn","sid","immutableId","enabled"}],
       "groups":[{"id","found":true,"name","description","mail","kind","manageable","reason","members":["<userId>"]}]}

    members are the group's DIRECT user members (nested groups are not expanded). A group that does not exist comes
    back with found:false, reason not_found and no members. Kinds and reasons as in Search-IDLCloudGroups.ps1.
    On failure the response is {"ok":false,"error":"..."} with a non-zero exit code.

.EXAMPLE
    Get-Content .\snapshot.json -Raw | pwsh -NoProfile -File .\Get-IDLCloudSnapshot.ps1 | ConvertFrom-Json

.NOTES
    Requires PowerShell 7+ and the application permissions User.Read.All, Group.Read.All and GroupMember.ReadWrite.All
    (or GroupMember.Read.All). Exit codes: 0 ok, 2 invalid input, 3 certificate missing, 4 sign-in failed / Graph
    unreachable / access denied, 5 unexpected error.
    Logs: $PSScriptRoot\Logs\Get-IDLCloudSnapshot_<yyyyMMdd_HHmmss>.log. Nothing is ever written to stdout except the response.
#>

#Requires -Version 7.0

[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

Import-Module (Join-Path $PSScriptRoot 'IDLifecycle.Common.psm1') -Force
Import-Module (Join-Path $PSScriptRoot 'IDLifecycle.Cloud.psm1') -Force

$exitCode = 5
$response = $null
$maxGroups = 1000

try{
    Start-IDLRun -ScriptName 'Get-IDLCloudSnapshot' -ScriptRoot $PSScriptRoot

    # ---- Phase: read and validate the request ----
    $request = Read-IDLRequest
    $config = Get-IDLCloudConfig -Request $request
    $rawGroups = Get-IDLField -Object $request -Name 'groups' -Default @()
    if (
        ($null -ne $rawGroups) -and
        ($rawGroups -isnot [System.Collections.IList])
    ){
        throw (New-IDLException -Kind Input -Message 'groups must be an array of object ids.')
    }

    $groupIds = Get-IDLCloudGuidList -Value $rawGroups -Name 'groups'
    if ($groupIds.Count -gt $maxGroups){
        throw (New-IDLException -Kind Input -Message "groups may hold at most $maxGroups object ids.")
    }

    $match = ConvertTo-IDLString -Value (Get-IDLField -Object $request -Name 'match')
    $match = (([string]::IsNullOrWhiteSpace($match)) ? ('sid') : ($match.Trim().ToLowerInvariant()))
    if ($match -notin @('sid', 'immutable_id', 'upn')){
        throw (New-IDLException -Kind Input -Message 'match must be sid, immutable_id or upn.')
    }

    $includeUsers = [bool](Get-IDLField -Object $request -Name 'includeUsers' -Default $true)
    Write-IDLLog "Request: groups=$($groupIds.Count) match=$match includeUsers=$includeUsers; $(Get-IDLCloudConfigText -Config $config)"

    # ---- Phase: users ----
    $users = [System.Collections.Generic.List[object]]::new()
    if ($includeUsers){
        $phase = Start-IDLPhase -Name 'Read synced users'
        $path = '/users?$filter={0}&$select=id,userPrincipalName,onPremisesSecurityIdentifier,onPremisesImmutableId,accountEnabled&$count=true&$top=999' -f [uri]::EscapeDataString('onPremisesSyncEnabled eq true')
        foreach ($u in (Invoke-IDLGraph -Config $config -Path $path -All -Eventual)){
            $users.Add([ordered]@{
                    id          = ([string](Get-IDLField -Object $u -Name 'id')).ToLowerInvariant()
                    upn         = [string](Get-IDLField -Object $u -Name 'userPrincipalName')
                    sid         = [string](Get-IDLField -Object $u -Name 'onPremisesSecurityIdentifier')
                    immutableId = [string](Get-IDLField -Object $u -Name 'onPremisesImmutableId')
                    enabled     = [bool](Get-IDLField -Object $u -Name 'accountEnabled' -Default $true)
                })
        }

        Write-IDLLog "$($users.Count) synced user(s)"
        Stop-IDLPhase -Phase $phase
    }

    # ---- Phase: groups ----
    $phase = Start-IDLPhase -Name 'Read groups and members'
    $select = Get-IDLGraphGroupSelect
    $groups = [System.Collections.Generic.List[object]]::new()
    foreach ($id in $groupIds){
        try{
            $g = Invoke-IDLGraph -Config $config -Path "/groups/$($id)?`$select=$select"
        }
        catch{
            if ((Get-IDLGraphErrorStatus -Exception $_.Exception) -ne 404){
                throw
            }

            Write-IDLLog "Group $id not found" 'WARN'
            Add-IDLCounter -Name 'NotFound'
            $groups.Add([ordered]@{ id = $id; found = $false; name = ''; description = ''; mail = ''; kind = ''; manageable = $false; reason = 'not_found'; members = [string[]]@() })
            continue
        }

        $entry = ConvertTo-IDLCloudGroup -Group $g -ExchangeEnabled $config.ExchangeEnabled
        $memberObjects = Invoke-IDLGraph -Config $config -Path "/groups/$id/members/microsoft.graph.user?`$select=id&`$top=999&`$count=true" -All -Eventual
        $members = @($memberObjects | ForEach-Object { ([string](Get-IDLField -Object $_ -Name 'id')).ToLowerInvariant() })
        $out = [ordered]@{ id = $entry.id; found = $true }
        foreach ($key in @('name', 'description', 'mail', 'kind', 'manageable', 'reason')){
            $out[$key] = $entry[$key]
        }

        $out.members = [string[]]$members
        $groups.Add($out)
        Add-IDLCounter -Name 'Resolved'
        Write-IDLLog ("Group {0} '{1}' ({2}{3}): {4} direct user member(s)" -f $id, $entry.name, $entry.kind, (($entry.manageable) ? ('') : (", not manageable: $($entry.reason)")), $members.Count)
    }

    Add-IDLCounter -Name 'Total' -By ($users.Count + $groups.Count)
    Stop-IDLPhase -Phase $phase

    $response = [ordered]@{ ok = $true; users = [object[]]$users.ToArray(); groups = [object[]]$groups.ToArray() }
    $exitCode = 0
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
