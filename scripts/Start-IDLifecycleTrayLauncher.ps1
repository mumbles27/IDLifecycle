<#
.SYNOPSIS
    Starts the IDLifecycle tray icon with whichever PowerShell 7 the signed-in user has.

.DESCRIPTION
    The tray shortcuts use this launcher when PowerShell 7 is not installed machine-wide (%ProgramFiles%\PowerShell\7),
    for example when it comes from the Microsoft Store: the Store's pwsh.exe lives in a version-numbered folder that
    changes with every update and that a shortcut cannot start directly. Windows PowerShell 5.1 (always at the same path)
    runs this script; it finds pwsh.exe (machine-wide install, else the user's "pwsh" app alias on PATH) and starts
    Start-IDLifecycleTray.ps1 hidden, on an STA thread, with the given execution policy. It changes nothing else.
    Written for Windows PowerShell 5.1 and PowerShell 7.

.PARAMETER ExecutionPolicy
    Passed to the tray's pwsh.exe: AllSigned, RemoteSigned (default) or Unrestricted - the installer uses
    powershell.execution_policy from config.json. Bypass is refused.

.EXAMPLE
    powershell.exe -NoProfile -WindowStyle Hidden -ExecutionPolicy RemoteSigned -File "C:\Program Files\IDLifecycle\scripts\Start-IDLifecycleTrayLauncher.ps1"

.NOTES
    Exit codes: 0 tray started, 2 invalid execution policy, 3 PowerShell 7 not found.
#>
[CmdletBinding()]
param(
    [string]$ExecutionPolicy = 'RemoteSigned'
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

if ($ExecutionPolicy -notin @('AllSigned', 'RemoteSigned', 'Unrestricted')){
    exit 2
}

$pwsh = $null
$machine = Join-Path -Path $env:ProgramFiles -ChildPath 'PowerShell\7\pwsh.exe'
if (Test-Path -LiteralPath $machine -PathType Leaf){
    $pwsh = $machine
}
else{
    $found = @(Get-Command -Name 'pwsh.exe' -CommandType Application -ErrorAction SilentlyContinue)
    if ($found.Count -gt 0){
        $pwsh = $found[0].Source
    }
}

if (-not $pwsh){
    exit 3
}

$tray = Join-Path -Path $PSScriptRoot -ChildPath 'Start-IDLifecycleTray.ps1'
Start-Process -FilePath $pwsh -WindowStyle Hidden -ArgumentList @(
    '-NoProfile', '-STA', '-WindowStyle', 'Hidden', '-ExecutionPolicy', $ExecutionPolicy, '-File', ('"{0}"' -f $tray))
exit 0
