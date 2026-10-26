$here = Split-Path -Parent $MyInvocation.MyCommand.Path
. (Join-Path $here 'TestHelpers.ps1')
. (Join-Path $here 'SetupTestHelpers.ps1')

# The setup window's helpers are dot-sourced with -DefineOnly (no window, nothing started). The command lines it
# builds are run against Install-/Uninstall-IDLifecycle.ps1 with SetupFakes.ps1 (temp folders only), exactly as
# the window starts them: pwsh -File <script> <arguments>, secrets only in the child's environment.

$script:GuiScript = Join-Path $script:ScriptsDir 'Start-IDLifecycleSetup.ps1'
. $script:GuiScript -DefineOnly

Describe 'Start-IDLifecycleSetup.ps1: input checks, command lines, log following (no window)' {

    $release = Join-Path $script:StateDir ('guirel_' + [guid]::NewGuid().ToString('N').Substring(0, 8))
    $null = New-Item -ItemType Directory -Force -Path $release
    Set-Content -LiteralPath (Join-Path $release 'SHA256SUMS') -Value 'x'
    Set-Content -LiteralPath (Join-Path $release 'idlifecycled.exe') -Value 'x'
    $adminPw = ConvertTo-SecureString -String 'Sentinel-Gui-Admin-4Hd' -AsPlainText -Force

    function Get-Settings {
        param([hashtable]$Change = @{})

        $s = @{
            SourceKind           = 'Release'
            SourceDir            = $release
            GitHubRepo           = 'mumbles27/IDLifecycle'
            GitHubRef            = 'main'
            GitHubToken          = $null
            GoVersion            = 'go1.27.3'
            InstallDir           = (Join-Path $script:StateDir 'gui\Program Files\IDLifecycle')
            DataDir              = (Join-Path $script:StateDir 'gui\ProgramData\IDLifecycle')
            ServiceName          = 'IDLifecycle'
            AccountType          = 'Managed'
            ServiceAccount       = 'CORP\svc-idl$'
            ServicePassword      = $null
            BuiltInAccount       = 'NT AUTHORITY\NetworkService'
            ServiceExists        = $false
            ExistingAccount      = ''
            ConfigExists         = $false
            DbDriver             = 'sqlite'
            DbDsn                = $null
            DsnInEnvVar          = $false
            DbDsnEnvVar          = 'IDL_DB_DSN'
            Listen               = ''
            TlsCertFile          = ''
            TlsKeyFile           = ''
            AdminUser            = 'admin'
            AdminPassword        = $adminPw
            AdminPasswordConfirm = $adminPw
            Start                = $true
            InstallPrerequisites = $false
            RequireSignature     = $false
            Force                = $false
            RestoreWanted        = $false
            RestoreFrom          = ''
            RestorePassphrase    = $null
            RestoreConfig        = $false
        }
        foreach ($k in $Change.Keys){
            $s[$k] = $Change[$k]
        }

        return $s
    }

    function Get-Problems {
        param([hashtable]$Change = @{}, [switch]$DryRun, [string]$Page = '')

        return @(Get-IDLGuiInstallProblems -Settings (Get-Settings -Change $Change) -DryRun:$DryRun | Where-Object { (-not $Page) -or ($_.Page -eq $Page) })
    }

    function Secure([string]$Text){
        return (ConvertTo-SecureString -String $Text -AsPlainText -Force)
    }

    It 'accepts the defaults (gMSA, SQLite, loopback) and reports each mistake on its own page' {
        @(Get-Problems).Count | Should Be 0

        (Get-Problems -Change @{ SourceDir = $script:StateDir } -Page 'Source')[0].Message | Should Match 'not an IDLifecycle release folder'
        (Get-Problems -Change @{ InstallDir = 'C:\' } -Page 'Locations')[0].Message | Should Match "program folder 'C:\\' cannot be used"
        (Get-Problems -Change @{ DataDir = (Join-Path $script:StateDir 'gui\Program Files\IDLifecycle\data') } -Page 'Locations')[0].Message | Should Match 'must be separate'
        (Get-Problems -Change @{ ServiceName = 'bad name!' } -Page 'Locations').Count | Should Be 1

        (Get-Problems -Change @{ ServiceAccount = 'CORP\svc-idl' } -Page 'Account')[0].Message | Should Match 'with the \$ at the end'
        (Get-Problems -Change @{ ServiceAccount = '' } -Page 'Account')[0].Message | Should Match 'Enter the gMSA'
        (Get-Problems -Change @{ AccountType = 'Password'; ServiceAccount = 'CORP\svc-idl$' } -Page 'Account')[0].Message | Should Match 'choose that option instead'
        (Get-Problems -Change @{ AccountType = '' } -Page 'Account').Count | Should Be 1
        @(Get-Problems -Change @{ AccountType = 'BuiltIn'; BuiltInAccount = 'Virtual' }).Count | Should Be 0

        (Get-Problems -Change @{ DbDriver = 'mssql' } -Page 'Database')[0].Message | Should Match 'Enter the connection string for Microsoft SQL Server'
        @(Get-Problems -Change @{ DbDriver = 'mssql'; DbDsn = (Secure 'sqlserver://h?database=d') }).Count | Should Be 0
        @(Get-Problems -Change @{ DbDriver = 'postgres'; DsnInEnvVar = $true }).Count | Should Be 0
        (Get-Problems -Change @{ DbDriver = 'mysql'; DsnInEnvVar = $true; DbDsnEnvVar = 'PATH' } -Page 'Database')[0].Message | Should Match 'must start with IDL_'
        (Get-Problems -Change @{ DbDriver = 'mysql'; DsnInEnvVar = $true; DbDsnEnvVar = 'IDL_SETUP_DB_DSN' } -Page 'Database')[0].Message | Should Match 'reserved'
        (Get-Problems -Change @{ DbDriver = 'oracle' } -Page 'Database').Count | Should Be 1

        (Get-Problems -Change @{ Listen = '0.0.0.0:8443' } -Page 'Web')[0].Message | Should Match 'needs HTTPS'
        (Get-Problems -Change @{ Listen = 'localhost' } -Page 'Web')[0].Message | Should Match 'must be host:port'
        @(Get-Problems -Change @{ Listen = 'localhost:9443' }).Count | Should Be 0
        (Get-Problems -Change @{ TlsCertFile = $release } -Page 'Web')[0].Message | Should Match 'both the certificate file and the key file'
        $cert = Join-Path $release 'SHA256SUMS'
        @(Get-Problems -Change @{ Listen = '0.0.0.0:8443'; TlsCertFile = $cert; TlsKeyFile = $cert }).Count | Should Be 0
        (Get-Problems -Change @{ Listen = '0.0.0.0:8443'; TlsCertFile = $cert; TlsKeyFile = (Join-Path $release 'nope.pem') } -Page 'Web')[0].Message | Should Match 'File not found'
        (Get-Problems -Change @{ AdminUser = 'bad user' } -Page 'Web').Count | Should Be 1
    }

    It 'source: GitHub needs a valid repo/ref and Go; a local zip or folder must exist and be a release' {
        @(Get-Problems -Change @{ SourceKind = 'GitHub'; SourceDir = '' }).Count | Should Be 0
        @(Get-Problems -Change @{ SourceKind = 'GitHub'; GitHubRepo = 'https://github.com/Org/Repo.git'; GitHubRef = 'v1.2.0' }).Count | Should Be 0
        (Get-Problems -Change @{ SourceKind = 'GitHub'; GitHubRepo = 'no slash' } -Page 'Source')[0].Message | Should Match 'owner/name'
        (Get-Problems -Change @{ SourceKind = 'GitHub'; GitHubRef = '../x' } -Page 'Source')[0].Message | Should Match 'branch, tag or commit'
        (Get-Problems -Change @{ SourceKind = 'GitHub'; GoVersion = '' } -Page 'Source')[0].Message | Should Match 'Go is not installed'
        (Get-Problems -Change @{ SourceKind = 'Local'; SourceDir = '' } -Page 'Source')[0].Message | Should Match 'Choose the release zip'
        (Get-Problems -Change @{ SourceKind = 'Local'; SourceDir = (Join-Path $release 'nope.zip') } -Page 'Source')[0].Message | Should Match 'File not found'
        (Get-Problems -Change @{ SourceKind = 'Local'; SourceDir = $script:StateDir } -Page 'Source')[0].Message | Should Match 'not an IDLifecycle release folder'
        @(Get-Problems -Change @{ SourceKind = 'Local'; SourceDir = $release }).Count | Should Be 0
        $zip = Join-Path $release 'r.zip'
        Set-Content -LiteralPath $zip -Value 'x'
        @(Get-Problems -Change @{ SourceKind = 'Local'; SourceDir = $zip }).Count | Should Be 0
        (Get-Problems -Change @{ SourceKind = '' } -Page 'Source').Count | Should Be 1

        $c = New-IDLGuiInstallCommand -Settings (Get-Settings -Change @{ SourceKind = 'GitHub'; GitHubRef = 'v1.2.0'; GitHubToken = (Secure 'Sentinel-Gh-Token') }) -ScriptsDir 'C:\R\scripts'
        $a = @($c.Arguments)
        $a -contains '-FromGitHub' | Should Be $true
        $a -contains '-SourceDir' | Should Be $false
        $a[[array]::IndexOf($a, '-GitHubRef') + 1] | Should Be 'v1.2.0'
        $a[[array]::IndexOf($a, '-GitHubRepo') + 1] | Should Be 'mumbles27/IDLifecycle'
        $c.Secrets['IDL_GITHUB_TOKEN'] -is [securestring] | Should Be $true
        ($a -join ' ') + $c.Display | Should Not Match 'Sentinel'
        ((Get-IDLGuiInstallSummary -Settings (Get-Settings -Change @{ SourceKind = 'GitHub' })) -join "`n") | Should Match 'GitHub mumbles27/IDLifecycle @ main, built here with go1\.27\.3'
        $l = New-IDLGuiInstallCommand -Settings (Get-Settings -Change @{ SourceKind = 'Local'; SourceDir = $zip }) -ScriptsDir 'C:\R\scripts'
        @($l.Arguments)[[array]::IndexOf(@($l.Arguments), '-SourceDir') + 1] | Should Be $zip
    }

    It 'restore: needs an existing backup file and (for a real run) its passphrase, which goes only into the child environment; no admin password needed' {
        $bak = Join-Path $release 'IDLifecycle_X_20261001_020000.idlbak'
        Set-Content -LiteralPath $bak -Value 'IDLBAK1'
        (Get-Problems -Change @{ RestoreWanted = $true } -Page 'Options')[0].Message | Should Match 'Choose the backup file'
        (Get-Problems -Change @{ RestoreWanted = $true; RestoreFrom = (Join-Path $release 'gone.idlbak') } -Page 'Options')[0].Message | Should Match 'not found'
        (Get-Problems -Change @{ RestoreWanted = $true; RestoreFrom = $bak } -Page 'Options')[0].Message | Should Match 'passphrase'
        @(Get-Problems -Change @{ RestoreWanted = $true; RestoreFrom = $bak } -DryRun).Count | Should Be 0
        $ok = @{ RestoreWanted = $true; RestoreFrom = $bak; RestorePassphrase = (Secure 'Sentinel-Restore-Pass'); RestoreConfig = $true; AdminPassword = $null; AdminPasswordConfirm = $null }
        @(Get-Problems -Change $ok).Count | Should Be 0

        $c = New-IDLGuiInstallCommand -Settings (Get-Settings -Change $ok) -ScriptsDir 'C:\R\scripts'
        $a = @($c.Arguments)
        $a[[array]::IndexOf($a, '-RestoreFrom') + 1] | Should Be $bak
        $a -contains '-RestoreConfig' | Should Be $true
        $c.Secrets['IDL_BACKUP_PASSPHRASE'] -is [securestring] | Should Be $true
        (($a -join ' ') + $c.Display) | Should Not Match 'Sentinel'
        ((Get-IDLGuiInstallSummary -Settings (Get-Settings -Change $ok)) -join "`n") | Should Match 'Restore: .* replaces the database contents and the settings'
    }

    It 'administrator and service passwords: required only where a real run needs them' {
        (Get-Problems -Change @{ AdminPassword = (Secure 'short'); AdminPasswordConfirm = (Secure 'short') } -Page 'Web')[0].Message | Should Match 'at least 12'
        (Get-Problems -Change @{ AdminPasswordConfirm = (Secure 'Something-else-123') } -Page 'Web')[0].Message | Should Match 'do not match'
        (Get-Problems -Change @{ AdminPassword = $null; AdminPasswordConfirm = $null } -Page 'Web')[0].Message | Should Match 'Enter a password for the first administrator'
        @(Get-Problems -Change @{ AdminPassword = $null; AdminPasswordConfirm = $null } -DryRun).Count | Should Be 0
        # An existing config.json keeps its users and its database.
        @(Get-Problems -Change @{ AdminPassword = $null; AdminPasswordConfirm = $null; ConfigExists = $true; DbDriver = 'mssql' }).Count | Should Be 0

        $user = @{ AccountType = 'Password'; ServiceAccount = 'CORP\svc-idl' }
        (Get-Problems -Change $user -Page 'Account')[0].Message | Should Match "Enter the password of 'CORP\\svc-idl'"
        @(Get-Problems -Change $user -DryRun).Count | Should Be 0
        $user['ServicePassword'] = Secure 'Svc-Pw-1'
        @(Get-Problems -Change $user).Count | Should Be 0

        # Existing service already on that account: the stored password is kept (none needed); another account needs one.
        $same = @{ AccountType = 'Password'; ServiceAccount = 'CORP\svc-idl'; ServiceExists = $true; ExistingAccount = 'corp\SVC-IDL' }
        Test-IDLGuiServicePasswordNeeded -Settings (Get-Settings -Change $same) | Should Be $false
        @(Get-Problems -Change $same).Count | Should Be 0
        $same['ExistingAccount'] = 'NT AUTHORITY\NetworkService'
        Test-IDLGuiServicePasswordNeeded -Settings (Get-Settings -Change $same) | Should Be $true
        # '.\name' is compared as COMPUTERNAME\name, like the installer does.
        $local = @{ AccountType = 'Password'; ServiceAccount = '.\svc-local'; ServiceExists = $true; ExistingAccount = "$env:COMPUTERNAME\svc-local" }
        Test-IDLGuiServicePasswordNeeded -Settings (Get-Settings -Change $local) | Should Be $false
        Test-IDLGuiServicePasswordNeeded -Settings (Get-Settings -Change @{ AccountType = 'Managed'; ServiceExists = $false }) | Should Be $false
    }

    It 'builds the install command: arguments for pwsh -File, secrets only as SecureStrings for the child environment' {
        $s = Get-Settings -Change @{
            AccountType     = 'Password'
            ServiceAccount  = '.\svc-local'
            ServicePassword = (Secure 'Sentinel-Svc-Pw-2Kq')
            DbDriver        = 'mssql'
            DbDsn           = (Secure 'sqlserver://u:Sentinel-Dsn-8Wp@h?database=d')
            Listen          = '0.0.0.0:8443'
            TlsCertFile     = 'C:\certs\idl.pem'
            TlsKeyFile      = 'C:\certs\idl.key'
            Start           = $false
            Force           = $true
        }
        $c = New-IDLGuiInstallCommand -Settings $s -ScriptsDir 'C:\Rel\scripts'
        $c.Script | Should Be 'C:\Rel\scripts\Install-IDLifecycle.ps1'
        $c.ScriptName | Should Be 'Install-IDLifecycle'
        $a = @($c.Arguments)
        $a[[array]::IndexOf($a, '-ServiceAccount') + 1] | Should Be "$env:COMPUTERNAME\svc-local"
        $a[[array]::IndexOf($a, '-DbDriver') + 1] | Should Be 'mssql'
        $a[[array]::IndexOf($a, '-Listen') + 1] | Should Be '0.0.0.0:8443'
        $a -contains '-Start:$false' | Should Be $true
        $a -contains '-Force' | Should Be $true
        $a -contains '-NonInteractive' | Should Be $true
        $a -contains '-DryRun' | Should Be $false
        $a -contains '-DbDsnEnvVar' | Should Be $false
        ($a -join ' ') | Should Not Match 'Sentinel|Password'
        @($c.Secrets.Keys | Sort-Object) -join ',' | Should Be 'IDL_PASSWORD,IDL_SERVICE_PASSWORD,IDL_SETUP_DB_DSN'
        $c.Secrets['IDL_SERVICE_PASSWORD'] -is [securestring] | Should Be $true
        $c.Display | Should Not Match 'Sentinel'
        $c.Display | Should Match "-DbDsn \(Read-Host -AsSecureString 'Connection string'\)"
        $c.Display | Should Match '-ServiceAccountPassword \(Read-Host -AsSecureString'

        $d = New-IDLGuiInstallCommand -Settings (Get-Settings -Change @{ AccountType = 'BuiltIn'; BuiltInAccount = 'Virtual'; ServiceName = 'IDL2'; ConfigExists = $true; DbDriver = 'mssql'; AdminPassword = $null }) -ScriptsDir 'C:\Rel\scripts' -DryRun
        $b = @($d.Arguments)
        $b[[array]::IndexOf($b, '-ServiceAccount') + 1] | Should Be 'NT SERVICE\IDL2'
        $b -contains '-DryRun' | Should Be $true
        $b -contains '-DbDriver' | Should Be $false
        $b -contains '-Start:$false' | Should Be $false
        $d.Secrets.Count | Should Be 0
        # Values with quotes are quoted correctly in the copyable command.
        (New-IDLGuiInstallCommand -Settings (Get-Settings -Change @{ InstallDir = "C:\It's here" }) -ScriptsDir 'C:\R').Display | Should Match "-InstallDir 'C:\\It''s here'"
    }

    It 'the install command runs Install-IDLifecycle.ps1 as built (fakes): DSN, service and admin passwords reach it only through the environment' {
        $fx = New-SetupFixture
        $s = Get-Settings -Change @{
            SourceDir       = $fx.Source
            InstallDir      = $fx.Install
            DataDir         = $fx.Data
            ServiceName     = 'IDLGuiTest'
            AccountType     = 'Password'
            ServiceAccount  = 'TESTDOM\svc-idl'
            ServicePassword = (Secure 'Sentinel-Svc-Pw-2Kq')
            DbDriver        = 'mssql'
            DbDsn           = (Secure 'sqlserver://u:Sentinel-Dsn-8Wp@h?database=d')
            Start           = $false
        }
        $c = New-IDLGuiInstallCommand -Settings $s -ScriptsDir $script:ScriptsDir
        $childEnv = @{ IDL_FAKE_STATE = $fx.State; IDL_SETUP_TEST = '1'; IDL_SETUP_TEST_LOGDIR = (Join-Path $fx.Root 'logs') }
        foreach ($k in $c.Secrets.Keys){
            $childEnv[$k] = [System.Net.NetworkCredential]::new('', $c.Secrets[$k]).Password
        }

        $argList = @('-NoProfile', '-NonInteractive', '-ExecutionPolicy', 'Bypass', '-File', $c.Script) + @($c.Arguments) + @('-TestHook', $script:SetupHook)
        $r = Invoke-ChildProcess -Psi (New-ChildProcessInfo -Arguments $argList -StatePath (New-StubStateFile) -ExtraEnv $childEnv) -TimeoutSec 300
        $log = (Get-ChildItem -LiteralPath (Join-Path $fx.Root 'logs') -Filter 'Install-IDLifecycle_2*.log' | Select-Object -First 1).FullName
        $logText = [System.IO.File]::ReadAllText($log)

        $r.ExitCode | Should Be 0
        ($logText + $r.StdOutRaw + $r.StdErr) | Should Not Match 'Sentinel'
        $cfg = [System.IO.File]::ReadAllText($fx.Config) | ConvertFrom-Json -AsHashtable
        $cfg['database']['dsn'] | Should Be 'sqlserver://u:Sentinel-Dsn-8Wp@h?database=d'
        $installer = @(Get-FakeCalls -Path $fx.State -Op 'serviceInstaller')[0]['data']
        $installer['servicePasswordLength'] | Should Be 'Sentinel-Svc-Pw-2Kq'.Length
        @(Get-FakeCalls -Path $fx.State -Op 'testCredential')[0]['data']['passwordLength'] | Should Be 'Sentinel-Svc-Pw-2Kq'.Length
        @(Get-FakeOps -Path $fx.State) -contains 'daemon:user add' | Should Be $true
        # -Start:$false arrived as a switch value: nothing was started.
        @(Get-FakeOps -Path $fx.State) -contains 'start' | Should Be $false
        (ConvertTo-Json -InputObject @((Read-FakeSetupState -Path $fx.State)['calls']) -Depth 10) | Should Not Match 'Sentinel'
        Remove-SetupFixture -Fixture $fx
    }

    It 'uninstall: checks (backup folder, typed confirmation) and the command; -RemoveData is sent with -Force only after the confirmation' {
        $u = @{ ServiceName = 'IDLifecycle'; InstallDir = ''; DataDir = ''; ServiceAccount = ''; RemoveProgramFiles = $true; RemoveData = $false; BackupMode = 'Backup'; BackupTo = ''; RemoveDbDsnEnvVar = $false; Confirmation = '' }
        @(Get-IDLGuiUninstallProblems -Settings $u).Count | Should Be 0
        $c = New-IDLGuiUninstallCommand -Settings $u -ScriptsDir 'C:\R\scripts'
        ($c.Arguments -join ' ') | Should Be '-ServiceName IDLifecycle -NonInteractive'

        $u.RemoveData = $true
        $p = @(Get-IDLGuiUninstallProblems -Settings $u)
        ($p.Message -join ' ') | Should Match 'Choose a folder for the backup'
        ($p.Message -join ' ') | Should Match "type the service name 'IDLifecycle'"
        @(Get-IDLGuiUninstallProblems -Settings $u -DryRun).Message -join ' ' | Should Not Match 'type the service name'
        $u.BackupTo = 'C:\'
        (@(Get-IDLGuiUninstallProblems -Settings $u -DryRun).Message -join ' ') | Should Match "backup folder 'C:\\' cannot be used"
        $u.BackupTo = 'D:\Backup'
        $u.Confirmation = 'idlifecycle'
        (@(Get-IDLGuiUninstallProblems -Settings $u).Message -join ' ') | Should Match 'same case'
        $u.Confirmation = ' IDLifecycle '
        @(Get-IDLGuiUninstallProblems -Settings $u).Count | Should Be 0
        $u.RemoveProgramFiles = $false
        $u.RemoveDbDsnEnvVar = $true
        ((New-IDLGuiUninstallCommand -Settings $u -ScriptsDir 'C:\R\scripts' -DryRun).Arguments -join ' ') | Should Be '-ServiceName IDLifecycle -RemoveProgramFiles:$false -RemoveData -BackupTo D:\Backup -Force -RemoveDbDsnEnvVar -DryRun -NonInteractive'
        $u.BackupMode = 'None'
        ((New-IDLGuiUninstallCommand -Settings $u -ScriptsDir 'C:\R\scripts').Arguments -join ' ') | Should Match '-RemoveData -NoBackup -Force'
        $u.BackupMode = ''
        (@(Get-IDLGuiUninstallProblems -Settings $u -DryRun).Message -join ' ') | Should Match 'back up the data folder first'
        $u.ServiceName = 'x y'
        @(Get-IDLGuiUninstallProblems -Settings $u -DryRun).Count | Should BeGreaterThan 0
        (Get-IDLGuiUninstallSummary -Settings @{ ServiceName = 'S'; InstallDir = ''; DataDir = 'D:\Data'; RemoveProgramFiles = $true; RemoveData = $true; BackupMode = 'None'; BackupTo = ''; RemoveDbDsnEnvVar = $false }) -join "`n" | Should Match 'D:\\Data - PERMANENTLY DELETED \(NO backup\)'
    }

    It 'the uninstall command runs Uninstall-IDLifecycle.ps1 as built (fakes): RemoveProgramFiles:$false keeps the program folder' {
        $fx = New-SetupFixture
        $null = New-Item -ItemType Directory -Force -Path (Join-Path $fx.Install 'scripts'), $fx.Data
        [System.IO.File]::WriteAllText((Join-Path $fx.Install 'idlifecycled.exe'), 'FAKE')
        $cfg = [ordered]@{ data_dir = $fx.Data; database = @{ driver = 'sqlite' } }
        [System.IO.File]::WriteAllText($fx.Config, (ConvertTo-Json -InputObject $cfg -Depth 5))
        $exe = Join-Path $fx.Install 'idlifecycled.exe'
        Update-FakeSetupState -Path $fx.State -Change {
            param($st)
            $st['services'] = @{ IDLGuiUn = @{ State = 'Running'; PathName = ('"{0}" service -config "{1}"' -f $exe, $fx.Config); StartName = 'NT AUTHORITY\NetworkService' } }
            $st['eventSources'] = @('IDLifecycle')
        }

        $u = @{ ServiceName = 'IDLGuiUn'; InstallDir = $fx.Install; DataDir = ''; ServiceAccount = ''; RemoveProgramFiles = $false; RemoveData = $false; BackupMode = 'Backup'; BackupTo = ''; RemoveDbDsnEnvVar = $false; Confirmation = '' }
        $c = New-IDLGuiUninstallCommand -Settings $u -ScriptsDir $script:ScriptsDir
        $childEnv = @{ IDL_FAKE_STATE = $fx.State; IDL_SETUP_TEST = '1'; IDL_SETUP_TEST_LOGDIR = (Join-Path $fx.Root 'logs') }
        $argList = @('-NoProfile', '-NonInteractive', '-ExecutionPolicy', 'Bypass', '-File', $c.Script) + @($c.Arguments) + @('-TestHook', $script:SetupHook)
        $r = Invoke-ChildProcess -Psi (New-ChildProcessInfo -Arguments $argList -StatePath (New-StubStateFile) -ExtraEnv $childEnv) -TimeoutSec 300
        $r.ExitCode | Should Be 0
        (@(Get-FakeOps -Path $fx.State) -join ',') | Should Be 'stop,delete,removeEventSource'
        (Test-Path -LiteralPath $exe) | Should Be $true
        (Test-Path -LiteralPath $fx.Config) | Should Be $true
        Remove-SetupFixture -Fixture $fx
    }

    It 'explains exit codes and colours log lines' {
        Get-IDLGuiExitText -Code 0 -DryRun | Should Match 'Nothing was changed'
        Get-IDLGuiExitText -Code 0 | Should Be 'Installation finished successfully.'
        Get-IDLGuiExitText -Code 0 -Uninstall | Should Be 'Uninstall finished successfully.'
        foreach ($code in 1..5){
            Get-IDLGuiExitText -Code $code | Should Not BeNullOrEmpty
        }

        Get-IDLGuiExitText -Code 7 | Should Match 'exit code 7'

        (Format-IDLGuiLogLine -Line '2026-01-01 10:11:12 [INFO] WOULD: CreateService x').Kind | Should Be 'Would'
        (Format-IDLGuiLogLine -Line '2026-01-01 10:11:12 [INFO] WOULD: CreateService x').Text | Should Be '10:11:12  WOULD: CreateService x'
        (Format-IDLGuiLogLine -Line '2026-01-01 10:11:12 [INFO]   DONE: x').Kind | Should Be 'Done'
        (Format-IDLGuiLogLine -Line '2026-01-01 10:11:12 [INFO] SKIP: x (already)').Kind | Should Be 'Skip'
        (Format-IDLGuiLogLine -Line '2026-01-01 10:11:12 [INFO] FAILED: x').Kind | Should Be 'Error'
        (Format-IDLGuiLogLine -Line '2026-01-01 10:11:12 [ERROR] FATAL: x').Kind | Should Be 'Error'
        (Format-IDLGuiLogLine -Line '2026-01-01 10:11:12 [WARN] careful').Kind | Should Be 'Warn'
        (Format-IDLGuiLogLine -Line '2026-01-01 10:11:12 [DEBUG] Stack').Kind | Should Be 'Debug'
        (Format-IDLGuiLogLine -Line 'plain text').Text | Should Be 'plain text'
        Test-IDLGuiLogLine -Line '2026-01-01 10:11:12 [WARN] x' | Should Be $true
        Test-IDLGuiLogLine -Line 'Install-IDLifecycle: cannot use the log folder' | Should Be $false

        Get-IDLGuiUrlFromLog -Lines @('2026-01-01 10:11:12 [INFO] ---- Next steps ----', '2026-01-01 10:11:12 [INFO] GUI: https://idl.corp:8443/ ') | Should Be 'https://idl.corp:8443/'
        Get-IDLGuiUrlFromLog -Lines @('nothing here') | Should Be ''
    }

    It 'follows a log file that is still being written (partial lines, UTF-8 split between reads) and finds the new log' {
        $dir = Join-Path $script:StateDir ('guilog_' + [guid]::NewGuid().ToString('N').Substring(0, 8))
        $null = New-Item -ItemType Directory -Force -Path $dir
        $old = Join-Path $dir 'Install-IDLifecycle_20260101_000000.log'
        Set-Content -LiteralPath $old -Value 'old'
        $before = Get-IDLGuiLogSnapshot -Dirs @($dir, (Join-Path $dir 'missing')) -ScriptName 'Install-IDLifecycle'
        @($before).Count | Should Be 1
        Find-IDLGuiNewLog -Dirs @($dir) -ScriptName 'Install-IDLifecycle' -Before $before | Should Be ''

        $path = Join-Path $dir 'Install-IDLifecycle_20260101_000001.log'
        Set-Content -LiteralPath (Join-Path $dir 'Install-IDLifecycle_Report_20260101_000001.csv') -Value 'r'
        $writer = [System.IO.FileStream]::new($path, [System.IO.FileMode]::Create, [System.IO.FileAccess]::Write, [System.IO.FileShare]::ReadWrite)
        try{
            Find-IDLGuiNewLog -Dirs @($dir) -ScriptName 'Install-IDLifecycle' -Before $before | Should Be $path
            $tail = New-IDLGuiLogTail -Path $path
            $bytes = [System.Text.UTF8Encoding]::new($false).GetBytes("first line`nsecond with ü")
            # Write up to the middle of the two-byte 'ü' and an unfinished line.
            $writer.Write($bytes, 0, $bytes.Length - 1)
            $writer.Flush()
            $lines = @(Read-IDLGuiLogTail -Tail $tail)
            ($lines -join '|') | Should Be 'first line'
            $writer.Write($bytes, $bytes.Length - 1, 1)
            $more = [System.Text.UTF8Encoding]::new($false).GetBytes(" end`r`nthird")
            $writer.Write($more, 0, $more.Length)
            $writer.Flush()
            ($(Read-IDLGuiLogTail -Tail $tail) -join '|') | Should Be 'second with ü end'
            @(Read-IDLGuiLogTail -Tail $tail).Count | Should Be 0
            ($(Read-IDLGuiLogTail -Tail $tail -Final) -join '|') | Should Be 'third'
        }
        finally{
            $writer.Dispose()
        }

        Remove-Item -LiteralPath $dir -Recurse -Force -ErrorAction SilentlyContinue
    }

    It 'the window definition loads, every control the code uses exists, and every page of the flows is defined' {
        $names = @(Get-IDLGuiControlNames -Xaml $script:Xaml)
        $names.Count | Should BeGreaterThan 50
        @($names | Group-Object | Where-Object { $_.Count -gt 1 }).Count | Should Be 0
        $text = [System.IO.File]::ReadAllText($script:GuiScript)
        $used = @([regex]::Matches($text, '\$(?:script:Ui|ui)\.(\w+)') | ForEach-Object { $_.Groups[1].Value } | Sort-Object -Unique)
        @($used | Where-Object { $names -notcontains $_ }) -join ',' | Should Be ''
        $pages = @([regex]::Matches($text, "@\{ Page = '(Page\w+)'") | ForEach-Object { $_.Groups[1].Value } | Sort-Object -Unique)
        $pages.Count | Should Be 10
        @($pages | Where-Object { $names -notcontains $_ }) -join ',' | Should Be ''

        if ([System.Threading.Thread]::CurrentThread.GetApartmentState() -eq 'STA'){
            Initialize-IDLGuiAssemblies | Should Be $true
            $window = [System.Windows.Markup.XamlReader]::Load([System.Xml.XmlReader]::Create([System.IO.StringReader]::new($script:Xaml)))
            @($names | Where-Object { $null -eq $window.FindName($_) }) -join ',' | Should Be ''
        }
    }

    It 'Setup.cmd starts the window from scripts\ with PowerShell 7 (CRLF line endings, as cmd.exe needs for labels)' {
        $cmd = Join-Path $script:RepoRoot 'Setup.cmd'
        $bytes = [System.IO.File]::ReadAllBytes($cmd)
        $text = [System.Text.Encoding]::ASCII.GetString($bytes)
        $text | Should Match 'scripts\\Start-IDLifecycleSetup\.ps1'
        $text | Should Match 'pwsh\.exe'
        $text | Should Match '-ExecutionPolicy Bypass -STA -File'
        ([regex]::Matches($text, "(?<!`r)`n")).Count | Should Be 0
    }

    Remove-Item -LiteralPath $release -Recurse -Force -ErrorAction SilentlyContinue
}
