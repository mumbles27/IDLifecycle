# Test hook for Install-IDLifecycle.ps1 / Uninstall-IDLifecycle.ps1 (passed as -TestHook; dot-sourced by the script
# AFTER its own system-touching functions are defined, so these definitions replace them).
#
# Everything a real run would do to the machine - services, sc.exe, machine environment variables, event log
# sources, elevation, AD module / gMSA checks, ODBC drivers, the HTTP probe, Install-IDLifecycleService.ps1 - is
# simulated against the JSON file named by $env:IDL_FAKE_STATE. idlifecycled.exe is replaced by FakeDaemon.ps1
# unless $env:IDL_FAKE_REAL_DAEMON is '1' (end-to-end test with a freshly built binary in a temp folder).
#
# State file: { elevated, adModule, gmsaOk, credentialOk, odbcDrivers[], services{ name: {State, PathName, StartName} },
#               eventSources[], machineEnv{}, httpStatus, installerExit, admins, checkFail{ name: detail },
#               checkSkip{ name: detail }, checkOmit[], echoPassword, echoDsn, ownerOverrides{ path: sid }, installAcl[],
#               lockdownExit, lockedDown[], startState, githubSource, githubFail, goVersion, goBuildFail,
#               handOffExit, calls[] }

$script:FakeStatePath = $env:IDL_FAKE_STATE
$script:FakeDaemonPath = Join-Path -Path $PSScriptRoot -ChildPath 'FakeDaemon.ps1'
if ([string]::IsNullOrEmpty($script:FakeStatePath)){
    throw 'SetupFakes.ps1: IDL_FAKE_STATE is not set; refusing to run with real system functions.'
}

# Setup logs go to a temp folder (never %ProgramData%), not locked (a SYSTEM/Administrators-only folder would lock
# out the non-elevated test user).
if ([string]::IsNullOrEmpty($env:IDL_SETUP_TEST_LOGDIR)){
    throw 'SetupFakes.ps1: IDL_SETUP_TEST_LOGDIR is not set.'
}

$script:SetupLogRoot = $env:IDL_SETUP_TEST_LOGDIR
$script:SetupLogLock = $false
$script:SetupLogOverride = $true

# Owners cannot be set to another account without elevation, so tests simulate them: state ownerOverrides maps a
# full path (case-insensitive) to an owner SID; everything else reports its real owner.
function Get-IDLPathOwnerSid {
    param([string]$Path)

    $overrides = (Read-FakeState)['ownerOverrides']
    if ($null -ne $overrides){
        $full = [System.IO.Path]::GetFullPath($Path).TrimEnd('\')
        foreach ($key in $overrides.Keys){
            if ([System.IO.Path]::GetFullPath([string]$key).TrimEnd('\').Equals($full, [System.StringComparison]::OrdinalIgnoreCase)){
                return [string]$overrides[$key]
            }
        }
    }

    return (Get-IDLOwnerSid -Path $Path)
}

# The real InstallDir ACL (SYSTEM/Administrators/TrustedInstaller FullControl, Users read) would lock the
# non-elevated test user out of its own temp folder; New-IDLInstallDirSecurity is unit-tested instead.
function Set-IDLInstallDirAcl {
    param([string]$Path, [string]$ServiceAccount)

    Add-FakeCall -Op 'installDirAcl' -Data @{ path = $Path; account = $ServiceAccount }
    $s = Read-FakeState
    $s['installAcl'] = @(@($s['installAcl']) + @($Path))
    Save-FakeState -State $s
}

function Test-IDLInstallDirAcl {
    param([string]$Path, [string]$ServiceAccount)

    return (@((Read-FakeState)['installAcl']) -contains $Path)
}

function Get-IDLSetupDaemonServices {
    $services = (Read-FakeState)['services']
    return [string[]]@($services.Keys | Where-Object { [string]$services[$_]['PathName'] -match 'idlifecycled\.exe' })
}

function Read-FakeState {
    return ([System.IO.File]::ReadAllText($script:FakeStatePath) | ConvertFrom-Json -AsHashtable)
}

function Save-FakeState {
    param($State)

    [System.IO.File]::WriteAllText($script:FakeStatePath, (ConvertTo-Json -InputObject $State -Depth 20), [System.Text.UTF8Encoding]::new($false))
}

function Add-FakeCall {
    param([string]$Op, $Data = $null)

    $s = Read-FakeState
    $s['calls'] = @($s['calls']) + @(@{ op = $Op; data = $Data })
    Save-FakeState -State $s
}

function Test-IDLSetupElevated {
    return [bool](Read-FakeState)['elevated']
}

function Test-IDLSetupAdModule {
    return [bool](Read-FakeState)['adModule']
}

function Install-IDLSetupFeature {
    param([string]$Name)

    Add-FakeCall -Op 'installFeature' -Data $Name
    $s = Read-FakeState
    $s['adModule'] = $true
    Save-FakeState -State $s
}

function Test-IDLSetupGmsa {
    param([string]$SamAccountName)

    Add-FakeCall -Op 'testGmsa' -Data $SamAccountName
    return [bool](Read-FakeState)['gmsaOk']
}

# state credentialOk: true / false, or 'throw' (no DC reachable). Only the password's length is recorded.
function Test-IDLSetupCredential {
    param([string]$Account, [securestring]$Password)

    Add-FakeCall -Op 'testCredential' -Data @{ account = $Account; passwordLength = $Password.Length }
    $ok = (Read-FakeState)['credentialOk']
    if ([string]$ok -eq 'throw'){
        throw 'The server could not be contacted.'
    }

    return [bool]$ok
}

function Get-IDLSetupOdbcDrivers {
    return [string[]]@((Read-FakeState)['odbcDrivers'])
}

function Get-IDLSetupService {
    param([string]$Name)

    $services = (Read-FakeState)['services']
    if (
        ($null -eq $services) -or
        (-not $services.Contains($Name))
    ){
        return $null
    }

    $svc = $services[$Name]
    return [pscustomobject]@{
        Name      = $Name
        State     = [string]$svc['State']
        PathName  = [string]$svc['PathName']
        StartName = [string]$svc['StartName']
    }
}

function Set-FakeServiceState {
    param([string]$Name, [string]$State)

    $s = Read-FakeState
    $s['services'][$Name]['State'] = $State
    Save-FakeState -State $s
}

function Stop-IDLSetupService {
    param([string]$Name)

    Add-FakeCall -Op 'stop' -Data $Name
    Set-FakeServiceState -Name $Name -State 'Stopped'
}

function Start-IDLSetupService {
    param([string]$Name)

    Add-FakeCall -Op 'start' -Data $Name
    $target = [string](Read-FakeState)['startState']
    Set-FakeServiceState -Name $Name -State ((($target) ? ($target) : ('Running')))
}

function Remove-IDLSetupService {
    param([string]$Name)

    Add-FakeCall -Op 'delete' -Data $Name
    $s = Read-FakeState
    $null = $s['services'].Remove($Name)
    Save-FakeState -State $s
}

if ($env:IDL_FAKE_REAL_DAEMON -ne '1'){
    function Get-IDLSetupDaemonCommand {
        param([string]$Exe)

        return @{
            FileName  = [System.Environment]::ProcessPath
            Arguments = @('-NoProfile', '-NonInteractive', '-ExecutionPolicy', 'Bypass', '-File', $script:FakeDaemonPath, $Exe)
        }
    }

    # The fake `lockdown` records the folder instead of changing ACLs (a real lockdown would lock the test user out).
    function Test-IDLDirectoryLockedDown {
        param([string]$Path, [string]$ServiceAccount)

        return (@((Read-FakeState)['lockedDown']) -contains $Path)
    }
}

function Invoke-IDLServiceInstaller {
    param([string]$ScriptPath, [string[]]$Arguments, [hashtable]$Environment = @{})

    # The service account password is recorded as present + length only.
    $servicePassword = [string]$Environment['IDL_SERVICE_PASSWORD']
    Add-FakeCall -Op 'serviceInstaller' -Data @{
        script                = $ScriptPath
        args                  = @($Arguments)
        envNames              = @($Environment.Keys | Sort-Object)
        servicePasswordLength = $servicePassword.Length
    }
    $s = Read-FakeState
    $code = [int]$s['installerExit']
    $map = @{}
    for ($i = 0; $i -lt $Arguments.Count; $i++){
        if (
            ($Arguments[$i] -like '-*') -and
            ($i + 1 -lt $Arguments.Count) -and
            ($Arguments[$i + 1] -notlike '-*')
        ){
            $map[$Arguments[$i].TrimStart('-')] = $Arguments[$i + 1]
        }
    }

    $dry = $Arguments -contains '-DryRun'
    $name = $map['ServiceName']
    $stamp = '2026-01-01 00:00:00'
    $lines = @("$stamp [INFO] Script start: Install-IDLifecycleService (fake)")
    if ($code -ne 0){
        $lines += "$stamp [ERROR] FATAL: fake installer failure"
    }
    elseif ($s['services'].Contains($name)){
        $lines += "$stamp [INFO] SKIP: ConfigureService $name (already configured)"
    }
    elseif ($dry){
        $lines += "$stamp [INFO] WOULD: CreateService $name"
    }
    else{
        $s['services'][$name] = @{
            State     = 'Stopped'
            PathName  = ('"{0}" service -config "{1}"' -f $map['BinaryPath'], $map['ConfigPath'])
            StartName = $map['ServiceAccount']
        }
        Save-FakeState -State $s
        $lines += "$stamp [INFO] DONE: CreateService $name"
    }

    return [pscustomobject]@{ ExitCode = $code; Lines = [string[]]$lines }
}

function Get-IDLMachineEnvVar {
    param([string]$Name)

    $envs = (Read-FakeState)['machineEnv']
    return ((($null -ne $envs) -and $envs.Contains($Name)) ? ([string]$envs[$Name]) : ($null))
}

function Set-IDLMachineEnvVar {
    param([string]$Name, [string]$Value)

    Add-FakeCall -Op 'setMachineEnv' -Data $Name
    $s = Read-FakeState
    $s['machineEnv'][$Name] = $Value
    Save-FakeState -State $s
}

function Remove-IDLMachineEnvVar {
    param([string]$Name)

    Add-FakeCall -Op 'removeMachineEnv' -Data $Name
    $s = Read-FakeState
    $null = $s['machineEnv'].Remove($Name)
    Save-FakeState -State $s
}

function Test-IDLEventSource {
    param([string]$Source)

    return (@((Read-FakeState)['eventSources']) -contains $Source)
}

function New-IDLEventSource {
    param([string]$Source)

    Add-FakeCall -Op 'newEventSource' -Data $Source
    $s = Read-FakeState
    $s['eventSources'] = @($s['eventSources']) + @($Source)
    Save-FakeState -State $s
}

function Remove-IDLEventSource {
    param([string]$Source)

    Add-FakeCall -Op 'removeEventSource' -Data $Source
    $s = Read-FakeState
    $s['eventSources'] = @(@($s['eventSources']) | Where-Object { $_ -ne $Source })
    Save-FakeState -State $s
}

function Invoke-IDLHttpProbe {
    param([string]$Url, [switch]$SkipCertificateCheck)

    Add-FakeCall -Op 'probe' -Data @{ url = $Url; skipCert = [bool]$SkipCertificateCheck }
    return [int](Read-FakeState)['httpStatus']
}

function Read-IDLSetupText {
    param([string]$Prompt)

    throw "SetupFakes: unexpected prompt '$Prompt'"
}

# -FromGitHub: state githubSource = a folder that stands for the repository (zipped WITH its folder name, like
# GitHub's zipball); githubFail = an error message to throw. Only the token's length is recorded.
function Invoke-IDLSetupGitHubDownload {
    param([string]$Repo, [string]$Ref, [string]$Token = '', [string]$OutFile)

    Add-FakeCall -Op 'githubDownload' -Data @{ repo = $Repo; ref = $Ref; tokenLength = $Token.Length }
    $s = Read-FakeState
    if ($s['githubFail']){
        throw (New-IDLException -Kind Dependency -Message ([string]$s['githubFail']))
    }

    Compress-Archive -LiteralPath ([string]$s['githubSource']) -DestinationPath $OutFile
    return '0123456789abcdef0123456789abcdef01234567'
}

# state goVersion: 'go1.27.3', or '' / missing = Go not installed.
function Get-IDLSetupGoVersion {
    $v = [string](Read-FakeState)['goVersion']
    return (($v) ? ($v) : ($null))
}

function Invoke-IDLSetupGoBuild {
    param([string]$SourceRoot, [string]$OutExe, [string]$Version)

    Add-FakeCall -Op 'goBuild' -Data @{ source = $SourceRoot; version = $Version }
    if ((Read-FakeState)['goBuildFail']){
        throw (New-IDLException -Kind Dependency -Message 'go build failed (exit code 1): fake compile error')
    }

    [System.IO.File]::WriteAllText($OutExe, "FAKE-IDLIFECYCLED $Version", [System.Text.UTF8Encoding]::new($false))
}

# The staged installer cannot load this hook (it does not sit next to tests\powershell), so the hand-off is
# simulated: it records the arguments, which secrets were passed (names + lengths only), and checks the staged
# release with the installer's own Get-IDLTrustedRelease. state handOffExit = the exit code to return.
function Invoke-IDLSetupHandOff {
    param([string]$ScriptPath, [string[]]$Arguments, [hashtable]$Environment = @{})

    $source = $Arguments[[array]::IndexOf($Arguments, '-SourceDir') + 1]
    $release = Get-IDLTrustedRelease -Root $source
    $lengths = @{}
    foreach ($k in $Environment.Keys){
        $lengths[$k] = ([string]$Environment[$k]).Length
    }

    Add-FakeCall -Op 'handOff' -Data @{
        script         = $ScriptPath
        args           = @($Arguments)
        secretLengths  = $lengths
        releaseFiles   = @($release.Files | ForEach-Object { $_.Relative })
        exeText        = [System.IO.File]::ReadAllText($release.Exe)
    }
    return [int](Read-FakeState)['handOffExit']
}

function Read-IDLSetupPassword {
    param([string]$Prompt)

    throw "SetupFakes: unexpected password prompt '$Prompt'"
}

# Tray / Start menu shortcuts and the branding folder: never the real Start menu or %ProgramData%. Shortcuts live in
# state shortcuts{ path: {Target, Arguments, Icon, WorkingDirectory} }; the roots are folders below the test log dir.
# state brandingUntrusted = the reason Get-IDLSetupBrandingProblem returns for an existing branding folder ('' = fine).
$script:FakeTrayRoot = Join-Path -Path (Split-Path -Parent $env:IDL_FAKE_STATE) -ChildPath 'desktop' # per fixture: re-runs see the same folders

function Get-IDLSetupShortcutRoots {
    return @{ Startup = (Join-Path -Path $script:FakeTrayRoot -ChildPath 'Startup'); Programs = (Join-Path -Path $script:FakeTrayRoot -ChildPath 'Programs\IDLifecycle') }
}

function Get-IDLSetupShortcut {
    param([string]$Path)

    $all = (Read-FakeState)['shortcuts']
    if (
        ($null -eq $all) -or
        (-not $all.Contains($Path))
    ){
        return $null
    }

    return $all[$Path]
}

function New-IDLSetupShortcut {
    param([string]$Path, [string]$Target, [string]$Arguments = '', [string]$WorkingDirectory = '', [string]$Icon = '', [string]$Description = '')

    Add-FakeCall -Op 'newShortcut' -Data @{ path = $Path; target = $Target; arguments = $Arguments; icon = $Icon }
    $s = Read-FakeState
    if ($null -eq $s['shortcuts']){
        $s['shortcuts'] = @{}
    }

    $s['shortcuts'][$Path] = @{ Target = $Target; Arguments = $Arguments; Icon = $Icon; WorkingDirectory = $WorkingDirectory }
    Save-FakeState -State $s
}

function Test-IDLSetupDesktopSession {
    return (-not [bool](Read-FakeState)['noDesktop'])
}

function Test-IDLSetupTrayRunning {
    return [bool](Read-FakeState)['trayRunning']
}

function Start-IDLSetupTrayNow {
    param([string]$Shortcut)

    Add-FakeCall -Op 'startTray' -Data $Shortcut
    $s = Read-FakeState
    $s['trayRunning'] = $true
    Save-FakeState -State $s
}

function Remove-IDLSetupShortcut {
    param([string]$Path)

    Add-FakeCall -Op 'removeShortcut' -Data $Path
    $s = Read-FakeState
    $null = $s['shortcuts'].Remove($Path)
    Save-FakeState -State $s
}

function Get-IDLSetupBrandingRoot {
    return (Join-Path -Path $script:FakeTrayRoot -ChildPath 'IDLifecycle-Branding')
}

function Get-IDLSetupBrandingProblem {
    param([string]$Path)

    $why = [string](Read-FakeState)['brandingUntrusted']
    return (($why) ? ($why) : ($null))
}

function Initialize-IDLSetupBrandingDir {
    param([string]$Path)

    Add-FakeCall -Op 'brandingDir' -Data $Path
    $null = New-Item -ItemType Directory -Path $Path -Force
    [System.IO.File]::WriteAllText((Join-Path -Path $Path -ChildPath 'README.txt'), (Get-IDLBrandingReadme), [System.Text.UTF8Encoding]::new($false))
}

function Remove-IDLSetupBrandingDir {
    param([string]$Path)

    Add-FakeCall -Op 'removeBrandingDir' -Data $Path
    Remove-Item -LiteralPath $Path -Recurse -Force
}
