# Shared helpers for the IDLifecycle Pester (3.x) tests. Dot-source from each *.Tests.ps1.
#
# SAFETY: scripts under test are ALWAYS run in a child pwsh whose PSModulePath contains ONLY the stub directory,
# with IDL_STUB_AD_STATE pointing at a throw-away JSON file. Before the first run we prove (in such a child) that
# `ActiveDirectory` resolves to the stub (version 99.0.0) and abort loudly otherwise. This process never imports
# the ActiveDirectory module.


$script:TestsRoot = Split-Path -Parent $MyInvocation.MyCommand.Path
$script:RepoRoot = Split-Path -Parent (Split-Path -Parent $script:TestsRoot)
$script:ScriptsDir = Join-Path $script:RepoRoot 'scripts'
$script:StubDir = Join-Path $script:TestsRoot 'StubAD'
$script:LogsDir = Join-Path $script:ScriptsDir 'Logs'
$script:StateDir = Join-Path ([System.IO.Path]::GetTempPath()) 'idl_pester'
$script:Pwsh = [System.Environment]::ProcessPath
if (
    ([string]::IsNullOrEmpty($script:Pwsh)) -or
    ((Split-Path -Leaf $script:Pwsh) -notmatch '^pwsh')
){
    $script:Pwsh = 'C:\Program Files\PowerShell\7\pwsh.exe'
}

$script:PreflightDone = $false
$script:Utf8NoBom = New-Object System.Text.UTF8Encoding($false)
$script:Root = 'DC=stub,DC=test'

function New-ChildProcessInfo {
    param([string[]]$Arguments, [string]$StatePath, [bool]$NoState = $false, [hashtable]$ExtraEnv = @{})

    $psi = New-Object System.Diagnostics.ProcessStartInfo
    $psi.FileName = $script:Pwsh
    foreach ($a in $Arguments){
        $psi.ArgumentList.Add($a)
    }

    $psi.UseShellExecute = $false
    $psi.CreateNoWindow = $true
    $psi.RedirectStandardInput = $true
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError = $true
    $psi.StandardInputEncoding = $script:Utf8NoBom
    $psi.StandardOutputEncoding = $script:Utf8NoBom
    $psi.StandardErrorEncoding = $script:Utf8NoBom
    $psi.WorkingDirectory = $script:RepoRoot

    # The one and only module path: the stub directory.
    $psi.Environment['PSModulePath'] = $script:StubDir
    if ($NoState){
        $null = $psi.Environment.Remove('IDL_STUB_AD_STATE')
    }
    else{
        $psi.Environment['IDL_STUB_AD_STATE'] = $StatePath
    }

    foreach ($key in $ExtraEnv.Keys){
        $psi.Environment[[string]$key] = [string]$ExtraEnv[$key]
    }

    return $psi
}

function Invoke-ChildProcess {
    param($Psi, [string]$StdIn = '', [int]$TimeoutSec = 180)

    $p = [System.Diagnostics.Process]::Start($Psi)
    $outTask = $p.StandardOutput.ReadToEndAsync()
    $errTask = $p.StandardError.ReadToEndAsync()
    $p.StandardInput.Write($StdIn)
    $p.StandardInput.Close()

    if (-not $p.WaitForExit($TimeoutSec * 1000)){
        try{
            $p.Kill($true)
        }
        catch{
            Write-Warning "Could not kill timed-out child: $($_.Exception.Message)"
        }
        throw "Child pwsh timed out after $TimeoutSec s"
    }

    $p.WaitForExit()
    return [pscustomobject]@{
        ExitCode  = $p.ExitCode
        StdOutRaw = $outTask.Result
        StdErr    = $errTask.Result
    }
}

function Assert-StubOnlyEnvironment {
    # Aborts loudly unless a child pwsh with the restricted module path resolves ActiveDirectory to the stub.
    if ($script:PreflightDone){
        return
    }

    $state = Join-Path $script:StateDir 'preflight.json'
    $null = New-Item -ItemType Directory -Force -Path $script:StateDir
    [System.IO.File]::WriteAllText($state, '{}', $script:Utf8NoBom)

    $probe = @'
$all = @(Get-Module ActiveDirectory -ListAvailable | ForEach-Object { "$($_.Version)|$($_.Path)" })
Import-Module ActiveDirectory -ErrorAction Stop
$m = Get-Module ActiveDirectory
[pscustomobject]@{ Version = $m.Version.ToString(); Path = $m.Path; Available = $all } | ConvertTo-Json -Compress
'@
    $psi = New-ChildProcessInfo -Arguments @('-NoProfile', '-NonInteractive', '-Command', $probe) -StatePath $state
    $r = Invoke-ChildProcess -Psi $psi -TimeoutSec 60
    if ($r.ExitCode -ne 0){
        throw "SAFETY ABORT: stub preflight failed (exit $($r.ExitCode)): $($r.StdErr) $($r.StdOutRaw)"
    }

    $info = $r.StdOutRaw | ConvertFrom-Json
    if (
        ($info.Version -ne '99.0.0') -or
        (-not ([string]$info.Path).StartsWith($script:StubDir, [System.StringComparison]::OrdinalIgnoreCase))
    ){
        throw "SAFETY ABORT: ActiveDirectory in the child resolved to '$($info.Path)' version '$($info.Version)', not the stub. Refusing to run any test."
    }

    $script:PreflightDone = $true
}

function New-StubStateFile {
    # Writes a state file and returns its path. Each argument is a hashtable/array in the documented schema.
    param(
        [object[]]$Users = @(),
        [object[]]$Groups = @(),
        [string[]]$Ous = @(),
        [object[]]$Objects = @(),
        [object[]]$Faults = @(),
        [bool]$FailConnect = $false,
        [hashtable]$Schema = @{}
    )

    $null = New-Item -ItemType Directory -Force -Path $script:StateDir
    $path = Join-Path $script:StateDir ([guid]::NewGuid().ToString() + '.json')
    $state = [ordered]@{
        users       = @($Users)
        groups      = @($Groups)
        ous         = @($Ous)
        objects     = @($Objects)
        calls       = @()
        faults      = @($Faults)
        failConnect = $FailConnect
        schema      = $Schema
    }
    [System.IO.File]::WriteAllText($path, (ConvertTo-Json -InputObject $state -Depth 20), $script:Utf8NoBom)
    return $path
}

function Read-StubStateFile {
    param([string]$Path)

    return (ConvertFrom-Json -InputObject ([System.IO.File]::ReadAllText($Path, $script:Utf8NoBom)))
}

function Get-StubCalls {
    # Mutating calls recorded by the stub (array, possibly empty).
    param([string]$Path, [string]$Cmdlet = '')

    $calls = @((Read-StubStateFile -Path $Path).calls)
    if ($Cmdlet){
        $calls = @($calls | Where-Object { $_.cmdlet -eq $Cmdlet })
    }

    return , $calls
}

function Get-StubUser {
    param([string]$Path, [string]$Sam = '', [string]$ExternalId = '')

    $users = @((Read-StubStateFile -Path $Path).users)
    foreach ($u in $users){
        if ($Sam -and ($u.sam -eq $Sam)){
            return $u
        }

        if (
            $ExternalId -and
            $u.attributes.PSObject.Properties['employeeID'] -and
            ($u.attributes.employeeID -eq $ExternalId)
        ){
            return $u
        }
    }

    return $null
}

function New-StubUserEntry {
    param(
        [string]$Sam,
        [string]$ExternalId,
        [string]$Ou = "OU=Staff,$script:Root",
        [string]$Cn = '',
        [bool]$Enabled = $true,
        [hashtable]$Attributes = @{},
        [string[]]$MemberOf = @(),
        [string]$ObjectClass = 'user',
        [string]$Guid = ''
    )

    if (-not $Cn){
        $Cn = $Sam
    }

    if (-not $Guid){
        $Guid = [guid]::NewGuid().ToString()
    }

    $attrs = [ordered]@{}
    if ($ExternalId){
        $attrs['employeeID'] = $ExternalId
    }

    foreach ($k in $Attributes.Keys){
        $attrs[$k] = $Attributes[$k]
    }

    return [ordered]@{
        objectGuid  = $Guid
        dn          = "CN=$Cn,$Ou"
        sam         = $Sam
        upn         = "$Sam@stub.test"
        enabled     = $Enabled
        objectClass = $ObjectClass
        attributes  = $attrs
        memberOf    = @($MemberOf)
    }
}

function New-StandardState {
    # Standard fixture: OUs, groups and (optionally) extra users.
    param([object[]]$Users = @(), [object[]]$Faults = @(), [bool]$FailConnect = $false, [object[]]$Objects = @())

    $ous = @("OU=Staff,$script:Root", "OU=Sales,$script:Root", "OU=Disabled,$script:Root", "OU=Groups,$script:Root")
    $groups = @(
        @{ dn = "CN=GG-AllStaff,OU=Groups,$script:Root"; sam = 'GG-AllStaff' },
        @{ dn = "CN=GG-Sales,OU=Groups,$script:Root"; sam = 'GG-Sales' },
        @{ dn = "CN=GG-Old,OU=Groups,$script:Root"; sam = 'GG-Old' }
    )

    return (New-StubStateFile -Users $Users -Groups $groups -Ous $ous -Objects $Objects -Faults $Faults -FailConnect $FailConnect)
}

function Get-LogDirSnapshot {
    if (-not (Test-Path -LiteralPath $script:LogsDir)){
        return @()
    }

    return @(Get-ChildItem -LiteralPath $script:LogsDir -File | ForEach-Object { $_.FullName })
}

function Get-NewLogFiles {
    param([string[]]$Baseline)

    return @(Get-LogDirSnapshot | Where-Object { $Baseline -notcontains $_ })
}

function Remove-NewLogFiles {
    param([string[]]$Baseline)

    foreach ($f in (Get-NewLogFiles -Baseline $Baseline)){
        Remove-Item -LiteralPath $f -Force -ErrorAction SilentlyContinue
    }
}

function Test-SingleJsonDocument {
    # True when the text is exactly one JSON document (strict parser; trailing content is an error).
    param([string]$Text)

    if ([string]::IsNullOrWhiteSpace($Text)){
        return $false
    }

    try{

        $doc = [System.Text.Json.JsonDocument]::Parse($Text.Trim())
        $doc.Dispose()
        return (($Text.Trim() -split "`n").Count -eq 1)
    }
    catch{
        return $false
    }
}

function Invoke-IDLScript {
    <#
      Runs scripts\<Script>.ps1 in a child pwsh against the stub and returns
      { ExitCode; StdOut; StdErr; Json; SingleJson; LogFile; ReportFile; Report }.
      -Request is serialised to JSON (hashtable/ordered); -RawInput overrides it.
    #>
    param(
        [Parameter(Mandatory)][string]$Script,
        $Request = $null,
        $RawInput = $null,
        [string]$StatePath = '',
        [switch]$DryRun,
        [switch]$NoState,
        [switch]$KeepFiles,
        [string[]]$ScriptArguments = @(),
        [hashtable]$ExtraEnv = @{},
        [int]$TimeoutSec = 180
    )

    Assert-StubOnlyEnvironment

    if (-not $NoState -and -not $StatePath){
        throw 'Invoke-IDLScript requires -StatePath (or -NoState).'
    }

    $baseline = Get-LogDirSnapshot
    $scriptPath = Join-Path $script:ScriptsDir ($Script + '.ps1')
    $arguments = @('-NoProfile', '-NonInteractive', '-ExecutionPolicy', 'Bypass', '-File', $scriptPath) + $ScriptArguments
    if ($DryRun){
        $arguments += '-DryRun'
    }

    $stdin = ''
    if ($null -ne $RawInput){
        $stdin = $RawInput
    }
    elseif ($null -ne $Request){
        $stdin = ConvertTo-Json -InputObject $Request -Depth 20 -Compress
    }

    $psi = New-ChildProcessInfo -Arguments $arguments -StatePath $StatePath -NoState:$NoState.IsPresent -ExtraEnv $ExtraEnv
    $r = Invoke-ChildProcess -Psi $psi -StdIn $stdin -TimeoutSec $TimeoutSec

    $json = $null
    try{
        if (-not [string]::IsNullOrWhiteSpace($r.StdOutRaw)){
            $json = $r.StdOutRaw | ConvertFrom-Json
        }
    }
    catch{
        $json = $null
    }

    $new = Get-NewLogFiles -Baseline $baseline
    $log = @($new | Where-Object { $_ -like "*\$($Script)_2*.log" }) | Select-Object -First 1
    $report = @($new | Where-Object { $_ -like "*\$($Script)_Report_*.csv" }) | Select-Object -First 1

    $logText = (($log) ? ([System.IO.File]::ReadAllText($log, $script:Utf8NoBom)) : (''))
    $reportText = (($report) ? ([System.IO.File]::ReadAllText($report, $script:Utf8NoBom)) : (''))
    if (-not $KeepFiles){
        # Tests read the content above; the files themselves are removed so scripts\Logs stays clean.
        Remove-NewLogFiles -Baseline $baseline
    }

    return [pscustomobject]@{
        ExitCode   = $r.ExitCode
        StdOut     = $r.StdOutRaw
        StdErr     = $r.StdErr
        Json       = $json
        SingleJson = (Test-SingleJsonDocument -Text $r.StdOutRaw)
        LogFile    = $log
        ReportFile = $report
        LogText    = $logText
        ReportText = $reportText
        NewFiles   = $new
    }
}

function Remove-StubStateFile {
    param([string]$Path)

    if ($Path -and (Test-Path -LiteralPath $Path)){
        Remove-Item -LiteralPath $Path -Force -ErrorAction SilentlyContinue
    }
}

function Invoke-StubSnippet {
    # Runs arbitrary PowerShell in a stub-only child (for testing the stub itself). Returns { ExitCode; StdOut; StdErr }.
    param([string]$Code, [string]$StatePath, [switch]$NoState)

    Assert-StubOnlyEnvironment
    $psi = New-ChildProcessInfo -Arguments @('-NoProfile', '-NonInteractive', '-Command', $Code) -StatePath $StatePath -NoState:$NoState.IsPresent
    return (Invoke-ChildProcess -Psi $psi -TimeoutSec 60)
}
