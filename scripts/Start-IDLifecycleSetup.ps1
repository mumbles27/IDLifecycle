<#
.SYNOPSIS
    Graphical installer / uninstaller for IDLifecycle: a step-by-step window in front of Install-IDLifecycle.ps1 and
    Uninstall-IDLifecycle.ps1.

.DESCRIPTION
    Started by double-clicking Setup.cmd in a release folder (or the installed program folder). It
      1. asks for administrator rights (UAC). Declined: it keeps running, but only a test run (-DryRun) is possible;
      2. shows what is already installed (service, its account, program and data folder, database driver);
      3. Install or upgrade: release, program and data folder -> service account (gMSA/MSA, user account with
         password, or built-in) -> database (SQLite, SQL Server, PostgreSQL, MySQL, Db2 + connection string) -> web
         console (address, TLS certificate, first administrator) -> options -> review;
         Uninstall: what to remove (program files, data folder with backup and typed confirmation, DSN variable) ->
         review;
      4. runs the real script in a child pwsh (-NonInteractive) and shows its log live, coloured, with the result,
         the web console link and the log folder.
    Every check and every change is made by the two scripts; this window only collects the input, checks it in
    advance so mistakes show up on the page where they were made, and builds the command line. "Test run" passes
    -DryRun. The equivalent PowerShell command (secrets replaced by Read-Host) is shown on the review page.

    Secrets (GUI admin password, service account password, connection string) are read from password boxes as
    SecureStrings and reach the child only through its environment (IDL_PASSWORD, IDL_SERVICE_PASSWORD,
    IDL_SETUP_DB_DSN), which the scripts remove at once - never a command line, log or report.

    Needs a Windows edition with the desktop experience (WPF); on Server Core use the scripts directly.

.PARAMETER Mode
    Start on a page: Choose (default), Install or Uninstall.

.PARAMETER SourceDir
    Release folder (default: the parent of this script's folder).

.PARAMETER NoElevate
    Do not ask for administrator rights at start.

.PARAMETER ShowConsole
    Keep the console window visible (for troubleshooting the window itself).

.EXAMPLE
    C:\Install\IDLifecycle-1.0.0\Setup.cmd

.EXAMPLE
    pwsh -NoProfile -STA -File C:\Install\IDLifecycle-1.0.0\scripts\Start-IDLifecycleSetup.ps1 -Mode Uninstall

.NOTES
    Requires PowerShell 7+ on Windows with WPF. Exit codes: 0 closed normally, 3 WPF is not available, 5 unexpected
    error. The result of an install/uninstall run is shown in the window; the scripts' own logs and CSV reports are
    in %ProgramData%\IDLifecycle-Setup\Logs (a non-elevated test run: %LOCALAPPDATA%\IDLifecycle-Setup\Logs).
#>

#Requires -Version 7.0

[CmdletBinding()]
param(
    [ValidateSet('Choose', 'Install', 'Uninstall')]
    [string]$Mode = 'Choose',

    [string]$SourceDir = '',

    [switch]$NoElevate,

    [switch]$ShowConsole,

    # Test seam: define the functions and return without a window (dot-sourced by Start-IDLifecycleSetup.Tests.ps1).
    [Parameter(DontShow)]
    [switch]$DefineOnly
)

Import-Module (Join-Path $PSScriptRoot 'IDLifecycle.Common.psm1') -Force

$script:SecretEnvNames = @('IDL_PASSWORD', 'IDL_SERVICE_PASSWORD', 'IDL_SETUP_DB_DSN', 'IDL_GITHUB_TOKEN', 'IDL_BACKUP_PASSPHRASE')
$script:DefaultGitHubRepo = 'mumbles27/IDLifecycle'
$script:ServiceNamePattern = '^[A-Za-z0-9_.\-]{1,80}$'
$script:DbDrivers = [ordered]@{
    sqlite   = 'SQLite (built in, nothing to set up)'
    mssql    = 'Microsoft SQL Server'
    postgres = 'PostgreSQL'
    mysql    = 'MySQL / MariaDB'
    db2      = 'IBM Db2 (via ODBC)'
}
$script:DsnExamples = @{
    mssql    = "sqlserver://sql01.corp.local?database=IDLifecycle   (Windows sign-in as the service account)`nsqlserver://idl_app:PASSWORD@sql01.corp.local:1433?database=IDLifecycle"
    postgres = 'postgres://idl_app:PASSWORD@pg01.corp.local:5432/idlifecycle?sslmode=require'
    mysql    = 'idl_app:PASSWORD@tcp(mysql01.corp.local:3306)/idlifecycle'
    db2      = "Driver={IBM DB2 ODBC DRIVER};Hostname=db2.corp.local;Port=50000;Protocol=TCPIP;Database=IDLDB;Uid=idl;Pwd=PASSWORD;`nDSN=IDLDB;Uid=idl;Pwd=PASSWORD;   (a 64-bit system DSN)"
}

#region Pure helpers (no WPF, no system changes; covered by Start-IDLifecycleSetup.Tests.ps1)

function ConvertTo-IDLGuiQuoted {
    # A PowerShell single-quoted literal.
    [CmdletBinding()]
    param(
        [AllowEmptyString()][string]$Value
    )

    return ("'" + $Value.Replace("'", "''") + "'")
}

function Get-IDLGuiSecureLength {
    [CmdletBinding()]
    param(
        [AllowNull()][securestring]$Value
    )

    return (($null -eq $Value) ? (0) : ($Value.Length))
}

function Test-IDLGuiSecureEqual {
    # True when both SecureStrings hold the same text (the plain copies are dropped at once).
    [CmdletBinding()]
    param(
        [AllowNull()][securestring]$First,
        [AllowNull()][securestring]$Second
    )

    $a = (($null -eq $First) ? ('') : ([System.Net.NetworkCredential]::new('', $First).Password))
    $b = (($null -eq $Second) ? ('') : ([System.Net.NetworkCredential]::new('', $Second).Password))
    try{
        return ($a -ceq $b)
    }
    finally{
        $a = $null
        $b = $null
    }
}

function Get-IDLGuiServiceAccount {
    # The account name the install will use, from the account page's choice.
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][hashtable]$Settings
    )

    if ($Settings.AccountType -eq 'BuiltIn'){
        if ($Settings.BuiltInAccount -eq 'Virtual'){
            return ('NT SERVICE\{0}' -f $Settings.ServiceName)
        }

        return [string]$Settings.BuiltInAccount
    }

    return (ConvertTo-IDLServiceAccountName -Account ([string]$Settings.ServiceAccount))
}

function Test-IDLGuiServicePasswordNeeded {
    # A user account needs its password when the service is new or moves to that account (else the stored one is kept).
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][hashtable]$Settings
    )

    if ($Settings.AccountType -ne 'Password'){
        return $false
    }

    if (-not $Settings.ServiceExists){
        return $true
    }

    $existing = ConvertTo-IDLServiceAccountName -Account ([string]$Settings.ExistingAccount)
    return (-not ($existing -ieq (Get-IDLGuiServiceAccount -Settings $Settings)))
}

function New-IDLGuiProblem {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Page,
        [Parameter(Mandatory)][string]$Message
    )

    return [pscustomobject]@{ Page = $Page; Message = $Message }
}

function Get-IDLGuiInstallProblems {
    <#
      Checks the install settings the way Install-IDLifecycle.ps1 will, so a mistake is reported on the page where it
      was made. Returns @{ Page; Message }[] (Page: Locations, Account, Database, Web). -DryRun relaxes what a test
      run does not need (passwords).
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][hashtable]$Settings,
        [switch]$DryRun
    )

    $problems = [System.Collections.Generic.List[object]]::new()
    $s = $Settings

    # Source: this release folder, GitHub (built here with Go) or a release zip/folder
    # Without the SHA-256 check (least safe) a folder only needs idlifecycled.exe and scripts\.
    $isReleaseFolder = {
        param([string]$Dir)
        (Test-Path -LiteralPath (Join-Path -Path $Dir -ChildPath 'idlifecycled.exe') -PathType Leaf) -and
        (
            ([bool]$s['SkipIntegrityCheck'] -and (Test-Path -LiteralPath (Join-Path -Path $Dir -ChildPath 'scripts') -PathType Container)) -or
            (Test-Path -LiteralPath (Join-Path -Path $Dir -ChildPath 'SHA256SUMS') -PathType Leaf)
        )
    }
    $notRelease = {
        param([string]$Dir)
        if (-not (Test-Path -LiteralPath (Join-Path -Path $Dir -ChildPath 'idlifecycled.exe') -PathType Leaf)){
            return "'$Dir' has no idlifecycled.exe. Choose a release folder or zip, or GitHub (which builds it)."
        }

        return "'$Dir' has no SHA256SUMS, so it is not a release folder. Build one with New-IDLifecycleRelease.ps1, or - least safe, for testing - tick 'Skip the SHA-256 file check' under Options."
    }
    switch ([string]$s.SourceKind){
        'Release'{
            if (
                ([string]::IsNullOrWhiteSpace($s.SourceDir)) -or
                (-not (& $isReleaseFolder $s.SourceDir))
            ){
                $problems.Add((New-IDLGuiProblem -Page 'Source' -Message (& $notRelease ([string]$s.SourceDir))))
            }
        }
        'GitHub'{
            $repo = (([string]$s.GitHubRepo).Trim() -replace '^https?://(www\.)?github\.com/', '' -replace '\.git$', '').TrimEnd('/')
            if ($repo -notmatch '^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$'){
                $problems.Add((New-IDLGuiProblem -Page 'Source' -Message "Repository '$($s.GitHubRepo)' must be owner/name or https://github.com/owner/name."))
            }

            if (
                ([string]$s.GitHubRef -notmatch '^[A-Za-z0-9._/-]{1,200}$') -or
                ([string]$s.GitHubRef -match '\.\.')
            ){
                $problems.Add((New-IDLGuiProblem -Page 'Source' -Message 'Enter the branch, tag or commit to build, e.g. main or v1.2.0.'))
            }

            if (-not $s.GoVersion){
                $problems.Add((New-IDLGuiProblem -Page 'Source' -Message 'Go is not installed on this server, and building from GitHub needs it (https://go.dev/dl/, version 1.22 or newer; then restart this window). Or install from a release zip.'))
            }
        }
        'Local'{
            $path = ([string]$s.SourceDir).Trim()
            if (-not $path){
                $problems.Add((New-IDLGuiProblem -Page 'Source' -Message 'Choose the release zip or release folder.'))
            }
            elseif ($path -match '\.zip$'){
                if (-not (Test-Path -LiteralPath $path -PathType Leaf)){
                    $problems.Add((New-IDLGuiProblem -Page 'Source' -Message "File not found: $path"))
                }
            }
            elseif (
                (-not (Test-Path -LiteralPath $path -PathType Container)) -or
                (-not (& $isReleaseFolder $path))
            ){
                $problems.Add((New-IDLGuiProblem -Page 'Source' -Message ((Test-Path -LiteralPath $path -PathType Container) ? (& $notRelease $path) : ("Folder not found: $path"))))
            }
        }
        default{
            $problems.Add((New-IDLGuiProblem -Page 'Source' -Message 'Choose where to install IDLifecycle from.'))
        }
    }

    # Locations

    $paths = [ordered]@{ InstallDir = 'program folder'; DataDir = 'data folder' }
    foreach ($key in $paths.Keys){
        $value = [string]$s[$key]
        if ([string]::IsNullOrWhiteSpace($value)){
            $problems.Add((New-IDLGuiProblem -Page 'Locations' -Message "Choose the $($paths[$key])."))
            continue
        }

        $reason = Get-IDLUnsafePathReason -Path $value
        if ($reason){
            $problems.Add((New-IDLGuiProblem -Page 'Locations' -Message "The $($paths[$key]) '$value' cannot be used: $reason."))
        }
    }

    if (
        (-not [string]::IsNullOrWhiteSpace($s.InstallDir)) -and
        (-not [string]::IsNullOrWhiteSpace($s.DataDir)) -and
        (
            (Test-IDLPathWithin -Child $s.DataDir -Parent $s.InstallDir) -or
            (Test-IDLPathWithin -Child $s.InstallDir -Parent $s.DataDir)
        )
    ){
        $problems.Add((New-IDLGuiProblem -Page 'Locations' -Message 'The program folder and the data folder must be separate (neither inside the other).'))
    }

    if ([string]$s.ServiceName -notmatch $script:ServiceNamePattern){
        $problems.Add((New-IDLGuiProblem -Page 'Locations' -Message "Service name '$($s.ServiceName)' is not valid (letters, digits, '_', '.', '-' only)."))
    }

    # Service account
    $account = Get-IDLGuiServiceAccount -Settings $s
    $kind = Get-IDLServiceAccountKind -Account $account
    switch ([string]$s.AccountType){
        'Managed'{
            if ([string]::IsNullOrWhiteSpace($s.ServiceAccount)){
                $problems.Add((New-IDLGuiProblem -Page 'Account' -Message 'Enter the gMSA or MSA name, e.g. CORP\svc-idl$.'))
            }
            elseif ($kind -ne 'Managed'){
                $problems.Add((New-IDLGuiProblem -Page 'Account' -Message "'$($s.ServiceAccount)' is not a managed service account name: it is DOMAIN\name`$ (with the `$ at the end), e.g. CORP\svc-idl`$."))
            }
        }
        'Password'{
            if ([string]::IsNullOrWhiteSpace($s.ServiceAccount)){
                $problems.Add((New-IDLGuiProblem -Page 'Account' -Message 'Enter the user account, e.g. CORP\svc-idl.'))
            }
            elseif ($kind -ne 'Password'){
                $problems.Add((New-IDLGuiProblem -Page 'Account' -Message "'$($s.ServiceAccount)' is not a user account name: use DOMAIN\name, .\name (a local account) or name@domain. A name ending in `$ is a gMSA/MSA - choose that option instead."))
            }
            elseif (
                (-not $DryRun) -and
                (Test-IDLGuiServicePasswordNeeded -Settings $s) -and
                ((Get-IDLGuiSecureLength -Value $s.ServicePassword) -eq 0)
            ){
                $problems.Add((New-IDLGuiProblem -Page 'Account' -Message "Enter the password of '$account' (Windows stores it for the service)."))
            }
        }
        'BuiltIn'{
            if ($kind -ne 'BuiltIn'){
                $problems.Add((New-IDLGuiProblem -Page 'Account' -Message 'Choose a built-in account.'))
            }
        }
        default{
            $problems.Add((New-IDLGuiProblem -Page 'Account' -Message 'Choose the kind of service account.'))
        }
    }

    # Database (an existing config.json keeps its database; nothing on this page is used then)
    if (-not $s.ConfigExists){
        $driver = [string]$s.DbDriver
        if (-not $script:DbDrivers.Contains($driver)){
            $problems.Add((New-IDLGuiProblem -Page 'Database' -Message 'Choose a database.'))
        }
        elseif ($driver -ne 'sqlite'){
            if ($s.DsnInEnvVar){
                $name = [string]$s.DbDsnEnvVar
                if ($name -cnotmatch '^IDL_[A-Za-z0-9_]{1,120}$'){
                    $problems.Add((New-IDLGuiProblem -Page 'Database' -Message "Environment variable name '$name' is not allowed: it must start with IDL_ and use only letters, digits and '_' (e.g. IDL_DB_DSN)."))
                }
                elseif ($name -in $script:SecretEnvNames){
                    $problems.Add((New-IDLGuiProblem -Page 'Database' -Message "'$name' is reserved by the installer; use another name, e.g. IDL_DB_DSN."))
                }
            }
            elseif ((Get-IDLGuiSecureLength -Value $s.DbDsn) -eq 0){
                $problems.Add((New-IDLGuiProblem -Page 'Database' -Message "Enter the connection string for $($script:DbDrivers[$driver])."))
            }
        }
    }

    # Web console
    $listen = ([string]$s.Listen).Trim()
    $hasCert = -not [string]::IsNullOrWhiteSpace($s.TlsCertFile)
    $hasKey = -not [string]::IsNullOrWhiteSpace($s.TlsKeyFile)
    if ($listen){
        $parts = Split-IDLListen -Value $listen
        if ($null -eq $parts){
            $problems.Add((New-IDLGuiProblem -Page 'Web' -Message "Web console address '$listen' must be host:port, e.g. 127.0.0.1:8443 or 0.0.0.0:8443."))
        }
        elseif (
            (-not (Test-IDLLoopbackHost -HostName $parts.Host)) -and
            ((-not $hasCert) -or (-not $hasKey))
        ){
            $problems.Add((New-IDLGuiProblem -Page 'Web' -Message "'$listen' can be reached from the network, so the console needs HTTPS: choose a certificate file and its key file (PEM)."))
        }
    }

    if ($hasCert -ne $hasKey){
        $problems.Add((New-IDLGuiProblem -Page 'Web' -Message 'Choose both the certificate file and the key file (or neither).'))
    }

    foreach ($file in @($s.TlsCertFile, $s.TlsKeyFile)){
        if (
            (-not [string]::IsNullOrWhiteSpace($file)) -and
            (-not (Test-Path -LiteralPath $file -PathType Leaf))
        ){
            $problems.Add((New-IDLGuiProblem -Page 'Web' -Message "File not found: $file"))
        }
    }

    if ([string]$s.AdminUser -notmatch '^[A-Za-z0-9._@\-]{1,64}$'){
        $problems.Add((New-IDLGuiProblem -Page 'Web' -Message "Administrator name '$($s.AdminUser)' is not valid (letters, digits, '.', '_', '@', '-')."))
    }

    $adminLength = Get-IDLGuiSecureLength -Value $s.AdminPassword
    if ($adminLength -gt 0){
        if ($adminLength -lt 12){
            $problems.Add((New-IDLGuiProblem -Page 'Web' -Message 'The administrator password must be at least 12 characters.'))
        }
        elseif (-not (Test-IDLGuiSecureEqual -First $s.AdminPassword -Second $s.AdminPasswordConfirm)){
            $problems.Add((New-IDLGuiProblem -Page 'Web' -Message 'The two administrator passwords do not match.'))
        }
    }
    elseif (
        (-not $s.ConfigExists) -and
        (-not $DryRun) -and
        (-not $s.RestoreFrom)
    ){
        $problems.Add((New-IDLGuiProblem -Page 'Web' -Message "Enter a password for the first administrator '$($s.AdminUser)' (at least 12 characters)."))
    }

    # Restore from a backup
    if ($s.RestoreWanted){
        $file = ([string]$s.RestoreFrom).Trim()
        if (-not $file){
            $problems.Add((New-IDLGuiProblem -Page 'Options' -Message 'Choose the backup file (.idlbak) to restore.'))
        }
        elseif (-not (Test-Path -LiteralPath $file -PathType Leaf)){
            $problems.Add((New-IDLGuiProblem -Page 'Options' -Message "Backup file not found: $file"))
        }

        if (
            (-not $DryRun) -and
            ((Get-IDLGuiSecureLength -Value $s.RestorePassphrase) -eq 0)
        ){
            $problems.Add((New-IDLGuiProblem -Page 'Options' -Message 'Enter the backup passphrase.'))
        }
    }

    return $problems.ToArray()
}

function New-IDLGuiInstallCommand {
    <#
      The Install-IDLifecycle.ps1 run for these settings: @{ Script; Arguments (string[], for pwsh -File); Secrets
      (env var name -> SecureString); Display (the same as a PowerShell command, secrets replaced by Read-Host) }.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][hashtable]$Settings,
        [Parameter(Mandatory)][string]$ScriptsDir,
        [switch]$DryRun
    )

    $s = $Settings
    $arguments = [System.Collections.Generic.List[string]]::new()
    $display = [System.Collections.Generic.List[string]]::new()
    $secrets = [ordered]@{}
    $script = Join-Path -Path $ScriptsDir -ChildPath 'Install-IDLifecycle.ps1'

    $addValue = {
        param([string]$Name, [string]$Value)

        $arguments.Add("-$Name")
        $arguments.Add($Value)
        $display.Add(('-{0} {1}' -f $Name, (ConvertTo-IDLGuiQuoted -Value $Value)))
    }
    $addSwitch = {
        param([string]$Name)

        $arguments.Add("-$Name")
        $display.Add("-$Name")
    }

    if ($s.SourceKind -eq 'GitHub'){
        & $addSwitch 'FromGitHub'
        & $addValue 'GitHubRepo' ([string]$s.GitHubRepo).Trim()
        & $addValue 'GitHubRef' ([string]$s.GitHubRef).Trim()
        if ((Get-IDLGuiSecureLength -Value $s.GitHubToken) -gt 0){
            $secrets['IDL_GITHUB_TOKEN'] = $s.GitHubToken
            $display.Add("-GitHubToken (Read-Host -AsSecureString 'GitHub token')")
        }
    }
    else{
        & $addValue 'SourceDir' ([string]$s.SourceDir).Trim()
    }

    & $addValue 'InstallDir' ([string]$s.InstallDir)
    & $addValue 'DataDir' ([string]$s.DataDir)
    & $addValue 'ServiceName' ([string]$s.ServiceName)
    $account = Get-IDLGuiServiceAccount -Settings $s
    & $addValue 'ServiceAccount' $account

    if (
        ($s.AccountType -eq 'Password') -and
        ((Get-IDLGuiSecureLength -Value $s.ServicePassword) -gt 0)
    ){
        $secrets['IDL_SERVICE_PASSWORD'] = $s.ServicePassword
        $display.Add(('-ServiceAccountPassword (Read-Host -AsSecureString {0})' -f (ConvertTo-IDLGuiQuoted -Value "Password for $account")))
    }

    if (
        (-not $s.ConfigExists) -and
        ($s.DbDriver -ne 'sqlite')
    ){
        & $addValue 'DbDriver' ([string]$s.DbDriver)
        if ($s.DsnInEnvVar){
            & $addValue 'DbDsnEnvVar' ([string]$s.DbDsnEnvVar)
        }

        if ((Get-IDLGuiSecureLength -Value $s.DbDsn) -gt 0){
            $secrets['IDL_SETUP_DB_DSN'] = $s.DbDsn
            $display.Add("-DbDsn (Read-Host -AsSecureString 'Connection string')")
        }
    }

    $listen = ([string]$s.Listen).Trim()
    if ($listen){
        & $addValue 'Listen' $listen
    }

    if (-not [string]::IsNullOrWhiteSpace($s.TlsCertFile)){
        & $addValue 'TlsCertFile' ([string]$s.TlsCertFile)
        & $addValue 'TlsKeyFile' ([string]$s.TlsKeyFile)
    }

    & $addValue 'AdminUser' ([string]$s.AdminUser)
    if ((Get-IDLGuiSecureLength -Value $s.AdminPassword) -gt 0){
        $secrets['IDL_PASSWORD'] = $s.AdminPassword
        $display.Add(('-AdminPassword (Read-Host -AsSecureString {0})' -f (ConvertTo-IDLGuiQuoted -Value "Password for $($s.AdminUser)")))
    }

    if ($s.RestoreFrom){
        & $addValue 'RestoreFrom' ([string]$s.RestoreFrom).Trim()
        if ($s.RestoreConfig){
            & $addSwitch 'RestoreConfig'
        }

        if ((Get-IDLGuiSecureLength -Value $s.RestorePassphrase) -gt 0){
            $secrets['IDL_BACKUP_PASSPHRASE'] = $s.RestorePassphrase
            $display.Add("-RestorePassphrase (Read-Host -AsSecureString 'Backup passphrase')")
        }
    }

    if (-not $s.Start){
        $arguments.Add('-Start:$false')
        $display.Add('-Start:$false')
    }

    foreach ($pair in @(@('InstallPrerequisites', $s.InstallPrerequisites), @('RequireSignature', $s.RequireSignature), @('SkipIntegrityCheck', [bool]$s['SkipIntegrityCheck']), @('NoTray', [bool]$s['NoTray']), @('Force', $s.Force))){
        if ($pair[1]){
            & $addSwitch $pair[0]
        }
    }

    if ($DryRun){
        & $addSwitch 'DryRun'
    }

    $arguments.Add('-NonInteractive')

    return [pscustomobject]@{
        Script     = $script
        ScriptName = 'Install-IDLifecycle'
        Arguments  = [string[]]$arguments.ToArray()
        Secrets    = $secrets
        DryRun     = [bool]$DryRun
        Display    = ('& {0} {1}' -f (ConvertTo-IDLGuiQuoted -Value $script), ($display -join ' '))
    }
}

function Get-IDLGuiInstallSummary {
    # Plain-language lines for the review page (never a secret, only whether one was entered).
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][hashtable]$Settings
    )

    $s = $Settings
    $account = Get-IDLGuiServiceAccount -Settings $s
    $kindText = @{ Managed = 'managed service account (gMSA/MSA)'; Password = 'user account'; BuiltIn = 'built-in account' }[[string]$s.AccountType]
    $lines = [System.Collections.Generic.List[string]]::new()
    $lines.Add(('{0}  {1}' -f (($s.ServiceExists) ? ('Upgrade / repair of service') : ('New service')), $s.ServiceName))
    $sourceText = switch ([string]$s.SourceKind){
        'GitHub'{ "GitHub $(([string]$s.GitHubRepo).Trim()) @ $(([string]$s.GitHubRef).Trim()), built here with $($s.GoVersion)$((((Get-IDLGuiSecureLength -Value $s.GitHubToken) -gt 0)) ? (' (token entered)') : (''))" }
        'Local'{ ([string]$s.SourceDir).Trim() }
        default{ "$($s.SourceDir) (this release folder)" }
    }
    $lines.Add("Source:             $sourceText")
    $lines.Add("Program folder:     $($s.InstallDir)")
    $lines.Add("Data folder:        $($s.DataDir)")
    $lines.Add("Service account:    $account ($kindText)")
    if ($s.AccountType -eq 'Password'){
        $pwText = (((Get-IDLGuiSecureLength -Value $s.ServicePassword) -gt 0) ? ('entered (checked against AD before anything changes)') : ('not entered - the stored password is kept'))
        $lines.Add("  password:         $pwText")
    }

    if ($s.ConfigExists){
        $lines.Add('Database:           kept from the existing config.json')
    }
    else{
        $lines.Add("Database:           $($script:DbDrivers[[string]$s.DbDriver])")
        if ($s.DbDriver -ne 'sqlite'){
            $where = (($s.DsnInEnvVar) ? ("machine environment variable $($s.DbDsnEnvVar)") : ('config.json in the locked-down data folder'))
            $dsnText = (((Get-IDLGuiSecureLength -Value $s.DbDsn) -gt 0) ? ('entered') : ('not entered (the variable must already exist)'))
            $lines.Add("  connection:       $dsnText; stored in $where")
        }
    }

    $listen = ([string]$s.Listen).Trim()
    $lines.Add(('Web console:        {0}{1}' -f (($listen) ? ($listen) : ((($s.ConfigExists) ? ('kept from config.json') : ('127.0.0.1:8443 (this server only)')))), ((-not [string]::IsNullOrWhiteSpace($s.TlsCertFile)) ? (' with HTTPS') : (''))))
    $adminText = (((Get-IDLGuiSecureLength -Value $s.AdminPassword) -gt 0) ? ('password entered') : ('no password entered (only needed when no administrator exists yet)'))
    $lines.Add("Administrator:      $($s.AdminUser) - $adminText")
    $options = @()
    $options += (($s.Start) ? ('start the service at the end') : ('do not start the service'))
    if ($s.InstallPrerequisites){
        $options += 'install the AD PowerShell module if missing'
    }

    if ($s.RequireSignature){
        $options += 'require a signed idlifecycled.exe'
    }

    if ([bool]$s['SkipIntegrityCheck']){
        $options += 'SKIP the SHA-256 file check (least safe)'
    }

    $options += (([bool]$s['NoTray']) ? ('no tray icon') : ('tray icon for everyone who signs in'))

    if ($s.Force){
        $options += 'replace changed program files (upgrade)'
    }

    $lines.Add("Options:            $($options -join '; ')")
    if ($s.RestoreFrom){
        $lines.Add("Restore:            $(([string]$s.RestoreFrom).Trim()) replaces the database contents$((($s.RestoreConfig)) ? (' and the settings in config.json') : (''))")
    }
    return [string[]]$lines.ToArray()
}

function Get-IDLGuiUninstallProblems {
    # Returns @{ Page = 'Uninstall'; Message }[]. -DryRun does not need the typed confirmation.
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][hashtable]$Settings,
        [switch]$DryRun
    )

    $problems = [System.Collections.Generic.List[object]]::new()
    $s = $Settings
    if ([string]$s.ServiceName -notmatch $script:ServiceNamePattern){
        $problems.Add((New-IDLGuiProblem -Page 'Uninstall' -Message "Service name '$($s.ServiceName)' is not valid (letters, digits, '_', '.', '-' only)."))
    }

    foreach ($pair in @(@('InstallDir', 'program folder'), @('DataDir', 'data folder'))){
        $value = [string]$s[$pair[0]]
        if (-not [string]::IsNullOrWhiteSpace($value)){
            $reason = Get-IDLUnsafePathReason -Path $value
            if ($reason){
                $problems.Add((New-IDLGuiProblem -Page 'Uninstall' -Message "The $($pair[1]) '$value' cannot be used: $reason."))
            }
        }
    }

    if ($s.RemoveData){
        if ($s.BackupMode -eq 'Backup'){
            if ([string]::IsNullOrWhiteSpace($s.BackupTo)){
                $problems.Add((New-IDLGuiProblem -Page 'Uninstall' -Message 'Choose a folder for the backup of the data folder (or choose "No backup").'))
            }
            else{
                $reason = Get-IDLUnsafePathReason -Path $s.BackupTo
                if ($reason){
                    $problems.Add((New-IDLGuiProblem -Page 'Uninstall' -Message "The backup folder '$($s.BackupTo)' cannot be used: $reason."))
                }
            }
        }
        elseif ($s.BackupMode -ne 'None'){
            $problems.Add((New-IDLGuiProblem -Page 'Uninstall' -Message 'Choose whether to back up the data folder first.'))
        }

        if (
            (-not $DryRun) -and
            (([string]$s.Confirmation).Trim() -cne [string]$s.ServiceName)
        ){
            $problems.Add((New-IDLGuiProblem -Page 'Uninstall' -Message "To delete the data folder, type the service name '$($s.ServiceName)' in the confirmation box (exactly, same case)."))
        }
    }

    return $problems.ToArray()
}

function New-IDLGuiUninstallCommand {
    # The Uninstall-IDLifecycle.ps1 run: same shape as New-IDLGuiInstallCommand (no secrets). The typed
    # confirmation is checked by this window, so -RemoveData is sent with -Force.
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][hashtable]$Settings,
        [Parameter(Mandatory)][string]$ScriptsDir,
        [switch]$DryRun
    )

    $s = $Settings
    $arguments = [System.Collections.Generic.List[string]]::new()
    $display = [System.Collections.Generic.List[string]]::new()
    $script = Join-Path -Path $ScriptsDir -ChildPath 'Uninstall-IDLifecycle.ps1'
    $addValue = {
        param([string]$Name, [string]$Value)

        $arguments.Add("-$Name")
        $arguments.Add($Value)
        $display.Add(('-{0} {1}' -f $Name, (ConvertTo-IDLGuiQuoted -Value $Value)))
    }
    $addSwitch = {
        param([string]$Name)

        $arguments.Add("-$Name")
        $display.Add("-$Name")
    }

    & $addValue 'ServiceName' ([string]$s.ServiceName)
    foreach ($name in @('InstallDir', 'DataDir', 'ServiceAccount')){
        if (-not [string]::IsNullOrWhiteSpace($s[$name])){
            & $addValue $name ([string]$s[$name]).Trim()
        }
    }

    if (-not $s.RemoveProgramFiles){
        $arguments.Add('-RemoveProgramFiles:$false')
        $display.Add('-RemoveProgramFiles:$false')
    }

    if ($s.RemoveData){
        & $addSwitch 'RemoveData'
        if ($s.BackupMode -eq 'Backup'){
            & $addValue 'BackupTo' ([string]$s.BackupTo).Trim()
        }
        else{
            & $addSwitch 'NoBackup'
        }

        & $addSwitch 'Force'
    }

    if ($s.RemoveDbDsnEnvVar){
        & $addSwitch 'RemoveDbDsnEnvVar'
    }

    if ($DryRun){
        & $addSwitch 'DryRun'
    }

    $arguments.Add('-NonInteractive')

    return [pscustomobject]@{
        Script     = $script
        ScriptName = 'Uninstall-IDLifecycle'
        Arguments  = [string[]]$arguments.ToArray()
        Secrets    = [ordered]@{}
        DryRun     = [bool]$DryRun
        Display    = ('& {0} {1}' -f (ConvertTo-IDLGuiQuoted -Value $script), ($display -join ' '))
    }
}

function Get-IDLGuiUninstallSummary {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][hashtable]$Settings
    )

    $s = $Settings
    $lines = [System.Collections.Generic.List[string]]::new()
    $lines.Add("Service:            $($s.ServiceName) - stopped and deleted")
    $lines.Add('Event log source:   IDLifecycle - removed (unless another IDLifecycle service still uses it)')
    $installText = (($s.InstallDir) ? ($s.InstallDir) : ('found from the service'))
    $lines.Add(('Program folder:     {0} - {1}' -f $installText, (($s.RemoveProgramFiles) ? ('removed (only the files the release installed)') : ('kept'))))
    $dataText = (($s.DataDir) ? ($s.DataDir) : ('found from the service'))
    if ($s.RemoveData){
        $backup = (($s.BackupMode -eq 'Backup') ? ("backed up to $($s.BackupTo) first") : ('NO backup'))
        $lines.Add("Data folder:        $dataText - PERMANENTLY DELETED ($backup): audit log, database, secret.key")
    }
    else{
        $lines.Add("Data folder:        $dataText - kept")
    }

    $lines.Add(('DSN variable:       {0}' -f (($s.RemoveDbDsnEnvVar) ? ('removed (if config.json names an IDL_* variable)') : ('kept'))))
    $lines.Add('Not touched:        Active Directory (accounts, the gMSA) and server databases')
    return [string[]]$lines.ToArray()
}

function Get-IDLGuiExitText {
    # What an exit code of Install-/Uninstall-IDLifecycle.ps1 means, for the result banner.
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][int]$Code,
        [switch]$DryRun,
        [switch]$Uninstall
    )

    $verb = (($Uninstall) ? ('Uninstall') : ('Installation'))
    switch ($Code){
        0{
            if ($DryRun){
                return 'Test run finished without problems. Nothing was changed - the blue WOULD lines show what a real run will do.'
            }

            return "$verb finished successfully."
        }
        1{
            return "$verb finished, but some steps failed (red lines). Fix the cause and run again - steps that are already done are skipped."
        }
        2{
            return 'Stopped: a setting or the release folder was refused (see the red line). Nothing was changed.'
        }
        3{
            return 'Stopped: something this needs is missing (see the red line), e.g. the AD PowerShell module or a database driver.'
        }
        4{
            return 'Stopped: access denied or not running as administrator.'
        }
        5{
            return 'Stopped by an unexpected error (see the red lines and the log file).'
        }
        default{
            return "Stopped: the service installer failed with exit code $Code (see the red lines)."
        }
    }
}

function Test-IDLGuiLogLine {
    # True for a line in the scripts' log format ("yyyy-MM-dd HH:mm:ss [LEVEL] message").
    [CmdletBinding()]
    param(
        [AllowEmptyString()][string]$Line
    )

    return ($Line -match '^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2} \[(INFO|WARN|ERROR|DEBUG)\] ')
}

function Format-IDLGuiLogLine {
    # A log line for the window: @{ Text ("HH:mm:ss  message"); Kind (Error, Warn, Done, Would, Skip, Debug, Info) }.
    [CmdletBinding()]
    param(
        [AllowEmptyString()][string]$Line
    )

    $m = [regex]::Match($Line, '^\d{4}-\d{2}-\d{2} (?<t>\d{2}:\d{2}:\d{2}) \[(?<l>INFO|WARN|ERROR|DEBUG)\] (?<m>.*)$')
    if (-not $m.Success){
        return [pscustomobject]@{ Text = $Line; Kind = 'Info' }
    }

    $message = $m.Groups['m'].Value
    $kind = switch ($m.Groups['l'].Value){
        'ERROR'{ 'Error' }
        'WARN'{ 'Warn' }
        'DEBUG'{ 'Debug' }
        default{
            if ($message -match '^\s*FAILED:'){
                'Error'
            }
            elseif ($message -match '^\s*(DONE|VERIFIED):|^Release ready'){
                'Done'
            }
            elseif ($message -match '^\s*WOULD:'){
                'Would'
            }
            elseif ($message -match '^\s*SKIP:'){
                'Skip'
            }
            else{
                'Info'
            }
        }
    }

    return [pscustomobject]@{ Text = ('{0}  {1}' -f $m.Groups['t'].Value, $message); Kind = $kind }
}

function Get-IDLGuiUrlFromLog {
    # The web console URL from the installer's "GUI: <url>" next-steps line, or ''.
    [CmdletBinding()]
    param(
        [AllowEmptyCollection()][string[]]$Lines
    )

    $url = ''
    foreach ($line in $Lines){
        $m = [regex]::Match($line, '\] GUI: (?<u>https?://\S+)')
        if ($m.Success){
            $url = $m.Groups['u'].Value
        }
    }

    return $url
}

function Find-IDLGuiNewLog {
    # The newest <ScriptName>_<timestamp>.log in -Dirs that is not in -Before (the log of the run just started), or ''.
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string[]]$Dirs,
        [Parameter(Mandatory)][string]$ScriptName,
        [AllowEmptyCollection()][string[]]$Before = @()
    )

    $found = @(foreach ($dir in $Dirs){
            if (Test-Path -LiteralPath $dir -PathType Container){
                Get-ChildItem -LiteralPath $dir -Filter "$($ScriptName)_2*.log" -File -ErrorAction SilentlyContinue |
                    Where-Object { $Before -notcontains $_.FullName }
            }
        })
    if ($found.Count -eq 0){
        return ''
    }

    return [string]($found | Sort-Object LastWriteTimeUtc | Select-Object -Last 1).FullName
}

function Get-IDLGuiLogSnapshot {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string[]]$Dirs,
        [Parameter(Mandatory)][string]$ScriptName
    )

    return [string[]]@(foreach ($dir in $Dirs){
            if (Test-Path -LiteralPath $dir -PathType Container){
                Get-ChildItem -LiteralPath $dir -Filter "$($ScriptName)_2*.log" -File -ErrorAction SilentlyContinue | ForEach-Object { $_.FullName }
            }
        })
}

function New-IDLGuiLogTail {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Path
    )

    return @{ Path = $Path; Offset = [long]0; Partial = ''; Decoder = [System.Text.UTF8Encoding]::new($false).GetDecoder() }
}

function Read-IDLGuiLogTail {
    # Complete lines appended to the file since the last call (the file is shared with the writer; a UTF-8
    # character split between two reads is kept in the decoder, an unfinished last line in Partial). -Final also
    # returns the unfinished last line.
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][hashtable]$Tail,
        [switch]$Final
    )

    $text = ''
    $stream = $null
    try{
        $stream = [System.IO.FileStream]::new($Tail.Path, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, ([System.IO.FileShare]::ReadWrite -bor [System.IO.FileShare]::Delete))
        if ($stream.Length -gt $Tail.Offset){
            $null = $stream.Seek($Tail.Offset, [System.IO.SeekOrigin]::Begin)
            $buffer = [byte[]]::new($stream.Length - $Tail.Offset)
            $read = $stream.Read($buffer, 0, $buffer.Length)
            $Tail.Offset += $read
            $chars = [char[]]::new($Tail.Decoder.GetCharCount($buffer, 0, $read))
            $count = $Tail.Decoder.GetChars($buffer, 0, $read, $chars, 0)
            $text = [string]::new($chars, 0, $count)
        }
    }
    catch [System.IO.IOException]{
        $text = ''
    }
    finally{
        if ($null -ne $stream){
            $stream.Dispose()
        }
    }

    $all = $Tail.Partial + $text
    $parts = $all -split "`r?`n"
    $Tail.Partial = $parts[-1]
    $lines = @($parts | Select-Object -SkipLast 1)
    if (
        ($Final) -and
        ($Tail.Partial -ne '')
    ){
        $lines += $Tail.Partial
        $Tail.Partial = ''
    }

    return [string[]]$lines
}

function Get-IDLGuiDetectedState {
    <#
      What is installed now: @{ ServiceExists; State; StartName; InstallDir; ConfigPath; DataDir; DbDriver;
      ConfigReadable }. Read-only (Win32_Service + config.json).
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$ServiceName
    )

    $state = @{
        ServiceExists  = $false
        State          = ''
        StartName      = ''
        InstallDir     = ''
        ConfigPath     = ''
        DataDir        = ''
        DbDriver       = ''
        ConfigReadable = $false
    }
    if ($ServiceName -notmatch $script:ServiceNamePattern){
        return $state
    }

    $svc = Get-CimInstance -ClassName Win32_Service -Filter ("Name='{0}'" -f $ServiceName) -ErrorAction SilentlyContinue
    if ($null -eq $svc){
        return $state
    }

    $state.ServiceExists = $true
    $state.State = [string]$svc.State
    $state.StartName = [string]$svc.StartName
    $m = [regex]::Match([string]$svc.PathName, '^\s*"(?<exe>[^"]+)"\s+service\s+-config\s+"(?<cfg>[^"]+)"')
    if ($m.Success){
        $state.InstallDir = Split-Path -Parent $m.Groups['exe'].Value
        $state.ConfigPath = $m.Groups['cfg'].Value
        $state.DataDir = Split-Path -Parent $state.ConfigPath
        try{
            $cfg = Get-Content -LiteralPath $state.ConfigPath -Raw -Encoding utf8 -ErrorAction Stop | ConvertFrom-Json -AsHashtable
            $state.ConfigReadable = $true
            $dataDir = [string](Get-IDLField -Object $cfg -Name 'data_dir')
            if ($dataDir){
                $state.DataDir = $dataDir
            }

            $driver = [string](Get-IDLField -Object (Get-IDLField -Object $cfg -Name 'database') -Name 'driver')
            $state.DbDriver = (($driver) ? ($driver.ToLowerInvariant()) : ('sqlite'))
        }
        catch{
            $state.ConfigReadable = $false
        }
    }

    return $state
}

#endregion

#region Window (WPF)

$script:Xaml = @'
<Window xmlns="http://schemas.microsoft.com/winfx/2006/xaml/presentation"
        xmlns:x="http://schemas.microsoft.com/winfx/2006/xaml"
        Title="IDLifecycle Setup" Width="940" Height="700" MinWidth="780" MinHeight="580"
        WindowStartupLocation="CenterScreen" FontFamily="Segoe UI" FontSize="13" Background="#F4F5F7">
  <Window.Resources>
    <Style TargetType="TextBlock" x:Key="H1">
      <Setter Property="FontSize" Value="20"/>
      <Setter Property="FontWeight" Value="SemiBold"/>
      <Setter Property="Margin" Value="0,0,0,6"/>
    </Style>
    <Style TargetType="TextBlock" x:Key="Lead">
      <Setter Property="TextWrapping" Value="Wrap"/>
      <Setter Property="Foreground" Value="#4A4F57"/>
      <Setter Property="Margin" Value="0,0,0,14"/>
    </Style>
    <Style TargetType="TextBlock" x:Key="Label">
      <Setter Property="FontWeight" Value="SemiBold"/>
      <Setter Property="Margin" Value="0,10,0,3"/>
    </Style>
    <Style TargetType="TextBlock" x:Key="Hint">
      <Setter Property="TextWrapping" Value="Wrap"/>
      <Setter Property="Foreground" Value="#5F6670"/>
      <Setter Property="FontSize" Value="12"/>
      <Setter Property="Margin" Value="0,3,0,0"/>
    </Style>
    <Style TargetType="TextBox">
      <Setter Property="Padding" Value="4,3"/>
    </Style>
    <Style TargetType="PasswordBox">
      <Setter Property="Padding" Value="4,3"/>
    </Style>
    <Style TargetType="Button">
      <Setter Property="Padding" Value="14,5"/>
      <Setter Property="MinWidth" Value="90"/>
    </Style>
    <Style TargetType="RadioButton">
      <Setter Property="Margin" Value="0,8,0,0"/>
      <Setter Property="FontWeight" Value="SemiBold"/>
    </Style>
    <Style TargetType="CheckBox">
      <Setter Property="Margin" Value="0,10,0,0"/>
    </Style>
    <Style TargetType="Border" x:Key="Card">
      <Setter Property="Background" Value="White"/>
      <Setter Property="BorderBrush" Value="#DADDE2"/>
      <Setter Property="BorderThickness" Value="1"/>
      <Setter Property="CornerRadius" Value="4"/>
      <Setter Property="Padding" Value="14,10"/>
      <Setter Property="Margin" Value="0,0,0,12"/>
    </Style>
  </Window.Resources>

  <DockPanel>
    <Border DockPanel.Dock="Top" Background="#1F3A5F" Padding="22,14">
      <StackPanel>
        <TextBlock Text="IDLifecycle Setup" Foreground="White" FontSize="20" FontWeight="SemiBold"/>
        <TextBlock x:Name="HeaderSubtitle" Foreground="#C9D6E8" Text="Install, upgrade or remove the IDLifecycle service"/>
      </StackPanel>
    </Border>

    <Border x:Name="ElevationBanner" DockPanel.Dock="Top" Background="#FFF4CE" BorderBrush="#E3C766" BorderThickness="0,0,0,1" Padding="22,8" Visibility="Collapsed">
      <DockPanel>
        <Button x:Name="ElevateButton" DockPanel.Dock="Right" Content="Restart as administrator" Margin="12,0,0,0"/>
        <TextBlock VerticalAlignment="Center" TextWrapping="Wrap" Text="Not running as administrator: only a test run is possible (it changes nothing). Restart as administrator to install or uninstall."/>
      </DockPanel>
    </Border>

    <Border DockPanel.Dock="Bottom" Background="White" BorderBrush="#DADDE2" BorderThickness="0,1,0,0" Padding="22,10">
      <DockPanel LastChildFill="False">
        <Button x:Name="CloseButton" DockPanel.Dock="Left" Content="Close"/>
        <Button x:Name="PrimaryButton" DockPanel.Dock="Right" Content="Next" Margin="8,0,0,0" FontWeight="SemiBold"/>
        <Button x:Name="TestRunButton" DockPanel.Dock="Right" Content="Test run (no changes)" Margin="8,0,0,0" Visibility="Collapsed"/>
        <Button x:Name="BackButton" DockPanel.Dock="Right" Content="Back"/>
      </DockPanel>
    </Border>

    <Border DockPanel.Dock="Left" Width="190" Background="#E7EAEF" Padding="0,18,0,0">
      <StackPanel x:Name="StepList"/>
    </Border>

    <TabControl x:Name="Pages" BorderThickness="0" Background="Transparent" Padding="0">
      <TabControl.ItemContainerStyle>
        <Style TargetType="TabItem">
          <Setter Property="Visibility" Value="Collapsed"/>
        </Style>
      </TabControl.ItemContainerStyle>

      <!-- Start -->
      <TabItem x:Name="PageWelcome">
        <ScrollViewer VerticalScrollBarVisibility="Auto" Padding="26,20">
          <StackPanel>
            <TextBlock Style="{StaticResource H1}" Text="What would you like to do?"/>
            <TextBlock Style="{StaticResource Lead}" Text="This window walks you through the setup and runs the IDLifecycle installer for you. Every step can be tried first with a test run that changes nothing."/>
            <Border Style="{StaticResource Card}">
              <StackPanel>
                <TextBlock Style="{StaticResource Label}" Margin="0,0,0,4" Text="This server"/>
                <TextBlock x:Name="StatusText" TextWrapping="Wrap" FontFamily="Consolas" FontSize="12.5"/>
              </StackPanel>
            </Border>
            <Button x:Name="ChooseInstallButton" HorizontalContentAlignment="Left" Padding="16,10" Margin="0,4,0,8">
              <StackPanel>
                <TextBlock x:Name="ChooseInstallTitle" FontSize="15" FontWeight="SemiBold" Text="Install IDLifecycle"/>
                <TextBlock x:Name="ChooseInstallText" TextWrapping="Wrap" Foreground="#4A4F57" Text="Copy the program, create the configuration and database, register and start the Windows service."/>
              </StackPanel>
            </Button>
            <TextBlock x:Name="InstallFromInstalledHint" Style="{StaticResource Hint}" Margin="4,-4,0,10" Visibility="Collapsed"
                       Text="This window runs from the installed program folder: to upgrade, choose GitHub or a newer release zip on the next page."/>
            <Button x:Name="ChooseUninstallButton" HorizontalContentAlignment="Left" Padding="16,10">
              <StackPanel>
                <TextBlock FontSize="15" FontWeight="SemiBold" Text="Uninstall IDLifecycle"/>
                <TextBlock TextWrapping="Wrap" Foreground="#4A4F57" Text="Stop and remove the service and the program files. The data (database, audit log) is kept unless you choose to delete it."/>
              </StackPanel>
            </Button>
          </StackPanel>
        </ScrollViewer>
      </TabItem>

      <!-- Install: source -->
      <TabItem x:Name="PageSource">
        <ScrollViewer VerticalScrollBarVisibility="Auto" Padding="26,20">
          <StackPanel>
            <TextBlock Style="{StaticResource H1}" Text="Install from"/>
            <TextBlock Style="{StaticResource Lead}" Text="Where the version to install comes from. It is checked and copied into a folder only administrators can change before anything runs from it."/>
            <RadioButton x:Name="SrcReleaseRadio" GroupName="Src" Content="This release folder"/>
            <TextBlock x:Name="SrcReleaseText" Style="{StaticResource Hint}" Margin="20,2,0,0" FontFamily="Consolas"/>
            <RadioButton x:Name="SrcGitHubRadio" GroupName="Src" Content="GitHub - download the source and build it on this server"/>
            <Border x:Name="SrcGitHubPanel" Style="{StaticResource Card}" Margin="20,8,0,0">
              <StackPanel>
                <TextBlock Style="{StaticResource Label}" Margin="0,0,0,3" Text="Repository"/>
                <TextBox x:Name="GitHubRepoBox" Width="380" HorizontalAlignment="Left"/>
                <TextBlock Style="{StaticResource Label}" Text="Branch, tag or commit"/>
                <TextBox x:Name="GitHubRefBox" Width="260" HorizontalAlignment="Left"/>
                <TextBlock Style="{StaticResource Hint}" Text="main for the latest code, or a tag such as v1.2.0 for a fixed version."/>
                <TextBlock Style="{StaticResource Label}" Text="Access token (only for a private repository)"/>
                <PasswordBox x:Name="GitHubTokenBox" Width="380" HorizontalAlignment="Left"/>
                <TextBlock Style="{StaticResource Hint}" Text="A fine-grained token with read-only 'Contents' access to the repository. Used for the download only; never stored or logged."/>
                <TextBlock x:Name="GoStatusText" Style="{StaticResource Hint}" FontSize="13" Margin="0,10,0,0"/>
              </StackPanel>
            </Border>
            <RadioButton x:Name="SrcLocalRadio" GroupName="Src" Content="A release zip or release folder"/>
            <Border x:Name="SrcLocalPanel" Style="{StaticResource Card}" Margin="20,8,0,0">
              <StackPanel>
                <DockPanel>
                  <Button x:Name="LocalFolderBrowse" DockPanel.Dock="Right" Content="Folder..." Margin="6,0,0,0"/>
                  <Button x:Name="LocalZipBrowse" DockPanel.Dock="Right" Content="Zip..." Margin="6,0,0,0"/>
                  <TextBox x:Name="LocalPathBox"/>
                </DockPanel>
                <TextBlock Style="{StaticResource Hint}" Text="Made by New-IDLifecycleRelease.ps1 (-Zip) on a machine with Go - for servers without Go or internet access. It must sit where only administrators and you can change it (e.g. your Downloads folder, or C:\Install)."/>
              </StackPanel>
            </Border>
          </StackPanel>
        </ScrollViewer>
      </TabItem>

      <!-- Install: locations -->
      <TabItem x:Name="PageLocations">
        <ScrollViewer VerticalScrollBarVisibility="Auto" Padding="26,20">
          <StackPanel>
            <TextBlock Style="{StaticResource H1}" Text="Where to install"/>
            <TextBlock Style="{StaticResource Lead}" Text="The defaults are right for most servers."/>
            <TextBlock x:Name="LocationsNote" Style="{StaticResource Hint}" Foreground="#1F5FA8" Margin="0,0,0,8" Visibility="Collapsed"/>
            <TextBlock Style="{StaticResource Label}" Text="Program folder"/>
            <DockPanel>
              <Button x:Name="InstallDirBrowse" DockPanel.Dock="Right" Content="Browse..." Margin="6,0,0,0"/>
              <TextBox x:Name="InstallDirBox"/>
            </DockPanel>
            <TextBlock Style="{StaticResource Label}" Text="Data folder"/>
            <DockPanel>
              <Button x:Name="DataDirBrowse" DockPanel.Dock="Right" Content="Browse..." Margin="6,0,0,0"/>
              <TextBox x:Name="DataDirBox"/>
            </DockPanel>
            <TextBlock Style="{StaticResource Hint}" Text="Holds the configuration, the SQLite database, the encryption key and the logs. It is locked down to administrators and the service account."/>
            <Expander Header="Advanced" Margin="0,14,0,0">
              <StackPanel Margin="4,0,0,0">
                <TextBlock Style="{StaticResource Label}" Text="Windows service name"/>
                <TextBox x:Name="ServiceNameBox" Width="260" HorizontalAlignment="Left"/>
                <TextBlock Style="{StaticResource Hint}" Text="Change only to run a second instance on the same server."/>
              </StackPanel>
            </Expander>
          </StackPanel>
        </ScrollViewer>
      </TabItem>

      <!-- Install: service account -->
      <TabItem x:Name="PageAccount">
        <ScrollViewer VerticalScrollBarVisibility="Auto" Padding="26,20">
          <StackPanel>
            <TextBlock Style="{StaticResource H1}" Text="Service account"/>
            <TextBlock Style="{StaticResource Lead}" Text="The Windows account the service runs as. It makes the Active Directory changes, so it needs delegated rights on the target OUs (set up in AD, not here)."/>
            <RadioButton x:Name="AcctManagedRadio" GroupName="Acct" Content="Group Managed Service Account (gMSA) or MSA - recommended"/>
            <TextBlock Style="{StaticResource Hint}" Margin="20,2,0,0" Text="Windows changes its password automatically. The name ends with $, e.g. CORP\svc-idl$. This server must be allowed to use it (Test-ADServiceAccount is checked)."/>
            <RadioButton x:Name="AcctUserRadio" GroupName="Acct" Content="Domain or local user account"/>
            <TextBlock Style="{StaticResource Hint}" Margin="20,2,0,0" Text="A normal account such as CORP\svc-idl, .\svc-idl or svc-idl@corp.com. You enter its password; when it changes, run this setup again with the new one."/>
            <RadioButton x:Name="AcctBuiltInRadio" GroupName="Acct" Content="Built-in Windows account"/>
            <TextBlock Style="{StaticResource Hint}" Margin="20,2,0,0" Text="Network Service, Local System or a virtual account. AD changes are then made as this server's computer account. Not recommended."/>

            <Border Style="{StaticResource Card}" Margin="0,16,0,0">
              <StackPanel>
                <StackPanel x:Name="AccountNamePanel">
                  <TextBlock x:Name="AccountNameLabel" Style="{StaticResource Label}" Margin="0,0,0,3" Text="Account name"/>
                  <TextBox x:Name="AccountNameBox" Width="340" HorizontalAlignment="Left"/>
                </StackPanel>
                <StackPanel x:Name="ServicePasswordPanel">
                  <TextBlock Style="{StaticResource Label}" Text="Password"/>
                  <PasswordBox x:Name="ServicePasswordBox" Width="340" HorizontalAlignment="Left"/>
                  <TextBlock x:Name="ServicePasswordHint" Style="{StaticResource Hint}" Text="Checked against AD before anything is changed (one sign-in attempt). Stored only by Windows for the service."/>
                </StackPanel>
                <StackPanel x:Name="BuiltInPanel">
                  <TextBlock Style="{StaticResource Label}" Margin="0,0,0,3" Text="Built-in account"/>
                  <ComboBox x:Name="BuiltInCombo" Width="340" HorizontalAlignment="Left">
                    <ComboBoxItem Tag="NT AUTHORITY\NetworkService" Content="Network Service (NT AUTHORITY\NetworkService)" IsSelected="True"/>
                    <ComboBoxItem Tag="LocalSystem" Content="Local System (LocalSystem)"/>
                    <ComboBoxItem Tag="NT AUTHORITY\LocalService" Content="Local Service (cannot sign in to AD)"/>
                    <ComboBoxItem Tag="Virtual" Content="Virtual account (NT SERVICE\service name)"/>
                  </ComboBox>
                </StackPanel>
              </StackPanel>
            </Border>
            <TextBlock Style="{StaticResource Hint}" Text="The account is granted 'Log on as a service' on this server. If a domain Group Policy sets that right, add the account there too."/>
          </StackPanel>
        </ScrollViewer>
      </TabItem>

      <!-- Install: database -->
      <TabItem x:Name="PageDatabase">
        <ScrollViewer VerticalScrollBarVisibility="Auto" Padding="26,20">
          <StackPanel>
            <TextBlock Style="{StaticResource H1}" Text="Database"/>
            <TextBlock Style="{StaticResource Lead}" Text="Where IDLifecycle keeps its rules, proposed changes and audit log. SQLite needs nothing else. A server database must already exist (empty) with a login that may create tables."/>
            <TextBlock x:Name="DbExistingNote" Style="{StaticResource Hint}" Foreground="#1F5FA8" FontSize="13" Margin="0,0,0,8" Visibility="Collapsed"/>
            <StackPanel x:Name="DbChoicePanel">
              <RadioButton x:Name="DbSqliteRadio" GroupName="Db" Tag="sqlite" Content="SQLite (built in, nothing to set up)"/>
              <RadioButton x:Name="DbMssqlRadio" GroupName="Db" Tag="mssql" Content="Microsoft SQL Server"/>
              <RadioButton x:Name="DbPostgresRadio" GroupName="Db" Tag="postgres" Content="PostgreSQL"/>
              <RadioButton x:Name="DbMysqlRadio" GroupName="Db" Tag="mysql" Content="MySQL / MariaDB"/>
              <RadioButton x:Name="DbDb2Radio" GroupName="Db" Tag="db2" Content="IBM Db2 (needs the 64-bit IBM ODBC driver on this server)"/>
              <Border x:Name="DsnPanel" Style="{StaticResource Card}" Margin="0,16,0,0">
                <StackPanel>
                  <TextBlock Style="{StaticResource Label}" Margin="0,0,0,3" Text="Connection string"/>
                  <Grid>
                    <PasswordBox x:Name="DsnPasswordBox"/>
                    <TextBox x:Name="DsnTextBox" Visibility="Collapsed"/>
                  </Grid>
                  <CheckBox x:Name="DsnShowCheck" Content="Show" Margin="0,4,0,0"/>
                  <TextBlock Style="{StaticResource Hint}" Text="Examples:"/>
                  <TextBlock x:Name="DsnExample" Style="{StaticResource Hint}" FontFamily="Consolas"/>
                  <TextBlock Style="{StaticResource Hint}" Text="Stored in config.json inside the locked-down data folder. It never appears in a log."/>
                  <CheckBox x:Name="DsnEnvCheck" Content="Keep it in a machine environment variable instead (advanced)"/>
                  <StackPanel x:Name="DsnEnvPanel" Margin="20,0,0,0">
                    <TextBox x:Name="DsnEnvNameBox" Width="260" HorizontalAlignment="Left" Margin="0,4,0,0"/>
                    <TextBlock Style="{StaticResource Hint}" Text="Name starting with IDL_. Every local user can read machine variables, and the service only sees a new one after a reboot. Leave the connection string empty if the variable already exists."/>
                  </StackPanel>
                </StackPanel>
              </Border>
            </StackPanel>
          </StackPanel>
        </ScrollViewer>
      </TabItem>

      <!-- Install: web console -->
      <TabItem x:Name="PageWeb">
        <ScrollViewer VerticalScrollBarVisibility="Auto" Padding="26,20">
          <StackPanel>
            <TextBlock Style="{StaticResource H1}" Text="Web console and administrator"/>
            <TextBlock Style="{StaticResource Lead}" Text="IDLifecycle is managed in a browser. By default the console is only reachable on this server."/>
            <TextBlock Style="{StaticResource Label}" Text="Address (host:port)"/>
            <TextBox x:Name="ListenBox" Width="260" HorizontalAlignment="Left"/>
            <TextBlock x:Name="ListenHint" Style="{StaticResource Hint}" Text="Empty = 127.0.0.1:8443 (this server only). To reach it from other computers use e.g. 0.0.0.0:8443 - that needs an HTTPS certificate."/>
            <TextBlock Style="{StaticResource Label}" Text="HTTPS certificate (PEM, optional)"/>
            <DockPanel>
              <Button x:Name="TlsCertBrowse" DockPanel.Dock="Right" Content="Browse..." Margin="6,0,0,0"/>
              <TextBox x:Name="TlsCertBox"/>
            </DockPanel>
            <TextBlock Style="{StaticResource Label}" Text="HTTPS private key (PEM)"/>
            <DockPanel>
              <Button x:Name="TlsKeyBrowse" DockPanel.Dock="Right" Content="Browse..." Margin="6,0,0,0"/>
              <TextBox x:Name="TlsKeyBox"/>
            </DockPanel>
            <Border Style="{StaticResource Card}" Margin="0,18,0,0">
              <StackPanel>
                <TextBlock Style="{StaticResource Label}" Margin="0,0,0,3" Text="First administrator of the web console"/>
                <TextBlock x:Name="AdminNote" Style="{StaticResource Hint}" Margin="0,0,0,4"/>
                <TextBlock Style="{StaticResource Label}" Text="User name"/>
                <TextBox x:Name="AdminUserBox" Width="260" HorizontalAlignment="Left"/>
                <TextBlock Style="{StaticResource Label}" Text="Password (at least 12 characters)"/>
                <PasswordBox x:Name="AdminPasswordBox" Width="260" HorizontalAlignment="Left"/>
                <TextBlock Style="{StaticResource Label}" Text="Password again"/>
                <PasswordBox x:Name="AdminPasswordConfirmBox" Width="260" HorizontalAlignment="Left"/>
              </StackPanel>
            </Border>
          </StackPanel>
        </ScrollViewer>
      </TabItem>

      <!-- Install: options -->
      <TabItem x:Name="PageOptions">
        <ScrollViewer VerticalScrollBarVisibility="Auto" Padding="26,20">
          <StackPanel>
            <TextBlock Style="{StaticResource H1}" Text="Options"/>
            <CheckBox x:Name="StartCheck" Content="Start the service when the installation is done" IsChecked="True"/>
            <CheckBox x:Name="TrayCheck" Content="Add the tray icon for everyone who signs in to this server" IsChecked="True"/>
            <TextBlock Style="{StaticResource Hint}" Margin="20,2,0,0" Text="Shows the service state and opens the admin console; restarting the service and changing the web console settings from it ask for administrator rights."/>
            <CheckBox x:Name="PrereqCheck" Content="Install the Active Directory PowerShell module (RSAT) if it is missing"/>
            <TextBlock Style="{StaticResource Hint}" Margin="20,2,0,0" Text="Runs Install-WindowsFeature RSAT-AD-PowerShell. Without it, a missing module stops the installation with a message."/>
            <CheckBox x:Name="ForceCheck" Content="Upgrade: replace program files that changed"/>
            <TextBlock Style="{StaticResource Hint}" Margin="20,2,0,0" Text="Needed to upgrade an existing installation. It also lets the installer correct the folder paths in an existing config.json and replace a stored connection string. It never bypasses the release checks."/>
            <CheckBox x:Name="SignatureCheck" Content="Require a digitally signed idlifecycled.exe"/>
            <TextBlock Style="{StaticResource Hint}" Margin="20,2,0,0" Text="Only if your organisation signs its builds."/>
            <CheckBox x:Name="SkipHashCheck" Content="Skip the SHA-256 file check (least safe - for testing only)"/>
            <TextBlock Style="{StaticResource Hint}" Margin="20,2,0,0" Foreground="#B42318" TextWrapping="Wrap" Text="Installs whatever the folder or zip holds without SHA256SUMS: nothing proves the files are the ones that were built, so a changed or added script would be installed and run by the service. Use it to try out a folder you put together yourself (an idlifecycled.exe next to the scripts folder). The folder permission check, the required files and the signature option still apply. For production, use a release built with New-IDLifecycleRelease.ps1."/>
            <CheckBox x:Name="RestoreCheck" Content="Restore from a backup (recovery, moving servers or databases)" Margin="0,18,0,0"/>
            <Border x:Name="RestorePanel" Style="{StaticResource Card}" Margin="20,8,0,0">
              <StackPanel>
                <TextBlock Style="{StaticResource Hint}" Margin="0,0,0,6" Text="An .idlbak file from Settings -> Backups (or idlifecycled backup). Its rules, history, users and encrypted secrets replace this database's contents - into the database chosen on the Database page, which may be a different type than the original."/>
                <DockPanel>
                  <Button x:Name="RestoreBrowse" DockPanel.Dock="Right" Content="Browse..." Margin="6,0,0,0"/>
                  <TextBox x:Name="RestoreFileBox"/>
                </DockPanel>
                <TextBlock Style="{StaticResource Label}" Text="Backup passphrase"/>
                <PasswordBox x:Name="RestorePassBox" Width="340" HorizontalAlignment="Left"/>
                <CheckBox x:Name="RestoreConfigCheck" Content="Also take its settings from config.json (web address, HTTPS files, CSV folders)"/>
                <TextBlock Style="{StaticResource Hint}" Margin="20,2,0,0" Text="Folders and the database connection always stay as set in this setup."/>
                <TextBlock Style="{StaticResource Hint}" Margin="0,8,0,0" Text="If this server already has data, also tick 'Upgrade: replace program files' above - it allows replacing it. Everyone signs in again afterwards, with the accounts from the backup."/>
              </StackPanel>
            </Border>
          </StackPanel>
        </ScrollViewer>
      </TabItem>

      <!-- Install: review -->
      <TabItem x:Name="PageReview">
        <DockPanel Margin="26,20">
          <StackPanel DockPanel.Dock="Top">
            <TextBlock Style="{StaticResource H1}" Text="Review"/>
            <TextBlock x:Name="ReviewLead" Style="{StaticResource Lead}" Text="Start with a test run: it checks everything (release, prerequisites, account, password) and shows what would change, without changing anything."/>
            <TextBlock x:Name="ReviewProblems" TextWrapping="Wrap" Foreground="#B3261E" Margin="0,0,0,8" Visibility="Collapsed"/>
          </StackPanel>
          <Expander DockPanel.Dock="Bottom" Header="Show the equivalent PowerShell command" Margin="0,10,0,0">
            <DockPanel Margin="0,6,0,0">
              <Button x:Name="CopyCommandButton" DockPanel.Dock="Right" Content="Copy" VerticalAlignment="Top" Margin="6,0,0,0"/>
              <TextBox x:Name="CommandBox" IsReadOnly="True" TextWrapping="Wrap" FontFamily="Consolas" FontSize="12" MaxHeight="110" VerticalScrollBarVisibility="Auto"/>
            </DockPanel>
          </Expander>
          <TextBox x:Name="SummaryBox" IsReadOnly="True" FontFamily="Consolas" FontSize="12.5" TextWrapping="Wrap" VerticalScrollBarVisibility="Auto" Background="White"/>
        </DockPanel>
      </TabItem>

      <!-- Uninstall: options -->
      <TabItem x:Name="PageUninstall">
        <ScrollViewer VerticalScrollBarVisibility="Auto" Padding="26,20">
          <StackPanel>
            <TextBlock Style="{StaticResource H1}" Text="What to remove"/>
            <Border Style="{StaticResource Card}">
              <StackPanel>
                <TextBlock x:Name="UninstallStatus" TextWrapping="Wrap" FontFamily="Consolas" FontSize="12.5"/>
              </StackPanel>
            </Border>
            <TextBlock Style="{StaticResource Label}" Text="Windows service name"/>
            <StackPanel Orientation="Horizontal">
              <TextBox x:Name="UnServiceNameBox" Width="260"/>
              <Button x:Name="UnRefreshButton" Content="Look again" Margin="6,0,0,0"/>
            </StackPanel>
            <TextBlock Style="{StaticResource Hint}" Text="The service is always stopped and deleted, and the IDLifecycle event log source removed."/>
            <CheckBox x:Name="RemoveProgramCheck" Content="Remove the program files" IsChecked="True"/>
            <TextBlock Style="{StaticResource Hint}" Margin="20,2,0,0" Text="Only the files the release installed (and its logs) are deleted; anything else in the folder is kept and listed."/>
            <CheckBox x:Name="RemoveDataCheck" Content="Also delete the data folder (database, audit log, encryption key)"/>
            <Border x:Name="RemoveDataPanel" Style="{StaticResource Card}" BorderBrush="#E3A29D" Background="#FFF7F6" Margin="20,8,0,0">
              <StackPanel>
                <TextBlock TextWrapping="Wrap" Foreground="#B3261E" FontWeight="SemiBold" Text="This permanently deletes the audit history and all settings. It cannot be undone."/>
                <RadioButton x:Name="BackupRadio" GroupName="Backup" Content="Back up the data folder first (recommended) to:" IsChecked="True"/>
                <DockPanel Margin="20,4,0,0">
                  <Button x:Name="BackupToBrowse" DockPanel.Dock="Right" Content="Browse..." Margin="6,0,0,0"/>
                  <TextBox x:Name="BackupToBox"/>
                </DockPanel>
                <TextBlock Style="{StaticResource Hint}" Margin="20,2,0,0" Text="A new zip in a folder only administrators and you can open. It contains the encryption key - keep it as safe as the server."/>
                <RadioButton x:Name="NoBackupRadio" GroupName="Backup" Content="No backup"/>
                <TextBlock x:Name="ConfirmLabel" Style="{StaticResource Label}" Margin="0,12,0,3" Text="Type the service name to confirm"/>
                <TextBox x:Name="ConfirmBox" Width="260" HorizontalAlignment="Left"/>
              </StackPanel>
            </Border>
            <CheckBox x:Name="RemoveDsnVarCheck" Content="Remove the machine environment variable that holds the connection string (if one is used)"/>
            <Expander Header="Advanced" Margin="0,14,0,0">
              <StackPanel Margin="4,0,0,0">
                <TextBlock Style="{StaticResource Hint}" Text="Leave empty to use what the service is registered with."/>
                <TextBlock Style="{StaticResource Label}" Text="Program folder"/>
                <TextBox x:Name="UnInstallDirBox"/>
                <TextBlock Style="{StaticResource Label}" Text="Data folder"/>
                <TextBox x:Name="UnDataDirBox"/>
                <TextBlock Style="{StaticResource Label}" Text="Service account (only needed when the service is already gone)"/>
                <TextBox x:Name="UnAccountBox" Width="340" HorizontalAlignment="Left"/>
              </StackPanel>
            </Expander>
            <TextBlock Style="{StaticResource Hint}" Margin="0,14,0,0" Text="Active Directory (accounts, the gMSA) and server databases are never touched; drop a server database yourself if you no longer need it."/>
          </StackPanel>
        </ScrollViewer>
      </TabItem>

      <!-- Uninstall: review -->
      <TabItem x:Name="PageUninstallReview">
        <DockPanel Margin="26,20">
          <StackPanel DockPanel.Dock="Top">
            <TextBlock Style="{StaticResource H1}" Text="Review"/>
            <TextBlock Style="{StaticResource Lead}" Text="A test run shows exactly what would be removed, without removing anything."/>
            <TextBlock x:Name="UnReviewProblems" TextWrapping="Wrap" Foreground="#B3261E" Margin="0,0,0,8" Visibility="Collapsed"/>
          </StackPanel>
          <Expander DockPanel.Dock="Bottom" Header="Show the equivalent PowerShell command" Margin="0,10,0,0">
            <DockPanel Margin="0,6,0,0">
              <Button x:Name="UnCopyCommandButton" DockPanel.Dock="Right" Content="Copy" VerticalAlignment="Top" Margin="6,0,0,0"/>
              <TextBox x:Name="UnCommandBox" IsReadOnly="True" TextWrapping="Wrap" FontFamily="Consolas" FontSize="12" MaxHeight="110" VerticalScrollBarVisibility="Auto"/>
            </DockPanel>
          </Expander>
          <TextBox x:Name="UnSummaryBox" IsReadOnly="True" FontFamily="Consolas" FontSize="12.5" TextWrapping="Wrap" VerticalScrollBarVisibility="Auto" Background="White"/>
        </DockPanel>
      </TabItem>

      <!-- Run -->
      <TabItem x:Name="PageRun">
        <DockPanel Margin="26,20">
          <StackPanel DockPanel.Dock="Top">
            <TextBlock x:Name="RunTitle" Style="{StaticResource H1}" Text="Running"/>
            <Border x:Name="RunBanner" Background="#E8F0FB" BorderBrush="#B9CDEB" BorderThickness="1" CornerRadius="4" Padding="12,8" Margin="0,0,0,10">
              <TextBlock x:Name="RunStatus" TextWrapping="Wrap" Text="Starting..."/>
            </Border>
            <ProgressBar x:Name="RunProgress" Height="4" IsIndeterminate="True" Margin="0,0,0,10"/>
          </StackPanel>
          <StackPanel DockPanel.Dock="Bottom" Orientation="Horizontal" Margin="0,10,0,0">
            <Button x:Name="OpenWebButton" Content="Open the web console" Margin="0,0,8,0" Visibility="Collapsed"/>
            <Button x:Name="OpenLogButton" Content="Open log folder" Margin="0,0,8,0" IsEnabled="False"/>
            <Button x:Name="CopyLogButton" Content="Copy log" IsEnabled="False"/>
          </StackPanel>
          <ListBox x:Name="LogList" FontFamily="Consolas" FontSize="12" Background="White" ScrollViewer.HorizontalScrollBarVisibility="Auto" VirtualizingStackPanel.IsVirtualizing="True"/>
        </DockPanel>
      </TabItem>
    </TabControl>
  </DockPanel>
</Window>
'@

function Get-IDLGuiControlNames {
    # Every x:Name in the window definition.
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Xaml
    )

    return [string[]]@([regex]::Matches($Xaml, 'x:Name="(?<n>[A-Za-z0-9_]+)"') | ForEach-Object { $_.Groups['n'].Value })
}

function Initialize-IDLGuiAssemblies {
    # $true when WPF could be loaded (not on Server Core).
    try{
        Add-Type -AssemblyName PresentationFramework, PresentationCore, WindowsBase
        return $true
    }
    catch{
        return $false
    }
}

function Set-IDLGuiConsoleVisible {
    # Hides / shows the console window this process was started in (Setup.cmd's). Best effort.
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][bool]$Visible
    )

    try{
        if (-not ('IDLifecycle.SetupConsole' -as [type])){
            Add-Type -Namespace 'IDLifecycle' -Name 'SetupConsole' -MemberDefinition @'
[DllImport("kernel32.dll")] public static extern System.IntPtr GetConsoleWindow();
[DllImport("user32.dll")] public static extern bool ShowWindow(System.IntPtr hWnd, int nCmdShow);
'@
        }

        $handle = [IDLifecycle.SetupConsole]::GetConsoleWindow()
        if ($handle -ne [System.IntPtr]::Zero){
            $null = [IDLifecycle.SetupConsole]::ShowWindow($handle, (($Visible) ? (5) : (0)))
        }
    }
    catch{
        Write-Verbose "Console window not changed: $($_.Exception.Message)"
    }
}

function Start-IDLGuiElevated {
    # Starts this window again as administrator (UAC). $true when it was started (this instance then closes).
    [CmdletBinding()]
    param(
        [string]$StartMode = 'Choose'
    )

    $argList = @('-NoProfile', '-ExecutionPolicy', 'Bypass', '-STA', '-File', ('"{0}"' -f $PSCommandPath), '-Mode', $StartMode, '-NoElevate')
    if ($script:SourceDirArg){
        $argList += @('-SourceDir', ('"{0}"' -f $script:SourceDirArg))
    }

    if ($ShowConsole){
        $argList += '-ShowConsole'
    }

    try{
        Start-Process -FilePath ([System.Environment]::ProcessPath) -ArgumentList $argList -Verb RunAs
        return $true
    }
    catch{
        # ERROR_CANCELLED (1223): the user said No in the UAC prompt.
        return $false
    }
}

function Select-IDLGuiFolder {
    [CmdletBinding()]
    param(
        [string]$Initial = '',
        [string]$Title = 'Choose a folder'
    )

    $start = $Initial
    while (
        ($start) -and
        (-not (Test-Path -LiteralPath $start -PathType Container))
    ){
        $start = Split-Path -Parent $start
    }

    if ('Microsoft.Win32.OpenFolderDialog' -as [type]){
        $dialog = [Microsoft.Win32.OpenFolderDialog]::new()
        $dialog.Title = $Title
        if ($start){
            $dialog.InitialDirectory = $start
        }

        if ($dialog.ShowDialog($script:Window)){
            return $dialog.FolderName
        }

        return $null
    }

    Add-Type -AssemblyName System.Windows.Forms
    $dialog = [System.Windows.Forms.FolderBrowserDialog]::new()
    $dialog.Description = $Title
    if ($start){
        $dialog.SelectedPath = $start
    }

    if ($dialog.ShowDialog() -eq [System.Windows.Forms.DialogResult]::OK){
        return $dialog.SelectedPath
    }

    return $null
}

function Select-IDLGuiFile {
    [CmdletBinding()]
    param(
        [string]$Initial = '',
        [string]$Title = 'Choose a file',
        [string]$Filter = 'PEM files (*.pem;*.crt;*.cer;*.key)|*.pem;*.crt;*.cer;*.key|All files (*.*)|*.*'
    )

    $dialog = [Microsoft.Win32.OpenFileDialog]::new()
    $dialog.Title = $Title
    $dialog.Filter = $Filter
    if (
        ($Initial) -and
        (Test-Path -LiteralPath (Split-Path -Parent $Initial) -PathType Container)
    ){
        $dialog.InitialDirectory = Split-Path -Parent $Initial
    }

    if ($dialog.ShowDialog($script:Window)){
        return $dialog.FileName
    }

    return $null
}

function Show-IDLGuiMessage {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Text,
        [string]$Icon = 'Information',
        [string]$Buttons = 'OK'
    )

    return [System.Windows.MessageBox]::Show($script:Window, $Text, 'IDLifecycle Setup', $Buttons, $Icon)
}

function Get-IDLGuiBrush {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Color
    )

    return [System.Windows.Media.BrushConverter]::new().ConvertFromString($Color)
}

function Get-IDLGuiDsnSecure {
    # The connection string box as a SecureString (from the visible text box while "Show" is ticked).
    if ($script:Ui.DsnShowCheck.IsChecked){
        $text = $script:Ui.DsnTextBox.Text
        return (($text) ? (ConvertTo-SecureString -String $text -AsPlainText -Force) : ($null))
    }

    return (($script:Ui.DsnPasswordBox.SecurePassword.Length -gt 0) ? ($script:Ui.DsnPasswordBox.SecurePassword) : ($null))
}

function Get-IDLGuiSelectedDriver {
    foreach ($radio in @($script:Ui.DbSqliteRadio, $script:Ui.DbMssqlRadio, $script:Ui.DbPostgresRadio, $script:Ui.DbMysqlRadio, $script:Ui.DbDb2Radio)){
        if ($radio.IsChecked){
            return [string]$radio.Tag
        }
    }

    return ''
}

function Get-IDLGuiInstallSettings {
    # The install page values as the settings hashtable the pure helpers take.
    $ui = $script:Ui
    $dataDir = $ui.DataDirBox.Text.Trim()
    $configExists = (
        (-not [string]::IsNullOrWhiteSpace($dataDir)) -and
        (Test-Path -LiteralPath (Join-Path -Path $dataDir -ChildPath 'config.json') -PathType Leaf)
    )
    $accountType = (($ui.AcctManagedRadio.IsChecked) ? ('Managed') : ((($ui.AcctUserRadio.IsChecked) ? ('Password') : ((($ui.AcctBuiltInRadio.IsChecked) ? ('BuiltIn') : (''))))))
    $serviceName = $ui.ServiceNameBox.Text.Trim()
    $detected = $script:Detected
    $sameService = ($detected.Name -ieq $serviceName)

    return @{
        SourceKind           = (($ui.SrcGitHubRadio.IsChecked) ? ('GitHub') : ((($ui.SrcLocalRadio.IsChecked) ? ('Local') : ((($ui.SrcReleaseRadio.IsChecked) ? ('Release') : (''))))))
        SourceDir            = (($ui.SrcLocalRadio.IsChecked) ? ($ui.LocalPathBox.Text.Trim()) : ($script:ReleaseRoot))
        GitHubRepo           = $ui.GitHubRepoBox.Text.Trim()
        GitHubRef            = $ui.GitHubRefBox.Text.Trim()
        GitHubToken          = (($ui.GitHubTokenBox.SecurePassword.Length -gt 0) ? ($ui.GitHubTokenBox.SecurePassword) : ($null))
        GoVersion            = $script:GoVersion
        InstallDir           = $ui.InstallDirBox.Text.Trim()
        DataDir              = $dataDir
        ServiceName          = $serviceName
        AccountType          = $accountType
        ServiceAccount       = $ui.AccountNameBox.Text.Trim()
        ServicePassword      = (($ui.ServicePasswordBox.SecurePassword.Length -gt 0) ? ($ui.ServicePasswordBox.SecurePassword) : ($null))
        BuiltInAccount       = [string]$ui.BuiltInCombo.SelectedItem.Tag
        ServiceExists        = ($sameService -and $detected.ServiceExists)
        ExistingAccount      = (($sameService) ? ($detected.StartName) : (''))
        ConfigExists         = $configExists
        DbDriver             = Get-IDLGuiSelectedDriver
        DbDsn                = Get-IDLGuiDsnSecure
        DsnInEnvVar          = [bool]$ui.DsnEnvCheck.IsChecked
        DbDsnEnvVar          = $ui.DsnEnvNameBox.Text.Trim()
        Listen               = $ui.ListenBox.Text.Trim()
        TlsCertFile          = $ui.TlsCertBox.Text.Trim()
        TlsKeyFile           = $ui.TlsKeyBox.Text.Trim()
        AdminUser            = $ui.AdminUserBox.Text.Trim()
        AdminPassword        = (($ui.AdminPasswordBox.SecurePassword.Length -gt 0) ? ($ui.AdminPasswordBox.SecurePassword) : ($null))
        AdminPasswordConfirm = (($ui.AdminPasswordConfirmBox.SecurePassword.Length -gt 0) ? ($ui.AdminPasswordConfirmBox.SecurePassword) : ($null))
        Start                = [bool]$ui.StartCheck.IsChecked
        InstallPrerequisites = [bool]$ui.PrereqCheck.IsChecked
        RequireSignature     = [bool]$ui.SignatureCheck.IsChecked
        SkipIntegrityCheck   = [bool]$ui.SkipHashCheck.IsChecked
        NoTray               = (-not [bool]$ui.TrayCheck.IsChecked)
        Force                = [bool]$ui.ForceCheck.IsChecked
        RestoreFrom          = ((($ui.RestoreCheck.IsChecked)) ? ($ui.RestoreFileBox.Text.Trim()) : (''))
        RestorePassphrase    = ((($ui.RestoreCheck.IsChecked) -and ($ui.RestorePassBox.SecurePassword.Length -gt 0)) ? ($ui.RestorePassBox.SecurePassword) : ($null))
        RestoreConfig        = ([bool]$ui.RestoreCheck.IsChecked -and [bool]$ui.RestoreConfigCheck.IsChecked)
        RestoreWanted        = [bool]$ui.RestoreCheck.IsChecked
    }
}

function Get-IDLGuiUninstallSettings {
    $ui = $script:Ui
    return @{
        ServiceName        = $ui.UnServiceNameBox.Text.Trim()
        InstallDir         = $ui.UnInstallDirBox.Text.Trim()
        DataDir            = $ui.UnDataDirBox.Text.Trim()
        ServiceAccount     = $ui.UnAccountBox.Text.Trim()
        RemoveProgramFiles = [bool]$ui.RemoveProgramCheck.IsChecked
        RemoveData         = [bool]$ui.RemoveDataCheck.IsChecked
        BackupMode         = (($ui.BackupRadio.IsChecked) ? ('Backup') : ((($ui.NoBackupRadio.IsChecked) ? ('None') : (''))))
        BackupTo           = $ui.BackupToBox.Text.Trim()
        RemoveDbDsnEnvVar  = [bool]$ui.RemoveDsnVarCheck.IsChecked
        Confirmation       = $ui.ConfirmBox.Text
    }
}

function Update-IDLGuiDetection {
    # Re-reads the service (by the given name) and refreshes the status texts.
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$ServiceName
    )

    $d = Get-IDLGuiDetectedState -ServiceName $ServiceName
    $d['Name'] = $ServiceName
    $script:Detected = $d

    $lines = [System.Collections.Generic.List[string]]::new()
    $lines.Add(('Administrator:   {0}' -f (($script:Elevated) ? ('yes') : ('NO - test runs only'))))
    if ($d.ServiceExists){
        $lines.Add("Service:         $ServiceName ($($d.State)), runs as $($d.StartName)")
        $lines.Add("Program folder:  $((($d.InstallDir) ? ($d.InstallDir) : ('(unknown)')))")
        $lines.Add("Data folder:     $((($d.DataDir) ? ($d.DataDir) : ('(unknown)')))")
        $lines.Add("Database:        $((($d.ConfigReadable) ? ($d.DbDriver) : ('(config.json not readable)')))")
    }
    else{
        $lines.Add("Service:         $ServiceName is not installed")
    }

    $release = $script:ReleaseRoot
    $isRelease = (Test-Path -LiteralPath (Join-Path -Path $release -ChildPath 'SHA256SUMS') -PathType Leaf)
    $lines.Add("This setup:      $release$((($script:RunningFromInstall) ? (' (the installed copy)') : ((($isRelease) ? (' (release folder)') : (' (a copy of the repository - install from GitHub or a release zip)')))))")
    $text = $lines -join "`n"
    $script:Ui.StatusText.Text = $text
    $script:Ui.UninstallStatus.Text = $text
}

function Set-IDLGuiRestorePanels {
    $script:Ui.RestorePanel.Visibility = (($script:Ui.RestoreCheck.IsChecked) ? ('Visible') : ('Collapsed'))
}

function Set-IDLGuiSourcePanels {
    $ui = $script:Ui
    $ui.SrcGitHubPanel.Visibility = (($ui.SrcGitHubRadio.IsChecked) ? ('Visible') : ('Collapsed'))
    $ui.SrcLocalPanel.Visibility = (($ui.SrcLocalRadio.IsChecked) ? ('Visible') : ('Collapsed'))
}

function Get-IDLGuiGoVersion {
    # 'go1.27.3' when Go is on PATH (the -FromGitHub build needs it), else ''.
    try{
        $go = Get-Command -Name 'go' -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
        if ($null -eq $go){
            return ''
        }

        $m = [regex]::Match([string]((& $go.Source version 2>$null) -join ' '), '\bgo(\d+\.\d+(\.\d+)?)\b')
        return (($m.Success) ? ('go' + $m.Groups[1].Value) : (''))
    }
    catch{
        return ''
    }
}

function Set-IDLGuiAccountPanels {
    $ui = $script:Ui
    $managed = [bool]$ui.AcctManagedRadio.IsChecked
    $user = [bool]$ui.AcctUserRadio.IsChecked
    $builtIn = [bool]$ui.AcctBuiltInRadio.IsChecked
    $ui.AccountNamePanel.Visibility = ((($managed -or $user)) ? ('Visible') : ('Collapsed'))
    $ui.ServicePasswordPanel.Visibility = (($user) ? ('Visible') : ('Collapsed'))
    $ui.BuiltInPanel.Visibility = (($builtIn) ? ('Visible') : ('Collapsed'))
    $ui.AccountNameLabel.Text = (($managed) ? ('gMSA / MSA name (ends with $)') : ('User account (DOMAIN\name, .\name or name@domain)'))
    if ($user){
        $settings = Get-IDLGuiInstallSettings
        $ui.ServicePasswordHint.Text = (((Test-IDLGuiServicePasswordNeeded -Settings $settings)) `
                ? ('Required. Checked against AD before anything is changed (one sign-in attempt). Stored only by Windows for the service.') `
                : ('The service already runs as this account: leave empty to keep the stored password, or enter the new one after it was changed.'))
    }
}

function Set-IDLGuiDatabasePanels {
    $ui = $script:Ui
    $settings = Get-IDLGuiInstallSettings
    $driver = $settings.DbDriver
    if ($settings.ConfigExists){
        $ui.DbExistingNote.Text = "An existing configuration was found in $($settings.DataDir)\config.json. Its database settings are kept; nothing on this page is used. (To change the database, edit config.json.)"
        $ui.DbExistingNote.Visibility = 'Visible'
        $ui.DbChoicePanel.IsEnabled = $false
    }
    else{
        $ui.DbExistingNote.Visibility = 'Collapsed'
        $ui.DbChoicePanel.IsEnabled = $true
    }

    $ui.DsnPanel.Visibility = ((($driver) -and ($driver -ne 'sqlite')) ? ('Visible') : ('Collapsed'))
    $ui.DsnExample.Text = (($script:DsnExamples.ContainsKey($driver)) ? ($script:DsnExamples[$driver]) : (''))
    $ui.DsnEnvPanel.Visibility = (($ui.DsnEnvCheck.IsChecked) ? ('Visible') : ('Collapsed'))
}

function Set-IDLGuiWebPanels {
    $ui = $script:Ui
    $settings = Get-IDLGuiInstallSettings
    $ui.AdminNote.Text = (($settings.ConfigExists) `
            ? ('An existing installation keeps its users: leave the password empty. It is only used when no administrator exists yet.') `
            : ('You sign in to the web console with this account the first time. More users are added in the console.'))
    $ui.ListenHint.Text = (($settings.ConfigExists) `
            ? ('Empty = keep the address in the existing config.json. To reach it from other computers use e.g. 0.0.0.0:8443 - that needs an HTTPS certificate.') `
            : ('Empty = 127.0.0.1:8443 (this server only). To reach it from other computers use e.g. 0.0.0.0:8443 - that needs an HTTPS certificate.'))
}

function Set-IDLGuiUninstallPanels {
    $ui = $script:Ui
    $ui.RemoveDataPanel.Visibility = (($ui.RemoveDataCheck.IsChecked) ? ('Visible') : ('Collapsed'))
    $ui.BackupToBox.IsEnabled = [bool]$ui.BackupRadio.IsChecked
    $ui.BackupToBrowse.IsEnabled = [bool]$ui.BackupRadio.IsChecked
    $name = $ui.UnServiceNameBox.Text.Trim()
    $ui.ConfirmLabel.Text = "Type the service name '$name' to confirm"
}

function Initialize-IDLGuiInstallDefaults {
    # Fills the install pages: defaults, or what the existing service uses (upgrade / repair).
    $ui = $script:Ui
    $d = $script:Detected
    $hasManifest = (Test-Path -LiteralPath (Join-Path -Path $script:ReleaseRoot -ChildPath 'SHA256SUMS') -PathType Leaf)
    $hasExe = (Test-Path -LiteralPath (Join-Path -Path $script:ReleaseRoot -ChildPath 'idlifecycled.exe') -PathType Leaf)
    $isRelease = ($hasManifest -and (-not $script:RunningFromInstall))
    # A folder with an exe but no SHA256SUMS can only be installed with 'Skip the SHA-256 file check' (least safe).
    $usable = (($hasManifest -or $hasExe) -and (-not $script:RunningFromInstall))
    $ui.SrcReleaseText.Text = $(
        if ($script:RunningFromInstall){ '(not available: this is the installed copy)' }
        elseif ($isRelease){ $script:ReleaseRoot }
        elseif ($hasExe){ "$($script:ReleaseRoot) - no SHA256SUMS: only with 'Skip the SHA-256 file check' under Options (least safe)" }
        else{ '(not available: this folder is not a release - it has no SHA256SUMS and no idlifecycled.exe)' }
    )
    $ui.SrcReleaseRadio.IsEnabled = $usable
    $ui.SrcReleaseRadio.IsChecked = $isRelease
    $ui.SrcGitHubRadio.IsChecked = (-not $isRelease)
    $ui.GitHubRepoBox.Text = $script:DefaultGitHubRepo
    $ui.GitHubRefBox.Text = 'main'
    $ui.GoStatusText.Text = (($script:GoVersion) ? ("Go found on this server: $($script:GoVersion)") : ('Go is NOT installed on this server: install Go 1.22+ from https://go.dev/dl/ and restart this window, or use a release zip.'))
    $ui.GoStatusText.Foreground = Get-IDLGuiBrush -Color ((($script:GoVersion)) ? ('#2E7D32') : ('#B3261E'))
    $ui.ServiceNameBox.Text = $d.Name
    $ui.InstallDirBox.Text = Join-Path -Path $env:ProgramFiles -ChildPath 'IDLifecycle'
    $ui.DataDirBox.Text = Join-Path -Path $env:ProgramData -ChildPath 'IDLifecycle'
    $ui.AdminUserBox.Text = 'admin'
    $ui.DsnEnvNameBox.Text = 'IDL_DB_DSN'
    $ui.DbSqliteRadio.IsChecked = $true
    $ui.AcctManagedRadio.IsChecked = $true

    if ($d.ServiceExists){
        if ($d.InstallDir){
            $ui.InstallDirBox.Text = $d.InstallDir
        }

        if ($d.DataDir){
            $ui.DataDirBox.Text = $d.DataDir
        }

        $account = ConvertTo-IDLServiceAccountName -Account $d.StartName
        switch (Get-IDLServiceAccountKind -Account $account){
            'Managed'{
                $ui.AcctManagedRadio.IsChecked = $true
                $ui.AccountNameBox.Text = $account
            }
            'Password'{
                $ui.AcctUserRadio.IsChecked = $true
                $ui.AccountNameBox.Text = $d.StartName
            }
            'BuiltIn'{
                $ui.AcctBuiltInRadio.IsChecked = $true
                foreach ($item in $ui.BuiltInCombo.Items){
                    if (
                        ([string]$item.Tag -ieq $account) -or
                        (([string]$item.Tag -eq 'Virtual') -and ($account -like 'NT SERVICE\*'))
                    ){
                        $ui.BuiltInCombo.SelectedItem = $item
                    }
                }
            }
        }

        $ui.ForceCheck.IsChecked = $true
        $ui.LocationsNote.Text = "IDLifecycle is already installed (service '$($d.Name)'). Its folders and account are filled in below: running the setup again upgrades or repairs it and keeps the configuration and data."
        $ui.LocationsNote.Visibility = 'Visible'
        $ui.ChooseInstallTitle.Text = 'Upgrade or repair IDLifecycle'
        $ui.ChooseInstallText.Text = 'Install the program files from this release over the existing installation. The configuration, database and users are kept.'
    }
}

function Show-IDLGuiStepList {
    # The left-hand list: the steps of the current flow, the current one highlighted.
    $ui = $script:Ui
    $ui.StepList.Children.Clear()
    $titles = @('Start')
    if ($script:Flow){
        $titles += @($script:Flows[$script:Flow] | ForEach-Object { $_.Title })
    }

    for ($i = 0; $i -lt $titles.Count; $i++){
        $current = ($i -eq ($script:Step + 1))
        $done = ($i -lt ($script:Step + 1))
        $tb = [System.Windows.Controls.TextBlock]::new()
        $tb.Text = (($done) ? ([string][char]0x2713 + '  ' + $titles[$i]) : ('    ' + $titles[$i]))
        $tb.Padding = [System.Windows.Thickness]::new(18, 7, 10, 7)
        $tb.FontWeight = (($current) ? ([System.Windows.FontWeights]::SemiBold) : ([System.Windows.FontWeights]::Normal))
        $tb.Foreground = Get-IDLGuiBrush -Color ((($current) ? ('#1F3A5F') : ((($done) ? ('#3C7A47') : ('#5F6670')))))
        if ($current){
            $tb.Background = Get-IDLGuiBrush -Color '#D5DDE8'
        }

        $null = $ui.StepList.Children.Add($tb)
    }
}

function Show-IDLGuiPage {
    # Shows the page of the current step and sets the buttons for it.
    $ui = $script:Ui
    $running = ($null -ne $script:Run)
    if ($script:Step -lt 0){
        $ui.Pages.SelectedItem = $ui.PageWelcome
        $ui.BackButton.Visibility = 'Collapsed'
        $ui.PrimaryButton.Visibility = 'Collapsed'
        $ui.TestRunButton.Visibility = 'Collapsed'
        $ui.HeaderSubtitle.Text = 'Install, upgrade or remove the IDLifecycle service'
        Show-IDLGuiStepList
        return
    }

    $page = $script:Flows[$script:Flow][$script:Step].Page
    $ui.Pages.SelectedItem = $ui[$page]
    $ui.HeaderSubtitle.Text = (($script:Flow -eq 'Install') ? ('Install or upgrade') : ('Uninstall'))
    $ui.BackButton.Visibility = 'Visible'
    $ui.PrimaryButton.Visibility = 'Visible'
    $ui.TestRunButton.Visibility = 'Collapsed'
    $ui.PrimaryButton.IsEnabled = $true
    $ui.BackButton.IsEnabled = $true
    $ui.CloseButton.IsEnabled = $true
    $ui.PrimaryButton.Content = 'Next'

    switch ($page){
        'PageSource'{
            Set-IDLGuiSourcePanels
        }
        'PageOptions'{
            Set-IDLGuiRestorePanels
        }
        'PageAccount'{
            Set-IDLGuiAccountPanels
        }
        'PageDatabase'{
            Set-IDLGuiDatabasePanels
        }
        'PageWeb'{
            Set-IDLGuiWebPanels
        }
        'PageUninstall'{
            Set-IDLGuiUninstallPanels
        }
        'PageReview'{
            Update-IDLGuiReview
            $ui.TestRunButton.Visibility = 'Visible'
            $ui.PrimaryButton.Content = (($script:Detected.ServiceExists) ? ('Upgrade') : ('Install'))
            $ui.PrimaryButton.IsEnabled = $script:Elevated
        }
        'PageUninstallReview'{
            Update-IDLGuiReview
            $ui.TestRunButton.Visibility = 'Visible'
            $ui.PrimaryButton.Content = 'Uninstall'
            $ui.PrimaryButton.IsEnabled = $script:Elevated
        }
        'PageRun'{
            $ui.BackButton.IsEnabled = (-not $running)
            $ui.CloseButton.IsEnabled = (-not $running)
            $ui.PrimaryButton.IsEnabled = (-not $running)
            $last = $script:LastRun
            if (
                (-not $running) -and
                ($null -ne $last) -and
                ($last.DryRun) -and
                ($last.ExitCode -eq 0) -and
                ($script:Elevated)
            ){
                $ui.PrimaryButton.Content = (($script:Flow -eq 'Install') ? ((($script:Detected.ServiceExists) ? ('Upgrade now') : ('Install now'))) : ('Uninstall now'))
            }
            else{
                $ui.PrimaryButton.Content = 'Finish'
            }
        }
    }

    Show-IDLGuiStepList
}

function Update-IDLGuiReview {
    $ui = $script:Ui
    if ($script:Flow -eq 'Install'){
        $settings = Get-IDLGuiInstallSettings
        $ui.SummaryBox.Text = (Get-IDLGuiInstallSummary -Settings $settings) -join "`r`n"
        $ui.CommandBox.Text = (New-IDLGuiInstallCommand -Settings $settings -ScriptsDir $script:ScriptsDir).Display
        $problems = @(Get-IDLGuiInstallProblems -Settings $settings)
        $box = $ui.ReviewProblems
    }
    else{
        $settings = Get-IDLGuiUninstallSettings
        $ui.UnSummaryBox.Text = (Get-IDLGuiUninstallSummary -Settings $settings) -join "`r`n"
        $ui.UnCommandBox.Text = (New-IDLGuiUninstallCommand -Settings $settings -ScriptsDir $script:ScriptsDir).Display
        $problems = @(Get-IDLGuiUninstallProblems -Settings $settings)
        $box = $ui.UnReviewProblems
    }

    if ($problems.Count -gt 0){
        $box.Text = "Still needed before a real run (a test run works without these):`n" + (($problems | ForEach-Object { [string][char]0x2022 + ' ' + $_.Message }) -join "`n")
        $box.Visibility = 'Visible'
    }
    else{
        $box.Visibility = 'Collapsed'
    }
}

function Get-IDLGuiPageTag {
    # The problem Page tag that belongs to a TabItem name.
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Page
    )

    return @{ PageSource = 'Source'; PageLocations = 'Locations'; PageAccount = 'Account'; PageDatabase = 'Database'; PageWeb = 'Web'; PageOptions = 'Options'; PageUninstall = 'Uninstall' }[$Page]
}

function Invoke-IDLGuiNext {
    $page = $script:Flows[$script:Flow][$script:Step].Page
    $tag = Get-IDLGuiPageTag -Page $page
    if ($tag){
        # Non-administrators can only test-run, so they are not asked for what only a real run needs.
        $dry = (-not $script:Elevated)
        $all = (($script:Flow -eq 'Install') `
                ? (@(Get-IDLGuiInstallProblems -Settings (Get-IDLGuiInstallSettings) -DryRun:$dry)) `
                : (@(Get-IDLGuiUninstallProblems -Settings (Get-IDLGuiUninstallSettings) -DryRun:$dry)))
        $mine = @($all | Where-Object { $_.Page -eq $tag })
        if ($mine.Count -gt 0){
            $null = Show-IDLGuiMessage -Text (($mine | ForEach-Object { $_.Message }) -join "`n`n") -Icon 'Warning'
            return
        }
    }

    if ($page -eq 'PageLocations'){
        Update-IDLGuiDetection -ServiceName $script:Ui.ServiceNameBox.Text.Trim()
    }

    $script:Step++
    Show-IDLGuiPage
}

function Add-IDLGuiLogLine {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][AllowEmptyString()][string]$Text,
        [string]$Kind = 'Info'
    )

    $colors = @{ Error = '#B3261E'; Warn = '#8A5A00'; Done = '#2E7D32'; Would = '#1F5FA8'; Skip = '#6B7280'; Debug = '#9AA0A6'; Info = '#1F2328' }
    $item = [System.Windows.Controls.ListBoxItem]::new()
    $item.Content = $Text
    $item.Foreground = Get-IDLGuiBrush -Color $colors[$Kind]
    if ($Kind -eq 'Error'){
        $item.FontWeight = [System.Windows.FontWeights]::SemiBold
    }

    $list = $script:Ui.LogList
    $null = $list.Items.Add($item)
    $list.ScrollIntoView($item)
}

function Start-IDLGuiRun {
    # Runs the command in a child pwsh and follows its log file. Secrets go only into the child's environment.
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]$Command
    )

    $ui = $script:Ui
    $logDirs = @(
        (Join-Path -Path $env:ProgramData -ChildPath 'IDLifecycle-Setup\Logs'),
        (Join-Path -Path $env:LOCALAPPDATA -ChildPath 'IDLifecycle-Setup\Logs')
    )
    $before = Get-IDLGuiLogSnapshot -Dirs $logDirs -ScriptName $Command.ScriptName

    $psi = [System.Diagnostics.ProcessStartInfo]::new([System.Environment]::ProcessPath)
    foreach ($a in (@('-NoProfile', '-NonInteractive', '-ExecutionPolicy', 'Bypass', '-File', $Command.Script) + @($Command.Arguments))){
        $psi.ArgumentList.Add([string]$a)
    }

    $psi.UseShellExecute = $false
    $psi.CreateNoWindow = $true
    $psi.RedirectStandardInput = $true
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError = $true
    $psi.StandardOutputEncoding = [System.Text.UTF8Encoding]::new($false)
    $psi.StandardErrorEncoding = [System.Text.UTF8Encoding]::new($false)
    foreach ($name in $script:SecretEnvNames){
        $null = $psi.Environment.Remove($name)
    }

    $process = $null
    try{
        foreach ($name in $Command.Secrets.Keys){
            $psi.Environment[$name] = [System.Net.NetworkCredential]::new('', $Command.Secrets[$name]).Password
        }

        $process = [System.Diagnostics.Process]::Start($psi)
    }
    finally{
        foreach ($name in $script:SecretEnvNames){
            $null = $psi.Environment.Remove($name)
        }
    }

    $process.StandardInput.Close()

    $isUninstall = ($Command.ScriptName -eq 'Uninstall-IDLifecycle')
    $ui.LogList.Items.Clear()
    $ui.OpenWebButton.Visibility = 'Collapsed'
    $ui.OpenLogButton.IsEnabled = $false
    $ui.CopyLogButton.IsEnabled = $false
    $ui.RunProgress.Visibility = 'Visible'
    $ui.RunTitle.Text = (($Command.DryRun) ? ((($isUninstall) ? ('Test run: uninstall') : ('Test run: install'))) : ((($isUninstall) ? ('Uninstalling') : ('Installing'))))
    $ui.RunStatus.Text = (($Command.DryRun) ? ('Checking everything - nothing is changed...') : ('Working - this usually takes under a minute. Do not close this window.'))
    $ui.RunBanner.Background = Get-IDLGuiBrush -Color '#E8F0FB'
    $ui.RunBanner.BorderBrush = Get-IDLGuiBrush -Color '#B9CDEB'

    $script:Run = @{
        Process     = $process
        OutTask     = $process.StandardOutput.ReadLineAsync()
        ErrTask     = $process.StandardError.ReadLineAsync()
        OutDone     = $false
        ErrDone     = $false
        LogDirs     = $logDirs
        Before      = $before
        Tail        = $null
        Seen        = [System.Collections.Generic.List[string]]::new()
        ScriptName  = $Command.ScriptName
        DryRun      = $Command.DryRun
        Uninstall   = $isUninstall
        Lines       = [System.Collections.Generic.List[string]]::new()
        StdLines    = [System.Collections.Generic.List[string]]::new()
        StartedUtc  = [DateTime]::UtcNow
    }

    $script:Step = $script:Flows[$script:Flow].Count - 1
    Show-IDLGuiPage
    $script:Timer.Start()
}

function Read-IDLGuiStream {
    # Drains the finished ReadLineAsync results of one redirected stream (stdout/stderr of the child).
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][ValidateSet('Out', 'Err')][string]$Which
    )

    $r = $script:Run
    $reader = (($Which -eq 'Out') ? ($r.Process.StandardOutput) : ($r.Process.StandardError))
    while (
        (-not $r["$($Which)Done"]) -and
        ($r["$($Which)Task"].IsCompleted)
    ){
        $line = $r["$($Which)Task"].Result
        if ($null -eq $line){
            $r["$($Which)Done"] = $true
            break
        }

        if (-not [string]::IsNullOrWhiteSpace($line)){
            $r.StdLines.Add($line)
            # Log-format lines also reach the log file (shown from there); anything else (e.g. "cannot use the log
            # folder") is shown at once.
            if (-not (Test-IDLGuiLogLine -Line $line)){
                Add-IDLGuiLogLine -Text $line -Kind ((($Which -eq 'Err')) ? ('Error') : ('Info'))
                $r.Lines.Add($line)
            }
        }

        $r["$($Which)Task"] = $reader.ReadLineAsync()
    }
}

function Show-IDLGuiLogLines {
    [CmdletBinding()]
    param(
        [AllowEmptyCollection()][string[]]$Lines
    )

    foreach ($line in $Lines){
        $script:Run.Lines.Add($line)
        $f = Format-IDLGuiLogLine -Line $line
        Add-IDLGuiLogLine -Text $f.Text -Kind $f.Kind
    }
}

function Invoke-IDLGuiTick {
    # DispatcherTimer tick (UI thread): new output, new log lines, and the end of the run.
    $r = $script:Run
    if ($null -eq $r){
        $script:Timer.Stop()
        return
    }

    Read-IDLGuiStream -Which 'Out'
    Read-IDLGuiStream -Which 'Err'

    if ($null -eq $r.Tail){
        $path = Find-IDLGuiNewLog -Dirs $r.LogDirs -ScriptName $r.ScriptName -Before $r.Before
        if ($path){
            $r.Tail = New-IDLGuiLogTail -Path $path
        }
    }

    if ($null -ne $r.Tail){
        Show-IDLGuiLogLines -Lines (Read-IDLGuiLogTail -Tail $r.Tail)
        # GitHub / zip source: the installer hands over to the staged release's installer, which writes its own log.
        $newer = Find-IDLGuiNewLog -Dirs $r.LogDirs -ScriptName $r.ScriptName -Before (@($r.Before) + @($r.Seen) + @($r.Tail.Path))
        if ($newer){
            Show-IDLGuiLogLines -Lines (Read-IDLGuiLogTail -Tail $r.Tail -Final)
            Add-IDLGuiLogLine -Text '---- continued by the installer of the version being installed ----' -Kind 'Info'
            $r.Seen.Add($r.Tail.Path)
            $r.Tail = New-IDLGuiLogTail -Path $newer
            Show-IDLGuiLogLines -Lines (Read-IDLGuiLogTail -Tail $r.Tail)
        }
    }

    if (
        (-not $r.Process.HasExited) -or
        (-not $r.OutDone) -or
        (-not $r.ErrDone)
    ){
        return
    }

    $script:Timer.Stop()
    if ($null -eq $r.Tail){
        $path = Find-IDLGuiNewLog -Dirs $r.LogDirs -ScriptName $r.ScriptName -Before $r.Before
        if ($path){
            $r.Tail = New-IDLGuiLogTail -Path $path
        }
    }

    if ($null -ne $r.Tail){
        Show-IDLGuiLogLines -Lines (Read-IDLGuiLogTail -Tail $r.Tail -Final)
    }
    else{
        # No log file (the script stopped before it could create one): show what it printed.
        Show-IDLGuiLogLines -Lines @($r.StdLines | Where-Object { Test-IDLGuiLogLine -Line $_ })
    }

    $code = $r.Process.ExitCode
    $r.Process.Dispose()
    Complete-IDLGuiRun -ExitCode $code
}

function Complete-IDLGuiRun {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][int]$ExitCode
    )

    $ui = $script:Ui
    $r = $script:Run
    $ui.RunProgress.Visibility = 'Collapsed'
    $ui.RunStatus.Text = Get-IDLGuiExitText -Code $ExitCode -DryRun:$r.DryRun -Uninstall:$r.Uninstall
    $ok = ($ExitCode -eq 0)
    $ui.RunBanner.Background = Get-IDLGuiBrush -Color ((($ok) ? ('#E7F4EA') : ('#FBEAE9')))
    $ui.RunBanner.BorderBrush = Get-IDLGuiBrush -Color ((($ok) ? ('#A8D5B2') : ('#E3A29D')))
    $ui.RunTitle.Text = (($ok) ? ((($r.DryRun) ? ('Test run complete') : ('Done'))) : ('Stopped'))

    $script:LastLogPath = (($null -ne $r.Tail) ? ($r.Tail.Path) : (''))
    $script:LastLines = [string[]]$r.Lines.ToArray()
    $ui.OpenLogButton.IsEnabled = $true
    $ui.CopyLogButton.IsEnabled = ($script:LastLines.Count -gt 0)

    $script:LastUrl = ''
    if (
        $ok -and
        (-not $r.DryRun) -and
        (-not $r.Uninstall)
    ){
        $script:LastUrl = Get-IDLGuiUrlFromLog -Lines $script:LastLines
        if ($script:LastUrl){
            $ui.OpenWebButton.Visibility = 'Visible'
            $ui.RunStatus.Text += " Sign in to the web console at $($script:LastUrl), then follow the first-time configuration steps in the install guide (AD settings, sources, mappings, roles - and a sync in dry-run first)."
        }
    }

    $script:LastRun = @{ DryRun = $r.DryRun; ExitCode = $ExitCode }
    $script:Run = $null
    if (
        (-not $r.DryRun) -and
        $ok
    ){
        Update-IDLGuiDetection -ServiceName $script:Detected.Name
    }

    Show-IDLGuiPage
}

function Get-IDLGuiUnverifiedWarning {
    return "Install WITHOUT the SHA-256 file check?`n`nThis is the least safe way to install: nothing proves the program and scripts are the ones that were built or reviewed, and the service will run them with its rights in Active Directory.`n`nUse it only for testing. For production, install from a release built with New-IDLifecycleRelease.ps1 (or from GitHub, which checks its own build)."
}

function Invoke-IDLGuiStartRun {
    # Validates and starts a test run (-DryRun) or a real run of the current flow.
    [CmdletBinding()]
    param(
        [switch]$DryRun
    )

    if ($script:Flow -eq 'Install'){
        $settings = Get-IDLGuiInstallSettings
        $problems = @(Get-IDLGuiInstallProblems -Settings $settings -DryRun:$DryRun)
        $command = New-IDLGuiInstallCommand -Settings $settings -ScriptsDir $script:ScriptsDir -DryRun:$DryRun
    }
    else{
        $settings = Get-IDLGuiUninstallSettings
        $problems = @(Get-IDLGuiUninstallProblems -Settings $settings -DryRun:$DryRun)
        $command = New-IDLGuiUninstallCommand -Settings $settings -ScriptsDir $script:ScriptsDir -DryRun:$DryRun
    }

    if ($problems.Count -gt 0){
        $null = Show-IDLGuiMessage -Text (($problems | ForEach-Object { $_.Message }) -join "`n`n") -Icon 'Warning'
        return
    }

    if (
        (-not $DryRun) -and
        ($script:Flow -eq 'Uninstall')
    ){
        $what = (($settings.RemoveData) ? ("`n`nThe data folder will be PERMANENTLY deleted$((($settings.BackupMode -eq 'Backup') ? (' (after the backup)') : (' - without a backup'))).") : (''))
        $answer = Show-IDLGuiMessage -Text "Remove IDLifecycle (service '$($settings.ServiceName)') from this server?$what" -Icon 'Warning' -Buttons 'YesNo'
        if ($answer -ne [System.Windows.MessageBoxResult]::Yes){
            return
        }
    }

    if (
        (-not $DryRun) -and
        ($script:Flow -eq 'Install') -and
        ([bool]$settings['SkipIntegrityCheck'])
    ){
        $answer = Show-IDLGuiMessage -Text (Get-IDLGuiUnverifiedWarning) -Icon 'Warning' -Buttons 'YesNo'
        if ($answer -ne [System.Windows.MessageBoxResult]::Yes){
            return
        }
    }

    Start-IDLGuiRun -Command $command
}

function Invoke-IDLGuiSafely {
    # Runs an event handler; an unexpected error is shown instead of being lost.
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][scriptblock]$Action
    )

    try{
        & $Action
    }
    catch{
        $null = Show-IDLGuiMessage -Text "Unexpected error: $($_.Exception.Message)" -Icon 'Error'
    }
}

function Register-IDLGuiHandlers {
    $ui = $script:Ui

    $ui.ElevateButton.Add_Click({
            Invoke-IDLGuiSafely {
                $startMode = (($script:Flow) ? ($script:Flow) : ('Choose'))
                if (Start-IDLGuiElevated -StartMode $startMode){
                    $script:Window.Close()
                }
                else{
                    $null = Show-IDLGuiMessage -Text 'Administrator rights were not granted. You can still do a test run.'
                }
            }
        })

    $ui.ChooseInstallButton.Add_Click({
            Invoke-IDLGuiSafely {
                $script:Flow = 'Install'
                $script:Step = 0
                Show-IDLGuiPage
            }
        })

    $ui.ChooseUninstallButton.Add_Click({
            Invoke-IDLGuiSafely {
                $script:Flow = 'Uninstall'
                $script:Step = 0
                Show-IDLGuiPage
            }
        })

    $ui.BackButton.Add_Click({
            Invoke-IDLGuiSafely {
                $script:Step--
                if ($script:Step -lt 0){
                    $script:Flow = ''
                }

                Show-IDLGuiPage
            }
        })

    $ui.PrimaryButton.Add_Click({
            Invoke-IDLGuiSafely {
                $page = $script:Flows[$script:Flow][$script:Step].Page
                switch ($page){
                    { $_ -in @('PageReview', 'PageUninstallReview') }{
                        Invoke-IDLGuiStartRun
                    }
                    'PageRun'{
                        if ([string]$script:Ui.PrimaryButton.Content -like '* now'){
                            Invoke-IDLGuiStartRun
                        }
                        else{
                            $script:Window.Close()
                        }
                    }
                    default{
                        Invoke-IDLGuiNext
                    }
                }
            }
        })

    $ui.TestRunButton.Add_Click({ Invoke-IDLGuiSafely { Invoke-IDLGuiStartRun -DryRun } })
    $ui.CloseButton.Add_Click({ $script:Window.Close() })

    foreach ($radio in @($ui.AcctManagedRadio, $ui.AcctUserRadio, $ui.AcctBuiltInRadio)){
        $radio.Add_Checked({ Invoke-IDLGuiSafely { Set-IDLGuiAccountPanels } })
    }

    $ui.AccountNameBox.Add_TextChanged({ Invoke-IDLGuiSafely { Set-IDLGuiAccountPanels } })

    foreach ($radio in @($ui.DbSqliteRadio, $ui.DbMssqlRadio, $ui.DbPostgresRadio, $ui.DbMysqlRadio, $ui.DbDb2Radio)){
        $radio.Add_Checked({ Invoke-IDLGuiSafely { Set-IDLGuiDatabasePanels } })
    }

    $ui.DsnEnvCheck.Add_Click({ Invoke-IDLGuiSafely { Set-IDLGuiDatabasePanels } })
    $ui.DsnShowCheck.Add_Click({
            Invoke-IDLGuiSafely {
                if ($script:Ui.DsnShowCheck.IsChecked){
                    $script:Ui.DsnTextBox.Text = $script:Ui.DsnPasswordBox.Password
                    $script:Ui.DsnPasswordBox.Password = ''
                    $script:Ui.DsnPasswordBox.Visibility = 'Collapsed'
                    $script:Ui.DsnTextBox.Visibility = 'Visible'
                }
                else{
                    $script:Ui.DsnPasswordBox.Password = $script:Ui.DsnTextBox.Text
                    $script:Ui.DsnTextBox.Text = ''
                    $script:Ui.DsnTextBox.Visibility = 'Collapsed'
                    $script:Ui.DsnPasswordBox.Visibility = 'Visible'
                }
            }
        })

    $ui.RemoveDataCheck.Add_Click({ Invoke-IDLGuiSafely { Set-IDLGuiUninstallPanels } })
    $ui.BackupRadio.Add_Checked({ Invoke-IDLGuiSafely { Set-IDLGuiUninstallPanels } })
    $ui.NoBackupRadio.Add_Checked({ Invoke-IDLGuiSafely { Set-IDLGuiUninstallPanels } })
    $ui.UnServiceNameBox.Add_TextChanged({ Invoke-IDLGuiSafely { Set-IDLGuiUninstallPanels } })
    $script:Ui.UnRefreshButton.Add_Click({ Invoke-IDLGuiSafely { Update-IDLGuiDetection -ServiceName $script:Ui.UnServiceNameBox.Text.Trim() } })

    $folderPickers = @(
        @($ui.LocalFolderBrowse, $ui.LocalPathBox, 'Choose the IDLifecycle release folder'),
        @($ui.InstallDirBrowse, $ui.InstallDirBox, 'Choose the program folder'),
        @($ui.DataDirBrowse, $ui.DataDirBox, 'Choose the data folder'),
        @($ui.BackupToBrowse, $ui.BackupToBox, 'Choose where to put the backup')
    )
    foreach ($picker in $folderPickers){
        $button = $picker[0]
        $button.Tag = @{ Box = $picker[1]; Title = $picker[2] }
        $button.Add_Click({
                param($btn)
                Invoke-IDLGuiSafely {
                    $chosen = Select-IDLGuiFolder -Initial $btn.Tag.Box.Text -Title $btn.Tag.Title
                    if ($chosen){
                        $btn.Tag.Box.Text = $chosen
                    }
                }
            })
    }

    $ui.RestoreCheck.Add_Click({ Invoke-IDLGuiSafely { Set-IDLGuiRestorePanels } })
    $ui.RestoreBrowse.Add_Click({
            Invoke-IDLGuiSafely {
                $chosen = Select-IDLGuiFile -Initial $script:Ui.RestoreFileBox.Text -Title 'Choose the IDLifecycle backup' -Filter 'IDLifecycle backup (*.idlbak)|*.idlbak|All files (*.*)|*.*'
                if ($chosen){
                    $script:Ui.RestoreFileBox.Text = $chosen
                }
            }
        })

    $ui.LocalZipBrowse.Add_Click({
            Invoke-IDLGuiSafely {
                $chosen = Select-IDLGuiFile -Initial $script:Ui.LocalPathBox.Text -Title 'Choose the IDLifecycle release zip' -Filter 'Release zip (*.zip)|*.zip'
                if ($chosen){
                    $script:Ui.LocalPathBox.Text = $chosen
                }
            }
        })

    foreach ($radio in @($ui.SrcReleaseRadio, $ui.SrcGitHubRadio, $ui.SrcLocalRadio)){
        $radio.Add_Checked({ Invoke-IDLGuiSafely { Set-IDLGuiSourcePanels } })
    }

    foreach ($picker in @(@($ui.TlsCertBrowse, $ui.TlsCertBox, 'Choose the certificate file (PEM)'), @($ui.TlsKeyBrowse, $ui.TlsKeyBox, 'Choose the private key file (PEM)'))){
        $button = $picker[0]
        $button.Tag = @{ Box = $picker[1]; Title = $picker[2] }
        $button.Add_Click({
                param($btn)
                Invoke-IDLGuiSafely {
                    $chosen = Select-IDLGuiFile -Initial $btn.Tag.Box.Text -Title $btn.Tag.Title
                    if ($chosen){
                        $btn.Tag.Box.Text = $chosen
                    }
                }
            })
    }

    $script:Ui.CopyCommandButton.Add_Click({ Invoke-IDLGuiSafely { [System.Windows.Clipboard]::SetText($script:Ui.CommandBox.Text) } })
    $script:Ui.UnCopyCommandButton.Add_Click({ Invoke-IDLGuiSafely { [System.Windows.Clipboard]::SetText($script:Ui.UnCommandBox.Text) } })
    $ui.CopyLogButton.Add_Click({ Invoke-IDLGuiSafely { [System.Windows.Clipboard]::SetText(($script:LastLines -join "`r`n")) } })
    $ui.OpenWebButton.Add_Click({ Invoke-IDLGuiSafely { Start-Process -FilePath $script:LastUrl } })
    $ui.OpenLogButton.Add_Click({
            Invoke-IDLGuiSafely {
                if ($script:LastLogPath){
                    Start-Process -FilePath 'explorer.exe' -ArgumentList ('/select,"{0}"' -f $script:LastLogPath)
                }
                else{
                    $dir = (($script:Elevated) ? (Join-Path -Path $env:ProgramData -ChildPath 'IDLifecycle-Setup\Logs') : (Join-Path -Path $env:LOCALAPPDATA -ChildPath 'IDLifecycle-Setup\Logs'))
                    Start-Process -FilePath 'explorer.exe' -ArgumentList ('"{0}"' -f $dir)
                }
            }
        })

    $script:Window.Add_Closing({
            param($win, $e)
            if ($null -ne $script:Run){
                $null = Show-IDLGuiMessage -Text 'The setup is still running. Wait until it has finished - stopping it halfway could leave the installation half done.' -Icon 'Warning'
                $e.Cancel = $true
            }
        })
}

function Set-IDLGuiBranding {
    # Window icon and title from the brand (scripts\branding, or a trusted %ProgramData%\IDLifecycle-Branding).
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]$Window
    )

    try{
        $brand = Get-IDLBranding -DefaultDir (Join-Path -Path $PSScriptRoot -ChildPath 'branding')
        if ($brand.ProductName -ne 'IDLifecycle'){
            $Window.Title = ([string]$Window.Title).Replace('IDLifecycle', $brand.ProductName)
        }

        if ($brand.IconPath){
            $Window.Icon = [System.Windows.Media.Imaging.BitmapFrame]::Create([uri]::new($brand.IconPath))
        }
    }
    catch{
        Write-Verbose "Branding not applied: $($_.Exception.Message)"
    }
}

function Initialize-IDLGuiWindow {
    # Builds the window, fills it from what is installed, wires the handlers and shows the first page.
    [CmdletBinding()]
    param(
        [string]$StartMode = 'Choose'
    )

    $reader = [System.Xml.XmlReader]::Create([System.IO.StringReader]::new($script:Xaml))
    $script:Window = [System.Windows.Markup.XamlReader]::Load($reader)
    $script:Ui = @{}
    foreach ($name in (Get-IDLGuiControlNames -Xaml $script:Xaml)){
        $script:Ui[$name] = $script:Window.FindName($name)
    }

    Set-IDLGuiBranding -Window $script:Window

    $script:Flows = @{
        Install   = @(
            @{ Page = 'PageSource'; Title = 'Source' },
            @{ Page = 'PageLocations'; Title = 'Locations' },
            @{ Page = 'PageAccount'; Title = 'Service account' },
            @{ Page = 'PageDatabase'; Title = 'Database' },
            @{ Page = 'PageWeb'; Title = 'Web console' },
            @{ Page = 'PageOptions'; Title = 'Options' },
            @{ Page = 'PageReview'; Title = 'Review' },
            @{ Page = 'PageRun'; Title = 'Run' }
        )
        Uninstall = @(
            @{ Page = 'PageUninstall'; Title = 'What to remove' },
            @{ Page = 'PageUninstallReview'; Title = 'Review' },
            @{ Page = 'PageRun'; Title = 'Run' }
        )
    }
    $script:Flow = ''
    $script:Step = -1
    $script:Run = $null
    $script:LastRun = $null
    $script:LastLines = [string[]]@()
    $script:LastLogPath = ''
    $script:LastUrl = ''
    $script:RunningFromInstall = $false
    $script:GoVersion = Get-IDLGuiGoVersion

    Update-IDLGuiDetection -ServiceName 'IDLifecycle'
    $script:RunningFromInstall = (
        ($script:Detected.ServiceExists) -and
        ($script:Detected.InstallDir) -and
        (Test-IDLPathWithin -Child $script:ScriptsDir -Parent $script:Detected.InstallDir)
    )
    Update-IDLGuiDetection -ServiceName 'IDLifecycle'
    Initialize-IDLGuiInstallDefaults
    $script:Ui.UnServiceNameBox.Text = $script:Detected.Name
    $script:Ui.ElevationBanner.Visibility = (($script:Elevated) ? ('Collapsed') : ('Visible'))
    if ($script:RunningFromInstall){
        $script:Ui.InstallFromInstalledHint.Visibility = 'Visible'
    }

    $script:Timer = [System.Windows.Threading.DispatcherTimer]::new()
    $script:Timer.Interval = [TimeSpan]::FromMilliseconds(200)
    $script:Timer.Add_Tick({
            try{
                Invoke-IDLGuiTick
            }
            catch{
                $script:Timer.Stop()
                $script:Run = $null
                Add-IDLGuiLogLine -Text "Setup window error while following the run: $($_.Exception.Message)" -Kind 'Error'
                Show-IDLGuiPage
            }
        })

    Register-IDLGuiHandlers
    if ($StartMode -eq 'Install'){
        $script:Flow = 'Install'
        $script:Step = 0
    }
    elseif ($StartMode -eq 'Uninstall'){
        $script:Flow = 'Uninstall'
        $script:Step = 0
    }

    Show-IDLGuiPage
}

#endregion

if ($DefineOnly){
    return
}

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$exitCode = 5
$consoleHidden = $false
try{
    $script:ScriptsDir = $PSScriptRoot
    $script:SourceDirArg = $SourceDir
    $script:ReleaseRoot = (($SourceDir) ? ([System.IO.Path]::GetFullPath($SourceDir).TrimEnd('\')) : (Split-Path -Parent $PSScriptRoot))
    $script:Elevated = Test-IDLProcessElevated

    if (
        (-not $script:Elevated) -and
        (-not $NoElevate)
    ){
        if (Start-IDLGuiElevated -StartMode $Mode){
            exit 0
        }
    }

    if (-not (Initialize-IDLGuiAssemblies)){
        [Console]::Error.WriteLine('Start-IDLifecycleSetup: the graphical setup needs WPF, which this Windows edition (e.g. Server Core) does not have. Use scripts\Install-IDLifecycle.ps1 / Uninstall-IDLifecycle.ps1 instead.')
        $exitCode = 3
        exit $exitCode
    }

    Initialize-IDLGuiWindow -StartMode $Mode

    if (-not $ShowConsole){
        Set-IDLGuiConsoleVisible -Visible $false
        $consoleHidden = $true
    }

    $null = $script:Window.ShowDialog()
    $exitCode = 0
}
catch{
    $exitCode = 5
    [Console]::Error.WriteLine("Start-IDLifecycleSetup: $($_.Exception.Message)")
    [Console]::Error.WriteLine($_.ScriptStackTrace)
}
finally{
    if ($consoleHidden){
        Set-IDLGuiConsoleVisible -Visible $true
    }
}

exit $exitCode
