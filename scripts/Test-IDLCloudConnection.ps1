<#
.SYNOPSIS
    Checks the Microsoft Entra / Exchange Online connection of IDLifecycle step by step and says what is missing.

.DESCRIPTION
    Report-only script (it changes nothing). Reads ONE JSON request document from stdin and writes ONE JSON response
    document to stdout.

    Request:
      {"cloud":{...see IDLifecycle.Cloud.psm1...}}

    Response (exit 0 even when checks fail - only bad input or an unexpected error is a script failure):
      {"ok":true, "healthy":false, "checks":[{"name","ok","warning","detail"}]}

    Checks, in order (a check that cannot run because an earlier one failed is ok:false with "skipped: ..."):
      certificate         (certificate auth) found in LocalMachine\My or CurrentUser\My with a private key; not expired
                          (warning when it expires within 30 days); with Exchange Online enabled the key must be a CSP
                          (CryptoAPI) key - Exchange app-only sign-in does not accept CNG keys.
      token               an app-only token for Microsoft Graph can be obtained.
      graph_permissions   the token's application permissions (roles claim) cover GroupMember.ReadWrite.All,
                          Group.Read.All and User.Read.All (Directory.* / *.ReadWrite.All equivalents count); a warning
                          names permissions broader than IDLifecycle needs.
      graph_read          groups and users can be read.
      exchange_module     (Exchange enabled) ExchangeOnlineManagement 3.x is installed for this account.
      exchange_connect    (Exchange enabled) app-only connection to Exchange Online and a recipient read.
      exchange_permissions (Exchange enabled) the app's Exchange role exposes Get-/Add-/Remove-DistributionGroupMember.

.EXAMPLE
    Get-Content .\cloud.json -Raw | pwsh -NoProfile -File .\Test-IDLCloudConnection.ps1 | ConvertFrom-Json

.NOTES
    Requires PowerShell 7+. Exit codes: 0 checks ran (see healthy), 2 invalid input, 5 unexpected error.
    Logs: $PSScriptRoot\Logs\Test-IDLCloudConnection_<yyyyMMdd_HHmmss>.log. Nothing is ever written to stdout except the response.
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
$checks = [System.Collections.Generic.List[object]]::new()

function Add-Check {
    param([string]$Name, [bool]$Ok, [string]$Detail, [bool]$Warning = $false)

    $checks.Add([ordered]@{ name = $Name; ok = $Ok; warning = $Warning; detail = (Protect-IDLText -Text $Detail) })
    $level = $(
        if (-not $Ok){ 'WARN' }
        elseif ($Warning){ 'WARN' }
        else{ 'INFO' }
    )
    Write-IDLLog ('Check {0}: {1} - {2}' -f $Name, (($Ok) ? ((($Warning) ? ('ok (warning)') : ('ok'))) : ('FAILED')), $Detail) $level
}

# Each needed permission and the application permissions that also grant it.
$needed = [ordered]@{
    'GroupMember.ReadWrite.All' = @('GroupMember.ReadWrite.All', 'Group.ReadWrite.All', 'Directory.ReadWrite.All')
    'Group.Read.All'            = @('Group.Read.All', 'Group.ReadWrite.All', 'Directory.Read.All', 'Directory.ReadWrite.All')
    'User.Read.All'             = @('User.Read.All', 'User.ReadWrite.All', 'Directory.Read.All', 'Directory.ReadWrite.All')
}
$broader = @('Directory.ReadWrite.All', 'Directory.Read.All', 'Group.ReadWrite.All', 'User.ReadWrite.All', 'RoleManagement.ReadWrite.Directory', 'Application.ReadWrite.All')

try{
    Start-IDLRun -ScriptName 'Test-IDLCloudConnection' -ScriptRoot $PSScriptRoot

    $request = Read-IDLRequest
    $config = Get-IDLCloudConfig -Request $request
    Write-IDLLog "Request: $(Get-IDLCloudConfigText -Config $config)"

    # ---- certificate ----
    $certOk = $true
    if ($config.Auth -eq 'certificate'){
        try{
            $cert = Get-IDLCloudCertificate -Thumbprint $config.CertThumbprint
            $key = Get-IDLCloudKeyInfo -Certificate $cert
            $days = [int][math]::Floor(($cert.NotAfter.ToUniversalTime() - [datetime]::UtcNow).TotalDays)
            $keyText = "key provider '$($key.Provider)' ($((($key.IsCsp)) ? ('CSP') : ('CNG')))"
            $detail = "$($cert.Subject), valid until $($cert.NotAfter.ToString('yyyy-MM-dd')) ($days day(s)), $keyText"
            if ($cert.NotAfter.ToUniversalTime() -lt [datetime]::UtcNow){
                $certOk = $false
                Add-Check -Name 'certificate' -Ok $false -Detail "expired: $detail"
            }
            elseif (
                ($config.ExchangeEnabled) -and
                (-not $key.IsCsp)
            ){
                Add-Check -Name 'certificate' -Ok $false -Detail "Exchange Online app-only sign-in needs a CSP key, but this certificate's key is CNG ($detail). Create one with New-IDLCloudCertificate.ps1."
            }
            else{
                Add-Check -Name 'certificate' -Ok $true -Warning ($days -lt 30) -Detail ((($days -lt 30)) ? ("expires soon - renew it and upload the new .cer: $detail") : ($detail))
            }
        }
        catch{
            $certOk = $false
            Add-Check -Name 'certificate' -Ok $false -Detail $_.Exception.Message
        }
    }

    # ---- token and Graph ----
    $token = $null
    if (-not $certOk){
        Add-Check -Name 'token' -Ok $false -Detail 'skipped: the certificate is not usable'
    }
    else{
        try{
            $token = Get-IDLCloudToken -Config $config
            Add-Check -Name 'token' -Ok $true -Detail "signed in to tenant $($config.TenantId) as application $($config.ClientId)"
        }
        catch{
            Add-Check -Name 'token' -Ok $false -Detail $_.Exception.Message
        }
    }

    if ($null -eq $token){
        Add-Check -Name 'graph_permissions' -Ok $false -Detail 'skipped: no token'
        Add-Check -Name 'graph_read' -Ok $false -Detail 'skipped: no token'
    }
    else{
        $claims = ConvertFrom-IDLJwtPayload -Token $token
        $roleList = Get-IDLCloudList -Object $claims -Name 'roles'
        $roles = [string[]]@($roleList | ForEach-Object { [string]$_ })
        $missing = @($needed.Keys | Where-Object { -not @($needed[$_] | Where-Object { $roles -contains $_ }).Count })
        $extra = @($broader | Where-Object { $roles -contains $_ })
        $present = (($roles.Count -gt 0) ? ($roles -join ', ') : ('none'))
        if ($missing.Count -gt 0){
            Add-Check -Name 'graph_permissions' -Ok $false -Detail ("missing application permission(s): {0} (grant them and admin consent; present: {1})" -f ($missing -join ', '), $present)
        }
        elseif ($extra.Count -gt 0){
            Add-Check -Name 'graph_permissions' -Ok $true -Warning $true -Detail ("present: {0}; broader than IDLifecycle needs: {1}" -f $present, ($extra -join ', '))
        }
        else{
            Add-Check -Name 'graph_permissions' -Ok $true -Detail "present: $present"
        }

        try{
            $null = Invoke-IDLGraph -Config $config -Path '/groups?$top=1&$select=id'
            $null = Invoke-IDLGraph -Config $config -Path '/users?$top=1&$select=id'
            Add-Check -Name 'graph_read' -Ok $true -Detail 'groups and users can be read'
        }
        catch{
            Add-Check -Name 'graph_read' -Ok $false -Detail $_.Exception.Message
        }
    }

    # ---- Exchange Online ----
    if ($config.ExchangeEnabled){
        $module = $null
        try{
            $module = Import-IDLExchangeModule
            Add-Check -Name 'exchange_module' -Ok $true -Detail "ExchangeOnlineManagement $($module.Version)"
        }
        catch{
            Add-Check -Name 'exchange_module' -Ok $false -Detail $_.Exception.Message
        }

        $connected = $false
        if ($null -eq $module){
            Add-Check -Name 'exchange_connect' -Ok $false -Detail 'skipped: the module is missing'
        }
        elseif (-not $certOk){
            Add-Check -Name 'exchange_connect' -Ok $false -Detail 'skipped: the certificate is not usable'
        }
        else{
            try{
                Connect-IDLExchange -Config $config
                $connected = $true
                $null = @(Get-Recipient -ResultSize 1 -ErrorAction Stop)
                Add-Check -Name 'exchange_connect' -Ok $true -Detail "connected to $($config.Organization)"
            }
            catch{
                Add-Check -Name 'exchange_connect' -Ok $false -Detail $_.Exception.Message
            }
        }

        if (-not $connected){
            Add-Check -Name 'exchange_permissions' -Ok $false -Detail 'skipped: not connected'
        }
        else{
            $missingCommands = @('Get-DistributionGroupMember', 'Add-DistributionGroupMember', 'Remove-DistributionGroupMember' |
                    Where-Object { $null -eq (Get-Command -Name $_ -ErrorAction SilentlyContinue) })
            if ($missingCommands.Count -gt 0){
                Add-Check -Name 'exchange_permissions' -Ok $false -Detail ("the app's Exchange role does not allow {0}: add its service principal to a role group with the Distribution Groups role (and Security Group Creation and Membership for mail-enabled security groups), or assign Exchange Recipient Administrator" -f ($missingCommands -join ', '))
            }
            else{
                Add-Check -Name 'exchange_permissions' -Ok $true -Detail 'distribution group membership cmdlets are available'
            }
        }
    }

    $healthy = (@($checks | Where-Object { -not $_.ok }).Count -eq 0)
    Add-IDLCounter -Name 'Total' -By $checks.Count
    Add-IDLCounter -Name 'Failed' -By @($checks | Where-Object { -not $_.ok }).Count
    $response = [ordered]@{ ok = $true; healthy = $healthy; checks = [object[]]$checks.ToArray() }
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
    Disconnect-IDLExchange
    Stop-IDLRun -ExitCode $exitCode
}

Write-IDLResponse -Response $response
exit $exitCode
