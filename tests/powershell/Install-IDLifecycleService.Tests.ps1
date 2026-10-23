$here = Split-Path -Parent $MyInvocation.MyCommand.Path
. (Join-Path $here 'TestHelpers.ps1')

Describe 'Install-IDLifecycleService.ps1 (DryRun smoke test only; never registers a service)' {

    function New-InstallFixture {
        $dir = Join-Path $script:StateDir ('install_' + [guid]::NewGuid().ToString('N'))
        $null = New-Item -ItemType Directory -Force -Path $dir
        $exe = Join-Path $dir 'idlifecycled.exe'
        Set-Content -LiteralPath $exe -Value 'not a real binary'
        $data = Join-Path $dir 'data'
        $cfg = Join-Path $dir 'config.json'
        $json = ConvertTo-Json -InputObject @{ data_dir = $data; powershell = @{ scripts_dir = $script:ScriptsDir } } -Compress
        [System.IO.File]::WriteAllText($cfg, $json, $script:Utf8NoBom)
        return @{ Dir = $dir; Exe = $exe; Cfg = $cfg; Data = $data }
    }

    $serviceName = 'IDLifecycleDryRunTest'

    It 'DryRun logs WOULD: for every step and changes nothing (no dirs, no service)' {
        $fx = New-InstallFixture
        $state = New-StubStateFile
        $logsExisted = Test-Path -LiteralPath $script:LogsDir
        $args1 = @('-BinaryPath', $fx.Exe, '-ConfigPath', $fx.Cfg, '-ServiceAccount', 'TESTDOM\svc-idl$', '-ServiceName', $serviceName)
        $r = Invoke-IDLScript -Script 'Install-IDLifecycleService' -StatePath $state -ScriptArguments $args1 -DryRun

        $r.ExitCode | Should Be 0
        $r.LogText | Should Match 'WOULD: CreateDirectory'
        $r.LogText | Should Match 'WOULD: CreateService'
        $r.LogText | Should Match 'WOULD: SetFailureActions'
        $r.LogText | Should Match 'Mode: DRY RUN'
        $r.LogText | Should Match 'ExitCode=0'
        $r.ReportText | Should Match 'Would'
        $r.ReportText | Should Match 'CreateService'
        (Test-Path -LiteralPath $fx.Data) | Should Be $false
        @(Get-Service -Name $serviceName -ErrorAction SilentlyContinue).Count | Should Be 0
        Remove-Item -LiteralPath $fx.Dir -Recurse -Force -ErrorAction SilentlyContinue
    }

    It 'rejects bad input with exit code 2 (missing binary, password-style account, bad service name, bad config)' {
        $fx = New-InstallFixture
        $state = New-StubStateFile
        $good = @('-BinaryPath', $fx.Exe, '-ConfigPath', $fx.Cfg, '-ServiceAccount', 'TESTDOM\svc-idl$', '-ServiceName', $serviceName)

        $bin = @('-BinaryPath', (Join-Path $fx.Dir 'missing.exe'), '-ConfigPath', $fx.Cfg, '-ServiceAccount', 'TESTDOM\svc-idl$')
        (Invoke-IDLScript -Script 'Install-IDLifecycleService' -StatePath $state -ScriptArguments $bin -DryRun).ExitCode | Should Be 2

        $acct = @('-BinaryPath', $fx.Exe, '-ConfigPath', $fx.Cfg, '-ServiceAccount', 'TESTDOM\regular.user')
        (Invoke-IDLScript -Script 'Install-IDLifecycleService' -StatePath $state -ScriptArguments $acct -DryRun).ExitCode | Should Be 2

        $name = @('-BinaryPath', $fx.Exe, '-ConfigPath', $fx.Cfg, '-ServiceAccount', 'TESTDOM\svc-idl$', '-ServiceName', 'bad name!')
        (Invoke-IDLScript -Script 'Install-IDLifecycleService' -StatePath $state -ScriptArguments $name -DryRun).ExitCode | Should Be 2

        $badCfg = Join-Path $fx.Dir 'bad.json'
        Set-Content -LiteralPath $badCfg -Value '{"nope":1}'
        $cfg = @('-BinaryPath', $fx.Exe, '-ConfigPath', $badCfg, '-ServiceAccount', 'TESTDOM\svc-idl$')
        (Invoke-IDLScript -Script 'Install-IDLifecycleService' -StatePath $state -ScriptArguments $cfg -DryRun).ExitCode | Should Be 2

        Remove-Item -LiteralPath $fx.Dir -Recurse -Force -ErrorAction SilentlyContinue
    }
}
