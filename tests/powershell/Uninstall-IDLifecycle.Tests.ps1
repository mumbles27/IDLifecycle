$here = Split-Path -Parent $MyInvocation.MyCommand.Path
. (Join-Path $here 'TestHelpers.ps1')
. (Join-Path $here 'SetupTestHelpers.ps1')

# Every run uses SetupFakes.ps1 (no real service / sc.exe / env var / event source) and temp folders only.
# Guard tests with dangerous paths always run with -DryRun as a second safety net.

Describe 'Uninstall-IDLifecycle.ps1 (fakes for every system change; temp folders only)' {

    $serviceName = 'IDLUninstallTest'
    $dsnSecret = 'Sentinel-Uninst-Pw-3Rt'

    function New-InstalledFixture {
        # An "installed" layout as Install-IDLifecycle.ps1 leaves it: program folder with SHA256SUMS, data folder with
        # config.json (data_dir = itself) + secret.key + DB, and a fake running service whose binPath points at them.
        param([string]$Driver = 'sqlite', [string]$Dsn = '', [switch]$NoManifest)

        $fx = New-SetupFixture
        $null = New-Item -ItemType Directory -Force -Path (Join-Path $fx.Install 'scripts\Logs'), $fx.Data
        [System.IO.File]::WriteAllText((Join-Path $fx.Install 'idlifecycled.exe'), 'FAKE-IDLIFECYCLED 1.0.0')
        [System.IO.File]::WriteAllText((Join-Path $fx.Install 'scripts\IDLifecycle.Common.psm1'), '# fake')
        [System.IO.File]::WriteAllText((Join-Path $fx.Install 'scripts\New-IDLUser.ps1'), '# fake')
        [System.IO.File]::WriteAllText((Join-Path $fx.Install 'scripts\Logs\New-IDLUser_Report_1.csv'), 'report')
        if (-not $NoManifest){
            $lines = foreach ($rel in @('idlifecycled.exe', 'scripts\IDLifecycle.Common.psm1', 'scripts\New-IDLUser.ps1')){
                '{0}  {1}' -f (Get-FileHash -LiteralPath (Join-Path $fx.Install $rel) -Algorithm SHA256).Hash.ToLowerInvariant(), $rel.Replace('\', '/')
            }
            [System.IO.File]::WriteAllText((Join-Path $fx.Install 'SHA256SUMS'), (($lines -join "`n") + "`n"))
        }

        [System.IO.File]::WriteAllText((Join-Path $fx.Data 'idlifecycle.db'), 'db')
        [System.IO.File]::WriteAllText((Join-Path $fx.Data 'secret.key'), 'key')
        $dsnValue = (($Dsn) ? ($Dsn) : ('file:' + (Join-Path $fx.Data 'idlifecycle.db')))
        $cfg = [ordered]@{ listen = '127.0.0.1:8443'; data_dir = $fx.Data; database = @{ driver = $Driver; dsn = $dsnValue } }
        [System.IO.File]::WriteAllText($fx.Config, (ConvertTo-Json -InputObject $cfg -Depth 5))
        $exe = Join-Path $fx.Install 'idlifecycled.exe'
        $svcName = $serviceName
        Update-FakeSetupState -Path $fx.State -Change {
            param($s)
            $s['services'] = @{ $svcName = @{ State = 'Running'; PathName = ('"{0}" service -config "{1}"' -f $exe, $fx.Config); StartName = 'NT AUTHORITY\NetworkService' } }
            $s['eventSources'] = @('IDLifecycle')
        }
        return $fx
    }

    It 'default run removes service, event source and program files but KEEPS the data folder; re-run skips everything' {
        $fx = New-InstalledFixture
        $r = Invoke-SetupScript -Script 'Uninstall-IDLifecycle' -Params @{ ServiceName = $serviceName } -StatePath $fx.State

        $r.ExitCode | Should Be 0
        $ops = @(Get-FakeOps -Path $fx.State)
        ($ops -join ',') | Should Be 'stop,delete,removeEventSource'
        $state = Read-FakeSetupState -Path $fx.State
        $state['services'].Contains($serviceName) | Should Be $false
        @($state['eventSources']).Count | Should Be 0
        (Test-Path -LiteralPath $fx.Install) | Should Be $false
        (Test-Path -LiteralPath (Join-Path $fx.Data 'idlifecycle.db')) | Should Be $true
        (Test-Path -LiteralPath $fx.Config) | Should Be $true
        $r.ReportText | Should Match '"RemoveInstallDir","Applied",.*files listed in SHA256SUMS'
        $r.ReportText | Should Match '"RemoveDataDir","Skipped",.*kept \(default'
        $r.LogText | Should Match 'Active Directory: NOT changed'
        $r.LogText | Should Match 'NT AUTHORITY\\NetworkService'
        $r.LogText | Should Match 'Database: sqlite file \(kept in'
        @($r.ScriptsLogsNew).Count | Should Be 0

        # Already uninstalled (service gone, so the folders must be named): every step skipped, exit 0.
        Reset-FakeCalls -Path $fx.State
        $r2 = Invoke-SetupScript -Script 'Uninstall-IDLifecycle' -Params @{ ServiceName = $serviceName; InstallDir = $fx.Install; DataDir = $fx.Data } -StatePath $fx.State
        $r2.ExitCode | Should Be 0
        $r2.ReportText | Should Not Match '"Applied"'
        $r2.ReportText | Should Not Match '"Failed"'
        foreach ($step in @('StopService', 'DeleteService', 'RemoveEventSource', 'RemoveInstallDir', 'RemoveDataDir', 'BackupData')){
            $r2.ReportText | Should Match ('"' + $step + '","Skipped"')
        }

        @(Get-FakeOps -Path $fx.State).Count | Should Be 0
        Remove-SetupFixture -Fixture $fx
    }

    It 'manifest-based removal leaves unknown files (reported) and a re-run skips them' {
        $fx = New-InstalledFixture
        Set-Content -LiteralPath (Join-Path $fx.Install 'notes.txt') -Value 'not ours'
        $r = Invoke-SetupScript -Script 'Uninstall-IDLifecycle' -Params @{ ServiceName = $serviceName } -StatePath $fx.State
        $r.ExitCode | Should Be 0
        $r.LogText | Should Match 'left in .*not part of the install\): notes\.txt'
        (Test-Path -LiteralPath (Join-Path $fx.Install 'notes.txt')) | Should Be $true
        (Test-Path -LiteralPath (Join-Path $fx.Install 'idlifecycled.exe')) | Should Be $false
        (Test-Path -LiteralPath (Join-Path $fx.Install 'scripts')) | Should Be $false

        $r2 = Invoke-SetupScript -Script 'Uninstall-IDLifecycle' -Params @{ ServiceName = $serviceName; InstallDir = $fx.Install; DataDir = $fx.Data } -StatePath $fx.State
        $r2.ExitCode | Should Be 0
        $r2.ReportText | Should Match '"RemoveInstallDir","Skipped",.*no IDLifecycle program files left'
        Remove-SetupFixture -Fixture $fx
    }

    It 'DryRun reports WOULD: and changes nothing' {
        $fx = New-InstalledFixture
        $r = Invoke-SetupScript -Script 'Uninstall-IDLifecycle' -Params @{ ServiceName = $serviceName; RemoveData = $true; Force = $true; NoBackup = $true; DryRun = $true } -StatePath $fx.State
        $r.ExitCode | Should Be 0
        foreach ($step in @('StopService', 'DeleteService', 'RemoveEventSource', 'RemoveInstallDir', 'RemoveDataDir')){
            $r.LogText | Should Match ('WOULD: ' + $step + ' ')
        }

        $r.ReportText | Should Not Match '"Applied"'
        @(Get-FakeOps -Path $fx.State).Count | Should Be 0
        (Test-Path -LiteralPath (Join-Path $fx.Install 'idlifecycled.exe')) | Should Be $true
        (Test-Path -LiteralPath $fx.Config) | Should Be $true
        (Read-FakeSetupState -Path $fx.State)['services'][$serviceName]['State'] | Should Be 'Running'
        Remove-SetupFixture -Fixture $fx
    }

    It '-RemoveData without -Force, or without -BackupTo/-NoBackup, is refused before anything is touched' {
        $fx = New-InstalledFixture
        $r = Invoke-SetupScript -Script 'Uninstall-IDLifecycle' -Params @{ ServiceName = $serviceName; RemoveData = $true; NoBackup = $true } -StatePath $fx.State
        $r.ExitCode | Should Be 2
        $r.LogText | Should Match '-RemoveData deletes .* add -Force'

        $r = Invoke-SetupScript -Script 'Uninstall-IDLifecycle' -Params @{ ServiceName = $serviceName; RemoveData = $true; Force = $true } -StatePath $fx.State
        $r.ExitCode | Should Be 2
        $r.LogText | Should Match '-RemoveData needs -BackupTo'

        @(Get-FakeOps -Path $fx.State).Count | Should Be 0
        (Test-Path -LiteralPath (Join-Path $fx.Install 'idlifecycled.exe')) | Should Be $true
        (Test-Path -LiteralPath $fx.Config) | Should Be $true
        Remove-SetupFixture -Fixture $fx
    }

    It 'a failed backup prevents any removal and leaves no zip (unusable target, or a file locked mid-backup)' {
        $fx = New-InstalledFixture
        $blocker = Join-Path $fx.Root 'not-a-folder.txt'
        Set-Content -LiteralPath $blocker -Value 'x'
        $r = Invoke-SetupScript -Script 'Uninstall-IDLifecycle' -Params @{ ServiceName = $serviceName; RemoveData = $true; Force = $true; BackupTo = (Join-Path $blocker 'sub') } -StatePath $fx.State
        $r.ExitCode | Should Be 1
        $r.ReportText | Should Match '"BackupData","Failed"'

        # A data file that cannot be read while the zip is being written: the partial zip is deleted again.
        $backup = Join-Path $fx.Root 'backup'
        $lock = [System.IO.File]::Open((Join-Path $fx.Data 'secret.key'), 'Open', 'Read', 'None')
        try{
            $r = Invoke-SetupScript -Script 'Uninstall-IDLifecycle' -Params @{ ServiceName = $serviceName; RemoveData = $true; Force = $true; BackupTo = $backup } -StatePath $fx.State
        }
        finally{
            $lock.Dispose()
        }

        $r.ExitCode | Should Be 1
        $r.ReportText | Should Match '"BackupData","Failed"'
        @(Get-ChildItem -LiteralPath $backup -Recurse -Filter '*.zip' -Force -ErrorAction SilentlyContinue).Count | Should Be 0

        $ops = @(Get-FakeOps -Path $fx.State)
        $ops -contains 'delete' | Should Be $false
        $ops -contains 'removeEventSource' | Should Be $false
        (Test-Path -LiteralPath (Join-Path $fx.Install 'idlifecycled.exe')) | Should Be $true
        (Test-Path -LiteralPath (Join-Path $fx.Data 'idlifecycle.db')) | Should Be $true
        (Read-FakeSetupState -Path $fx.State)['services'].Contains($serviceName) | Should Be $true
        Remove-SetupFixture -Fixture $fx
    }

    It '-RemoveData -Force -BackupTo: zip in a new restricted folder, then data + IDL_ env var removed; server DB only reported' {
        $dsn = "sqlserver://idl:$dsnSecret@sql01?database=idlprod"
        $fx = New-InstalledFixture -Driver 'mssql' -Dsn 'env:IDL_T_UNINST_DSN'
        Update-FakeSetupState -Path $fx.State -Change { param($s) $s['machineEnv'] = @{ IDL_T_UNINST_DSN = $dsn; OTHER_VAR = 'keep' } }
        $backup = Join-Path $fx.Root 'backup'
        $p = @{ ServiceName = $serviceName; RemoveData = $true; Force = $true; BackupTo = $backup; RemoveDbDsnEnvVar = $true }
        $r = Invoke-SetupScript -Script 'Uninstall-IDLifecycle' -Params $p -StatePath $fx.State

        $r.ExitCode | Should Be 0
        $r.AllText | Should Not Match ([regex]::Escape($dsnSecret))
        $folder = @(Get-ChildItem -LiteralPath $backup -Directory -Filter "IDLifecycle-backup_$($serviceName)_*")
        $folder.Count | Should Be 1
        $zip = @(Get-ChildItem -LiteralPath $folder[0].FullName -Filter '*.zip')
        $zip.Count | Should Be 1
        Add-Type -AssemblyName System.IO.Compression.FileSystem
        $archive = [System.IO.Compression.ZipFile]::OpenRead($zip[0].FullName)
        try{
            $names = @($archive.Entries | ForEach-Object { $_.FullName })
        }
        finally{
            $archive.Dispose()
        }

        $names -contains 'data/config.json' | Should Be $true
        $names -contains 'data/secret.key' | Should Be $true
        $names -contains 'scripts-Logs/New-IDLUser_Report_1.csv' | Should Be $true
        # The folder was restricted before the zip was written; the zip inherits it.
        $acl = [System.IO.FileSystemAclExtensions]::GetAccessControl([System.IO.DirectoryInfo]::new($folder[0].FullName), [System.Security.AccessControl.AccessControlSections]::Access)
        $acl.AreAccessRulesProtected | Should Be $true
        $sids = @($acl.GetAccessRules($true, $true, [System.Security.Principal.SecurityIdentifier]) | ForEach-Object { $_.IdentityReference.Value })
        $sids -contains 'S-1-5-32-545' | Should Be $false
        $sids -contains 'S-1-5-11' | Should Be $false
        $zipSids = @([System.IO.FileSystemAclExtensions]::GetAccessControl([System.IO.FileInfo]::new($zip[0].FullName), [System.Security.AccessControl.AccessControlSections]::Access).GetAccessRules($true, $true, [System.Security.Principal.SecurityIdentifier]) | ForEach-Object { $_.IdentityReference.Value })
        $zipSids -contains 'S-1-5-32-545' | Should Be $false

        (Test-Path -LiteralPath $fx.Data) | Should Be $false
        (Test-Path -LiteralPath $fx.Install) | Should Be $false
        $envs = (Read-FakeSetupState -Path $fx.State)['machineEnv']
        $envs.Contains('IDL_T_UNINST_DSN') | Should Be $false
        $envs['OTHER_VAR'] | Should Be 'keep'
        $r.LogText | Should Match "Database: mssql database 'idlprod' on 'sql01' was NOT dropped"
        Remove-SetupFixture -Fixture $fx
    }

    It 'L1: DSN with "/" and "@" in the password: target printed without any password fragment' {
        $fx = New-InstalledFixture -Driver 'postgres' -Dsn 'postgres://idl:Fake/Part2@x@db01/idl'
        $r = Invoke-SetupScript -Script 'Uninstall-IDLifecycle' -Params @{ ServiceName = $serviceName; DryRun = $true } -StatePath $fx.State
        $r.ExitCode | Should Be 0
        $r.LogText | Should Match "postgres database 'idl' on 'db01' was NOT dropped"
        $r.AllText | Should Not Match 'Part2'
        $r.AllText | Should Not Match 'Fake/'
        Remove-SetupFixture -Fixture $fx
    }

    It 'L5: an env:NAME DSN variable that is not IDL_* is never removed' {
        $fx = New-InstalledFixture -Driver 'mssql' -Dsn 'env:SOME_DB_DSN'
        Update-FakeSetupState -Path $fx.State -Change { param($s) $s['machineEnv'] = @{ SOME_DB_DSN = 'sqlserver://a:b@c?database=d' } }
        $r = Invoke-SetupScript -Script 'Uninstall-IDLifecycle' -Params @{ ServiceName = $serviceName; RemoveDbDsnEnvVar = $true } -StatePath $fx.State
        $r.ExitCode | Should Be 0
        $r.ReportText | Should Match '"RemoveDbDsnEnvVar","Skipped",.*not an IDL_\* variable'
        (Read-FakeSetupState -Path $fx.State)['machineEnv'].Contains('SOME_DB_DSN') | Should Be $true
        @(Get-FakeOps -Path $fx.State) -contains 'removeMachineEnv' | Should Be $false
        Remove-SetupFixture -Fixture $fx
    }

    It 'L9: the shared event source is kept while another idlifecycled service exists' {
        $fx = New-InstalledFixture
        Update-FakeSetupState -Path $fx.State -Change { param($s) $s['services']['IDLOther'] = @{ State = 'Running'; PathName = '"D:\Other\idlifecycled.exe" service -config "D:\Other\config.json"'; StartName = 'LocalSystem' } }
        $r = Invoke-SetupScript -Script 'Uninstall-IDLifecycle' -Params @{ ServiceName = $serviceName } -StatePath $fx.State
        $r.ExitCode | Should Be 0
        $r.ReportText | Should Match '"RemoveEventSource","Skipped",.*still used by service\(s\) IDLOther'
        @((Read-FakeSetupState -Path $fx.State)['eventSources']) -contains 'IDLifecycle' | Should Be $true
        Remove-SetupFixture -Fixture $fx
    }

    It 'M2/L6: refuses to delete a tree others can write to, a data folder without its markers, and InstallDir without manifest unless named' {
        # A file in InstallDir that BUILTIN\Users may change: nothing is deleted.
        $fx = New-InstalledFixture
        Grant-TestAce -Path (Join-Path $fx.Install 'scripts\New-IDLUser.ps1') -Rights 'Modify'
        $r = Invoke-SetupScript -Script 'Uninstall-IDLifecycle' -Params @{ ServiceName = $serviceName } -StatePath $fx.State
        $r.ExitCode | Should Be 1
        $r.ReportText | Should Match '"RemoveInstallDir","Failed",.*while others can change it'
        (Test-Path -LiteralPath (Join-Path $fx.Install 'idlifecycled.exe')) | Should Be $true

        # DataDir needs secret.key AND config.json whose data_dir is the folder.
        $fx2 = New-InstalledFixture
        Remove-Item -LiteralPath (Join-Path $fx2.Data 'secret.key') -Force
        $r = Invoke-SetupScript -Script 'Uninstall-IDLifecycle' -Params @{ ServiceName = $serviceName; RemoveData = $true; Force = $true; NoBackup = $true } -StatePath $fx2.State
        $r.ExitCode | Should Be 1
        $r.ReportText | Should Match '"RemoveDataDir","Failed",.*not an IDLifecycle data folder'
        (Test-Path -LiteralPath $fx2.Config) | Should Be $true

        # The service's exe folder has no install manifest: InstallDir must be passed explicitly.
        $fx3 = New-InstalledFixture -NoManifest
        $r = Invoke-SetupScript -Script 'Uninstall-IDLifecycle' -Params @{ ServiceName = $serviceName } -StatePath $fx3.State
        $r.ExitCode | Should Be 2
        $r.LogText | Should Match 'holds no SHA256SUMS install manifest; pass -InstallDir'
        @(Get-FakeOps -Path $fx3.State).Count | Should Be 0
        foreach ($f in @($fx, $fx2, $fx3)){
            Remove-SetupFixture -Fixture $f
        }
    }

    It 'refuses dangerous InstallDir / DataDir / BackupTo values (path guard, exit 2, DryRun as extra net)' {
        $fx = New-InstalledFixture
        $bad = @('C:\', 'C:\Windows', 'C:\Program Files', $env:USERPROFILE, '', 'C:\Windows\System32\IDL', 'relative\dir', 'C:\Users',
            'C:\Users\Public', 'C:\Users\Default', 'C:\ProgramData\Microsoft\IDL', 'C:\$Recycle.Bin\IDL', 'C:\IDL:stream', 'C:\IDL::$INDEX_ALLOCATION')
        foreach ($value in $bad){
            foreach ($name in @('InstallDir', 'DataDir')){
                $p = @{ ServiceName = $serviceName; RemoveData = $true; Force = $true; NoBackup = $true; DryRun = $true; $name = $value }
                $r = Invoke-SetupScript -Script 'Uninstall-IDLifecycle' -Params $p -StatePath $fx.State
                $r.ExitCode | Should Be 2
                $r.LogText | Should Match "$name '.*' refused"
            }
        }

        $r = Invoke-SetupScript -Script 'Uninstall-IDLifecycle' -Params @{ ServiceName = $serviceName; DryRun = $true; BackupTo = (Join-Path $fx.Data 'bak') } -StatePath $fx.State
        $r.ExitCode | Should Be 2
        $r.LogText | Should Match 'BackupTo .* must not be inside'
        @(Get-FakeOps -Path $fx.State).Count | Should Be 0
        (Test-Path -LiteralPath (Join-Path $fx.Install 'idlifecycled.exe')) | Should Be $true
        Remove-SetupFixture -Fixture $fx
    }

    It 'refuses to remove program files that would take the data folder with them, and a service that is not IDLifecycle' {
        $fx = New-InstalledFixture
        $r = Invoke-SetupScript -Script 'Uninstall-IDLifecycle' -Params @{ ServiceName = $serviceName; InstallDir = $fx.Root; DataDir = $fx.Data; DryRun = $true } -StatePath $fx.State
        $r.ExitCode | Should Be 2
        $r.LogText | Should Match 'is inside InstallDir'

        Update-FakeSetupState -Path $fx.State -Change { param($s) $s['services']['Spooler'] = @{ State = 'Running'; PathName = 'C:\Windows\System32\spoolsv.exe'; StartName = 'LocalSystem' } }
        $r = Invoke-SetupScript -Script 'Uninstall-IDLifecycle' -Params @{ ServiceName = 'Spooler'; InstallDir = $fx.Install; DataDir = $fx.Data } -StatePath $fx.State
        $r.ExitCode | Should Be 2
        $r.LogText | Should Match 'does not look like IDLifecycle'
        @(Get-FakeOps -Path $fx.State).Count | Should Be 0
        Remove-SetupFixture -Fixture $fx
    }

    It 'removal never follows a junction out of InstallDir and refuses a folder that is not an IDLifecycle install' {
        $fx = New-InstalledFixture -NoManifest
        $outside = Join-Path $fx.Root 'outside'
        $null = New-Item -ItemType Directory -Force -Path $outside
        Set-Content -LiteralPath (Join-Path $outside 'precious.txt') -Value 'keep me'
        $null = New-Item -ItemType Junction -Path (Join-Path $fx.Install 'scripts\linked') -Target $outside
        $r = Invoke-SetupScript -Script 'Uninstall-IDLifecycle' -Params @{ ServiceName = $serviceName; InstallDir = $fx.Install } -StatePath $fx.State
        $r.ExitCode | Should Be 0
        (Test-Path -LiteralPath $fx.Install) | Should Be $false
        (Test-Path -LiteralPath (Join-Path $outside 'precious.txt')) | Should Be $true

        # Same with the manifest-based removal.
        $fx2 = New-InstalledFixture
        $outside2 = Join-Path $fx2.Root 'outside'
        $null = New-Item -ItemType Directory -Force -Path $outside2
        Set-Content -LiteralPath (Join-Path $outside2 'precious.txt') -Value 'keep me'
        $null = New-Item -ItemType Junction -Path (Join-Path $fx2.Install 'scripts\linked') -Target $outside2
        $r = Invoke-SetupScript -Script 'Uninstall-IDLifecycle' -Params @{ ServiceName = $serviceName } -StatePath $fx2.State
        $r.ExitCode | Should Be 0
        (Test-Path -LiteralPath (Join-Path $outside2 'precious.txt')) | Should Be $true
        (Test-Path -LiteralPath (Join-Path $fx2.Install 'scripts\linked')) | Should Be $false

        $foreign = Join-Path $fx.Root 'foreign'
        $null = New-Item -ItemType Directory -Force -Path $foreign
        Set-Content -LiteralPath (Join-Path $foreign 'other-app.exe') -Value 'x'
        $r = Invoke-SetupScript -Script 'Uninstall-IDLifecycle' -Params @{ ServiceName = $serviceName; InstallDir = $foreign; DataDir = $fx.Data } -StatePath $fx.State
        $r.ExitCode | Should Be 1
        $r.ReportText | Should Match '"RemoveInstallDir","Failed".*does not look like an IDLifecycle install'
        (Test-Path -LiteralPath (Join-Path $foreign 'other-app.exe')) | Should Be $true
        Remove-SetupFixture -Fixture $fx2
        Remove-SetupFixture -Fixture $fx
    }

    It 'L7: the test hook is refused without IDL_SETUP_TEST=1 (nothing is removed)' {
        $fx = New-InstalledFixture
        $r = Invoke-SetupScript -Script 'Uninstall-IDLifecycle' -Params @{ ServiceName = $serviceName } -StatePath $fx.State -NoTestEnv
        $r.ExitCode | Should Be 2
        $r.StdErr | Should Match '-TestHook refused'
        (Test-Path -LiteralPath (Join-Path $fx.Install 'idlifecycled.exe')) | Should Be $true
        Remove-SetupFixture -Fixture $fx
    }
}
