<#
.SYNOPSIS
    FAKE 'ExchangeOnlineManagement' module (version 99.0.0) used ONLY by the IDLifecycle Pester tests.

.DESCRIPTION
    Implements the cmdlets the IDLifecycle cloud scripts use against a JSON state file whose path is in the
    environment variable IDL_STUB_EXO_STATE (each script run is a separate process, so every cmdlet reads the file and
    mutating cmdlets write it back). Throws on import when IDL_STUB_EXO_STATE is not set.

    State file schema:
    {
      "recipients": [ { "edoid": "<Entra object id>", "guid": "<Exchange guid>", "name": "Finance DL",
                        "smtp": "finance@contoso.test", "type": "MailUniversalDistributionGroup|UserMailbox|..." } ],
      "members": { "<group edoid>": ["<user edoid>"] },
      "failConnect": false,                 // Connect-ExchangeOnline throws (sign-in failure)
      "missingCommands": ["Add-DistributionGroupMember"],   // not exported: the app's RBAC role lacks them
      "faults": [ { "cmdlet": "Add-DistributionGroupMember", "match": "<substring of identity/member>", "message": "boom" } ],
      "calls": [ ... appended: {cmdlet, args, result} for Connect/Disconnect and every Add/Remove attempt ]
    }
#>

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

if ([string]::IsNullOrEmpty($env:IDL_STUB_EXO_STATE)){
    throw 'Stub ExchangeOnlineManagement refuses to load: IDL_STUB_EXO_STATE is not set.'
}

$script:Utf8 = [System.Text.UTF8Encoding]::new($false)
$script:Connected = $false

function Read-StubExoState {
    ConvertFrom-Json -InputObject ([System.IO.File]::ReadAllText($env:IDL_STUB_EXO_STATE, $script:Utf8)) -AsHashtable -Depth 20
}

function Write-StubExoState($State){
    [System.IO.File]::WriteAllText($env:IDL_STUB_EXO_STATE, (ConvertTo-Json -InputObject $State -Depth 20), $script:Utf8)
}

function Add-StubExoCall($State, [string]$Cmdlet, [hashtable]$CallArgs, [string]$Result){
    if ($null -eq $State['calls']){ $State['calls'] = @() }
    $State['calls'] = @(@($State['calls']) + [ordered]@{ cmdlet = $Cmdlet; args = $CallArgs; result = $Result })
}

function Assert-StubExoConnected {
    if (-not $script:Connected){
        throw 'There is no active Exchange Online session. Run Connect-ExchangeOnline first.'
    }
}

function Find-StubExoRecipient($State, [string]$Identity){
    foreach ($r in @($State['recipients'])){
        foreach ($key in @('edoid', 'guid', 'name', 'smtp')){
            if (([string]$r[$key]) -ieq $Identity){ return $r }
        }
    }
    return $null
}

function ConvertTo-StubExoRecipient($R){
    [pscustomobject]@{
        Identity                  = [string]$R['name']
        Name                      = [string]$R['name']
        Guid                      = [guid][string]$R['guid']
        ExternalDirectoryObjectId = [string]$R['edoid']
        PrimarySmtpAddress        = [string]$R['smtp']
        RecipientTypeDetails      = [string]$R['type']
    }
}

function Invoke-StubExoFault($State, [string]$Cmdlet, [string]$Probe){
    foreach ($f in @($State['faults'])){
        if ($null -eq $f){ continue }
        if (([string]$f['cmdlet'] -eq $Cmdlet) -and ($Probe.IndexOf([string]$f['match'], [System.StringComparison]::OrdinalIgnoreCase) -ge 0)){
            throw ([string]$f['message'])
        }
    }
}

function Connect-ExchangeOnline {
    [CmdletBinding()]
    param(
        [string]$AppId, [System.Security.Cryptography.X509Certificates.X509Certificate2]$Certificate, [string]$CertificateThumbprint,
        [string]$Organization, [string]$ExchangeEnvironmentName = 'O365Default', [switch]$ShowBanner, [switch]$SkipLoadingFormatData,
        [string[]]$CommandName
    )

    $state = Read-StubExoState
    $callArgs = @{
        AppId = $AppId; Organization = $Organization; ExchangeEnvironmentName = $ExchangeEnvironmentName
        CertificateObjectThumbprint = (($null -ne $Certificate) ? ($Certificate.Thumbprint) : (''))
        CertificateHasPrivateKey = (($null -ne $Certificate) ? ($Certificate.HasPrivateKey) : ($false))
        CertificateThumbprint = $CertificateThumbprint; CommandName = @($CommandName)
    }
    if ([bool]$state['failConnect']){
        Add-StubExoCall $state 'Connect-ExchangeOnline' $callArgs 'error'
        Write-StubExoState $state
        throw 'AADSTS700027: Client assertion failed signature validation. (stub)'
    }

    Add-StubExoCall $state 'Connect-ExchangeOnline' $callArgs 'ok'
    Write-StubExoState $state
    $script:Connected = $true
}

function Disconnect-ExchangeOnline {
    [CmdletBinding(SupportsShouldProcess)]
    param()

    $state = Read-StubExoState
    Add-StubExoCall $state 'Disconnect-ExchangeOnline' @{} 'ok'
    Write-StubExoState $state
    $script:Connected = $false
}

function Get-Recipient {
    [CmdletBinding()]
    param([string]$Identity, [string]$Filter, $ResultSize = 1000)

    Assert-StubExoConnected
    $state = Read-StubExoState
    $all = @($state['recipients'] | Where-Object { $null -ne $_ })
    if ($Identity){
        $all = @($all | Where-Object { $r = $_; @('edoid', 'guid', 'name', 'smtp') | Where-Object { ([string]$r[$_]) -ieq $Identity } })
    }

    if ($Filter){
        if ($Filter -notmatch "^ExternalDirectoryObjectId -eq '([0-9a-fA-F-]{36})'$"){
            throw "Stub Get-Recipient does not support the filter: $Filter"
        }

        $want = $Matches[1]
        $all = @($all | Where-Object { ([string]$_['edoid']) -ieq $want })
    }

    $limit = (($ResultSize -eq 'Unlimited') ? ([int]::MaxValue) : ([int]$ResultSize))
    return @($all | Select-Object -First $limit | ForEach-Object { ConvertTo-StubExoRecipient $_ })
}

function Get-DistributionGroupMember {
    [CmdletBinding()]
    param([Parameter(Mandatory)][string]$Identity, $ResultSize = 1000)

    Assert-StubExoConnected
    $state = Read-StubExoState
    $group = Find-StubExoRecipient $state $Identity
    if ($null -eq $group){ throw "The operation couldn't be performed because object '$Identity' couldn't be found." }
    $ids = @($state['members'][[string]$group['edoid']])
    return @($ids | Where-Object { $_ } | ForEach-Object { $r = Find-StubExoRecipient $state $_; if ($r){ ConvertTo-StubExoRecipient $r } })
}

function Set-StubExoMembership {
    param([string]$Cmdlet, [string]$Identity, [string]$Member, [bool]$Bypass, [bool]$Add)

    Assert-StubExoConnected
    $state = Read-StubExoState
    $callArgs = @{ Identity = $Identity; Member = $Member; BypassSecurityGroupManagerCheck = $Bypass }
    try{
        Invoke-StubExoFault $state $Cmdlet "$Identity|$Member"
        $group = Find-StubExoRecipient $state $Identity
        if ($null -eq $group){ throw "The operation couldn't be performed because object '$Identity' couldn't be found." }
        $user = Find-StubExoRecipient $state $Member
        if ($null -eq $user){ throw "Couldn't find object ""$Member"". Please make sure that it was spelled correctly or specify a different object." }
        if ($null -eq $state['members']){ $state['members'] = @{} }
        $key = [string]$group['edoid']
        $current = @(@($state['members'][$key]) | Where-Object { $_ })
        $uid = [string]$user['edoid']
        if ($Add){
            if ($current -contains $uid){ throw "The recipient ""$($user['name'])"" is already a member of the group ""$($group['name'])""." }
            $state['members'][$key] = @($current + $uid)
        }
        else{
            if ($current -notcontains $uid){ throw "The recipient ""$($user['name'])"" isn't a member of the group ""$($group['name'])""." }
            $state['members'][$key] = @($current | Where-Object { $_ -ne $uid })
        }

        Add-StubExoCall $state $Cmdlet $callArgs 'ok'
        Write-StubExoState $state
    }
    catch{
        $fresh = Read-StubExoState
        Add-StubExoCall $fresh $Cmdlet $callArgs ('error: ' + $_.Exception.Message)
        Write-StubExoState $fresh
        throw
    }
}

function Add-DistributionGroupMember {
    [CmdletBinding(SupportsShouldProcess)]
    param([Parameter(Mandatory)][string]$Identity, [Parameter(Mandatory)][string]$Member, [switch]$BypassSecurityGroupManagerCheck)

    Set-StubExoMembership -Cmdlet 'Add-DistributionGroupMember' -Identity $Identity -Member $Member -Bypass $BypassSecurityGroupManagerCheck.IsPresent -Add $true
}

function Remove-DistributionGroupMember {
    [CmdletBinding(SupportsShouldProcess)]
    param([Parameter(Mandatory)][string]$Identity, [Parameter(Mandatory)][string]$Member, [switch]$BypassSecurityGroupManagerCheck)

    Set-StubExoMembership -Cmdlet 'Remove-DistributionGroupMember' -Identity $Identity -Member $Member -Bypass $BypassSecurityGroupManagerCheck.IsPresent -Add $false
}

# The app's Exchange role decides which cmdlets a real session gets; missingCommands models a role without them.
$missing = @((Read-StubExoState)['missingCommands'] | Where-Object { $_ })
$exported = @('Connect-ExchangeOnline', 'Disconnect-ExchangeOnline', 'Get-Recipient', 'Get-DistributionGroupMember',
    'Add-DistributionGroupMember', 'Remove-DistributionGroupMember') | Where-Object { $missing -notcontains $_ }
Export-ModuleMember -Function $exported
