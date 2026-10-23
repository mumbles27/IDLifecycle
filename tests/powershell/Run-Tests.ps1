<#
.SYNOPSIS
    Runs every Pester test for the IDLifecycle PowerShell scripts against the fake ActiveDirectory module.

.DESCRIPTION
    1. Parses every script/module under scripts\ and tests\ and fails on syntax errors.
    2. Proves (in a child pwsh whose PSModulePath contains ONLY tests\powershell\StubAD) that the
       ActiveDirectory module resolves to the stub (version 99.0.0). If not, it aborts loudly and runs nothing.
    3. Runs all *.Tests.ps1 files in this folder with Pester 3.x (Pester 3.4.0 is what is installed).
    4. Removes temp state files and any log/report files the scripts created under scripts\Logs.

    Tests never touch a real domain: every script under test runs in a child pwsh with a stub-only module path.
    This runner itself never imports the ActiveDirectory module.

.EXAMPLE
    pwsh -NoProfile -File .\tests\powershell\Run-Tests.ps1

.EXAMPLE
    pwsh -NoProfile -File .\tests\powershell\Run-Tests.ps1; if ($LASTEXITCODE -ne 0) { 'tests failed' }

.NOTES
    Requires PowerShell 7+ and Pester 3.x (installed side-by-side with Windows PowerShell's copy).
    Exit codes: 0 all tests passed, 1 one or more tests failed, 2 syntax error found, 3 Pester missing,
    5 unexpected error / stub safety check failed.
    Run log: tests\powershell\Logs\Run-Tests_<yyyyMMdd_HHmmss>.log
#>

#Requires -Version 7.0

[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$exitCode = 5
$repoRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$scriptsLogs = Join-Path $repoRoot 'scripts\Logs'
$logBaseline = @()
if (Test-Path -LiteralPath $scriptsLogs){
    $logBaseline = @(Get-ChildItem -LiteralPath $scriptsLogs -File | ForEach-Object { $_.FullName })
}

Import-Module (Join-Path $repoRoot 'scripts\IDLifecycle.Common.psm1') -Force

try{
    Start-IDLRun -ScriptName 'Run-Tests' -ScriptRoot $PSScriptRoot

    # ---- Phase: syntax check ----
    $phase = Start-IDLPhase -Name 'Syntax check'
    $syntaxErrors = 0
    $files = @(Get-ChildItem -LiteralPath $repoRoot -Recurse -File -Include '*.ps1', '*.psm1', '*.psd1' |
            Where-Object { $_.FullName -notmatch '\\(Logs|\.git|\.claude)\\' })
    foreach ($file in $files){
        $tokens = $null
        $errors = $null
        $null = [System.Management.Automation.Language.Parser]::ParseFile($file.FullName, [ref]$tokens, [ref]$errors)
        foreach ($e in $errors){
            $syntaxErrors++
            Write-IDLLog "SYNTAX ERROR $($file.FullName):$($e.Extent.StartLineNumber): $($e.Message)" 'ERROR'
        }
    }

    Write-IDLLog "Parsed $($files.Count) PowerShell file(s); $syntaxErrors syntax error(s)"
    Stop-IDLPhase -Phase $phase
    if ($syntaxErrors -gt 0){
        $exitCode = 2
    }
    else{
        # ---- Phase: Pester + stub safety check ----
        $phase = Start-IDLPhase -Name 'Load Pester'
        $pesterLoaded = $false
        try{
            Import-Module Pester -RequiredVersion 3.4.0 -Force -ErrorAction Stop
            $pesterLoaded = $true
        }
        catch{
            $exitCode = 3
            Write-IDLLog "Pester 3.4.0 could not be loaded: $($_.Exception.Message)" 'ERROR'
        }

        if ($pesterLoaded){
            Write-IDLLog "Pester $((Get-Module Pester).Version) loaded"
            Stop-IDLPhase -Phase $phase

            . (Join-Path $PSScriptRoot 'TestHelpers.ps1')
            Assert-StubOnlyEnvironment
            Write-IDLLog 'Safety check passed: child pwsh resolves ActiveDirectory to the stub (99.0.0)'

            # ---- Phase: tests ----
            $phase = Start-IDLPhase -Name 'Run Pester tests'
            $result = Invoke-Pester -Path $PSScriptRoot -PassThru
            Stop-IDLPhase -Phase $phase

            Write-IDLLog ('Pester result: total={0} passed={1} failed={2} skipped={3} pending={4}' -f $result.TotalCount, $result.PassedCount, $result.FailedCount, $result.SkippedCount, $result.PendingCount)
            $exitCode = (($result.FailedCount -gt 0) ? (1) : (0))
        }
    }
}
catch{
    $exitCode = 5
    Write-IDLLog "FATAL: $($_.Exception.Message)" 'ERROR'
    Write-IDLLog "Stack: $($_.ScriptStackTrace)" 'DEBUG'
}
finally{
    # Remove anything the tests left behind.
    if (Test-Path -LiteralPath $scriptsLogs){
        foreach ($f in @(Get-ChildItem -LiteralPath $scriptsLogs -File | ForEach-Object { $_.FullName })){
            if ($logBaseline -notcontains $f){
                Remove-Item -LiteralPath $f -Force -ErrorAction SilentlyContinue
            }
        }
    }

    $stateDir = Join-Path ([System.IO.Path]::GetTempPath()) 'idl_pester'
    if (Test-Path -LiteralPath $stateDir){
        Remove-Item -LiteralPath $stateDir -Recurse -Force -ErrorAction SilentlyContinue
    }

    Stop-IDLRun -ExitCode $exitCode
}

exit $exitCode
