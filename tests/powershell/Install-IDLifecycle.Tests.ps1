$here = Split-Path -Parent $MyInvocation.MyCommand.Path
. (Join-Path $here 'TestHelpers.ps1')
. (Join-Path $here 'SetupTestHelpers.ps1')

# Every run uses SetupFakes.ps1 (no real service / env var / event source / AD / DB) and temp folders only.

Describe 'Install-IDLifecycle.ps1 (fakes for every system change; temp folders only)' {

    $adminPw = 'Sentinel-Admin-Pw-7Q2x'
    $dsn = 'sqlserver://idl:Sentinel-Dsn-Pw-9Zk@sql01?database=idl'

    function Get-BaseParams {
        param($Fx, [hashtable]$Extra = @{})

        $p = @{
            SourceDir      = $Fx.Source
            InstallDir     = $Fx.Install
            DataDir        = $Fx.Data
            ServiceAccount = 'TESTDOM\svc-idl$'
            ServiceName    = 'IDLSetupTest'
        }
        foreach ($k in $Extra.Keys){
            $p[$k] = $Extra[$k]
        }

        return $p
    }

    It 'DryRun reports WOULD: for every step and changes nothing' {
        $fx = New-SetupFixture
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx -Extra @{ DryRun = $true }) -StatePath $fx.State

        $r.ExitCode | Should Be 0
        $r.LogText | Should Match 'Mode: DRY RUN'
        foreach ($step in @('CreateDirectory', 'CopyProgramFiles', 'UnblockProgramFiles', 'LockdownInstallDir', 'InitConfig', 'LockdownDataDir', 'RegisterEventSource', 'CreateAdminUser', 'RegisterService', 'Check', 'StartService', 'ProbeHttp')){
            $r.LogText | Should Match ('WOULD: ' + $step + ' ')
            $r.ReportText | Should Match ('"' + $step + '","Would"')
        }

        $r.ReportText | Should Not Match '"Applied"'
        $r.LogText | Should Match 'Program files: install 1\.0\.0'
        (Test-Path -LiteralPath $fx.Install) | Should Be $false
        (Test-Path -LiteralPath $fx.Data) | Should Be $false
        # Only read-only calls: the gMSA test and `version` of the source exe.
        ((Get-FakeOps -Path $fx.State) -join ',') | Should Be 'testGmsa,daemon:version'
        @((Read-FakeSetupState -Path $fx.State)['services'].Keys).Count | Should Be 0
        Remove-SetupFixture -Fixture $fx
    }

    It 'rejects invalid input with exit code 2 before touching anything' {
        $fx = New-SetupFixture
        $cases = @(
            @{ Listen = '0.0.0.0:8443' },
            @{ Listen = '10.1.2.3:443' },
            @{ Listen = 'not-a-listen' },
            @{ ServiceAccount = '' },
            @{ ServiceAccount = 'TESTDOM\jdoe' },
            @{ ServiceAccount = 'not an account' },
            @{ ServiceAccount = 'TESTDOM\a\b' },
            @{ DbDriver = 'oracle' },
            @{ DbDriver = 'mssql' },
            @{ ServiceName = 'bad name!' },
            @{ TlsCertFile = (Join-Path $fx.Root 'missing.pem') }
        )
        foreach ($case in $cases){
            $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx -Extra $case) -StatePath $fx.State -AdminPassword $adminPw
            $r.ExitCode | Should Be 2
            $r.LogText | Should Match 'FATAL'
        }

        # Short password; and a fresh live install without any password (no admin could be created).
        (Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx) -StatePath $fx.State -AdminPassword 'short').ExitCode | Should Be 2
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx) -StatePath $fx.State
        $r.ExitCode | Should Be 2
        $r.LogText | Should Match 'No administrator can exist yet'

        @(Get-FakeOps -Path $fx.State).Count | Should Be 0
        (Test-Path -LiteralPath $fx.Install) | Should Be $false
        (Test-Path -LiteralPath $fx.Data) | Should Be $false
        Remove-SetupFixture -Fixture $fx
    }

    It 'user service account: password checked first, handed only to the service installer, never logged; kept on re-run' {
        $svcPw = 'Sentinel-Svc-Pw-3Lm6'
        $fx = New-SetupFixture
        $user = Get-BaseParams -Fx $fx -Extra @{ ServiceAccount = 'TESTDOM\svc-user' }

        # Without a password a fresh install is refused before anything happens.
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params $user -StatePath $fx.State -AdminPassword $adminPw
        $r.ExitCode | Should Be 2
        $r.LogText | Should Match 'is a user account: pass -ServiceAccountPassword'
        @(Get-FakeOps -Path $fx.State).Count | Should Be 0

        # Wrong password: exit 3, nothing changed.
        Update-FakeSetupState -Path $fx.State -Change { param($s) $s['credentialOk'] = $false }
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params $user -StatePath $fx.State -AdminPassword $adminPw -ServicePassword $svcPw
        $r.ExitCode | Should Be 3
        $r.LogText | Should Match 'was rejected'
        $r.AllText | Should Not Match ([regex]::Escape($svcPw))
        (Get-FakeOps -Path $fx.State) -contains 'daemon:init' | Should Be $false
        (Test-Path -LiteralPath $fx.Install) | Should Be $false

        # Right password: installed; the password reached the service installer through its environment only.
        Update-FakeSetupState -Path $fx.State -Change { param($s) $s['credentialOk'] = $true; $s['calls'] = @() }
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params $user -StatePath $fx.State -AdminPassword $adminPw -ServicePassword $svcPw
        $r.ExitCode | Should Be 0
        $r.AllText | Should Not Match ([regex]::Escape($svcPw))
        $r.LogText | Should Match "password verified"
        $r.LogText | Should Match 'is a user account with a password'
        $calls = @(Get-FakeCalls -Path $fx.State)
        (ConvertTo-Json -InputObject $calls -Depth 10) | Should Not Match ([regex]::Escape($svcPw))
        $cred = @($calls | Where-Object { $_['op'] -eq 'testCredential' })
        $cred.Count | Should Be 1
        $cred[0]['data']['account'] | Should Be 'TESTDOM\svc-user'
        $cred[0]['data']['passwordLength'] | Should Be $svcPw.Length
        $installer = @($calls | Where-Object { $_['op'] -eq 'serviceInstaller' })[0]
        @($installer['data']['envNames']) -join ',' | Should Be 'IDL_SERVICE_PASSWORD'
        $installer['data']['servicePasswordLength'] | Should Be $svcPw.Length
        foreach ($c in @($calls | Where-Object { $_['op'] -like 'daemon:*' })){
            @($c['data']['idlEnv']) -contains 'IDL_SERVICE_PASSWORD' | Should Be $false
        }

        # Re-run without the password: the service already runs as this account, so the stored password is kept.
        Reset-FakeCalls -Path $fx.State
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params $user -StatePath $fx.State -AdminPassword $adminPw
        $r.ExitCode | Should Be 0
        $installer = @(Get-FakeCalls -Path $fx.State -Op 'serviceInstaller')[0]
        @($installer['data']['envNames']).Count | Should Be 0
        @(Get-FakeCalls -Path $fx.State -Op 'testCredential').Count | Should Be 0
        Remove-SetupFixture -Fixture $fx
    }

    It 'user service account: an unreachable DC only warns; a password for a gMSA is refused' {
        $svcPw = 'Sentinel-Svc-Pw-3Lm6'
        $fx = New-SetupFixture
        Update-FakeSetupState -Path $fx.State -Change { param($s) $s['credentialOk'] = 'throw' }
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx -Extra @{ ServiceAccount = '.\svc-local'; DryRun = $true }) -StatePath $fx.State -ServicePassword $svcPw
        $r.ExitCode | Should Be 0
        $r.LogText | Should Match 'Could not verify the password'
        $r.LogText | Should Match ([regex]::Escape("$($env:COMPUTERNAME)\svc-local"))

        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx -Extra @{ DryRun = $true }) -StatePath $fx.State -ServicePassword $svcPw
        $r.ExitCode | Should Be 2
        $r.LogText | Should Match 'only for a user account'
        $r.AllText | Should Not Match ([regex]::Escape($svcPw))
        Remove-SetupFixture -Fixture $fx
    }

    It 'non-loopback listen without TLS names the reason' {
        $fx = New-SetupFixture
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx -Extra @{ Listen = '0.0.0.0:8443'; DryRun = $true }) -StatePath $fx.State
        $r.ExitCode | Should Be 2
        $r.LogText | Should Match 'not a loopback address: -TlsCertFile and -TlsKeyFile are required'
        Remove-SetupFixture -Fixture $fx
    }

    It 'preflight dependency errors exit 3: db2 without an IBM ODBC driver, unusable gMSA, missing AD module' {
        $fx = New-SetupFixture
        Update-FakeSetupState -Path $fx.State -Change { param($s) $s['machineEnv']['IDL_T_DB2'] = 'DATABASE=idl;HOSTNAME=db2host;PWD=x' }
        $db2 = Get-BaseParams -Fx $fx -Extra @{ DbDriver = 'db2'; DbDsnEnvVar = 'IDL_T_DB2'; DryRun = $true }

        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params $db2 -StatePath $fx.State
        $r.ExitCode | Should Be 3
        $r.LogText | Should Match 'IBM Data Server Driver for ODBC and CLI'

        Update-FakeSetupState -Path $fx.State -Change { param($s) $s['odbcDrivers'] = @('SQL Server', 'IBM DB2 ODBC DRIVER - DB2COPY1') }
        (Invoke-SetupScript -Script 'Install-IDLifecycle' -Params $db2 -StatePath $fx.State).ExitCode | Should Be 0

        Update-FakeSetupState -Path $fx.State -Change { param($s) $s['gmsaOk'] = $false }
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx -Extra @{ DryRun = $true }) -StatePath $fx.State
        $r.ExitCode | Should Be 3
        $r.LogText | Should Match 'Test-ADServiceAccount returned false'
        $r.LogText | Should Match 'PrincipalsAllowedToRetrieveManagedPassword'

        Update-FakeSetupState -Path $fx.State -Change { param($s) $s['gmsaOk'] = $true; $s['adModule'] = $false }
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx -Extra @{ DryRun = $true }) -StatePath $fx.State
        $r.ExitCode | Should Be 3
        $r.LogText | Should Match 'Install-WindowsFeature RSAT-AD-PowerShell'
        @(Get-FakeOps -Path $fx.State | Where-Object { $_ -eq 'installFeature' }).Count | Should Be 0

        Remove-SetupFixture -Fixture $fx
    }

    It 'live install: every step applied, admin password never in log/report/stdout/stderr or on a command line' {
        $fx = New-SetupFixture
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx) -StatePath $fx.State -AdminPassword $adminPw

        $r.ExitCode | Should Be 0
        $r.AllText | Should Not Match ([regex]::Escape($adminPw))
        foreach ($step in @('CreateDirectory', 'CopyProgramFiles', 'UnblockProgramFiles', 'LockdownInstallDir', 'InitConfig', 'LockdownDataDir', 'RegisterEventSource', 'CreateAdminUser', 'RegisterService', 'StartService')){
            $r.ReportText | Should Match ('"' + $step + '","Applied"')
        }

        $r.ReportText | Should Match '"Check","Verified"'
        $r.ReportText | Should Match '"ProbeHttp","Verified"'
        $r.LogText | Should Match 'GUI: http://127\.0\.0\.1:8443/'
        $r.LogText | Should Match 'Settings -> AD'

        (Test-Path -LiteralPath (Join-Path $fx.Install 'idlifecycled.exe')) | Should Be $true
        (Test-Path -LiteralPath (Join-Path $fx.Install 'scripts\Install-IDLifecycleService.ps1')) | Should Be $true
        (Test-Path -LiteralPath $fx.Config) | Should Be $true
        # Copied files are unblocked; the source copy keeps its Mark-of-the-Web (the installer never touches the source).
        $installed = Join-Path $fx.Install 'scripts\IDLifecycle.Common.psm1'
        $null -eq (Get-Item -LiteralPath $installed -Stream Zone.Identifier -ErrorAction SilentlyContinue) | Should Be $true
        $src = Join-Path $fx.Source 'scripts\IDLifecycle.Common.psm1'
        $srcBlocked = ($null -ne (Get-Item -LiteralPath (Join-Path $script:ScriptsDir 'IDLifecycle.Common.psm1') -Stream Zone.Identifier -ErrorAction SilentlyContinue))
        ($null -ne (Get-Item -LiteralPath $src -Stream Zone.Identifier -ErrorAction SilentlyContinue)) | Should Be $srcBlocked

        # IDL_PASSWORD reached exactly one child (user add) and no other process; nothing carried it on argv.
        $calls = @(Get-FakeCalls -Path $fx.State)
        $userAdd = @($calls | Where-Object { $_['op'] -eq 'daemon:user add' })
        $userAdd.Count | Should Be 1
        $userAdd[0]['data']['hasPassword'] | Should Be $true
        $userAdd[0]['data']['passwordLength'] | Should Be $adminPw.Length
        foreach ($c in @($calls | Where-Object { ($_['op'] -like 'daemon:*') -and ($_['op'] -ne 'daemon:user add') })){
            $c['data']['hasPassword'] | Should Be $false
        }

        (ConvertTo-Json -InputObject $calls -Depth 10) | Should Not Match ([regex]::Escape($adminPw))

        $state = Read-FakeSetupState -Path $fx.State
        $state['services']['IDLSetupTest']['State'] | Should Be 'Running'
        $state['eventSources'] -contains 'IDLifecycle' | Should Be $true
        $installer = @($calls | Where-Object { $_['op'] -eq 'serviceInstaller' })[0]
        $installer['data']['script'] | Should Be (Join-Path $fx.Install 'scripts\Install-IDLifecycleService.ps1')
        ($installer['data']['args'] -join ' ') | Should Be ('-BinaryPath {0} -ConfigPath {1} -ServiceAccount TESTDOM\svc-idl$ -ServiceName IDLSetupTest' -f (Join-Path $fx.Install 'idlifecycled.exe'), $fx.Config)
        $probe = @($calls | Where-Object { $_['op'] -eq 'probe' })[0]
        $probe['data']['url'] | Should Be 'http://127.0.0.1:8443/'
        $probe['data']['skipCert'] | Should Be $false

        # ---- idempotent re-run: everything already in place is reported as skipped ----
        Reset-FakeCalls -Path $fx.State
        $r2 = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx) -StatePath $fx.State -AdminPassword $adminPw
        $r2.ExitCode | Should Be 0
        $r2.ReportText | Should Not Match '"Applied"'
        foreach ($step in @('CreateDirectory', 'CopyProgramFiles', 'UnblockProgramFiles', 'LockdownInstallDir', 'InitConfig', 'LockdownDataDir', 'RegisterEventSource', 'CreateAdminUser', 'RegisterService', 'StartService')){
            $r2.ReportText | Should Match ('"' + $step + '","Skipped"')
        }

        $r2.LogText | Should Match 'kept existing config\.json \(never overwritten\)'
        $r2.LogText | Should Match 'same version 1\.0\.0, all files identical'
        $r2.AllText | Should Not Match ([regex]::Escape($adminPw))
        $ops = @(Get-FakeOps -Path $fx.State)
        foreach ($op in @('daemon:init', 'daemon:lockdown', 'daemon:user add', 'stop', 'start', 'newEventSource')){
            $ops -contains $op | Should Be $false
        }

        Remove-SetupFixture -Fixture $fx
    }

    It 'tray: tray.json for the tray, Start menu + Startup shortcuts with the service''s execution policy, a protected branding folder; -NoTray removes the tray shortcuts' {
        $fx = New-SetupFixture
        Set-Content -LiteralPath (Join-Path $fx.Source 'Setup.cmd') -Value '@echo launcher'
        Write-ReleaseManifest -Root $fx.Source
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx) -StatePath $fx.State -AdminPassword $adminPw
        $r.ExitCode | Should Be 0

        Import-Module (Join-Path $script:ScriptsDir 'IDLifecycle.Common.psm1') -Force
        $tray = Read-IDLTrayInfo -Path (Join-Path $fx.Install 'tray.json')
        $tray['service_name'] | Should Be 'IDLSetupTest'
        $tray['console_url'] | Should Be 'http://127.0.0.1:8443/'
        $tray['tls'] | Should Be $false
        $tray['config_path'] | Should Be $fx.Config
        $tray['install_dir'] | Should Be $fx.Install
        $tray['version'] | Should Be '1.0.0'
        $tray['execution_policy'] | Should Be 'RemoteSigned'
        [System.IO.File]::ReadAllText((Join-Path $fx.Install 'tray.json')) | Should Not Match ([regex]::Escape($adminPw))

        $desktop = Join-Path (Split-Path -Parent $fx.State) 'desktop'
        $shortcuts = (Read-FakeSetupState -Path $fx.State)['shortcuts']
        @($shortcuts.Keys | Sort-Object) -join '|' | Should Be ((@(
                    (Join-Path $desktop 'Programs\IDLifecycle\IDLifecycle Setup.lnk'),
                    (Join-Path $desktop 'Programs\IDLifecycle\IDLifecycle Tray.lnk'),
                    (Join-Path $desktop 'Startup\IDLifecycle Tray.lnk')) | Sort-Object) -join '|')
        $lnk = $shortcuts[(Join-Path $desktop 'Startup\IDLifecycle Tray.lnk')]
        $lnk['Arguments'] | Should Be ('-NoProfile -STA -WindowStyle Hidden -ExecutionPolicy RemoteSigned -File "{0}"' -f (Join-Path $fx.Install 'scripts\Start-IDLifecycleTray.ps1'))
        $lnk['Arguments'] | Should Not Match 'Bypass'
        $shortcuts[(Join-Path $desktop 'Programs\IDLifecycle\IDLifecycle Setup.lnk')]['Target'] | Should Be (Join-Path $fx.Install 'Setup.cmd')
        (Test-Path -LiteralPath (Join-Path $desktop 'IDLifecycle-Branding\README.txt')) | Should Be $true
        foreach ($step in @('WriteTrayInfo', 'BrandingFolder', 'SetupShortcut', 'TrayShortcut', 'TrayAutostart')){
            $r.ReportText | Should Match ('"' + $step + '","Applied"')
        }

        # Re-run: everything is up to date.
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx) -StatePath $fx.State -AdminPassword $adminPw
        $r.ExitCode | Should Be 0
        foreach ($step in @('WriteTrayInfo', 'BrandingFolder', 'SetupShortcut', 'TrayShortcut', 'TrayAutostart')){
            $r.ReportText | Should Match ('"' + $step + '","Skipped"')
        }

        # A branding folder others can change is reported (WARN), never "fixed" silently.
        Update-FakeSetupState -Path $fx.State -Change { param($s) $s['brandingUntrusted'] = 'BUILTIN\Users can change it' }
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx -Extra @{ NoTray = $true }) -StatePath $fx.State -AdminPassword $adminPw
        $r.ExitCode | Should Be 0
        $r.LogText | Should Match '\[WARN\] SKIP: BrandingFolder .*not protected \(BUILTIN\\Users can change it\)'
        $r.ReportText | Should Match '"RemoveTrayShortcut","Applied"'
        $r.ReportText | Should Match '"RemoveTrayAutostart","Applied"'
        @((Read-FakeSetupState -Path $fx.State)['shortcuts'].Keys) -join '|' | Should Be (Join-Path $desktop 'Programs\IDLifecycle\IDLifecycle Setup.lnk')
        $r.LogText | Should Not Match 'Tray icon: starts for everyone'
        Remove-SetupFixture -Fixture $fx
    }

    It 'IDL_PASSWORD in the environment is used for user add only and removed from the installer process' {
        $fx = New-SetupFixture
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx -Extra @{ Start = $false }) -StatePath $fx.State -ExtraEnv @{ IDL_PASSWORD = $adminPw }
        $r.ExitCode | Should Be 0
        $r.AllText | Should Not Match ([regex]::Escape($adminPw))
        $r.ReportText | Should Match '"StartService","Skipped"'
        foreach ($c in @(Get-FakeCalls -Path $fx.State | Where-Object { $_['op'] -like 'daemon:*' })){
            $c['data']['hasPassword'] | Should Be ($c['op'] -eq 'daemon:user add')
        }

        @(Get-FakeOps -Path $fx.State | Where-Object { $_ -in @('start', 'probe') }).Count | Should Be 0
        Remove-SetupFixture -Fixture $fx
    }

    It 'an existing config.json is never overwritten (init is not even called) and its listen address is used' {
        $fx = New-SetupFixture
        $null = New-Item -ItemType Directory -Force -Path $fx.Data
        $json = ConvertTo-Json -InputObject ([ordered]@{
                listen     = '127.0.0.1:9555'
                data_dir   = $fx.Data
                database   = @{ driver = 'sqlite'; dsn = 'file:keep.db' }
                powershell = @{ scripts_dir = (Join-Path $fx.Install 'scripts') }
                log_level  = 'debug'
            })
        [System.IO.File]::WriteAllText($fx.Config, $json, $script:Utf8NoBom)
        $before = (Get-FileHash -LiteralPath $fx.Config -Algorithm SHA256).Hash
        Update-FakeSetupState -Path $fx.State -Change { param($s) $s['admins'] = 1 }

        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx -Extra @{ Listen = '127.0.0.1:8443'; DbDriver = 'sqlite' }) -StatePath $fx.State
        $r.ExitCode | Should Be 0
        (Get-FileHash -LiteralPath $fx.Config -Algorithm SHA256).Hash | Should Be $before
        $r.ReportText | Should Match '"InitConfig","Skipped",.*kept existing config\.json'
        $r.LogText | Should Match '-Listen is ignored'
        @(Get-FakeOps -Path $fx.State) -contains 'daemon:init' | Should Be $false
        @(Get-FakeCalls -Path $fx.State -Op 'probe')[0]['data']['url'] | Should Be 'http://127.0.0.1:9555/'
        # No admin password was needed: one already exists.
        $r.ReportText | Should Match '"CreateAdminUser","Skipped"'
        Remove-SetupFixture -Fixture $fx
    }

    It 'upgrade: refuses without -Force; with -Force stops the running service, copies, and starts it again' {
        $fx = New-SetupFixture
        (Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx) -StatePath $fx.State -AdminPassword $adminPw).ExitCode | Should Be 0
        [System.IO.File]::WriteAllText($fx.Exe, 'FAKE-IDLIFECYCLED 1.1.0', $script:Utf8NoBom)
        Write-ReleaseManifest -Root $fx.Source
        Reset-FakeCalls -Path $fx.State

        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx) -StatePath $fx.State
        $r.ExitCode | Should Be 2
        $r.LogText | Should Match 'upgrade 1\.0\.0 -> 1\.1\.0'
        $r.LogText | Should Match '-Force'
        @(Get-FakeOps -Path $fx.State) -contains 'stop' | Should Be $false
        [System.IO.File]::ReadAllText((Join-Path $fx.Install 'idlifecycled.exe')) | Should Match '1\.0\.0'

        Reset-FakeCalls -Path $fx.State
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx -Extra @{ Force = $true; Start = $false }) -StatePath $fx.State
        $r.ExitCode | Should Be 0
        $ops = @(Get-FakeOps -Path $fx.State)
        $stopAt = [array]::IndexOf($ops, 'stop')
        $startAt = [array]::IndexOf($ops, 'start')
        $stopAt | Should Not Be -1
        ($startAt -gt $stopAt) | Should Be $true
        ($stopAt -lt [array]::IndexOf($ops, 'serviceInstaller')) | Should Be $true
        $r.ReportText | Should Match '"StopService","Applied"'
        $r.ReportText | Should Match '"CopyProgramFiles","Applied",.*upgrade 1\.0\.0 -> 1\.1\.0'
        $r.ReportText | Should Match '"StartService","Applied",.*restart'
        [System.IO.File]::ReadAllText((Join-Path $fx.Install 'idlifecycled.exe')) | Should Match '1\.1\.0'
        (Read-FakeSetupState -Path $fx.State)['services']['IDLSetupTest']['State'] | Should Be 'Running'
        Remove-SetupFixture -Fixture $fx
    }

    It 'a failing check stops the install before the service is started (exit 1, detail reported)' {
        $fx = New-SetupFixture
        Update-FakeSetupState -Path $fx.State -Change { param($s) $s['checkFail'] = @{ scripts_unblocked = 'boom: 2 script file(s) carry Mark-of-the-Web' } }
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx) -StatePath $fx.State -AdminPassword $adminPw
        $r.ExitCode | Should Be 1
        $r.LogText | Should Match 'scripts_unblocked\s+FAIL\s+boom'
        $r.ReportText | Should Match '"Check","Failed".*boom: 2 script file'
        $r.LogText | Should Match 'The service was NOT started'
        $ops = @(Get-FakeOps -Path $fx.State)
        $ops -contains 'start' | Should Be $false
        $ops -contains 'probe' | Should Be $false
        (Read-FakeSetupState -Path $fx.State)['services']['IDLSetupTest']['State'] | Should Be 'Stopped'
        Remove-SetupFixture -Fixture $fx
    }

    It 'check contract: skipped ad_module is fine; a skipped required check or a missing name fails the install' {
        $fx = New-SetupFixture
        Update-FakeSetupState -Path $fx.State -Change { param($s) $s['checkSkip'] = @{ scripts = 'not run: fake reason' } }
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx) -StatePath $fx.State -AdminPassword $adminPw
        $r.ExitCode | Should Be 1
        $r.LogText | Should Match 'scripts\s+skip\s+not run: fake reason'
        $r.LogText | Should Match 'ad_module\s+skip'
        $r.ReportText | Should Match '"Check","Failed".*scripts: not run'
        @(Get-FakeOps -Path $fx.State) -contains 'start' | Should Be $false

        Update-FakeSetupState -Path $fx.State -Change { param($s) $s['checkSkip'] = @{}; $s['checkOmit'] = @('powershell'); $s['calls'] = @() }
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx) -StatePath $fx.State
        $r.ExitCode | Should Be 1
        $r.ReportText | Should Match '"Check","Failed".*powershell: missing from the check output'
        @(Get-FakeOps -Path $fx.State) -contains 'start' | Should Be $false

        # Back to the normal output: everything passes, and the skipped ad_module is reported as skip, not FAIL.
        Update-FakeSetupState -Path $fx.State -Change { param($s) $s['checkOmit'] = @() }
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx) -StatePath $fx.State
        $r.ExitCode | Should Be 0
        $r.LogText | Should Match 'ad_module\s+skip\s+not run: -skip-ad-module'
        $r.LogText | Should Not Match '\sFAIL\s'
        $r.ReportText | Should Match '"Check","Verified",.*all 8 checks that ran passed'
        Remove-SetupFixture -Fixture $fx
    }

    It 'propagates a failure of Install-IDLifecycleService.ps1 (its exit code) and starts nothing' {
        $fx = New-SetupFixture
        Update-FakeSetupState -Path $fx.State -Change { param($s) $s['installerExit'] = 3 }
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx) -StatePath $fx.State -AdminPassword $adminPw
        $r.ExitCode | Should Be 3
        $r.ReportText | Should Match '"RegisterService","Failed"'
        @(Get-FakeOps -Path $fx.State) -contains 'start' | Should Be $false
        Remove-SetupFixture -Fixture $fx
    }

    It 'server DB default: DSN stored in config.json after lockdown (placeholder for init, no env var), service started' {
        $fx = New-SetupFixture
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx -Extra @{ DbDriver = 'mssql' }) -StatePath $fx.State -AdminPassword $adminPw -DbDsn $dsn
        $r.ExitCode | Should Be 0
        $r.AllText | Should Not Match 'Sentinel-Dsn-Pw'
        $r.AllText | Should Not Match ([regex]::Escape($adminPw))
        $r.ReportText | Should Match '"SetConfigDsn","Applied"'
        $r.ReportText | Should Match '"VerifyDataDirAcl","Verified"'
        $r.ReportText | Should Match '"StartService","Applied"'
        $r.ReportText | Should Not Match 'SetDbDsnEnvVar'
        $r.LogText | Should Not Match 'NOT a secret store'
        # The DSN is written only after the data dir was locked down.
        $r.ReportText.IndexOf('"LockdownDataDir","Applied"') | Should BeLessThan $r.ReportText.IndexOf('"SetConfigDsn","Applied"')

        $cfg = [System.IO.File]::ReadAllText($fx.Config) | ConvertFrom-Json -AsHashtable
        $cfg['database']['driver'] | Should Be 'mssql'
        $cfg['database']['dsn'] | Should Be $dsn
        $cfg['listen'] | Should Be '127.0.0.1:8443'
        $cfg['powershell']['scripts_dir'] | Should Be (Join-Path $fx.Install 'scripts')
        @(Get-ChildItem -LiteralPath $fx.Data -Filter '*.tmp' -Force).Count | Should Be 0

        $state = Read-FakeSetupState -Path $fx.State
        @($state['machineEnv'].Keys).Count | Should Be 0
        $ops = @(Get-FakeOps -Path $fx.State)
        $ops -contains 'setMachineEnv' | Should Be $false
        $ops -contains 'start' | Should Be $true
        # No child ever had the DSN on its command line; init got the placeholder.
        (ConvertTo-Json -InputObject @($state['calls']) -Depth 10) | Should Not Match 'Sentinel-Dsn-Pw'
        (@(Get-FakeCalls -Path $fx.State -Op 'daemon:init')[0]['data']['args'] -join ' ') | Should Match '-db-dsn env:IDL_SETUP_PLACEHOLDER'

        # Re-run with the same DSN: skipped.
        Reset-FakeCalls -Path $fx.State
        $r2 = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx -Extra @{ DbDriver = 'mssql' }) -StatePath $fx.State -DbDsn $dsn
        $r2.ExitCode | Should Be 0
        $r2.ReportText | Should Match '"SetConfigDsn","Skipped"'
        $r2.ReportText | Should Not Match '"Applied"'
        $r2.AllText | Should Not Match 'Sentinel-Dsn-Pw'

        # A different DSN is refused without -Force (nothing touched) ...
        $other = 'sqlserver://idl:Sentinel-New-Pw-5Lm@sql02?database=idl'
        Reset-FakeCalls -Path $fx.State
        $before = (Get-FileHash -LiteralPath $fx.Config -Algorithm SHA256).Hash
        $r3 = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx -Extra @{ DbDriver = 'mssql' }) -StatePath $fx.State -DbDsn $other
        $r3.ExitCode | Should Be 2
        $r3.LogText | Should Match 'already holds a different database connection string; re-run with -Force'
        $r3.AllText | Should Not Match 'Sentinel-New-Pw|Sentinel-Dsn-Pw'
        (Get-FileHash -LiteralPath $fx.Config -Algorithm SHA256).Hash | Should Be $before
        @(Get-FakeOps -Path $fx.State).Count | Should Be 0

        # ... and replaced with -Force, other fields kept.
        $r4 = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx -Extra @{ DbDriver = 'mssql'; Force = $true }) -StatePath $fx.State -DbDsn $other
        $r4.ExitCode | Should Be 0
        $r4.ReportText | Should Match '"SetConfigDsn","Applied"'
        $r4.AllText | Should Not Match 'Sentinel-New-Pw|Sentinel-Dsn-Pw'
        $cfg = [System.IO.File]::ReadAllText($fx.Config) | ConvertFrom-Json -AsHashtable
        $cfg['database']['dsn'] | Should Be $other
        $cfg['database']['driver'] | Should Be 'mssql'
        $cfg['data_dir'] | Should Be $fx.Data
        Remove-SetupFixture -Fixture $fx
    }

    It 'server DB opt-in -DbDsnEnvVar: DSN goes to a machine env var (never argv/log/config), config gets env:NAME, start waits for a reboot' {
        $fx = New-SetupFixture
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx -Extra @{ DbDriver = 'mssql'; DbDsnEnvVar = 'IDL_DB_DSN' }) -StatePath $fx.State -AdminPassword $adminPw -DbDsn $dsn
        $r.ExitCode | Should Be 0
        $r.AllText | Should Not Match 'Sentinel-Dsn-Pw'
        $r.AllText | Should Not Match ([regex]::Escape($adminPw))
        $r.ReportText | Should Match '"SetDbDsnEnvVar","Applied","Machine:IDL_DB_DSN"'
        $r.LogText | Should Match 'NOT a secret store'
        $r.LogText | Should Match 'BUILTIN\\Users'
        $r.ReportText | Should Match '"StartService","Skipped",.*reboot required'

        $state = Read-FakeSetupState -Path $fx.State
        $state['machineEnv']['IDL_DB_DSN'] | Should Be $dsn
        $cfg = [System.IO.File]::ReadAllText($fx.Config) | ConvertFrom-Json -AsHashtable
        $cfg['database']['driver'] | Should Be 'mssql'
        $cfg['database']['dsn'] | Should Be 'env:IDL_DB_DSN'
        (ConvertTo-Json -InputObject @($state['calls']) -Depth 10) | Should Not Match 'Sentinel-Dsn-Pw'
        # The DSN variable is handed to the idlifecycled children (check / user add) that need the database.
        $check = @(Get-FakeCalls -Path $fx.State -Op 'daemon:check')[0]
        @($check['data']['idlEnv']) -contains 'IDL_DB_DSN' | Should Be $true
        @(Get-FakeOps -Path $fx.State | Where-Object { $_ -in @('start', 'probe') }).Count | Should Be 0

        # Re-run with the same DSN: the variable is already in place, so the service is started.
        Reset-FakeCalls -Path $fx.State
        $r2 = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx -Extra @{ DbDriver = 'mssql'; DbDsnEnvVar = 'IDL_DB_DSN' }) -StatePath $fx.State -DbDsn $dsn
        $r2.ExitCode | Should Be 0
        $r2.ReportText | Should Match '"SetDbDsnEnvVar","Skipped"'
        $r2.ReportText | Should Not Match 'SetConfigDsn'
        $r2.ReportText | Should Match '"StartService","Applied"'
        $r2.AllText | Should Not Match 'Sentinel-Dsn-Pw'
        Remove-SetupFixture -Fixture $fx
    }

    It 'IDL_SETUP_DB_DSN (how the setup GUI passes the DSN) works like -DbDsn: stored in config.json, never logged or passed on' {
        $fx = New-SetupFixture
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx -Extra @{ DbDriver = 'mssql' }) -StatePath $fx.State -AdminPassword $adminPw -ExtraEnv @{ IDL_SETUP_DB_DSN = $dsn }
        $r.ExitCode | Should Be 0
        $r.AllText | Should Not Match 'Sentinel-Dsn-Pw'
        $r.ReportText | Should Match '"SetConfigDsn","Applied"'
        $cfg = [System.IO.File]::ReadAllText($fx.Config) | ConvertFrom-Json -AsHashtable
        $cfg['database']['dsn'] | Should Be $dsn
        $state = Read-FakeSetupState -Path $fx.State
        (ConvertTo-Json -InputObject @($state['calls']) -Depth 10) | Should Not Match 'Sentinel-Dsn-Pw|IDL_SETUP_DB_DSN'
        @(Get-FakeCalls -Path $fx.State -Op 'serviceInstaller')[0]['data']['envNames'] -contains 'IDL_SETUP_DB_DSN' | Should Be $false
        Remove-SetupFixture -Fixture $fx
    }

    It 'RestoreFrom: header checked in the preflight, restore before the check, passphrase only in that child; the backup''s admin makes user add unnecessary' {
        $fx = New-SetupFixture
        $bak = Join-Path $fx.Root 'IDLifecycle_OLD_20261001_020000.idlbak'
        [System.IO.File]::WriteAllText($bak, 'IDLBAK1 fake backup')
        $pass = 'backup passphrase 1'

        # Not a backup: refused in the preflight, before anything is installed.
        $notBak = Join-Path $fx.Root 'notes.txt'
        Set-Content -LiteralPath $notBak -Value 'hello'
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx -Extra @{ RestoreFrom = $notBak }) -StatePath $fx.State -ExtraEnv @{ IDL_BACKUP_PASSPHRASE = $pass }
        $r.ExitCode | Should Be 2
        $r.LogText | Should Match 'not an IDLifecycle backup file'
        @(Get-FakeOps -Path $fx.State) -contains 'daemon:init' | Should Be $false

        # No passphrase in a non-interactive run; -RestoreConfig alone.
        Reset-FakeCalls -Path $fx.State
        (Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx -Extra @{ RestoreFrom = $bak }) -StatePath $fx.State).ExitCode | Should Be 2
        (Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx -Extra @{ RestoreConfig = $true; DryRun = $true }) -StatePath $fx.State).ExitCode | Should Be 2
        @(Get-FakeOps -Path $fx.State).Count | Should Be 0

        # Wrong passphrase: the step fails, nothing is started.
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx -Extra @{ RestoreFrom = $bak }) -StatePath $fx.State -ExtraEnv @{ IDL_BACKUP_PASSPHRASE = 'not the passphrase' }
        $r.ExitCode | Should Be 1
        $r.LogText | Should Match 'idlifecycled restore failed with exit code 2: error: the backup cannot be decrypted'
        @(Get-FakeOps -Path $fx.State) -contains 'start' | Should Be $false

        # The right passphrase on the fresh install: restored, the backup's admin is used, the service starts.
        Reset-FakeCalls -Path $fx.State
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx -Extra @{ RestoreFrom = $bak; RestoreConfig = $true }) -StatePath $fx.State -ExtraEnv @{ IDL_BACKUP_PASSPHRASE = $pass }
        $r.ExitCode | Should Be 0
        $r.AllText | Should Not Match ([regex]::Escape($pass))
        $r.LogText | Should Match 'backup: Backup made 2026-10-01'
        $r.ReportText | Should Match '"RestoreBackup","Applied"'
        $r.ReportText | Should Match '"CreateAdminUser","Skipped"'
        $ops = @(Get-FakeOps -Path $fx.State)
        $ops -contains 'daemon:user add' | Should Be $false
        [array]::IndexOf($ops, 'daemon:restore') | Should BeLessThan ([array]::LastIndexOf($ops, 'daemon:check'))
        $restores = @(Get-FakeCalls -Path $fx.State -Op 'daemon:restore')
        $restores.Count | Should Be 2
        $restores[0]['data']['args'] -contains '-inspect' | Should Be $true
        $restores[0]['data']['backupPassLength'] | Should Be 0
        $restores[1]['data']['backupPassLength'] | Should Be $pass.Length
        $restores[1]['data']['args'] -contains '-with-config' | Should Be $true
        foreach ($c in @(Get-FakeCalls -Path $fx.State | Where-Object { ($_['op'] -like 'daemon:*') -and ($_['op'] -ne 'daemon:restore') })){
            $c['data']['backupPassLength'] | Should Be 0
        }

        $state = Read-FakeSetupState -Path $fx.State
        $state['restored'] | Should Be $true
        $state['restoredWithConfig'] | Should Be $true

        # Again over existing data: refused without -Force; with -Force the running service is stopped first.
        Reset-FakeCalls -Path $fx.State
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx -Extra @{ RestoreFrom = $bak }) -StatePath $fx.State -ExtraEnv @{ IDL_BACKUP_PASSPHRASE = $pass }
        $r.ExitCode | Should Be 1
        $r.LogText | Should Match 'Re-run with -Force to replace the existing data'
        (Read-FakeSetupState -Path $fx.State)['services']['IDLSetupTest']['State'] | Should Be 'Running'

        Reset-FakeCalls -Path $fx.State
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx -Extra @{ RestoreFrom = $bak; Force = $true }) -StatePath $fx.State -ExtraEnv @{ IDL_BACKUP_PASSPHRASE = $pass }
        $r.ExitCode | Should Be 0
        $ops = @(Get-FakeOps -Path $fx.State)
        [array]::IndexOf($ops, 'stop') | Should BeLessThan ([array]::LastIndexOf($ops, 'daemon:restore'))
        $ops -contains 'start' | Should Be $true
        @(Get-FakeCalls -Path $fx.State -Op 'daemon:restore')[-1]['data']['args'] -contains '-force' | Should Be $true
        Remove-SetupFixture -Fixture $fx
    }

    It 'TLS: wildcard listen probes loopback without cert validation; a named host is probed with validation' {
        $fx = New-SetupFixture
        $cert = Join-Path $fx.Root 'cert.pem'
        $key = Join-Path $fx.Root 'key.pem'
        Set-Content -LiteralPath $cert -Value 'dummy'
        Set-Content -LiteralPath $key -Value 'dummy'
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx -Extra @{ Listen = '0.0.0.0:8443'; TlsCertFile = $cert; TlsKeyFile = $key }) -StatePath $fx.State -AdminPassword $adminPw
        $r.ExitCode | Should Be 0
        $probe = @(Get-FakeCalls -Path $fx.State -Op 'probe')[0]
        $probe['data']['url'] | Should Be 'https://127.0.0.1:8443/'
        $probe['data']['skipCert'] | Should Be $true

        $fx2 = New-SetupFixture
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx2 -Extra @{ Listen = 'idl.example.test:8443'; TlsCertFile = $cert; TlsKeyFile = $key }) -StatePath $fx2.State -AdminPassword $adminPw
        $r.ExitCode | Should Be 0
        $probe = @(Get-FakeCalls -Path $fx2.State -Op 'probe')[0]
        $probe['data']['url'] | Should Be 'https://idl.example.test:8443/'
        $probe['data']['skipCert'] | Should Be $false
        Remove-SetupFixture -Fixture $fx2
        Remove-SetupFixture -Fixture $fx
    }

    It 'a failing HTTP probe fails the install (exit 1)' {
        $fx = New-SetupFixture
        Update-FakeSetupState -Path $fx.State -Change { param($s) $s['httpStatus'] = 503 }
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx) -StatePath $fx.State -AdminPassword $adminPw -TimeoutSec 120
        $r.ExitCode | Should Be 1
        $r.ReportText | Should Match '"ProbeHttp","Failed".*HTTP 503'
        Remove-SetupFixture -Fixture $fx
    }

    It 'refuses dangerous InstallDir / DataDir values (path guard, exit 2)' {
        $fx = New-SetupFixture
        foreach ($bad in @('C:\', 'C:\Windows', 'C:\Program Files', $env:USERPROFILE, '')){
            foreach ($name in @('InstallDir', 'DataDir')){
                $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx -Extra @{ $name = $bad; DryRun = $true }) -StatePath $fx.State
                $r.ExitCode | Should Be 2
                $r.LogText | Should Match "$name '.*' refused"
            }
        }

        # InstallDir and DataDir must not be nested.
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx -Extra @{ DataDir = (Join-Path $fx.Install 'data'); DryRun = $true }) -StatePath $fx.State
        $r.ExitCode | Should Be 2
        @(Get-FakeOps -Path $fx.State).Count | Should Be 0
        Remove-SetupFixture -Fixture $fx
    }
}

Describe 'Install-IDLifecycle.ps1 security (release trust, pre-created folders, InstallDir ACL, unblock scope, logs, secrets, hook guards)' {

    $adminPw = 'Sentinel-Admin-Pw-7Q2x'
    $untrustedSid = 'S-1-5-21-1111111111-2222222222-3333333333-1001'

    function Get-BaseParams {
        param($Fx, [hashtable]$Extra = @{})

        $p = @{
            SourceDir      = $Fx.Source
            InstallDir     = $Fx.Install
            DataDir        = $Fx.Data
            ServiceAccount = 'TESTDOM\svc-idl$'
            ServiceName    = 'IDLSetupTest'
        }
        foreach ($k in $Extra.Keys){
            $p[$k] = $Extra[$k]
        }

        return $p
    }

    It 'C1: a planted/modified exe, an extra script, a missing manifest or -RequireSignature on an unsigned exe are refused before anything runs' {
        $fx = New-SetupFixture
        $p = Get-BaseParams -Fx $fx -Extra @{ DryRun = $true }

        # Planted exe (content differs from SHA256SUMS): refused, and `version` was never run.
        [System.IO.File]::WriteAllText($fx.Exe, 'PLANTED-BY-A-USER 6.6.6', $script:Utf8NoBom)
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params $p -StatePath $fx.State
        $r.ExitCode | Should Be 2
        $r.LogText | Should Match "'idlifecycled\.exe' does not match SHA256SUMS"
        @(Get-FakeOps -Path $fx.State) -contains 'daemon:version' | Should Be $false
        [System.IO.File]::WriteAllText($fx.Exe, 'FAKE-IDLIFECYCLED 1.0.0', $script:Utf8NoBom)

        # Extra file below scripts\ that the manifest does not list.
        Set-Content -LiteralPath (Join-Path $fx.Source 'scripts\Evil.ps1') -Value '# planted'
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params $p -StatePath $fx.State
        $r.ExitCode | Should Be 2
        $r.LogText | Should Match 'not listed in SHA256SUMS: scripts\\Evil\.ps1'
        Remove-Item -LiteralPath (Join-Path $fx.Source 'scripts\Evil.ps1') -Force

        # An exe only in bin\ is not accepted (one location: <SourceDir>\idlifecycled.exe).
        $null = New-Item -ItemType Directory -Path (Join-Path $fx.Source 'bin')
        Move-Item -LiteralPath $fx.Exe -Destination (Join-Path $fx.Source 'bin\idlifecycled.exe')
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params $p -StatePath $fx.State
        $r.ExitCode | Should Be 2
        $r.LogText | Should Match "missing 'idlifecycled\.exe'"
        Move-Item -LiteralPath (Join-Path $fx.Source 'bin\idlifecycled.exe') -Destination $fx.Exe
        Remove-Item -LiteralPath (Join-Path $fx.Source 'bin') -Force

        # Unsigned exe with -RequireSignature.
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx -Extra @{ DryRun = $true; RequireSignature = $true }) -StatePath $fx.State
        $r.ExitCode | Should Be 2
        $r.LogText | Should Match 'no valid Authenticode signature'

        # Setup.cmd (the GUI launcher) is the only other file a release may list at its root.
        Set-Content -LiteralPath (Join-Path $fx.Source 'Other.cmd') -Value '@echo planted'
        Write-ReleaseManifest -Root $fx.Source -Extra @('Other.cmd')
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params $p -StatePath $fx.State
        $r.ExitCode | Should Be 2
        $r.LogText | Should Match "lists 'Other\.cmd', which is not part of a release"
        Remove-Item -LiteralPath (Join-Path $fx.Source 'Other.cmd') -Force
        Set-Content -LiteralPath (Join-Path $fx.Source 'Setup.cmd') -Value '@echo launcher'
        Write-ReleaseManifest -Root $fx.Source
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params $p -StatePath $fx.State
        $r.ExitCode | Should Be 0
        [System.IO.File]::WriteAllText((Join-Path $fx.Source 'Setup.cmd'), '@echo modified', $script:Utf8NoBom)
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params $p -StatePath $fx.State
        $r.ExitCode | Should Be 2
        $r.LogText | Should Match "'Setup\.cmd' does not match SHA256SUMS"
        Remove-Item -LiteralPath (Join-Path $fx.Source 'Setup.cmd') -Force
        Write-ReleaseManifest -Root $fx.Source
        # The accepted run above did run `version`; the refusal below must not.
        Reset-FakeCalls -Path $fx.State

        # No manifest at all.
        Remove-Item -LiteralPath (Join-Path $fx.Source 'SHA256SUMS') -Force
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params $p -StatePath $fx.State
        $r.ExitCode | Should Be 2
        $r.LogText | Should Match 'has no SHA256SUMS manifest'
        @(Get-FakeOps -Path $fx.State) -contains 'daemon:version' | Should Be $false
        Remove-SetupFixture -Fixture $fx
    }

    It 'SkipIntegrityCheck (least safe): no or a stale SHA256SUMS is accepted with warnings; the installed manifest lists what was copied' {
        $fx = New-SetupFixture
        $stagingDir = Join-Path $script:StateDir 'Staging'
        $tempBefore = @(Get-ChildItem -LiteralPath $stagingDir -Filter 'unverified_*' -ErrorAction SilentlyContinue).Count
        Remove-Item -LiteralPath (Join-Path $fx.Source 'SHA256SUMS') -Force
        Set-Content -LiteralPath (Join-Path $fx.Source 'scripts\Extra-Test.ps1') -Value '# a local test script'
        Set-Content -LiteralPath (Join-Path $fx.Source 'Setup.cmd') -Value '@echo launcher'
        $null = New-Item -ItemType Directory -Path (Join-Path $fx.Source 'scripts\Logs')
        Set-Content -LiteralPath (Join-Path $fx.Source 'scripts\Logs\old.log') -Value 'not shipped'

        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx -Extra @{ SkipIntegrityCheck = $true }) -StatePath $fx.State -AdminPassword $adminPw
        $r.ExitCode | Should Be 0
        $r.LogText | Should Match 'UNVERIFIED INSTALL \(-SkipIntegrityCheck, least safe\)'
        $r.LogText | Should Match 'Reminder: this was an UNVERIFIED install'
        $r.ReportText | Should Match '"IntegrityCheck","Skipped"'
        (Test-Path -LiteralPath (Join-Path $fx.Install 'scripts\Extra-Test.ps1')) | Should Be $true
        (Test-Path -LiteralPath (Join-Path $fx.Install 'scripts\Logs\old.log')) | Should Be $false

        # The installed SHA256SUMS describes exactly the copied files, so upgrade and uninstall work as usual.
        $manifest = Get-Content -LiteralPath (Join-Path $fx.Install 'SHA256SUMS')
        @($manifest | Where-Object { $_ -match '  scripts/Extra-Test\.ps1$' }).Count | Should Be 1
        @($manifest | Where-Object { $_ -match '  Setup\.cmd$' }).Count | Should Be 1
        @($manifest | Where-Object { $_ -match 'Logs' }).Count | Should Be 0
        foreach ($line in $manifest){
            $hash, $rel = $line -split '  ', 2
            (Get-FileHash -LiteralPath (Join-Path $fx.Install $rel) -Algorithm SHA256).Hash | Should Be $hash.ToUpperInvariant()
        }

        # The generated manifest's staging folder is gone.
        @(Get-ChildItem -LiteralPath $stagingDir -Filter 'unverified_*' -ErrorAction SilentlyContinue).Count | Should Be $tempBefore

        # A changed file in a folder with a stale manifest is accepted too (that is the point - and the risk).
        Write-ReleaseManifest -Root $fx.Source
        [System.IO.File]::WriteAllText((Join-Path $fx.Source 'scripts\Extra-Test.ps1'), '# changed after the manifest', $script:Utf8NoBom)
        # (the installed copy differs now, so this is an upgrade: -Force)
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx -Extra @{ SkipIntegrityCheck = $true; DryRun = $true; Force = $true }) -StatePath $fx.State
        $r.ExitCode | Should Be 0
        $r.LogText | Should Match 'UNVERIFIED INSTALL'
        Remove-SetupFixture -Fixture $fx
    }

    It 'SkipIntegrityCheck still refuses an untrusted folder, a missing exe or required script, and an unsigned exe with -RequireSignature' {
        $fx = New-SetupFixture
        $tempBefore = @(Get-ChildItem -LiteralPath (Join-Path $script:StateDir 'Staging') -Filter 'unverified_*' -ErrorAction SilentlyContinue).Count
        $p = Get-BaseParams -Fx $fx -Extra @{ SkipIntegrityCheck = $true; DryRun = $true }

        Remove-Item -LiteralPath (Join-Path $fx.Source 'scripts\New-IDLUser.ps1') -Force
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params $p -StatePath $fx.State
        $r.ExitCode | Should Be 2
        $r.LogText | Should Match 'is missing: scripts\\New-IDLUser\.ps1'
        Copy-Item -LiteralPath (Join-Path $script:ScriptsDir 'New-IDLUser.ps1') -Destination (Join-Path $fx.Source 'scripts\New-IDLUser.ps1')

        Move-Item -LiteralPath $fx.Exe -Destination (Join-Path $fx.Root 'moved.exe')
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params $p -StatePath $fx.State
        $r.ExitCode | Should Be 2
        $r.LogText | Should Match 'has no idlifecycled\.exe\. In a source checkout build it first'
        Move-Item -LiteralPath (Join-Path $fx.Root 'moved.exe') -Destination $fx.Exe

        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx -Extra @{ SkipIntegrityCheck = $true; DryRun = $true; RequireSignature = $true }) -StatePath $fx.State
        $r.ExitCode | Should Be 2
        $r.LogText | Should Match 'no valid Authenticode signature'
        @(Get-ChildItem -LiteralPath (Join-Path $script:StateDir 'Staging') -Filter 'unverified_*' -ErrorAction SilentlyContinue).Count | Should Be $tempBefore

        Grant-TestAce -Path (Join-Path $fx.Source 'scripts') -Sid 'S-1-5-32-545' -Rights 'Modify'
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params $p -StatePath $fx.State
        $r.ExitCode | Should Be 2
        $r.LogText | Should Match 'is not trusted'
        @(Get-FakeOps -Path $fx.State) -contains 'daemon:version' | Should Be $false
        Remove-SetupFixture -Fixture $fx
    }

    It 'C1: a SourceDir (or file, or parent) that others can change is refused - also with -Force' {
        $fx = New-SetupFixture
        $p = Get-BaseParams -Fx $fx -Extra @{ DryRun = $true; Force = $true }

        Grant-TestAce -Path (Join-Path $fx.Source 'scripts\IDLifecycle.Common.psm1') -Rights 'Modify'
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params $p -StatePath $fx.State
        $r.ExitCode | Should Be 2
        $r.LogText | Should Match "is not trusted: .*IDLifecycle\.Common\.psm1' lets BUILTIN\\Users change it"
        @(Get-FakeOps -Path $fx.State) -contains 'daemon:version' | Should Be $false

        $fx2 = New-SetupFixture
        Grant-TestAce -Path $fx2.Source -Rights 'Write'
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx2 -Extra @{ DryRun = $true }) -StatePath $fx2.State
        $r.ExitCode | Should Be 2
        $r.LogText | Should Match 'is not trusted'

        # Parent chain: "delete child" on the parent lets Users replace the release folder.
        $fx3 = New-SetupFixture
        Grant-TestAce -Path $fx3.Root -Rights 'DeleteSubdirectoriesAndFiles'
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx3 -Extra @{ DryRun = $true }) -StatePath $fx3.State
        $r.ExitCode | Should Be 2
        $r.LogText | Should Match 'is not trusted: parent folder'

        # Inherit-only templates on a parent are fine for a release folder with inheritance off (what
        # New-IDLifecycleRelease.ps1 creates) - like C:\'s CREATOR OWNER / Users (IO) entries.
        $fx4 = New-SetupFixture
        $relInfo = [System.IO.DirectoryInfo]::new($fx4.Source)
        $relAcl = [System.IO.FileSystemAclExtensions]::GetAccessControl($relInfo, [System.Security.AccessControl.AccessControlSections]::Access)
        $relAcl.SetAccessRuleProtection($true, $true)
        [System.IO.FileSystemAclExtensions]::SetAccessControl($relInfo, $relAcl)
        Grant-TestAce -Path $fx4.Root -Rights 'Modify' -InheritOnly
        (Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx4 -Extra @{ DryRun = $true }) -StatePath $fx4.State).ExitCode | Should Not Be 2

        # Untrusted owner of the release folder (simulated).
        $fx5 = New-SetupFixture
        Update-FakeSetupState -Path $fx5.State -Change { param($s) $s['ownerOverrides'] = @{ $fx5.Source = $untrustedSid } }
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx5 -Extra @{ DryRun = $true }) -StatePath $fx5.State
        $r.ExitCode | Should Be 2
        $r.LogText | Should Match 'is owned by .*which is not SYSTEM, Administrators, TrustedInstaller or you'
        foreach ($f in @($fx, $fx2, $fx3, $fx4, $fx5)){
            Remove-SetupFixture -Fixture $f
        }
    }

    It 'H1: a pre-created DataDir / config.json or InstallDir owned by someone else is refused (exit 2) before init/lockdown' {
        $fx = New-SetupFixture
        $null = New-Item -ItemType Directory -Force -Path $fx.Data
        Update-FakeSetupState -Path $fx.State -Change { param($s) $s['ownerOverrides'] = @{ $fx.Data = $untrustedSid } }
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx) -StatePath $fx.State -AdminPassword $adminPw
        $r.ExitCode | Should Be 2
        $r.LogText | Should Match 'already exists and is owned by .*Delete it if you did not create it'
        $ops = @(Get-FakeOps -Path $fx.State)
        $ops -contains 'daemon:init' | Should Be $false
        $ops -contains 'daemon:lockdown' | Should Be $false
        (Test-Path -LiteralPath $fx.Install) | Should Be $false

        # Trusted folder but a planted config.json owned by someone else.
        $cfg = ConvertTo-Json -InputObject ([ordered]@{ data_dir = $fx.Data; powershell = @{ scripts_dir = (Join-Path $fx.Install 'scripts') }; database = @{ driver = 'sqlite'; dsn = 'x' } })
        [System.IO.File]::WriteAllText($fx.Config, $cfg, $script:Utf8NoBom)
        Update-FakeSetupState -Path $fx.State -Change { param($s) $s['ownerOverrides'] = @{ $fx.Config = $untrustedSid } }
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx -Extra @{ Force = $true }) -StatePath $fx.State
        $r.ExitCode | Should Be 2
        $r.LogText | Should Match 'config\.json. already exists and is owned by'

        # Pre-created InstallDir owned by someone else.
        Remove-Item -LiteralPath $fx.Config -Force
        $null = New-Item -ItemType Directory -Force -Path $fx.Install
        Update-FakeSetupState -Path $fx.State -Change { param($s) $s['ownerOverrides'] = @{ $fx.Install = $untrustedSid } }
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx) -StatePath $fx.State -AdminPassword $adminPw
        $r.ExitCode | Should Be 2
        $r.LogText | Should Match 'IDLifecycle. already exists and is owned by'
        @(Get-ChildItem -LiteralPath $fx.Install -Force).Count | Should Be 0
        Remove-SetupFixture -Fixture $fx
    }

    It 'H1: config.json pointing at another scripts_dir/data_dir is refused; -Force corrects only those fields (before the lockdown, which uses data_dir)' {
        $fx = New-SetupFixture
        $null = New-Item -ItemType Directory -Force -Path $fx.Data
        $other = Join-Path $fx.Root 'elsewhere\scripts'
        $cfg = [ordered]@{
            listen     = '127.0.0.1:9666'
            data_dir   = (Join-Path $fx.Root 'otherdata')
            database   = [ordered]@{ driver = 'sqlite'; dsn = 'file:keep.db' }
            powershell = [ordered]@{ exe = 'pwsh.exe'; scripts_dir = $other; timeout_seconds = 600 }
            log_level  = 'debug'
        }
        [System.IO.File]::WriteAllText($fx.Config, (ConvertTo-Json -InputObject $cfg -Depth 5), $script:Utf8NoBom)
        Update-FakeSetupState -Path $fx.State -Change { param($s) $s['admins'] = 1 }

        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx) -StatePath $fx.State
        $r.ExitCode | Should Be 2
        $r.LogText | Should Match 'does not point at this install'
        @(Get-FakeOps -Path $fx.State).Count | Should Be 0

        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx -Extra @{ Force = $true }) -StatePath $fx.State
        $r.ExitCode | Should Be 0
        $r.ReportText | Should Match '"SetConfigPaths","Applied"'
        # Paths first: lockdown restricts the folder named in data_dir.
        $r.ReportText.IndexOf('"SetConfigPaths"') | Should BeLessThan $r.ReportText.IndexOf('"LockdownDataDir"')
        $after = [System.IO.File]::ReadAllText($fx.Config) | ConvertFrom-Json -AsHashtable
        $after['powershell']['scripts_dir'] | Should Be (Join-Path $fx.Install 'scripts')
        $after['data_dir'] | Should Be $fx.Data
        $after['listen'] | Should Be '127.0.0.1:9666'
        $after['log_level'] | Should Be 'debug'
        $after['powershell']['timeout_seconds'] | Should Be 600
        $after['database']['dsn'] | Should Be 'file:keep.db'
        Remove-SetupFixture -Fixture $fx
    }

    It 'H2/M1: InstallDir gets the protected ACL; unknown files in InstallDir\scripts need -Force and are removed, never unblocked' {
        $fx = New-SetupFixture
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx) -StatePath $fx.State -AdminPassword $adminPw
        $r.ExitCode | Should Be 0
        $r.ReportText | Should Match '"LockdownInstallDir","Applied"'
        $acl = @(Get-FakeCalls -Path $fx.State -Op 'installDirAcl')
        $acl.Count | Should Be 1
        $acl[0]['data']['path'] | Should Be $fx.Install
        $acl[0]['data']['account'] | Should Be 'TESTDOM\svc-idl$'
        (Test-Path -LiteralPath (Join-Path $fx.Install 'SHA256SUMS')) | Should Be $true

        # A blocked file in Logs (not part of the release) and a planted, blocked script.
        $evil = Join-Path $fx.Install 'scripts\Evil.ps1'
        $logScript = Join-Path $fx.Install 'scripts\Logs\Old.ps1'
        $null = New-Item -ItemType Directory -Force -Path (Split-Path $logScript)
        foreach ($f in @($evil, $logScript)){
            Set-Content -LiteralPath $f -Value '# planted'
            Set-Content -LiteralPath $f -Stream Zone.Identifier -Value "[ZoneTransfer]`r`nZoneId=3"
        }

        # A release file that is still blocked but matches the manifest is the only one unblocked.
        $known = Join-Path $fx.Install 'scripts\Get-IDLSnapshot.ps1'
        Set-Content -LiteralPath $known -Stream Zone.Identifier -Value "[ZoneTransfer]`r`nZoneId=3"

        Reset-FakeCalls -Path $fx.State
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx) -StatePath $fx.State
        $r.ExitCode | Should Be 2
        $r.LogText | Should Match 'contains file\(s\) this release does not ship: scripts\\Evil\.ps1'
        (Test-Path -LiteralPath $evil) | Should Be $true

        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx -Extra @{ Force = $true }) -StatePath $fx.State
        $r.ExitCode | Should Be 0
        $r.ReportText | Should Match '"RemoveUnknownFiles","Applied"'
        $r.ReportText | Should Match '"UnblockProgramFiles","Applied",.*1 verified release file'
        (Test-Path -LiteralPath $evil) | Should Be $false
        ($null -eq (Get-Item -LiteralPath $known -Stream Zone.Identifier -ErrorAction SilentlyContinue)) | Should Be $true
        ($null -ne (Get-Item -LiteralPath $logScript -Stream Zone.Identifier -ErrorAction SilentlyContinue)) | Should Be $true
        $r.LogText | Should Not Match 'unblocked .*Old\.ps1'
        Remove-SetupFixture -Fixture $fx
    }

    It 'M4: logs go to the protected setup log folder (never scripts\Logs); a junction as log folder is refused' {
        $fx = New-SetupFixture
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx -Extra @{ DryRun = $true }) -StatePath $fx.State
        $r.ExitCode | Should Be 0
        $r.LogText | Should Match ('Setup log folder: ' + [regex]::Escape($r.LogDir))
        @($r.ScriptsLogsNew).Count | Should Be 0

        $target = Join-Path $fx.Root 'logtarget'
        $null = New-Item -ItemType Directory -Force -Path $target
        $link = Join-Path $fx.Root 'loglink'
        $null = New-Item -ItemType Junction -Path $link -Target $target
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx -Extra @{ DryRun = $true }) -StatePath $fx.State -LogDir $link
        $r.ExitCode | Should Be 2
        $r.StdErr | Should Match 'cannot use the log folder .*junction or symbolic link'
        @(Get-ChildItem -LiteralPath $target -Force).Count | Should Be 0
        [System.IO.Directory]::Delete($link, $false)
        Remove-SetupFixture -Fixture $fx
    }

    It 'secrets: a child that echoes the password / DSN (with "/" and "@" in it) on failure is scrubbed everywhere' {
        $fx = New-SetupFixture
        Update-FakeSetupState -Path $fx.State -Change { param($s) $s['echoPassword'] = $true }
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx) -StatePath $fx.State -AdminPassword $adminPw
        $r.ExitCode | Should Be 1
        $r.ReportText | Should Match '"CreateAdminUser","Failed"'
        $r.AllText | Should Match "debug password='\*\*\*'"
        $r.AllText | Should Not Match 'Sentinel-Admin'

        $fx2 = New-SetupFixture
        $dsn = 'postgres://idl:Sentinel/Part2@x@db01/idl?sslmode=require'
        Update-FakeSetupState -Path $fx2.State -Change { param($s) $s['echoDsn'] = $true }
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx2 -Extra @{ DbDriver = 'postgres' }) -StatePath $fx2.State -AdminPassword $adminPw -DbDsn $dsn
        $r.ExitCode | Should Be 1
        $r.ReportText | Should Match '"PrepareDatabase","Failed"'
        $r.AllText | Should Not Match 'Sentinel'
        $r.AllText | Should Not Match 'Part2'
        Remove-SetupFixture -Fixture $fx2
        Remove-SetupFixture -Fixture $fx
    }

    It 'L5/L8: -DbDsnEnvVar must be IDL_*; NT SERVICE accounts must be plain names' {
        $fx = New-SetupFixture
        foreach ($bad in @('PATH', 'idl_lower', 'IDL_', 'IDL-X')){
            $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx -Extra @{ DbDriver = 'mssql'; DbDsnEnvVar = $bad; DryRun = $true }) -StatePath $fx.State
            $r.ExitCode | Should Be 2
        }

        # The names the installer reads secrets from cannot hold the DSN.
        foreach ($reserved in @('IDL_PASSWORD', 'IDL_SERVICE_PASSWORD', 'IDL_SETUP_DB_DSN')){
            $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx -Extra @{ DbDriver = 'mssql'; DbDsnEnvVar = $reserved; DryRun = $true }) -StatePath $fx.State
            $r.ExitCode | Should Be 2
            $r.LogText | Should Match "DbDsnEnvVar '$reserved' is reserved"
        }

        foreach ($bad in @('NT SERVICE\a b', 'NT SERVICE\x\y', 'NT SERVICE\')){
            (Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx -Extra @{ ServiceAccount = $bad; DryRun = $true }) -StatePath $fx.State).ExitCode | Should Be 2
        }

        (Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx -Extra @{ ServiceAccount = 'NT SERVICE\IDLifecycle'; DryRun = $true }) -StatePath $fx.State).ExitCode | Should Be 0
        Remove-SetupFixture -Fixture $fx
    }

    It 'L7: the test hook is refused without IDL_SETUP_TEST=1 or from outside tests\powershell (nothing happens)' {
        $fx = New-SetupFixture
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx -Extra @{ DryRun = $true }) -StatePath $fx.State -NoTestEnv
        $r.ExitCode | Should Be 2
        $r.StdErr | Should Match '-TestHook refused \(IDL_SETUP_TEST is not set to 1\)'

        $copy = Join-Path $fx.Root 'SetupFakes.ps1'
        Copy-Item -LiteralPath $script:SetupHook -Destination $copy
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx -Extra @{ DryRun = $true }) -StatePath $fx.State -HookPath $copy
        $r.ExitCode | Should Be 2
        $r.StdErr | Should Match 'the hook must be a file inside'
        @(Get-FakeOps -Path $fx.State).Count | Should Be 0
        Remove-SetupFixture -Fixture $fx
    }

    It 'L3: a failed upgrade restarts the service it stopped; a -Force DSN change restarts a running service' {
        $fx = New-SetupFixture
        (Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx) -StatePath $fx.State -AdminPassword $adminPw).ExitCode | Should Be 0
        [System.IO.File]::WriteAllText($fx.Exe, 'FAKE-IDLIFECYCLED 1.1.0', $script:Utf8NoBom)
        Write-ReleaseManifest -Root $fx.Source
        Update-FakeSetupState -Path $fx.State -Change { param($s) $s['checkFail'] = @{ scripts = 'boom' }; $s['calls'] = @() }
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-BaseParams -Fx $fx -Extra @{ Force = $true }) -StatePath $fx.State
        $r.ExitCode | Should Be 1
        $ops = @(Get-FakeOps -Path $fx.State)
        [array]::IndexOf($ops, 'stop') | Should BeLessThan ([array]::LastIndexOf($ops, 'start'))
        $r.LogText | Should Match 'Restarted service .* that this run had stopped'
        (Read-FakeSetupState -Path $fx.State)['services']['IDLSetupTest']['State'] | Should Be 'Running'

        $fx2 = New-SetupFixture
        $p = Get-BaseParams -Fx $fx2 -Extra @{ DbDriver = 'mssql' }
        (Invoke-SetupScript -Script 'Install-IDLifecycle' -Params $p -StatePath $fx2.State -AdminPassword $adminPw -DbDsn 'sqlserver://a:Sentinel-One-Pw@h?database=d').ExitCode | Should Be 0
        Reset-FakeCalls -Path $fx2.State
        $p['Force'] = $true
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params $p -StatePath $fx2.State -DbDsn 'sqlserver://a:Sentinel-Two-Pw@h?database=d'
        $r.ExitCode | Should Be 0
        $r.ReportText | Should Match '"SetConfigDsn","Applied"'
        $r.ReportText | Should Match '"RestartService","Applied"'
        $r.AllText | Should Not Match 'Sentinel-(One|Two)'
        Remove-SetupFixture -Fixture $fx2
        Remove-SetupFixture -Fixture $fx
    }
}

Describe 'Install-IDLifecycle.ps1 sources: build from GitHub, release zip (download, go build and hand-off are faked)' {

    $adminPw = 'Sentinel-Admin-Pw-7Q2x'
    $token = 'Sentinel-GitHub-Token-4Kd'

    function Get-SourceParams {
        param($Fx, [hashtable]$Extra = @{})

        $p = @{ InstallDir = $Fx.Install; DataDir = $Fx.Data; ServiceAccount = 'TESTDOM\svc-idl$'; ServiceName = 'IDLSourceTest' }
        foreach ($k in $Extra.Keys){
            $p[$k] = $Extra[$k]
        }

        return $p
    }

    function New-GitHubFixture {
        # A setup fixture plus a folder that stands for the repository as GitHub zips it (owner-repo-sha\...).
        $fx = New-SetupFixture
        $repo = Join-Path $fx.Root 'gh\mumbles27-IDLifecycle-0123456'
        $null = New-Item -ItemType Directory -Force -Path (Join-Path $repo 'scripts'), (Join-Path $repo 'cmd\idlifecycled')
        Get-ChildItem -LiteralPath $script:ScriptsDir -File | Copy-Item -Destination (Join-Path $repo 'scripts')
        Set-Content -LiteralPath (Join-Path $repo 'go.mod') -Value 'module idlifecycle'
        Set-Content -LiteralPath (Join-Path $repo 'cmd\idlifecycled\main.go') -Value 'package main'
        Copy-Item -LiteralPath (Join-Path $script:RepoRoot 'Setup.cmd') -Destination $repo
        Update-FakeSetupState -Path $fx.State -Change { param($s) $s['githubSource'] = $repo }
        return $fx
    }

    function Get-HandOff {
        param([string]$StatePath)

        $calls = @(Get-FakeCalls -Path $StatePath -Op 'handOff')
        if ($calls.Count -eq 0){
            return $null
        }

        $d = $calls[0]['data']
        $a = @($d['args'])
        return @{ Data = $d; Args = $a; Source = $a[[array]::IndexOf($a, '-SourceDir') + 1] }
    }

    It 'FromGitHub: downloads the ref, builds, lays out a release the installer trusts, hands over with the same parameters, removes staging' {
        $fx = New-GitHubFixture
        $p = Get-SourceParams -Fx $fx -Extra @{ FromGitHub = $true; GitHubRef = 'v1.2.0'; DryRun = $true; Start = $false; Force = $true }
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params $p -StatePath $fx.State -AdminPassword $adminPw -DbDsn 'sqlserver://u:Sentinel-Dsn-Pw-9Zk@h?database=d' -ExtraEnv @{ IDL_GITHUB_TOKEN = $token }

        $r.ExitCode | Should Be 0
        ((Get-FakeOps -Path $fx.State) -join ',') | Should Be 'githubDownload,goBuild,handOff'
        $dl = @(Get-FakeCalls -Path $fx.State -Op 'githubDownload')[0]['data']
        $dl['repo'] | Should Be 'mumbles27/IDLifecycle'
        $dl['ref'] | Should Be 'v1.2.0'
        $dl['tokenLength'] | Should Be $token.Length
        $r.LogText | Should Match 'Downloaded mumbles27/IDLifecycle@v1\.2\.0 \(commit 0123456789abcdef'
        $r.LogText | Should Match 'Go found: go1\.27\.3'

        $h = Get-HandOff -StatePath $fx.State
        $h.Data['exeText'] | Should Be 'FAKE-IDLIFECYCLED v1.2.0-0123456'
        @($h.Data['releaseFiles']) -contains 'Setup.cmd' | Should Be $true
        @($h.Data['releaseFiles']) -contains 'scripts\Install-IDLifecycle.ps1' | Should Be $true
        $h.Data['script'] | Should Be (Join-Path $h.Source 'scripts\Install-IDLifecycle.ps1')
        foreach ($expected in @('-DryRun', '-Start:$false', '-Force', '-ServiceAccount', 'TESTDOM\svc-idl$', '-ServiceName', 'IDLSourceTest', '-InstallDir', $fx.Install)){
            $h.Args -contains $expected | Should Be $true
        }

        foreach ($absent in @('-FromGitHub', '-GitHubRef', '-TestHook', '-AdminPassword', '-DbDsn')){
            $h.Args -contains $absent | Should Be $false
        }

        # Secrets go to the staged installer only in its environment; the token is never passed on.
        $h.Data['secretLengths']['IDL_PASSWORD'] | Should Be $adminPw.Length
        $h.Data['secretLengths']['IDL_SETUP_DB_DSN'] | Should Be 'sqlserver://u:Sentinel-Dsn-Pw-9Zk@h?database=d'.Length
        $h.Data['secretLengths'].Contains('IDL_GITHUB_TOKEN') | Should Be $false
        $r.AllText | Should Not Match 'Sentinel'
        (ConvertTo-Json -InputObject @((Read-FakeSetupState -Path $fx.State)['calls']) -Depth 10) | Should Not Match 'Sentinel'

        $r.ReportText | Should Match '"DownloadSource","Applied"'
        $r.ReportText | Should Match '"BuildRelease","Applied"'
        $r.ReportText | Should Match '"HandOff","Applied"'
        (Test-Path -LiteralPath (Split-Path -Parent $h.Source)) | Should Be $false
        Remove-SetupFixture -Fixture $fx
    }

    It 'FromGitHub: no Go (exit 3), a failed download or build (exit 3) and bad input (exit 2) stop before anything is installed' {
        $fx = New-GitHubFixture
        $p = Get-SourceParams -Fx $fx -Extra @{ FromGitHub = $true; DryRun = $true }

        Update-FakeSetupState -Path $fx.State -Change { param($s) $s['goVersion'] = '' }
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params $p -StatePath $fx.State
        $r.ExitCode | Should Be 3
        $r.LogText | Should Match 'Go was not found'
        @(Get-FakeOps -Path $fx.State).Count | Should Be 0

        Update-FakeSetupState -Path $fx.State -Change { param($s) $s['goVersion'] = 'go1.27.3'; $s['githubFail'] = 'GitHub download of x failed: repository or ref not found - a private repository needs -GitHubToken / IDL_GITHUB_TOKEN' }
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params $p -StatePath $fx.State
        $r.ExitCode | Should Be 3
        $r.LogText | Should Match 'private repository needs -GitHubToken'
        @(Get-FakeOps -Path $fx.State) -contains 'handOff' | Should Be $false

        Update-FakeSetupState -Path $fx.State -Change { param($s) $s['githubFail'] = ''; $s['goBuildFail'] = $true; $s['calls'] = @() }
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params $p -StatePath $fx.State
        $r.ExitCode | Should Be 3
        $r.LogText | Should Match 'go build failed'
        ((Get-FakeOps -Path $fx.State) -join ',') | Should Be 'githubDownload,goBuild'

        Update-FakeSetupState -Path $fx.State -Change { param($s) $s['goBuildFail'] = $false; $s['calls'] = @() }
        foreach ($bad in @(@{ GitHubRepo = 'not a repo' }, @{ GitHubRef = '../etc' }, @{ SourceDir = $fx.Source })){
            $q = Get-SourceParams -Fx $fx -Extra (@{ FromGitHub = $true; DryRun = $true } + $bad)
            (Invoke-SetupScript -Script 'Install-IDLifecycle' -Params $q -StatePath $fx.State).ExitCode | Should Be 2
        }

        $q = Get-SourceParams -Fx $fx -Extra @{ FromGitHub = $true; GitHubRepo = 'https://github.com/Other-Org/IDL.fork.git'; DryRun = $true }
        (Invoke-SetupScript -Script 'Install-IDLifecycle' -Params $q -StatePath $fx.State).ExitCode | Should Be 0
        @(Get-FakeCalls -Path $fx.State -Op 'githubDownload')[0]['data']['repo'] | Should Be 'Other-Org/IDL.fork'

        # A real run needs elevation, checked before anything is downloaded.
        Update-FakeSetupState -Path $fx.State -Change { param($s) $s['elevated'] = $false; $s['calls'] = @() }
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-SourceParams -Fx $fx -Extra @{ FromGitHub = $true }) -StatePath $fx.State -AdminPassword $adminPw
        $r.ExitCode | Should Be 4
        @(Get-FakeOps -Path $fx.State).Count | Should Be 0
        Remove-SetupFixture -Fixture $fx
    }

    It 'the staged installer''s exit code is returned; on failure the staging folder is kept for inspection' {
        $fx = New-GitHubFixture
        Update-FakeSetupState -Path $fx.State -Change { param($s) $s['handOffExit'] = 1 }
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-SourceParams -Fx $fx -Extra @{ FromGitHub = $true; DryRun = $true }) -StatePath $fx.State
        $r.ExitCode | Should Be 1
        $r.LogText | Should Match 'staged installer finished with exit code 1'
        $r.LogText | Should Match 'Staging folder kept for inspection'
        $h = Get-HandOff -StatePath $fx.State
        (Test-Path -LiteralPath $h.Source) | Should Be $true
        Remove-Item -LiteralPath (Split-Path -Parent $h.Source) -Recurse -Force
        Remove-SetupFixture -Fixture $fx
    }

    It 'release zip: extracted into staging and handed over; a zip that is not a release, has an unsafe entry or sits where others can write is refused (exit 2)' {
        $fx = New-SetupFixture
        Set-Content -LiteralPath (Join-Path $fx.Source 'Setup.cmd') -Value '@echo launcher'
        Write-ReleaseManifest -Root $fx.Source
        $zipDir = Join-Path $fx.Root 'zips'
        $null = New-Item -ItemType Directory -Path $zipDir
        $zip = Join-Path $zipDir 'IDLifecycle-1.0.0.zip'
        Compress-Archive -LiteralPath $fx.Source -DestinationPath $zip
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-SourceParams -Fx $fx -Extra @{ SourceDir = $zip; DryRun = $true }) -StatePath $fx.State
        $r.ExitCode | Should Be 0
        @(Get-FakeOps -Path $fx.State) -contains 'githubDownload' | Should Be $false
        $h = Get-HandOff -StatePath $fx.State
        $h.Data['exeText'] | Should Be 'FAKE-IDLIFECYCLED 1.0.0'
        @($h.Data['releaseFiles']) -contains 'Setup.cmd' | Should Be $true
        $r.LogText | Should Match 'Extracted .*IDLifecycle-1\.0\.0\.zip \(SHA256 [0-9A-F]{64}\)'

        $notRelease = Join-Path $zipDir 'random.zip'
        Compress-Archive -LiteralPath (Join-Path $fx.Source 'scripts') -DestinationPath $notRelease
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-SourceParams -Fx $fx -Extra @{ SourceDir = $notRelease; DryRun = $true }) -StatePath $fx.State
        $r.ExitCode | Should Be 2
        $r.LogText | Should Match 'not an IDLifecycle release zip'

        Add-Type -AssemblyName System.IO.Compression, System.IO.Compression.FileSystem
        $evil = Join-Path $zipDir 'evil.zip'
        $archive = [System.IO.Compression.ZipFile]::Open($evil, [System.IO.Compression.ZipArchiveMode]::Create)
        $w = [System.IO.StreamWriter]::new($archive.CreateEntry('../escaped.txt').Open())
        $w.Write('x')
        $w.Dispose()
        $archive.Dispose()
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-SourceParams -Fx $fx -Extra @{ SourceDir = $evil; DryRun = $true }) -StatePath $fx.State
        $r.ExitCode | Should Be 2
        $r.LogText | Should Match 'unsafe entry'

        Grant-TestAce -Path $zipDir -Sid 'S-1-5-32-545' -Rights 'Modify'
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-SourceParams -Fx $fx -Extra @{ SourceDir = $zip; DryRun = $true }) -StatePath $fx.State
        $r.ExitCode | Should Be 2
        $r.LogText | Should Match 'is not trusted'
        Remove-SetupFixture -Fixture $fx
    }

    It 'SkipIntegrityCheck: a zip without SHA256SUMS (one top folder, like GitHub "Download ZIP") is handed over with the flag; -FromGitHub drops it' {
        $fx = New-SetupFixture
        Remove-Item -LiteralPath (Join-Path $fx.Source 'SHA256SUMS') -Force
        $zipDir = Join-Path $fx.Root 'zips'
        $null = New-Item -ItemType Directory -Path $zipDir
        $wrap = Join-Path $fx.Root 'wrap\IDLifecycle-main'
        $null = New-Item -ItemType Directory -Force -Path $wrap
        Copy-Item -Path (Join-Path $fx.Source '*') -Destination $wrap -Recurse
        $zip = Join-Path $zipDir 'IDLifecycle-main.zip'
        Compress-Archive -LiteralPath $wrap -DestinationPath $zip

        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-SourceParams -Fx $fx -Extra @{ SourceDir = $zip; DryRun = $true }) -StatePath $fx.State
        $r.ExitCode | Should Be 2
        $r.LogText | Should Match 'not an IDLifecycle release zip \(no SHA256SUMS\).*-SkipIntegrityCheck'

        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-SourceParams -Fx $fx -Extra @{ SourceDir = $zip; DryRun = $true; SkipIntegrityCheck = $true }) -StatePath $fx.State
        $r.ExitCode | Should Be 0
        $h = Get-HandOff -StatePath $fx.State
        (Split-Path -Leaf $h.Source) | Should Be 'IDLifecycle-main'
        $h.Args -contains '-SkipIntegrityCheck' | Should Be $true
        Remove-SetupFixture -Fixture $fx

        $fx = New-GitHubFixture
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params (Get-SourceParams -Fx $fx -Extra @{ FromGitHub = $true; DryRun = $true; SkipIntegrityCheck = $true }) -StatePath $fx.State -ExtraEnv @{ IDL_GITHUB_TOKEN = $token }
        $r.ExitCode | Should Be 0
        (Get-HandOff -StatePath $fx.State).Args -contains '-SkipIntegrityCheck' | Should Be $false
        $r.LogText | Should Match 'not needed with -FromGitHub'
        Remove-SetupFixture -Fixture $fx
    }
}

Describe 'New-IDLifecycleRelease.ps1 (temp folders only)' {

    It 'builds a restricted release folder with SHA256SUMS that the installer accepts; refuses a non-empty OutDir' {
        $root = Join-Path $script:StateDir ('rel_' + [guid]::NewGuid().ToString('N').Substring(0, 8))
        $null = New-Item -ItemType Directory -Force -Path $root
        $fakeExe = Join-Path $root 'built.exe'
        [System.IO.File]::WriteAllText($fakeExe, 'FAKE-IDLIFECYCLED 2.0.0', $script:Utf8NoBom)
        $out = Join-Path $root 'IDLifecycle-2.0.0'
        $logs = Join-Path $root 'logs'

        $psi = New-ChildProcessInfo -Arguments @('-NoProfile', '-NonInteractive', '-ExecutionPolicy', 'Bypass', '-File', (Join-Path $script:ScriptsDir 'New-IDLifecycleRelease.ps1'), '-OutDir', $out, '-ExePath', $fakeExe, '-LogDir', $logs, '-Zip') -StatePath (New-StubStateFile)
        $r = Invoke-ChildProcess -Psi $psi -TimeoutSec 120
        $r.ExitCode | Should Be 0
        # -Zip: the release folder (with its name) in <OutDir>.zip.
        Add-Type -AssemblyName System.IO.Compression.FileSystem
        $zipFile = [System.IO.Compression.ZipFile]::OpenRead("$out.zip")
        $entries = @($zipFile.Entries | ForEach-Object { $_.FullName.Replace('\', '/') })
        $zipFile.Dispose()
        $entries -contains 'IDLifecycle-2.0.0/Setup.cmd' | Should Be $true
        $entries -contains 'IDLifecycle-2.0.0/SHA256SUMS' | Should Be $true
        (Test-Path -LiteralPath (Join-Path $out 'idlifecycled.exe')) | Should Be $true
        $manifest = [System.IO.File]::ReadAllText((Join-Path $out 'SHA256SUMS'))
        $manifest | Should Match '  idlifecycled\.exe'
        $manifest | Should Match '  scripts/Install-IDLifecycle\.ps1'
        $manifest | Should Match '  scripts/Start-IDLifecycleSetup\.ps1'
        $manifest | Should Match '  Setup\.cmd'
        $manifest | Should Not Match 'Logs/'
        [System.IO.File]::ReadAllText((Join-Path $out 'Setup.cmd')) | Should Match 'Start-IDLifecycleSetup\.ps1'
        $acl = [System.IO.FileSystemAclExtensions]::GetAccessControl([System.IO.DirectoryInfo]::new($out), [System.Security.AccessControl.AccessControlSections]::Access)
        $acl.AreAccessRulesProtected | Should Be $true
        @($acl.GetAccessRules($true, $true, [System.Security.Principal.SecurityIdentifier]) | ForEach-Object { $_.IdentityReference.Value }) -contains 'S-1-5-32-545' | Should Be $false

        # The installer accepts it as SourceDir.
        $state = New-FakeSetupState -Path (Join-Path $root 'fake.json')
        $p = @{ SourceDir = $out; InstallDir = (Join-Path $root 'inst'); DataDir = (Join-Path $root 'data'); ServiceAccount = 'TESTDOM\svc-idl$'; ServiceName = 'IDLRelTest'; DryRun = $true }
        $ir = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params $p -StatePath $state
        $ir.ExitCode | Should Be 0
        $ir.LogText | Should Match 'match SHA256SUMS'

        # A second run into the same (non-empty) folder is refused.
        $psi = New-ChildProcessInfo -Arguments @('-NoProfile', '-NonInteractive', '-ExecutionPolicy', 'Bypass', '-File', (Join-Path $script:ScriptsDir 'New-IDLifecycleRelease.ps1'), '-OutDir', $out, '-ExePath', $fakeExe, '-LogDir', $logs) -StatePath (New-StubStateFile)
        (Invoke-ChildProcess -Psi $psi -TimeoutSec 120).ExitCode | Should Be 2
        Remove-Item -LiteralPath $root -Recurse -Force -ErrorAction SilentlyContinue
    }
}

Describe 'Install-IDLifecycle.ps1 end-to-end with a freshly built idlifecycled.exe (temp data dir, sqlite, loopback, no service)' {

    # Built once into a temp folder. Skipped (not failed) when Go is missing or the tree does not compile right now.
    $buildDir = Join-Path $script:StateDir ('gobuild_' + [guid]::NewGuid().ToString('N').Substring(0, 8))
    $builtExe = Join-Path $buildDir 'idlifecycled.exe'
    $built = $false
    if (Get-Command -Name go -CommandType Application -ErrorAction SilentlyContinue){
        $null = New-Item -ItemType Directory -Force -Path $buildDir
        Push-Location $script:RepoRoot
        try{
            $out = & go build -o $builtExe ./cmd/idlifecycled 2>&1
            $built = (($LASTEXITCODE -eq 0) -and (Test-Path -LiteralPath $builtExe))
            if (-not $built){
                Write-Warning "go build failed; skipping the end-to-end test: $($out -join ' ')"
            }
        }
        finally{
            Pop-Location
        }
    }

    It 'init + lockdown + user add + check pass for real; a re-run skips every step' -Skip:(-not $built) {
        $pw = 'Sentinel-E2E-Pw-4Hd9'
        $fx = New-SetupFixture -ExeSource $builtExe
        $p = @{
            SourceDir      = $fx.Source
            InstallDir     = $fx.Install
            DataDir        = $fx.Data
            ServiceAccount = 'NT AUTHORITY\NetworkService'
            ServiceName    = 'IDLSetupE2E'
        }
        $realDaemon = @{ IDL_FAKE_REAL_DAEMON = '1' }
        $r = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params $p -StatePath $fx.State -AdminPassword $pw -ExtraEnv $realDaemon

        $r.ExitCode | Should Be 0
        $r.AllText | Should Not Match ([regex]::Escape($pw))
        foreach ($name in @('config', 'data_dir_acl', 'database', 'admin_user', 'powershell', 'scripts', 'scripts_unblocked')){
            $r.LogText | Should Match ("  $name\s+ok\s")
        }

        $r.ReportText | Should Match '"InitConfig","Applied"'
        $r.ReportText | Should Match '"CreateAdminUser","Applied"'
        $cfg = [System.IO.File]::ReadAllText($fx.Config) | ConvertFrom-Json -AsHashtable
        $cfg['listen'] | Should Be '127.0.0.1:8443'
        $cfg['powershell']['scripts_dir'] | Should Be (Join-Path $fx.Install 'scripts')
        (Test-Path -LiteralPath (Join-Path $fx.Data 'idlifecycle.db')) | Should Be $true

        $r2 = Invoke-SetupScript -Script 'Install-IDLifecycle' -Params $p -StatePath $fx.State -ExtraEnv $realDaemon
        $r2.ExitCode | Should Be 0
        $r2.ReportText | Should Not Match '"Applied"'
        $r2.ReportText | Should Match '"InitConfig","Skipped"'
        $r2.ReportText | Should Match '"LockdownDataDir","Skipped"'
        $r2.ReportText | Should Match '"CreateAdminUser","Skipped"'
        Remove-SetupFixture -Fixture $fx
    }

    if (Test-Path -LiteralPath $buildDir){
        Remove-Item -LiteralPath $buildDir -Recurse -Force -ErrorAction SilentlyContinue
    }
}
