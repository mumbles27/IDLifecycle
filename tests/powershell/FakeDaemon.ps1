# Stand-in for idlifecycled.exe used by SetupFakes.ps1: pwsh -File FakeDaemon.ps1 <exe path> <idlifecycled args...>
# Implements just enough of version / init / lockdown / user add / check / restore against $env:IDL_FAKE_STATE.
# It records each call (arguments, whether IDL_PASSWORD was present and its length, which IDL_* variables were set)
# but never the password itself.
# The arguments are read from the raw command line so idlifecycled flags such as -config are not bound by pwsh.

$ErrorActionPreference = 'Stop'

$all = [System.Environment]::GetCommandLineArgs()
$index = -1
for ($i = 0; $i -lt $all.Count; $i++){
    if ($all[$i] -ieq $PSCommandPath){
        $index = $i
        break
    }
}

if ($index -lt 0){
    [Console]::Error.WriteLine('FakeDaemon: cannot find its own path on the command line')
    exit 5
}

$exe = $all[$index + 1]
$argv = @(($index + 2 -lt $all.Count) ? ($all[($index + 2)..($all.Count - 1)]) : (@()))
$statePath = $env:IDL_FAKE_STATE
$state = [System.IO.File]::ReadAllText($statePath) | ConvertFrom-Json -AsHashtable

function Get-Flag {
    param([string]$Name, [string]$Default = '')

    for ($i = 0; $i -lt $argv.Count - 1; $i++){
        if ($argv[$i] -eq "-$Name"){
            return $argv[$i + 1]
        }
    }

    return $Default
}

function Save-State {
    [System.IO.File]::WriteAllText($statePath, (ConvertTo-Json -InputObject $state -Depth 20), [System.Text.UTF8Encoding]::new($false))
}

$password = [string]$env:IDL_PASSWORD
$op = (($argv.Count -gt 0) ? ($argv[0]) : (''))
if (
    ($op -eq 'user') -and
    ($argv.Count -gt 1)
){
    $op = 'user ' + $argv[1]
}

$state['calls'] = @($state['calls']) + @(@{
        op             = "daemon:$op"
        data           = @{
            exe            = $exe
            args           = @($argv)
            hasPassword    = (-not [string]::IsNullOrEmpty($password))
            passwordLength = $password.Length
            backupPassLength = ([string]$env:IDL_BACKUP_PASSPHRASE).Length
            idlEnv         = @(Get-ChildItem env: | Where-Object { $_.Name -like 'IDL_*' -and $_.Name -notlike 'IDL_FAKE*' -and $_.Name -notlike 'IDL_STUB*' } | ForEach-Object { $_.Name } | Sort-Object)
        }
    })
Save-State

switch ($op){
    'version' {
        $text = ([System.IO.File]::ReadAllText($exe)).Trim()
        Write-Output ('idlifecycled ' + ($text -split '\s+')[-1])
        exit 0
    }

    'init' {
        $dir = Get-Flag -Name 'dir'
        $cfgPath = Join-Path -Path $dir -ChildPath 'config.json'
        $null = New-Item -ItemType Directory -Force -Path $dir
        if (Test-Path -LiteralPath $cfgPath){
            [Console]::Error.WriteLine("refusing to overwrite existing $cfgPath")
            exit 1
        }

        $driver = Get-Flag -Name 'db-driver' -Default 'sqlite'
        $dsn = Get-Flag -Name 'db-dsn' -Default (Join-Path -Path $dir -ChildPath 'idlifecycle.db')
        $cfg = [ordered]@{
            listen     = (Get-Flag -Name 'listen' -Default '127.0.0.1:8443')
            tls        = @{ cert_file = (Get-Flag -Name 'tls-cert'); key_file = (Get-Flag -Name 'tls-key') }
            data_dir   = $dir
            database   = @{ driver = $driver; dsn = $dsn }
            powershell = @{ exe = 'pwsh.exe'; scripts_dir = (Get-Flag -Name 'scripts') }
        }
        [System.IO.File]::WriteAllText($cfgPath, (ConvertTo-Json -InputObject $cfg -Depth 5), [System.Text.UTF8Encoding]::new($false))
        Write-Output "Wrote $cfgPath"
        exit 0
    }

    'lockdown' {
        $code = [int]$state['lockdownExit']
        if ($code -eq 0){
            $cfg = [System.IO.File]::ReadAllText((Get-Flag -Name 'config')) | ConvertFrom-Json -AsHashtable
            $state['lockedDown'] = @($state['lockedDown']) + @([string]$cfg['data_dir'])
            Save-State
            Write-Output "Restricted $($cfg['data_dir'])"
        }
        else{
            [Console]::Error.WriteLine('error: fake lockdown failure')
        }

        exit $code
    }

    'user add' {
        if ([string]::IsNullOrEmpty($password)){
            [Console]::Error.WriteLine('error: no password supplied (set IDL_PASSWORD or pipe one line on stdin)')
            exit 1
        }

        if ([bool]$state['echoPassword']){
            # Failure path that leaks the secret the way a careless tool would; the installer must scrub it.
            [Console]::Error.WriteLine("error: cannot create user: debug password='$password'")
            exit 1
        }

        if ($password.Length -lt 12){
            [Console]::Error.WriteLine('error: password too short')
            exit 1
        }

        $state['admins'] = [int]$state['admins'] + 1
        Save-State
        Write-Output "Created admin user `"$(Get-Flag -Name 'username')`""
        exit 0
    }

    'check' {
        # Contract: every name, always, in this order; a check that did not run has ok=false, skipped=true; top-level
        # ok covers the non-skipped checks but is false whenever config failed.
        # State knobs: checkFail{name: detail} fails a check, checkSkip{name: detail} marks it skipped,
        # checkOmit[] drops names from the output (to prove the installer never treats a missing name as a pass).
        $cfgPath = Get-Flag -Name 'config'
        $exists = Test-Path -LiteralPath $cfgPath
        $checks = [System.Collections.Generic.List[object]]::new()
        $fail = $state['checkFail']
        $skip = $state['checkSkip']
        $omit = @($state['checkOmit'])
        $dsnText = ''
        if ($exists){
            $dsnText = [string](([System.IO.File]::ReadAllText($cfgPath) | ConvertFrom-Json -AsHashtable)['database']['dsn'])
            if ($dsnText -match '^env:(?<n>.+)$'){
                $dsnText = [string][System.Environment]::GetEnvironmentVariable($Matches['n'])
            }
        }

        foreach ($name in @('config', 'data_dir_acl', 'database', 'admin_user', 'powershell', 'scripts', 'scripts_unblocked', 'program_files_acl', 'ad_module')){
            $ok = $true
            $skipped = $false
            $detail = 'fake ok'
            if ($name -eq 'config'){
                $ok = $exists
                $detail = (($exists) ? ($cfgPath) : ("read config: $cfgPath not found"))
            }
            elseif (-not $exists){
                $ok = $false
                $skipped = $true
                $detail = 'not run: config did not load'
            }
            elseif ($name -eq 'admin_user'){
                $ok = ([int]$state['admins'] -gt 0)
                $detail = (($ok) ? ("$($state['admins']) enabled administrator(s)") : ('no enabled administrator'))
            }
            elseif (
                ($name -eq 'ad_module') -and
                ($argv -contains '-skip-ad-module')
            ){
                $ok = $false
                $skipped = $true
                $detail = 'not run: -skip-ad-module'
            }

            if (
                ($null -ne $fail) -and
                ($fail.Contains($name))
            ){
                $ok = $false
                $detail = [string]$fail[$name]
            }

            if (
                ($name -eq 'database') -and
                ($exists) -and
                ([bool]$state['echoDsn'])
            ){
                # A driver error that quotes the whole connection string.
                $ok = $false
                $detail = "mssql: login failed for connection string '$dsnText'"
            }

            if (
                ($null -ne $skip) -and
                ($skip.Contains($name))
            ){
                $ok = $false
                $skipped = $true
                $detail = [string]$skip[$name]
            }

            if ($omit -contains $name){
                continue
            }

            $checks.Add([ordered]@{ name = $name; ok = $ok; skipped = $skipped; detail = $detail })
        }

        $allOk = (
            $exists -and
            (@($checks | Where-Object { (-not $_['skipped']) -and (-not $_['ok']) }).Count -eq 0)
        )
        Write-Output (ConvertTo-Json -InputObject ([ordered]@{ ok = $allOk; checks = $checks.ToArray() }) -Depth 5)
        exit ((($allOk) ? (0) : (1)))
    }

    'restore' {
        # A "backup" here is a text file starting with IDLBAK1. State: backupPassphrase (the right one, default
        # 'backup passphrase 1'), restoreAdmins (administrators in the backup, default 1).
        $in = Get-Flag -Name 'in'
        if (-not (Test-Path -LiteralPath $in -PathType Leaf) -or -not ([System.IO.File]::ReadAllText($in).StartsWith('IDLBAK1'))){
            [Console]::Error.WriteLine('error: not an IDLifecycle backup file')
            exit 2
        }

        Write-Output 'Backup made 2026-10-01T02:00:00Z by version 1.0.0 (sqlite database, schema 2)'
        if ($argv -contains '-inspect'){
            exit 0
        }

        $want = (($state['backupPassphrase']) ? ([string]$state['backupPassphrase']) : ('backup passphrase 1'))
        if ([string]$env:IDL_BACKUP_PASSPHRASE -cne $want){
            [Console]::Error.WriteLine('error: the backup cannot be decrypted: wrong passphrase, or the file was changed or damaged')
            exit 2
        }

        if (([int]$state['admins'] -gt 0) -and ($argv -notcontains '-force')){
            [Console]::Error.WriteLine('error: the database already has data (users: 1); restoring replaces ALL of it - pass -force to do that')
            exit 1
        }

        $state['admins'] = (($null -ne $state['restoreAdmins']) ? ([int]$state['restoreAdmins']) : (1))
        $state['restored'] = $true
        $state['restoredWithConfig'] = ($argv -contains '-with-config')
        Save-State
        Write-Output 'Restored: users 1 rows'
        exit 0
    }

    default {
        [Console]::Error.WriteLine("FakeDaemon: unsupported command '$op'")
        exit 2
    }
}
