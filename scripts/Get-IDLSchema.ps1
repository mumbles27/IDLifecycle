<#
.SYNOPSIS
    Lists the attributes an AD object class (default: user) may carry, for the IDLifecycle field mapper.

.DESCRIPTION
    Report-only script (it changes nothing, so there is no -DryRun). Reads ONE JSON request document from
    stdin and writes ONE JSON response document to stdout.

    Request:
      {"server":"", "className":"user"}

    Response (ok:true):
      {"ok":true, "className":"user",
       "attributes":[{"name":"givenName","description":"...","syntax":"string","singleValued":true,
                      "systemOnly":false,"constructed":false,"linkId":0,"rangeUpper":64}]}

    The attribute set is the class's mustContain/systemMustContain/mayContain/systemMayContain, followed
    through subClassOf and the auxiliary classes, from two one-level queries of the schema partition (all
    classSchema and all attributeSchema objects). syntax is one of string, dn, integer, boolean, time,
    binary, oid, other (from attributeSyntax). constructed comes from systemFlags bit 0x4.

    On failure the response is {"ok":false,"error":"..."} with a non-zero exit code.

.EXAMPLE
    '{"className":"user"}' | pwsh -NoProfile -File .\Get-IDLSchema.ps1 | ConvertFrom-Json

.NOTES
    Requires PowerShell 7+, the ActiveDirectory module and read access to the schema partition (any
    authenticated account has it by default).
    Exit codes: 0 ok, 2 invalid input, 3 ActiveDirectory module missing, 4 cannot reach AD, 5 unexpected error.
    Logs: $PSScriptRoot\Logs\Get-IDLSchema_<yyyyMMdd_HHmmss>.log. Nothing is ever written to stdout except the response.
#>

#Requires -Version 7.0

[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

Import-Module (Join-Path $PSScriptRoot 'IDLifecycle.Common.psm1') -Force

# attributeSyntax OID -> the coarse syntax the field mapper understands.
$script:SyntaxMap = @{
    '2.5.5.1'  = 'dn'        # DN
    '2.5.5.2'  = 'oid'       # OID
    '2.5.5.3'  = 'string'    # case-sensitive string
    '2.5.5.4'  = 'string'    # case-insensitive (teletex) string
    '2.5.5.5'  = 'string'    # printable / IA5 string
    '2.5.5.6'  = 'string'    # numeric string
    '2.5.5.7'  = 'other'     # DN-binary / OR-name
    '2.5.5.8'  = 'boolean'
    '2.5.5.9'  = 'integer'
    '2.5.5.10' = 'binary'    # octet string
    '2.5.5.11' = 'time'      # generalized / UTC time
    '2.5.5.12' = 'string'    # Unicode string
    '2.5.5.13' = 'other'     # presentation address
    '2.5.5.14' = 'other'     # DN-string
    '2.5.5.15' = 'binary'    # NT security descriptor
    '2.5.5.16' = 'integer'   # large integer
    '2.5.5.17' = 'binary'    # SID
}

function ConvertTo-IDLInt {
    param([AllowNull()]$Value)

    $text = [string]$Value
    $n = 0
    if ([int]::TryParse($text, [ref]$n)){
        return $n
    }

    return 0
}

$exitCode = 5
$response = $null

try{
    Start-IDLRun -ScriptName 'Get-IDLSchema' -ScriptRoot $PSScriptRoot

    # ---- Phase: read and validate the request ----
    $request = Read-IDLRequest
    $server = ConvertTo-IDLString -Value (Get-IDLField -Object $request -Name 'server')
    if ($null -eq $server){
        $server = ''
    }

    $className = ConvertTo-IDLString -Value (Get-IDLField -Object $request -Name 'className')
    if ([string]::IsNullOrWhiteSpace($className)){
        $className = 'user'
    }

    if (-not (Test-IDLAttributeName -Name $className)){
        throw (New-IDLException -Kind Input -Message "className '$className' is not a valid LDAP class name.")
    }

    Write-IDLLog "Request: server='$server' className=$className"

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

    $schemaNc = [string](Get-ADRootDSE @serverParams).schemaNamingContext
    if ([string]::IsNullOrWhiteSpace($schemaNc)){
        throw (New-IDLException -Kind Connection -Message 'The domain controller did not report a schemaNamingContext.')
    }

    # ---- Phase: classes ----
    $phase = Start-IDLPhase -Name 'Read classes'
    $classProps = @('lDAPDisplayName', 'subClassOf', 'auxiliaryClass', 'systemAuxiliaryClass', 'mustContain', 'systemMustContain', 'mayContain', 'systemMayContain')
    $classes = @{}
    foreach ($c in @(Get-ADObject -SearchBase $schemaNc -SearchScope OneLevel -LDAPFilter '(objectClass=classSchema)' -Properties $classProps -ResultPageSize 1000 @serverParams)){
        $name = Get-IDLAdValue -Object $c -Name 'lDAPDisplayName'
        if ($name){
            $classes[$name.ToLowerInvariant()] = $c
        }
    }

    if (-not $classes.ContainsKey($className.ToLowerInvariant())){
        throw (New-IDLException -Kind Input -Message "The schema has no class '$className'.")
    }

    $wanted = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::OrdinalIgnoreCase)
    $visited = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::OrdinalIgnoreCase)
    $queue = [System.Collections.Generic.Queue[string]]::new()
    $queue.Enqueue($className)
    while ($queue.Count -gt 0){
        $name = $queue.Dequeue()
        if (-not $visited.Add($name)){
            continue
        }

        $c = $classes[$name.ToLowerInvariant()]
        if ($null -eq $c){
            Write-IDLLog "Class '$name' is referenced but not in the schema; skipped" 'WARN'
            continue
        }

        foreach ($p in @('mustContain', 'systemMustContain', 'mayContain', 'systemMayContain')){
            foreach ($a in (Get-IDLAdValues -Object $c -Name $p)){
                $null = $wanted.Add($a)
            }
        }

        foreach ($p in @('subClassOf', 'auxiliaryClass', 'systemAuxiliaryClass')){
            foreach ($next in (Get-IDLAdValues -Object $c -Name $p)){
                $queue.Enqueue($next)
            }
        }
    }

    Write-IDLLog "Class '$className': $($visited.Count) class(es) in its hierarchy, $($wanted.Count) attribute(s)"
    Stop-IDLPhase -Phase $phase

    # ---- Phase: attributes ----
    $phase = Start-IDLPhase -Name 'Read attributes'
    $attrProps = @('lDAPDisplayName', 'adminDescription', 'attributeSyntax', 'isSingleValued', 'systemOnly', 'systemFlags', 'linkID', 'rangeUpper')
    $attributes = [System.Collections.Generic.List[object]]::new()
    foreach ($a in @(Get-ADObject -SearchBase $schemaNc -SearchScope OneLevel -LDAPFilter '(objectClass=attributeSchema)' -Properties $attrProps -ResultPageSize 1000 @serverParams)){
        $name = Get-IDLAdValue -Object $a -Name 'lDAPDisplayName'
        if (
            (-not $name) -or
            (-not $wanted.Contains($name))
        ){
            continue
        }

        $syntax = $script:SyntaxMap[[string](Get-IDLAdValue -Object $a -Name 'attributeSyntax')]
        $attributes.Add([ordered]@{
                name         = $name
                description  = [string](Get-IDLAdValue -Object $a -Name 'adminDescription')
                syntax       = (($syntax) ? ($syntax) : ('other'))
                singleValued = ((Get-IDLAdValue -Object $a -Name 'isSingleValued') -eq 'True')
                systemOnly   = ((Get-IDLAdValue -Object $a -Name 'systemOnly') -eq 'True')
                constructed  = (((ConvertTo-IDLInt (Get-IDLAdValue -Object $a -Name 'systemFlags')) -band 4) -ne 0)
                linkId       = (ConvertTo-IDLInt (Get-IDLAdValue -Object $a -Name 'linkID'))
                rangeUpper   = (ConvertTo-IDLInt (Get-IDLAdValue -Object $a -Name 'rangeUpper'))
            })
    }

    Add-IDLCounter -Name 'Total' -By $attributes.Count
    Stop-IDLPhase -Phase $phase

    $response = [ordered]@{
        ok         = $true
        className  = $className
        attributes = @($attributes | Sort-Object -Property { $_.name })
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
