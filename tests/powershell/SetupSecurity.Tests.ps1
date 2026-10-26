$here = Split-Path -Parent $MyInvocation.MyCommand.Path
. (Join-Path $here 'TestHelpers.ps1')
. (Join-Path $here 'SetupTestHelpers.ps1')

# Pure / temp-folder-only tests of the setup helpers in IDLifecycle.Common.psm1 and of functions parsed out of the
# setup scripts (never executed as a whole). Nothing outside the temp state dir is changed.
Import-Module (Join-Path $script:ScriptsDir 'IDLifecycle.Common.psm1') -Force

Describe 'Setup helpers: path guard, ACL trust, manifest, DSN scrubbing, protected folders, test-hook gate' {

    function New-TempDir {
        $d = Join-Path $script:StateDir ('sec_' + [guid]::NewGuid().ToString('N').Substring(0, 10))
        $null = New-Item -ItemType Directory -Force -Path $d
        return $d
    }

    It 'Get-IDLUnsafePathReason refuses roots, system folders, profiles, ADS, short names and odd paths' {
        $bad = @(
            '', '   ', 'C:\', 'C:', 'D:\', 'C:\Windows', 'c:\windows\', 'C:\Windows\System32', 'C:\Windows\Temp\x',
            'C:\Program Files', 'C:\Program Files (x86)', 'C:\ProgramData', 'C:\Users', $env:USERPROFILE,
            (Split-Path -Parent $env:USERPROFILE), $env:APPDATA, $env:LOCALAPPDATA, $env:TEMP,
            'relative\path', '\\server\share\idl', 'C:\Prog*', 'C:\IDL?', 'C:\PROGRA~1\IDLifecycle',
            'C:\Program Files\..\Windows', 'C:\Windows.',
            'C:\IDL:stream', 'C:\IDL::$INDEX_ALLOCATION', 'C:\Users\Default', 'C:\Users\Public', 'C:\Users\someone',
            'C:\ProgramData\Microsoft', 'C:\ProgramData\Microsoft\Crypto\x', 'C:\$Recycle.Bin\S-1-5-21', 'D:\$Recycle.Bin',
            'C:\System Volume Information\x', 'E:\Recovery\x', (Join-Path $PSHOME 'Modules\x'), $PSHOME
        )
        foreach ($p in $bad){
            (Get-IDLUnsafePathReason -Path $p) | Should Not BeNullOrEmpty
        }
    }

    It 'Get-IDLUnsafePathReason accepts normal application folders and temp sub-folders' {
        foreach ($p in @('C:\Program Files\IDLifecycle', 'C:\ProgramData\IDLifecycle', 'C:\ProgramData\IDLifecycle-Setup\Logs', 'D:\Apps\IDLifecycle', (Join-Path $script:StateDir 'x\y'), 'C:\Install\IDLifecycle-1.0.0')){
            (Get-IDLUnsafePathReason -Path $p) | Should BeNullOrEmpty
        }
    }

    It 'Get-IDLUnsafePathReason refuses a junction (or a path below one)' {
        $root = New-TempDir
        $target = Join-Path $root 'target'
        $null = New-Item -ItemType Directory -Force -Path $target
        $link = Join-Path $root 'link'
        $null = New-Item -ItemType Junction -Path $link -Target $target
        (Get-IDLUnsafePathReason -Path $link) | Should Match 'junction'
        (Get-IDLUnsafePathReason -Path (Join-Path $link 'sub')) | Should Match 'junction'
        (Get-IDLUnsafePathReason -Path $target) | Should BeNullOrEmpty
        [System.IO.Directory]::Delete($link, $false)
        Remove-Item -LiteralPath $root -Recurse -Force -ErrorAction SilentlyContinue
    }

    It 'Get-IDLUntrustedAclReason / -TreeReason: write ACEs for others fail, inherit-only templates and parent create-rights do not' {
        $root = New-TempDir
        $sub = Join-Path $root 'release'
        $null = New-Item -ItemType Directory -Force -Path (Join-Path $sub 'scripts')
        $file = Join-Path $sub 'scripts\a.ps1'
        Set-Content -LiteralPath $file -Value 'x'

        (Get-IDLUntrustedTreeReason -Path $sub -ParentChain -RefuseReparsePoints) | Should BeNullOrEmpty
        Test-IDLTrustedOwner -Path $sub | Should Be $true

        # An inherit-only template does not apply to the folder itself (it does to children created later).
        $rootIo = New-TempDir
        Grant-TestAce -Path $rootIo -Rights 'Modify' -InheritOnly
        (Get-IDLUntrustedAclReason -Path $rootIo -Mode Full) | Should BeNullOrEmpty

        $root2 = New-TempDir
        Grant-TestAce -Path $root2 -Rights 'CreateFiles, CreateDirectories'
        (Get-IDLUntrustedAclReason -Path $root2 -Mode Parent) | Should BeNullOrEmpty
        (Get-IDLUntrustedAclReason -Path $root2 -Mode Full) | Should Match 'BUILTIN\\Users'

        $root3 = New-TempDir
        Grant-TestAce -Path $root3 -Rights 'DeleteSubdirectoriesAndFiles'
        (Get-IDLUntrustedAclReason -Path $root3 -Mode Parent) | Should Match 'BUILTIN\\Users'

        Grant-TestAce -Path $file -Rights 'Write'
        (Get-IDLUntrustedTreeReason -Path $sub) | Should Match 'a\.ps1. lets BUILTIN\\Users change it'
        (Get-IDLUntrustedAclReason -Path $sub -Mode Full -OwnerSid 'S-1-5-21-1-2-3-1001') | Should Match 'is owned by'

        $null = New-Item -ItemType Junction -Path (Join-Path $sub 'link') -Target $root2
        (Get-IDLUntrustedTreeReason -Path $sub -RefuseReparsePoints) | Should Not BeNullOrEmpty
        [System.IO.Directory]::Delete((Join-Path $sub 'link'), $false)
        foreach ($d in @($root, $root2, $root3, $rootIo)){
            Remove-Item -LiteralPath $d -Recurse -Force -ErrorAction SilentlyContinue
        }
    }

    It 'Read-IDLManifest parses sha256sum lines and refuses malformed or unsafe entries' {
        $root = New-TempDir
        $m = Join-Path $root 'SHA256SUMS'
        $h = 'a' * 64
        [System.IO.File]::WriteAllText($m, "# comment`n$h  idlifecycled.exe`n$h *scripts/IDLifecycle.Common.psm1`n")
        $entries = Read-IDLManifest -Path $m
        $entries.Count | Should Be 2
        $entries[1].Relative | Should Be 'scripts\IDLifecycle.Common.psm1'
        $entries[0].Hash | Should Be ('A' * 64)

        foreach ($line in @('nothex  x', "$h  ../evil.ps1", "$h  C:/evil.ps1", "$h  /abs.ps1", "$h  scripts/Logs/a.ps1", "$h  scripts/./a.ps1", "$h  a.ps1`n$h  A.ps1")){
            [System.IO.File]::WriteAllText($m, "$line`n")
            $threw = $false
            try{
                $null = Read-IDLManifest -Path $m
            }
            catch{
                $threw = $true
            }

            $threw | Should Be $true
        }
        Remove-Item -LiteralPath $root -Recurse -Force -ErrorAction SilentlyContinue
    }

    It 'Register-IDLDsnSecrets / Get-IDLDsnTarget: no password fragment survives, target has no userinfo' {
        # Passwords: the values that must be scrubbed on their own. Leaks: fragments that must never be in the target text.
        $cases = @(
            @{ Dsn = 'postgres://idl:Fake/Part2@db01/idl'; Passwords = @('Fake/Part2'); Leaks = @('Fake', 'Part2'); Target = "database 'idl' on 'db01'" },
            @{ Dsn = 'postgres://idl:p@ss/w0rd@db01:5432/idl?sslmode=require'; Passwords = @('p@ss/w0rd'); Leaks = @('ss/w0rd', 'w0rd'); Target = "database 'idl' on 'db01:5432'" },
            @{ Dsn = 'sqlserver://idl:S3cr%2Fet@sql01?database=idlprod'; Passwords = @('S3cr%2Fet', 'S3cr/et'); Leaks = @('S3cr'); Target = "database 'idlprod' on 'sql01'" },
            @{ Dsn = 'idl:My@Pass@tcp(mysql01:3306)/idl?parseTime=true'; Passwords = @('My@Pass'); Leaks = @('Pass'); Target = "database 'idl' on 'mysql01:3306'" },
            @{ Dsn = 'Server=sql01;Database=idl;User Id=idl;Password=Pw0rd-KV;With=Semi'; Passwords = @('Pw0rd-KV'); Leaks = @('Pw0rd'); Target = "database 'idl' on 'sql01'" },
            @{ Dsn = 'DATABASE=idl;HOSTNAME=db2host;PORT=50000;UID=u;PWD={abc;def}'; Passwords = @('{abc;def}', 'abc;def'); Leaks = @('abc'); Target = "database 'idl' on 'db2host'" }
        )
        foreach ($c in $cases){
            Register-IDLDsnSecrets -Dsn $c.Dsn
            (Protect-IDLText -Text "x $($c.Dsn) y") | Should Be 'x *** y'
            foreach ($pw in $c.Passwords){
                (Protect-IDLText -Text "x $pw y") | Should Not Match ([regex]::Escape($pw))
            }

            $target = Get-IDLDsnTarget -Dsn $c.Dsn
            $target | Should Be $c.Target
            foreach ($leak in $c.Leaks){
                $target | Should Not Match ([regex]::Escape($leak))
            }
        }
    }

    It 'Initialize-IDLProtectedDir -Lock creates a protected folder; refuses a junction; Set-IDLDirectoryLockdown keeps a trusted owner' {
        $root = New-TempDir
        $dir = Join-Path $root 'protected'
        Initialize-IDLProtectedDir -Path $dir -Lock -IncludeCurrentUser
        $acl = Get-IDLDirectoryDacl -Path $dir
        $acl.AreAccessRulesProtected | Should Be $true
        $sids = @($acl.GetAccessRules($true, $true, [System.Security.Principal.SecurityIdentifier]) | ForEach-Object { $_.IdentityReference.Value })
        ($sids | Sort-Object -Unique) -join ',' | Should Be ((@('S-1-5-18', 'S-1-5-32-544', [System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value) | Sort-Object -Unique) -join ',')
        Test-IDLTrustedOwner -Path $dir | Should Be $true

        $target = Join-Path $root 'target'
        $null = New-Item -ItemType Directory -Force -Path $target
        $link = Join-Path $root 'link'
        $null = New-Item -ItemType Junction -Path $link -Target $target
        $threw = $false
        try{
            Initialize-IDLProtectedDir -Path $link
        }
        catch{
            $threw = $true
        }

        $threw | Should Be $true
        [System.IO.Directory]::Delete($link, $false)
        Remove-Item -LiteralPath $root -Recurse -Force -ErrorAction SilentlyContinue
    }

    It 'Get-IDLTestHookRefusal: needs IDL_SETUP_TEST=1, a hook inside tests\powershell, and a script outside InstallDir/Program Files' {
        $old = $env:IDL_SETUP_TEST
        try{
            $env:IDL_SETUP_TEST = $null
            (Get-IDLTestHookRefusal -HookPath $script:SetupHook -ScriptRoot $script:ScriptsDir) | Should Match 'IDL_SETUP_TEST'
            $env:IDL_SETUP_TEST = '1'
            (Get-IDLTestHookRefusal -HookPath $script:SetupHook -ScriptRoot $script:ScriptsDir) | Should BeNullOrEmpty
            (Get-IDLTestHookRefusal -HookPath (Join-Path $script:StateDir 'x.ps1') -ScriptRoot $script:ScriptsDir) | Should Match 'inside'
            (Get-IDLTestHookRefusal -HookPath $script:SetupHook -ScriptRoot $script:ScriptsDir -InstallDir $script:RepoRoot) | Should Match 'runs from'
            (Get-IDLTestHookRefusal -HookPath $script:SetupHook -ScriptRoot (Join-Path $env:ProgramFiles 'IDLifecycle\scripts')) | Should Not BeNullOrEmpty
        }
        finally{
            $env:IDL_SETUP_TEST = $old
        }
    }
}

Describe 'Functions parsed from the setup scripts (never run as a whole)' {

    It 'New-IDLInstallDirSecurity: protected; SYSTEM/Administrators/TrustedInstaller full; Users + service account read only' {
        . ([scriptblock]::Create((Get-ScriptFunctionText -Path (Join-Path $script:ScriptsDir 'Install-IDLifecycle.ps1') -Name 'New-IDLInstallDirSecurity')))
        $svc = 'S-1-5-20'
        $sec = New-IDLInstallDirSecurity -ServiceAccountSid $svc
        $sec.AreAccessRulesProtected | Should Be $true
        $rules = @($sec.GetAccessRules($true, $true, [System.Security.Principal.SecurityIdentifier]))
        $bySid = @{}
        foreach ($r in $rules){
            $bySid[$r.IdentityReference.Value] = $r.FileSystemRights
        }

        foreach ($sid in @('S-1-5-18', 'S-1-5-32-544', 'S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464')){
            $bySid[$sid] | Should Be ([System.Security.AccessControl.FileSystemRights]::FullControl)
        }

        foreach ($sid in @('S-1-5-32-545', $svc)){
            (([int64]$bySid[$sid]) -band (2 -bor 4 -bor 64 -bor 65536 -bor 262144 -bor 524288)) | Should Be 0
            (([int64]$bySid[$sid]) -band [int64][System.Security.AccessControl.FileSystemRights]::ReadAndExecute) | Should Be ([int64][System.Security.AccessControl.FileSystemRights]::ReadAndExecute)
        }

        @($rules | Where-Object { $_.IdentityReference.Value -in @('S-1-1-0', 'S-1-5-11', 'S-1-3-0') }).Count | Should Be 0
        $rules.Count | Should Be 5
    }

    It 'Install-IDLifecycleService.ps1 hands sc.exe an EMPTY password argument under Standard/Windows and Legacy argument passing' {
        $fn = Get-ScriptFunctionText -Path (Join-Path $script:ScriptsDir 'Install-IDLifecycleService.ps1') -Name 'Get-IDLScEmptyArgument'
        $echo = Join-Path $script:StateDir ('echoargs_' + [guid]::NewGuid().ToString('N').Substring(0, 8) + '.ps1')
        $null = New-Item -ItemType Directory -Force -Path $script:StateDir
        [System.IO.File]::WriteAllText($echo, '$a = [Environment]::GetCommandLineArgs(); $i = [array]::IndexOf($a, $PSCommandPath); for ($j = $i + 1; $j -lt $a.Count; $j++){ "<{0}>" -f $a[$j] }')
        foreach ($mode in @('Windows', 'Standard', 'Legacy')){
            $code = "$fn`n`$PSNativeCommandArgumentPassing = '$mode'`n& ([Environment]::ProcessPath) -NoProfile -NonInteractive -File '$echo' 'password=' (Get-IDLScEmptyArgument) 'next'"
            $r = Invoke-StubSnippet -Code $code -NoState
            $r.ExitCode | Should Be 0
            ($r.StdOutRaw -split "`r?`n" | Where-Object { $_ }) -join '|' | Should Be '<password=>|<>|<next>'
        }

        Remove-Item -LiteralPath $echo -Force -ErrorAction SilentlyContinue
    }
}
