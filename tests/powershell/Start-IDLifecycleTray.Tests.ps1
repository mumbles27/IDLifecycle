$here = Split-Path -Parent $MyInvocation.MyCommand.Path
. (Join-Path $here 'TestHelpers.ps1')
. (Join-Path $here 'SetupTestHelpers.ps1')

# The tray's helpers are dot-sourced with -DefineOnly (no icon, nothing started). The UI objects are built (never
# shown) when this thread is STA. Branding is checked against temp folders whose owners are simulated.

$script:TrayScript = Join-Path $script:ScriptsDir 'Start-IDLifecycleTray.ps1'
. $script:TrayScript -DefineOnly

Describe 'Start-IDLifecycleTray.ps1: tray.json, menu, command lines, settings, branding (no icon shown)' {

    $root = Join-Path $script:StateDir ('tray_' + [guid]::NewGuid().ToString('N').Substring(0, 8))
    $null = New-Item -ItemType Directory -Force -Path $root
    $info = [ordered]@{
        service_name = 'IDLifecycle'; console_url = 'https://idl.corp.test:8443/'; listen = '0.0.0.0:8443'; tls = $true; log_level = 'info'
        data_dir = 'C:\ProgramData\IDLifecycle'; install_dir = 'C:\Program Files\IDLifecycle'; config_path = 'C:\ProgramData\IDLifecycle\config.json'
        version = '1.2.0'; execution_policy = 'RemoteSigned'
    }

    It 'tray.json: written atomically and read back; anything that is not what it must be is refused' {
        $path = Join-Path $root 'tray.json'
        Write-IDLTrayInfo -Path $path -Info ([ordered]@{} + $info)
        $back = Read-IDLTrayInfo -Path $path
        $back['console_url'] | Should Be 'https://idl.corp.test:8443/'
        $back['tls'] | Should Be $true
        $back['written_at'] | Should Match '^\d{4}-\d{2}-\d{2}T'
        (Test-Path -LiteralPath "$path.tmp") | Should Be $false

        foreach ($bad in @(
                @{ service_name = 'x; calc' },
                @{ console_url = 'file:///C:/Windows/System32/calc.exe' },
                @{ console_url = 'https://idl.corp.test:8443/path?x=1' },
                @{ console_url = 'javascript:alert(1)' },
                @{ install_dir = 'relative\path' },
                @{ config_path = '' },
                @{ log_level = 'loud' },
                @{ execution_policy = 'Whatever' }
            )){
            $doc = [ordered]@{} + $info
            foreach ($k in $bad.Keys){
                $doc[$k] = $bad[$k]
            }

            $threw = $false
            try{
                $null = ConvertTo-IDLTrayInfo -Doc $doc
            }
            catch{
                $threw = $true
            }

            "$(ConvertTo-Json -InputObject $bad -Compress) refused: $threw" | Should Be "$(ConvertTo-Json -InputObject $bad -Compress) refused: True"
        }

        $noPolicy = [ordered]@{} + $info
        $noPolicy['execution_policy'] = ''
        (ConvertTo-IDLTrayInfo -Doc $noPolicy)['execution_policy'] | Should Be 'RemoteSigned'
        $missing = $false
        try{
            $null = Read-IDLTrayInfo -Path (Join-Path $root 'missing.json')
        }
        catch{
            $missing = ($_.Exception.Message -match 'not found')
        }

        $missing | Should Be $true
    }

    It 'menu and tooltip follow the service state' {
        $running = @(Get-IDLTrayMenuModel -Info $info -State 'Running' -LogFolder 'C:\x')
        ($running | ForEach-Object { $_.Key }) -join ',' | Should Be 'open,status,-,settings,restart,stop,logs,-,exit'
        ($running | Where-Object { $_.Key -eq 'open' }).Bold | Should Be $true
        ($running | Where-Object { $_.Key -eq 'status' }).Text | Should Be 'Service: Running (version 1.2.0)'
        ($running | Where-Object { $_.Key -eq 'status' }).Enabled | Should Be $false
        ($running | Where-Object { $_.Key -eq 'restart' }).Enabled | Should Be $true

        $stopped = @(Get-IDLTrayMenuModel -Info $info -State 'Stopped' -LogFolder '')
        @($stopped | Where-Object { $_.Key -eq 'stop' }).Count | Should Be 0
        ($stopped | Where-Object { $_.Key -eq 'start' }).Enabled | Should Be $true
        ($stopped | Where-Object { $_.Key -eq 'restart' }).Enabled | Should Be $false
        ($stopped | Where-Object { $_.Key -eq 'logs' }).Enabled | Should Be $false

        $gone = @(Get-IDLTrayMenuModel -Info $info -State 'NotInstalled')
        ($gone | Where-Object { $_.Key -eq 'start' }).Enabled | Should Be $false
        ($gone | Where-Object { $_.Key -eq 'status' }).Text | Should Match 'Not installed'

        Get-IDLTrayTooltip -Product 'IDLifecycle' -State 'StopPending' | Should Be 'IDLifecycle - Stopping'
        (Get-IDLTrayTooltip -Product ('A' * 70) -State 'Running').Length | Should Be 63
    }

    It 'builds the elevated helper''s command line: only known values, quoted so Windows splits it back exactly' {
        if (-not ('IDLTrayTest.Argv' -as [type])){
            Add-Type -Namespace 'IDLTrayTest' -Name 'Argv' -MemberDefinition @'
[DllImport("shell32.dll", SetLastError = true, CharSet = CharSet.Unicode)]
public static extern System.IntPtr CommandLineToArgvW(string lpCmdLine, out int pNumArgs);
[DllImport("kernel32.dll")] public static extern System.IntPtr LocalFree(System.IntPtr hMem);
public static string[] Split(string line) {
    int n; System.IntPtr p = CommandLineToArgvW("x " + line, out n);
    string[] r = new string[n - 1];
    for (int i = 1; i < n; i++) { r[i - 1] = System.Runtime.InteropServices.Marshal.PtrToStringUni(System.Runtime.InteropServices.Marshal.ReadIntPtr(p, i * System.IntPtr.Size)); }
    LocalFree(p); return r;
}
'@
        }

        foreach ($v in @('plain', 'with space', 'C:\Program Files\X\', 'a"b', '', 'C:\dir with\trailing\\', 'tab	here')){
            [IDLTrayTest.Argv]::Split((ConvertTo-IDLCommandLineArg -Value $v))[0] | Should Be $v
        }

        $c = New-IDLTrayActionCommand -Action 'SetConfig' -Info $info -Settings @{ Listen = '0.0.0.0:9443'; TlsCertFile = ''; TlsKeyFile = '' } -Restart -ResultFile 'C:\Temp\IDLTray_x.json' -HelperPath 'C:\Program Files\IDLifecycle\scripts\Invoke-IDLTrayAction.ps1'
        $c.FilePath | Should Be ([System.Environment]::ProcessPath)
        $split = [IDLTrayTest.Argv]::Split($c.Arguments)
        ($split -join '|') | Should Be (($c.ArgumentList) -join '|')
        $split[[array]::IndexOf($split, '-ExecutionPolicy') + 1] | Should Be 'RemoteSigned'
        $split[[array]::IndexOf($split, '-File') + 1] | Should Be 'C:\Program Files\IDLifecycle\scripts\Invoke-IDLTrayAction.ps1'
        $split[[array]::IndexOf($split, '-ConfigPath') + 1] | Should Be 'C:\ProgramData\IDLifecycle\config.json'
        $split[[array]::IndexOf($split, '-TlsCertFile') + 1] | Should Be ''
        $split -contains '-Restart' | Should Be $true
        $split -contains '-LogLevel' | Should Be $false
        $split -contains 'Bypass' | Should Be $false

        $r = New-IDLTrayActionCommand -Action 'Restart' -Info $info
        $rs = [IDLTrayTest.Argv]::Split($r.Arguments)
        ($rs[($rs.Count - 4)..($rs.Count - 1)] -join ' ') | Should Be '-Action Restart -ServiceName IDLifecycle'
        $rs -contains '-ConfigPath' | Should Be $false

        # The policy comes from tray.json (what the service uses for its scripts); it is never weakened.
        $signed = [ordered]@{} + $info
        $signed['execution_policy'] = 'AllSigned'
        $s2 = [IDLTrayTest.Argv]::Split((New-IDLTrayActionCommand -Action 'Stop' -Info $signed).Arguments)
        $s2[[array]::IndexOf($s2, '-ExecutionPolicy') + 1] | Should Be 'AllSigned'
    }

    It 'settings: only real changes are sent; HTTPS is kept, replaced or switched off; the same checks as the setup window' {
        $cert = Join-Path $root 'idl.pem'
        $key = Join-Path $root 'idl.key'
        Set-Content -LiteralPath $cert, $key -Value 'x'
        $base = @{ Listen = '0.0.0.0:8443'; TlsCertFile = ''; TlsKeyFile = ''; LogLevel = 'info'; TlsOff = $false }

        (Get-IDLTraySettingsChanges -Info $info -Form $base).Count | Should Be 0
        @(Get-IDLTraySettingsProblems -Info $info -Form $base).Count | Should Be 0

        $f = $base.Clone(); $f['LogLevel'] = 'debug'; $f['Listen'] = ' 0.0.0.0:9443 '
        $c = Get-IDLTraySettingsChanges -Info $info -Form $f
        (@($c.Keys | Sort-Object) -join ',') | Should Be 'Listen,LogLevel'
        $c['Listen'] | Should Be '0.0.0.0:9443'

        $f = $base.Clone(); $f['TlsCertFile'] = $cert; $f['TlsKeyFile'] = $key
        (Get-IDLTraySettingsChanges -Info $info -Form $f)['TlsKeyFile'] | Should Be $key
        @(Get-IDLTraySettingsProblems -Info $info -Form $f).Count | Should Be 0

        $f = $base.Clone(); $f['TlsCertFile'] = $cert
        (Get-IDLTraySettingsProblems -Info $info -Form $f) -join ' ' | Should Match 'both the certificate file and the key file'

        $f = $base.Clone(); $f['TlsCertFile'] = (Join-Path $root 'no.pem'); $f['TlsKeyFile'] = $key
        (Get-IDLTraySettingsProblems -Info $info -Form $f) -join ' ' | Should Match 'File not found'

        # Switching HTTPS off is only possible for an address on this server.
        $f = $base.Clone(); $f['TlsOff'] = $true
        (Get-IDLTraySettingsProblems -Info $info -Form $f) -join ' ' | Should Match 'needs HTTPS'
        $f['Listen'] = '127.0.0.1:8443'
        @(Get-IDLTraySettingsProblems -Info $info -Form $f).Count | Should Be 0
        $c = Get-IDLTraySettingsChanges -Info $info -Form $f
        $c['TlsCertFile'] | Should Be ''
        $c['TlsKeyFile'] | Should Be ''

        # Without HTTPS today, a network address needs both files.
        $plain = [ordered]@{} + $info
        $plain['tls'] = $false
        $plain['listen'] = '127.0.0.1:8443'
        (Get-IDLTraySettingsProblems -Info $plain -Form @{ Listen = '0.0.0.0:8443'; LogLevel = 'info' }) -join ' ' | Should Match 'needs HTTPS'
        (Get-IDLTraySettingsProblems -Info $plain -Form @{ Listen = ''; LogLevel = 'info' }) -join ' ' | Should Match 'Enter the web console address'
        (Get-IDLTraySettingsProblems -Info $plain -Form @{ Listen = 'nonsense'; LogLevel = 'info' }) -join ' ' | Should Match 'must be host:port'
    }

    It 'reads the helper result: done, refused, cancelled UAC; the result file is removed' {
        $file = New-IDLTrayResultPath
        Split-Path -Leaf $file | Should Match '^IDLTray_[0-9a-f]{32}\.json$'
        (Split-Path -Parent $file).TrimEnd('\') | Should Be ([System.IO.Path]::GetTempPath().TrimEnd('\'))
        [System.IO.File]::WriteAllText($file, '{"ok":true,"message":"The service was restarted.","state":"Running"}')
        $r = Read-IDLTrayResult -ExitCode 0 -ResultFile $file
        $r.Ok | Should Be $true
        $r.Message | Should Be 'The service was restarted.'
        (Test-Path -LiteralPath $file) | Should Be $false

        $r = Read-IDLTrayResult -ExitCode -1 -ResultFile $file
        $r.Cancelled | Should Be $true
        $r = Read-IDLTrayResult -ExitCode 2 -ResultFile $file
        $r.Ok | Should Be $false
        $r.Message | Should Match 'refused'
    }

    It 'log folder: the first one this user can read, else none' {
        $data = Join-Path $root 'data'
        $null = New-Item -ItemType Directory -Force -Path (Join-Path $data 'logs')
        $i = [ordered]@{} + $info
        $i['data_dir'] = $data
        Get-IDLTrayLogFolder -Info $i | Should Be (Join-Path $data 'logs')
        $i['data_dir'] = (Join-Path $root 'nowhere')
        $script:SetupLogDir = (Join-Path $root 'nowhere\setup')
        Get-IDLTrayLogFolder -Info $i | Should Be ''
    }

    It 'branding: defaults from scripts\branding; a protected override folder wins; one others can change is ignored' {
        $default = Join-Path $root 'branding-default'
        $null = New-Item -ItemType Directory -Force -Path $default
        [System.IO.File]::WriteAllText((Join-Path $default 'brand.json'), '{"product_name":"IDLifecycle"}')
        [System.IO.File]::WriteAllBytes((Join-Path $default 'idlifecycle.ico'), [byte[]](0, 0, 1, 0))
        $b = Get-IDLBranding -DefaultDir $default -OverrideDir (Join-Path $root 'no-override')
        $b.ProductName | Should Be 'IDLifecycle'
        $b.IconPath | Should Be (Join-Path $default 'idlifecycle.ico')
        $b.Custom | Should Be $false

        $over = Join-Path $root 'IDLifecycle-Branding'
        $null = New-Item -ItemType Directory -Force -Path $over
        [System.IO.File]::WriteAllText((Join-Path $over 'brand.json'), '{"product_name":"Acme Identity"}')
        [System.IO.File]::WriteAllBytes((Join-Path $over 'tray.ico'), [byte[]](0, 0, 1, 0))
        $admins = { param($p) 'S-1-5-32-544' }
        $me = [System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value

        # Protected like the installer makes it: SYSTEM/Administrators full, Users read; owner simulated as Administrators.
        $protect = {
            $acl = New-IDLBrandingDirSecurity
            $acl.SetOwner([System.Security.Principal.SecurityIdentifier]::new($me)) # a non-elevated test cannot give it away
            [System.IO.FileSystemAclExtensions]::SetAccessControl([System.IO.DirectoryInfo]::new($over), $acl)
            foreach ($f in @(Get-ChildItem -LiteralPath $over -File)){
                $fa = [System.IO.FileSystemAclExtensions]::GetAccessControl([System.IO.FileInfo]::new($f.FullName))
                $fa.SetAccessRuleProtection($false, $false)
                foreach ($rule in @($fa.GetAccessRules($true, $false, [System.Security.Principal.SecurityIdentifier]))){
                    $null = $fa.RemoveAccessRuleAll($rule)
                }

                [System.IO.FileSystemAclExtensions]::SetAccessControl([System.IO.FileInfo]::new($f.FullName), $fa)
            }
        }
        $open = {
            $o = [System.IO.FileSystemAclExtensions]::GetAccessControl([System.IO.DirectoryInfo]::new($over))
            $o.AddAccessRule([System.Security.AccessControl.FileSystemAccessRule]::new([System.Security.Principal.SecurityIdentifier]::new($me), 'FullControl', 'ContainerInherit, ObjectInherit', 'None', 'Allow'))
            [System.IO.FileSystemAclExtensions]::SetAccessControl([System.IO.DirectoryInfo]::new($over), $o)
        }

        & $protect
        try{
            $b = Get-IDLBranding -DefaultDir $default -OverrideDir $over -OwnerSidResolver $admins
            $b.Warning | Should Be ''
            $b.ProductName | Should Be 'Acme Identity'
            $b.IconPath | Should Be (Join-Path $over 'tray.ico')
            $b.Custom | Should Be $true

            # The real owner (the test user, not an administrator account) is not trusted.
            $b = Get-IDLBranding -DefaultDir $default -OverrideDir $over
            $b.ProductName | Should Be 'IDLifecycle'
            $b.Warning | Should Match 'ignored'

            # A bad product name and an oversized icon are ignored, the rest still applies.
            & $open
            [System.IO.File]::WriteAllText((Join-Path $over 'brand.json'), ('{"product_name":"' + ('x' * 41) + '"}'))
            [System.IO.File]::WriteAllBytes((Join-Path $over 'tray.ico'), [byte[]]::new(1MB + 1))
            & $protect
            $b = Get-IDLBranding -DefaultDir $default -OverrideDir $over -OwnerSidResolver $admins
            $b.ProductName | Should Be 'IDLifecycle'
            $b.IconPath | Should Be (Join-Path $default 'idlifecycle.ico')
        }
        finally{
            & $open
        }

        # Users may change it: ignored, with a warning that says why.
        Grant-TestAce -Path $over -Sid 'S-1-5-32-545' -Rights 'Modify'
        $b = Get-IDLBranding -DefaultDir $default -OverrideDir $over -OwnerSidResolver $admins
        $b.Custom | Should Be $false
        $b.Warning | Should Match 'Users'
    }

    It 'the settings window and the tray icon build (STA), every control the code uses exists' {
        $names = @(Get-IDLTraySettingsControlNames)
        $text = [System.IO.File]::ReadAllText($script:TrayScript)
        $used = @([regex]::Matches($text, '\$ui\.(\w+)') | ForEach-Object { $_.Groups[1].Value } | Sort-Object -Unique)
        @($used | Where-Object { $names -notcontains $_ }) -join ',' | Should Be ''

        if ([System.Threading.Thread]::CurrentThread.GetApartmentState() -eq 'STA'){
            $brand = Get-IDLBranding -DefaultDir (Join-Path $script:ScriptsDir 'branding') -OverrideDir ''
            $w = New-IDLTraySettingsWindow -Branding $brand
            @($names | Where-Object { $null -eq $w.Ui[$_] }) -join ',' | Should Be ''
            $w.Window.Title | Should Be "$($brand.ProductName) settings"
            $w.Window.Close()

            $tray = New-IDLTrayIcon -Branding $brand
            try{
                $script:Clicked = @()
                Update-IDLTrayMenu -Tray $tray -Model @(Get-IDLTrayMenuModel -Info $info -State 'Running') -OnClick { param($k) $script:Clicked += $k }
                $tray.Items.Keys.Count | Should Be 7
                $tray.Items['status'].Enabled | Should Be $false
                $tray.Menu.Items.Count | Should Be 9
                # A click reaches the handler with the item's key (no closures: the handler sees this script's state).
                $tray.Items['settings'].PerformClick()
                $tray.Items['restart'].PerformClick()
                ($script:Clicked -join ',') | Should Be 'settings,restart'
            }
            finally{
                $tray.Icon.Dispose()
            }
        }
    }

    Remove-Item -LiteralPath $root -Recurse -Force -ErrorAction SilentlyContinue
}
