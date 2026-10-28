<#
.SYNOPSIS
    Searches the domain for groups by name, for the group picker in the IDLifecycle GUI (role grants, lifecycle groups).

.DESCRIPTION
    Report-only script (it changes nothing, so there is no -DryRun). Reads ONE JSON request document from
    stdin and writes ONE JSON response document to stdout.

    Request:
      {"server":"", "query":"fin", "max":25}

    query: 2-64 characters (at least 2 that are not "*"); "*" is a wildcard, every other character is matched literally
    (LDAP-escaped). The query is matched as *query* against cn, sAMAccountName, displayName and description of every
    group in the domain (default naming context). max: 1-200, default 25.

    Response (ok:true), sorted by name (case-insensitive):
      {"ok":true, "groups":[{"dn":"CN=GG-Finance,OU=Groups,DC=corp,DC=example,DC=com","sam":"GG-Finance",
        "name":"GG-Finance","description":"Finance staff","category":"security|distribution",
        "scope":"global|universal|domainlocal","privileged":false}], "truncated":false}

    category and scope come from groupType (security bit 0x80000000; 0x2 global, 0x4 domain local, 0x8 universal).
    privileged = adminCount=1 or a well-known privileged RID (512/518/519/544/548/549/550/551) - the scripts refuse to
    add accounts to such groups. Up to 1000 matching groups are read; truncated = more groups matched than "max" (or
    than 1000: refine the query).
    On failure the response is {"ok":false,"error":"..."} with a non-zero exit code.

.EXAMPLE
    '{"query":"finance","max":10}' | pwsh -NoProfile -File .\Search-IDLGroups.ps1 | ConvertFrom-Json

.NOTES
    Requires PowerShell 7+ and the ActiveDirectory module; any authenticated account can read groups by default.
    Exit codes: 0 ok, 2 invalid input, 3 ActiveDirectory module missing, 4 cannot reach AD, 5 unexpected error.
    Logs: $PSScriptRoot\Logs\Search-IDLGroups_<yyyyMMdd_HHmmss>.log. Nothing is ever written to stdout except the response.
#>

#Requires -Version 7.0

[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

Import-Module (Join-Path $PSScriptRoot 'IDLifecycle.Common.psm1') -Force

$exitCode = 5
$response = $null
$readLimit = 1000

function ConvertTo-IDLGroupSearchPattern {
    # "*query*" with every character but "*" LDAP-escaped; runs of "*" collapse to one.
    param([Parameter(Mandatory)][string]$Query)

    $parts = foreach ($piece in ($Query -split '\*')){
        ConvertTo-IDLLdapFilterValue -Value $piece
    }

    return ('*' + ($parts -join '*') + '*') -replace '\*{2,}', '*'
}

function Get-IDLGroupKind {
    # category / scope from groupType; GroupCategory / GroupScope as a fallback.
    param([Parameter(Mandatory)]$Group)

    $raw = Get-IDLAdValue -Object $Group -Name 'groupType'
    [long]$type = 0
    if ([long]::TryParse([string]$raw, [ref]$type)){
        $bits = $type -band 0xFFFFFFFFL
        $category = ((($bits -band 0x80000000L) -ne 0) ? ('security') : ('distribution'))
        $scope = $(
            if (($bits -band 0x8) -ne 0){ 'universal' }
            elseif (($bits -band 0x4) -ne 0){ 'domainlocal' }
            else{ 'global' }
        )
        return @{ Category = $category; Scope = $scope }
    }

    $cat = [string](Get-IDLAdValue -Object $Group -Name 'GroupCategory')
    $sc = ([string](Get-IDLAdValue -Object $Group -Name 'GroupScope')).ToLowerInvariant()
    return @{
        Category = ((($cat -ieq 'Distribution')) ? ('distribution') : ('security'))
        Scope    = ((($sc -in @('global', 'universal', 'domainlocal'))) ? ($sc) : ('global'))
    }
}

try{
    Start-IDLRun -ScriptName 'Search-IDLGroups' -ScriptRoot $PSScriptRoot

    # ---- Phase: read and validate the request ----
    $request = Read-IDLRequest
    $server = ConvertTo-IDLString -Value (Get-IDLField -Object $request -Name 'server')
    if ($null -eq $server){
        $server = ''
    }

    $query = ConvertTo-IDLString -Value (Get-IDLField -Object $request -Name 'query')
    $query = (($null -eq $query) ? ('') : ($query.Trim()))
    if (
        ($query.Length -lt 2) -or
        ($query.Length -gt 64) -or
        (($query -replace '\*', '').Length -lt 2)
    ){
        throw (New-IDLException -Kind Input -Message 'query must be 2-64 characters, at least 2 of them not "*".')
    }

    if ($query -match '[\x00-\x1F\x7F]'){
        throw (New-IDLException -Kind Input -Message 'query must not contain control characters.')
    }

    $max = 25
    $rawMax = Get-IDLField -Object $request -Name 'max'
    if ($null -ne $rawMax){
        $parsed = 0
        if (
            (-not [int]::TryParse([string]$rawMax, [ref]$parsed)) -or
            ($parsed -lt 1) -or
            ($parsed -gt 200)
        ){
            throw (New-IDLException -Kind Input -Message 'max must be a whole number from 1 to 200.')
        }

        $max = $parsed
    }

    Write-IDLLog "Request: server='$server' query='$query' max=$max"

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

    Test-IDLAdConnection -Server $server
    $serverParams = Get-IDLServerParams -Server $server
    $root = [string](Get-ADRootDSE @serverParams).defaultNamingContext
    if ([string]::IsNullOrWhiteSpace($root)){
        throw (New-IDLException -Kind Connection -Message 'The domain controller did not report a defaultNamingContext.')
    }

    # ---- Phase: search ----
    $phase = Start-IDLPhase -Name 'Search groups'
    $pattern = ConvertTo-IDLGroupSearchPattern -Query $query
    $filter = '(&(objectCategory=group)(|(cn={0})(sAMAccountName={0})(displayName={0})(description={0})))' -f $pattern
    Write-IDLLog "LDAP filter: $filter (base $root)"
    $found = @(Get-ADGroup -LDAPFilter $filter -SearchBase $root -SearchScope Subtree -ResultPageSize 500 -ResultSetSize ($readLimit + 1) `
            -Properties @('description', 'displayName', 'adminCount', 'objectSid', 'groupType') @serverParams)

    $groups = [System.Collections.Generic.List[object]]::new()
    foreach ($g in ($found | Select-Object -First $readLimit)){
        $kind = Get-IDLGroupKind -Group $g
        $description = Get-IDLAdValue -Object $g -Name 'description'
        $groups.Add([ordered]@{
                dn          = [string]$g.DistinguishedName
                sam         = [string]$g.SamAccountName
                name        = [string]$g.Name
                description = (($null -eq $description) ? ('') : ([string]$description))
                category    = $kind.Category
                scope       = $kind.Scope
                privileged  = ($null -ne (Get-IDLPrivilegedReason -Group $g))
            })
    }

    $sorted = @($groups | Sort-Object -Property @{ Expression = { ([string]$_.name).ToLowerInvariant() } }, @{ Expression = { [string]$_.dn } })
    $truncated = ($found.Count -gt $readLimit) -or ($sorted.Count -gt $max)
    $result = @($sorted | Select-Object -First $max)
    Add-IDLCounter -Name 'Total' -By $result.Count
    Write-IDLLog "$($found.Count) group(s) matched; returning $($result.Count)$((($truncated)) ? (' (truncated)') : (''))"
    Stop-IDLPhase -Phase $phase

    $response = [ordered]@{ ok = $true; groups = [object[]]$result; truncated = [bool]$truncated }
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
