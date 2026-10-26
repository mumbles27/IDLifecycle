$here = Split-Path -Parent $MyInvocation.MyCommand.Path
. (Join-Path $here 'TestHelpers.ps1')

# Pure helpers (Get-IDLBlockedFiles / Unblock-IDLScriptFiles) are exercised in this process on temp files only.
Import-Module (Join-Path $script:ScriptsDir 'IDLifecycle.Common.psm1') -Force

Describe 'Install-IDLifecycleService.ps1 (DryRun only; never registers a service; unblock helpers on temp files)' {

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

        foreach ($bad in @('TESTDOM\a\b', 'not an account', 'nodomain')){
            $acct = @('-BinaryPath', $fx.Exe, '-ConfigPath', $fx.Cfg, '-ServiceAccount', $bad)
            (Invoke-IDLScript -Script 'Install-IDLifecycleService' -StatePath $state -ScriptArguments $acct -DryRun).ExitCode | Should Be 2
        }

        $name = @('-BinaryPath', $fx.Exe, '-ConfigPath', $fx.Cfg, '-ServiceAccount', 'TESTDOM\svc-idl$', '-ServiceName', 'bad name!')
        (Invoke-IDLScript -Script 'Install-IDLifecycleService' -StatePath $state -ScriptArguments $name -DryRun).ExitCode | Should Be 2

        $badCfg = Join-Path $fx.Dir 'bad.json'
        Set-Content -LiteralPath $badCfg -Value '{"nope":1}'
        $cfg = @('-BinaryPath', $fx.Exe, '-ConfigPath', $badCfg, '-ServiceAccount', 'TESTDOM\svc-idl$')
        (Invoke-IDLScript -Script 'Install-IDLifecycleService' -StatePath $state -ScriptArguments $cfg -DryRun).ExitCode | Should Be 2

        Remove-Item -LiteralPath $fx.Dir -Recurse -Force -ErrorAction SilentlyContinue
    }

    It 'accepts a user account: DryRun plans the logon right and the credential; a live run without a password is refused' {
        $fx = New-InstallFixture
        $state = New-StubStateFile
        $user = @('-BinaryPath', $fx.Exe, '-ConfigPath', $fx.Cfg, '-ServiceAccount', 'TESTDOM\svc-user', '-ServiceName', $serviceName)
        $r = Invoke-IDLScript -Script 'Install-IDLifecycleService' -StatePath $state -ScriptArguments $user -DryRun
        $r.ExitCode | Should Be 0
        $r.LogText | Should Match 'WOULD: GrantServiceLogonRight TESTDOM\\svc-user'
        $r.LogText | Should Match 'WOULD: CreateService'
        $r.LogText | Should Match 'WOULD: SetServiceCredential'
        $r.LogText | Should Match 'is a user account with a password'

        # Not a dry run: refused at validation (exit 2) before the elevation check or any change.
        $r = Invoke-IDLScript -Script 'Install-IDLifecycleService' -StatePath $state -ScriptArguments $user
        $r.ExitCode | Should Be 2
        $r.LogText | Should Match 'pass -ServiceAccountPassword'
        (Test-Path -LiteralPath $fx.Data) | Should Be $false
        @(Get-Service -Name $serviceName -ErrorAction SilentlyContinue).Count | Should Be 0
        Remove-Item -LiteralPath $fx.Dir -Recurse -Force -ErrorAction SilentlyContinue
    }

    It 'gMSA/MSA: logon right planned, no credential step; a password for one is refused and never logged' {
        $fx = New-InstallFixture
        $state = New-StubStateFile
        $gmsa = @('-BinaryPath', $fx.Exe, '-ConfigPath', $fx.Cfg, '-ServiceAccount', 'TESTDOM\svc-idl$', '-ServiceName', $serviceName)
        $r = Invoke-IDLScript -Script 'Install-IDLifecycleService' -StatePath $state -ScriptArguments $gmsa -DryRun
        $r.ExitCode | Should Be 0
        $r.LogText | Should Match 'WOULD: GrantServiceLogonRight'
        $r.LogText | Should Not Match 'SetServiceCredential'

        $pw = 'Sentinel-Svc-Pw-4Kq8'
        $r = Invoke-IDLScript -Script 'Install-IDLifecycleService' -StatePath $state -ScriptArguments $gmsa -DryRun -ExtraEnv @{ IDL_SERVICE_PASSWORD = $pw }
        $r.ExitCode | Should Be 2
        $r.LogText | Should Match 'only for a user account'
        ($r.LogText + $r.StdOut + $r.StdErr) | Should Not Match ([regex]::Escape($pw))

        # Built-in accounts need no logon right step.
        $builtIn = @('-BinaryPath', $fx.Exe, '-ConfigPath', $fx.Cfg, '-ServiceAccount', 'NT AUTHORITY\NetworkService', '-ServiceName', $serviceName)
        $r = Invoke-IDLScript -Script 'Install-IDLifecycleService' -StatePath $state -ScriptArguments $builtIn -DryRun
        $r.ExitCode | Should Be 0
        $r.LogText | Should Not Match 'GrantServiceLogonRight'
        Remove-Item -LiteralPath $fx.Dir -Recurse -Force -ErrorAction SilentlyContinue
    }

    It 'Get-IDLServiceAccountKind / ConvertTo-IDLServiceAccountName classify account names' {
        $kinds = @{
            'LocalSystem' = 'BuiltIn'; 'NT AUTHORITY\NetworkService' = 'BuiltIn'; 'NT SERVICE\IDLifecycle' = 'BuiltIn'
            'CORP\svc-idl$' = 'Managed'; 'svc-idl$@corp.example' = 'Managed'
            'CORP\svc-idl' = 'Password'; '.\svc-local' = 'Password'; 'svc-idl@corp.example' = 'Password'
            'svc-idl' = ''; 'CORP\a\b' = ''; 'CORP\with space' = ''; '' = ''
        }
        foreach ($k in $kinds.Keys){
            (Get-IDLServiceAccountKind -Account $k) | Should Be $kinds[$k]
        }

        (ConvertTo-IDLServiceAccountName -Account ' .\svc-local ') | Should Be ('{0}\svc-local' -f $env:COMPUTERNAME)
        (ConvertTo-IDLServiceAccountName -Account 'CORP\svc-idl') | Should Be 'CORP\svc-idl'
    }

    It 'Test-IDLServiceLogonRight is false for an account that does not resolve' {
        Test-IDLServiceLogonRight -Account 'NOSUCHDOMAIN-IDL\nobody' | Should Be $false
    }

    function New-BlockedScriptsDir {
        # A throw-away scripts dir: three blocked script files, one clean script, one blocked non-script, one blocked file under Logs.
        $dir = Join-Path $script:StateDir ('motw_' + [guid]::NewGuid().ToString('N'))
        $null = New-Item -ItemType Directory -Force -Path (Join-Path $dir 'Logs')
        $null = New-Item -ItemType Directory -Force -Path (Join-Path $dir 'Sub')
        $blocked = @((Join-Path $dir 'A.ps1'), (Join-Path $dir 'B.psm1'), (Join-Path $dir 'Sub\C.psd1'))
        $others = @((Join-Path $dir 'Clean.ps1'), (Join-Path $dir 'Notes.txt'), (Join-Path $dir 'Logs\Old.ps1'))
        foreach ($f in ($blocked + $others)){
            Set-Content -LiteralPath $f -Value '# test'
        }

        foreach ($f in ($blocked + @((Join-Path $dir 'Notes.txt'), (Join-Path $dir 'Logs\Old.ps1')))){
            Set-Content -Path $f -Stream Zone.Identifier -Value "[ZoneTransfer]`r`nZoneId=3"
        }

        return @{ Dir = $dir; Blocked = $blocked }
    }

    function Test-HasZoneId {
        param([string]$Path)
        return ($null -ne (Get-Item -LiteralPath $Path -Stream Zone.Identifier -ErrorAction SilentlyContinue))
    }

    It 'Get-IDLBlockedFiles lists only .ps1/.psm1/.psd1 files with a Zone.Identifier stream (Logs skipped)' {
        $b = New-BlockedScriptsDir
        $found = @(Get-IDLBlockedFiles -Path $b.Dir | Sort-Object)
        ($found -join '|') | Should Be ((@($b.Blocked) | Sort-Object) -join '|')
        @(Get-IDLBlockedFiles -Path (Join-Path $b.Dir 'missing')).Count | Should Be 0
        Remove-Item -LiteralPath $b.Dir -Recurse -Force -ErrorAction SilentlyContinue
    }

    It 'Unblock-IDLScriptFiles (the installer apply step) removes the stream and returns nothing still blocked' {
        $b = New-BlockedScriptsDir
        $left = @(Unblock-IDLScriptFiles -Path $b.Dir)
        $left.Count | Should Be 0
        foreach ($f in $b.Blocked){
            Test-HasZoneId -Path $f | Should Be $false
        }

        @(Get-IDLBlockedFiles -Path $b.Dir).Count | Should Be 0
        # Files the step does not own are left alone.
        Test-HasZoneId -Path (Join-Path $b.Dir 'Notes.txt') | Should Be $true
        Test-HasZoneId -Path (Join-Path $b.Dir 'Logs\Old.ps1') | Should Be $true
        Remove-Item -LiteralPath $b.Dir -Recurse -Force -ErrorAction SilentlyContinue
    }

    It 'DryRun reports WOULD: unblock for each blocked script in powershell.scripts_dir and unblocks nothing' {
        $fx = New-InstallFixture
        $b = New-BlockedScriptsDir
        $json = ConvertTo-Json -InputObject @{ data_dir = $fx.Data; powershell = @{ scripts_dir = $b.Dir } } -Compress
        [System.IO.File]::WriteAllText($fx.Cfg, $json, $script:Utf8NoBom)
        $state = New-StubStateFile
        $args1 = @('-BinaryPath', $fx.Exe, '-ConfigPath', $fx.Cfg, '-ServiceAccount', 'TESTDOM\svc-idl$', '-ServiceName', $serviceName)
        $r = Invoke-IDLScript -Script 'Install-IDLifecycleService' -StatePath $state -ScriptArguments $args1 -DryRun

        $r.ExitCode | Should Be 0
        $r.LogText | Should Match '3 blocked file\(s\)'
        foreach ($f in $b.Blocked){
            $r.LogText | Should Match ('WOULD: unblock ' + [regex]::Escape($f))
            Test-HasZoneId -Path $f | Should Be $true
        }

        $r.LogText | Should Match 'WOULD: UnblockScripts'
        $r.LogText | Should Not Match 'Clean\.ps1'
        $r.ReportText | Should Match 'UnblockScripts'
        Remove-Item -LiteralPath $b.Dir -Recurse -Force -ErrorAction SilentlyContinue
        Remove-Item -LiteralPath $fx.Dir -Recurse -Force -ErrorAction SilentlyContinue
    }

    It 'DryRun skips the unblock step when nothing is blocked' {
        $fx = New-InstallFixture
        $b = New-BlockedScriptsDir
        $null = Unblock-IDLScriptFiles -Path $b.Dir
        $json = ConvertTo-Json -InputObject @{ data_dir = $fx.Data; powershell = @{ scripts_dir = $b.Dir } } -Compress
        [System.IO.File]::WriteAllText($fx.Cfg, $json, $script:Utf8NoBom)
        $state = New-StubStateFile
        $args1 = @('-BinaryPath', $fx.Exe, '-ConfigPath', $fx.Cfg, '-ServiceAccount', 'TESTDOM\svc-idl$', '-ServiceName', $serviceName)
        $r = Invoke-IDLScript -Script 'Install-IDLifecycleService' -StatePath $state -ScriptArguments $args1 -DryRun
        $r.ExitCode | Should Be 0
        $r.LogText | Should Match 'SKIP: UnblockScripts'
        Remove-Item -LiteralPath $b.Dir -Recurse -Force -ErrorAction SilentlyContinue
        Remove-Item -LiteralPath $fx.Dir -Recurse -Force -ErrorAction SilentlyContinue
    }
}
