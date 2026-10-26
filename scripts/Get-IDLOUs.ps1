<#
.SYNOPSIS
    Lists the organizational units below the search bases (or the whole domain), for the OU picker in the
    IDLifecycle GUI (OU placement, Change OU on an approval).

.DESCRIPTION
    Report-only script (it changes nothing, so there is no -DryRun). Reads ONE JSON request document from
    stdin and writes ONE JSON response document to stdout.

    Request:
      {"server":"", "searchBases":["OU=Staff,DC=corp,DC=example,DC=com"]}     (empty list = the whole domain)

    Response (ok:true):
      {"ok":true, "ous":[{"dn":"OU=Finance,OU=Staff,DC=corp,DC=example,DC=com","name":"Finance"}]}

    One subtree query per search base for objectClass=organizationalUnit (the base itself is included when it is
    an OU). Duplicates from overlapping bases are dropped.
    On failure the response is {"ok":false,"error":"..."} with a non-zero exit code.

.EXAMPLE
    '{"searchBases":[]}' | pwsh -NoProfile -File .\Get-IDLOUs.ps1 | ConvertFrom-Json

.NOTES
    Requires PowerShell 7+ and the ActiveDirectory module; any authenticated account can read OUs by default.
    Exit codes: 0 ok, 2 invalid input, 3 ActiveDirectory module missing, 4 cannot reach AD, 5 unexpected error.
    Logs: $PSScriptRoot\Logs\Get-IDLOUs_<yyyyMMdd_HHmmss>.log. Nothing is ever written to stdout except the response.
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
    Start-IDLRun -ScriptName 'Get-IDLOUs' -ScriptRoot $PSScriptRoot

    # ---- Phase: read and validate the request ----
    $request = Read-IDLRequest
    $server = ConvertTo-IDLString -Value (Get-IDLField -Object $request -Name 'server')
    if ($null -eq $server){
        $server = ''
    }

    [string[]]$bases = Get-IDLStringList -Value (Get-IDLField -Object $request -Name 'searchBases')
    foreach ($b in $bases){
        if (-not (Test-IDLDistinguishedName -Value $b)){
            throw (New-IDLException -Kind Input -Message "searchBases: '$b' is not a distinguished name.")
        }
    }

    Write-IDLLog "Request: server='$server' searchBases=$($bases.Count)"

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
    if ($bases.Count -eq 0){
        $root = [string](Get-ADRootDSE @serverParams).defaultNamingContext
        if ([string]::IsNullOrWhiteSpace($root)){
            throw (New-IDLException -Kind Connection -Message 'The domain controller did not report a defaultNamingContext.')
        }

        $bases = @($root)
    }

    # ---- Phase: OUs ----
    $phase = Start-IDLPhase -Name 'Read OUs'
    $seen = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::OrdinalIgnoreCase)
    $ous = [System.Collections.Generic.List[object]]::new()
    foreach ($base in $bases){
        foreach ($o in @(Get-ADObject -SearchBase $base -SearchScope Subtree -LDAPFilter '(objectClass=organizationalUnit)' -ResultPageSize 1000 @serverParams)){
            $dn = [string]$o.DistinguishedName
            if ((-not $dn) -or (-not $seen.Add($dn))){
                continue
            }

            $ous.Add([ordered]@{ dn = $dn; name = ((Get-IDLLeafRdn -Dn $dn) -replace '^(?i)(OU|CN)=', '') })
        }
    }

    Add-IDLCounter -Name 'Total' -By $ous.Count
    Write-IDLLog "$($ous.Count) OU(s) below $($bases.Count) base(s)"
    Stop-IDLPhase -Phase $phase

    $response = [ordered]@{ ok = $true; ous = $ous.ToArray() }
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
