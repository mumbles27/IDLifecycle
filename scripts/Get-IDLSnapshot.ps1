<#
.SYNOPSIS
    Takes a read-only snapshot of the managed AD users (and resolves role-rule groups) for IDLifecycle.

.DESCRIPTION
    Report-only script (it changes nothing, so there is no -DryRun). Reads ONE JSON request document from
    stdin and writes ONE JSON response document to stdout.

    Request:
      {"server":"", "searchBases":["OU=Staff,DC=corp,DC=example,DC=com"], "idAttribute":"employeeID",
       "properties":["givenName","sn","title"], "groups":["GG-AllStaff","CN=Marketing,OU=Groups,DC=..."],
       "includeUnlinked":false}

    Response (ok:true):
      {"ok":true,
       "users":[{"objectGuid","dn","sam","upn","sid","enabled","parentOu","externalId","attributes":{..},"memberOf":[..]}],
       "groups":[{"identity","dn","found"}]}

    Users are every user under the search bases whose idAttribute is populated (enabled or not), fetched with
    one LDAP query per search base and only the properties that are needed. With "includeUnlinked": true the
    users whose idAttribute is empty are returned as well, with "externalId": "" (computer and managed service
    account objects are left out);
    the service uses them to correlate people with accounts that existed before it. Groups may be given as
    sAMAccountName or DN and are resolved in bulk. Attribute values are strings (null when unset).

    On failure the response is {"ok":false,"error":"..."} with a non-zero exit code.

.EXAMPLE
    '{"searchBases":["OU=Staff,DC=corp,DC=example,DC=com"],"idAttribute":"employeeID","properties":["title"],"groups":["GG-AllStaff"]}' |
        pwsh -NoProfile -File .\Get-IDLSnapshot.ps1

.EXAMPLE
    Get-Content .\snapshot-request.json -Raw | pwsh -NoProfile -File .\Get-IDLSnapshot.ps1 | ConvertFrom-Json

.NOTES
    Requires PowerShell 7+, the ActiveDirectory module and read access to the search bases.
    Exit codes: 0 ok, 2 invalid input, 3 ActiveDirectory module missing, 4 cannot reach AD, 5 unexpected error.
    Logs: $PSScriptRoot\Logs\Get-IDLSnapshot_<yyyyMMdd_HHmmss>.log. Nothing is ever written to stdout except the response.
#>

#Requires -Version 7.0

[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

Import-Module (Join-Path $PSScriptRoot 'IDLifecycle.Common.psm1') -Force

$exitCode = 5
$response = $null

try{
    Start-IDLRun -ScriptName 'Get-IDLSnapshot' -ScriptRoot $PSScriptRoot

    # ---- Phase: read and validate the request ----
    $request = Read-IDLRequest
    $common = Get-IDLRequestCommon -Request $request
    $server = $common.Server
    $idAttribute = $common.IdAttribute

    $searchBases = Get-IDLStringList -Value (Get-IDLField -Object $request -Name 'searchBases')
    if ($searchBases.Count -eq 0){
        throw (New-IDLException -Kind Input -Message 'searchBases is missing or empty.')
    }

    $properties = Get-IDLStringList -Value (Get-IDLField -Object $request -Name 'properties')
    foreach ($name in $properties){
        if (-not (Test-IDLAttributeName -Name $name)){
            throw (New-IDLException -Kind Input -Message "properties contains an invalid LDAP attribute name: '$name'.")
        }
    }

    $groupIdentities = Get-IDLStringList -Value (Get-IDLField -Object $request -Name 'groups')

    $includeUnlinked = Get-IDLField -Object $request -Name 'includeUnlinked'
    if (
        ($null -ne $includeUnlinked) -and
        ($includeUnlinked -isnot [bool])
    ){
        throw (New-IDLException -Kind Input -Message 'includeUnlinked must be true or false.')
    }

    $includeUnlinked = [bool]$includeUnlinked
    Write-IDLLog ("Request: server='{0}' idAttribute={1} searchBases={2} properties={3} groups={4} includeUnlinked={5}" -f $server, $idAttribute, $searchBases.Count, $properties.Count, $groupIdentities.Count, $includeUnlinked)

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

    # ---- Phase: users ----
    $phase = Start-IDLPhase -Name 'Query users'
    $fetchProperties = [string[]]@(@($idAttribute) + $properties + @('memberOf') | Select-Object -Unique)
    # includeUnlinked: every user (for correlation with existing accounts), otherwise only linked ones.
    $filter = (($includeUnlinked) ? ('(sAMAccountName=*)') : ('({0}=*)' -f $idAttribute))
    $seen = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::OrdinalIgnoreCase)
    $users = [System.Collections.Generic.List[object]]::new()
    $unlinked = 0

    foreach ($base in $searchBases){
        $found = @(Get-ADUser -LDAPFilter $filter -SearchBase $base -SearchScope Subtree -Properties $fetchProperties -ResultPageSize 1000 @serverParams)
        Write-IDLLog "Search base '$base': $($found.Count) user(s)$((($includeUnlinked)) ? ('') : (" with $idAttribute populated"))"

        foreach ($user in $found){
            $guid = ([guid]$user.ObjectGUID).ToString()
            if (-not $seen.Add($guid)){
                continue
            }

            $externalId = Get-IDLAdValue -Object $user -Name $idAttribute
            if ($null -eq $externalId){
                # Only person accounts are offered for correlation (a computer object is a "user" in LDAP terms).
                $class = $user.PSObject.Properties['ObjectClass']
                if (
                    (-not $includeUnlinked) -or
                    (($null -ne $class) -and ([string]$class.Value -match '^(?i)(computer|msDS-.*)$'))
                ){
                    continue
                }

                $externalId = ''
                $unlinked++
            }

            $attributes = [ordered]@{}
            foreach ($name in $properties){
                $attributes[$name] = Get-IDLAdValue -Object $user -Name $name
            }

            $users.Add([ordered]@{
                    objectGuid = $guid
                    dn         = [string]$user.DistinguishedName
                    sam        = [string]$user.SamAccountName
                    upn        = (Get-IDLAdValue -Object $user -Name 'UserPrincipalName')
                    sid        = (Get-IDLAdValue -Object $user -Name 'SID')
                    enabled    = [bool]$user.Enabled
                    parentOu   = (Get-IDLParentDn -Dn ([string]$user.DistinguishedName))
                    externalId = $externalId
                    attributes = $attributes
                    memberOf   = (Get-IDLAdValues -Object $user -Name 'MemberOf')
                })
        }
    }

    Add-IDLCounter -Name 'Total' -By $users.Count
    if ($includeUnlinked){
        Write-IDLLog "$unlinked user(s) without $idAttribute included"
    }

    Stop-IDLPhase -Phase $phase

    # ---- Phase: groups ----
    $phase = Start-IDLPhase -Name 'Resolve groups'
    $groupMap = Resolve-IDLGroups -Identities $groupIdentities -Server $server
    $groupResults = [System.Collections.Generic.List[object]]::new()
    foreach ($identity in $groupIdentities){
        $entry = $groupMap[$identity]
        $groupResults.Add([ordered]@{
                identity = $identity
                dn       = $entry.Dn
                found    = [bool]$entry.Found
            })
    }

    Stop-IDLPhase -Phase $phase

    $response = [ordered]@{
        ok     = $true
        users  = $users.ToArray()
        groups = $groupResults.ToArray()
    }
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
