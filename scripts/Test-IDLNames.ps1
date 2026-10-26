<#
.SYNOPSIS
    Reports which account names (sAMAccountName, userPrincipalName, mail, cn) are already used in AD, for IDLifecycle.

.DESCRIPTION
    Report-only script (it changes nothing, so there is no -DryRun). Reads ONE JSON request document from
    stdin and writes ONE JSON response document to stdout.

    Request:
      {"server":"", "names":[{"field":"sam","value":"jdoe"}, {"field":"upn","value":"jdoe@corp.example.com"},
                             {"field":"mail","value":"jane.doe@corp.example.com"},
                             {"field":"cn","value":"Jane Doe","ou":"OU=Staff,DC=corp,DC=example,DC=com"}],
       "exclude":["<objectGUID>"]}

    Rules (the same the create script applies):
      * sam and upn: taken when ANY object in the domain (user, group, computer ...) has the value.
      * mail: taken when any object (users, contacts, groups ...) has it as mail or as an smtp: proxyAddresses
        entry; compared case-insensitively.
      * cn: taken when any object directly below "ou" has that name.
      * Objects listed in "exclude" (objectGUIDs) never count - used when an account is renamed and its own
        current names must not block it.
    LDAP filters are batched (at most 50 terms per query); duplicate names in the request are checked once.

    Response (ok:true):
      {"ok":true, "taken":[{"field":"sam","value":"jdoe","dn":"CN=John Doe,OU=Staff,...","objectGuid":"..."}],
       "missingOus":["OU=Nope,..."]}
    "taken" has one entry per name and object that holds it ("value" exactly as requested); "missingOus" lists cn
    target OUs that do not exist (nothing could be checked there). On failure the response is
    {"ok":false,"error":"..."} with a non-zero exit code.

.EXAMPLE
    '{"names":[{"field":"sam","value":"jdoe"},{"field":"mail","value":"jane.doe@corp.example.com"}]}' |
        pwsh -NoProfile -File .\Test-IDLNames.ps1

.NOTES
    Requires PowerShell 7+, the ActiveDirectory module and read access to the domain.
    Exit codes: 0 ok, 2 invalid input, 3 ActiveDirectory module missing, 4 cannot reach AD, 5 unexpected error.
    Logs: $PSScriptRoot\Logs\Test-IDLNames_<yyyyMMdd_HHmmss>.log (only counts are logged, not the names).
    Nothing is ever written to stdout except the response.
#>

#Requires -Version 7.0

[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

Import-Module (Join-Path $PSScriptRoot 'IDLifecycle.Common.psm1') -Force

$exitCode = 5
$response = $null
$maxNames = 5000

try{
    Start-IDLRun -ScriptName 'Test-IDLNames' -ScriptRoot $PSScriptRoot

    # ---- Phase: read and validate the request ----
    $request = Read-IDLRequest
    $server = ConvertTo-IDLString -Value (Get-IDLField -Object $request -Name 'server')
    $server = (($null -eq $server) ? ('') : ($server.Trim()))

    $rawNames = Get-IDLField -Object $request -Name 'names'
    if ($rawNames -isnot [System.Collections.IList]){
        throw (New-IDLException -Kind Input -Message 'names is missing or not an array.')
    }

    if ($rawNames.Count -gt $maxNames){
        throw (New-IDLException -Kind Input -Message "names has $($rawNames.Count) entries; at most $maxNames are allowed per request.")
    }

    $names = [System.Collections.Generic.List[object]]::new()
    $seen = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::OrdinalIgnoreCase)
    for ($i = 0; $i -lt $rawNames.Count; $i++){
        $entry = $rawNames[$i]
        if ($entry -isnot [System.Collections.IDictionary]){
            throw (New-IDLException -Kind Input -Message "names[$i] is not a JSON object.")
        }

        $field = ([string](ConvertTo-IDLString -Value (Get-IDLField -Object $entry -Name 'field'))).Trim().ToLowerInvariant()
        if ($field -notin @('sam', 'upn', 'mail', 'cn')){
            throw (New-IDLException -Kind Input -Message "names[$i].field must be sam, upn, mail or cn.")
        }

        $value = ([string](ConvertTo-IDLString -Value (Get-IDLField -Object $entry -Name 'value'))).Trim()
        if (
            ($value -eq '') -or
            ($value.Length -gt 256) -or
            ($value -cmatch '[\x00-\x1f\x7f]')
        ){
            throw (New-IDLException -Kind Input -Message "names[$i].value must be 1-256 printable characters.")
        }

        $ou = ''
        if ($field -eq 'cn'){
            $ou = ([string](ConvertTo-IDLString -Value (Get-IDLField -Object $entry -Name 'ou'))).Trim()
            if (-not (Test-IDLDnShape -Value $ou)){
                throw (New-IDLException -Kind Input -Message "names[$i].ou is required for cn and must be a well-formed distinguished name.")
            }
        }

        if ($seen.Add("$field|$value|$ou")){
            $names.Add(@{ Field = $field; Value = $value; Ou = $ou })
        }
    }

    $exclude = [System.Collections.Generic.List[string]]::new()
    foreach ($text in (Get-IDLStringList -Value (Get-IDLField -Object $request -Name 'exclude'))){
        $guid = [guid]::Empty
        if (
            (-not [guid]::TryParse($text, [ref]$guid)) -or
            ($guid -eq [guid]::Empty)
        ){
            throw (New-IDLException -Kind Input -Message 'exclude must contain valid non-empty objectGUIDs.')
        }

        $exclude.Add($guid.ToString())
    }

    Write-IDLLog ("Request: server='{0}' names={1} exclude={2}" -f $server, $names.Count, $exclude.Count)

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

    # ---- Phase: lookups ----
    $phase = Start-IDLPhase -Name 'Look up names'
    $lookup = Find-IDLTakenNames -Names $names.ToArray() -Server $server -Exclude ([string[]]$exclude.ToArray())
    Add-IDLCounter -Name 'Total' -By $names.Count
    Add-IDLCounter -Name 'Taken' -By @($lookup.Taken).Count
    foreach ($ou in $lookup.MissingOus){
        Write-IDLLog "Target OU does not exist (cn not checked): $ou" 'WARN'
    }

    Write-IDLLog ("{0} of {1} name(s) are taken" -f @($lookup.Taken | ForEach-Object { "$($_.field)|$($_.value)" } | Select-Object -Unique).Count, $names.Count)
    Stop-IDLPhase -Phase $phase

    $response = [ordered]@{
        ok         = $true
        taken      = @($lookup.Taken)
        missingOus = @($lookup.MissingOus)
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
