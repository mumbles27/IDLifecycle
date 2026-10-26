<#
.SYNOPSIS
    Builds an IDLifecycle release folder: idlifecycled.exe, scripts\ and a SHA256SUMS manifest, restricted so that
    Install-IDLifecycle.ps1 will trust it.

.DESCRIPTION
    1. Creates -OutDir (it must not exist yet, or be empty) and restricts it: inheritance off, SYSTEM, Administrators and
       you FullControl, owner Administrators when elevated (otherwise you). Install-IDLifecycle.ps1 refuses a release
       folder that anyone else can change - e.g. one below C:\, where BUILTIN\Users may create and change folders.
    2. Puts idlifecycled.exe at the root: -Build runs `go build` in -RepoRoot (with -Version stamped in), or -ExePath
       copies a binary you built/signed yourself.
    3. Copies -RepoRoot\scripts (every file, Logs folders excluded) to <OutDir>\scripts, and -RepoRoot\Setup.cmd (the
       double-click launcher for the setup window) to <OutDir>\Setup.cmd.
    4. Writes <OutDir>\SHA256SUMS ("<sha256>  <path>" for every shipped file) and checks that the folder, its files
       and its parent folders pass the installer's trust rules.
    5. With -Zip, also writes <OutDir>.zip (for servers without Go or internet access).
    Then double-click <OutDir>\Setup.cmd (the setup window), or run <OutDir>\scripts\Install-IDLifecycle.ps1 (its default
    SourceDir is <OutDir>). Servers with Go and access to GitHub can skip this script: the installer and the setup
    window can build from the repository themselves (-FromGitHub).

    SHA256SUMS sits next to the files it describes, so it proves the release was not changed after it was built
    (integrity) - not who built it. The ACL on the folder is what keeps others from changing both. Sign the exe
    (Authenticode) and use the installer's -RequireSignature for authenticity.

.PARAMETER OutDir
    Release folder to create, e.g. C:\Install\IDLifecycle-1.2.0.

.PARAMETER Build
    Build idlifecycled.exe with `go build` (Go 1.22+ on PATH).

.PARAMETER Version
    Version stamped into the binary with -Build (default: dev).

.PARAMETER ExePath
    Use this prebuilt idlifecycled.exe instead of -Build.

.PARAMETER RepoRoot
    Repository root (holds scripts\ and cmd\idlifecycled). Default: the parent of this script's folder.

.PARAMETER LogDir
    Log folder. Default: %LOCALAPPDATA%\IDLifecycle-Setup\Logs.

.PARAMETER Zip
    Also write <OutDir>.zip. The installer accepts it as -SourceDir; the setup window as 'Release zip or folder'.

.EXAMPLE
    .\scripts\New-IDLifecycleRelease.ps1 -OutDir C:\Install\IDLifecycle-1.2.0 -Build -Version 1.2.0

.EXAMPLE
    .\scripts\New-IDLifecycleRelease.ps1 -OutDir C:\Install\IDLifecycle-1.2.0 -ExePath .\bin\idlifecycled.exe

.NOTES
    Exit codes: 0 ok, 1 the finished folder would not be trusted by the installer, 2 invalid input, 3 Go missing or
    go build failed, 5 unexpected error (e.g. a copy failed).
#>

#Requires -Version 7.0

[CmdletBinding()]
param(
    [Parameter(Mandatory)]
    [string]$OutDir,

    [switch]$Build,

    [string]$Version = 'dev',

    [string]$ExePath = '',

    [string]$RepoRoot = '',

    [string]$LogDir = '',

    [switch]$Zip
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

Import-Module (Join-Path $PSScriptRoot 'IDLifecycle.Common.psm1') -Force

if ([string]::IsNullOrWhiteSpace($LogDir)){
    $LogDir = Join-Path -Path $env:LOCALAPPDATA -ChildPath 'IDLifecycle-Setup\Logs'
}

$exitCode = 5

try{
    Initialize-IDLProtectedDir -Path $LogDir
    Start-IDLRun -ScriptName 'New-IDLifecycleRelease' -ScriptRoot $PSScriptRoot -LogDir $LogDir -IsAction -Parameters @{
        OutDir   = $OutDir
        Build    = [bool]$Build
        Version  = $Version
        ExePath  = $ExePath
        RepoRoot = $RepoRoot
        Zip      = [bool]$Zip
    }

    # ---- Phase: validation ----
    $phase = Start-IDLPhase -Name 'Validate input'
    if ([string]::IsNullOrWhiteSpace($RepoRoot)){
        $RepoRoot = Split-Path -Parent $PSScriptRoot
    }

    $RepoRoot = [System.IO.Path]::GetFullPath($RepoRoot).TrimEnd('\')
    $scriptsSource = Join-Path -Path $RepoRoot -ChildPath 'scripts'
    if (-not (Test-Path -LiteralPath $scriptsSource -PathType Container)){
        throw (New-IDLException -Kind Input -Message "RepoRoot '$RepoRoot' has no scripts folder.")
    }

    if ($Build -eq [bool]$ExePath){
        throw (New-IDLException -Kind Input -Message 'Pass exactly one of -Build or -ExePath.')
    }

    if (
        ($ExePath) -and
        (-not (Test-Path -LiteralPath $ExePath -PathType Leaf))
    ){
        throw (New-IDLException -Kind Input -Message "ExePath not found: $ExePath")
    }

    if ($Version -notmatch '^[A-Za-z0-9][A-Za-z0-9.\-+]{0,63}$'){
        throw (New-IDLException -Kind Input -Message "Version '$Version' is not valid.")
    }

    $reason = Get-IDLUnsafePathReason -Path $OutDir
    if ($reason){
        throw (New-IDLException -Kind Input -Message "OutDir '$OutDir' refused: $reason.")
    }

    $OutDir = [System.IO.Path]::GetFullPath($OutDir).TrimEnd('\')
    if (Test-IDLPathWithin -Child $OutDir -Parent $RepoRoot){
        throw (New-IDLException -Kind Input -Message "OutDir must be outside the repository ($RepoRoot).")
    }

    if (
        (Test-Path -LiteralPath $OutDir) -and
        (@(Get-ChildItem -LiteralPath $OutDir -Force).Count -gt 0)
    ){
        throw (New-IDLException -Kind Input -Message "OutDir '$OutDir' already exists and is not empty; pick a new folder per release.")
    }

    if (
        ($Build) -and
        (-not (Get-Command -Name 'go' -CommandType Application -ErrorAction SilentlyContinue))
    ){
        throw (New-IDLException -Kind Dependency -Message 'go was not found on PATH (needed for -Build); or build it yourself and pass -ExePath.')
    }

    Stop-IDLPhase -Phase $phase

    # ---- Phase: release folder ----
    $phase = Start-IDLPhase -Name 'Release folder'
    Initialize-IDLProtectedDir -Path $OutDir -Lock -IncludeCurrentUser
    Write-IDLLog "Created $OutDir (inheritance off; SYSTEM, Administrators and you)"
    Add-IDLReportRow -Action 'CreateReleaseFolder' -Result 'Applied' -Identity $OutDir

    $exe = Join-Path -Path $OutDir -ChildPath 'idlifecycled.exe'
    if ($Build){
        Push-Location -LiteralPath $RepoRoot
        try{
            $output = & go build -ldflags "-X idlifecycle/internal/app.Version=$Version" -o $exe ./cmd/idlifecycled 2>&1
            if ($LASTEXITCODE -ne 0){
                throw (New-IDLException -Kind Dependency -Message "go build failed (exit code $LASTEXITCODE): $($output -join ' ')")
            }
        }
        finally{
            Pop-Location
        }

        Write-IDLLog "Built idlifecycled.exe $Version"
    }
    else{
        Copy-Item -LiteralPath $ExePath -Destination $exe
        Write-IDLLog "Copied $ExePath"
    }

    $count = Copy-IDLReleaseFiles -SourceRoot $RepoRoot -OutDir $OutDir
    Write-IDLLog "Wrote SHA256SUMS ($count file(s))"
    Add-IDLReportRow -Action 'WriteManifest' -Result 'Applied' -Identity (Join-Path -Path $OutDir -ChildPath 'SHA256SUMS') -Details "$count file(s)"
    Stop-IDLPhase -Phase $phase

    # ---- Phase: verify like the installer will ----
    $phase = Start-IDLPhase -Name 'Verify'
    $reason = Get-IDLUntrustedTreeReason -Path $OutDir -ParentChain -RefuseReparsePoints
    if ($reason){
        Write-IDLLog "FAILED: the release folder would be refused by Install-IDLifecycle.ps1: $reason. Put it below a folder that only administrators can change (e.g. a new C:\Install created by an administrator)." 'ERROR'
        Add-IDLReportRow -Action 'VerifyTrust' -Result 'Failed' -Identity $OutDir -ErrorText $reason
        $exitCode = 1
    }
    else{
        Write-IDLLog "Release ready: double-click $(Join-Path -Path $OutDir -ChildPath 'Setup.cmd') (setup GUI), or run $(Join-Path -Path $OutDir -ChildPath 'scripts\Install-IDLifecycle.ps1') from an elevated session."
        Add-IDLReportRow -Action 'VerifyTrust' -Result 'Verified' -Identity $OutDir
        $exitCode = 0
        if ($Zip){
            # The folder inside the zip keeps the release name, so it unpacks to one folder.
            $zipPath = "$OutDir.zip"
            Compress-Archive -LiteralPath $OutDir -DestinationPath $zipPath -CompressionLevel Optimal
            Write-IDLLog "Wrote ${zipPath}: copy it to a server and pick it as 'Release zip or folder' in the setup window, or pass it as -SourceDir."
            Add-IDLReportRow -Action 'WriteZip' -Result 'Applied' -Identity $zipPath
        }
    }

    Stop-IDLPhase -Phase $phase
}
catch{
    $exitCode = Get-IDLExitCode -Exception $_.Exception
    Write-IDLLog "FATAL: $($_.Exception.Message)" 'ERROR'
    Write-IDLLog "Stack: $($_.ScriptStackTrace)" 'DEBUG'
}
finally{
    Stop-IDLRun -ExitCode $exitCode
}

exit $exitCode
