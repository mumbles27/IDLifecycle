<#
.SYNOPSIS
    Safely uninstalls IDLifecycle: stops and deletes the Windows service, removes the event log source and the
    program folder, and - only when explicitly asked - backs up and removes the data folder and the DSN variable.

.DESCRIPTION
    Steps (each logged and reported; already-done steps are Skipped; -DryRun reports WOULD:):
      1. Stop the service (wait up to 60 s).
      2. Back up the data folder (and InstallDir\scripts\Logs) to -BackupTo: a new folder
         IDLifecycle-backup_<ServiceName>_<yyyyMMdd_HHmmss> that is restricted to SYSTEM, Administrators and you
         BEFORE the zip is written into it; the entry count is verified. On any failure the partial zip is deleted and
         NOTHING is removed (the run stops).
      3. sc.exe delete the service and wait until it is gone.
      4. Remove the "IDLifecycle" Application event log source - unless another service still runs idlifecycled.exe.
      5. Remove InstallDir (default: yes; -RemoveProgramFiles:$false keeps it). With the SHA256SUMS install manifest
         that Install-IDLifecycle.ps1 copies there, only the listed files (plus the generated *.log/*.csv in
         scripts\Logs) are deleted, then empty folders; anything else is left and reported.
      6. Remove DataDir only with -RemoveData AND (-Force or the service name typed as confirmation) AND (-BackupTo or
         an explicit -NoBackup). The data folder holds the audit log, the database (SQLite) and secret.key.
      7. With -RemoveDbDsnEnvVar, remove the machine environment variable named in config.json ("dsn": "env:IDL_...");
         only names matching ^IDL_[A-Za-z0-9_]+$ are ever removed.

    Safety: InstallDir, DataDir and BackupTo go through the path guard (Get-IDLUnsafePathReason in the Common module:
    no drive roots, Windows, Program Files/ProgramData/Users themselves, profiles, system folders, junctions, ...).
    A folder is deleted only when nobody but SYSTEM, Administrators, TrustedInstaller, you and the service account can
    change anything in it (so no one can swap in a junction while it is being deleted); every entry's reparse
    attribute is re-read right before it is deleted or descended into, links are removed without following them, and
    nothing outside the folder is ever touched. InstallDir is derived from the service's binPath only when that
    folder holds the install manifest; DataDir is removed only when it holds secret.key AND a config.json whose
    data_dir is that folder.

    It does NOT touch Active Directory (the gMSA and every account the service created or manages stay as they are)
    and it does NOT drop server databases (mssql, postgres, mysql, db2): the driver and database are printed so an
    administrator can drop them (and the login) by hand.

    Re-running it after a successful uninstall reports every step as Skipped and exits 0.

.PARAMETER ServiceName
    Windows service name. Default: IDLifecycle.

.PARAMETER InstallDir
    Program folder. Default: the folder of idlifecycled.exe in the service's binPath (only if it holds SHA256SUMS),
    else C:\Program Files\IDLifecycle.

.PARAMETER DataDir
    Data folder. Default: data_dir of the config.json named by the service's binPath (-config), else C:\ProgramData\IDLifecycle.

.PARAMETER ServiceAccount
    Account allowed to write in the data folder and scripts\Logs (default: the service's log-on account). Only
    needed with -RemoveData when the service is already gone.

.PARAMETER RemoveProgramFiles
    Remove InstallDir (default). -RemoveProgramFiles:$false keeps it.

.PARAMETER RemoveData
    Also remove DataDir (audit log, database, secret.key). Needs -Force (or a typed confirmation) and -BackupTo or -NoBackup.

.PARAMETER BackupTo
    Folder that receives IDLifecycle-backup_<ServiceName>_<yyyyMMdd_HHmmss>\<same>.zip of DataDir (+ scripts\Logs) first.

.PARAMETER NoBackup
    Confirms -RemoveData without a backup.

.PARAMETER RemoveDbDsnEnvVar
    Remove the machine environment variable that holds the DSN (config.json "dsn": "env:IDL_...").

.PARAMETER Force
    Confirms -RemoveData without a prompt.

.PARAMETER NonInteractive
    Never prompt.

.PARAMETER DryRun
    Report what would be done (WOULD:) and change nothing (no elevation needed).

.EXAMPLE
    .\Uninstall-IDLifecycle.ps1 -DryRun

.EXAMPLE
    .\Uninstall-IDLifecycle.ps1 -RemoveData -BackupTo D:\Backup -Force -RemoveDbDsnEnvVar

.NOTES
    Requires PowerShell 7+, Windows and an elevated session (not for -DryRun).
    Exit codes: 0 ok (also when already uninstalled), 1 some steps failed, 2 invalid input / refused by the path
    guard / missing confirmation, 3 dependency missing, 4 not elevated / access denied, 5 unexpected error.
    Logs and CSV report go to %ProgramData%\IDLifecycle-Setup\Logs (restricted to SYSTEM and Administrators); a
    non-elevated -DryRun logs to %LOCALAPPDATA%\IDLifecycle-Setup\Logs.
#>

#Requires -Version 7.0

[CmdletBinding()]
param(
    [string]$ServiceName = 'IDLifecycle',

    [string]$InstallDir = '',

    [string]$DataDir = '',

    [string]$ServiceAccount = '',

    [switch]$RemoveProgramFiles = $true,

    [switch]$RemoveData,

    [string]$BackupTo = '',

    [switch]$NoBackup,

    [switch]$RemoveDbDsnEnvVar,

    [switch]$Force,

    [switch]$NonInteractive,

    [switch]$DryRun,

    # Test seam: honoured only when IDL_SETUP_TEST=1 and the file lies in <repo>\tests\powershell (Get-IDLTestHookRefusal).
    [Parameter(DontShow)]
    [string]$TestHook = ''
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

Import-Module (Join-Path $PSScriptRoot 'IDLifecycle.Common.psm1') -Force

$script:FailedSteps = 0
$script:EventSourceName = 'IDLifecycle'
$script:ManifestName = 'SHA256SUMS'
$script:SetupLogRoot = Join-Path -Path $env:ProgramData -ChildPath 'IDLifecycle-Setup\Logs'
$script:SetupLogLock = $true
$script:SetupLogOverride = $false

#region System-touching functions (replaced by -TestHook in the Pester tests)

function Test-IDLSetupElevated {
    return (Test-IDLProcessElevated)
}

function Get-IDLSetupService {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Name
    )

    $svc = Get-CimInstance -ClassName Win32_Service -Filter ("Name='{0}'" -f $Name.Replace("'", "''")) -ErrorAction SilentlyContinue
    if ($null -eq $svc){
        return $null
    }

    return [pscustomobject]@{
        Name      = [string]$svc.Name
        State     = [string]$svc.State
        PathName  = [string]$svc.PathName
        StartName = [string]$svc.StartName
    }
}

function Get-IDLSetupDaemonServices {
    # Names of every service whose binPath runs idlifecycled.exe.
    return [string[]]@(Get-CimInstance -ClassName Win32_Service -ErrorAction SilentlyContinue |
            Where-Object { [string]$_.PathName -match 'idlifecycled\.exe' } |
            ForEach-Object { [string]$_.Name })
}

function Stop-IDLSetupService {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Name
    )

    Stop-Service -Name $Name -Force
}

function Remove-IDLSetupService {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Name
    )

    $output = & sc.exe delete $Name 2>&1
    if ($LASTEXITCODE -ne 0){
        throw "sc.exe delete $Name failed with exit code ${LASTEXITCODE}: $($output -join ' ')"
    }
}

function Test-IDLEventSource {
    # $true / $false, or $null when it cannot be determined (SourceExists scans every log, including Security).
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Source
    )

    try{
        return [System.Diagnostics.EventLog]::SourceExists($Source)
    }
    catch{
        return $null
    }
}

function Remove-IDLEventSource {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Source
    )

    [System.Diagnostics.EventLog]::DeleteEventSource($Source)
}

function Get-IDLMachineEnvVar {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Name
    )

    return [System.Environment]::GetEnvironmentVariable($Name, [System.EnvironmentVariableTarget]::Machine)
}

function Remove-IDLMachineEnvVar {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Name
    )

    [System.Environment]::SetEnvironmentVariable($Name, $null, [System.EnvironmentVariableTarget]::Machine)
}

function Get-IDLPathOwnerSid {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Path
    )

    return (Get-IDLOwnerSid -Path $Path)
}

function Read-IDLSetupText {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Prompt
    )

    return (Read-Host -Prompt $Prompt)
}

#endregion

#region Helpers

function Test-IDLSetupInteractive {
    if ($NonInteractive){
        return $false
    }

    if (
        (-not [Environment]::UserInteractive) -or
        ([Console]::IsInputRedirected)
    ){
        return $false
    }

    foreach ($arg in [Environment]::GetCommandLineArgs()){
        if ($arg -match '^-noni'){
            return $false
        }
    }

    return $true
}

function Test-IDLIsReparsePoint {
    # Fresh read of the attribute (never a cached enumeration value), right before acting on the path.
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Path
    )

    return ((([System.IO.File]::GetAttributes($Path)) -band [System.IO.FileAttributes]::ReparsePoint) -ne 0)
}

function Assert-IDLInsideRoot {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Path,
        [Parameter(Mandatory)][string]$Root
    )

    $full = [System.IO.Path]::GetFullPath($Path).TrimEnd('\')
    if (-not $full.StartsWith($Root.TrimEnd('\') + '\', [System.StringComparison]::OrdinalIgnoreCase)){
        throw "Refusing to delete '$full': it is not inside '$Root'."
    }

    # No folder between Root and the entry may have become a link since it was checked.
    $cursor = Split-Path -Parent $full
    while (
        ($cursor.Length -gt $Root.TrimEnd('\').Length) -and
        ($cursor.StartsWith($Root.TrimEnd('\') + '\', [System.StringComparison]::OrdinalIgnoreCase))
    ){
        if (Test-IDLIsReparsePoint -Path $cursor){
            throw "Refusing to delete below '$cursor': it became a junction or symbolic link."
        }

        $cursor = Split-Path -Parent $cursor
    }

    if (Test-IDLIsReparsePoint -Path $Root){
        throw "Refusing to delete: '$Root' became a junction or symbolic link."
    }
}

function Remove-IDLEntry {
    # Deletes one file, or a link (file or folder) without following it.
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Path,
        [Parameter(Mandatory)][string]$Root
    )

    Assert-IDLInsideRoot -Path $Path -Root $Root
    $attributes = [System.IO.File]::GetAttributes($Path)
    $isDir = (($attributes -band [System.IO.FileAttributes]::Directory) -ne 0)
    if (($attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0){
        if ($isDir){
            [System.IO.Directory]::Delete($Path, $false)
        }
        else{
            [System.IO.File]::Delete($Path)
        }

        return
    }

    if ($isDir){
        throw "'$Path' is a folder."
    }

    if (($attributes -band [System.IO.FileAttributes]::ReadOnly) -ne 0){
        [System.IO.File]::SetAttributes($Path, ($attributes -band (-bnot [System.IO.FileAttributes]::ReadOnly)))
    }

    [System.IO.File]::Delete($Path)
}

function Remove-IDLEmptyDirs {
    # Removes empty folders below Root (deepest first) and Root itself when it ends up empty; links are unlinked.
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Root,
        [string[]]$Keep = @()
    )

    $rootFull = [System.IO.Path]::GetFullPath($Root).TrimEnd('\')
    $dirs = @(Get-ChildItem -LiteralPath $rootFull -Recurse -Directory -Force -ErrorAction SilentlyContinue |
            Sort-Object { $_.FullName.Length } -Descending)
    foreach ($d in $dirs){
        if (-not (Test-Path -LiteralPath $d.FullName)){
            continue
        }

        if (@($Keep | Where-Object { $_ -and (Test-IDLPathWithin -Child $_ -Parent $d.FullName) }).Count -gt 0){
            continue
        }

        Assert-IDLInsideRoot -Path $d.FullName -Root $rootFull
        if (Test-IDLIsReparsePoint -Path $d.FullName){
            [System.IO.Directory]::Delete($d.FullName, $false)
            continue
        }

        if (@([System.IO.Directory]::EnumerateFileSystemEntries($d.FullName)).Count -eq 0){
            [System.IO.Directory]::Delete($d.FullName, $false)
        }
    }

    if (
        (@([System.IO.Directory]::EnumerateFileSystemEntries($rootFull)).Count -eq 0) -and
        (-not (Test-IDLIsReparsePoint -Path $rootFull))
    ){
        [System.IO.Directory]::Delete($rootFull, $false)
    }
}

function Remove-IDLTree {
    <#
      Deletes the folder's contents and then the folder itself, never following junctions/symbolic links (the link
      is removed, its target is not touched) and refusing any entry that is not below Root. Attributes are re-read
      right before each recursion/delete. Paths in -Keep (and their parent folders) are left in place.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Root,
        [string[]]$Keep = @()
    )

    $rootFull = [System.IO.Path]::GetFullPath($Root).TrimEnd('\')
    $keepFull = @($Keep | Where-Object { $_ } | ForEach-Object { [System.IO.Path]::GetFullPath($_).TrimEnd('\') })
    $cmp = [System.StringComparison]::OrdinalIgnoreCase

    $removeDir = $null
    $removeDir = {
        param([string]$Dir)

        $keptSomething = $false
        foreach ($entry in @([System.IO.Directory]::EnumerateFileSystemEntries($Dir))){
            $full = $entry.TrimEnd('\')
            Assert-IDLInsideRoot -Path $full -Root $rootFull
            if (@($keepFull | Where-Object { $_.Equals($full, $cmp) }).Count -gt 0){
                $keptSomething = $true
                continue
            }

            $attributes = [System.IO.File]::GetAttributes($full)
            $isDir = (($attributes -band [System.IO.FileAttributes]::Directory) -ne 0)
            if (
                ($isDir) -and
                (($attributes -band [System.IO.FileAttributes]::ReparsePoint) -eq 0)
            ){
                $holdsKept = (@($keepFull | Where-Object { $_.StartsWith($full + '\', $cmp) }).Count -gt 0)
                $inner = & $removeDir $full
                if (
                    $holdsKept -or
                    $inner
                ){
                    $keptSomething = $true
                    continue
                }

                Assert-IDLInsideRoot -Path $full -Root $rootFull
                if (Test-IDLIsReparsePoint -Path $full){
                    throw "Refusing to delete '$full': it became a junction or symbolic link."
                }

                [System.IO.Directory]::Delete($full, $false)
                continue
            }

            Remove-IDLEntry -Path $full -Root $rootFull
        }

        return $keptSomething
    }

    if (Test-IDLIsReparsePoint -Path $rootFull){
        throw "Refusing to delete '$rootFull': it is a junction or symbolic link."
    }

    $kept = & $removeDir $rootFull
    if (-not $kept){
        [System.IO.Directory]::Delete($rootFull, $false)
    }

    return [bool]$kept
}

function Remove-IDLManifestFiles {
    <#
      Deletes exactly the files listed in <Root>\SHA256SUMS, the generated *.log / *.csv files in scripts\Logs (unless
      kept), then SHA256SUMS and the empty folders. Returns the relative paths of everything that was left.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Root,
        [string[]]$Keep = @()
    )

    $rootFull = [System.IO.Path]::GetFullPath($Root).TrimEnd('\')
    $manifest = Join-Path -Path $rootFull -ChildPath $script:ManifestName
    $entries = Read-IDLManifest -Path $manifest
    foreach ($e in $entries){
        $target = Join-Path -Path $rootFull -ChildPath $e.Relative
        if (Test-Path -LiteralPath $target){
            Remove-IDLEntry -Path $target -Root $rootFull
        }
    }

    $logs = Join-Path -Path $rootFull -ChildPath 'scripts\Logs'
    $keepLogs = (@($Keep | Where-Object { $_ -and (Test-IDLPathWithin -Child $_ -Parent $logs) }).Count -gt 0)
    if (
        (-not $keepLogs) -and
        (Test-Path -LiteralPath $logs -PathType Container) -and
        (-not (Test-IDLIsReparsePoint -Path $logs))
    ){
        foreach ($file in @(Get-ChildItem -LiteralPath $logs -File -Force | Where-Object { $_.Extension -in @('.log', '.csv') })){
            Remove-IDLEntry -Path $file.FullName -Root $rootFull
        }
    }

    Remove-IDLEntry -Path $manifest -Root $rootFull
    Remove-IDLEmptyDirs -Root $rootFull -Keep $Keep

    if (-not (Test-Path -LiteralPath $rootFull)){
        return , [string[]]@()
    }

    return , [string[]]@(Get-ChildItem -LiteralPath $rootFull -Recurse -File -Force | ForEach-Object { $_.FullName.Substring($rootFull.Length).TrimStart('\') })
}

function New-IDLDataBackup {
    <#
      Creates <Destination>\<Name>\ (new, restricted to SYSTEM, Administrators and the current user BEFORE anything is
      written into it), zips DataDir (+ optional extra folders under their own prefix) into <Name>.zip there and
      verifies the entry count. On any failure the partial zip (and the empty folder) are deleted again.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Source,
        [Parameter(Mandatory)][string]$Destination,
        [Parameter(Mandatory)][string]$Name,
        [hashtable]$Extra = @{}
    )

    Add-Type -AssemblyName System.IO.Compression.FileSystem
    Initialize-IDLProtectedDir -Path $Destination
    $folder = Join-Path -Path $Destination -ChildPath $Name
    if (Test-Path -LiteralPath $folder){
        throw "Backup folder already exists: $folder"
    }

    Initialize-IDLProtectedDir -Path $folder -Lock -IncludeCurrentUser
    $zip = Join-Path -Path $folder -ChildPath ($Name + '.zip')
    try{
        $expected = 0
        $archive = [System.IO.Compression.ZipFile]::Open($zip, [System.IO.Compression.ZipArchiveMode]::Create)
        try{
            $sets = [ordered]@{ 'data' = $Source }
            foreach ($k in $Extra.Keys){
                $sets[[string]$k] = [string]$Extra[$k]
            }

            foreach ($prefix in $sets.Keys){
                $dir = [System.IO.Path]::GetFullPath($sets[$prefix]).TrimEnd('\')
                if (-not (Test-Path -LiteralPath $dir -PathType Container)){
                    continue
                }

                foreach ($file in @(Get-ChildItem -LiteralPath $dir -Recurse -File -Force -Attributes '!ReparsePoint')){
                    $rel = $file.FullName.Substring($dir.Length).TrimStart('\').Replace('\', '/')
                    $null = [System.IO.Compression.ZipFileExtensions]::CreateEntryFromFile($archive, $file.FullName, "$prefix/$rel", [System.IO.Compression.CompressionLevel]::Optimal)
                    $expected++
                }
            }
        }
        finally{
            $archive.Dispose()
        }

        $check = [System.IO.Compression.ZipFile]::OpenRead($zip)
        try{
            $count = $check.Entries.Count
        }
        finally{
            $check.Dispose()
        }

        if ($count -ne $expected){
            throw "Backup verification failed: $count entries in $zip, expected $expected."
        }

        return [pscustomobject]@{ Path = $zip; Files = $expected }
    }
    catch{
        if (Test-Path -LiteralPath $zip){
            Remove-Item -LiteralPath $zip -Force -ErrorAction SilentlyContinue
        }

        if (
            (Test-Path -LiteralPath $folder) -and
            (@(Get-ChildItem -LiteralPath $folder -Force).Count -eq 0)
        ){
            Remove-Item -LiteralPath $folder -Force -ErrorAction SilentlyContinue
        }

        throw
    }
}

function Wait-IDLSetupServiceGone {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Name,
        [int]$TimeoutSec = 30
    )

    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSec)
    while ($null -ne (Get-IDLSetupService -Name $Name)){
        if ([DateTime]::UtcNow -gt $deadline){
            return $false
        }

        Start-Sleep -Milliseconds 500
    }

    return $true
}

function Wait-IDLSetupServiceState {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Name,
        [Parameter(Mandatory)][string]$State,
        [int]$TimeoutSec = 60
    )

    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSec)
    while ($true){
        $svc = Get-IDLSetupService -Name $Name
        if (
            ($null -eq $svc) -or
            ($svc.State -eq $State)
        ){
            return
        }

        if ([DateTime]::UtcNow -gt $deadline){
            throw "Service '$Name' did not reach state $State within $TimeoutSec s (now: $($svc.State))."
        }

        Start-Sleep -Milliseconds 500
    }
}

function Invoke-SetupStep {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Action,
        [Parameter(Mandatory)][string]$Identity,
        [string]$Details = '',
        [Parameter(Mandatory)][scriptblock]$IsSatisfied,
        [Parameter(Mandatory)][scriptblock]$Apply,
        [string]$SatisfiedReason = 'already done'
    )

    Add-IDLCounter -Name 'Total'

    try{
        if (& $IsSatisfied){
            Write-IDLLog "SKIP: $Action $Identity ($SatisfiedReason)"
            Add-IDLCounter -Name 'Skipped'
            Add-IDLSkipReason -Reason $SatisfiedReason
            Add-IDLReportRow -Action $Action -Result 'Skipped' -Identity $Identity -Details $SatisfiedReason
            return
        }

        if ($DryRun){
            Write-IDLLog "WOULD: $Action $Identity $Details"
            Add-IDLCounter -Name 'WouldChange'
            Add-IDLCounter -Name 'Succeeded'
            Add-IDLReportRow -Action $Action -Result 'Would' -Identity $Identity -Details $Details
            return
        }

        & $Apply
        Write-IDLLog "DONE: $Action $Identity $Details"
        Add-IDLCounter -Name 'Changed'
        Add-IDLCounter -Name 'Succeeded'
        Add-IDLReportRow -Action $Action -Result 'Applied' -Identity $Identity -Details $Details
    }
    catch{
        $script:FailedSteps++
        Add-IDLCounter -Name 'Failed'
        Write-IDLLog "FAILED: $Action $Identity - $($_.Exception.Message)" 'ERROR'
        Add-IDLReportRow -Action $Action -Result 'Failed' -Identity $Identity -Details $Details -ErrorText $_.Exception.Message
    }
}

function Add-SetupSkip {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Action,
        [Parameter(Mandatory)][string]$Identity,
        [Parameter(Mandatory)][string]$Reason,
        [ValidateSet('INFO', 'WARN')][string]$Level = 'INFO'
    )

    Add-IDLCounter -Name 'Total'
    Add-IDLCounter -Name 'Skipped'
    Add-IDLSkipReason -Reason $Reason
    Write-IDLLog "SKIP: $Action $Identity ($Reason)" $Level
    Add-IDLReportRow -Action $Action -Result 'Skipped' -Identity $Identity -Details $Reason
}

function Assert-NoFailedSteps {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Phase
    )

    if ($script:FailedSteps -gt 0){
        $ex = New-IDLException -Kind Fatal -Message "Stopping after phase '$Phase': $($script:FailedSteps) step(s) failed. Nothing after this point was removed."
        $ex.Data['IDLStepFailure'] = $true
        throw $ex
    }
}

#endregion

if ($TestHook){
    $hookRefusal = Get-IDLTestHookRefusal -HookPath $TestHook -ScriptRoot $PSScriptRoot -InstallDir $InstallDir
    if ($hookRefusal){
        [Console]::Error.WriteLine("Uninstall-IDLifecycle: -TestHook refused ($hookRefusal); it is only for the Pester tests.")
        exit 2
    }

    [Console]::Error.WriteLine("Uninstall-IDLifecycle: TEST HOOK $TestHook replaces the system-touching functions.")
    . $TestHook
}

# Setup logs: a protected folder, never next to the scripts (which this run may delete).
try{
    if (
        (-not $script:SetupLogOverride) -and
        (-not (Test-IDLSetupElevated))
    ){
        $script:SetupLogRoot = Join-Path -Path $env:LOCALAPPDATA -ChildPath 'IDLifecycle-Setup\Logs'
        $script:SetupLogLock = $false
    }

    Initialize-IDLProtectedDir -Path $script:SetupLogRoot -Lock:$script:SetupLogLock
}
catch{
    [Console]::Error.WriteLine("Uninstall-IDLifecycle: cannot use the log folder '$($script:SetupLogRoot)': $($_.Exception.Message)")
    exit 2
}

$exitCode = 5

try{
    Start-IDLRun -ScriptName 'Uninstall-IDLifecycle' -ScriptRoot $PSScriptRoot -LogDir $script:SetupLogRoot -IsAction -DryRun:$DryRun -Parameters @{
        ServiceName        = $ServiceName
        InstallDir         = $InstallDir
        DataDir            = $DataDir
        ServiceAccount     = $ServiceAccount
        RemoveProgramFiles = [bool]$RemoveProgramFiles
        RemoveData         = [bool]$RemoveData
        BackupTo           = $BackupTo
        NoBackup           = [bool]$NoBackup
        RemoveDbDsnEnvVar  = [bool]$RemoveDbDsnEnvVar
        Force              = [bool]$Force
        NonInteractive     = [bool]$NonInteractive
        DryRun             = [bool]$DryRun
    }

    if ($TestHook){
        Write-IDLLog "TEST HOOK loaded: $TestHook (system-touching functions are replaced)" 'WARN'
    }

    Write-IDLLog "Setup log folder: $($script:SetupLogRoot)"

    # ---- Phase: validation (exit 2) - nothing is touched before this has passed ----
    $phase = Start-IDLPhase -Name 'Validate input'

    if (
        ($PSVersionTable.PSEdition -ne 'Core') -or
        (-not $IsWindows)
    ){
        throw (New-IDLException -Kind Dependency -Message 'This uninstaller only runs on Windows with PowerShell 7+.')
    }

    if ($ServiceName -notmatch '^[A-Za-z0-9_.\-]{1,80}$'){
        throw (New-IDLException -Kind Input -Message "ServiceName '$ServiceName' is not valid (letters, digits, '_', '.', '-' only).")
    }

    if (
        ($RemoveData) -and
        (-not $BackupTo) -and
        (-not $NoBackup)
    ){
        throw (New-IDLException -Kind Input -Message '-RemoveData needs -BackupTo <folder> (recommended) or an explicit -NoBackup.')
    }

    if (
        ($BackupTo) -and
        ($NoBackup)
    ){
        throw (New-IDLException -Kind Input -Message '-BackupTo and -NoBackup contradict each other.')
    }

    # Explicit values go through the path guard as given (an explicit '' is refused, not replaced by a default).
    foreach ($name in @('InstallDir', 'DataDir', 'BackupTo')){
        if ($PSBoundParameters.ContainsKey($name)){
            $value = [string]$PSBoundParameters[$name]
            $reason = Get-IDLUnsafePathReason -Path $value
            if ($reason){
                throw (New-IDLException -Kind Input -Message "$name '$value' refused: $reason.")
            }
        }
    }

    $service = Get-IDLSetupService -Name $ServiceName
    $serviceExe = ''
    $serviceConfig = ''
    if ($null -ne $service){
        $m = [regex]::Match($service.PathName, '^\s*"(?<exe>[^"]+)"\s+service\s+-config\s+"(?<cfg>[^"]+)"')
        if (
            (-not $m.Success) -or
            ((Split-Path -Leaf $m.Groups['exe'].Value) -ne 'idlifecycled.exe')
        ){
            throw (New-IDLException -Kind Input -Message "Service '$ServiceName' does not look like IDLifecycle (binPath: $($service.PathName)); refusing to remove it.")
        }

        $serviceExe = $m.Groups['exe'].Value
        $serviceConfig = $m.Groups['cfg'].Value
        Write-IDLLog "Service '$ServiceName' found: state $($service.State), account $($service.StartName), binPath $($service.PathName)"
    }
    else{
        Write-IDLLog "Service '$ServiceName' is not installed"
    }

    if (-not $PSBoundParameters.ContainsKey('InstallDir')){
        $candidate = Join-Path -Path $env:ProgramFiles -ChildPath 'IDLifecycle'
        if ($serviceExe){
            $exeDir = Split-Path -Parent $serviceExe
            if (Test-Path -LiteralPath (Join-Path -Path $exeDir -ChildPath $script:ManifestName) -PathType Leaf){
                $candidate = $exeDir
            }
            elseif (
                ($RemoveProgramFiles) -and
                (-not [System.IO.Path]::GetFullPath($exeDir).TrimEnd('\').Equals([System.IO.Path]::GetFullPath($candidate).TrimEnd('\'), [System.StringComparison]::OrdinalIgnoreCase))
            ){
                throw (New-IDLException -Kind Input -Message "The service runs '$serviceExe', but '$exeDir' holds no $($script:ManifestName) install manifest; pass -InstallDir explicitly (or -RemoveProgramFiles:`$false).")
            }
        }

        $InstallDir = $candidate
    }

    $configPath = $serviceConfig
    if (
        (-not $configPath) -and
        ($PSBoundParameters.ContainsKey('DataDir'))
    ){
        $configPath = Join-Path -Path $DataDir -ChildPath 'config.json'
    }

    if (-not $configPath){
        $configPath = Join-Path -Path (Join-Path -Path $env:ProgramData -ChildPath 'IDLifecycle') -ChildPath 'config.json'
    }

    $config = $null
    if (Test-Path -LiteralPath $configPath -PathType Leaf){
        try{
            $config = Get-Content -LiteralPath $configPath -Raw -Encoding utf8 | ConvertFrom-Json -AsHashtable
        }
        catch{
            Write-IDLLog "Could not read $configPath ($($_.Exception.Message)); using defaults." 'WARN'
        }
    }

    if (-not $PSBoundParameters.ContainsKey('DataDir')){
        $configured = [string](Get-IDLField -Object $config -Name 'data_dir')
        $DataDir = (($configured) ? ($configured) : (Split-Path -Parent $configPath))
    }

    foreach ($pair in @(@('InstallDir', $InstallDir), @('DataDir', $DataDir))){
        $reason = Get-IDLUnsafePathReason -Path $pair[1]
        if ($reason){
            throw (New-IDLException -Kind Input -Message "$($pair[0]) '$($pair[1])' refused: $reason.")
        }
    }

    $InstallDir = [System.IO.Path]::GetFullPath($InstallDir).TrimEnd('\')
    $DataDir = [System.IO.Path]::GetFullPath($DataDir).TrimEnd('\')

    if (
        ($RemoveProgramFiles) -and
        (-not $RemoveData) -and
        (Test-IDLPathWithin -Child $DataDir -Parent $InstallDir)
    ){
        throw (New-IDLException -Kind Input -Message "DataDir '$DataDir' is inside InstallDir '$InstallDir': removing the program files would delete the data. Use -RemoveProgramFiles:`$false (or -RemoveData).")
    }

    if ($BackupTo){
        $BackupTo = [System.IO.Path]::GetFullPath($BackupTo).TrimEnd('\')
        foreach ($inner in @($DataDir, $InstallDir)){
            if (Test-IDLPathWithin -Child $BackupTo -Parent $inner){
                throw (New-IDLException -Kind Input -Message "BackupTo '$BackupTo' must not be inside '$inner' (it would be removed with it).")
            }
        }
    }

    # The service account may legitimately write in the data folder and scripts\Logs.
    $accountName = (($ServiceAccount) ? ($ServiceAccount) : ((($null -ne $service) ? ($service.StartName) : (''))))
    $extraTrusted = @()
    if ($accountName){
        $lookup = (($accountName -eq 'LocalSystem') ? ('NT AUTHORITY\SYSTEM') : ($accountName))
        try{
            $extraTrusted = @(([System.Security.Principal.NTAccount]::new($lookup)).Translate([System.Security.Principal.SecurityIdentifier]).Value)
        }
        catch{
            Write-IDLLog "Could not resolve the service account '$accountName' to a SID; its write access in the data folder will block -RemoveData." 'WARN'
        }
    }

    $database = Get-IDLField -Object $config -Name 'database'
    $driver = [string](Get-IDLField -Object $database -Name 'driver')
    if (-not $driver){
        $driver = 'sqlite'
    }

    $dsn = [string](Get-IDLField -Object $database -Name 'dsn')
    $dsnVar = ''
    $dsnValue = $dsn
    if ($dsn -match '^env:(?<name>[A-Za-z_][A-Za-z0-9_]*)$'){
        $dsnVar = $Matches['name']
        $dsnValue = Get-IDLMachineEnvVar -Name $dsnVar
        if ([string]::IsNullOrEmpty($dsnValue)){
            $dsnValue = [System.Environment]::GetEnvironmentVariable($dsnVar)
        }
    }

    Register-IDLDsnSecrets -Dsn $dsnValue

    if (
        ($RemoveData) -and
        (-not $Force)
    ){
        if (-not (Test-IDLSetupInteractive)){
            throw (New-IDLException -Kind Input -Message "-RemoveData deletes '$DataDir' (audit log, database, secret.key): add -Force, or run interactively and confirm by typing the service name.")
        }

        $typed = [string](Read-IDLSetupText -Prompt "Type the service name '$ServiceName' to confirm PERMANENT removal of '$DataDir'")
        if ($typed.Trim() -cne $ServiceName){
            throw (New-IDLException -Kind Input -Message 'Confirmation did not match the service name; nothing was removed.')
        }

        Write-IDLLog 'Data removal confirmed interactively'
    }

    if (
        (-not $DryRun) -and
        (-not (Test-IDLSetupElevated))
    ){
        throw (New-IDLException -Kind Connection -Message 'Run this script from an elevated (Administrator) session (or use -DryRun).')
    }

    $runningInside = Test-IDLPathWithin -Child $PSScriptRoot -Parent $InstallDir
    $logsDir = Join-Path -Path $InstallDir -ChildPath 'scripts\Logs'
    Write-IDLLog "InstallDir '$InstallDir' (remove: $([bool]$RemoveProgramFiles)); DataDir '$DataDir' (remove: $([bool]$RemoveData)); database driver $driver"
    Stop-IDLPhase -Phase $phase

    # ---- Phase: stop the service ----
    $phase = Start-IDLPhase -Name 'Stop service'
    if ($null -eq $service){
        Add-SetupSkip -Action 'StopService' -Identity $ServiceName -Reason 'service not installed'
    }
    else{
        Invoke-SetupStep -Action 'StopService' -Identity $ServiceName `
            -SatisfiedReason 'already stopped' `
            -IsSatisfied { $service.State -eq 'Stopped' } `
            -Apply {
            Stop-IDLSetupService -Name $ServiceName
            Wait-IDLSetupServiceState -Name $ServiceName -State 'Stopped' -TimeoutSec 60
        }
    }

    Assert-NoFailedSteps -Phase 'Stop service'
    Stop-IDLPhase -Phase $phase

    # ---- Phase: backup (a failure stops everything: nothing is removed) ----
    $phase = Start-IDLPhase -Name 'Backup'
    if (-not $BackupTo){
        Add-SetupSkip -Action 'BackupData' -Identity $DataDir -Reason ((($NoBackup) ? ('not wanted (-NoBackup)') : ('not requested (-BackupTo)')))
    }
    elseif (-not (Test-Path -LiteralPath $DataDir -PathType Container)){
        Add-SetupSkip -Action 'BackupData' -Identity $DataDir -Reason 'data folder not present'
    }
    else{
        $backupName = 'IDLifecycle-backup_{0}_{1}' -f $ServiceName, (Get-Date -Format 'yyyyMMdd_HHmmss')
        Invoke-SetupStep -Action 'BackupData' -Identity $DataDir -Details "-> $(Join-Path -Path $BackupTo -ChildPath "$backupName\$backupName.zip") (data + scripts\Logs; folder restricted to SYSTEM, Administrators, current user)" `
            -IsSatisfied { $false } `
            -Apply {
            $backup = New-IDLDataBackup -Source $DataDir -Destination $BackupTo -Name $backupName -Extra @{ 'scripts-Logs' = $logsDir }
            Write-IDLLog "  backup: $($backup.Path) ($($backup.Files) file(s)); it contains secret.key and the database - store it like the server itself" 'WARN'
        }
    }

    Assert-NoFailedSteps -Phase 'Backup'
    Stop-IDLPhase -Phase $phase

    # ---- Phase: delete the service and the event source ----
    $phase = Start-IDLPhase -Name 'Delete service'
    if ($null -eq $service){
        Add-SetupSkip -Action 'DeleteService' -Identity $ServiceName -Reason 'service not installed'
    }
    else{
        Invoke-SetupStep -Action 'DeleteService' -Identity $ServiceName -Details 'sc.exe delete' `
            -IsSatisfied { $false } `
            -Apply {
            Remove-IDLSetupService -Name $ServiceName
            if (-not (Wait-IDLSetupServiceGone -Name $ServiceName -TimeoutSec 30)){
                Write-IDLLog "Service '$ServiceName' is marked for deletion but still listed; it disappears when every handle is closed (close services.msc / Event Viewer) or after a reboot." 'WARN'
            }
        }
    }

    Assert-NoFailedSteps -Phase 'Delete service'

    $sourceState = Test-IDLEventSource -Source $script:EventSourceName
    $otherServices = @(Get-IDLSetupDaemonServices | Where-Object { $_ -ne $ServiceName })
    if ($sourceState -eq $false){
        Add-SetupSkip -Action 'RemoveEventSource' -Identity "Application/$($script:EventSourceName)" -Reason 'not registered'
    }
    elseif ($otherServices.Count -gt 0){
        Add-SetupSkip -Action 'RemoveEventSource' -Identity "Application/$($script:EventSourceName)" -Reason "still used by service(s) $($otherServices -join ', ')"
    }
    else{
        Invoke-SetupStep -Action 'RemoveEventSource' -Identity "Application/$($script:EventSourceName)" -Details ((($null -eq $sourceState) ? ('if registered (could not check without elevation)') : (''))) `
            -IsSatisfied { $false } `
            -Apply {
            if ($null -eq $sourceState){
                throw 'Could not determine whether the event source exists (access denied).'
            }

            Remove-IDLEventSource -Source $script:EventSourceName
        }
    }

    Assert-NoFailedSteps -Phase 'Delete service'
    Stop-IDLPhase -Phase $phase

    # ---- Phase: program files ----
    $phase = Start-IDLPhase -Name 'Program files'
    $manifestPath = Join-Path -Path $InstallDir -ChildPath $script:ManifestName
    $hasManifest = Test-Path -LiteralPath $manifestPath -PathType Leaf
    $hasMarkers = (
        (Test-Path -LiteralPath (Join-Path -Path $InstallDir -ChildPath 'idlifecycled.exe') -PathType Leaf) -or
        (Test-Path -LiteralPath (Join-Path -Path $InstallDir -ChildPath 'scripts\IDLifecycle.Common.psm1') -PathType Leaf)
    )
    if (-not $RemoveProgramFiles){
        Add-SetupSkip -Action 'RemoveInstallDir' -Identity $InstallDir -Reason 'kept (-RemoveProgramFiles:$false)'
    }
    elseif (-not (Test-Path -LiteralPath $InstallDir -PathType Container)){
        Add-SetupSkip -Action 'RemoveInstallDir' -Identity $InstallDir -Reason 'not present'
    }
    elseif (
        (-not $hasManifest) -and
        (-not $hasMarkers) -and
        (@(Get-ChildItem -LiteralPath $InstallDir -Recurse -File -Force | Where-Object { $_.Extension -in @('.exe', '.ps1', '.psm1', '.psd1') }).Count -eq 0)
    ){
        # What an earlier manifest-based removal left behind (files it does not own): nothing to do any more.
        $left = @(Get-ChildItem -LiteralPath $InstallDir -Recurse -File -Force | ForEach-Object { $_.FullName.Substring($InstallDir.Length).TrimStart('\') })
        Add-SetupSkip -Action 'RemoveInstallDir' -Identity $InstallDir -Reason "no IDLifecycle program files left ($($left.Count) other file(s) kept)"
    }
    else{
        $keep = @()
        if ($runningInside){
            $keep = @($logsDir)
            Write-IDLLog "This script runs from inside InstallDir: '$logsDir' is kept; delete it by hand afterwards." 'WARN'
        }

        Invoke-SetupStep -Action 'RemoveInstallDir' -Identity $InstallDir -Details ((($hasManifest) ? ("files listed in $($script:ManifestName)") : ('whole folder (no install manifest)'))) `
            -IsSatisfied { $false } `
            -Apply {
            if (
                (-not $hasManifest) -and
                (-not $hasMarkers)
            ){
                throw "'$InstallDir' holds neither $($script:ManifestName), idlifecycled.exe nor scripts\IDLifecycle.Common.psm1; refusing to delete a folder that does not look like an IDLifecycle install."
            }

            $untrusted = Get-IDLUntrustedTreeReason -Path $InstallDir -ExcludeRelative @('scripts\Logs') -ExtraTrustedSids $extraTrusted -OwnerSidResolver { param($p) Get-IDLPathOwnerSid -Path $p }
            if ($untrusted){
                throw "refusing to delete '$InstallDir' while others can change it ($untrusted); fix its ACL (re-run Install-IDLifecycle.ps1) or delete it by hand."
            }

            if ($hasManifest){
                $left = Remove-IDLManifestFiles -Root $InstallDir -Keep $keep
                if ($left.Count -gt 0){
                    Write-IDLLog "  left in '$InstallDir' (not part of the install): $($left -join ', ')" 'WARN'
                }
            }
            else{
                $null = Remove-IDLTree -Root $InstallDir -Keep $keep
            }
        }
    }

    Assert-NoFailedSteps -Phase 'Program files'
    Stop-IDLPhase -Phase $phase

    # ---- Phase: data ----
    $phase = Start-IDLPhase -Name 'Data'
    if (-not $RemoveData){
        Add-SetupSkip -Action 'RemoveDataDir' -Identity $DataDir -Reason 'kept (default: it holds the audit log, database and secret.key; pass -RemoveData)'
    }
    elseif (-not (Test-Path -LiteralPath $DataDir -PathType Container)){
        Add-SetupSkip -Action 'RemoveDataDir' -Identity $DataDir -Reason 'not present'
    }
    else{
        Invoke-SetupStep -Action 'RemoveDataDir' -Identity $DataDir `
            -IsSatisfied { $false } `
            -Apply {
            $cfgFile = Join-Path -Path $DataDir -ChildPath 'config.json'
            $keyFile = Join-Path -Path $DataDir -ChildPath 'secret.key'
            $ownDataDir = ''
            if (Test-Path -LiteralPath $cfgFile -PathType Leaf){
                try{
                    $ownDataDir = [string](Get-IDLField -Object (Get-Content -LiteralPath $cfgFile -Raw -Encoding utf8 | ConvertFrom-Json -AsHashtable) -Name 'data_dir')
                }
                catch{
                    $ownDataDir = ''
                }
            }

            if (
                (-not (Test-Path -LiteralPath $keyFile -PathType Leaf)) -or
                (-not $ownDataDir) -or
                (-not [System.IO.Path]::GetFullPath($ownDataDir).TrimEnd('\').Equals($DataDir, [System.StringComparison]::OrdinalIgnoreCase))
            ){
                throw "'$DataDir' is not an IDLifecycle data folder (it needs secret.key and a config.json whose data_dir is this folder); refusing to delete it."
            }

            $untrusted = Get-IDLUntrustedTreeReason -Path $DataDir -ExtraTrustedSids $extraTrusted -OwnerSidResolver { param($p) Get-IDLPathOwnerSid -Path $p }
            if ($untrusted){
                throw "refusing to delete '$DataDir' while others can change it ($untrusted); pass -ServiceAccount if that is the service account, or delete it by hand."
            }

            $null = Remove-IDLTree -Root $DataDir
        }
    }

    if (-not $dsnVar){
        if ($RemoveDbDsnEnvVar){
            Add-SetupSkip -Action 'RemoveDbDsnEnvVar' -Identity '(none)' -Reason 'config.json does not read the DSN from an environment variable (or config.json was not found)'
        }
    }
    elseif ($dsnVar -cnotmatch '^IDL_[A-Za-z0-9_]{1,120}$'){
        Add-SetupSkip -Action 'RemoveDbDsnEnvVar' -Identity "Machine:$dsnVar" -Level 'WARN' -Reason 'not an IDL_* variable - never removed by this script; remove it yourself if it is no longer needed'
    }
    elseif (-not $RemoveDbDsnEnvVar){
        Add-SetupSkip -Action 'RemoveDbDsnEnvVar' -Identity "Machine:$dsnVar" -Reason 'kept (pass -RemoveDbDsnEnvVar)'
    }
    else{
        Invoke-SetupStep -Action 'RemoveDbDsnEnvVar' -Identity "Machine:$dsnVar" `
            -SatisfiedReason 'not set' `
            -IsSatisfied { [string]::IsNullOrEmpty((Get-IDLMachineEnvVar -Name $dsnVar)) } `
            -Apply { Remove-IDLMachineEnvVar -Name $dsnVar }
    }

    Assert-NoFailedSteps -Phase 'Data'
    Stop-IDLPhase -Phase $phase

    Write-IDLLog '---- Not touched by this script ----'
    $account = (($accountName) ? ($accountName) : ('the service account'))
    Write-IDLLog "Active Directory: NOT changed. $account and every account the service created, disabled or manages remain as they are; remove the gMSA / delegations yourself if no longer needed."
    if ($driver -eq 'sqlite'){
        $kept = ((($RemoveData) -and (-not $DryRun)) ? ('removed with the data folder') : ("kept in $DataDir"))
        Write-IDLLog "Database: sqlite file ($kept)."
    }
    else{
        Write-IDLLog "Database: $driver $(Get-IDLDsnTarget -Dsn $dsnValue) was NOT dropped. Drop it (and its login) manually if it is no longer needed." 'WARN'
    }

    $exitCode = (($script:FailedSteps -gt 0) ? (1) : (0))
}
catch{
    if ($_.Exception.Data.Contains('IDLStepFailure')){
        $exitCode = 1
        Write-IDLLog $_.Exception.Message 'ERROR'
    }
    else{
        $exitCode = Get-IDLExitCode -Exception $_.Exception
        Write-IDLLog "FATAL: $($_.Exception.Message)" 'ERROR'
        Write-IDLLog "Stack: $($_.ScriptStackTrace)" 'DEBUG'
    }
}
finally{
    $dsnValue = $null
    Stop-IDLRun -ExitCode $exitCode
}

exit $exitCode
