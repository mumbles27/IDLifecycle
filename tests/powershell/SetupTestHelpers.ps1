# Helpers for Install-IDLifecycle.Tests.ps1 / Uninstall-IDLifecycle.Tests.ps1. Dot-source AFTER TestHelpers.ps1.
#
# SAFETY: the setup scripts run in a child pwsh (stub-only PSModulePath, like every other test) with
# IDL_SETUP_TEST=1 and -TestHook SetupFakes.ps1, which replaces every system-touching function (services, sc.exe,
# machine environment variables, event log sources, elevation, AD/gMSA/ODBC checks, HTTP probe, owner lookups,
# the InstallDir ACL, Install-IDLifecycleService.ps1) with a simulation in a temp JSON file, and points the setup
# log folder at a temp folder. InstallDir/DataDir/BackupTo/release folders are always under the temp state dir.
# No real service, ACL outside temp, Program Files/ProgramData, AD or database is ever touched.

$script:SetupHook = Join-Path $script:TestsRoot 'SetupFakes.ps1'

function New-FakeSetupState {
    # Writes a fake state file (defaults: elevated, AD module present, gMSA usable, nothing installed).
    param([Parameter(Mandatory)][string]$Path, [hashtable]$Overrides = @{})

    $state = [ordered]@{
        elevated       = $true
        adModule       = $true
        gmsaOk         = $true
        credentialOk   = $true
        odbcDrivers    = @()
        services       = @{}
        eventSources   = @()
        machineEnv     = @{}
        httpStatus     = 200
        installerExit  = 0
        admins         = 0
        checkFail      = @{}
        checkSkip      = @{}
        checkOmit      = @()
        echoPassword   = $false
        echoDsn        = $false
        ownerOverrides = @{}
        installAcl     = @()
        lockdownExit   = 0
        lockedDown     = @()
        startState     = ''
        githubSource   = ''
        githubFail     = ''
        goVersion      = 'go1.27.3'
        goBuildFail    = $false
        handOffExit    = 0
        calls          = @()
    }
    foreach ($k in $Overrides.Keys){
        $state[$k] = $Overrides[$k]
    }

    [System.IO.File]::WriteAllText($Path, (ConvertTo-Json -InputObject $state -Depth 20), $script:Utf8NoBom)
    return $Path
}

function Read-FakeSetupState {
    param([Parameter(Mandatory)][string]$Path)

    return ([System.IO.File]::ReadAllText($Path) | ConvertFrom-Json -AsHashtable)
}

function Update-FakeSetupState {
    param([Parameter(Mandatory)][string]$Path, [Parameter(Mandatory)][scriptblock]$Change)

    $s = Read-FakeSetupState -Path $Path
    & $Change $s
    [System.IO.File]::WriteAllText($Path, (ConvertTo-Json -InputObject $s -Depth 20), $script:Utf8NoBom)
}

function Get-FakeOps {
    # Operation names recorded by the fakes, in order (e.g. daemon:init, stop, start, probe).
    # Enumerated (no unary comma), so callers wrap it in @() to get a flat array.
    param([Parameter(Mandatory)][string]$Path)

    return @(@((Read-FakeSetupState -Path $Path)['calls']) | Where-Object { $null -ne $_ } | ForEach-Object { [string]$_['op'] })
}

function Get-FakeCalls {
    param([Parameter(Mandatory)][string]$Path, [string]$Op = '')

    return @(@((Read-FakeSetupState -Path $Path)['calls']) | Where-Object { ($null -ne $_) -and ((-not $Op) -or ($_['op'] -eq $Op)) })
}

function Reset-FakeCalls {
    param([Parameter(Mandatory)][string]$Path)

    Update-FakeSetupState -Path $Path -Change { param($s) $s['calls'] = @() }
}

function Write-ReleaseManifest {
    # SHA256SUMS for a release folder: idlifecycled.exe (+ Setup.cmd when present) + every file under scripts\ (what
    # New-IDLifecycleRelease.ps1 writes). -Extra lists more root-relative files (to test refusals).
    param([Parameter(Mandatory)][string]$Root, [string[]]$Extra = @())

    $rels = @('idlifecycled.exe') + @($Extra)
    if (Test-Path -LiteralPath (Join-Path $Root 'Setup.cmd') -PathType Leaf){
        $rels += 'Setup.cmd'
    }

    $scripts = Join-Path $Root 'scripts'
    foreach ($f in @(Get-ChildItem -LiteralPath $scripts -Recurse -File -Force)){
        $rels += ('scripts\' + $f.FullName.Substring($scripts.Length).TrimStart('\'))
    }

    $lines = foreach ($rel in ($rels | Sort-Object)){
        '{0}  {1}' -f (Get-FileHash -LiteralPath (Join-Path $Root $rel) -Algorithm SHA256).Hash.ToLowerInvariant(), $rel.Replace('\', '/')
    }

    [System.IO.File]::WriteAllText((Join-Path $Root 'SHA256SUMS'), (($lines -join "`n") + "`n"), $script:Utf8NoBom)
}

function New-SetupFixture {
    <#
      A throw-away release folder (fake idlifecycled.exe whose content carries the version, a copy of the repo
      scripts - Mark-of-the-Web included, the repo itself is never unblocked - and SHA256SUMS) plus empty
      install/data locations. The temp folder inherits SYSTEM/Administrators/you only, so it passes the trust check.
    #>
    param([string]$Version = '1.0.0', [string]$ExeSource = '')

    $root = Join-Path $script:StateDir ('setup_' + [guid]::NewGuid().ToString('N').Substring(0, 12))
    $src = Join-Path $root 'release'
    $null = New-Item -ItemType Directory -Force -Path (Join-Path $src 'scripts')
    Get-ChildItem -LiteralPath $script:ScriptsDir -File | Copy-Item -Destination (Join-Path $src 'scripts')
    $exe = Join-Path $src 'idlifecycled.exe'
    if ($ExeSource){
        Copy-Item -LiteralPath $ExeSource -Destination $exe
    }
    else{
        [System.IO.File]::WriteAllText($exe, "FAKE-IDLIFECYCLED $Version", $script:Utf8NoBom)
    }

    Write-ReleaseManifest -Root $src
    $state = New-FakeSetupState -Path (Join-Path $root 'fake-state.json')
    return @{
        Root    = $root
        Source  = $src
        Exe     = $exe
        Install = (Join-Path $root 'Program Files\IDLifecycle')
        Data    = (Join-Path $root 'ProgramData\IDLifecycle')
        Config  = (Join-Path $root 'ProgramData\IDLifecycle\config.json')
        State   = $state
    }
}

function Remove-SetupFixture {
    param($Fixture)

    if (
        ($null -ne $Fixture) -and
        ($Fixture.Root) -and
        ($Fixture.Root.StartsWith($script:StateDir, [System.StringComparison]::OrdinalIgnoreCase))
    ){
        Remove-Item -LiteralPath $Fixture.Root -Recurse -Force -ErrorAction SilentlyContinue
    }
}

function Grant-TestAce {
    # Adds an Allow ACE on a temp file/folder (to simulate a user-writable release or install folder).
    param([Parameter(Mandatory)][string]$Path, [string]$Sid = 'S-1-5-32-545', [string]$Rights = 'Modify', [switch]$InheritOnly)

    $item = Get-Item -LiteralPath $Path -Force
    $sidObj = [System.Security.Principal.SecurityIdentifier]::new($Sid)
    if ($item -is [System.IO.DirectoryInfo]){
        $prop = (($InheritOnly) ? ('InheritOnly') : ('None'))
        $acl = [System.IO.FileSystemAclExtensions]::GetAccessControl($item, [System.Security.AccessControl.AccessControlSections]::Access)
        $acl.AddAccessRule([System.Security.AccessControl.FileSystemAccessRule]::new($sidObj, $Rights, 'ContainerInherit, ObjectInherit', $prop, 'Allow'))
        [System.IO.FileSystemAclExtensions]::SetAccessControl($item, $acl)
    }
    else{
        $acl = [System.IO.FileSystemAclExtensions]::GetAccessControl($item, [System.Security.AccessControl.AccessControlSections]::Access)
        $acl.AddAccessRule([System.Security.AccessControl.FileSystemAccessRule]::new($sidObj, $Rights, 'Allow'))
        [System.IO.FileSystemAclExtensions]::SetAccessControl($item, $acl)
    }
}

function Invoke-SetupScript {
    <#
      Runs scripts\<Script>.ps1 in a child pwsh with the given parameters (splatted from JSON, so switches and
      -Start:$false work) plus -TestHook SetupFakes.ps1, IDL_SETUP_TEST=1, a temp setup log folder and
      -NonInteractive. -AdminPassword / -DbDsn / -ServicePassword (-ServiceAccountPassword) are turned into SecureStrings inside the child from environment
      variables that are removed before the script starts.
      -NoTestEnv omits IDL_SETUP_TEST, -HookPath replaces the hook path (both for the hook-guard tests),
      -LogDir replaces the temp log folder.
      Returns { ExitCode; StdOut; StdErr; LogText; ReportText; AllText; LogDir; ScriptsLogsNew }.
    #>
    param(
        [Parameter(Mandatory)][string]$Script,
        [Parameter(Mandatory)][hashtable]$Params,
        [Parameter(Mandatory)][string]$StatePath,
        [string]$AdminPassword = '',
        [string]$DbDsn = '',
        [string]$ServicePassword = '',
        [hashtable]$ExtraEnv = @{},
        [switch]$NoTestEnv,
        [string]$HookPath = '',
        [string]$LogDir = '',
        [int]$TimeoutSec = 300
    )

    Assert-StubOnlyEnvironment

    $p = @{}
    foreach ($k in $Params.Keys){
        $p[$k] = $Params[$k]
    }

    $p['TestHook'] = (($HookPath) ? ($HookPath) : ($script:SetupHook))
    $p['NonInteractive'] = $true

    $code = @'
$ErrorActionPreference = 'Stop'
$p = $env:IDL_T_PARAMS | ConvertFrom-Json -AsHashtable
if ($env:IDL_T_ADMIN){ $p['AdminPassword'] = ConvertTo-SecureString -String $env:IDL_T_ADMIN -AsPlainText -Force }
if ($env:IDL_T_DSN){ $p['DbDsn'] = ConvertTo-SecureString -String $env:IDL_T_DSN -AsPlainText -Force }
if ($env:IDL_T_SVCPW){ $p['ServiceAccountPassword'] = ConvertTo-SecureString -String $env:IDL_T_SVCPW -AsPlainText -Force }
Remove-Item -Path env:IDL_T_ADMIN, env:IDL_T_DSN, env:IDL_T_SVCPW, env:IDL_T_PARAMS -ErrorAction SilentlyContinue
& $env:IDL_T_SCRIPT @p
exit $LASTEXITCODE
'@

    $ownLogDir = (-not $LogDir)
    if ($ownLogDir){
        $LogDir = Join-Path $script:StateDir ('setuplogs_' + [guid]::NewGuid().ToString('N').Substring(0, 12))
    }

    $envs = @{
        IDL_T_SCRIPT          = (Join-Path $script:ScriptsDir ($Script + '.ps1'))
        IDL_T_PARAMS          = (ConvertTo-Json -InputObject $p -Compress -Depth 5)
        IDL_T_ADMIN           = $AdminPassword
        IDL_T_DSN             = $DbDsn
        IDL_T_SVCPW           = $ServicePassword
        IDL_FAKE_STATE        = $StatePath
        IDL_SETUP_TEST        = '1'
        IDL_SETUP_TEST_LOGDIR = $LogDir
    }
    foreach ($k in $ExtraEnv.Keys){
        $envs[$k] = $ExtraEnv[$k]
    }

    $stub = New-StubStateFile
    $baseline = Get-LogDirSnapshot
    $psi = New-ChildProcessInfo -Arguments @('-NoProfile', '-NonInteractive', '-ExecutionPolicy', 'Bypass', '-Command', $code) -StatePath $stub -ExtraEnv $envs
    foreach ($k in @('IDL_T_ADMIN', 'IDL_T_DSN', 'IDL_T_SVCPW')){
        if (-not $envs[$k]){
            $null = $psi.Environment.Remove($k)
        }
    }

    if ($NoTestEnv){
        $null = $psi.Environment.Remove('IDL_SETUP_TEST')
    }

    $r = Invoke-ChildProcess -Psi $psi -TimeoutSec $TimeoutSec

    $logText = ''
    $reportText = ''
    if (Test-Path -LiteralPath $LogDir -PathType Container){
        $log = @(Get-ChildItem -LiteralPath $LogDir -Filter "$($Script)_2*.log" -File) | Select-Object -First 1
        $report = @(Get-ChildItem -LiteralPath $LogDir -Filter "$($Script)_Report_*.csv" -File) | Select-Object -First 1
        $logText = (($log) ? ([System.IO.File]::ReadAllText($log.FullName, $script:Utf8NoBom)) : (''))
        $reportText = (($report) ? ([System.IO.File]::ReadAllText($report.FullName, $script:Utf8NoBom)) : (''))
        if (
            ($ownLogDir) -and
            ((([System.IO.File]::GetAttributes($LogDir)) -band [System.IO.FileAttributes]::ReparsePoint) -eq 0)
        ){
            Remove-Item -LiteralPath $LogDir -Recurse -Force -ErrorAction SilentlyContinue
        }
    }

    # Anything that still lands in scripts\Logs (it must not, for these scripts) is reported and cleaned up.
    $scriptsLogsNew = @(Get-NewLogFiles -Baseline $baseline | Where-Object { (Split-Path -Leaf $_) -like "$($Script)_*" })
    Remove-NewLogFiles -Baseline $baseline
    Remove-StubStateFile -Path $stub

    return [pscustomobject]@{
        ExitCode       = $r.ExitCode
        StdOut         = $r.StdOutRaw
        StdErr         = $r.StdErr
        LogText        = $logText
        ReportText     = $reportText
        AllText        = ($logText + "`n" + $reportText + "`n" + $r.StdOutRaw + "`n" + $r.StdErr)
        LogDir         = $LogDir
        ScriptsLogsNew = $scriptsLogsNew
    }
}

function Get-ScriptFunctionText {
    # Source text of one function defined in a script (parsed, never executed).
    param([Parameter(Mandatory)][string]$Path, [Parameter(Mandatory)][string]$Name)

    $tokens = $null
    $errors = $null
    $ast = [System.Management.Automation.Language.Parser]::ParseFile($Path, [ref]$tokens, [ref]$errors)
    $fn = $ast.FindAll({ param($n) ($n -is [System.Management.Automation.Language.FunctionDefinitionAst]) -and ($n.Name -eq $Name) }, $true) | Select-Object -First 1
    if ($null -eq $fn){
        throw "Function $Name not found in $Path"
    }

    return $fn.Extent.Text
}
