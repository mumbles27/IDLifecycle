<#
.SYNOPSIS
    Registers the IDLifecycle Go service (idlifecycled.exe) with the Windows Service Control Manager.

.DESCRIPTION
    One-shot, idempotent installer. It:
      0. unblocks (Unblock-File) every .ps1/.psm1/.psd1 in the scripts directory ("powershell.scripts_dir", else
         this script's folder) that carries a Mark-of-the-Web Zone.Identifier stream - under RemoteSigned such files
         refuse to load and every sync would fail - and fails the step if any is still blocked afterwards;
      1. creates the data directory (config "data_dir", holds the database and key material) and the scripts Logs
         directory (employee PII), then LOCKS BOTH DOWN: ACL inheritance is switched off WITHOUT copying inherited
         entries, every explicit entry is removed, and only SYSTEM and BUILTIN\Administrators (FullControl) and
         the service account (Modify) are granted, inherited by children. Users, Authenticated Users,
         CREATOR OWNER and Everyone get nothing. The step counts as already done only when the directory is in
         exactly that state (inheritance off, none of those four present), so inherited entries can never hide a
         loose ACL. Existing files inherit the new ACL unless they have inheritance disabled themselves;
      2. grants the service account read access to the config file;
      3. creates the service (or fixes binPath / log-on account if it drifted) running as the given account,
         start type Automatic (Delayed Start), with a description;
      4. sets failure recovery: restart after 5s, 30s and 60s, counter reset after 24h, also on non-crash failures.
    The service command line is:  "<BinaryPath>" service -config "<ConfigPath>"
    It does NOT start the service. Re-running it only changes what is out of line and reports the rest as skipped.

    The service account must be a gMSA (name ends with $, e.g. CORP\svc-idl$) or a built-in/virtual account
    (LocalSystem, NT AUTHORITY\NetworkService, NT SERVICE\...). Passwords are deliberately not supported, so no
    credential ever touches a command line.

.PARAMETER BinaryPath
    Full path to idlifecycled.exe. Must exist.

.PARAMETER ConfigPath
    Full path to the service config.json (see docs/ARCHITECTURE.md section 9). Must exist and contain "data_dir".
    The optional "powershell.scripts_dir" decides which Logs folder gets the service account ACL.

.PARAMETER ServiceAccount
    Log-on account for the service, e.g. 'CORP\svc-idl$' (gMSA). The account needs the "Log on as a service" right
    (normally granted by GPO) and must be allowed to retrieve the gMSA password on this host.

.PARAMETER ServiceName
    Name of the Windows service. Default: IDLifecycle.

.PARAMETER DryRun
    Look at the current state, log/report WOULD: statements for everything that would change, and change nothing.

.EXAMPLE
    .\Install-IDLifecycleService.ps1 -BinaryPath 'C:\Program Files\IDLifecycle\idlifecycled.exe' -ConfigPath 'C:\ProgramData\IDLifecycle\config.json' -ServiceAccount 'CORP\svc-idl$' -DryRun

.EXAMPLE
    .\Install-IDLifecycleService.ps1 -BinaryPath 'C:\Program Files\IDLifecycle\idlifecycled.exe' -ConfigPath 'C:\ProgramData\IDLifecycle\config.json' -ServiceAccount 'CORP\svc-idl$' -ServiceName IDLifecycle

.NOTES
    Requires PowerShell 7+, Windows, sc.exe, and an elevated session (not required for -DryRun).
    Exit codes: 0 ok, 1 some steps failed, 2 invalid input, 3 dependency missing (not Windows / no sc.exe),
    4 not elevated / access denied, 5 unexpected error.
    Logs and CSV report go to $PSScriptRoot\Logs (Install-IDLifecycleService_*.log / _Report_*.csv).
#>

#Requires -Version 7.0

[CmdletBinding()]
param(
    [Parameter(Mandatory)]
    [string]$BinaryPath,

    [Parameter(Mandatory)]
    [string]$ConfigPath,

    [Parameter(Mandatory)]
    [string]$ServiceAccount,

    [string]$ServiceName = 'IDLifecycle',

    [switch]$DryRun
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

Import-Module (Join-Path $PSScriptRoot 'IDLifecycle.Common.psm1') -Force

$script:FailedSteps = 0

function Invoke-InstallStep {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Action,
        [Parameter(Mandatory)][string]$Identity,
        [string]$Details = '',
        [Parameter(Mandatory)][scriptblock]$IsSatisfied,
        [Parameter(Mandatory)][scriptblock]$Apply
    )

    Add-IDLCounter -Name 'Total'

    try{
        if (& $IsSatisfied){
            Write-IDLLog "SKIP: $Action $Identity (already configured)"
            Add-IDLCounter -Name 'Skipped'
            Add-IDLSkipReason -Reason 'already configured'
            Add-IDLReportRow -Action $Action -Result 'Skipped' -Identity $Identity -Details 'already configured'
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

function Invoke-ScExe {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string[]]$Arguments
    )

    $output = & sc.exe @Arguments 2>&1
    if ($LASTEXITCODE -ne 0){
        throw "sc.exe $($Arguments -join ' ') failed with exit code ${LASTEXITCODE}: $($output -join ' ')"
    }

    return $output
}

function Test-AccessRulePresent {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Path,
        [Parameter(Mandatory)][string]$Account,
        [Parameter(Mandatory)][System.Security.AccessControl.FileSystemRights]$Rights
    )

    if (-not (Test-Path -LiteralPath $Path)){
        return $false
    }

    foreach ($rule in (Get-Acl -LiteralPath $Path).Access){
        if (
            ($rule.AccessControlType -eq 'Allow') -and
            ($rule.IdentityReference.Value -ieq $Account) -and
            (($rule.FileSystemRights -band $Rights) -eq $Rights)
        ){
            return $true
        }
    }

    return $false
}

function Grant-AccessRule {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Path,
        [Parameter(Mandatory)][string]$Account,
        [Parameter(Mandatory)][System.Security.AccessControl.FileSystemRights]$Rights,
        [switch]$IsDirectory
    )

    # DACL only (Get-Acl/Set-Acl also touch owner/SACL, which needs SeSecurityPrivilege).
    if ($IsDirectory){
        $info = [System.IO.DirectoryInfo]::new($Path)
        $inherit = [System.Security.AccessControl.InheritanceFlags]'ContainerInherit, ObjectInherit'
    }
    else{
        $info = [System.IO.FileInfo]::new($Path)
        $inherit = [System.Security.AccessControl.InheritanceFlags]::None
    }

    $acl = [System.IO.FileSystemAclExtensions]::GetAccessControl($info, [System.Security.AccessControl.AccessControlSections]::Access)

    $rule = [System.Security.AccessControl.FileSystemAccessRule]::new(
        $Account,
        $Rights,
        $inherit,
        [System.Security.AccessControl.PropagationFlags]::None,
        [System.Security.AccessControl.AccessControlType]::Allow
    )
    $acl.AddAccessRule($rule)
    [System.IO.FileSystemAclExtensions]::SetAccessControl($info, $acl)
}

$exitCode = 5

try{
    Start-IDLRun -ScriptName 'Install-IDLifecycleService' -ScriptRoot $PSScriptRoot -IsAction -DryRun:$DryRun -Parameters @{
        BinaryPath     = $BinaryPath
        ConfigPath     = $ConfigPath
        ServiceAccount = $ServiceAccount
        ServiceName    = $ServiceName
        DryRun         = [bool]$DryRun
    }

    # ---- Phase: validation (exit 2) and dependencies (exit 3) ----
    $phase = Start-IDLPhase -Name 'Validate input'

    if (
        ($PSVersionTable.PSEdition -ne 'Core') -or
        (-not $IsWindows)
    ){
        throw (New-IDLException -Kind Dependency -Message 'This installer only runs on Windows with PowerShell 7+.')
    }

    if (-not (Get-Command -Name 'sc.exe' -CommandType Application -ErrorAction SilentlyContinue)){
        throw (New-IDLException -Kind Dependency -Message 'sc.exe was not found on PATH.')
    }

    if ($ServiceName -notmatch '^[A-Za-z0-9_.\-]{1,80}$'){
        throw (New-IDLException -Kind Input -Message "ServiceName '$ServiceName' is not valid (letters, digits, '_', '.', '-' only).")
    }

    if (-not (Test-Path -LiteralPath $BinaryPath -PathType Leaf)){
        throw (New-IDLException -Kind Input -Message "BinaryPath not found: $BinaryPath")
    }

    if (-not (Test-Path -LiteralPath $ConfigPath -PathType Leaf)){
        throw (New-IDLException -Kind Input -Message "ConfigPath not found: $ConfigPath")
    }

    $builtInAccounts = @('LocalSystem', 'NT AUTHORITY\LocalService', 'NT AUTHORITY\NetworkService', 'NT AUTHORITY\SYSTEM')
    $isBuiltIn = (
        ($ServiceAccount -in $builtInAccounts) -or
        ($ServiceAccount -like 'NT SERVICE\*')
    )
    $isGmsa = ($ServiceAccount -match '^[^\\@\s]+\\[^\\\s]+\$$') -or ($ServiceAccount -match '^[^\\@\s]+\$@[^\\\s]+$')
    if (
        (-not $isBuiltIn) -and
        (-not $isGmsa)
    ){
        throw (New-IDLException -Kind Input -Message "ServiceAccount '$ServiceAccount' must be a gMSA (DOMAIN\name`$) or a built-in/virtual account; password accounts are not supported.")
    }

    try{
        $config = Get-Content -LiteralPath $ConfigPath -Raw -Encoding utf8 | ConvertFrom-Json -AsHashtable
    }
    catch{
        throw (New-IDLException -Kind Input -Message "ConfigPath is not valid JSON: $ConfigPath")
    }

    $dataDir = [string](Get-IDLField -Object $config -Name 'data_dir')
    if ([string]::IsNullOrWhiteSpace($dataDir)){
        throw (New-IDLException -Kind Input -Message 'config.json has no "data_dir".')
    }

    $scriptsDir = $PSScriptRoot
    $psConfig = Get-IDLField -Object $config -Name 'powershell'
    $configuredScriptsDir = [string](Get-IDLField -Object $psConfig -Name 'scripts_dir')
    if (
        (-not [string]::IsNullOrWhiteSpace($configuredScriptsDir)) -and
        (Test-Path -LiteralPath $configuredScriptsDir -PathType Container)
    ){
        $scriptsDir = $configuredScriptsDir
    }

    $logsDir = Join-Path -Path $scriptsDir -ChildPath 'Logs'
    $aclAccount = (($ServiceAccount -eq 'LocalSystem') ? ('NT AUTHORITY\SYSTEM') : ($ServiceAccount))

    if (
        (-not $DryRun) -and
        (-not ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator))
    ){
        throw (New-IDLException -Kind Connection -Message 'Run this script from an elevated (Administrator) session.')
    }

    Write-IDLLog "Service '$ServiceName' as '$ServiceAccount'; data dir '$dataDir'; logs dir '$logsDir'"
    Stop-IDLPhase -Phase $phase

    # ---- Phase: Mark-of-the-Web ----
    # Copied/downloaded scripts carry a Zone.Identifier stream; under RemoteSigned they refuse to load and every
    # sync fails. Unblock them, then verify that none is left.
    $phase = Start-IDLPhase -Name 'Unblock scripts'
    $blockedFiles = @(Get-IDLBlockedFiles -Path $scriptsDir)
    Write-IDLLog "Scripts dir '$scriptsDir': $($blockedFiles.Count) blocked file(s) (Zone.Identifier)"
    foreach ($blockedFile in $blockedFiles){
        Write-IDLLog (($DryRun) ? ("WOULD: unblock $blockedFile") : ("  blocked: $blockedFile"))
    }

    Invoke-InstallStep -Action 'UnblockScripts' -Identity $scriptsDir `
        -Details "Unblock-File on $($blockedFiles.Count) .ps1/.psm1/.psd1 file(s)" `
        -IsSatisfied { $blockedFiles.Count -eq 0 } `
        -Apply {
        $stillBlocked = @(Unblock-IDLScriptFiles -Path $scriptsDir)
        if ($stillBlocked.Count -gt 0){
            throw ("{0} script file(s) are still blocked (Zone.Identifier) and will not load under RemoteSigned: {1}. Unblock them manually (Unblock-File) and re-run." -f $stillBlocked.Count, ($stillBlocked -join ', '))
        }
    }

    Stop-IDLPhase -Phase $phase

    # ---- Phase: directories and ACLs ----
    $phase = Start-IDLPhase -Name 'Directories and ACLs'

    Invoke-InstallStep -Action 'CreateDirectory' -Identity $dataDir `
        -IsSatisfied { Test-Path -LiteralPath $dataDir -PathType Container } `
        -Apply { $null = New-Item -Path $dataDir -ItemType Directory -Force }

    Invoke-InstallStep -Action 'CreateDirectory' -Identity $logsDir `
        -IsSatisfied { Test-Path -LiteralPath $logsDir -PathType Container } `
        -Apply { $null = New-Item -Path $logsDir -ItemType Directory -Force }

    # Lockdown is its own step per directory: inheritance OFF (inherited ACEs are dropped, not copied), no
    # Users / Authenticated Users / CREATOR OWNER / Everyone ACE, only SYSTEM + Administrators (FullControl)
    # and the service account (Modify). "Satisfied" means exactly that state, so inherited ACEs can never
    # make the step look done.
    foreach ($lockDir in @($dataDir, $logsDir)){
        Invoke-InstallStep -Action 'LockDownDirectory' -Identity $lockDir `
            -Details "inheritance off; SYSTEM+Administrators FullControl; $ServiceAccount Modify; no other ACEs" `
            -IsSatisfied { Test-IDLDirectoryLockedDown -Path $lockDir -ServiceAccount $aclAccount } `
            -Apply { Set-IDLDirectoryLockdown -Path $lockDir -ServiceAccount $aclAccount }
    }

    Invoke-InstallStep -Action 'GrantConfigRead' -Identity "$ConfigPath <- $ServiceAccount" `
        -Details 'Read' `
        -IsSatisfied { Test-AccessRulePresent -Path $ConfigPath -Account $ServiceAccount -Rights Read } `
        -Apply { Grant-AccessRule -Path $ConfigPath -Account $ServiceAccount -Rights Read }

    Stop-IDLPhase -Phase $phase

    # ---- Phase: service registration ----
    $phase = Start-IDLPhase -Name 'Service registration'
    $binPath = '"{0}" service -config "{1}"' -f $BinaryPath, $ConfigPath
    $existing = Get-CimInstance -ClassName Win32_Service -Filter ("Name='{0}'" -f $ServiceName.Replace("'", "''")) -ErrorAction SilentlyContinue
    Write-IDLLog (($existing) ? ("Service exists: path=[$($existing.PathName)] account=[$($existing.StartName)] mode=$($existing.StartMode)") : ('Service does not exist yet'))

    $existingDelayed = $false
    $existingDescription = ''
    if ($existing){
        $delayedProp = $existing.PSObject.Properties['DelayedAutoStart']
        if ($delayedProp){
            $existingDelayed = [bool]$delayedProp.Value
        }

        $descriptionProp = $existing.PSObject.Properties['Description']
        if ($descriptionProp){
            $existingDescription = [string]$descriptionProp.Value
        }
    }

    $serviceDescription = 'Syncs HR source records to Active Directory with an approval workflow.'
    $logOnAccount = (($ServiceAccount -eq 'LocalSystem') ? ('LocalSystem') : ($ServiceAccount))

    if (-not $existing){
        Invoke-InstallStep -Action 'CreateService' -Identity $ServiceName -Details $binPath `
            -IsSatisfied { $false } `
            -Apply {
            $scArgs = @('create', $ServiceName, 'binPath=', $binPath, 'start=', 'delayed-auto', 'obj=', $logOnAccount, 'DisplayName=', 'IDLifecycle Identity Lifecycle Service')
            if ($isGmsa){
                $scArgs += @('password=', '""')
            }

            $null = Invoke-ScExe -Arguments $scArgs
        }
    }
    else{
        Invoke-InstallStep -Action 'ConfigureService' -Identity $ServiceName -Details "binPath/account -> $binPath as $logOnAccount" `
            -IsSatisfied {
            ([string]$existing.PathName -ceq $binPath) -and
            ([string]$existing.StartName -ieq $logOnAccount)
        } `
            -Apply {
            $scArgs = @('config', $ServiceName, 'binPath=', $binPath, 'obj=', $logOnAccount)
            if ($isGmsa){
                $scArgs += @('password=', '""')
            }

            $null = Invoke-ScExe -Arguments $scArgs
        }

        Invoke-InstallStep -Action 'SetStartType' -Identity $ServiceName -Details 'Automatic (Delayed Start)' `
            -IsSatisfied { ([string]$existing.StartMode -eq 'Auto') -and $existingDelayed } `
            -Apply { $null = Invoke-ScExe -Arguments @('config', $ServiceName, 'start=', 'delayed-auto') }
    }

    Invoke-InstallStep -Action 'SetDescription' -Identity $ServiceName `
        -IsSatisfied { $existing -and ($existingDescription -ceq $serviceDescription) } `
        -Apply { $null = Invoke-ScExe -Arguments @('description', $ServiceName, $serviceDescription) }

    Invoke-InstallStep -Action 'SetFailureActions' -Identity $ServiceName -Details 'restart/5s, restart/30s, restart/60s; reset 24h; also on non-crash failures' `
        -IsSatisfied {
        if (-not $existing){
            return $false
        }

        $text = (& sc.exe qfailure $ServiceName 2>&1) -join "`n"
        $delays = @([regex]::Matches($text, 'RESTART\s+--\s+Delay\s+=\s+(\d+)') | ForEach-Object { $_.Groups[1].Value })
        $reset = [regex]::Match($text, 'RESET_PERIOD[^:]*:\s*(\d+)')
        $flag = (& sc.exe qfailureflag $ServiceName 2>&1) -join "`n"
        return (
            ($delays -join ',' -eq '5000,30000,60000') -and
            ($reset.Success -and ($reset.Groups[1].Value -eq '86400')) -and
            ($flag -match 'TRUE')
        )
    } `
        -Apply {
        $null = Invoke-ScExe -Arguments @('failure', $ServiceName, 'reset=', '86400', 'actions=', 'restart/5000/restart/30000/restart/60000')
        $null = Invoke-ScExe -Arguments @('failureflag', $ServiceName, '1')
    }

    Stop-IDLPhase -Phase $phase

    $exitCode = (($script:FailedSteps -gt 0) ? (1) : (0))
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
