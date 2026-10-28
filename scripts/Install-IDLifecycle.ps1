<#
.SYNOPSIS
    Guided, idempotent full setup of IDLifecycle: program files, config, database, first admin, Windows service, start.

.DESCRIPTION
    Wraps idlifecycled.exe (init / lockdown / user add / check / version) and Install-IDLifecycleService.ps1:
      a. Release trust (before anything from SourceDir is run): SourceDir must be a release folder made by
         New-IDLifecycleRelease.ps1 - idlifecycled.exe at its root, scripts\, and a SHA256SUMS manifest of every shipped
         file. Refused (exit 2, no -Force bypass) when SourceDir, anything below it, or its parent folders can be changed
         by anyone but SYSTEM, Administrators, TrustedInstaller and you (see Get-IDLUntrustedAclReason in the Common
         module for the exact rules), when a file does not match SHA256SUMS, or when scripts\ holds a file the manifest
         does not list. SHA256SUMS lives in the same folder, so it gives INTEGRITY (nothing was swapped or added after
         the release was built), not authenticity - the ACL check is the real guard. A signed exe's signer is reported;
         -RequireSignature refuses an unsigned/invalid one.
         -SkipIntegrityCheck (LEAST SAFE; for testing) skips everything about SHA256SUMS: the files are whatever the folder
         holds (idlifecycled.exe, Setup.cmd, scripts*), their hashes are taken now and written as the installed manifest.
         The folder/parent ACL check, the required files and -RequireSignature still apply.
      b. Preflight: elevated (not for -DryRun), Windows + PowerShell 7, the ActiveDirectory module (RSAT-AD-PowerShell;
         installed only with -InstallPrerequisites), Test-ADServiceAccount for an MSA/gMSA, the password of a user
         service account (checked against AD / the local SAM), the IBM ODBC driver for db2.
         An existing DataDir / config.json / secret.key / database or InstallDir owned by an untrusted account is
         refused (someone else may have pre-created it - delete it if you did not).
      c. Copy the manifest's files (+ SHA256SUMS) to InstallDir; a running service is stopped first and started again
         afterwards (also when a later step fails). install / upgrade / same is reported from `idlifecycled version`;
         replacing files that differ, and removing files in InstallDir\scripts that the manifest does not list, needs
         -Force. Only files that this run verified against the manifest are unblocked. InstallDir then gets a
         protected ACL: SYSTEM, Administrators, TrustedInstaller FullControl; Users and the service account
         ReadAndExecute; owner Administrators (scripts\Logs keeps the service account's Modify grant).
      d. `init` writes <DataDir>\config.json when it does not exist; an existing config.json is never overwritten.
         Its powershell.scripts_dir / data_dir must point at this install (else exit 2; -Force corrects just those two
         fields, before the lockdown - which restricts the folder named in data_dir).
      e. `lockdown -account <ServiceAccount>` restricts the data directory; the "IDLifecycle" event source is registered.
      f. Creates the first admin GUI user when `check` reports none (password only in the child's IDL_PASSWORD).
      g. Install-IDLifecycleService.ps1 (from InstallDir\scripts) registers the service; its failures fail the install.
      h. Every required `check -json` entry must have run and passed (skipped ad_module is fine: the preflight checked
         it); otherwise nothing is started.
      i. Starts the service (unless -Start:$false; restarts it when this run changed its config), waits for Running
         (60 s), probes http(s)://<listen>/ for HTTP 200 and prints the GUI URL and next steps.
    Every step is logged and reported (CSV); a step already in the desired state is Skipped; -DryRun reports WOULD:.

    Server databases (mssql, postgres, mysql, db2) need an existing, empty database and a login allowed to create
    tables; the schema is created by `check`/the service. The connection string (and its password part) is never put
    on a command line or logged.
    Default (recommended): -DbDsn is stored as database.dsn in config.json. `init` gets only a placeholder; the real
    value is written (atomically) after `lockdown` has restricted the data dir to SYSTEM, Administrators, the installing
    user and the service account - the same protection as secret.key - and `check` confirms the ACL afterwards.
    Replacing a different stored DSN needs -Force.
    Opt-in: -DbDsnEnvVar IDL_<NAME> keeps the DSN in a MACHINE environment variable and config.json gets "env:NAME"
    (for teams that inject the variable with their own secret tooling). Machine environment variables are NOT secret
    (BUILTIN\Users can read the registry key and every process on the host receives them) and the Service Control
    Manager only sees a new/changed one after a reboot, so the service is not started in the run that changed it.

.PARAMETER SourceDir
    Release folder made by New-IDLifecycleRelease.ps1 (idlifecycled.exe, scripts\, SHA256SUMS), or that release as a
    .zip (New-IDLifecycleRelease.ps1 -Zip). Default: the parent of this script's folder (i.e. run the installer that sits
    inside the release folder). A zip must sit where only SYSTEM, Administrators, TrustedInstaller and you can change it;
    it is extracted into a protected staging folder and that release's own installer finishes the job (see -FromGitHub).

.PARAMETER FromGitHub
    Get the source from GitHub instead and build it on this server: downloads -GitHubRepo at -GitHubRef (the commit is
    resolved first and reported), builds idlifecycled.exe with Go (go on PATH; module downloads need internet access),
    lays out a release (exe, scripts\, Setup.cmd, SHA256SUMS) in a protected staging folder
    (%ProgramData%\IDLifecycle-Setup\Staging; a non-elevated -DryRun uses %LOCALAPPDATA%), then runs THAT release's
    Install-IDLifecycle.ps1 with the same parameters - so a newer version is installed by its own installer. Its log is a
    separate file in the same log folder. The staging folder is removed when that run succeeds. -DryRun still downloads
    and builds (into staging) but changes nothing else.

.PARAMETER GitHubRepo
    owner/name or https://github.com/owner/name. Default: mumbles27/IDLifecycle.

.PARAMETER GitHubRef
    Branch, tag or commit to build. Default: main.

.PARAMETER GitHubToken
    SecureString GitHub token (fine-grained, read-only 'Contents' on the repository) for a private repository. Falls back
    to the IDL_GITHUB_TOKEN environment variable (removed from this process as soon as it is read). Used only in the
    download requests; never logged, stored or passed on.

.PARAMETER InstallDir
    Program folder. Default: C:\Program Files\IDLifecycle.

.PARAMETER DataDir
    Data folder (config.json, SQLite DB, secret.key, logs). Default: C:\ProgramData\IDLifecycle.

.PARAMETER ServiceAccount
    Account the service runs as. Required; prompted for when missing and the session is interactive.
      - gMSA or standalone MSA (recommended): DOMAIN\name$ - Windows manages the password.
      - Normal domain or local user: DOMAIN\name, .\name or name@domain - needs -ServiceAccountPassword. The password
        is stored by the Service Control Manager; when it expires or changes, re-run with the new one.
      - Built-in: LocalSystem, NT AUTHORITY\NetworkService, NT AUTHORITY\LocalService, NT SERVICE\<name>.
    Domain/local accounts are granted "Log on as a service" locally when they lack it.

.PARAMETER ServiceAccountPassword
    SecureString password of a user service account (not for MSA/gMSA/built-in). Needed when the service is created
    or moved to that account; given on a later run, it updates the stored password. Falls back to the
    IDL_SERVICE_PASSWORD environment variable (removed from this process as soon as it is read), then to an
    interactive prompt. It is checked before anything changes and reaches only the service installer child, through
    its environment - never a command line, log or report.

.PARAMETER ServiceName
    Windows service name. Default: IDLifecycle.

.PARAMETER DbDriver
    sqlite (default), mssql, postgres, mysql or db2. Used only when config.json is created.

.PARAMETER DbDsn
    Connection string (SecureString) for a server database. Stored in config.json (database.dsn) inside the
    restricted data dir - unless -DbDsnEnvVar is given, then it is written to that machine environment variable.
    Falls back to the IDL_SETUP_DB_DSN environment variable (removed from this process as soon as it is read; this
    is how Start-IDLifecycleSetup.ps1 passes it).

.PARAMETER DbDsnEnvVar
    Opt-in: keep the DSN in this machine environment variable instead (must match ^IDL_[A-Za-z0-9_]+$; config.json
    gets "env:NAME"); for teams that inject the variable with their own secret tooling. With -DbDsn the variable is
    created/updated; without it, it must already exist at machine scope. Every local user can read machine variables.

.PARAMETER Listen
    host:port for the GUI (default of init: 127.0.0.1:8443). A non-loopback address needs -TlsCertFile/-TlsKeyFile.

.PARAMETER TlsCertFile
    PEM certificate file (with -TlsKeyFile).

.PARAMETER TlsKeyFile
    PEM private key file (with -TlsCertFile).

.PARAMETER AdminUser
    First GUI administrator. Default: admin.

.PARAMETER AdminPassword
    SecureString, min 12 characters. Falls back to the IDL_PASSWORD environment variable (removed from this
    process's environment as soon as it is read), then to an interactive prompt with confirmation.

.PARAMETER RestoreFrom
    An IDLifecycle backup (.idlbak, made in the GUI under Settings -> Backups or with idlifecycled backup) to load into
    this install: recovery, a move to another server, or a move to another database type. After the data folder is
    locked down and before the readiness check, idlifecycled restore replaces the database's contents with the backup's
    and puts its encryption key (secret.key) in place, so stored secrets keep working; the previous secret.key is kept
    as secret.key.replaced-<time>. A running service is stopped first and started again at the end. A database that
    already has data is only replaced with -Force. The backup's header is checked in the preflight. All GUI sign-ins
    end; the backup's users (and their passwords) apply afterwards, so -AdminPassword is only used when it has no admin.

.PARAMETER RestorePassphrase
    SecureString passphrase of the backup. Falls back to IDL_BACKUP_PASSPHRASE (removed from this process at once),
    then to a prompt. It reaches only the idlifecycled restore child, through its environment.

.PARAMETER RestoreConfig
    With -RestoreFrom: also take the backup's config.json (listen address, TLS files, log level, CSV folders,
    PowerShell settings); data_dir, database and powershell.scripts_dir stay as this install sets them.

.PARAMETER Start
    Start the service at the end (default). Use -Start:$false to only install it.

.PARAMETER RequireSignature
    Refuse an idlifecycled.exe without a valid Authenticode signature.

.PARAMETER SkipIntegrityCheck
    LEAST SAFE - for testing, or a folder you put together yourself (a self-built idlifecycled.exe next to the scripts\n    folder of a checkout) without New-IDLifecycleRelease.ps1. SHA256SUMS is not needed and not read: nothing proves the
    files are the ones that were built or reviewed, so a changed or added script would be installed and later run by the
    service. The installer takes idlifecycled.exe, Setup.cmd (if present) and every file below scripts (Logs excluded),
    hashes them now and writes those hashes as the installed SHA256SUMS (so upgrade and uninstall work as usual). Still
    enforced: the folder and its parents may only be changed by SYSTEM, Administrators, TrustedInstaller and you,
    idlifecycled.exe and every required script must be there, and -RequireSignature. A .zip without SHA256SUMS is accepted
    too (a single top folder in it, as in a GitHub "Download ZIP", is used as the release). Every run with it logs a
    warning and reports IntegrityCheck = Skipped. Use a release built by New-IDLifecycleRelease.ps1 for production.

.PARAMETER NoTray
    Do not add the tray icon (Start-IDLifecycleTray.ps1) to the common Startup folder and the Start menu; existing tray
    shortcuts are removed. Without it, everyone who signs in to this server gets the tray icon (Open admin console,
    service status, start / stop / restart and basic web console settings - each change asks for administrator
    rights). Always: <InstallDir>\tray.json (what the tray shows; readable by Users, no secrets), the Start menu entry
    "IDLifecycle Setup" and the branding folder %ProgramData%\IDLifecycle-Branding (SYSTEM/Administrators full,
    Users read; files in it replace the logo, icons and product name - see its README.txt).

.PARAMETER InstallPrerequisites
    Run Install-WindowsFeature RSAT-AD-PowerShell when the ActiveDirectory module is missing.

.PARAMETER NonInteractive
    Never prompt: missing required input is an error.

.PARAMETER Force
    Allow replacing existing program files that differ (upgrade), removing files in InstallDir\scripts that the
    release does not contain, replacing a different connection string stored in config.json, and correcting
    config.json's scripts_dir / data_dir. It never bypasses the release-trust checks.

.PARAMETER DryRun
    Look at the current state, report WOULD: for every change, change nothing (no elevation needed).

.EXAMPLE
    .\scripts\New-IDLifecycleRelease.ps1 -OutDir C:\Install\IDLifecycle-1.0.0 -Build -Version 1.0.0
    C:\Install\IDLifecycle-1.0.0\scripts\Install-IDLifecycle.ps1 -ServiceAccount 'CORP\svc-idl$' -DryRun

.EXAMPLE
    C:\Install\IDLifecycle-1.0.0\scripts\Install-IDLifecycle.ps1 -ServiceAccount 'CORP\svc-idl$' -DbDriver mssql -DbDsn (Read-Host -AsSecureString 'DSN') -AdminPassword (Read-Host -AsSecureString 'Admin password')

.EXAMPLE
    C:\Install\IDLifecycle-1.2.0\scripts\Install-IDLifecycle.ps1 -ServiceAccount 'CORP\svc-idl$' -Force   # upgrade

.EXAMPLE
    .\scripts\Install-IDLifecycle.ps1 -FromGitHub -GitHubRef v1.2.0 -GitHubToken (Read-Host -AsSecureString 'GitHub token') -ServiceAccount 'CORP\svc-idl$' -Force

.EXAMPLE
    .\scripts\Install-IDLifecycle.ps1 -SourceDir D:\Downloads\IDLifecycle-1.2.0.zip -ServiceAccount 'CORP\svc-idl$'

.NOTES
    Requires PowerShell 7+, Windows and an elevated session (not for -DryRun).
    Exit codes: 0 ok, 1 some steps failed (or a failing check), 2 invalid input / untrusted release, 3 dependency
    missing, 4 not elevated / access denied, 5 unexpected error. A failure of Install-IDLifecycleService.ps1 is
    propagated with its own exit code.
    Logs and CSV report go to %ProgramData%\IDLifecycle-Setup\Logs (created and restricted to SYSTEM and Administrators;
    refused if it is a junction or has an untrusted owner). A non-elevated -DryRun logs to
    %LOCALAPPDATA%\IDLifecycle-Setup\Logs instead. Secrets never appear there.
#>

#Requires -Version 7.0

[CmdletBinding()]
param(
    [string]$SourceDir = '',

    [switch]$FromGitHub,

    [string]$GitHubRepo = 'mumbles27/IDLifecycle',

    [string]$GitHubRef = 'main',

    [securestring]$GitHubToken,

    [string]$InstallDir = (Join-Path -Path $env:ProgramFiles -ChildPath 'IDLifecycle'),

    [string]$DataDir = (Join-Path -Path $env:ProgramData -ChildPath 'IDLifecycle'),

    [string]$ServiceAccount = '',

    [string]$ServiceName = 'IDLifecycle',

    [string]$DbDriver = 'sqlite',

    [securestring]$DbDsn,

    [string]$DbDsnEnvVar = '',

    [string]$Listen = '',

    [string]$TlsCertFile = '',

    [string]$TlsKeyFile = '',

    [string]$AdminUser = 'admin',

    [securestring]$AdminPassword,

    [securestring]$ServiceAccountPassword,

    [string]$RestoreFrom = '',

    [securestring]$RestorePassphrase,

    [switch]$RestoreConfig,

    [switch]$Start = $true,

    [switch]$RequireSignature,

    [switch]$SkipIntegrityCheck,

    [switch]$NoTray,

    [switch]$InstallPrerequisites,

    [switch]$NonInteractive,

    [switch]$Force,

    [switch]$DryRun,

    # Test seam: honoured only when IDL_SETUP_TEST=1 and the file lies in <repo>\tests\powershell (Get-IDLTestHookRefusal).
    [Parameter(DontShow)]
    [string]$TestHook = ''
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

Import-Module (Join-Path $PSScriptRoot 'IDLifecycle.Common.psm1') -Force

$script:FailedSteps = 0
$script:PropagatedExitCode = 0
$script:DaemonEnv = @{}
$script:EventSourceName = 'IDLifecycle'
$script:DsnPlaceholder = 'env:IDL_SETUP_PLACEHOLDER'
$script:ManifestName = 'SHA256SUMS'
$script:StoppedForUpdate = $false
$script:ConfigChanged = $false
$script:SetupLogRoot = Join-Path -Path $env:ProgramData -ChildPath 'IDLifecycle-Setup\Logs'
$script:SetupLogLock = $true
$script:UnverifiedTempDir = $null # -SkipIntegrityCheck: the generated manifest's folder, removed at the end
$script:SetupLogOverride = $false
# Checks of `idlifecycled check -json` that must have run and passed (ad_module is verified by the preflight and
# skipped with -skip-ad-module).
$script:RequiredChecks = @('config', 'data_dir_acl', 'database', 'admin_user', 'powershell', 'scripts', 'scripts_unblocked', 'program_files_acl')
$script:RequiredScripts = @(
    'IDLifecycle.Common.psm1', 'Get-IDLSnapshot.ps1', 'New-IDLUser.ps1', 'Set-IDLUser.ps1',
    'Disable-IDLUser.ps1', 'Enable-IDLUser.ps1', 'Remove-IDLUser.ps1', 'Test-IDLNames.ps1', 'Set-IDLLink.ps1',
    'Install-IDLifecycleService.ps1'
)

#region System-touching functions (replaced by -TestHook in the Pester tests)

function Test-IDLSetupElevated {
    return (Test-IDLProcessElevated)
}

function Test-IDLSetupAdModule {
    return ($null -ne (Get-Module -ListAvailable -Name 'ActiveDirectory' | Select-Object -First 1))
}

function Install-IDLSetupFeature {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Name
    )

    $result = Install-WindowsFeature -Name $Name
    if (-not $result.Success){
        throw "Install-WindowsFeature $Name did not succeed (exit code $($result.ExitCode))."
    }
}

function Test-IDLSetupGmsa {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$SamAccountName
    )

    Import-Module ActiveDirectory -ErrorAction Stop
    return [bool](Test-ADServiceAccount -Identity $SamAccountName)
}

function Test-IDLSetupCredential {
    # True when the password is right for the user account (domain: a DC; '.\name' or this computer's name: the
    # local SAM). One logon attempt, which counts toward the account lockout threshold when it fails.
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Account,
        [Parameter(Mandatory)][securestring]$Password
    )

    Add-Type -AssemblyName System.DirectoryServices.AccountManagement
    if ($Account -match '^([^\\]+)\\(.+)$'){
        $domain = $Matches[1]
        $user = $Matches[2]
    }
    else{
        $domain = $Account.Split('@')[1]
        $user = $Account
    }

    $isLocal = (($domain -eq '.') -or ($domain -ieq $env:COMPUTERNAME))
    $contextType = (($isLocal) ? ([System.DirectoryServices.AccountManagement.ContextType]::Machine) : ([System.DirectoryServices.AccountManagement.ContextType]::Domain))
    $context = (($isLocal) ? ([System.DirectoryServices.AccountManagement.PrincipalContext]::new($contextType)) : ([System.DirectoryServices.AccountManagement.PrincipalContext]::new($contextType, $domain)))
    try{
        return $context.ValidateCredentials($user, ([System.Net.NetworkCredential]::new('', $Password).Password))
    }
    finally{
        $context.Dispose()
    }
}

function Get-IDLSetupOdbcDrivers {
    return [string[]]@(Get-OdbcDriver -Platform '64-bit' | ForEach-Object { [string]$_.Name })
}

function Get-IDLSetupService {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Name
    )

    $svc = Get-CimInstance -ClassName Win32_Service -Filter ("Name='{0}'" -f $Name.Replace("'", "''")) -ErrorAction SilentlyContinue
    if ($null -eq $svc){
        return $null
    }

    return [pscustomobject]@{
        Name      = [string]$svc.Name
        State     = [string]$svc.State
        PathName  = [string]$svc.PathName
        StartName = [string]$svc.StartName
    }
}

function Stop-IDLSetupService {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Name
    )

    Stop-Service -Name $Name -Force
}

function Start-IDLSetupService {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Name
    )

    Start-Service -Name $Name
}

function Get-IDLSetupShortcutRoots {
    # Where the tray / setup shortcuts go: the common Startup folder and an "IDLifecycle" folder in the common Start menu.
    return @{
        Startup  = [System.Environment]::GetFolderPath([System.Environment+SpecialFolder]::CommonStartup)
        Programs = Join-Path -Path ([System.Environment]::GetFolderPath([System.Environment+SpecialFolder]::CommonPrograms)) -ChildPath 'IDLifecycle'
    }
}

function Get-IDLSetupShortcut {
    # The shortcut at Path as @{ Target; Arguments; Icon; WorkingDirectory }, or $null when there is none.
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Path
    )

    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)){
        return $null
    }

    $shell = New-Object -ComObject WScript.Shell
    try{
        $lnk = $shell.CreateShortcut($Path)
        return @{ Target = [string]$lnk.TargetPath; Arguments = [string]$lnk.Arguments; Icon = [string]$lnk.IconLocation; WorkingDirectory = [string]$lnk.WorkingDirectory }
    }
    finally{
        $null = [System.Runtime.InteropServices.Marshal]::ReleaseComObject($shell)
    }
}

function New-IDLSetupShortcut {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Path,
        [Parameter(Mandatory)][string]$Target,
        [AllowEmptyString()][string]$Arguments = '',
        [AllowEmptyString()][string]$WorkingDirectory = '',
        [AllowEmptyString()][string]$Icon = '',
        [AllowEmptyString()][string]$Description = ''
    )

    $dir = Split-Path -Parent $Path
    if (-not (Test-Path -LiteralPath $dir -PathType Container)){
        $null = New-Item -ItemType Directory -Path $dir -Force
    }

    $shell = New-Object -ComObject WScript.Shell
    try{
        $lnk = $shell.CreateShortcut($Path)
        $lnk.TargetPath = $Target
        $lnk.Arguments = $Arguments
        $lnk.WorkingDirectory = $WorkingDirectory
        $lnk.Description = $Description
        $lnk.WindowStyle = 7 # minimised: the tray hides its console at once
        if ($Icon){
            $lnk.IconLocation = $Icon
        }

        $lnk.Save()
    }
    finally{
        $null = [System.Runtime.InteropServices.Marshal]::ReleaseComObject($shell)
    }
}

function Test-IDLSetupDesktopSession {
    # True when this session has a desktop (Explorer runs in it), so a tray icon can be shown now.
    $session = (Get-Process -Id $PID).SessionId
    return ($session -ne 0) -and (@(Get-Process -Name 'explorer' -ErrorAction SilentlyContinue | Where-Object { $_.SessionId -eq $session }).Count -gt 0)
}

function Test-IDLSetupTrayRunning {
    # True when a tray already runs in this session (a second one would only exit again).
    $session = (Get-Process -Id $PID).SessionId
    return (@(Get-CimInstance -ClassName Win32_Process -Filter "Name='pwsh.exe'" -ErrorAction SilentlyContinue | Where-Object {
                ($_.SessionId -eq $session) -and ([string]$_.CommandLine -match 'Start-IDLifecycleTray\.ps1')
            }).Count -gt 0)
}

function Start-IDLSetupTrayNow {
    # Opens the tray shortcut through Explorer, so the tray runs as the signed-in user with Explorer's (not elevated)
    # rights - never with this elevated installer's token.
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Shortcut
    )

    Start-Process -FilePath (Join-Path -Path $env:SystemRoot -ChildPath 'explorer.exe') -ArgumentList ('"{0}"' -f $Shortcut)
}

function Remove-IDLSetupShortcut {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Path
    )

    Remove-Item -LiteralPath $Path -Force
}

function Get-IDLSetupBrandingRoot {
    return (Get-IDLBrandingRoot)
}

function Get-IDLSetupBrandingProblem {
    # Why the existing branding folder would be ignored (it can be changed by others), or $null.
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Path
    )

    return (Get-IDLBrandingUntrustedReason -Path $Path -OwnerSidResolver { param($p) Get-IDLPathOwnerSid -Path $p })
}

function Initialize-IDLSetupBrandingDir {
    # Creates the branding override folder with its protected ACL (SYSTEM/Administrators full, Users read) and a README.
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Path
    )

    if (-not (Test-Path -LiteralPath $Path)){
        $null = New-Item -ItemType Directory -Path $Path -Force
    }

    [System.IO.FileSystemAclExtensions]::SetAccessControl([System.IO.DirectoryInfo]::new($Path), (New-IDLBrandingDirSecurity))
    $readme = Join-Path -Path $Path -ChildPath 'README.txt'
    if (-not (Test-Path -LiteralPath $readme)){
        [System.IO.File]::WriteAllText($readme, (Get-IDLBrandingReadme), [System.Text.UTF8Encoding]::new($false))
    }
}

function Get-IDLSetupDaemonCommand {
    # How to run idlifecycled.exe: the file itself, no prefix arguments.
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Exe
    )

    return @{ FileName = $Exe; Arguments = @() }
}

function Get-IDLPathOwnerSid {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Path
    )

    return (Get-IDLOwnerSid -Path $Path)
}

function Get-IDLExeSignature {
    # @{ Status = 'Valid' | 'NotSigned' | ...; Signer = <subject or ''> }
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Path
    )

    $sig = Get-AuthenticodeSignature -LiteralPath $Path
    return @{
        Status = [string]$sig.Status
        Signer = (($null -ne $sig.SignerCertificate) ? ([string]$sig.SignerCertificate.Subject) : (''))
    }
}

function Set-IDLInstallDirAcl {
    # Owner Administrators, then the protected ACL from New-IDLInstallDirSecurity (propagated to inheriting children).
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Path,
        [Parameter(Mandatory)][string]$ServiceAccount
    )

    Set-IDLAdministratorsOwner -Path $Path
    $sid = ConvertTo-IDLSetupSid -Identity $ServiceAccount
    if ($null -eq $sid){
        throw "Cannot resolve service account '$ServiceAccount' to a SID."
    }

    [System.IO.FileSystemAclExtensions]::SetAccessControl([System.IO.DirectoryInfo]::new($Path), (New-IDLInstallDirSecurity -ServiceAccountSid $sid))
}

function Test-IDLInstallDirAcl {
    # Protected, trusted owner, and nobody outside SYSTEM/Administrators/TrustedInstaller/you can change anything
    # (scripts\Logs, where the service account writes, excluded).
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Path,
        [Parameter(Mandatory)][string]$ServiceAccount
    )

    if (-not (Test-Path -LiteralPath $Path -PathType Container)){
        return $false
    }

    if (-not (Get-IDLDirectoryDacl -Path $Path).AreAccessRulesProtected){
        return $false
    }

    return ($null -eq (Get-IDLUntrustedTreeReason -Path $Path -ExcludeRelative @('scripts\Logs') -OwnerSidResolver { param($p) Get-IDLPathOwnerSid -Path $p }))
}

function Invoke-IDLServiceInstaller {
    # Runs Install-IDLifecycleService.ps1 in a child pwsh (it imports Common with -Force and calls exit) and
    # returns its exit code and the lines of the log file it wrote. -Environment entries (IDL_SERVICE_PASSWORD) are
    # set only on that child's ProcessStartInfo, never in this process or on its command line.
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$ScriptPath,
        [Parameter(Mandatory)][string[]]$Arguments,
        [hashtable]$Environment = @{}
    )

    $logDir = Join-Path -Path (Split-Path -Parent $ScriptPath) -ChildPath 'Logs'
    $before = @()
    if (Test-Path -LiteralPath $logDir){
        $before = @(Get-ChildItem -LiteralPath $logDir -Filter 'Install-IDLifecycleService_*.log' -File | ForEach-Object { $_.FullName })
    }

    $psi = [System.Diagnostics.ProcessStartInfo]::new([System.Environment]::ProcessPath)
    foreach ($a in (@('-NoProfile', '-NonInteractive', '-ExecutionPolicy', 'Bypass', '-File', $ScriptPath) + @($Arguments))){
        $psi.ArgumentList.Add([string]$a)
    }

    $psi.UseShellExecute = $false
    $psi.CreateNoWindow = $true
    $psi.RedirectStandardInput = $true
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError = $true
    $psi.StandardOutputEncoding = [System.Text.UTF8Encoding]::new($false)
    $psi.StandardErrorEncoding = [System.Text.UTF8Encoding]::new($false)
    foreach ($name in @('IDL_PASSWORD', 'IDL_SERVICE_PASSWORD', 'IDL_SETUP_DB_DSN', 'IDL_GITHUB_TOKEN', 'IDL_BACKUP_PASSPHRASE')){
        $null = $psi.Environment.Remove($name)
    }

    foreach ($key in $Environment.Keys){
        $psi.Environment[[string]$key] = [string]$Environment[$key]
    }

    $p = [System.Diagnostics.Process]::Start($psi)
    $outTask = $p.StandardOutput.ReadToEndAsync()
    $errTask = $p.StandardError.ReadToEndAsync()
    $p.StandardInput.Close()
    $p.WaitForExit()
    $code = $p.ExitCode
    $output = @(($outTask.Result + "`n" + $errTask.Result) -split "`r?`n" | Where-Object { $_ -ne '' })
    $p.Dispose()

    $lines = @()
    if (Test-Path -LiteralPath $logDir){
        $new = @(Get-ChildItem -LiteralPath $logDir -Filter 'Install-IDLifecycleService_*.log' -File | Where-Object { $before -notcontains $_.FullName } | Sort-Object LastWriteTime | Select-Object -Last 1)
        if ($new.Count -gt 0){
            $lines = @(Get-Content -LiteralPath $new[0].FullName -Encoding utf8)
        }
    }

    if ($lines.Count -eq 0){
        $lines = @($output | ForEach-Object { [string]$_ })
    }

    return [pscustomobject]@{ ExitCode = $code; Lines = [string[]]$lines }
}

function Get-IDLMachineEnvVar {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Name
    )

    return [System.Environment]::GetEnvironmentVariable($Name, [System.EnvironmentVariableTarget]::Machine)
}

function Set-IDLMachineEnvVar {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Name,
        [Parameter(Mandatory)][string]$Value
    )

    [System.Environment]::SetEnvironmentVariable($Name, $Value, [System.EnvironmentVariableTarget]::Machine)
}

function Get-IDLMachineEnvReaders {
    # Principals other than SYSTEM/Administrators/TrustedInstaller/CREATOR OWNER that can read machine env vars.
    $key = 'HKLM:\SYSTEM\CurrentControlSet\Control\Session Manager\Environment'
    $trusted = @('S-1-5-18', 'S-1-5-32-544', 'S-1-3-0', 'S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464')
    $readers = [System.Collections.Generic.List[string]]::new()
    try{
        foreach ($rule in @((Get-Acl -LiteralPath $key).Access)){
            if (
                ($rule.AccessControlType -ne 'Allow') -or
                (($rule.RegistryRights -band [System.Security.AccessControl.RegistryRights]::QueryValues) -eq 0)
            ){
                continue
            }

            $sid = ConvertTo-IDLSetupSid -Identity $rule.IdentityReference
            if (($null -ne $sid) -and ($sid -in $trusted)){
                continue
            }

            if (-not $readers.Contains([string]$rule.IdentityReference.Value)){
                $readers.Add([string]$rule.IdentityReference.Value)
            }
        }
    }
    catch{
        $readers.Add("(could not read the ACL: $($_.Exception.Message))")
    }

    return [string[]]$readers.ToArray()
}

function Test-IDLEventSource {
    # $true / $false, or $null when it cannot be determined (SourceExists scans every log, including Security).
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Source
    )

    try{
        return [System.Diagnostics.EventLog]::SourceExists($Source)
    }
    catch{
        return $null
    }
}

function New-IDLEventSource {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Source
    )

    [System.Diagnostics.EventLog]::CreateEventSource($Source, 'Application')
}

function Invoke-IDLHttpProbe {
    # Returns the HTTP status code. Certificate validation is skipped only when the caller says so (loopback only).
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Url,
        [switch]$SkipCertificateCheck
    )

    $response = Invoke-WebRequest -Uri $Url -UseBasicParsing -TimeoutSec 10 -SkipHttpErrorCheck -SkipCertificateCheck:$SkipCertificateCheck
    return [int]$response.StatusCode
}

function Read-IDLSetupText {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Prompt
    )

    return (Read-Host -Prompt $Prompt)
}

function Read-IDLSetupPassword {
    # Prompts twice (hidden) and returns a SecureString, or throws when the entries differ.
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Prompt
    )

    $first = Read-Host -Prompt $Prompt -AsSecureString
    $second = Read-Host -Prompt "$Prompt (repeat)" -AsSecureString
    if ((ConvertFrom-IDLSecure -Value $first) -cne (ConvertFrom-IDLSecure -Value $second)){
        throw (New-IDLException -Kind Input -Message 'The two password entries do not match.')
    }

    return $first
}

function Get-IDLSetupGoVersion {
    # 'go1.27.3' when Go is on PATH, else $null.
    $go = Get-Command -Name 'go' -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($null -eq $go){
        return $null
    }

    $out = & $go.Source version 2>$null
    $m = [regex]::Match([string]($out -join ' '), '\bgo(\d+\.\d+(\.\d+)?)\b')
    return (($m.Success) ? ('go' + $m.Groups[1].Value) : ($null))
}

function Invoke-IDLSetupGitHubDownload {
    <#
      Resolves -Ref to a commit SHA and downloads that commit's source zip to -OutFile; returns the SHA. The token
      (plain text, may be empty) goes only into the Authorization header of the api.github.com requests; GitHub's
      redirect to codeload.github.com carries its own short-lived token and gets no Authorization header.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Repo,
        [Parameter(Mandatory)][string]$Ref,
        [AllowEmptyString()][string]$Token = '',
        [Parameter(Mandatory)][string]$OutFile
    )

    $headers = @{ 'User-Agent' = 'IDLifecycle-Setup'; 'X-GitHub-Api-Version' = '2022-11-28' }
    if ($Token){
        $headers['Authorization'] = "Bearer $Token"
    }

    $api = "https://api.github.com/repos/$Repo"
    try{
        $headers['Accept'] = 'application/vnd.github.sha'
        $sha = ([string](Invoke-RestMethod -Uri "$api/commits/$([uri]::EscapeDataString($Ref))" -Headers $headers -TimeoutSec 60)).Trim()
        $headers['Accept'] = 'application/vnd.github+json'
        Invoke-WebRequest -Uri "$api/zipball/$sha" -Headers $headers -OutFile $OutFile -TimeoutSec 600 -MaximumRedirection 5
    }
    catch{
        $status = 0
        if (
            ($null -ne $_.Exception.PSObject.Properties['Response']) -and
            ($null -ne $_.Exception.Response)
        ){
            $status = [int]$_.Exception.Response.StatusCode
        }

        $hint = switch ($status){
            401{ 'the token was refused (expired or mistyped)' }
            403{ 'access denied or rate-limited (a token raises the limit)' }
            404{ (($Token) ? ('repository or ref not found, or the token cannot read it') : ('repository or ref not found - a private repository needs -GitHubToken / IDL_GITHUB_TOKEN')) }
            default{ $_.Exception.Message }
        }
        throw (New-IDLException -Kind Dependency -Message "GitHub download of $Repo@$Ref failed: $hint")
    }

    if ($sha -notmatch '^[0-9a-f]{40}$'){
        throw (New-IDLException -Kind Dependency -Message "GitHub returned an unexpected commit id for $Repo@$Ref.")
    }

    return $sha
}

function Invoke-IDLSetupGoBuild {
    # go build of cmd\idlifecycled in -SourceRoot (CGO off, -trimpath, go.mod/go.sum enforced) into -OutExe.
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$SourceRoot,
        [Parameter(Mandatory)][string]$OutExe,
        [Parameter(Mandatory)][string]$Version
    )

    $psi = [System.Diagnostics.ProcessStartInfo]::new((Get-Command -Name 'go' -CommandType Application | Select-Object -First 1).Source)
    foreach ($a in @('build', '-trimpath', '-mod=readonly', '-ldflags', "-X idlifecycle/internal/app.Version=$Version", '-o', $OutExe, './cmd/idlifecycled')){
        $psi.ArgumentList.Add($a)
    }

    $psi.WorkingDirectory = $SourceRoot
    $psi.UseShellExecute = $false
    $psi.CreateNoWindow = $true
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError = $true
    $psi.Environment['CGO_ENABLED'] = '0'
    foreach ($name in @('IDL_PASSWORD', 'IDL_SERVICE_PASSWORD', 'IDL_SETUP_DB_DSN', 'IDL_GITHUB_TOKEN', 'IDL_BACKUP_PASSPHRASE')){
        $null = $psi.Environment.Remove($name)
    }

    $p = [System.Diagnostics.Process]::Start($psi)
    $outTask = $p.StandardOutput.ReadToEndAsync()
    $errTask = $p.StandardError.ReadToEndAsync()
    if (-not $p.WaitForExit(900000)){
        $p.Kill($true)
        throw (New-IDLException -Kind Dependency -Message 'go build did not finish within 15 minutes.')
    }

    $p.WaitForExit()
    $text = (($outTask.Result + "`n" + $errTask.Result) -split "`r?`n" | Where-Object { $_ } | Select-Object -Last 15) -join ' | '
    if ($p.ExitCode -ne 0){
        throw (New-IDLException -Kind Dependency -Message "go build failed (exit code $($p.ExitCode)): $text")
    }
}

function Invoke-IDLSetupHandOff {
    <#
      Runs the staged release's Install-IDLifecycle.ps1 in a child pwsh that shares this console (so its prompts and
      progress show) and returns its exit code. -Environment entries (the secrets) are set only on that child.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$ScriptPath,
        [Parameter(Mandatory)][string[]]$Arguments,
        [hashtable]$Environment = @{}
    )

    $psi = [System.Diagnostics.ProcessStartInfo]::new([System.Environment]::ProcessPath)
    foreach ($a in (@('-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', $ScriptPath) + @($Arguments))){
        $psi.ArgumentList.Add([string]$a)
    }

    $psi.UseShellExecute = $false
    foreach ($name in @('IDL_PASSWORD', 'IDL_SERVICE_PASSWORD', 'IDL_SETUP_DB_DSN', 'IDL_GITHUB_TOKEN', 'IDL_BACKUP_PASSPHRASE')){
        $null = $psi.Environment.Remove($name)
    }

    foreach ($key in $Environment.Keys){
        $psi.Environment[[string]$key] = [string]$Environment[$key]
    }

    $p = [System.Diagnostics.Process]::Start($psi)
    foreach ($key in @($Environment.Keys)){
        $null = $psi.Environment.Remove([string]$key)
    }

    $p.WaitForExit()
    return $p.ExitCode
}

#endregion

#region Helpers

function New-IDLInstallDirSecurity {
    <#
      The InstallDir ACL: inheritance off; SYSTEM, Administrators and TrustedInstaller FullControl; BUILTIN\Users and
      the service account ReadAndExecute; all inherited by files and folders. (Owner is set separately.)
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$ServiceAccountSid
    )

    $inherit = [System.Security.AccessControl.InheritanceFlags]'ContainerInherit, ObjectInherit'
    $none = [System.Security.AccessControl.PropagationFlags]::None
    $allow = [System.Security.AccessControl.AccessControlType]::Allow
    $full = [System.Security.AccessControl.FileSystemRights]::FullControl
    $read = [System.Security.AccessControl.FileSystemRights]::ReadAndExecute

    $sec = [System.Security.AccessControl.DirectorySecurity]::new()
    $sec.SetAccessRuleProtection($true, $false)
    $grants = [ordered]@{
        'S-1-5-18'                                                       = $full
        'S-1-5-32-544'                                                   = $full
        'S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464' = $full
        'S-1-5-32-545'                                                   = $read
    }
    if (-not $grants.Contains($ServiceAccountSid)){
        $grants[$ServiceAccountSid] = $read
    }

    foreach ($sid in $grants.Keys){
        $sec.AddAccessRule([System.Security.AccessControl.FileSystemAccessRule]::new([System.Security.Principal.SecurityIdentifier]::new($sid), $grants[$sid], $inherit, $none, $allow))
    }

    return $sec
}

function ConvertTo-IDLSetupSid {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]$Identity
    )

    try{
        if ($Identity -is [System.Security.Principal.SecurityIdentifier]){
            return $Identity.Value
        }

        $name = (([string]$Identity -eq 'LocalSystem') ? ('NT AUTHORITY\SYSTEM') : ([string]$Identity))
        return ([System.Security.Principal.NTAccount]::new($name)).Translate([System.Security.Principal.SecurityIdentifier]).Value
    }
    catch{
        return $null
    }
}

function ConvertFrom-IDLSecure {
    # SecureString -> plain string, freeing the unmanaged copy immediately. Callers drop the result as soon as possible.
    [CmdletBinding()]
    param(
        [AllowNull()][securestring]$Value
    )

    if ($null -eq $Value){
        return $null
    }

    $bstr = [System.Runtime.InteropServices.Marshal]::SecureStringToBSTR($Value)
    try{
        return [System.Runtime.InteropServices.Marshal]::PtrToStringBSTR($bstr)
    }
    finally{
        [System.Runtime.InteropServices.Marshal]::ZeroFreeBSTR($bstr)
    }
}

function Test-IDLSetupInteractive {
    if ($NonInteractive){
        return $false
    }

    if (
        (-not [Environment]::UserInteractive) -or
        ([Console]::IsInputRedirected)
    ){
        return $false
    }

    foreach ($arg in [Environment]::GetCommandLineArgs()){
        if ($arg -match '^-noni'){
            return $false
        }
    }

    return $true
}

function Test-IDLWildcardHost {
    [CmdletBinding()]
    param(
        [AllowEmptyString()][string]$HostName
    )

    return ($HostName -in @('', '0.0.0.0', '::', '[::]'))
}

function Test-IDLHasZoneId {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Path
    )

    return ($null -ne (Get-Item -LiteralPath $Path -Stream 'Zone.Identifier' -ErrorAction SilentlyContinue))
}

function Invoke-IDLDaemon {
    <#
      Runs idlifecycled with the given arguments in a child process and returns { ExitCode; StdOut; StdErr }.
      -Environment entries (e.g. IDL_PASSWORD) are set ONLY on that child's ProcessStartInfo and removed afterwards;
      they never enter this process's environment. $script:DaemonEnv (an env:NAME DSN) is passed to every call.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Exe,
        [Parameter(Mandatory)][string[]]$Arguments,
        [hashtable]$Environment = @{},
        [int]$TimeoutSec = 180
    )

    $command = Get-IDLSetupDaemonCommand -Exe $Exe
    $psi = [System.Diagnostics.ProcessStartInfo]::new([string]$command.FileName)
    foreach ($a in (@($command.Arguments) + @($Arguments))){
        $psi.ArgumentList.Add([string]$a)
    }

    $psi.UseShellExecute = $false
    $psi.CreateNoWindow = $true
    $psi.RedirectStandardInput = $true
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError = $true
    $psi.StandardOutputEncoding = [System.Text.UTF8Encoding]::new($false)
    $psi.StandardErrorEncoding = [System.Text.UTF8Encoding]::new($false)
    $null = $psi.Environment.Remove('IDL_PASSWORD')
    $null = $psi.Environment.Remove('IDL_SERVICE_PASSWORD')
    $null = $psi.Environment.Remove('IDL_SETUP_DB_DSN')
    $null = $psi.Environment.Remove('IDL_GITHUB_TOKEN')
    $null = $psi.Environment.Remove('IDL_BACKUP_PASSPHRASE')
    $keys = @($script:DaemonEnv.Keys) + @($Environment.Keys)
    foreach ($key in $script:DaemonEnv.Keys){
        $psi.Environment[[string]$key] = [string]$script:DaemonEnv[$key]
    }

    foreach ($key in $Environment.Keys){
        $psi.Environment[[string]$key] = [string]$Environment[$key]
    }

    try{
        $p = [System.Diagnostics.Process]::Start($psi)
        $outTask = $p.StandardOutput.ReadToEndAsync()
        $errTask = $p.StandardError.ReadToEndAsync()
        $p.StandardInput.Close()
        if (-not $p.WaitForExit($TimeoutSec * 1000)){
            try{
                $p.Kill($true)
            }
            catch{
                Write-IDLLog "Could not kill timed-out idlifecycled: $($_.Exception.Message)" 'WARN'
            }

            throw "idlifecycled $($Arguments[0]) timed out after $TimeoutSec s"
        }

        $p.WaitForExit()
        return [pscustomobject]@{
            ExitCode = $p.ExitCode
            StdOut   = [string]$outTask.Result
            StdErr   = [string]$errTask.Result
        }
    }
    finally{
        foreach ($key in $keys){
            $null = $psi.Environment.Remove([string]$key)
        }
    }
}

function Write-IDLDaemonOutput {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]$Result,
        [string]$Prefix = '  idlifecycled: '
    )

    foreach ($line in (([string]$Result.StdOut + "`n" + [string]$Result.StdErr) -split "`r?`n")){
        if (-not [string]::IsNullOrWhiteSpace($line)){
            Write-IDLLog ($Prefix + $line.TrimEnd())
        }
    }
}

function Get-IDLDaemonVersion {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Exe
    )

    $r = Invoke-IDLDaemon -Exe $Exe -Arguments @('version') -TimeoutSec 30
    if ($r.ExitCode -ne 0){
        throw "'$Exe version' failed with exit code $($r.ExitCode): $($r.StdErr.Trim())"
    }

    return ($r.StdOut.Trim() -replace '^idlifecycled\s+', '')
}

function Invoke-IDLCheck {
    # Runs `check -json -skip-ad-module` (connects to the DB and creates/migrates the schema) and returns the parsed document.
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Exe,
        [Parameter(Mandatory)][string]$ConfigPath
    )

    $r = Invoke-IDLDaemon -Exe $Exe -Arguments @('check', '-config', $ConfigPath, '-json', '-skip-ad-module')
    try{
        $doc = $r.StdOut | ConvertFrom-Json -AsHashtable
    }
    catch{
        Write-IDLDaemonOutput -Result $r
        throw "check did not return JSON (exit code $($r.ExitCode)): $($r.StdErr.Trim())"
    }

    if (
        ($doc -isnot [System.Collections.IDictionary]) -or
        (-not $doc.Contains('checks'))
    ){
        throw "check returned an unexpected document (exit code $($r.ExitCode))."
    }

    return $doc
}

function Get-IDLCheckEntry {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]$Doc,
        [Parameter(Mandatory)][string]$Name
    )

    foreach ($c in @($Doc['checks'])){
        if ([string]$c['name'] -eq $Name){
            return $c
        }
    }

    return $null
}

function Test-IDLCheckSkipped {
    # check -json marks checks it did not run with "skipped": true (e.g. ad_module with -skip-ad-module).
    [CmdletBinding()]
    param(
        [AllowNull()]$Entry
    )

    return (
        ($Entry -is [System.Collections.IDictionary]) -and
        ($Entry.Contains('skipped')) -and
        ([bool]$Entry['skipped'])
    )
}

function Test-IDLCheckPassed {
    # Passed = present, ok and actually run. A missing or skipped entry never counts as a pass.
    [CmdletBinding()]
    param(
        [AllowNull()]$Entry
    )

    return (
        ($Entry -is [System.Collections.IDictionary]) -and
        ([bool]$Entry['ok']) -and
        (-not (Test-IDLCheckSkipped -Entry $Entry))
    )
}

function Get-IDLCheckFailureText {
    [CmdletBinding()]
    param(
        [AllowNull()]$Entry,
        [Parameter(Mandatory)][string]$Name
    )

    if ($null -eq $Entry){
        return "check reported no $Name entry"
    }

    $prefix = (((Test-IDLCheckSkipped -Entry $Entry)) ? ('not run: ') : (''))
    return ($prefix + [string]$Entry['detail'])
}

function Get-IDLConfigValue {
    # Value of a dotted key ('database.dsn') in config.json, '' when absent.
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Path,
        [Parameter(Mandatory)][string]$Key
    )

    $node = Get-Content -LiteralPath $Path -Raw -Encoding utf8 | ConvertFrom-Json -AsHashtable
    foreach ($part in $Key.Split('.')){
        $node = Get-IDLField -Object $node -Name $part
    }

    return [string]$node
}

function Set-IDLConfigFields {
    <#
      Sets dotted string keys (e.g. 'database.dsn', 'powershell.scripts_dir') in config.json, keeping every other
      field and their order (System.Text.Json DOM). Written to a temp file in the same (restricted) folder, read back,
      then moved over config.json with File.Move(overwrite) - never a half-written config. Values are never logged.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Path,
        [Parameter(Mandatory)][System.Collections.IDictionary]$Fields
    )

    $root = [System.Text.Json.Nodes.JsonNode]::Parse([System.IO.File]::ReadAllText($Path))
    if ($root -isnot [System.Text.Json.Nodes.JsonObject]){
        throw "$Path is not a JSON object."
    }

    foreach ($key in $Fields.Keys){
        $parts = ([string]$key).Split('.')
        $node = $root
        for ($i = 0; $i -lt $parts.Count - 1; $i++){
            $child = $null
            if ($node.ContainsKey($parts[$i])){
                $child = $node[$parts[$i]]
            }

            if ($child -isnot [System.Text.Json.Nodes.JsonObject]){
                $child = [System.Text.Json.Nodes.JsonObject]::new()
                $node[$parts[$i]] = $child
            }

            $node = $child
        }

        $node[$parts[-1]] = [System.Text.Json.Nodes.JsonValue]::Create([string]$Fields[$key])
    }

    $options = [System.Text.Json.JsonSerializerOptions]::new()
    $options.WriteIndented = $true
    $options.Encoder = [System.Text.Encodings.Web.JavaScriptEncoder]::UnsafeRelaxedJsonEscaping
    $text = $root.ToJsonString($options) + "`n"

    $tmp = Join-Path -Path (Split-Path -Parent $Path) -ChildPath ('.config.json.{0}.tmp' -f [guid]::NewGuid().ToString('N'))
    try{
        [System.IO.File]::WriteAllText($tmp, $text, [System.Text.UTF8Encoding]::new($false))
        foreach ($key in $Fields.Keys){
            if ((Get-IDLConfigValue -Path $tmp -Key ([string]$key)) -cne [string]$Fields[$key]){
                throw "The rewritten config.json did not read back '$key'."
            }
        }

        [System.IO.File]::Move($tmp, $Path, $true)
    }
    finally{
        if (Test-Path -LiteralPath $tmp){
            Remove-Item -LiteralPath $tmp -Force -ErrorAction SilentlyContinue
        }
    }
}

function Get-IDLTrustedRelease {
    <#
      Verifies SourceDir BEFORE anything from it runs and returns @{ Exe; Files = @{Relative; Source; Hash}[];
      Manifest; Signature }. Throws an Input exception (exit 2) - no -Force bypass:
        * SourceDir, anything below it, or its parent chain can be changed by an untrusted account
          (Get-IDLUntrustedTreeReason; junctions/symlinks below SourceDir are refused too);
        * SHA256SUMS is missing or malformed, lists something other than idlifecycled.exe, Setup.cmd and scripts\*, lacks a
          required file, or a file does not match its hash;
        * scripts\ contains a file the manifest does not list;
        * -RequireSignature and idlifecycled.exe has no valid Authenticode signature.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Root
    )

    $hint = 'Build a release folder with scripts\New-IDLifecycleRelease.ps1 (it restricts the folder to SYSTEM, Administrators and you) and run the installer from it.'
    $reason = Get-IDLUntrustedTreeReason -Path $Root -ParentChain -RefuseReparsePoints -OwnerSidResolver { param($p) Get-IDLPathOwnerSid -Path $p }
    if ($reason){
        throw (New-IDLException -Kind Input -Message "SourceDir '$Root' is not trusted: $reason. $hint")
    }

    if ($SkipIntegrityCheck){
        return (Get-IDLUnverifiedRelease -Root $Root)
    }

    $manifestPath = Join-Path -Path $Root -ChildPath $script:ManifestName
    if (-not (Test-Path -LiteralPath $manifestPath -PathType Leaf)){
        throw (New-IDLException -Kind Input -Message "SourceDir '$Root' has no $($script:ManifestName) manifest. $hint")
    }

    try{
        $entries = Read-IDLManifest -Path $manifestPath
    }
    catch{
        throw (New-IDLException -Kind Input -Message "$manifestPath is not valid: $($_.Exception.Message)")
    }

    $listed = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::OrdinalIgnoreCase)
    $files = [System.Collections.Generic.List[object]]::new()
    foreach ($e in $entries){
        if (
            ($e.Relative -notin @('idlifecycled.exe', 'Setup.cmd')) -and
            (-not $e.Relative.StartsWith('scripts\', [System.StringComparison]::OrdinalIgnoreCase))
        ){
            throw (New-IDLException -Kind Input -Message "$($script:ManifestName) lists '$($e.Relative)', which is not part of a release (only idlifecycled.exe, Setup.cmd and scripts\*).")
        }

        $source = Join-Path -Path $Root -ChildPath $e.Relative
        if (-not (Test-Path -LiteralPath $source -PathType Leaf)){
            throw (New-IDLException -Kind Input -Message "SourceDir is missing '$($e.Relative)' listed in $($script:ManifestName).")
        }

        $actual = (Get-FileHash -LiteralPath $source -Algorithm SHA256).Hash
        if ($actual -ne $e.Hash){
            throw (New-IDLException -Kind Input -Message "'$($e.Relative)' does not match $($script:ManifestName) (SHA256 $actual, expected $($e.Hash)); refusing a modified release.")
        }

        $null = $listed.Add($e.Relative)
        $files.Add(@{ Relative = $e.Relative; Source = $source; Hash = $e.Hash })
    }

    $missing = @(@('idlifecycled.exe') + @($script:RequiredScripts | ForEach-Object { "scripts\$_" }) | Where-Object { -not $listed.Contains($_) })
    if ($missing.Count -gt 0){
        throw (New-IDLException -Kind Input -Message "$($script:ManifestName) does not list: $($missing -join ', ').")
    }

    $scriptsRoot = Join-Path -Path $Root -ChildPath 'scripts'
    $extra = @(Get-ChildItem -LiteralPath $scriptsRoot -Recurse -File -Force |
            ForEach-Object { 'scripts\' + $_.FullName.Substring($scriptsRoot.Length).TrimStart('\') } |
            Where-Object { (-not $listed.Contains($_)) -and (('\' + $_ + '\') -notmatch '\\Logs\\') })
    if ($extra.Count -gt 0){
        throw (New-IDLException -Kind Input -Message "SourceDir\scripts contains file(s) not listed in $($script:ManifestName): $($extra -join ', '); refusing.")
    }

    $exe = Join-Path -Path $Root -ChildPath 'idlifecycled.exe'
    $signature = Get-IDLExeSignature -Path $exe
    if (
        ($RequireSignature) -and
        ($signature.Status -ne 'Valid')
    ){
        throw (New-IDLException -Kind Input -Message "idlifecycled.exe has no valid Authenticode signature (status: $($signature.Status)) and -RequireSignature was given.")
    }

    return @{ Exe = $exe; Files = $files.ToArray(); Manifest = $manifestPath; Signature = $signature; Unverified = $false; TempDir = $null }
}

function Get-IDLUnverifiedRelease {
    <#
      -SkipIntegrityCheck (LEAST SAFE): the release is whatever Root holds - idlifecycled.exe, Setup.cmd (if present) and
      every file below scripts\ except Logs\. SHA256SUMS is not read. The hashes are taken now and written to a new
      manifest in a protected staging folder, which is installed as InstallDir\SHA256SUMS (upgrade, uninstall and the
      change detection use it as usual). Root's ACL was already checked by the caller. Returns the same shape as
      Get-IDLTrustedRelease plus Unverified and TempDir (removed at the end of the run).
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Root
    )

    $exe = Join-Path -Path $Root -ChildPath 'idlifecycled.exe'
    if (-not (Test-Path -LiteralPath $exe -PathType Leaf)){
        throw (New-IDLException -Kind Input -Message "SourceDir '$Root' has no idlifecycled.exe. In a source checkout build it first (go build -trimpath -o idlifecycled.exe .\cmd\idlifecycled), or install with -FromGitHub, which builds it for you.")
    }

    $scriptsRoot = Join-Path -Path $Root -ChildPath 'scripts'
    if (-not (Test-Path -LiteralPath $scriptsRoot -PathType Container)){
        throw (New-IDLException -Kind Input -Message "SourceDir '$Root' has no scripts folder.")
    }

    $relative = [System.Collections.Generic.List[string]]::new()
    $relative.Add('idlifecycled.exe')
    if (Test-Path -LiteralPath (Join-Path -Path $Root -ChildPath 'Setup.cmd') -PathType Leaf){
        $relative.Add('Setup.cmd')
    }

    foreach ($item in @(Get-ChildItem -LiteralPath $scriptsRoot -Recurse -File -Force)){
        $rel = 'scripts\' + $item.FullName.Substring($scriptsRoot.Length).TrimStart('\')
        if (('\' + $rel + '\') -notmatch '\\Logs\\'){
            $relative.Add($rel)
        }
    }

    $listed = [System.Collections.Generic.HashSet[string]]::new([string[]]$relative.ToArray(), [System.StringComparer]::OrdinalIgnoreCase)
    $missing = @($script:RequiredScripts | ForEach-Object { "scripts\$_" } | Where-Object { -not $listed.Contains($_) })
    if ($missing.Count -gt 0){
        throw (New-IDLException -Kind Input -Message "SourceDir '$Root' is missing: $($missing -join ', ').")
    }

    $files = [System.Collections.Generic.List[object]]::new()
    foreach ($rel in ($relative | Sort-Object)){
        $source = Join-Path -Path $Root -ChildPath $rel
        $files.Add(@{ Relative = $rel; Source = $source; Hash = (Get-FileHash -LiteralPath $source -Algorithm SHA256).Hash })
    }

    $signature = Get-IDLExeSignature -Path $exe
    if (
        ($RequireSignature) -and
        ($signature.Status -ne 'Valid')
    ){
        throw (New-IDLException -Kind Input -Message "idlifecycled.exe has no valid Authenticode signature (status: $($signature.Status)) and -RequireSignature was given.")
    }

    $stagingRoot = Get-IDLSetupStagingRoot
    Initialize-IDLProtectedDir -Path $stagingRoot -Lock:$script:SetupLogLock
    $tempDir = Join-Path -Path $stagingRoot -ChildPath ('unverified_{0}_{1}' -f (Get-Date -Format 'yyyyMMdd_HHmmss'), [guid]::NewGuid().ToString('N').Substring(0, 8))
    $null = New-Item -Path $tempDir -ItemType Directory
    $script:UnverifiedTempDir = $tempDir # removed at the end of the run, whatever happens
    $manifestPath = Join-Path -Path $tempDir -ChildPath $script:ManifestName
    $lines = foreach ($f in $files){
        '{0}  {1}' -f $f.Hash.ToLowerInvariant(), $f.Relative.Replace('\', '/')
    }
    [System.IO.File]::WriteAllText($manifestPath, (($lines -join "`n") + "`n"))

    return @{ Exe = $exe; Files = $files.ToArray(); Manifest = $manifestPath; Signature = $signature; Unverified = $true; TempDir = $tempDir }
}

function Get-IDLChangedProgramFiles {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][AllowEmptyCollection()][object[]]$Files,
        [Parameter(Mandatory)][string]$TargetDir
    )

    $changed = [System.Collections.Generic.List[object]]::new()
    foreach ($f in $Files){
        $target = Join-Path -Path $TargetDir -ChildPath $f.Relative
        if (
            (-not (Test-Path -LiteralPath $target -PathType Leaf)) -or
            ((Get-FileHash -LiteralPath $target -Algorithm SHA256).Hash -ne $f.Hash)
        ){
            $changed.Add(@{ Relative = $f.Relative; Source = $f.Source; Target = $target; Hash = $f.Hash })
        }
    }

    return , $changed.ToArray()
}

function Wait-IDLSetupServiceState {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Name,
        [Parameter(Mandatory)][string]$State,
        [int]$TimeoutSec = 60
    )

    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSec)
    while ($true){
        $svc = Get-IDLSetupService -Name $Name
        if (($null -ne $svc) -and ($svc.State -eq $State)){
            return
        }

        if ([DateTime]::UtcNow -gt $deadline){
            $now = (($null -eq $svc) ? ('missing') : ($svc.State))
            throw "Service '$Name' did not reach state $State within $TimeoutSec s (now: $now)."
        }

        Start-Sleep -Milliseconds 500
    }
}

function Invoke-SetupStep {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Action,
        [Parameter(Mandatory)][string]$Identity,
        [string]$Details = '',
        [Parameter(Mandatory)][scriptblock]$IsSatisfied,
        [Parameter(Mandatory)][scriptblock]$Apply,
        [string]$SatisfiedReason = 'already configured'
    )

    Add-IDLCounter -Name 'Total'

    try{
        if (& $IsSatisfied){
            Write-IDLLog "SKIP: $Action $Identity ($SatisfiedReason)"
            Add-IDLCounter -Name 'Skipped'
            Add-IDLSkipReason -Reason $SatisfiedReason
            Add-IDLReportRow -Action $Action -Result 'Skipped' -Identity $Identity -Details $SatisfiedReason
            return
        }

        if ($DryRun){
            Write-IDLLog "WOULD: $Action $Identity $Details"
            Add-IDLCounter -Name 'WouldChange'
            Add-IDLCounter -Name 'Succeeded'
            Add-IDLReportRow -Action $Action -Result 'Would' -Identity $Identity -Details $Details
            return
        }

        & $Apply
        Write-IDLLog "DONE: $Action $Identity $Details"
        Add-IDLCounter -Name 'Changed'
        Add-IDLCounter -Name 'Succeeded'
        Add-IDLReportRow -Action $Action -Result 'Applied' -Identity $Identity -Details $Details
    }
    catch{
        $script:FailedSteps++
        Add-IDLCounter -Name 'Failed'
        Write-IDLLog "FAILED: $Action $Identity - $($_.Exception.Message)" 'ERROR'
        Add-IDLReportRow -Action $Action -Result 'Failed' -Identity $Identity -Details $Details -ErrorText $_.Exception.Message
    }
}

function Add-SetupSkip {
    # A step that does not apply in this run (not requested / nothing to do).
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Action,
        [Parameter(Mandatory)][string]$Identity,
        [Parameter(Mandatory)][string]$Reason,
        [ValidateSet('INFO', 'WARN')][string]$Level = 'INFO'
    )

    Add-IDLCounter -Name 'Total'
    Add-IDLCounter -Name 'Skipped'
    Add-IDLSkipReason -Reason $Reason
    Write-IDLLog "SKIP: $Action $Identity ($Reason)" $Level
    Add-IDLReportRow -Action $Action -Result 'Skipped' -Identity $Identity -Details $Reason
}

function Add-SetupFailure {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Action,
        [Parameter(Mandatory)][string]$Identity,
        [Parameter(Mandatory)][string]$Message,
        [string]$Details = ''
    )

    $script:FailedSteps++
    Add-IDLCounter -Name 'Total'
    Add-IDLCounter -Name 'Failed'
    Write-IDLLog "FAILED: $Action $Identity - $Message" 'ERROR'
    Add-IDLReportRow -Action $Action -Result 'Failed' -Identity $Identity -Details $Details -ErrorText $Message
}

function ConvertTo-IDLGitHubRepo {
    # 'owner/name', 'https://github.com/owner/name' or '...name.git' -> 'owner/name'; throws on anything else.
    [CmdletBinding()]
    param(
        [AllowEmptyString()][string]$Repo
    )

    $r = ($Repo.Trim() -replace '^https?://(www\.)?github\.com/', '' -replace '\.git$', '').TrimEnd('/')
    if ($r -notmatch '^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$'){
        throw (New-IDLException -Kind Input -Message "GitHubRepo '$Repo' is not a GitHub repository (owner/name or https://github.com/owner/name).")
    }

    return $r
}

function Get-IDLSetupStagingRoot {
    # Next to the setup log folder: %ProgramData%\IDLifecycle-Setup\Staging (protected like the logs), or
    # %LOCALAPPDATA%\IDLifecycle-Setup\Staging for a non-elevated -DryRun.
    return (Join-Path -Path (Split-Path -Parent $script:SetupLogRoot) -ChildPath 'Staging')
}

function Get-IDLHandOffArguments {
    # The parameters this run was given, for the staged installer: -SourceDir is the staged release; the source
    # parameters, the secrets (passed in its environment) and -TestHook are left out.
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][System.Collections.Generic.IDictionary[string, object]]$Bound,
        [Parameter(Mandatory)][string]$Release
    )

    $arguments = [System.Collections.Generic.List[string]]::new()
    $arguments.Add('-SourceDir')
    $arguments.Add($Release)
    $names = @('InstallDir', 'DataDir', 'ServiceAccount', 'ServiceName', 'DbDriver', 'DbDsnEnvVar', 'Listen', 'TlsCertFile', 'TlsKeyFile',
        'AdminUser', 'RestoreFrom', 'RestoreConfig', 'Start', 'RequireSignature', 'SkipIntegrityCheck', 'NoTray', 'InstallPrerequisites', 'NonInteractive', 'Force', 'DryRun')
    foreach ($name in $names){
        if (-not $Bound.ContainsKey($name)){
            continue
        }

        $value = $Bound[$name]
        if ($value -is [System.Management.Automation.SwitchParameter]){
            $arguments.Add((($value.IsPresent) ? ("-$name") : ("-${name}:`$false")))
            continue
        }

        $arguments.Add("-$name")
        $arguments.Add([string]$value)
    }

    return [string[]]$arguments.ToArray()
}

function Invoke-IDLSetupFromStaging {
    <#
      -FromGitHub or a .zip -SourceDir: gets the release into a NEW folder below the protected staging root (GitHub:
      download the commit, build idlifecycled.exe with Go, lay out exe + scripts + Setup.cmd + SHA256SUMS; zip:
      check where it sits, extract it safely), then hands over to THAT release's Install-IDLifecycle.ps1 with the same
      parameters (secrets only in its environment). Returns its exit code. The staging folder is removed unless the
      staged installer failed (then it is kept for inspection).
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][System.Collections.Generic.IDictionary[string, object]]$Bound,
        [hashtable]$Secrets = @{},
        [AllowNull()][string]$TokenPlain
    )

    $phase = Start-IDLPhase -Name 'Get source'
    if (
        (-not $DryRun) -and
        (-not (Test-IDLSetupElevated))
    ){
        throw (New-IDLException -Kind Connection -Message 'Run this script from an elevated (Administrator) session (or use -DryRun).')
    }

    if ($FromGitHub){
        $repo = ConvertTo-IDLGitHubRepo -Repo $GitHubRepo
        if (
            ($GitHubRef -notmatch '^[A-Za-z0-9._/-]{1,200}$') -or
            ($GitHubRef -match '\.\.')
        ){
            throw (New-IDLException -Kind Input -Message "GitHubRef '$GitHubRef' is not a branch, tag or commit name.")
        }

        $go = Get-IDLSetupGoVersion
        if (-not $go){
            throw (New-IDLException -Kind Dependency -Message 'Go was not found (go.exe is not on PATH). Building from GitHub needs Go 1.22 or newer (https://go.dev/dl/). On a server without Go, install from a release zip instead (New-IDLifecycleRelease.ps1 -Zip on a machine that has Go).')
        }

        Write-IDLLog "Go found: $go"
    }

    $stagingRoot = Get-IDLSetupStagingRoot
    Initialize-IDLProtectedDir -Path $stagingRoot -Lock:$script:SetupLogLock
    $stage = Join-Path -Path $stagingRoot -ChildPath ('{0}_{1}' -f (Get-Date -Format 'yyyyMMdd_HHmmss'), [guid]::NewGuid().ToString('N').Substring(0, 8))
    $null = New-Item -Path $stage -ItemType Directory
    Write-IDLLog "Staging folder: $stage"
    $keep = $false
    try{
        if ($FromGitHub){
            $zip = Join-Path -Path $stage -ChildPath 'source.zip'
            $sha = Invoke-IDLSetupGitHubDownload -Repo $repo -Ref $GitHubRef -Token ([string]$TokenPlain) -OutFile $zip
            Write-IDLLog "Downloaded $repo@$GitHubRef (commit $sha)"
            Add-IDLReportRow -Action 'DownloadSource' -Result 'Applied' -Identity "$repo@$GitHubRef" -Details "commit $sha"
            $src = Expand-IDLZipSafe -ZipPath $zip -Destination (Join-Path -Path $stage -ChildPath 'src')
            Remove-Item -LiteralPath $zip -Force
            if (
                (-not (Test-Path -LiteralPath (Join-Path -Path $src -ChildPath 'go.mod') -PathType Leaf)) -or
                (-not (Test-Path -LiteralPath (Join-Path -Path $src -ChildPath 'cmd\idlifecycled') -PathType Container))
            ){
                throw (New-IDLException -Kind Input -Message "$repo@$GitHubRef is not the IDLifecycle source (go.mod or cmd\idlifecycled is missing).")
            }

            $release = Join-Path -Path $stage -ChildPath 'release'
            $null = New-Item -Path $release -ItemType Directory
            $refPart = ($GitHubRef -replace '[^A-Za-z0-9.\-+]', '-').Trim('-', '.', '+')
            $version = ('{0}-{1}' -f $refPart, $sha.Substring(0, 7)).TrimStart('-')
            if ($version.Length -gt 64){
                $version = $version.Substring($version.Length - 64).TrimStart('-', '.', '+')
            }

            Write-IDLLog "Building idlifecycled.exe $version (go build; module downloads may take a minute)"
            Invoke-IDLSetupGoBuild -SourceRoot $src -OutExe (Join-Path -Path $release -ChildPath 'idlifecycled.exe') -Version $version
            $count = Copy-IDLReleaseFiles -SourceRoot $src -OutDir $release
            Write-IDLLog "Built release $version ($count file(s) in SHA256SUMS)"
            Add-IDLReportRow -Action 'BuildRelease' -Result 'Applied' -Identity $release -Details "$version; $count file(s)"
        }
        else{
            $zipPath = [System.IO.Path]::GetFullPath($SourceDir)
            if (-not (Test-Path -LiteralPath $zipPath -PathType Leaf)){
                throw (New-IDLException -Kind Input -Message "SourceDir not found: $zipPath")
            }

            $reason = Get-IDLUntrustedTreeReason -Path $zipPath -ParentChain -OwnerSidResolver { param($p) Get-IDLPathOwnerSid -Path $p }
            if ($reason){
                throw (New-IDLException -Kind Input -Message "SourceDir '$zipPath' is not trusted: $reason. Put the zip in a folder that only administrators (and you) can change.")
            }

            $hash = (Get-FileHash -LiteralPath $zipPath -Algorithm SHA256).Hash
            $release = Expand-IDLZipSafe -ZipPath $zipPath -Destination (Join-Path -Path $stage -ChildPath 'release')
            if ($SkipIntegrityCheck){
                # A zip of a folder (GitHub "Download ZIP", Compress-Archive of a folder) has one top folder: use it.
                $top = @(Get-ChildItem -LiteralPath $release -Force)
                if (
                    ($top.Count -eq 1) -and
                    ($top[0].PSIsContainer) -and
                    (Test-Path -LiteralPath (Join-Path -Path $top[0].FullName -ChildPath 'scripts') -PathType Container)
                ){
                    $release = $top[0].FullName
                }
            }
            elseif (-not (Test-Path -LiteralPath (Join-Path -Path $release -ChildPath $script:ManifestName) -PathType Leaf)){
                throw (New-IDLException -Kind Input -Message "'$zipPath' is not an IDLifecycle release zip (no $($script:ManifestName)). Build one with New-IDLifecycleRelease.ps1 -Zip, or - least safe, for testing - add -SkipIntegrityCheck.")
            }

            Write-IDLLog "Extracted $zipPath (SHA256 $hash)"
            Add-IDLReportRow -Action 'ExtractRelease' -Result 'Applied' -Identity $zipPath -Details "SHA256 $hash"
        }

        $installer = Join-Path -Path $release -ChildPath 'scripts\Install-IDLifecycle.ps1'
        if (-not (Test-Path -LiteralPath $installer -PathType Leaf)){
            throw (New-IDLException -Kind Input -Message "The release has no scripts\Install-IDLifecycle.ps1.")
        }

        Stop-IDLPhase -Phase $phase

        $phase = Start-IDLPhase -Name 'Hand over'
        $arguments = Get-IDLHandOffArguments -Bound $Bound -Release $release
        if (
            ($FromGitHub) -and
            ($SkipIntegrityCheck)
        ){
            # The release was just built here with its own SHA256SUMS: check it as usual.
            $arguments = [string[]]@($arguments | Where-Object { $_ -ne '-SkipIntegrityCheck' })
            Write-IDLLog '-SkipIntegrityCheck is not needed with -FromGitHub (the release built here has its own SHA256SUMS); it is checked as usual.'
        }
        Write-IDLLog "Handing over to $installer (the release's own installer checks the release again and writes its own log in $($script:SetupLogRoot))"
        $code = Invoke-IDLSetupHandOff -ScriptPath $installer -Arguments $arguments -Environment $Secrets
        $keep = ($code -ne 0)
        Write-IDLLog "The staged installer finished with exit code $code" ((($code -eq 0)) ? ('INFO') : ('ERROR'))
        Add-IDLReportRow -Action 'HandOff' -Result ((($code -eq 0)) ? ('Applied') : ('Failed')) -Identity $installer -Details "exit code $code"
        Stop-IDLPhase -Phase $phase
        return $code
    }
    finally{
        if ($keep){
            Write-IDLLog "Staging folder kept for inspection: $stage (delete it when done)" 'WARN'
        }
        elseif (
            (Test-Path -LiteralPath $stage) -and
            (Test-IDLPathWithin -Child $stage -Parent $stagingRoot)
        ){
            Remove-Item -LiteralPath $stage -Recurse -Force -ErrorAction SilentlyContinue
        }
    }
}

function Assert-NoFailedSteps {
    # Later steps depend on earlier ones: stop at the first phase that had a failure (exit code 1).
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Phase
    )

    if ($script:FailedSteps -gt 0){
        $ex = New-IDLException -Kind Fatal -Message "Stopping after phase '$Phase': $($script:FailedSteps) step(s) failed. Nothing after this point was changed."
        $ex.Data['IDLStepFailure'] = $true
        throw $ex
    }
}

#endregion

if ($TestHook){
    $hookRefusal = Get-IDLTestHookRefusal -HookPath $TestHook -ScriptRoot $PSScriptRoot -InstallDir $InstallDir
    if ($hookRefusal){
        [Console]::Error.WriteLine("Install-IDLifecycle: -TestHook refused ($hookRefusal); it is only for the Pester tests.")
        exit 2
    }

    [Console]::Error.WriteLine("Install-IDLifecycle: TEST HOOK $TestHook replaces the system-touching functions.")
    . $TestHook
}

# Setup logs: a protected folder, never next to a (possibly user-writable) script.
try{
    if (
        (-not $script:SetupLogOverride) -and
        (-not (Test-IDLSetupElevated))
    ){
        $script:SetupLogRoot = Join-Path -Path $env:LOCALAPPDATA -ChildPath 'IDLifecycle-Setup\Logs'
        $script:SetupLogLock = $false
    }

    Initialize-IDLProtectedDir -Path $script:SetupLogRoot -Lock:$script:SetupLogLock
}
catch{
    [Console]::Error.WriteLine("Install-IDLifecycle: cannot use the log folder '$($script:SetupLogRoot)': $($_.Exception.Message)")
    exit 2
}

$exitCode = 5
$adminPlain = $null
$dsnPlain = $null
$servicePlain = $null
$tokenPlain = $null
$restorePlain = $null

try{
    # IDL_PASSWORD (same convention as `idlifecycled user add`) is taken out of this process's environment at once,
    # so no other child (check, lockdown, the service installer) ever inherits it.
    $envPassword = [System.Environment]::GetEnvironmentVariable('IDL_PASSWORD', [System.EnvironmentVariableTarget]::Process)
    if (-not [string]::IsNullOrEmpty($envPassword)){
        [System.Environment]::SetEnvironmentVariable('IDL_PASSWORD', $null, [System.EnvironmentVariableTarget]::Process)
        if ($null -eq $AdminPassword){
            $AdminPassword = ConvertTo-SecureString -String $envPassword -AsPlainText -Force
        }
    }

    $envPassword = $null

    # Same for the service account password: it may reach only the service installer child.
    $envServicePassword = [System.Environment]::GetEnvironmentVariable('IDL_SERVICE_PASSWORD', [System.EnvironmentVariableTarget]::Process)
    if (-not [string]::IsNullOrEmpty($envServicePassword)){
        [System.Environment]::SetEnvironmentVariable('IDL_SERVICE_PASSWORD', $null, [System.EnvironmentVariableTarget]::Process)
        if ($null -eq $ServiceAccountPassword){
            $ServiceAccountPassword = ConvertTo-SecureString -String $envServicePassword -AsPlainText -Force
        }
    }

    $envServicePassword = $null

    # And the connection string (used by Start-IDLifecycleSetup.ps1, which never puts it on a command line).
    $envDsn = [System.Environment]::GetEnvironmentVariable('IDL_SETUP_DB_DSN', [System.EnvironmentVariableTarget]::Process)
    if (-not [string]::IsNullOrEmpty($envDsn)){
        [System.Environment]::SetEnvironmentVariable('IDL_SETUP_DB_DSN', $null, [System.EnvironmentVariableTarget]::Process)
        if ($null -eq $DbDsn){
            $DbDsn = ConvertTo-SecureString -String $envDsn -AsPlainText -Force
        }
    }

    $envDsn = $null

    # And the backup passphrase (-RestoreFrom): it may reach only the idlifecycled restore child.
    $envRestore = [System.Environment]::GetEnvironmentVariable('IDL_BACKUP_PASSPHRASE', [System.EnvironmentVariableTarget]::Process)
    if (-not [string]::IsNullOrEmpty($envRestore)){
        [System.Environment]::SetEnvironmentVariable('IDL_BACKUP_PASSPHRASE', $null, [System.EnvironmentVariableTarget]::Process)
        if ($null -eq $RestorePassphrase){
            $RestorePassphrase = ConvertTo-SecureString -String $envRestore -AsPlainText -Force
        }
    }

    $envRestore = $null

    # And the GitHub token (-FromGitHub): it is only ever put into the download requests.
    $envToken = [System.Environment]::GetEnvironmentVariable('IDL_GITHUB_TOKEN', [System.EnvironmentVariableTarget]::Process)
    if (-not [string]::IsNullOrEmpty($envToken)){
        [System.Environment]::SetEnvironmentVariable('IDL_GITHUB_TOKEN', $null, [System.EnvironmentVariableTarget]::Process)
        if ($null -eq $GitHubToken){
            $GitHubToken = ConvertTo-SecureString -String $envToken -AsPlainText -Force
        }
    }

    $envToken = $null

    Start-IDLRun -ScriptName 'Install-IDLifecycle' -ScriptRoot $PSScriptRoot -LogDir $script:SetupLogRoot -IsAction -DryRun:$DryRun -Parameters @{
        SourceDir            = $SourceDir
        FromGitHub           = [bool]$FromGitHub
        GitHubRepo           = $GitHubRepo
        GitHubRef            = $GitHubRef
        GitHubToken          = (($null -ne $GitHubToken) ? ('(set)') : ('(not set)'))
        RestoreFrom          = $RestoreFrom
        RestorePassphrase    = (($null -ne $RestorePassphrase) ? ('(set)') : ('(not set)'))
        RestoreConfig        = [bool]$RestoreConfig
        InstallDir           = $InstallDir
        DataDir              = $DataDir
        ServiceAccount       = $ServiceAccount
        ServiceName          = $ServiceName
        DbDriver             = $DbDriver
        DbDsn                = (($null -ne $DbDsn) ? ('(set)') : ('(not set)'))
        DbDsnEnvVar          = $DbDsnEnvVar
        Listen               = $Listen
        TlsCertFile          = $TlsCertFile
        TlsKeyFile           = $TlsKeyFile
        AdminUser            = $AdminUser
        AdminPassword        = (($null -ne $AdminPassword) ? ('(set)') : ('(not set)'))
        ServiceAccountPassword = (($null -ne $ServiceAccountPassword) ? ('(set)') : ('(not set)'))
        Start               = [bool]$Start
        RequireSignature     = [bool]$RequireSignature
        NoTray               = [bool]$NoTray
        InstallPrerequisites = [bool]$InstallPrerequisites
        NonInteractive       = [bool]$NonInteractive
        Force                = [bool]$Force
        DryRun               = [bool]$DryRun
    }

    # Secrets are registered before anything else can log them (Start-IDLRun clears the list).
    $adminPlain = ConvertFrom-IDLSecure -Value $AdminPassword
    Register-IDLSecret -Secret $adminPlain
    $dsnPlain = ConvertFrom-IDLSecure -Value $DbDsn
    Register-IDLDsnSecrets -Dsn $dsnPlain
    $servicePlain = ConvertFrom-IDLSecure -Value $ServiceAccountPassword
    Register-IDLSecret -Secret $servicePlain
    $tokenPlain = ConvertFrom-IDLSecure -Value $GitHubToken
    Register-IDLSecret -Secret $tokenPlain
    $restorePlain = ConvertFrom-IDLSecure -Value $RestorePassphrase
    Register-IDLSecret -Secret $restorePlain

    if ($TestHook){
        Write-IDLLog "TEST HOOK loaded: $TestHook (system-touching functions are replaced)" 'WARN'
    }

    Write-IDLLog "Setup log folder: $($script:SetupLogRoot)"
    $interactive = Test-IDLSetupInteractive

    # ---- GitHub source or a release zip: stage it, then the staged release's own installer does everything else ----
    if (
        ($FromGitHub) -or
        ($SourceDir -match '\.zip$')
    ){
        if (
            ($FromGitHub) -and
            ($SourceDir)
        ){
            throw (New-IDLException -Kind Input -Message 'Use either -FromGitHub or -SourceDir, not both.')
        }

        $handOffSecrets = @{}
        foreach ($pair in @(@('IDL_PASSWORD', $adminPlain), @('IDL_SERVICE_PASSWORD', $servicePlain), @('IDL_SETUP_DB_DSN', $dsnPlain), @('IDL_BACKUP_PASSPHRASE', $restorePlain))){
            if (-not [string]::IsNullOrEmpty($pair[1])){
                $handOffSecrets[$pair[0]] = $pair[1]
            }
        }

        $exitCode = Invoke-IDLSetupFromStaging -Bound $PSBoundParameters -Secrets $handOffSecrets -TokenPlain $tokenPlain
        $handOffSecrets = $null
        # Nothing else to do here; exit still runs the finally block below (log summary, secrets cleared).
        exit $exitCode
    }

    # ---- Phase: validation (exit 2) - nothing is touched before this has passed ----
    $phase = Start-IDLPhase -Name 'Validate input'

    if (
        ($PSVersionTable.PSEdition -ne 'Core') -or
        (-not $IsWindows)
    ){
        throw (New-IDLException -Kind Dependency -Message 'This installer only runs on Windows with PowerShell 7+.')
    }

    if ($ServiceName -notmatch '^[A-Za-z0-9_.\-]{1,80}$'){
        throw (New-IDLException -Kind Input -Message "ServiceName '$ServiceName' is not valid (letters, digits, '_', '.', '-' only).")
    }

    $DbDriver = $DbDriver.Trim().ToLowerInvariant()
    if ($DbDriver -notin @('sqlite', 'mssql', 'postgres', 'mysql', 'db2')){
        throw (New-IDLException -Kind Input -Message "DbDriver '$DbDriver' is not valid (sqlite, mssql, postgres, mysql, db2).")
    }

    foreach ($pair in @(@('InstallDir', $InstallDir), @('DataDir', $DataDir))){
        $reason = Get-IDLUnsafePathReason -Path $pair[1]
        if ($reason){
            throw (New-IDLException -Kind Input -Message "$($pair[0]) '$($pair[1])' refused: $reason.")
        }
    }

    $InstallDir = [System.IO.Path]::GetFullPath($InstallDir).TrimEnd('\')
    $DataDir = [System.IO.Path]::GetFullPath($DataDir).TrimEnd('\')
    if (
        (Test-IDLPathWithin -Child $DataDir -Parent $InstallDir) -or
        (Test-IDLPathWithin -Child $InstallDir -Parent $DataDir)
    ){
        throw (New-IDLException -Kind Input -Message "InstallDir '$InstallDir' and DataDir '$DataDir' must be separate folders (neither inside the other).")
    }

    if ([string]::IsNullOrWhiteSpace($SourceDir)){
        $SourceDir = Split-Path -Parent $PSScriptRoot
    }

    if (-not (Test-Path -LiteralPath $SourceDir -PathType Container)){
        throw (New-IDLException -Kind Input -Message "SourceDir not found: $SourceDir")
    }

    $SourceDir = (Get-Item -LiteralPath $SourceDir).FullName.TrimEnd('\')
    $installedScripts = Join-Path -Path $InstallDir -ChildPath 'scripts'
    $configPath = Join-Path -Path $DataDir -ChildPath 'config.json'
    $configExists = Test-Path -LiteralPath $configPath -PathType Leaf

    if ($Listen){
        $listenParts = Split-IDLListen -Value $Listen
        if ($null -eq $listenParts){
            throw (New-IDLException -Kind Input -Message "Listen '$Listen' is not host:port.")
        }

        if (
            (-not (Test-IDLLoopbackHost -HostName $listenParts.Host)) -and
            ((-not $TlsCertFile) -or (-not $TlsKeyFile))
        ){
            throw (New-IDLException -Kind Input -Message "Listen '$Listen' is not a loopback address: -TlsCertFile and -TlsKeyFile are required (the GUI must not be served over plain HTTP on the network).")
        }
    }

    if ([bool]$TlsCertFile -ne [bool]$TlsKeyFile){
        throw (New-IDLException -Kind Input -Message '-TlsCertFile and -TlsKeyFile must be given together.')
    }

    foreach ($tlsFile in @($TlsCertFile, $TlsKeyFile)){
        if (
            ($tlsFile) -and
            (-not (Test-Path -LiteralPath $tlsFile -PathType Leaf))
        ){
            throw (New-IDLException -Kind Input -Message "TLS file not found: $tlsFile")
        }
    }

    # The existing config.json (never overwritten) decides the driver; -DbDriver only applies to a new one.
    $existingConfig = $null
    if ($configExists){
        try{
            $existingConfig = Get-Content -LiteralPath $configPath -Raw -Encoding utf8 | ConvertFrom-Json -AsHashtable
        }
        catch{
            throw (New-IDLException -Kind Input -Message "Existing config.json is not valid JSON: $configPath (it is never overwritten; fix or move it).")
        }
    }

    $effectiveDriver = $DbDriver
    $existingDsnText = ''
    $configFix = [ordered]@{}
    if ($null -ne $existingConfig){
        $existingDb = Get-IDLField -Object $existingConfig -Name 'database'
        $configuredDriver = [string](Get-IDLField -Object $existingDb -Name 'driver')
        $effectiveDriver = (([string]::IsNullOrWhiteSpace($configuredDriver)) ? ('sqlite') : ($configuredDriver.ToLowerInvariant()))
        $existingDsnText = [string](Get-IDLField -Object $existingDb -Name 'dsn')
        Register-IDLDsnSecrets -Dsn $existingDsnText

        # An existing config must point at THIS install: another scripts_dir would make the service run other scripts.
        $cfgScriptsDir = [string](Get-IDLField -Object (Get-IDLField -Object $existingConfig -Name 'powershell') -Name 'scripts_dir')
        $cfgDataDir = [string](Get-IDLField -Object $existingConfig -Name 'data_dir')
        $sameScripts = (
            (-not [string]::IsNullOrWhiteSpace($cfgScriptsDir)) -and
            ([System.IO.Path]::GetFullPath($cfgScriptsDir).TrimEnd('\').Equals($installedScripts, [System.StringComparison]::OrdinalIgnoreCase))
        )
        $sameData = (
            (-not [string]::IsNullOrWhiteSpace($cfgDataDir)) -and
            ([System.IO.Path]::GetFullPath($cfgDataDir).TrimEnd('\').Equals($DataDir, [System.StringComparison]::OrdinalIgnoreCase))
        )
        if (-not $sameScripts){
            $configFix['powershell.scripts_dir'] = $installedScripts
        }

        if (-not $sameData){
            $configFix['data_dir'] = $DataDir
        }

        if (
            ($configFix.Count -gt 0) -and
            (-not $Force)
        ){
            throw (New-IDLException -Kind Input -Message ("$configPath does not point at this install (powershell.scripts_dir '$cfgScriptsDir', data_dir '$cfgDataDir'; expected '$installedScripts' and '$DataDir'). " +
                    'Re-run with -Force to correct exactly these two fields, or fix config.json by hand.'))
        }
    }

    if (
        ($effectiveDriver -eq 'sqlite') -and
        (($null -ne $DbDsn) -or ($DbDsnEnvVar))
    ){
        throw (New-IDLException -Kind Input -Message 'sqlite uses <DataDir>\idlifecycle.db; -DbDsn/-DbDsnEnvVar are for server databases (mssql, postgres, mysql, db2).')
    }

    # Where the server-DB connection string lives: 'config' (default: database.dsn in config.json inside the
    # locked-down data dir) or 'env' (explicit -DbDsnEnvVar opt-in: a machine environment variable).
    $dsnMode = 'none'
    if ($DbDsnEnvVar){
        $dsnMode = 'env'
    }
    elseif ($null -ne $DbDsn){
        $dsnMode = 'config'
    }

    if (
        ($dsnMode -eq 'config') -and
        ($configExists) -and
        ($existingDsnText -cne $script:DsnPlaceholder) -and
        ($existingDsnText -cne $dsnPlain) -and
        (-not $Force)
    ){
        throw (New-IDLException -Kind Input -Message "$configPath already holds a different database connection string; re-run with -Force to replace it.")
    }

    if (
        ($DbDsnEnvVar) -and
        ($DbDsnEnvVar -cnotmatch '^IDL_[A-Za-z0-9_]{1,120}$')
    ){
        throw (New-IDLException -Kind Input -Message "DbDsnEnvVar '$DbDsnEnvVar' is not allowed: the name must match ^IDL_[A-Za-z0-9_]+`$ (so the uninstaller can never remove an unrelated variable).")
    }

    if ($DbDsnEnvVar -in @('IDL_PASSWORD', 'IDL_SERVICE_PASSWORD', 'IDL_SETUP_DB_DSN', 'IDL_GITHUB_TOKEN', 'IDL_BACKUP_PASSPHRASE')){
        throw (New-IDLException -Kind Input -Message "DbDsnEnvVar '$DbDsnEnvVar' is reserved for passing secrets to the installer; pick another name (e.g. IDL_DB_DSN).")
    }

    if (
        ($DbDriver -ne 'sqlite') -and
        (-not $configExists) -and
        ($dsnMode -eq 'none')
    ){
        throw (New-IDLException -Kind Input -Message "DbDriver $DbDriver needs a connection string: pass -DbDsn (SecureString; stored in config.json) or -DbDsnEnvVar <machine environment variable>.")
    }

    if ([string]::IsNullOrWhiteSpace($ServiceAccount)){
        if (-not $interactive){
            throw (New-IDLException -Kind Input -Message "-ServiceAccount is required (a gMSA/MSA such as 'CORP\svc-idl`$', a user account such as 'CORP\svc-idl' with -ServiceAccountPassword, or NT AUTHORITY\NetworkService / LocalSystem).")
        }

        $ServiceAccount = ([string](Read-IDLSetupText -Prompt "Service account (gMSA/MSA DOMAIN\name`$, user DOMAIN\name, or built-in)")).Trim()
    }

    $ServiceAccount = ConvertTo-IDLServiceAccountName -Account $ServiceAccount
    $accountKind = Get-IDLServiceAccountKind -Account $ServiceAccount
    if (-not $accountKind){
        throw (New-IDLException -Kind Input -Message "ServiceAccount '$ServiceAccount' is not valid: use a gMSA/MSA (DOMAIN\name`$), a user account (DOMAIN\name, .\name or name@domain) or a built-in account (LocalSystem, NT AUTHORITY\NetworkService, NT SERVICE\<name>).")
    }

    $isManaged = ($accountKind -eq 'Managed')
    $isPasswordAccount = ($accountKind -eq 'Password')
    if (
        (-not $isPasswordAccount) -and
        ($null -ne $servicePlain)
    ){
        throw (New-IDLException -Kind Input -Message "-ServiceAccountPassword is only for a user account; '$ServiceAccount' is a $(($isManaged) ? ('managed service account (Windows manages its password)') : ('built-in account')).")
    }

    if ($isPasswordAccount){
        # A password is needed when the service is new or moves to this account; otherwise the stored one is kept.
        $currentService = Get-IDLSetupService -Name $ServiceName
        $needsServicePassword = (
            ($null -eq $currentService) -or
            (-not ([string]$currentService.StartName -ieq $ServiceAccount))
        )
        if (
            $needsServicePassword -and
            ($null -eq $servicePlain) -and
            (-not $DryRun)
        ){
            if (-not $interactive){
                throw (New-IDLException -Kind Input -Message "ServiceAccount '$ServiceAccount' is a user account: pass -ServiceAccountPassword (SecureString) or set IDL_SERVICE_PASSWORD. (A gMSA such as DOMAIN\name`$ needs no password.)")
            }

            $ServiceAccountPassword = Read-IDLSetupPassword -Prompt "Password for service account '$ServiceAccount'"
            $servicePlain = ConvertFrom-IDLSecure -Value $ServiceAccountPassword
            Register-IDLSecret -Secret $servicePlain
        }

        if (
            ($null -ne $servicePlain) -and
            ($servicePlain.Length -eq 0)
        ){
            throw (New-IDLException -Kind Input -Message 'ServiceAccountPassword is empty.')
        }

        Write-IDLLog "Service account '$ServiceAccount' is a user account with a password. If the password expires or is changed in AD, re-run with the new -ServiceAccountPassword or the service will not start. A gMSA avoids this." 'WARN'
    }

    $aclAccount = (($ServiceAccount -eq 'LocalSystem') ? ('NT AUTHORITY\SYSTEM') : ($ServiceAccount))

    if ($AdminUser -notmatch '^[A-Za-z0-9._@\-]{1,64}$'){
        throw (New-IDLException -Kind Input -Message "AdminUser '$AdminUser' is not valid (letters, digits, '.', '_', '@', '-').")
    }

    if (
        ($null -ne $adminPlain) -and
        ($adminPlain.Length -lt 12)
    ){
        throw (New-IDLException -Kind Input -Message 'AdminPassword must be at least 12 characters.')
    }

    # A brand-new config has no administrator yet: make sure we can create one before changing anything (a restored
    # backup brings its own administrators).
    if (
        (-not $configExists) -and
        (-not $DryRun) -and
        (-not $RestoreFrom) -and
        ($null -eq $adminPlain)
    ){
        if (-not $interactive){
            throw (New-IDLException -Kind Input -Message "No administrator can exist yet: pass -AdminPassword (SecureString) or set IDL_PASSWORD for '$AdminUser'.")
        }

        $AdminPassword = Read-IDLSetupPassword -Prompt "Password for GUI administrator '$AdminUser' (min 12 characters)"
        $adminPlain = ConvertFrom-IDLSecure -Value $AdminPassword
        Register-IDLSecret -Secret $adminPlain
        if ($adminPlain.Length -lt 12){
            throw (New-IDLException -Kind Input -Message 'AdminPassword must be at least 12 characters.')
        }
    }

    # A backup to restore: the file must exist; its passphrase is needed for a real run (the header is checked in the
    # preflight, once the release's idlifecycled.exe is trusted).
    if ($RestoreFrom){
        if (-not (Test-Path -LiteralPath $RestoreFrom -PathType Leaf)){
            throw (New-IDLException -Kind Input -Message "RestoreFrom not found: $RestoreFrom")
        }

        $RestoreFrom = (Get-Item -LiteralPath $RestoreFrom).FullName
        if (
            ($null -eq $restorePlain) -and
            (-not $DryRun)
        ){
            if (-not $interactive){
                throw (New-IDLException -Kind Input -Message 'RestoreFrom needs the backup passphrase: pass -RestorePassphrase (SecureString) or set IDL_BACKUP_PASSPHRASE.')
            }

            $RestorePassphrase = Read-IDLSetupPassword -Prompt "Passphrase of the backup '$(Split-Path -Leaf $RestoreFrom)'"
            $restorePlain = ConvertFrom-IDLSecure -Value $RestorePassphrase
            Register-IDLSecret -Secret $restorePlain
        }
    }
    elseif ($RestoreConfig){
        throw (New-IDLException -Kind Input -Message '-RestoreConfig only applies together with -RestoreFrom.')
    }

    Write-IDLLog "Source '$SourceDir' -> '$InstallDir'; data '$DataDir'; service '$ServiceName' as '$ServiceAccount'; config $((($configExists) ? ('exists (kept)') : ('will be created')))"
    Stop-IDLPhase -Phase $phase

    # ---- Phase: release trust (exit 2) - before ANYTHING from SourceDir is run ----
    $phase = Start-IDLPhase -Name 'Release trust'
    $release = Get-IDLTrustedRelease -Root $SourceDir
    $sourceExe = $release.Exe
    if ($release.Unverified){
        Write-IDLLog "UNVERIFIED INSTALL (-SkipIntegrityCheck, least safe): $($script:ManifestName) was not checked, so nothing proves these $($release.Files.Count) file(s) are the ones that were built or reviewed. Use a release built by New-IDLifecycleRelease.ps1 for production." 'WARN'
        Write-IDLLog "Release '$SourceDir': folder and parents restricted; file list and hashes taken from the folder itself"
        Add-IDLReportRow -Action 'IntegrityCheck' -Result 'Skipped' -Identity $SourceDir -Details "-SkipIntegrityCheck: $($release.Files.Count) file(s) installed without a manifest check (least safe)"
    }
    else{
        Write-IDLLog "Release '$SourceDir': folder and parents restricted; $($release.Files.Count) file(s) match $($script:ManifestName)"
    }
    if ($release.Signature.Status -eq 'Valid'){
        Write-IDLLog "idlifecycled.exe is Authenticode-signed by: $($release.Signature.Signer)"
    }
    else{
        Write-IDLLog "idlifecycled.exe signature: $($release.Signature.Status) (use -RequireSignature to insist on a signed binary)"
    }

    Stop-IDLPhase -Phase $phase

    # ---- Phase: preflight (exit 2 / 3 / 4) ----
    $phase = Start-IDLPhase -Name 'Preflight'

    if (
        (-not $DryRun) -and
        (-not (Test-IDLSetupElevated))
    ){
        throw (New-IDLException -Kind Connection -Message 'Run this script from an elevated (Administrator) session (or use -DryRun).')
    }

    # Pre-created folders/files: an untrusted owner can rewrite the ACL at any time, so it is refused outright.
    $ownerChecks = @()
    if (Test-Path -LiteralPath $DataDir){
        $ownerChecks += $DataDir
        foreach ($name in @('config.json', 'secret.key', 'idlifecycle.db', 'idlifecycle.db-wal', 'idlifecycle.db-shm')){
            $candidate = Join-Path -Path $DataDir -ChildPath $name
            if (Test-Path -LiteralPath $candidate){
                $ownerChecks += $candidate
            }
        }
    }

    if (Test-Path -LiteralPath $InstallDir){
        $ownerChecks += $InstallDir
    }

    foreach ($candidate in $ownerChecks){
        $owner = Get-IDLPathOwnerSid -Path $candidate
        if ($owner -notin (Get-IDLTrustedOwnerSids)){
            throw (New-IDLException -Kind Input -Message ("'$candidate' already exists and is owned by $(ConvertTo-IDLAccountName -Sid $owner), not by SYSTEM, Administrators, TrustedInstaller or you. " +
                    'Delete it if you did not create it (or take ownership as an administrator: takeown /a /r /f) and re-run.'))
        }
    }

    $adModule = Test-IDLSetupAdModule
    if (-not $adModule){
        $hint = 'The ActiveDirectory PowerShell module is missing. Install it with: Install-WindowsFeature RSAT-AD-PowerShell (or re-run with -InstallPrerequisites).'
        if (-not $InstallPrerequisites){
            throw (New-IDLException -Kind Dependency -Message $hint)
        }

        Invoke-SetupStep -Action 'InstallPrerequisite' -Identity 'RSAT-AD-PowerShell' -Details 'Install-WindowsFeature' `
            -IsSatisfied { $false } `
            -Apply { Install-IDLSetupFeature -Name 'RSAT-AD-PowerShell' }
        Assert-NoFailedSteps -Phase 'Preflight'
        $adModule = ((-not $DryRun) ? (Test-IDLSetupAdModule) : ($false))
        if (
            (-not $DryRun) -and
            (-not $adModule)
        ){
            throw (New-IDLException -Kind Dependency -Message 'RSAT-AD-PowerShell was installed but the ActiveDirectory module is still not available; a reboot may be required.')
        }
    }
    else{
        Write-IDLLog 'ActiveDirectory module: available'
    }

    if ($isManaged){
        if ($adModule){
            $sam = (($ServiceAccount -match '\\') ? ($ServiceAccount.Split('\')[1]) : ($ServiceAccount.Split('@')[0]))
            $gmsaOk = $false
            $gmsaError = ''
            try{
                $gmsaOk = Test-IDLSetupGmsa -SamAccountName $sam
            }
            catch{
                $gmsaError = $_.Exception.Message
            }

            if (-not $gmsaOk){
                throw (New-IDLException -Kind Dependency -Message ("Managed service account '$ServiceAccount' is not usable on this host (Test-ADServiceAccount returned false{0}). " +
                        "gMSA: allow this computer to retrieve its password (Set-ADServiceAccount $sam -PrincipalsAllowedToRetrieveManagedPassword <this computer or a group it is in>; a group change needs a reboot or 'klist -li 0x3e7 purge'). " +
                        "Standalone MSA: link it to this computer (Add-ADComputerServiceAccount -Identity $env:COMPUTERNAME -ServiceAccount $sam). " +
                        "Then run Install-ADServiceAccount $sam and re-run this script." -f (($gmsaError) ? (": $gmsaError") : (''))))
            }

            Write-IDLLog "Managed service account '$ServiceAccount': Test-ADServiceAccount OK"
        }
        else{
            Write-IDLLog "Managed service account '$ServiceAccount' not verified (ActiveDirectory module not installed yet in this dry run)" 'WARN'
        }
    }

    if (
        $isPasswordAccount -and
        ($null -ne $ServiceAccountPassword)
    ){
        # A wrong password would only show up when the service fails to start, after everything else has changed.
        $credentialOk = $null
        try{
            $credentialOk = Test-IDLSetupCredential -Account $ServiceAccount -Password $ServiceAccountPassword
        }
        catch{
            Write-IDLLog "Could not verify the password of '$ServiceAccount' ($($_.Exception.Message)); continuing - the service start at the end shows whether it works." 'WARN'
        }

        if ($credentialOk -eq $false){
            throw (New-IDLException -Kind Dependency -Message "The password for service account '$ServiceAccount' was rejected (wrong password, or the account is disabled, locked or expired). Nothing was changed.")
        }

        if ($credentialOk){
            Write-IDLLog "Service account '$ServiceAccount': password verified"
        }
    }

    if ($effectiveDriver -eq 'db2'){
        $ibm = @(Get-IDLSetupOdbcDrivers | Where-Object { $_ -match 'IBM' })
        if ($ibm.Count -eq 0){
            throw (New-IDLException -Kind Dependency -Message 'DbDriver db2 needs the IBM Data Server Driver for ODBC and CLI (64-bit); no 64-bit ODBC driver whose name contains "IBM" is installed (Get-OdbcDriver -Platform 64-bit).')
        }

        Write-IDLLog "IBM ODBC driver(s): $($ibm -join ', ')"
    }

    if (
        ($DbDsnEnvVar) -and
        ($null -eq $dsnPlain)
    ){
        $existingDsn = Get-IDLMachineEnvVar -Name $DbDsnEnvVar
        if ([string]::IsNullOrEmpty($existingDsn)){
            throw (New-IDLException -Kind Input -Message "Machine environment variable '$DbDsnEnvVar' does not exist; pass -DbDsn (SecureString) to create it.")
        }

        Register-IDLDsnSecrets -Dsn $existingDsn
        $existingDsn = $null
    }

    $sourceVersion = Get-IDLDaemonVersion -Exe $sourceExe
    $installedExe = Join-Path -Path $InstallDir -ChildPath 'idlifecycled.exe'
    $installedVersion = $null
    if (Test-Path -LiteralPath $installedExe -PathType Leaf){
        try{
            $installedVersion = Get-IDLDaemonVersion -Exe $installedExe
        }
        catch{
            $installedVersion = '(unknown)'
            Write-IDLLog "Installed idlifecycled.exe did not report a version: $($_.Exception.Message)" 'WARN'
        }
    }

    $programFiles = @($release.Files) + @(@{ Relative = $script:ManifestName; Source = $release.Manifest; Hash = (Get-FileHash -LiteralPath $release.Manifest -Algorithm SHA256).Hash })
    $changedFiles = Get-IDLChangedProgramFiles -Files $programFiles -TargetDir $InstallDir
    $changedCode = @($changedFiles | Where-Object { $_.Relative -ne $script:ManifestName })
    $installKind = 'same'
    if ($null -eq $installedVersion){
        $installKind = 'install'
    }
    elseif ($changedCode.Count -gt 0){
        $installKind = (($installedVersion -ne $sourceVersion) ? ('upgrade') : ('update'))
    }

    # Files in InstallDir\scripts that this release does not ship (Logs excluded) are never unblocked or kept.
    $known = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::OrdinalIgnoreCase)
    foreach ($f in $programFiles){
        $null = $known.Add($f.Relative)
    }

    $unknownFiles = @()
    if (Test-Path -LiteralPath $installedScripts -PathType Container){
        $unknownFiles = @(Get-ChildItem -LiteralPath $installedScripts -Recurse -File -Force |
                ForEach-Object { 'scripts\' + $_.FullName.Substring($installedScripts.Length).TrimStart('\') } |
                Where-Object { (-not $known.Contains($_)) -and (('\' + $_ + '\') -notmatch '\\Logs\\') })
    }

    $versionText = switch ($installKind){
        'install' { "install $sourceVersion" }
        'upgrade' { "upgrade $installedVersion -> $sourceVersion" }
        'update' { "same version $sourceVersion, $($changedCode.Count) file(s) differ" }
        default { "same version $sourceVersion, all files identical" }
    }
    Write-IDLLog "Program files: $versionText"

    if (
        ($installKind -in @('upgrade', 'update')) -and
        (-not $Force)
    ){
        throw (New-IDLException -Kind Input -Message "InstallDir already holds idlifecycled $installedVersion and $($changedCode.Count) file(s) differ from SourceDir ($versionText). Re-run with -Force to replace them.")
    }

    if (
        ($unknownFiles.Count -gt 0) -and
        (-not $Force)
    ){
        throw (New-IDLException -Kind Input -Message "InstallDir\scripts contains file(s) this release does not ship: $($unknownFiles -join ', '). Re-run with -Force to remove them.")
    }

    # The backup must be one this release can read (header only; no passphrase needed).
    if ($RestoreFrom){
        $inspect = Invoke-IDLDaemon -Exe $sourceExe -Arguments @('restore', '-in', $RestoreFrom, '-inspect')
        if ($inspect.ExitCode -ne 0){
            throw (New-IDLException -Kind Input -Message "RestoreFrom '$RestoreFrom' cannot be used: $($inspect.StdErr.Trim())")
        }

        foreach ($line in @($inspect.StdOut -split "`r?`n" | Where-Object { $_ })){
            Write-IDLLog "  backup: $line"
        }
    }

    Stop-IDLPhase -Phase $phase

    # ---- Phase: program files ----
    $phase = Start-IDLPhase -Name 'Program files'
    $service = Get-IDLSetupService -Name $ServiceName
    $wasRunning = $false
    if (
        (($changedFiles.Count -gt 0) -or ($unknownFiles.Count -gt 0)) -and
        ($null -ne $service) -and
        ($service.State -ne 'Stopped')
    ){
        $wasRunning = $true
        Invoke-SetupStep -Action 'StopService' -Identity $ServiceName -Details "for the $installKind (started again afterwards)" `
            -IsSatisfied { $false } `
            -Apply {
            Stop-IDLSetupService -Name $ServiceName
            $script:StoppedForUpdate = $true
            Wait-IDLSetupServiceState -Name $ServiceName -State 'Stopped' -TimeoutSec 60
        }
        Assert-NoFailedSteps -Phase 'Program files'
    }

    Invoke-SetupStep -Action 'CreateDirectory' -Identity $InstallDir `
        -IsSatisfied { Test-Path -LiteralPath $InstallDir -PathType Container } `
        -Apply { $null = New-Item -Path $InstallDir -ItemType Directory -Force }

    Invoke-SetupStep -Action 'CopyProgramFiles' -Identity $InstallDir -Details "$versionText ($($changedFiles.Count) file(s))" `
        -SatisfiedReason "same version $sourceVersion, all files identical" `
        -IsSatisfied { $changedFiles.Count -eq 0 } `
        -Apply {
        foreach ($f in $changedFiles){
            $parent = Split-Path -Parent $f.Target
            if (-not (Test-Path -LiteralPath $parent -PathType Container)){
                $null = New-Item -Path $parent -ItemType Directory -Force
            }

            Copy-Item -LiteralPath $f.Source -Destination $f.Target -Force
            if ((Get-FileHash -LiteralPath $f.Target -Algorithm SHA256).Hash -ne $f.Hash){
                throw "'$($f.Relative)' does not match the manifest after copying."
            }

            Write-IDLLog "  copied $($f.Relative)"
        }
    }
    Assert-NoFailedSteps -Phase 'Program files'

    if ($unknownFiles.Count -gt 0){
        Invoke-SetupStep -Action 'RemoveUnknownFiles' -Identity $installedScripts -Details "$($unknownFiles.Count) file(s) not in the release: $($unknownFiles -join ', ')" `
            -IsSatisfied { $false } `
            -Apply {
            foreach ($rel in $unknownFiles){
                $target = Join-Path -Path $InstallDir -ChildPath $rel
                if ((([System.IO.File]::GetAttributes($target)) -band [System.IO.FileAttributes]::ReparsePoint) -ne 0){
                    throw "'$target' is a symbolic link; remove it by hand."
                }

                [System.IO.File]::Delete($target)
                Write-IDLLog "  removed $rel"
            }
        }
    }

    # Unblock only files that are part of this release and match the manifest (this run's copies, or identical ones).
    $toUnblock = @()
    foreach ($f in $release.Files){
        $target = Join-Path -Path $InstallDir -ChildPath $f.Relative
        $wouldCopy = @($changedFiles | Where-Object { $_.Relative -eq $f.Relative }).Count -gt 0
        if ($DryRun -and $wouldCopy){
            if (Test-IDLHasZoneId -Path $f.Source){
                $toUnblock += $target
            }

            continue
        }

        if (
            (Test-Path -LiteralPath $target -PathType Leaf) -and
            (Test-IDLHasZoneId -Path $target) -and
            ((Get-FileHash -LiteralPath $target -Algorithm SHA256).Hash -eq $f.Hash)
        ){
            $toUnblock += $target
        }
    }

    Invoke-SetupStep -Action 'UnblockProgramFiles' -Identity $InstallDir -Details "Unblock-File on $($toUnblock.Count) verified release file(s)" `
        -SatisfiedReason 'no blocked files' `
        -IsSatisfied { $toUnblock.Count -eq 0 } `
        -Apply {
        foreach ($file in $toUnblock){
            Unblock-File -LiteralPath $file
            Write-IDLLog "  unblocked $file"
        }

        $still = @($toUnblock | Where-Object { Test-IDLHasZoneId -Path $_ })
        if ($still.Count -gt 0){
            throw "$($still.Count) file(s) are still blocked: $($still -join ', ')"
        }
    }

    Invoke-SetupStep -Action 'LockdownInstallDir' -Identity $InstallDir -Details "inheritance off; SYSTEM, Administrators, TrustedInstaller FullControl; Users and $ServiceAccount ReadAndExecute; owner Administrators" `
        -IsSatisfied { Test-IDLInstallDirAcl -Path $InstallDir -ServiceAccount $aclAccount } `
        -Apply { Set-IDLInstallDirAcl -Path $InstallDir -ServiceAccount $aclAccount }

    Assert-NoFailedSteps -Phase 'Program files'
    Stop-IDLPhase -Phase $phase

    # ---- Phase: configuration ----
    $phase = Start-IDLPhase -Name 'Configuration'
    $dsnEnvChanged = $false

    if (
        ($dsnMode -eq 'env') -and
        ($null -ne $dsnPlain)
    ){
        $readers = @(Get-IDLMachineEnvReaders)
        $dsnEnvChanged = ((Get-IDLMachineEnvVar -Name $DbDsnEnvVar) -cne $dsnPlain)
        Invoke-SetupStep -Action 'SetDbDsnEnvVar' -Identity "Machine:$DbDsnEnvVar" -Details 'connection string (value never logged)' `
            -SatisfiedReason 'already holds this connection string' `
            -IsSatisfied { -not $dsnEnvChanged } `
            -Apply { Set-IDLMachineEnvVar -Name $DbDsnEnvVar -Value $dsnPlain }

        $readerText = (($readers.Count -gt 0) ? ($readers -join ', ') : ('nobody besides SYSTEM/Administrators'))
        Write-IDLLog ("Machine environment variable '$DbDsnEnvVar' is NOT a secret store: the registry key HKLM\SYSTEM\CurrentControlSet\Control\Session Manager\Environment is readable by $readerText, " +
            'and Windows copies every machine variable into the environment of every process started afterwards (any local user can read it with Get-ChildItem env:). ' +
            'Prefer the default (omit -DbDsnEnvVar: the DSN is kept in the locked-down config.json), or Windows integrated auth (mssql without user:pass@).') 'WARN'
        $script:DaemonEnv[$DbDsnEnvVar] = $dsnPlain
    }

    if ($configExists){
        foreach ($ignored in @(@('DbDriver', ($DbDriver -ne 'sqlite')), @('Listen', [bool]$Listen), @('TlsCertFile', [bool]$TlsCertFile))){
            if (
                ($ignored[1]) -and
                ($PSBoundParameters.ContainsKey($ignored[0]))
            ){
                Write-IDLLog "-$($ignored[0]) is ignored: the existing $configPath is kept as it is (edit it by hand to change it)." 'WARN'
            }
        }
    }

    $initArgs = @('init', '-dir', $DataDir, '-scripts', $installedScripts)
    if ($DbDriver -ne 'sqlite'){
        # The real DSN never goes on a command line: config mode gets a placeholder that is replaced in config.json
        # once lockdown has restricted the data dir (SetConfigDsn below).
        $initDsn = (($dsnMode -eq 'env') ? ("env:$DbDsnEnvVar") : ($script:DsnPlaceholder))
        $initArgs += @('-db-driver', $DbDriver, '-db-dsn', $initDsn)
    }

    if ($Listen){
        $initArgs += @('-listen', $Listen)
    }

    if ($TlsCertFile){
        $initArgs += @('-tls-cert', $TlsCertFile, '-tls-key', $TlsKeyFile)
    }

    Invoke-SetupStep -Action 'InitConfig' -Identity $configPath -Details ($initArgs[1..($initArgs.Count - 1)] -join ' ') `
        -SatisfiedReason 'kept existing config.json (never overwritten)' `
        -IsSatisfied { Test-Path -LiteralPath $configPath -PathType Leaf } `
        -Apply {
        $r = Invoke-IDLDaemon -Exe $installedExe -Arguments $initArgs
        Write-IDLDaemonOutput -Result $r
        if ($r.ExitCode -ne 0){
            throw "idlifecycled init failed with exit code $($r.ExitCode): $($r.StdErr.Trim())"
        }

        if (-not (Test-Path -LiteralPath $configPath -PathType Leaf)){
            throw "idlifecycled init reported success but $configPath does not exist."
        }
    }
    Assert-NoFailedSteps -Phase 'Configuration'

    # -Force: point an existing config.json at this install BEFORE the lockdown - `idlifecycled lockdown` restricts the
    # folder named in data_dir, so a wrong data_dir would lock down the wrong folder. These two fields hold no secret
    # (the DSN is written only after the lockdown, below); the rewrite is atomic in the folder the file already lives in.
    if ($configFix.Count -gt 0){
        Invoke-SetupStep -Action 'SetConfigPaths' -Identity $configPath -Details "-Force: $(@($configFix.Keys) -join ', ') -> this install" `
            -IsSatisfied { $false } `
            -Apply {
            Set-IDLConfigFields -Path $configPath -Fields $configFix
            $script:ConfigChanged = $true
        }
        Assert-NoFailedSteps -Phase 'Configuration'
    }

    # Effective settings: from config.json when it exists (always in a live run by now), else what init would write.
    $cfgListen = (($Listen) ? ($Listen) : ('127.0.0.1:8443'))
    $cfgTls = [bool]$TlsCertFile
    $cfgLogLevel = 'info'
    $cfgPolicy = 'RemoteSigned'
    $cfgDsn = (($dsnMode -eq 'env') ? ("env:$DbDsnEnvVar") : (''))
    if (Test-Path -LiteralPath $configPath -PathType Leaf){
        $cfg = Get-Content -LiteralPath $configPath -Raw -Encoding utf8 | ConvertFrom-Json -AsHashtable
        $cfgListen = [string](Get-IDLField -Object $cfg -Name 'listen' -Default '')
        if ([string]::IsNullOrWhiteSpace($cfgListen)){
            $cfgListen = '127.0.0.1:8443'
        }

        $cfgTls = -not [string]::IsNullOrWhiteSpace([string](Get-IDLField -Object (Get-IDLField -Object $cfg -Name 'tls') -Name 'cert_file'))
        $cfgDsn = [string](Get-IDLField -Object (Get-IDLField -Object $cfg -Name 'database') -Name 'dsn')
        $level = [string](Get-IDLField -Object $cfg -Name 'log_level' -Default '')
        if ($level){
            $cfgLogLevel = $level
        }

        $policy = [string](Get-IDLField -Object (Get-IDLField -Object $cfg -Name 'powershell') -Name 'execution_policy')
        if ($policy){
            $cfgPolicy = $policy
        }
    }

    if (
        ($cfgDsn -match '^env:(?<name>[A-Za-z_][A-Za-z0-9_]*)$') -and
        ($cfgDsn -cne $script:DsnPlaceholder)
    ){
        $dsnVar = $Matches['name']
        if (-not $script:DaemonEnv.ContainsKey($dsnVar)){
            $value = [System.Environment]::GetEnvironmentVariable($dsnVar)
            if ([string]::IsNullOrEmpty($value)){
                $value = Get-IDLMachineEnvVar -Name $dsnVar
            }

            if ([string]::IsNullOrEmpty($value)){
                Write-IDLLog "config.json reads the DSN from env:$dsnVar but that variable is not set; the database check will fail." 'WARN'
            }
            else{
                Register-IDLDsnSecrets -Dsn $value
                $script:DaemonEnv[$dsnVar] = $value
            }

            $value = $null
        }
    }

    $lockdownArgs = @('lockdown', '-config', $configPath)
    if ($ServiceAccount -ne 'LocalSystem'){
        $lockdownArgs += @('-account', $ServiceAccount)
    }

    Invoke-SetupStep -Action 'LockdownDataDir' -Identity $DataDir -Details "SYSTEM, Administrators, current user; $ServiceAccount Modify" `
        -IsSatisfied { Test-IDLDirectoryLockedDown -Path $DataDir -ServiceAccount $aclAccount } `
        -Apply {
        $r = Invoke-IDLDaemon -Exe $installedExe -Arguments $lockdownArgs
        Write-IDLDaemonOutput -Result $r
        if ($r.ExitCode -ne 0){
            throw "idlifecycled lockdown failed with exit code $($r.ExitCode): $($r.StdErr.Trim())"
        }
    }

    # The connection string is only written into a data dir that is already restricted.
    Assert-NoFailedSteps -Phase 'Configuration'
    if ($dsnMode -eq 'config'){
        Invoke-SetupStep -Action 'SetConfigDsn' -Identity "$configPath database.dsn" -Details 'connection string (value never logged); atomic replace inside the restricted data dir' `
            -SatisfiedReason 'already holds this connection string' `
            -IsSatisfied { (Test-Path -LiteralPath $configPath -PathType Leaf) -and ((Get-IDLConfigValue -Path $configPath -Key 'database.dsn') -ceq $dsnPlain) } `
            -Apply {
            if (-not (Test-IDLDirectoryLockedDown -Path $DataDir -ServiceAccount $aclAccount)){
                throw "$DataDir is not restricted; refusing to write the connection string into it."
            }

            Set-IDLConfigFields -Path $configPath -Fields @{ 'database.dsn' = $dsnPlain }
            $script:ConfigChanged = $true
        }
    }

    Assert-NoFailedSteps -Phase 'Configuration'

    $sourceState = Test-IDLEventSource -Source $script:EventSourceName
    Invoke-SetupStep -Action 'RegisterEventSource' -Identity "Application/$($script:EventSourceName)" `
        -SatisfiedReason 'already registered' `
        -IsSatisfied { $sourceState -eq $true } `
        -Apply { New-IDLEventSource -Source $script:EventSourceName }

    Assert-NoFailedSteps -Phase 'Configuration'
    Stop-IDLPhase -Phase $phase

    # ---- Phase: database and first administrator ----
    $phase = Start-IDLPhase -Name 'Database and administrator'
    $adminExists = $false
    if ($RestoreFrom){
        Invoke-SetupStep -Action 'RestoreBackup' -Identity $RestoreFrom -Details "replaces the database contents and secret.key$((($RestoreConfig)) ? ('; config.json except data_dir, database, scripts_dir') : (''))$((($Force)) ? (' (-Force: also over existing data)') : (''))" `
            -IsSatisfied { $false } `
            -Apply {
            $svc = Get-IDLSetupService -Name $ServiceName
            if (
                ($null -ne $svc) -and
                ($svc.State -ne 'Stopped')
            ){
                Write-IDLLog "  stopping service '$ServiceName' for the restore (started again at the end)"
                Stop-IDLSetupService -Name $ServiceName
                Wait-IDLSetupServiceState -Name $ServiceName -State 'Stopped' -TimeoutSec 60
                $script:StoppedForUpdate = $true
            }

            $restoreArgs = @('restore', '-in', $RestoreFrom, '-config', $configPath)
            if ($Force){
                $restoreArgs += '-force'
            }

            if ($RestoreConfig){
                $restoreArgs += '-with-config'
            }

            $r = Invoke-IDLDaemon -Exe $installedExe -Arguments $restoreArgs -Environment @{ IDL_BACKUP_PASSPHRASE = $restorePlain }
            Write-IDLDaemonOutput -Result $r
            if ($r.ExitCode -ne 0){
                $hint = ((($r.StdErr -match 'already has data')) ? (' Re-run with -Force to replace the existing data.') : (''))
                throw "idlifecycled restore failed with exit code $($r.ExitCode): $($r.StdErr.Trim())$hint"
            }

            $script:ConfigChanged = $true
        }

        $restorePlain = $null
        Assert-NoFailedSteps -Phase 'Database and administrator'
    }

    if (-not $DryRun){
        # check connects to the DB and creates/migrates the schema - this is the database setup.
        $pre = Invoke-IDLCheck -Exe $installedExe -ConfigPath $configPath
        $acl = Get-IDLCheckEntry -Doc $pre -Name 'data_dir_acl'
        if (-not (Test-IDLCheckPassed -Entry $acl)){
            Add-SetupFailure -Action 'VerifyDataDirAcl' -Identity $DataDir -Message (Get-IDLCheckFailureText -Entry $acl -Name 'data_dir_acl')
        }
        else{
            Write-IDLLog "DONE: VerifyDataDirAcl $DataDir ($([string]$acl['detail']))"
            Add-IDLReportRow -Action 'VerifyDataDirAcl' -Result 'Verified' -Identity $DataDir -Details ([string]$acl['detail'])
        }

        $db = Get-IDLCheckEntry -Doc $pre -Name 'database'
        if (-not (Test-IDLCheckPassed -Entry $db)){
            Add-SetupFailure -Action 'PrepareDatabase' -Identity $effectiveDriver -Message (Get-IDLCheckFailureText -Entry $db -Name 'database')
        }
        else{
            Write-IDLLog "DONE: PrepareDatabase $effectiveDriver ($([string]$db['detail']))"
            Add-IDLReportRow -Action 'PrepareDatabase' -Result 'Verified' -Identity $effectiveDriver -Details ([string]$db['detail'])
        }

        Assert-NoFailedSteps -Phase 'Database and administrator'
        $adminEntry = Get-IDLCheckEntry -Doc $pre -Name 'admin_user'
        $adminExists = Test-IDLCheckPassed -Entry $adminEntry
    }

    Invoke-SetupStep -Action 'CreateAdminUser' -Identity $AdminUser -Details (($DryRun) ? ('role admin, unless check reports an enabled administrator (not checked in a dry run: check migrates the schema)') : ('role admin')) `
        -SatisfiedReason 'an enabled administrator already exists' `
        -IsSatisfied { $adminExists } `
        -Apply {
        if ($null -eq $adminPlain){
            if (-not $interactive){
                throw "No enabled administrator exists: pass -AdminPassword (SecureString) or set IDL_PASSWORD for '$AdminUser'."
            }

            $AdminPassword = Read-IDLSetupPassword -Prompt "Password for GUI administrator '$AdminUser' (min 12 characters)"
            $script:adminPlainPrompted = ConvertFrom-IDLSecure -Value $AdminPassword
            Register-IDLSecret -Secret $script:adminPlainPrompted
            $plain = $script:adminPlainPrompted
        }
        else{
            $plain = $adminPlain
        }

        try{
            $r = Invoke-IDLDaemon -Exe $installedExe -Arguments @('user', 'add', '-config', $configPath, '-username', $AdminUser, '-role', 'admin') -Environment @{ IDL_PASSWORD = $plain }
        }
        finally{
            $plain = $null
            $script:adminPlainPrompted = $null
        }

        Write-IDLDaemonOutput -Result $r
        if ($r.ExitCode -ne 0){
            throw "idlifecycled user add failed with exit code $($r.ExitCode): $($r.StdErr.Trim())"
        }
    }

    $adminPlain = $null
    Assert-NoFailedSteps -Phase 'Database and administrator'
    Stop-IDLPhase -Phase $phase

    # ---- Phase: Windows service ----
    $phase = Start-IDLPhase -Name 'Windows service'
    $serviceInstaller = Join-Path -Path $installedScripts -ChildPath 'Install-IDLifecycleService.ps1'
    $installerArgs = @('-BinaryPath', $installedExe, '-ConfigPath', $configPath, '-ServiceAccount', $ServiceAccount, '-ServiceName', $ServiceName)
    if ($DryRun){
        $installerArgs += '-DryRun'
    }

    Add-IDLCounter -Name 'Total'
    if (
        ($DryRun) -and
        ((-not (Test-Path -LiteralPath $serviceInstaller -PathType Leaf)) -or (-not (Test-Path -LiteralPath $configPath -PathType Leaf)))
    ){
        Write-IDLLog "WOULD: RegisterService $ServiceName (Install-IDLifecycleService.ps1 $($installerArgs -join ' '))"
        Add-IDLCounter -Name 'WouldChange'
        Add-IDLCounter -Name 'Succeeded'
        Add-IDLReportRow -Action 'RegisterService' -Result 'Would' -Identity $ServiceName -Details 'Install-IDLifecycleService.ps1 (program files / config not in place yet)'
    }
    else{
        $installerEnv = @{}
        if ($null -ne $servicePlain){
            $installerEnv['IDL_SERVICE_PASSWORD'] = $servicePlain
        }

        $child = Invoke-IDLServiceInstaller -ScriptPath $serviceInstaller -Arguments $installerArgs -Environment $installerEnv
        $installerEnv = $null
        $done = 0
        $would = 0
        foreach ($line in $child.Lines){
            if ($line -match '\] (DONE|WOULD|FAILED|SKIP): '){
                Write-IDLLog "  [service] $(($line -replace '^\S+ \S+ ', ''))"
                switch ($Matches[1]){
                    'DONE' { $done++ }
                    'WOULD' { $would++ }
                }
            }
        }

        if ($child.ExitCode -ne 0){
            $script:PropagatedExitCode = [int]$child.ExitCode
            Add-IDLCounter -Name 'Failed'
            $script:FailedSteps++
            $tail = (@($child.Lines | Where-Object { $_ -match 'FATAL|FAILED|ERROR' }) | Select-Object -Last 3) -join ' | '
            Write-IDLLog "FAILED: RegisterService $ServiceName - Install-IDLifecycleService.ps1 exit code $($child.ExitCode): $tail" 'ERROR'
            Add-IDLReportRow -Action 'RegisterService' -Result 'Failed' -Identity $ServiceName -Details "exit code $($child.ExitCode)" -ErrorText $tail
        }
        elseif (($done + $would) -eq 0){
            Write-IDLLog "SKIP: RegisterService $ServiceName (already configured)"
            Add-IDLCounter -Name 'Skipped'
            Add-IDLSkipReason -Reason 'already configured'
            Add-IDLReportRow -Action 'RegisterService' -Result 'Skipped' -Identity $ServiceName -Details 'already configured'
        }
        else{
            $result = (($DryRun) ? ('Would') : ('Applied'))
            Write-IDLLog "$((($DryRun) ? ('WOULD') : ('DONE'))): RegisterService $ServiceName ($($done + $would) change(s) by Install-IDLifecycleService.ps1)"
            Add-IDLCounter -Name (($DryRun) ? ('WouldChange') : ('Changed'))
            Add-IDLCounter -Name 'Succeeded'
            Add-IDLReportRow -Action 'RegisterService' -Result $result -Identity $ServiceName -Details "$($done + $would) change(s) by Install-IDLifecycleService.ps1"
        }
    }

    Assert-NoFailedSteps -Phase 'Windows service'
    Stop-IDLPhase -Phase $phase

    # ---- Phase: readiness check (all required checks must have run and passed, else nothing is started) ----
    $phase = Start-IDLPhase -Name 'Readiness check'
    if ($DryRun){
        Add-IDLCounter -Name 'Total'
        Add-IDLCounter -Name 'WouldChange'
        Add-IDLCounter -Name 'Succeeded'
        Write-IDLLog "WOULD: Check idlifecycled check -config $configPath -json -skip-ad-module (not run in a dry run: it creates/migrates the database schema)"
        Add-IDLReportRow -Action 'Check' -Result 'Would' -Identity $configPath -Details 'not run in a dry run (creates/migrates the schema)'
    }
    else{
        Add-IDLCounter -Name 'Total'
        $doc = Invoke-IDLCheck -Exe $installedExe -ConfigPath $configPath
        Write-IDLLog ('  {0,-18} {1,-5} {2}' -f 'CHECK', 'OK', 'DETAIL')
        $failing = [System.Collections.Generic.List[string]]::new()
        foreach ($c in @($doc['checks'])){
            $mark = (((Test-IDLCheckSkipped -Entry $c)) ? ('skip') : (((Test-IDLCheckPassed -Entry $c)) ? ('ok') : ('FAIL')))
            Write-IDLLog ('  {0,-18} {1,-5} {2}' -f [string]$c['name'], $mark, [string]$c['detail'])
            if (
                ($mark -eq 'FAIL') -or
                (($mark -eq 'skip') -and ([string]$c['name'] -in $script:RequiredChecks))
            ){
                $failing.Add("$([string]$c['name']): $((($mark -eq 'skip') ? ('not run: ') : ('')))$([string]$c['detail'])")
            }
        }

        # A required check that is missing from the output is a failure, never a pass.
        foreach ($required in $script:RequiredChecks){
            if ($null -eq (Get-IDLCheckEntry -Doc $doc -Name $required)){
                $failing.Add("${required}: missing from the check output")
            }
        }

        if (
            ($failing.Count -gt 0) -or
            (-not [bool]$doc['ok'])
        ){
            $script:FailedSteps++
            Add-IDLCounter -Name 'Failed'
            Write-IDLLog "FAILED: Check $configPath - $($failing -join '; ')" 'ERROR'
            Add-IDLReportRow -Action 'Check' -Result 'Failed' -Identity $configPath -ErrorText ($failing -join '; ')
            Write-IDLLog 'The service was NOT started. Fix the failing check(s) above and re-run this script.' 'ERROR'
        }
        else{
            Add-IDLCounter -Name 'Succeeded'
            $ran = @(@($doc['checks']) | Where-Object { -not (Test-IDLCheckSkipped -Entry $_) }).Count
            Write-IDLLog "DONE: Check $configPath (all $ran checks that ran passed)"
            Add-IDLReportRow -Action 'Check' -Result 'Verified' -Identity $configPath -Details "all $ran checks that ran passed"
        }
    }

    Assert-NoFailedSteps -Phase 'Readiness check'
    Stop-IDLPhase -Phase $phase

    # ---- Phase: start and probe ----
    $phase = Start-IDLPhase -Name 'Start'
    $listenParts = Split-IDLListen -Value $cfgListen
    $scheme = (($cfgTls) ? ('https') : ('http'))
    $probeHost = '127.0.0.1'
    $guiHost = '127.0.0.1'
    if ($null -ne $listenParts){
        if (Test-IDLWildcardHost -HostName $listenParts.Host){
            try{
                $guiHost = [System.Net.Dns]::GetHostEntry('').HostName
            }
            catch{
                $guiHost = $env:COMPUTERNAME
            }
        }
        else{
            $probeHost = $listenParts.Host
            $guiHost = $listenParts.Host
        }
    }

    $port = (($null -ne $listenParts) ? ($listenParts.Port) : (8443))
    $fmtHost = { param($h) (($h -match ':') ? ("[$h]") : ($h)) }
    $probeUrl = '{0}://{1}:{2}/' -f $scheme, (& $fmtHost $probeHost), $port
    $guiUrl = '{0}://{1}:{2}/' -f $scheme, (& $fmtHost $guiHost), $port
    $probeLoopback = Test-IDLLoopbackHost -HostName $probeHost
    $started = $false
    $current = Get-IDLSetupService -Name $ServiceName
    $runningNow = (($null -ne $current) -and ($current.State -eq 'Running'))

    if (
        (-not $Start) -and
        (-not $wasRunning) -and
        (-not ($runningNow -and $script:ConfigChanged))
    ){
        Add-SetupSkip -Action 'StartService' -Identity $ServiceName -Reason 'not requested (-Start:$false)'
    }
    elseif ($dsnEnvChanged){
        Add-SetupSkip -Action 'StartService' -Identity $ServiceName -Level 'WARN' `
            -Reason "reboot required: '$DbDsnEnvVar' was just set at machine scope and the Service Control Manager only passes machine environment variables to services after a restart; reboot, then Start-Service $ServiceName"
    }
    else{
        if (
            ($runningNow) -and
            ($script:ConfigChanged) -and
            (-not $wasRunning)
        ){
            # config.json changed under a running service: restart it so it reads the new settings.
            Invoke-SetupStep -Action 'RestartService' -Identity $ServiceName -Details 'config.json changed' `
                -IsSatisfied { $false } `
                -Apply {
                Stop-IDLSetupService -Name $ServiceName
                Wait-IDLSetupServiceState -Name $ServiceName -State 'Stopped' -TimeoutSec 60
                Start-IDLSetupService -Name $ServiceName
                Wait-IDLSetupServiceState -Name $ServiceName -State 'Running' -TimeoutSec 60
            }
        }
        else{
            Invoke-SetupStep -Action 'StartService' -Identity $ServiceName -Details (($wasRunning) ? ('restart after the program file update') : ('start')) `
                -SatisfiedReason 'already running' `
                -IsSatisfied { (-not $wasRunning) -and $runningNow } `
                -Apply {
                Start-IDLSetupService -Name $ServiceName
                Wait-IDLSetupServiceState -Name $ServiceName -State 'Running' -TimeoutSec 60
            }
        }

        Assert-NoFailedSteps -Phase 'Start'
        $script:StoppedForUpdate = $false
        $started = $true

        $skipCert = ($cfgTls -and $probeLoopback)
        Add-IDLCounter -Name 'Total'
        if ($DryRun){
            Add-IDLCounter -Name 'WouldChange'
            Add-IDLCounter -Name 'Succeeded'
            Write-IDLLog "WOULD: ProbeHttp $probeUrl (expect HTTP 200)"
            Add-IDLReportRow -Action 'ProbeHttp' -Result 'Would' -Identity $probeUrl -Details 'expect HTTP 200'
        }
        else{
            if ($skipCert){
                Write-IDLLog "Probing the loopback address $probeUrl without certificate validation (the certificate is issued for the real host name)."
            }

            $deadline = [DateTime]::UtcNow.AddSeconds(30)
            $lastError = ''
            $status = 0
            while ($true){
                try{
                    $status = Invoke-IDLHttpProbe -Url $probeUrl -SkipCertificateCheck:$skipCert
                    $lastError = "HTTP $status"
                }
                catch{
                    $status = 0
                    $lastError = $_.Exception.Message
                }

                if (
                    ($status -eq 200) -or
                    ([DateTime]::UtcNow -gt $deadline)
                ){
                    break
                }

                Start-Sleep -Seconds 2
            }

            if ($status -eq 200){
                Add-IDLCounter -Name 'Succeeded'
                Write-IDLLog "DONE: ProbeHttp $probeUrl (HTTP 200)"
                Add-IDLReportRow -Action 'ProbeHttp' -Result 'Verified' -Identity $probeUrl -Details 'HTTP 200'
            }
            else{
                $script:FailedSteps++
                Add-IDLCounter -Name 'Failed'
                Write-IDLLog "FAILED: ProbeHttp $probeUrl - $lastError (see $DataDir\logs\idlifecycle.log and the Application event log)" 'ERROR'
                Add-IDLReportRow -Action 'ProbeHttp' -Result 'Failed' -Identity $probeUrl -ErrorText $lastError
            }
        }
    }

    Stop-IDLPhase -Phase $phase

    # ---- Phase: tray icon, Start menu, branding folder (none of it is needed for the service to work) ----
    $phase = Start-IDLPhase -Name 'Tray and shortcuts'
    $trayInfoPath = Join-Path -Path $InstallDir -ChildPath 'tray.json'
    $trayInfo = [ordered]@{
        service_name     = $ServiceName
        console_url      = $guiUrl
        listen           = $cfgListen
        tls              = [bool]$cfgTls
        log_level        = $cfgLogLevel
        data_dir         = $DataDir
        install_dir      = $InstallDir
        config_path      = $configPath
        version          = [string]$sourceVersion
        execution_policy = $cfgPolicy
    }
    Invoke-SetupStep -Action 'WriteTrayInfo' -Identity $trayInfoPath -Details "$guiUrl (what the tray shows; no secrets)" `
        -SatisfiedReason 'up to date' `
        -IsSatisfied {
        try{
            $old = Read-IDLTrayInfo -Path $trayInfoPath
            $same = $true
            foreach ($k in $trayInfo.Keys){
                if ([string]$old[$k] -cne [string]$trayInfo[$k]){
                    $same = $false
                }
            }

            $same
        }
        catch{
            $false
        }
    } `
        -Apply { Write-IDLTrayInfo -Path $trayInfoPath -Info $trayInfo }

    $brandingRoot = Get-IDLSetupBrandingRoot
    $why = ((Test-Path -LiteralPath $brandingRoot -PathType Container) ? (Get-IDLSetupBrandingProblem -Path $brandingRoot) : ($null))
    if ($why){
        Add-SetupSkip -Action 'BrandingFolder' -Identity $brandingRoot -Level 'WARN' `
            -Reason "it exists but is not protected ($why); its files are ignored. Delete it if you did not create it, then re-run, or restrict it to SYSTEM and Administrators (Users read)"
    }
    else{
        Invoke-SetupStep -Action 'BrandingFolder' -Identity $brandingRoot -Details '(SYSTEM/Administrators full, Users read; README.txt lists the files that replace the default branding)' `
            -SatisfiedReason 'exists and is protected' `
            -IsSatisfied { Test-Path -LiteralPath $brandingRoot -PathType Container } `
            -Apply { Initialize-IDLSetupBrandingDir -Path $brandingRoot }
    }

    $roots = Get-IDLSetupShortcutRoots
    # A shortcut for everyone needs a program every user can start. The machine-wide PowerShell 7 is started directly;
    # without it (e.g. PowerShell from the Microsoft Store, whose pwsh.exe sits in a version-numbered folder that changes
    # with every update and cannot be started by a shortcut) Windows PowerShell 5.1 runs a small launcher that finds the
    # user's pwsh.exe at sign-in.
    $trayScript = Join-Path -Path $installedScripts -ChildPath 'Start-IDLifecycleTray.ps1'
    $machinePwsh = Join-Path -Path $env:ProgramFiles -ChildPath 'PowerShell\7\pwsh.exe'
    if (Test-Path -LiteralPath $machinePwsh -PathType Leaf){
        $trayTarget = $machinePwsh
        $trayArgs = '-NoProfile -STA -WindowStyle Hidden -ExecutionPolicy {0} -File "{1}"' -f $cfgPolicy, $trayScript
    }
    else{
        $trayTarget = Join-Path -Path $env:SystemRoot -ChildPath 'System32\WindowsPowerShell\v1.0\powershell.exe'
        $launcher = Join-Path -Path $installedScripts -ChildPath 'Start-IDLifecycleTrayLauncher.ps1'
        $trayArgs = '-NoProfile -WindowStyle Hidden -ExecutionPolicy {0} -File "{1}" -ExecutionPolicy {0}' -f $cfgPolicy, $launcher
        Write-IDLLog "PowerShell 7 is not installed machine-wide ($machinePwsh); the tray starts through Start-IDLifecycleTrayLauncher.ps1, which finds each user's pwsh.exe at sign-in."
    }

    $icon = Join-Path -Path $installedScripts -ChildPath 'branding\idlifecycle.ico'
    $iconLocation = (((Test-Path -LiteralPath $icon -PathType Leaf) -or $DryRun) ? ("$icon,0") : ("$trayTarget,0"))
    $shortcuts = [System.Collections.Generic.List[object]]::new()
    $setupCmd = Join-Path -Path $InstallDir -ChildPath 'Setup.cmd'
    if (
        (Test-Path -LiteralPath $setupCmd -PathType Leaf) -or
        ($DryRun)
    ){
        $shortcuts.Add(@{ Action = 'SetupShortcut'; Path = (Join-Path -Path $roots.Programs -ChildPath 'IDLifecycle Setup.lnk'); Target = $setupCmd; Arguments = ''; Description = 'Install, upgrade or remove IDLifecycle'; Tray = $false })
    }

    $shortcuts.Add(@{ Action = 'TrayShortcut'; Path = (Join-Path -Path $roots.Programs -ChildPath 'IDLifecycle Tray.lnk'); Target = $trayTarget; Arguments = $trayArgs; Description = 'IDLifecycle tray icon'; Tray = $true })
    $shortcuts.Add(@{ Action = 'TrayAutostart'; Path = (Join-Path -Path $roots.Startup -ChildPath 'IDLifecycle Tray.lnk'); Target = $trayTarget; Arguments = $trayArgs; Description = 'IDLifecycle tray icon (starts at sign-in)'; Tray = $true })
    foreach ($s in $shortcuts){
        if (
            ($s.Tray) -and
            ($NoTray)
        ){
            $existing = Get-IDLSetupShortcut -Path $s.Path
            if ($null -eq $existing){
                Add-SetupSkip -Action $s.Action -Identity $s.Path -Reason 'not wanted (-NoTray)'
            }
            else{
                Invoke-SetupStep -Action ('Remove' + $s.Action) -Identity $s.Path -Details '(-NoTray)' -IsSatisfied { $false } -Apply { Remove-IDLSetupShortcut -Path $s.Path }
            }

            continue
        }

        Invoke-SetupStep -Action $s.Action -Identity $s.Path -Details "-> $($s.Target) $($s.Arguments)".TrimEnd() `
            -SatisfiedReason 'up to date' `
            -IsSatisfied {
            $existing = Get-IDLSetupShortcut -Path $s.Path
            ($null -ne $existing) -and
            ($existing.Target -ieq $s.Target) -and
            ($existing.Arguments -ceq $s.Arguments) -and
            ($existing.Icon -ieq $iconLocation)
        } `
            -Apply { New-IDLSetupShortcut -Path $s.Path -Target $s.Target -Arguments $s.Arguments -WorkingDirectory $installedScripts -Icon $iconLocation -Description $s.Description }
    }

    # The Startup shortcut only runs at the next sign-in: start the tray now for whoever is signed in here.
    $trayLink = Join-Path -Path $roots.Programs -ChildPath 'IDLifecycle Tray.lnk'
    if ($NoTray){
        Add-SetupSkip -Action 'StartTray' -Identity $trayLink -Reason 'not wanted (-NoTray)'
    }
    elseif (-not (Test-IDLSetupDesktopSession)){
        Add-SetupSkip -Action 'StartTray' -Identity $trayLink -Reason 'no signed-in desktop here; it starts at the next sign-in'
    }
    else{
        Invoke-SetupStep -Action 'StartTray' -Identity $trayLink -Details '(as the signed-in user, without administrator rights; a tray that already runs stays as it is)' `
            -SatisfiedReason 'already running in this session' `
            -IsSatisfied { Test-IDLSetupTrayRunning } `
            -Apply { Start-IDLSetupTrayNow -Shortcut $trayLink }
    }

    Assert-NoFailedSteps -Phase 'Tray and shortcuts'
    Stop-IDLPhase -Phase $phase

    Write-IDLLog '---- Next steps ----'
    Write-IDLLog "GUI: $guiUrl $((($started -or $DryRun) ? ('') : ('(after the service is started)')))"
    Write-IDLLog "Sign in as '$AdminUser', then: Settings -> AD (search bases, default OU, UPN suffix) -> Sources (add + Test) -> Mappings -> Roles."
    Write-IDLLog 'Run a sync in DRY-RUN (the default), review the simulated changes, and only then turn dry-run off.'
    if (-not $NoTray){
        Write-IDLLog "Tray icon: starts for everyone at their next sign-in; start it now from Start menu -> IDLifecycle -> IDLifecycle Tray."
    }
    Write-IDLLog "Service account '$ServiceAccount' needs AD delegation on the target OUs and 'Log on as a service' (granted locally; add it to the GPO too if a GPO defines that right)."
    if ($isPasswordAccount){
        Write-IDLLog "'$ServiceAccount' uses a password: when it changes, re-run this installer with -ServiceAccountPassword. Exclude it from password expiry or move to a gMSA."
    }

    $exitCode = (($script:FailedSteps -gt 0) ? (1) : (0))
}
catch{
    if ($_.Exception.Data.Contains('IDLStepFailure')){
        $exitCode = (($script:PropagatedExitCode -ne 0) ? ($script:PropagatedExitCode) : (1))
        Write-IDLLog $_.Exception.Message 'ERROR'
    }
    else{
        $exitCode = Get-IDLExitCode -Exception $_.Exception
        Write-IDLLog "FATAL: $($_.Exception.Message)" 'ERROR'
        Write-IDLLog "Stack: $($_.ScriptStackTrace)" 'DEBUG'
    }
}
finally{
    # A service this run stopped for an update is never left stopped because a later step failed.
    if ($script:StoppedForUpdate){
        try{
            $svc = Get-IDLSetupService -Name $ServiceName
            if (
                ($null -ne $svc) -and
                ($svc.State -ne 'Running')
            ){
                Start-IDLSetupService -Name $ServiceName
                Write-IDLLog "Restarted service '$ServiceName' that this run had stopped (the run did not complete; check its state and the log above)." 'WARN'
            }
        }
        catch{
            Write-IDLLog "Could not restart service '$ServiceName' that this run had stopped: $($_.Exception.Message)" 'ERROR'
        }
    }

    if ($script:UnverifiedTempDir){
        if ($exitCode -eq 0){
            Write-IDLLog 'Reminder: this was an UNVERIFIED install (-SkipIntegrityCheck). Reinstall from a release built by New-IDLifecycleRelease.ps1 before production use.' 'WARN'
        }

        Remove-Item -LiteralPath $script:UnverifiedTempDir -Recurse -Force -ErrorAction SilentlyContinue
    }

    $adminPlain = $null
    $dsnPlain = $null
    $servicePlain = $null
    $tokenPlain = $null
    $restorePlain = $null
    $script:DaemonEnv = @{}
    Stop-IDLRun -ExitCode $exitCode
}

exit $exitCode
