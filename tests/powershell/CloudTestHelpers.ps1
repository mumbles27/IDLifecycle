# Shared helpers for the Pester (3.x) tests of the cloud scripts (Graph / Exchange Online). Dot-source AFTER TestHelpers.ps1.
#
# SAFETY: the scripts run in a child pwsh with IDL_CLOUD_TEST=1 and their login / Graph base URLs pointed at FakeGraph.ps1
# on 127.0.0.1 (the cloud module refuses any other override), and with a PSModulePath of ONLY StubAD + StubEXO, so the
# ExchangeOnlineManagement module they load is the stub (proved by Assert-StubExoEnvironment). Test certificates are
# created in CurrentUser\My and removed again; the real tenant is never contacted.

$script:StubExoDir = Join-Path $script:TestsRoot 'StubEXO'
$script:FakeGraphScript = Join-Path $script:TestsRoot 'FakeGraph.ps1'
$script:ExoPreflightDone = $false
$script:TestTenant = '11111111-2222-3333-4444-555555555555'
$script:TestClient = 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee'
$script:TestSecret = 'Sup3r-Secret-Value~xyz'
$script:AllRoles = @('GroupMember.ReadWrite.All', 'Group.Read.All', 'User.Read.All')

function Get-CloudModulePath {
    return ($script:StubDir + [System.IO.Path]::PathSeparator + $script:StubExoDir)
}

function Assert-StubExoEnvironment {
    if ($script:ExoPreflightDone){
        return
    }

    $exo = New-ExoStateFile
    $probe = 'Import-Module ExchangeOnlineManagement -ErrorAction Stop; $m = Get-Module ExchangeOnlineManagement; "$($m.Version)|$($m.Path)"'
    $psi = New-ChildProcessInfo -Arguments @('-NoProfile', '-NonInteractive', '-Command', $probe) -StatePath (New-StubStateFile) `
        -ExtraEnv @{ PSModulePath = (Get-CloudModulePath); IDL_STUB_EXO_STATE = $exo }
    $r = Invoke-ChildProcess -Psi $psi -TimeoutSec 60
    $parts = ([string]$r.StdOutRaw).Trim().Split('|')
    if (
        ($r.ExitCode -ne 0) -or
        ($parts[0] -ne '99.0.0') -or
        (-not ([string]$parts[1]).StartsWith($script:StubExoDir, [System.StringComparison]::OrdinalIgnoreCase))
    ){
        throw "SAFETY ABORT: ExchangeOnlineManagement in the child resolved to '$($r.StdOutRaw)' (exit $($r.ExitCode)), not the stub. $($r.StdErr)"
    }

    $script:ExoPreflightDone = $true
}

function New-TestCertificate {
    # A self-signed cert in CurrentUser\My: CSP (as New-IDLCloudCertificate makes) or CNG. Returns {Thumbprint; CerPath}.
    param([switch]$Cng, [int]$Days = 365)

    $null = New-Item -ItemType Directory -Force -Path $script:StateDir
    $params = @{
        Subject           = "CN=IDL-Pester-$([guid]::NewGuid().ToString('N').Substring(0, 8))"
        CertStoreLocation = 'Cert:\CurrentUser\My'
        KeyAlgorithm      = 'RSA'
        KeyLength         = 2048
        KeyExportPolicy   = 'NonExportable'
        NotAfter          = (Get-Date).AddDays($Days)
    }
    if (-not $Cng){
        $params.Provider = 'Microsoft Enhanced RSA and AES Cryptographic Provider'
        $params.KeySpec = 'KeyExchange'
    }

    $cert = New-SelfSignedCertificate @params
    $cer = Join-Path $script:StateDir ($cert.Thumbprint + '.cer')
    [System.IO.File]::WriteAllBytes($cer, $cert.Export([System.Security.Cryptography.X509Certificates.X509ContentType]::Cert))
    return [pscustomobject]@{ Thumbprint = $cert.Thumbprint; CerPath = $cer }
}

function Remove-TestCertificate {
    param($Certificate)

    if ($null -eq $Certificate){
        return
    }

    $path = "Cert:\CurrentUser\My\$($Certificate.Thumbprint)"
    if (Test-Path -LiteralPath $path){
        Remove-Item -LiteralPath $path -DeleteKey -ErrorAction SilentlyContinue
    }

    Remove-Item -LiteralPath $Certificate.CerPath -Force -ErrorAction SilentlyContinue
}

function Start-FakeGraph {
    $null = New-Item -ItemType Directory -Force -Path $script:StateDir
    $id = [guid]::NewGuid().ToString('N')
    $fake = [pscustomobject]@{
        StatePath = (Join-Path $script:StateDir "graph_$id.json")
        PortFile  = (Join-Path $script:StateDir "graph_$id.port")
        StopFile  = (Join-Path $script:StateDir "graph_$id.stop")
        Port      = 0
        Process   = $null
        Url       = ''
    }
    [System.IO.File]::WriteAllText($fake.StatePath, '{}', $script:Utf8NoBom)

    $psi = New-Object System.Diagnostics.ProcessStartInfo
    $psi.FileName = $script:Pwsh
    foreach ($a in @('-NoProfile', '-NonInteractive', '-ExecutionPolicy', 'Bypass', '-File', $script:FakeGraphScript,
            '-StatePath', $fake.StatePath, '-PortFile', $fake.PortFile, '-ParentPid', [string]$PID, '-StopFile', $fake.StopFile)){
        $psi.ArgumentList.Add($a)
    }

    $psi.UseShellExecute = $false
    $psi.CreateNoWindow = $true
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError = $true
    $fake.Process = [System.Diagnostics.Process]::Start($psi)
    $null = $fake.Process.StandardOutput.ReadToEndAsync()
    $null = $fake.Process.StandardError.ReadToEndAsync()

    $deadline = (Get-Date).AddSeconds(30)
    while (-not (Test-Path -LiteralPath $fake.PortFile)){
        if ((Get-Date) -gt $deadline -or $fake.Process.HasExited){
            throw "FakeGraph did not start (exited: $($fake.Process.HasExited))."
        }

        Start-Sleep -Milliseconds 100
    }

    $fake.Port = [int]([System.IO.File]::ReadAllText($fake.PortFile).Trim())
    $fake.Url = "http://127.0.0.1:$($fake.Port)"
    return $fake
}

function Stop-FakeGraph {
    param($Fake)

    if ($null -eq $Fake){
        return
    }

    [System.IO.File]::WriteAllText($Fake.StopFile, 'stop', $script:Utf8NoBom)
    if (-not $Fake.Process.WaitForExit(5000)){
        # Our own child; it did not see the stop file in time.
        $Fake.Process.Kill()
    }

    foreach ($f in @($Fake.StatePath, $Fake.PortFile, $Fake.StopFile, "$($Fake.StatePath).errors.log")){
        Remove-Item -LiteralPath $f -Force -ErrorAction SilentlyContinue
    }
}

function Set-FakeGraphState {
    param(
        [Parameter(Mandatory)]$Fake,
        [object[]]$Groups = @(),
        [object[]]$Users = @(),
        [string[]]$Roles = $script:AllRoles,
        [object[]]$Faults = @(),
        [string]$CertPath = '',
        [string]$Secret = '',
        [int]$PageSize = 100
    )

    $state = [ordered]@{
        tenant   = $script:TestTenant
        clientId = $script:TestClient
        certPath = $CertPath
        secret   = $Secret
        roles    = @($Roles)
        pageSize = $PageSize
        groups   = @($Groups)
        users    = @($Users)
        faults   = @($Faults)
        calls    = @()
    }
    [System.IO.File]::WriteAllText($Fake.StatePath, (ConvertTo-Json -InputObject $state -Depth 20), $script:Utf8NoBom)
}

function Read-FakeGraphState {
    param([Parameter(Mandatory)]$Fake)

    return (ConvertFrom-Json -InputObject ([System.IO.File]::ReadAllText($Fake.StatePath, $script:Utf8NoBom)))
}

function Get-FakeGraphCalls {
    # Graph calls (token calls excluded unless -IncludeToken), optionally only one HTTP method.
    param([Parameter(Mandatory)]$Fake, [string]$Method = '', [switch]$IncludeToken)

    $calls = @((Read-FakeGraphState -Fake $Fake).calls | Where-Object { $null -ne $_ })
    if (-not $IncludeToken){
        $calls = @($calls | Where-Object { $_.path -notmatch '/oauth2/v2\.0/token' })
    }

    if ($Method){
        $calls = @($calls | Where-Object { $_.method -eq $Method })
    }

    return $calls
}

function New-FakeGroup {
    param(
        [string]$Id, [string]$Name, [ValidateSet('security', 'm365', 'mail_security', 'distribution', 'none')][string]$Kind = 'security',
        [string[]]$Members = @(), [string[]]$MemberGroups = @(), [bool]$RoleAssignable = $false, [bool]$Dynamic = $false,
        [bool]$Synced = $false, [bool]$HideRoleFlag = $false, [string]$Description = ''
    )

    $types = [System.Collections.Generic.List[string]]::new()
    if ($Kind -eq 'm365'){ $types.Add('Unified') }
    if ($Dynamic){ $types.Add('DynamicMembership') }
    return [ordered]@{
        id                    = $Id
        displayName           = $Name
        description           = $Description
        mail                  = ((($Kind -in @('m365', 'mail_security', 'distribution'))) ? (($Name -replace '\W', '').ToLowerInvariant() + '@contoso.test') : ($null))
        mailEnabled           = ($Kind -in @('m365', 'mail_security', 'distribution'))
        securityEnabled       = ($Kind -in @('security', 'mail_security'))
        groupTypes            = @($types)
        isAssignableToRole    = $RoleAssignable
        onPremisesSyncEnabled = ((($Synced)) ? ($true) : ($null))
        members               = @($Members)
        memberGroups          = @($MemberGroups)
        hideRoleFlag          = $HideRoleFlag
    }
}

function New-FakeUser {
    param([string]$Id, [string]$Upn, [string]$Sid = '', [string]$ImmutableId = '', [bool]$Synced = $true, [bool]$Enabled = $true)

    return [ordered]@{
        id                           = $Id
        userPrincipalName            = $Upn
        onPremisesSecurityIdentifier = $Sid
        onPremisesImmutableId        = $ImmutableId
        accountEnabled               = $Enabled
        onPremisesSyncEnabled        = ((($Synced)) ? ($true) : ($null))
    }
}

function New-ExoStateFile {
    param([object[]]$Recipients = @(), [hashtable]$Members = @{}, [bool]$FailConnect = $false, [string[]]$MissingCommands = @(), [object[]]$Faults = @())

    $null = New-Item -ItemType Directory -Force -Path $script:StateDir
    $path = Join-Path $script:StateDir ('exo_' + [guid]::NewGuid().ToString('N') + '.json')
    $state = [ordered]@{
        recipients      = @($Recipients)
        members         = $Members
        failConnect     = $FailConnect
        missingCommands = @($MissingCommands)
        faults          = @($Faults)
        calls           = @()
    }
    [System.IO.File]::WriteAllText($path, (ConvertTo-Json -InputObject $state -Depth 20), $script:Utf8NoBom)
    return $path
}

function Read-ExoStateFile {
    param([string]$Path)

    return (ConvertFrom-Json -InputObject ([System.IO.File]::ReadAllText($Path, $script:Utf8NoBom)))
}

function Get-ExoCalls {
    param([string]$Path, [string]$Cmdlet = '')

    $calls = @((Read-ExoStateFile -Path $Path).calls | Where-Object { $null -ne $_ })
    if ($Cmdlet){
        $calls = @($calls | Where-Object { $_.cmdlet -eq $Cmdlet })
    }

    return $calls
}

function New-ExoRecipient {
    param([string]$Id, [string]$Name, [string]$Type = 'UserMailbox')

    return [ordered]@{ edoid = $Id; guid = [guid]::NewGuid().ToString(); name = $Name; smtp = (($Name -replace '\W', '').ToLowerInvariant() + '@contoso.test'); type = $Type }
}

function New-CloudBlock {
    # The request's "cloud" block: certificate auth by default, -Secret for a client secret, -Exchange to enable Exchange Online.
    param([string]$Thumbprint = '', [switch]$Secret, [switch]$Exchange, [string]$SecretValue = $script:TestSecret)

    $block = [ordered]@{
        environment    = 'global'
        tenantId       = $script:TestTenant
        clientId       = $script:TestClient
        auth           = ((($Secret)) ? ('secret') : ('certificate'))
        certThumbprint = $Thumbprint
        exchange       = [ordered]@{ enabled = [bool]$Exchange; organization = 'contoso.onmicrosoft.com' }
    }
    if ($Secret){
        $block.clientSecret = $SecretValue
    }

    return $block
}

function Invoke-CloudScript {
    <#
      Runs scripts\<Script>.ps1 against FakeGraph (+ the Exchange stub). Same result shape as Invoke-IDLScript.
    #>
    param(
        [Parameter(Mandatory)][string]$Script,
        [Parameter(Mandatory)]$Fake,
        $Request = $null,
        $RawInput = $null,
        [string]$ExoState = '',
        [switch]$DryRun,
        [string[]]$ScriptArguments = @(),
        [hashtable]$ExtraEnv = @{}
    )

    Assert-StubExoEnvironment
    if (-not $ExoState){
        $ExoState = New-ExoStateFile
    }

    $envTable = @{
        PSModulePath             = (Get-CloudModulePath)
        IDL_STUB_EXO_STATE       = $ExoState
        IDL_CLOUD_TEST           = '1'
        IDL_CLOUD_TEST_LOGIN_URL = $Fake.Url
        IDL_CLOUD_TEST_GRAPH_URL = $Fake.Url
    }
    foreach ($k in $ExtraEnv.Keys){
        $envTable[$k] = $ExtraEnv[$k]
    }

    $params = @{ Script = $Script; StatePath = (New-StubStateFile); ExtraEnv = $envTable; DryRun = $DryRun; ScriptArguments = $ScriptArguments }
    if ($null -ne $RawInput){
        $params.RawInput = $RawInput
    }
    else{
        $params.Request = $Request
    }

    return (Invoke-IDLScript @params)
}
