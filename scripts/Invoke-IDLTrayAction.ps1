<#
.SYNOPSIS
    Elevated helper of the IDLifecycle tray: start, stop or restart the service, or change the web console settings.

.DESCRIPTION
    Start-IDLifecycleTray.ps1 runs as a normal user and starts this script with "Run as administrator" (one UAC
    prompt per action). Everything is checked again here, as an administrator:
      * it runs elevated (else exit 4);
      * -ServiceName is the IDLifecycle service of THIS installation: its command line must be
        "<the folder above this script>\idlifecycled.exe" service -config "<config.json>" (else exit 2);
      * -ResultFile (optional) is IDLTray_<32 hex>.json directly in this user's temp folder;
      * SetConfig: the new web console settings pass the same checks as in the setup window (host:port, HTTPS for an
        address other than this server, both certificate files exist, log level) - nothing is written otherwise.
    SetConfig runs `idlifecycled config set` (only the given fields change; config.json keeps its ACL), rewrites
    <InstallDir>\tray.json from `idlifecycled config get`, and with -Restart restarts a running service so the change
    takes effect. Start / Stop / Restart wait up to 60 seconds for the service to get there.
    The result is written to -ResultFile as {"ok", "message", "state"}; the log goes to the protected setup log folder
    (%ProgramData%\IDLifecycle-Setup\Logs). No secrets are involved.

.PARAMETER Action
    Start, Stop, Restart or SetConfig.

.PARAMETER ServiceName
    The IDLifecycle service (tray.json service_name).

.PARAMETER ConfigPath
    SetConfig: config.json; must be the one the service uses (default: taken from the service's command line).

.PARAMETER Listen
    SetConfig: the web console address host:port.

.PARAMETER TlsCertFile
    SetConfig: HTTPS certificate file (PEM); an empty string switches HTTPS off (together with -TlsKeyFile '').

.PARAMETER TlsKeyFile
    SetConfig: the certificate's key file (PEM).

.PARAMETER LogLevel
    SetConfig: debug, info, warn or error.

.PARAMETER Restart
    SetConfig: restart the service afterwards when it is running (the change takes effect then).

.PARAMETER ResultFile
    Where the tray reads the result: <temp>\IDLTray_<32 hex digits>.json.

.PARAMETER NonInteractive
    Accepted for symmetry with the other setup scripts; this script never prompts.

.EXAMPLE
    .\Invoke-IDLTrayAction.ps1 -Action Restart -ServiceName IDLifecycle

.EXAMPLE
    .\Invoke-IDLTrayAction.ps1 -Action SetConfig -ServiceName IDLifecycle -Listen 0.0.0.0:8443 -TlsCertFile C:\certs\idl.pem -TlsKeyFile C:\certs\idl.key -Restart

.NOTES
    Exit codes: 0 done, 1 the action failed (service did not reach the state, config set failed), 2 invalid input or
    not this installation's service, 4 not elevated, 5 unexpected error.
#>
#Requires -Version 7.0
[CmdletBinding()]
param(
    [Parameter(Mandatory)][ValidateSet('Start', 'Stop', 'Restart', 'SetConfig')][string]$Action,

    [Parameter(Mandatory)][string]$ServiceName,

    [string]$ConfigPath = '',

    [AllowEmptyString()][string]$Listen,

    [AllowEmptyString()][string]$TlsCertFile,

    [AllowEmptyString()][string]$TlsKeyFile,

    [AllowEmptyString()][string]$LogLevel,

    [switch]$Restart,

    [string]$ResultFile = '',

    [switch]$NonInteractive,

    # Test seam: honoured only when IDL_SETUP_TEST=1 and the file lies in <repo>\tests\powershell (Get-IDLTestHookRefusal).
    [string]$TestHook = ''
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
Import-Module (Join-Path -Path $PSScriptRoot -ChildPath 'IDLifecycle.Common.psm1') -Force

$script:SetupLogRoot = Join-Path -Path $env:ProgramData -ChildPath 'IDLifecycle-Setup\Logs'
$script:SetupLogLock = $true
$script:SetupLogOverride = $false
$script:InstallDir = Split-Path -Parent $PSScriptRoot

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

function Stop-IDLSetupService {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Name
    )

    Stop-Service -Name $Name -Force
}

function Start-IDLSetupService {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Name
    )

    Start-Service -Name $Name
}

function Get-IDLSetupDaemonCommand {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Exe
    )

    return @{ FileName = $Exe; Arguments = @() }
}

#endregion

function Invoke-IDLTrayDaemon {
    # Runs idlifecycled with the arguments; returns { ExitCode; StdOut; StdErr }.
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Exe,
        [Parameter(Mandatory)][string[]]$Arguments
    )

    $command = Get-IDLSetupDaemonCommand -Exe $Exe
    $psi = [System.Diagnostics.ProcessStartInfo]::new([string]$command.FileName)
    foreach ($a in (@($command.Arguments) + @($Arguments))){
        $psi.ArgumentList.Add([string]$a)
    }

    $psi.UseShellExecute = $false
    $psi.CreateNoWindow = $true
    $psi.RedirectStandardInput = $true
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError = $true
    $psi.StandardOutputEncoding = [System.Text.UTF8Encoding]::new($false)
    $psi.StandardErrorEncoding = [System.Text.UTF8Encoding]::new($false)
    $p = [System.Diagnostics.Process]::Start($psi)
    $outTask = $p.StandardOutput.ReadToEndAsync()
    $errTask = $p.StandardError.ReadToEndAsync()
    $p.StandardInput.Close()
    if (-not $p.WaitForExit(60000)){
        $p.Kill($true)
        throw "idlifecycled $($Arguments[0..1] -join ' ') timed out"
    }

    $p.WaitForExit()
    return [pscustomobject]@{ ExitCode = $p.ExitCode; StdOut = [string]$outTask.Result; StdErr = [string]$errTask.Result }
}

function Get-IDLTrayServiceTarget {
    <#
      The service must be THIS installation's IDLifecycle service: "<InstallDir>\idlifecycled.exe" service -config
      "<config>". Returns @{ Service; Exe; ConfigPath } or throws an Input exception.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Name
    )

    if ($Name -notmatch '^[A-Za-z0-9_.\-]{1,80}$'){
        throw (New-IDLException -Kind Input -Message "'$Name' is not a service name.")
    }

    $svc = Get-IDLSetupService -Name $Name
    if ($null -eq $svc){
        throw (New-IDLException -Kind Input -Message "Service '$Name' does not exist.")
    }

    $m = [regex]::Match([string]$svc.PathName, '^"(?<exe>[^"]+)" service -config "(?<cfg>[^"]+)"$')
    $expected = Join-Path -Path $script:InstallDir -ChildPath 'idlifecycled.exe'
    if (
        (-not $m.Success) -or
        (-not [System.IO.Path]::GetFullPath($m.Groups['exe'].Value).Equals([System.IO.Path]::GetFullPath($expected), [System.StringComparison]::OrdinalIgnoreCase))
    ){
        throw (New-IDLException -Kind Input -Message "Service '$Name' is not the IDLifecycle service of this installation ($expected); refusing.")
    }

    return @{ Service = $svc; Exe = $expected; ConfigPath = $m.Groups['cfg'].Value }
}

function Test-IDLTrayResultPath {
    # $null when Path is a result file this script may write: IDLTray_<32 hex>.json directly in the user's temp folder.
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Path
    )

    if (-not [System.IO.Path]::IsPathFullyQualified($Path)){
        return 'it is not a full path'
    }

    $full = [System.IO.Path]::GetFullPath($Path)
    $temp = [System.IO.Path]::GetFullPath([System.IO.Path]::GetTempPath()).TrimEnd('\')
    if (
        (-not ([System.IO.Path]::GetDirectoryName($full).TrimEnd('\')).Equals($temp, [System.StringComparison]::OrdinalIgnoreCase)) -or
        ([System.IO.Path]::GetFileName($full) -notmatch '^IDLTray_[0-9a-f]{32}\.json$')
    ){
        return "it must be $temp\IDLTray_<32 hex digits>.json"
    }

    if (
        (Test-Path -LiteralPath $full) -and
        ((([System.IO.File]::GetAttributes($full)) -band [System.IO.FileAttributes]::ReparsePoint) -ne 0)
    ){
        return 'it is a link'
    }

    return $null
}

function Wait-IDLTrayServiceState {
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
            ($null -ne $svc) -and
            ($svc.State -eq $State)
        ){
            return
        }

        if ([DateTime]::UtcNow -gt $deadline){
            throw "Service '$Name' did not reach the state $State within $TimeoutSec seconds (it is $((($null -ne $svc) ? ($svc.State) : ('gone'))))."
        }

        Start-Sleep -Milliseconds 500
    }
}

function Write-IDLTrayResult {
    [CmdletBinding()]
    param(
        [AllowEmptyString()][string]$Path,
        [bool]$Ok,
        [string]$Message,
        [string]$State = ''
    )

    if (-not $Path){
        return
    }

    try{
        [System.IO.File]::WriteAllText($Path, (ConvertTo-Json -InputObject ([ordered]@{ ok = $Ok; message = $Message; state = $State }) -Compress), [System.Text.UTF8Encoding]::new($false))
    }
    catch{
        [Console]::Error.WriteLine("Invoke-IDLTrayAction: could not write the result file ${Path}: $($_.Exception.Message)")
    }
}

if ($TestHook){
    $hookRefusal = Get-IDLTestHookRefusal -HookPath $TestHook -ScriptRoot $PSScriptRoot
    if ($hookRefusal){
        [Console]::Error.WriteLine("Invoke-IDLTrayAction: -TestHook refused ($hookRefusal); it is only for the Pester tests.")
        exit 2
    }

    . $TestHook
    $script:InstallDir = (($env:IDL_TRAY_TEST_INSTALLDIR) ? ($env:IDL_TRAY_TEST_INSTALLDIR) : ($script:InstallDir))
}

$exitCode = 5
$resultOk = $false
$resultMessage = 'The action did not finish.'
$resultState = ''
$resultPath = ''

try{
    if (
        ($ResultFile) -and
        (-not (Test-IDLTrayResultPath -Path $ResultFile))
    ){
        $resultPath = [System.IO.Path]::GetFullPath($ResultFile)
    }

    if (-not (Test-IDLSetupElevated)){
        $resultMessage = 'This action needs administrator rights.'
        [Console]::Error.WriteLine("Invoke-IDLTrayAction: $resultMessage")
        $exitCode = 4
        exit 4
    }

    try{
        Initialize-IDLProtectedDir -Path $script:SetupLogRoot -Lock:$script:SetupLogLock
    }
    catch{
        [Console]::Error.WriteLine("Invoke-IDLTrayAction: cannot use the log folder '$($script:SetupLogRoot)': $($_.Exception.Message)")
        exit 2
    }

    Start-IDLRun -ScriptName 'Invoke-IDLTrayAction' -ScriptRoot $PSScriptRoot -LogDir $script:SetupLogRoot -IsAction -Parameters @{
        Action      = $Action
        ServiceName = $ServiceName
        ConfigPath  = $ConfigPath
        Listen      = (($PSBoundParameters.ContainsKey('Listen')) ? ($Listen) : ('(unchanged)'))
        TlsCertFile = (($PSBoundParameters.ContainsKey('TlsCertFile')) ? ($TlsCertFile) : ('(unchanged)'))
        TlsKeyFile  = (($PSBoundParameters.ContainsKey('TlsKeyFile')) ? ($TlsKeyFile) : ('(unchanged)'))
        LogLevel    = (($PSBoundParameters.ContainsKey('LogLevel')) ? ($LogLevel) : ('(unchanged)'))
        Restart     = [bool]$Restart
        ResultFile  = $ResultFile
    }

    if ($TestHook){
        Write-IDLLog "TEST HOOK loaded: $TestHook (system-touching functions are replaced)" 'WARN'
    }

    if ($ResultFile){
        $why = Test-IDLTrayResultPath -Path $ResultFile
        if ($why){
            throw (New-IDLException -Kind Input -Message "ResultFile '$ResultFile' refused: $why.")
        }
    }

    $target = Get-IDLTrayServiceTarget -Name $ServiceName
    $trayPath = Join-Path -Path $script:InstallDir -ChildPath 'tray.json'
    Write-IDLLog "Service '$ServiceName' ($($target.Service.State)): $($target.Exe), config $($target.ConfigPath)"

    switch ($Action){
        'Stop' {
            Add-IDLCounter -Name 'Total'
            if ($target.Service.State -eq 'Stopped'){
                Write-IDLLog "SKIP: StopService $ServiceName (already stopped)"
                Add-IDLCounter -Name 'Skipped'
                Add-IDLReportRow -Action 'StopService' -Result 'Skipped' -Identity $ServiceName -Details 'already stopped'
            }
            else{
                Stop-IDLSetupService -Name $ServiceName
                Wait-IDLTrayServiceState -Name $ServiceName -State 'Stopped'
                Write-IDLLog "DONE: StopService $ServiceName"
                Add-IDLCounter -Name 'Changed'
                Add-IDLReportRow -Action 'StopService' -Result 'Applied' -Identity $ServiceName
            }

            $resultMessage = "The service '$ServiceName' is stopped."
        }

        'Start' {
            Add-IDLCounter -Name 'Total'
            if ($target.Service.State -eq 'Running'){
                Write-IDLLog "SKIP: StartService $ServiceName (already running)"
                Add-IDLCounter -Name 'Skipped'
                Add-IDLReportRow -Action 'StartService' -Result 'Skipped' -Identity $ServiceName -Details 'already running'
            }
            else{
                Start-IDLSetupService -Name $ServiceName
                Wait-IDLTrayServiceState -Name $ServiceName -State 'Running'
                Write-IDLLog "DONE: StartService $ServiceName"
                Add-IDLCounter -Name 'Changed'
                Add-IDLReportRow -Action 'StartService' -Result 'Applied' -Identity $ServiceName
            }

            $resultMessage = "The service '$ServiceName' is running."
        }

        'Restart' {
            Add-IDLCounter -Name 'Total'
            if ($target.Service.State -ne 'Stopped'){
                Stop-IDLSetupService -Name $ServiceName
                Wait-IDLTrayServiceState -Name $ServiceName -State 'Stopped'
            }

            Start-IDLSetupService -Name $ServiceName
            Wait-IDLTrayServiceState -Name $ServiceName -State 'Running'
            Write-IDLLog "DONE: RestartService $ServiceName"
            Add-IDLCounter -Name 'Changed'
            Add-IDLReportRow -Action 'RestartService' -Result 'Applied' -Identity $ServiceName
            $resultMessage = "The service '$ServiceName' was restarted."
        }

        'SetConfig' {
            if (
                ($ConfigPath) -and
                (-not [System.IO.Path]::GetFullPath($ConfigPath).Equals([System.IO.Path]::GetFullPath($target.ConfigPath), [System.StringComparison]::OrdinalIgnoreCase))
            ){
                throw (New-IDLException -Kind Input -Message "ConfigPath '$ConfigPath' is not the config.json the service uses ($($target.ConfigPath)).")
            }

            $current = Invoke-IDLTrayDaemon -Exe $target.Exe -Arguments @('config', 'get', '-config', $target.ConfigPath)
            if ($current.ExitCode -ne 0){
                throw "idlifecycled config get failed (exit code $($current.ExitCode)): $($current.StdErr.Trim())"
            }

            $cur = $current.StdOut | ConvertFrom-Json -AsHashtable
            $effective = @{
                Listen      = (($PSBoundParameters.ContainsKey('Listen')) ? ($Listen) : ([string]$cur['listen']))
                TlsCertFile = (($PSBoundParameters.ContainsKey('TlsCertFile')) ? ($TlsCertFile) : ([string]$cur['tls_cert_file']))
                TlsKeyFile  = (($PSBoundParameters.ContainsKey('TlsKeyFile')) ? ($TlsKeyFile) : ([string]$cur['tls_key_file']))
                LogLevel    = (($PSBoundParameters.ContainsKey('LogLevel')) ? ($LogLevel) : (''))
            }
            $problems = @(Get-IDLConsoleSettingsProblems @effective)
            if (
                ($PSBoundParameters.ContainsKey('Listen')) -and
                ([string]::IsNullOrWhiteSpace($Listen))
            ){
                $problems += 'Enter the web console address (host:port).'
            }

            if ($problems.Count -gt 0){
                throw (New-IDLException -Kind Input -Message ($problems -join ' '))
            }

            $setArgs = [System.Collections.Generic.List[string]]::new()
            foreach ($pair in @(@('Listen', '-listen'), @('TlsCertFile', '-tls-cert'), @('TlsKeyFile', '-tls-key'), @('LogLevel', '-log-level'))){
                if ($PSBoundParameters.ContainsKey($pair[0])){
                    $setArgs.Add($pair[1])
                    $setArgs.Add([string]$PSBoundParameters[$pair[0]])
                }
            }

            Add-IDLCounter -Name 'Total'
            if ($setArgs.Count -eq 0){
                Write-IDLLog 'SKIP: SetConfig (nothing to change)'
                Add-IDLCounter -Name 'Skipped'
                Add-IDLReportRow -Action 'SetConfig' -Result 'Skipped' -Identity $target.ConfigPath -Details 'nothing to change'
                $after = $cur
            }
            else{
                $set = Invoke-IDLTrayDaemon -Exe $target.Exe -Arguments (@('config', 'set', '-config', $target.ConfigPath) + $setArgs.ToArray())
                if ($set.ExitCode -ne 0){
                    $kind = (($set.ExitCode -eq 2) ? ('Input') : ('Fatal'))
                    throw (New-IDLException -Kind $kind -Message "idlifecycled config set refused the change (exit code $($set.ExitCode)): $($set.StdErr.Trim())")
                }

                $after = $set.StdOut | ConvertFrom-Json -AsHashtable
                Write-IDLLog "DONE: SetConfig $($target.ConfigPath) ($($setArgs -join ' '))"
                Add-IDLCounter -Name 'Changed'
                Add-IDLReportRow -Action 'SetConfig' -Result 'Applied' -Identity $target.ConfigPath -Details ($setArgs -join ' ')
            }

            # tray.json follows config.json (version and execution policy are kept from the current file).
            $old = @{}
            try{
                $old = Read-IDLTrayInfo -Path $trayPath
            }
            catch{
                Write-IDLLog "tray.json is rewritten: $($_.Exception.Message)" 'WARN'
            }

            $info = [ordered]@{
                service_name     = $ServiceName
                console_url      = [string]$after['console_url']
                listen           = [string]$after['listen']
                tls              = (-not [string]::IsNullOrWhiteSpace([string]$after['tls_cert_file']))
                log_level        = [string]$after['log_level']
                data_dir         = [string]$after['data_dir']
                install_dir      = $script:InstallDir
                config_path      = $target.ConfigPath
                version          = [string]$old['version']
                execution_policy = [string]$old['execution_policy']
            }
            Write-IDLTrayInfo -Path $trayPath -Info $info
            Write-IDLLog "DONE: WriteTrayInfo $trayPath ($($info['console_url']))"

            $resultMessage = 'The web console settings were saved.'
            $svc = Get-IDLSetupService -Name $ServiceName
            if (
                ($Restart) -and
                ($setArgs.Count -gt 0) -and
                ($svc.State -eq 'Running')
            ){
                Stop-IDLSetupService -Name $ServiceName
                Wait-IDLTrayServiceState -Name $ServiceName -State 'Stopped'
                Start-IDLSetupService -Name $ServiceName
                Wait-IDLTrayServiceState -Name $ServiceName -State 'Running'
                Write-IDLLog "DONE: RestartService $ServiceName (to apply the settings)"
                Add-IDLReportRow -Action 'RestartService' -Result 'Applied' -Identity $ServiceName -Details 'to apply the settings'
                $resultMessage = 'The web console settings were saved and the service was restarted.'
            }
            elseif ($setArgs.Count -gt 0){
                $resultMessage += ' They take effect when the service is restarted.'
            }
        }
    }

    $resultState = [string](Get-IDLSetupService -Name $ServiceName).State
    $resultOk = $true
    $exitCode = 0
    Write-IDLLog "Result: $resultMessage (service state: $resultState)"
}
catch{
    $exitCode = Get-IDLExitCode -Exception $_.Exception
    if ($exitCode -eq 5 -and $_.Exception.Message -match 'did not reach the state|config get failed|config set refused'){
        $exitCode = 1
    }

    $resultMessage = $_.Exception.Message
    try{
        Write-IDLLog "FAILED: $Action $ServiceName - $resultMessage" 'ERROR'
    }
    catch{
        [Console]::Error.WriteLine("Invoke-IDLTrayAction: $resultMessage")
    }

    try{
        $svc = Get-IDLSetupService -Name $ServiceName
        $resultState = (($null -ne $svc) ? ([string]$svc.State) : (''))
    }
    catch{
        $resultState = ''
    }
}
finally{
    Write-IDLTrayResult -Path $resultPath -Ok $resultOk -Message $resultMessage -State $resultState
    try{
        Stop-IDLRun -ExitCode $exitCode
    }
    catch{
        # Start-IDLRun may not have run (refused before the log was set up).
        $null = $_
    }
}

exit $exitCode
