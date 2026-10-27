$here = Split-Path -Parent $MyInvocation.MyCommand.Path
. (Join-Path $here 'TestHelpers.ps1')
. (Join-Path $here 'SetupTestHelpers.ps1')

# The tray's elevated helper, run as the tray starts it (minus UAC) with SetupFakes.ps1: services, elevation and
# idlifecycled (FakeDaemon.ps1: config get / config set) are fakes; the "installation" is a temp folder given to the
# helper through IDL_TRAY_TEST_INSTALLDIR (honoured only together with the test hook).

Describe 'Invoke-IDLTrayAction.ps1 (fakes; temp folders only)' {

    function New-TrayFixture {
        param([hashtable]$Config = @{}, [string]$State = 'Running', [string]$PathName = '')

        $fx = New-SetupFixture
        $null = New-Item -ItemType Directory -Force -Path $fx.Install, $fx.Data
        [System.IO.File]::WriteAllText((Join-Path $fx.Install 'idlifecycled.exe'), 'FAKE-IDLIFECYCLED 1.0.0', $script:Utf8NoBom)
        $cfg = [ordered]@{
            listen     = '127.0.0.1:8443'
            tls        = @{ cert_file = ''; key_file = '' }
            data_dir   = $fx.Data
            log_level  = 'info'
            database   = @{ driver = 'sqlite'; dsn = 'file:x.db' }
            powershell = @{ exe = 'pwsh.exe'; scripts_dir = (Join-Path $fx.Install 'scripts'); execution_policy = 'RemoteSigned' }
        }
        foreach ($k in $Config.Keys){
            $cfg[$k] = $Config[$k]
        }

        [System.IO.File]::WriteAllText($fx.Config, (ConvertTo-Json -InputObject $cfg -Depth 5), $script:Utf8NoBom)
        $bin = (($PathName) ? ($PathName) : ('"{0}" service -config "{1}"' -f (Join-Path $fx.Install 'idlifecycled.exe'), $fx.Config))
        Update-FakeSetupState -Path $fx.State -Change {
            param($s)
            $s['services'] = @{ IDLTrayTest = @{ State = $State; PathName = $bin; StartName = 'NT SERVICE\IDLTrayTest' } }
        }

        $info = [ordered]@{
            service_name = 'IDLTrayTest'; console_url = 'http://localhost:8443/'; listen = '127.0.0.1:8443'; tls = $false; log_level = 'info'
            data_dir = $fx.Data; install_dir = $fx.Install; config_path = $fx.Config; version = '1.0.0'; execution_policy = 'RemoteSigned'
        }
        Import-Module (Join-Path $script:ScriptsDir 'IDLifecycle.Common.psm1') -Force
        Write-IDLTrayInfo -Path (Join-Path $fx.Install 'tray.json') -Info $info
        return $fx
    }

    function Invoke-Tray {
        param($Fx, [hashtable]$Params)

        return (Invoke-SetupScript -Script 'Invoke-IDLTrayAction' -Params $Params -StatePath $Fx.State -ExtraEnv @{ IDL_TRAY_TEST_INSTALLDIR = $Fx.Install })
    }

    function New-ResultPath {
        return (Join-Path ([System.IO.Path]::GetTempPath()) ('IDLTray_{0}.json' -f [guid]::NewGuid().ToString('N')))
    }

    It 'restarts, stops and starts this installation''s service and writes the result file' {
        $fx = New-TrayFixture
        $result = New-ResultPath
        $r = Invoke-Tray -Fx $fx -Params @{ Action = 'Restart'; ServiceName = 'IDLTrayTest'; ResultFile = $result }
        $r.ExitCode | Should Be 0
        ((Get-FakeOps -Path $fx.State) -join ',') | Should Be 'stop,start'
        $doc = [System.IO.File]::ReadAllText($result) | ConvertFrom-Json -AsHashtable
        $doc['ok'] | Should Be $true
        $doc['state'] | Should Be 'Running'
        $doc['message'] | Should Match 'restarted'
        $r.ReportText | Should Match '"RestartService","Applied"'
        Remove-Item -LiteralPath $result -Force

        Reset-FakeCalls -Path $fx.State
        (Invoke-Tray -Fx $fx -Params @{ Action = 'Stop'; ServiceName = 'IDLTrayTest' }).ExitCode | Should Be 0
        (Invoke-Tray -Fx $fx -Params @{ Action = 'Stop'; ServiceName = 'IDLTrayTest' }).ReportText | Should Match '"StopService","Skipped"'
        (Invoke-Tray -Fx $fx -Params @{ Action = 'Start'; ServiceName = 'IDLTrayTest' }).ExitCode | Should Be 0
        ((Get-FakeOps -Path $fx.State) -join ',') | Should Be 'stop,start'

        # A service that does not get there is a failure (exit 1), reported in the result.
        Update-FakeSetupState -Path $fx.State -Change { param($s) $s['startState'] = 'Stopped' }
        $result = New-ResultPath
        $r = Invoke-Tray -Fx $fx -Params @{ Action = 'Restart'; ServiceName = 'IDLTrayTest'; ResultFile = $result }
        $r.ExitCode | Should Be 1
        ([System.IO.File]::ReadAllText($result) | ConvertFrom-Json -AsHashtable)['ok'] | Should Be $false
        Remove-Item -LiteralPath $result -Force
        Remove-SetupFixture -Fixture $fx
    } -Skip:$false

    It 'refuses without elevation (exit 4), another service (exit 2) and a result file outside the temp folder (exit 2)' {
        $fx = New-TrayFixture
        Update-FakeSetupState -Path $fx.State -Change { param($s) $s['elevated'] = $false }
        $result = New-ResultPath
        $r = Invoke-Tray -Fx $fx -Params @{ Action = 'Restart'; ServiceName = 'IDLTrayTest'; ResultFile = $result }
        $r.ExitCode | Should Be 4
        ([System.IO.File]::ReadAllText($result) | ConvertFrom-Json -AsHashtable)['message'] | Should Match 'administrator'
        Remove-Item -LiteralPath $result -Force
        Update-FakeSetupState -Path $fx.State -Change { param($s) $s['elevated'] = $true }

        $r = Invoke-Tray -Fx $fx -Params @{ Action = 'Restart'; ServiceName = 'Spooler' }
        $r.ExitCode | Should Be 2
        $r.LogText | Should Match "Service 'Spooler' does not exist"

        $r = Invoke-Tray -Fx $fx -Params @{ Action = 'Restart'; ServiceName = 'bad name;x' }
        $r.ExitCode | Should Be 2

        foreach ($bad in @((Join-Path $fx.Root 'IDLTray_0123456789abcdef0123456789abcdef.json'), (Join-Path ([System.IO.Path]::GetTempPath()) 'evil.json'))){
            $r = Invoke-Tray -Fx $fx -Params @{ Action = 'Restart'; ServiceName = 'IDLTrayTest'; ResultFile = $bad }
            $r.ExitCode | Should Be 2
            $r.LogText | Should Match 'ResultFile .* refused'
            (Test-Path -LiteralPath $bad) | Should Be $false
        }

        @(Get-FakeOps -Path $fx.State) -contains 'stop' | Should Be $false
        Remove-SetupFixture -Fixture $fx

        # A service whose program is not this installation's idlifecycled.exe is never touched.
        $fx = New-TrayFixture -PathName '"C:\Other\idlifecycled.exe" service -config "C:\Other\config.json"'
        $r = Invoke-Tray -Fx $fx -Params @{ Action = 'Stop'; ServiceName = 'IDLTrayTest' }
        $r.ExitCode | Should Be 2
        $r.LogText | Should Match 'is not the IDLifecycle service of this installation'
        @(Get-FakeOps -Path $fx.State) -contains 'stop' | Should Be $false
        Remove-SetupFixture -Fixture $fx
    }

    It 'SetConfig: config set with only the given fields, tray.json follows, restart only when asked' {
        $fx = New-TrayFixture
        $cert = Join-Path $fx.Root 'idl.pem'
        $key = Join-Path $fx.Root 'idl.key'
        Set-Content -LiteralPath $cert, $key -Value 'x'
        $result = New-ResultPath
        $r = Invoke-Tray -Fx $fx -Params @{ Action = 'SetConfig'; ServiceName = 'IDLTrayTest'; Listen = '0.0.0.0:9443'; TlsCertFile = $cert; TlsKeyFile = $key; Restart = $true; ResultFile = $result }
        $r.ExitCode | Should Be 0
        $set = @(Get-FakeCalls -Path $fx.State -Op 'daemon:config set')[0]['data']['args']
        ($set -join ' ') | Should Be ("config set -config $($fx.Config) -listen 0.0.0.0:9443 -tls-cert $cert -tls-key $key")
        $cfg = [System.IO.File]::ReadAllText($fx.Config) | ConvertFrom-Json -AsHashtable
        $cfg['listen'] | Should Be '0.0.0.0:9443'
        $cfg['log_level'] | Should Be 'info'
        $tray = Read-IDLTrayInfo -Path (Join-Path $fx.Install 'tray.json')
        $tray['console_url'] | Should Be 'https://server.test:9443/'
        $tray['tls'] | Should Be $true
        $tray['version'] | Should Be '1.0.0'
        $tray['execution_policy'] | Should Be 'RemoteSigned'
        ((Get-FakeOps -Path $fx.State | Where-Object { $_ -in @('stop', 'start') }) -join ',') | Should Be 'stop,start'
        ([System.IO.File]::ReadAllText($result) | ConvertFrom-Json -AsHashtable)['message'] | Should Match 'restarted'
        Remove-Item -LiteralPath $result -Force

        Reset-FakeCalls -Path $fx.State
        $r = Invoke-Tray -Fx $fx -Params @{ Action = 'SetConfig'; ServiceName = 'IDLTrayTest'; LogLevel = 'debug' }
        $r.ExitCode | Should Be 0
        (@(Get-FakeCalls -Path $fx.State -Op 'daemon:config set')[0]['data']['args'] -join ' ') | Should Be "config set -config $($fx.Config) -log-level debug"
        @(Get-FakeOps -Path $fx.State) -contains 'stop' | Should Be $false
        $r.LogText | Should Match 'take effect when the service is restarted'
        (Read-IDLTrayInfo -Path (Join-Path $fx.Install 'tray.json'))['log_level'] | Should Be 'debug'
        Remove-SetupFixture -Fixture $fx
    }

    It 'SetConfig refuses invalid settings and another config.json before anything is written (exit 2)' {
        $fx = New-TrayFixture
        $before = [System.IO.File]::ReadAllText($fx.Config)
        foreach ($p in @(
                @{ Listen = '0.0.0.0:8443' },
                @{ Listen = 'not an address' },
                @{ Listen = '' },
                @{ LogLevel = 'chatty' },
                @{ TlsCertFile = (Join-Path $fx.Root 'missing.pem'); TlsKeyFile = (Join-Path $fx.Root 'missing.key') },
                @{ ConfigPath = (Join-Path $fx.Root 'other\config.json'); LogLevel = 'warn' }
            )){
            $params = @{ Action = 'SetConfig'; ServiceName = 'IDLTrayTest' }
            foreach ($k in $p.Keys){
                $params[$k] = $p[$k]
            }

            $r = Invoke-Tray -Fx $fx -Params $params
            $r.ExitCode | Should Be 2
        }

        @(Get-FakeCalls -Path $fx.State -Op 'daemon:config set').Count | Should Be 0
        [System.IO.File]::ReadAllText($fx.Config) | Should Be $before

        # config set's own refusal (exit 2) is passed on; nothing is restarted.
        Update-FakeSetupState -Path $fx.State -Change { param($s) $s['configSetExit'] = 2 }
        $r = Invoke-Tray -Fx $fx -Params @{ Action = 'SetConfig'; ServiceName = 'IDLTrayTest'; LogLevel = 'warn'; Restart = $true }
        $r.ExitCode | Should Be 2
        $r.LogText | Should Match 'config set refused'
        @(Get-FakeOps -Path $fx.State) -contains 'stop' | Should Be $false
        Remove-SetupFixture -Fixture $fx
    }

    It 'the test hook is refused without IDL_SETUP_TEST=1 (and the test install dir is then ignored)' {
        $fx = New-TrayFixture
        $r = Invoke-SetupScript -Script 'Invoke-IDLTrayAction' -Params @{ Action = 'Restart'; ServiceName = 'IDLTrayTest' } -StatePath $fx.State -NoTestEnv -ExtraEnv @{ IDL_TRAY_TEST_INSTALLDIR = $fx.Install }
        $r.ExitCode | Should Be 2
        $r.StdErr | Should Match 'TestHook refused'
        @(Get-FakeOps -Path $fx.State).Count | Should Be 0
        Remove-SetupFixture -Fixture $fx
    }
}
