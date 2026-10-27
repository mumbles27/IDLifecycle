<#
.SYNOPSIS
    IDLifecycle tray icon: service status, Open admin console, basic web console settings, start / stop / restart.

.DESCRIPTION
    Runs in the notification area of every user who signs in to the server (the installer adds it to the common
    Startup folder unless -NoTray). It runs as that user and changes nothing itself:
      * Open admin console (also a double-click) opens console_url from <InstallDir>\tray.json in the browser.
      * Settings... shows the service, its state, version, console address and data folder, and edits the web console
        address, the HTTPS certificate/key files and the log level.
      * Restart / Stop / Start service, and saving settings, run Invoke-IDLTrayAction.ps1 (next to this script) with
        "Run as administrator" - one UAC prompt per action. That helper checks everything again.
      * Open log folder opens the first log folder this user can read (the service's <data_dir>\logs, else the setup
        log folder); without one the item is disabled.
      * Exit closes the tray only; the service keeps running.
    The tooltip shows "<product> - <state>"; the state is read every 10 seconds, and a notification appears when the
    service stops without the tray having stopped it. The tray closes itself when tray.json disappears (uninstall).
    One tray per user session (mutex Local\IDLifecycleTray). Icon and product name come from Get-IDLBranding
    (scripts\branding, or a trusted %ProgramData%\IDLifecycle-Branding).

.PARAMETER TrayInfoPath
    tray.json written by the installer. Default: <the folder above this script>\tray.json.

.PARAMETER DefineOnly
    Define the functions and return without showing anything (used by the Pester tests).

.EXAMPLE
    pwsh -NoProfile -STA -WindowStyle Hidden -File "C:\Program Files\IDLifecycle\scripts\Start-IDLifecycleTray.ps1"

.NOTES
    Requires PowerShell 7, Windows with the desktop experience (WinForms + WPF) and an STA thread (-STA).
    Exit codes: 0 closed normally (or already running for this user), 2 tray.json missing or invalid, 3 no desktop
    (WinForms/WPF not available), 5 unexpected error.
#>
#Requires -Version 7.0
[CmdletBinding()]
param(
    [string]$TrayInfoPath = '',

    [switch]$DefineOnly
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
Import-Module (Join-Path -Path $PSScriptRoot -ChildPath 'IDLifecycle.Common.psm1') -Force

if (-not $TrayInfoPath){
    $TrayInfoPath = Join-Path -Path (Split-Path -Parent $PSScriptRoot) -ChildPath 'tray.json'
}

$script:HelperPath = Join-Path -Path $PSScriptRoot -ChildPath 'Invoke-IDLTrayAction.ps1'
$script:BrandDir = Join-Path -Path $PSScriptRoot -ChildPath 'branding'
$script:SetupLogDir = Join-Path -Path $env:ProgramData -ChildPath 'IDLifecycle-Setup\Logs'
$script:PollSeconds = 10

#region Pure helpers (tested with -DefineOnly)

function Get-IDLTrayStateText {
    # Win32 service / ServiceController states -> words for people.
    [CmdletBinding()]
    param(
        [AllowEmptyString()][string]$State
    )

    switch ($State){
        'Running' { return 'Running' }
        'Stopped' { return 'Stopped' }
        { $_ -in @('StartPending', 'Start Pending') } { return 'Starting' }
        { $_ -in @('StopPending', 'Stop Pending') } { return 'Stopping' }
        'Paused' { return 'Paused' }
        'NotInstalled' { return 'Not installed' }
        default { return 'Unknown' }
    }
}

function Get-IDLTrayTooltip {
    # "<product> - <state>", at most 63 characters (the NotifyIcon limit on every Windows version).
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Product,
        [AllowEmptyString()][string]$State
    )

    $text = '{0} - {1}' -f $Product, (Get-IDLTrayStateText -State $State)
    if ($text.Length -gt 63){
        $text = $text.Substring(0, 60) + '...'
    }

    return $text
}

function Get-IDLTrayMenuModel {
    <#
      The tray menu for a service state: @{ Key; Text; Enabled; Bold }[] in display order; Key '-' is a separator.
      Start is offered when the service is stopped, Stop when it runs or is starting; Restart only while it runs.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][System.Collections.IDictionary]$Info,
        [AllowEmptyString()][string]$State,
        [AllowEmptyString()][string]$LogFolder = ''
    )

    $stateText = Get-IDLTrayStateText -State $State
    $known = ($State -ne 'NotInstalled')
    $status = "Service: $stateText"
    if ($Info['version']){
        $status += " (version $($Info['version']))"
    }

    $items = [System.Collections.Generic.List[object]]::new()
    $items.Add(@{ Key = 'open'; Text = 'Open admin console'; Enabled = $true; Bold = $true })
    $items.Add(@{ Key = 'status'; Text = $status; Enabled = $false; Bold = $false })
    $items.Add(@{ Key = '-'; Text = ''; Enabled = $false; Bold = $false })
    $items.Add(@{ Key = 'settings'; Text = 'Settings...'; Enabled = $true; Bold = $false })
    $items.Add(@{ Key = 'restart'; Text = 'Restart service'; Enabled = ($State -eq 'Running'); Bold = $false })
    if ($State -in @('Running', 'StartPending', 'Start Pending')){
        $items.Add(@{ Key = 'stop'; Text = 'Stop service'; Enabled = $true; Bold = $false })
    }
    else{
        $items.Add(@{ Key = 'start'; Text = 'Start service'; Enabled = ($known -and ($State -eq 'Stopped')); Bold = $false })
    }

    $items.Add(@{ Key = 'logs'; Text = 'Open log folder'; Enabled = [bool]$LogFolder; Bold = $false })
    $items.Add(@{ Key = '-'; Text = ''; Enabled = $false; Bold = $false })
    $items.Add(@{ Key = 'exit'; Text = 'Exit'; Enabled = $true; Bold = $false })
    return [object[]]$items.ToArray()
}

function ConvertTo-IDLCommandLineArg {
    # Quotes one argument for a Windows command line (CommandLineToArgvW rules): "..." with \ before a quote doubled.
    [CmdletBinding()]
    param(
        [AllowEmptyString()][string]$Value
    )

    if (
        ($Value.Length -gt 0) -and
        ($Value -notmatch '[\s"]')
    ){
        return $Value
    }

    $sb = [System.Text.StringBuilder]::new('"')
    $slashes = 0
    foreach ($ch in $Value.ToCharArray()){
        if ($ch -eq '\'){
            $slashes++
            continue
        }

        if ($ch -eq '"'){
            $null = $sb.Append('\', 2 * $slashes + 1)
        }
        else{
            $null = $sb.Append('\', $slashes)
        }

        $slashes = 0
        $null = $sb.Append($ch)
    }

    $null = $sb.Append('\', 2 * $slashes)
    $null = $sb.Append('"')
    return $sb.ToString()
}

function New-IDLTrayActionCommand {
    <#
      The elevated helper's command line: this pwsh -NoProfile -NonInteractive -ExecutionPolicy <tray.json
      execution_policy, the one the service uses for its scripts> -File <Invoke-IDLTrayAction.ps1 next to this script>
      -Action ... -ServiceName ...
      Settings (SetConfig) is a hashtable with any of Listen, TlsCertFile, TlsKeyFile, LogLevel; only those are passed.
      Returns @{ FilePath; Arguments (one string); ArgumentList (the same, unquoted, for tests) }.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][ValidateSet('Start', 'Stop', 'Restart', 'SetConfig')][string]$Action,
        [Parameter(Mandatory)][System.Collections.IDictionary]$Info,
        [hashtable]$Settings = @{},
        [switch]$Restart,
        [string]$ResultFile = '',
        [string]$HelperPath = $script:HelperPath
    )

    $list = [System.Collections.Generic.List[string]]::new()
    foreach ($a in @('-NoProfile', '-NonInteractive', '-ExecutionPolicy', [string]$Info['execution_policy'], '-File', $HelperPath, '-Action', $Action, '-ServiceName', [string]$Info['service_name'])){
        $list.Add($a)
    }

    if ($Action -eq 'SetConfig'){
        $list.Add('-ConfigPath')
        $list.Add([string]$Info['config_path'])
        foreach ($name in @('Listen', 'TlsCertFile', 'TlsKeyFile', 'LogLevel')){
            if ($Settings.ContainsKey($name)){
                $list.Add("-$name")
                $list.Add([string]$Settings[$name])
            }
        }

        if ($Restart){
            $list.Add('-Restart')
        }
    }

    if ($ResultFile){
        $list.Add('-ResultFile')
        $list.Add($ResultFile)
    }

    return @{
        FilePath     = [System.Environment]::ProcessPath
        Arguments    = (($list | ForEach-Object { ConvertTo-IDLCommandLineArg -Value $_ }) -join ' ')
        ArgumentList = [string[]]$list.ToArray()
    }
}

function Get-IDLTraySettingsChanges {
    <#
      What the settings window changes: a hashtable with only Listen / LogLevel when they differ from tray.json, and
      the HTTPS files when the form names new ones (both) or TlsOff switches HTTPS off (both set to ''). Empty file
      boxes keep the current HTTPS setting (tray.json only knows whether HTTPS is on, not the file names).
      Form: @{ Listen; TlsCertFile; TlsKeyFile; LogLevel; TlsOff }.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][System.Collections.IDictionary]$Info,
        [Parameter(Mandatory)][hashtable]$Form
    )

    $changes = @{}
    $listen = ([string]$Form['Listen']).Trim()
    if ($listen -ne [string]$Info['listen']){
        $changes['Listen'] = $listen
    }

    if (
        ($Form['LogLevel']) -and
        ([string]$Form['LogLevel'] -ne [string]$Info['log_level'])
    ){
        $changes['LogLevel'] = [string]$Form['LogLevel']
    }

    $cert = ([string]$Form['TlsCertFile']).Trim()
    $key = ([string]$Form['TlsKeyFile']).Trim()
    if ([bool]$Form['TlsOff']){
        if ([bool]$Info['tls']){
            $changes['TlsCertFile'] = ''
            $changes['TlsKeyFile'] = ''
        }
    }
    elseif ($cert -or $key){
        $changes['TlsCertFile'] = $cert
        $changes['TlsKeyFile'] = $key
    }

    return $changes
}

function Get-IDLTraySettingsProblems {
    <#
      The settings window's checks (same rules and wording as the setup window and the elevated helper) for the
      settings that WILL apply: new HTTPS files when given, none when TlsOff, else the current state (Info tls).
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][System.Collections.IDictionary]$Info,
        [Parameter(Mandatory)][hashtable]$Form
    )

    $cert = ([string]$Form['TlsCertFile']).Trim()
    $key = ([string]$Form['TlsKeyFile']).Trim()
    $problems = [System.Collections.Generic.List[string]]::new()
    if ([string]::IsNullOrWhiteSpace([string]$Form['Listen'])){
        $problems.Add('Enter the web console address (host:port).')
    }

    $keep = (-not [bool]$Form['TlsOff']) -and (-not $cert) -and (-not $key) -and ([bool]$Info['tls'])
    $check = @{ Listen = ([string]$Form['Listen']); LogLevel = ([string]$Form['LogLevel']) }
    if (
        (-not $keep) -and
        (-not [bool]$Form['TlsOff'])
    ){
        $check['TlsCertFile'] = $cert
        $check['TlsKeyFile'] = $key
    }

    foreach ($p in @(Get-IDLConsoleSettingsProblems @check)){
        # HTTPS that stays on with its current files (checked by the service) satisfies the "needs HTTPS" rule.
        if (
            (-not $keep) -or
            ($p -notmatch 'needs HTTPS')
        ){
            $problems.Add($p)
        }
    }

    return [string[]]$problems.ToArray()
}

function Test-IDLTrayFolderReadable {
    [CmdletBinding()]
    param(
        [AllowEmptyString()][string]$Path
    )

    if (
        (-not $Path) -or
        (-not (Test-Path -LiteralPath $Path -PathType Container))
    ){
        return $false
    }

    try{
        $null = [System.IO.Directory]::EnumerateFileSystemEntries($Path).GetEnumerator().MoveNext()
        return $true
    }
    catch{
        return $false
    }
}

function Get-IDLTrayLogFolder {
    # The first log folder this user can read: <data_dir>\logs (admins only by default), else the setup logs; or ''.
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][System.Collections.IDictionary]$Info
    )

    foreach ($dir in @((Join-Path -Path ([string]$Info['data_dir']) -ChildPath 'logs'), $script:SetupLogDir)){
        if (Test-IDLTrayFolderReadable -Path $dir){
            return $dir
        }
    }

    return ''
}

function New-IDLTrayResultPath {
    # A fresh result file name in this user's temp folder (the helper refuses any other place).
    return (Join-Path -Path ([System.IO.Path]::GetTempPath()) -ChildPath ('IDLTray_{0}.json' -f [guid]::NewGuid().ToString('N')))
}

function Read-IDLTrayResult {
    # The helper's result: @{ Ok; Message; Cancelled }. ExitCode -1 = the UAC prompt was cancelled.
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][int]$ExitCode,
        [Parameter(Mandatory)][string]$ResultFile
    )

    if ($ExitCode -eq -1){
        return @{ Ok = $false; Cancelled = $true; Message = 'Cancelled: administrator rights are needed for this.' }
    }

    $message = ''
    $ok = ($ExitCode -eq 0)
    if (Test-Path -LiteralPath $ResultFile -PathType Leaf){
        try{
            $doc = [System.IO.File]::ReadAllText($ResultFile) | ConvertFrom-Json -AsHashtable
            $ok = ([bool]$doc['ok']) -and ($ExitCode -eq 0)
            $message = [string]$doc['message']
        }
        catch{
            $message = ''
        }
        finally{
            Remove-Item -LiteralPath $ResultFile -Force -ErrorAction SilentlyContinue
        }
    }

    if (-not $message){
        $message = switch ($ExitCode){
            0 { 'Done.' }
            2 { 'The request was refused (see the setup log folder for the reason).' }
            4 { 'Administrator rights are needed for this.' }
            default { "The action failed (exit code $ExitCode); see the setup log folder." }
        }
    }

    return @{ Ok = $ok; Cancelled = $false; Message = $message }
}

#endregion

#region System-touching functions

function Get-IDLTrayServiceState {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Name
    )

    $svc = Get-Service -Name $Name -ErrorAction SilentlyContinue
    if ($null -eq $svc){
        return 'NotInstalled'
    }

    return [string]$svc.Status
}

function Invoke-IDLTrayElevated {
    # Runs the helper as administrator and waits. Returns its exit code, or -1 when the UAC prompt was cancelled.
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][hashtable]$Command
    )

    try{
        $p = Start-Process -FilePath $Command.FilePath -ArgumentList $Command.Arguments -Verb RunAs -WindowStyle Hidden -Wait -PassThru
        return [int]$p.ExitCode
    }
    catch [System.ComponentModel.Win32Exception]{
        if ($_.Exception.NativeErrorCode -eq 1223){
            return -1
        }

        throw
    }
    catch{
        if ($_.Exception.InnerException -is [System.ComponentModel.Win32Exception] -and $_.Exception.InnerException.NativeErrorCode -eq 1223){
            return -1
        }

        throw
    }
}

function Open-IDLTrayTarget {
    # Opens a validated http(s) URL in the browser, or a folder in Explorer. Nothing else is ever started.
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Target
    )

    if ($Target -match '^https?://'){
        Start-Process -FilePath $Target
        return
    }

    if (Test-Path -LiteralPath $Target -PathType Container){
        Start-Process -FilePath 'explorer.exe' -ArgumentList (ConvertTo-IDLCommandLineArg -Value $Target)
    }
}

#endregion

#region UI

function Invoke-IDLTrayAction {
    # Runs one helper action elevated and returns @{ Ok; Message; Cancelled }.
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Action,
        [Parameter(Mandatory)][System.Collections.IDictionary]$Info,
        [hashtable]$Settings = @{},
        [switch]$Restart
    )

    $result = New-IDLTrayResultPath
    $command = New-IDLTrayActionCommand -Action $Action -Info $Info -Settings $Settings -Restart:$Restart -ResultFile $result
    $code = Invoke-IDLTrayElevated -Command $command
    return (Read-IDLTrayResult -ExitCode $code -ResultFile $result)
}

$script:SettingsXaml = @'
<Window xmlns="http://schemas.microsoft.com/winfx/2006/xaml/presentation"
        xmlns:x="http://schemas.microsoft.com/winfx/2006/xaml"
        Title="Settings" Width="560" SizeToContent="Height" ResizeMode="NoResize" WindowStartupLocation="CenterScreen"
        FontFamily="Segoe UI" FontSize="13" Background="#F4F6F9">
  <StackPanel Margin="20">
    <DockPanel Margin="0,0,0,14">
      <Image x:Name="LogoImage" Width="40" Height="40" Margin="0,0,12,0" DockPanel.Dock="Left"/>
      <StackPanel VerticalAlignment="Center">
        <TextBlock x:Name="ProductText" FontSize="18" FontWeight="SemiBold" Foreground="#14233D"/>
        <TextBlock Text="Basic settings of the service on this server" Foreground="#5A6577"/>
      </StackPanel>
    </DockPanel>
    <Border Background="White" BorderBrush="#D5DBE3" BorderThickness="1" CornerRadius="6" Padding="14" Margin="0,0,0,12">
      <Grid>
        <Grid.ColumnDefinitions><ColumnDefinition Width="150"/><ColumnDefinition Width="*"/></Grid.ColumnDefinitions>
        <Grid.RowDefinitions><RowDefinition/><RowDefinition/><RowDefinition/><RowDefinition/><RowDefinition/></Grid.RowDefinitions>
        <TextBlock Grid.Row="0" Text="Service" Foreground="#5A6577" Margin="0,2"/>
        <TextBlock Grid.Row="0" Grid.Column="1" x:Name="ServiceText" Margin="0,2"/>
        <TextBlock Grid.Row="1" Text="State" Foreground="#5A6577" Margin="0,2"/>
        <TextBlock Grid.Row="1" Grid.Column="1" x:Name="StateText" Margin="0,2"/>
        <TextBlock Grid.Row="2" Text="Version" Foreground="#5A6577" Margin="0,2"/>
        <TextBlock Grid.Row="2" Grid.Column="1" x:Name="VersionText" Margin="0,2"/>
        <TextBlock Grid.Row="3" Text="Admin console" Foreground="#5A6577" Margin="0,2"/>
        <TextBlock Grid.Row="3" Grid.Column="1" x:Name="UrlText" Margin="0,2" TextWrapping="Wrap"/>
        <TextBlock Grid.Row="4" Text="Data folder" Foreground="#5A6577" Margin="0,2"/>
        <TextBlock Grid.Row="4" Grid.Column="1" x:Name="DataText" Margin="0,2" TextWrapping="Wrap"/>
      </Grid>
    </Border>
    <Border Background="White" BorderBrush="#D5DBE3" BorderThickness="1" CornerRadius="6" Padding="14">
      <StackPanel>
        <TextBlock Text="Web console address (host:port)" Margin="0,0,0,3"/>
        <TextBox x:Name="ListenBox" Width="260" HorizontalAlignment="Left"/>
        <TextBlock Foreground="#5A6577" TextWrapping="Wrap" Margin="0,3,0,10" Text="127.0.0.1:8443 = this server only. 0.0.0.0:8443 = reachable from the network, which needs HTTPS (certificate and key below)."/>
        <TextBlock Text="HTTPS certificate file (PEM)" Margin="0,0,0,3"/>
        <DockPanel Margin="0,0,0,8">
          <Button x:Name="CertBrowse" DockPanel.Dock="Right" Content="Browse..." Margin="6,0,0,0" Padding="10,2"/>
          <TextBox x:Name="CertBox"/>
        </DockPanel>
        <TextBlock Text="HTTPS key file (PEM)" Margin="0,0,0,3"/>
        <DockPanel Margin="0,0,0,4">
          <Button x:Name="KeyBrowse" DockPanel.Dock="Right" Content="Browse..." Margin="6,0,0,0" Padding="10,2"/>
          <TextBox x:Name="KeyBox"/>
        </DockPanel>
        <TextBlock x:Name="TlsHint" Foreground="#5A6577" TextWrapping="Wrap" Margin="0,0,0,4" Text="Leave both empty to keep the current HTTPS setting."/>
        <CheckBox x:Name="TlsOffCheck" Content="Switch HTTPS off (only for an address on this server, e.g. 127.0.0.1:8443)" Margin="0,0,0,10"/>
        <TextBlock Text="Log level" Margin="0,0,0,3"/>
        <ComboBox x:Name="LogLevelBox" Width="140" HorizontalAlignment="Left"/>
        <CheckBox x:Name="RestartCheck" Content="Restart the service to apply the changes" IsChecked="True" Margin="0,12,0,0"/>
        <TextBlock x:Name="ProblemText" Foreground="#B42318" TextWrapping="Wrap" Margin="0,10,0,0" Visibility="Collapsed"/>
      </StackPanel>
    </Border>
    <TextBlock Foreground="#5A6577" TextWrapping="Wrap" Margin="0,10,0,0" Text="Saving asks for administrator rights. Everything else is set in the admin console."/>
    <StackPanel Orientation="Horizontal" HorizontalAlignment="Right" Margin="0,14,0,0">
      <Button x:Name="OpenButton" Content="Open admin console" Padding="12,4" Margin="0,0,8,0"/>
      <Button x:Name="SaveButton" Content="Save" Padding="18,4" Margin="0,0,8,0" IsDefault="True"/>
      <Button x:Name="CloseButton" Content="Close" Padding="14,4" IsCancel="True"/>
    </StackPanel>
  </StackPanel>
</Window>
'@

function Get-IDLTraySettingsControlNames {
    return [string[]]@([regex]::Matches($script:SettingsXaml, 'x:Name="(?<n>[A-Za-z0-9_]+)"') | ForEach-Object { $_.Groups['n'].Value })
}

function New-IDLTraySettingsWindow {
    # Loads the settings window (WPF) and returns @{ Window; Ui (name -> control) }. Needs an STA thread.
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]$Branding
    )

    Add-Type -AssemblyName PresentationFramework, PresentationCore, WindowsBase
    $reader = [System.Xml.XmlReader]::Create([System.IO.StringReader]::new($script:SettingsXaml))
    $window = [System.Windows.Markup.XamlReader]::Load($reader)
    $ui = @{}
    foreach ($name in (Get-IDLTraySettingsControlNames)){
        $ui[$name] = $window.FindName($name)
    }

    $window.Title = "$($Branding.ProductName) settings"
    $ui.ProductText.Text = $Branding.ProductName
    foreach ($pair in @(@($Branding.IconPath, 'window'), @($Branding.LogoPngPath, 'logo'))){
        if (-not $pair[0]){
            continue
        }

        try{
            $frame = [System.Windows.Media.Imaging.BitmapFrame]::Create([uri]::new($pair[0]))
            if ($pair[1] -eq 'window'){
                $window.Icon = $frame
            }
            else{
                $ui.LogoImage.Source = $frame
            }
        }
        catch{
            Write-Verbose "Branding image not shown: $($_.Exception.Message)"
        }
    }

    if ((-not $Branding.LogoPngPath) -and $Branding.IconPath){
        try{
            $ui.LogoImage.Source = [System.Windows.Media.Imaging.BitmapFrame]::Create([uri]::new($Branding.IconPath))
        }
        catch{
            Write-Verbose "Icon not shown: $($_.Exception.Message)"
        }
    }

    foreach ($level in @('debug', 'info', 'warn', 'error')){
        $null = $ui.LogLevelBox.Items.Add($level)
    }

    return @{ Window = $window; Ui = $ui }
}

function Show-IDLTraySettings {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][System.Collections.IDictionary]$Info,
        [Parameter(Mandatory)]$Branding,
        [Parameter(Mandatory)][string]$State
    )

    $w = New-IDLTraySettingsWindow -Branding $Branding
    $ui = $w.Ui
    $ui.ServiceText.Text = [string]$Info['service_name']
    $ui.StateText.Text = Get-IDLTrayStateText -State $State
    $ui.VersionText.Text = ((($Info['version'])) ? ([string]$Info['version']) : ('(unknown)'))
    $ui.UrlText.Text = [string]$Info['console_url']
    $ui.DataText.Text = [string]$Info['data_dir']
    $ui.ListenBox.Text = [string]$Info['listen']
    $ui.LogLevelBox.SelectedItem = ((($Info['log_level'])) ? ([string]$Info['log_level']) : ('info'))
    # The TLS file names are not in tray.json (it only says whether HTTPS is on); empty boxes mean "keep".
    $ui.CertBox.Text = ''
    $ui.KeyBox.Text = ''
    $ui.TlsHint.Text = ((([bool]$Info['tls'])) ? ('HTTPS is on. Leave both empty to keep the current certificate.') : ('HTTPS is off. Choose both files to switch it on.'))
    $ui.TlsOffCheck.Visibility = ((([bool]$Info['tls'])) ? ('Visible') : ('Collapsed'))

    # Event handlers run in this script's scope: they use $script: state (no closures, which would not see this
    # script's functions).
    $script:SettingsWindow = $w.Window
    $script:SettingsUi = $ui
    $script:SettingsInfo = $Info
    $script:SettingsBrand = $Branding
    $ui.CertBrowse.Add_Click({ Select-IDLTrayFile -Box $script:SettingsUi.CertBox -Title 'HTTPS certificate file' })
    $ui.KeyBrowse.Add_Click({ Select-IDLTrayFile -Box $script:SettingsUi.KeyBox -Title 'HTTPS key file' })
    $ui.OpenButton.Add_Click({ Open-IDLTrayTarget -Target ([string]$script:SettingsInfo['console_url']) })
    $ui.CloseButton.Add_Click({ $script:SettingsWindow.Close() })
    $ui.SaveButton.Add_Click({ Save-IDLTraySettings })
    try{
        [void]$w.Window.ShowDialog()
    }
    finally{
        $script:SettingsWindow = $null
        $script:SettingsUi = $null
    }
}

function Select-IDLTrayFile {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]$Box,
        [Parameter(Mandatory)][string]$Title
    )

    $dialog = [Microsoft.Win32.OpenFileDialog]::new()
    $dialog.Title = $Title
    $dialog.Filter = 'PEM files (*.pem;*.crt;*.cer;*.key)|*.pem;*.crt;*.cer;*.key|All files (*.*)|*.*'
    if ($dialog.ShowDialog($script:SettingsWindow)){
        $Box.Text = $dialog.FileName
    }
}

function Save-IDLTraySettings {
    # The settings window's Save: check, then SetConfig through the elevated helper.
    $ui = $script:SettingsUi
    $info = $script:SettingsInfo
    $form = @{
        Listen      = $ui.ListenBox.Text
        TlsCertFile = $ui.CertBox.Text
        TlsKeyFile  = $ui.KeyBox.Text
        LogLevel    = [string]$ui.LogLevelBox.SelectedItem
        TlsOff      = [bool]$ui.TlsOffCheck.IsChecked
    }
    $problems = @(Get-IDLTraySettingsProblems -Info $info -Form $form)
    if ($problems.Count -gt 0){
        $ui.ProblemText.Text = $problems -join "`n"
        $ui.ProblemText.Visibility = 'Visible'
        return
    }

    $changes = Get-IDLTraySettingsChanges -Info $info -Form $form
    if ($changes.Count -eq 0){
        $ui.ProblemText.Text = 'Nothing changed.'
        $ui.ProblemText.Visibility = 'Visible'
        return
    }

    $ui.SaveButton.IsEnabled = $false
    try{
        $r = Invoke-IDLTrayAction -Action 'SetConfig' -Info $info -Settings $changes -Restart:([bool]$ui.RestartCheck.IsChecked)
    }
    catch{
        $r = @{ Ok = $false; Cancelled = $false; Message = $_.Exception.Message }
    }
    finally{
        $ui.SaveButton.IsEnabled = $true
    }

    if ($r.Ok){
        [void][System.Windows.MessageBox]::Show($script:SettingsWindow, $r.Message, $script:SettingsBrand.ProductName, 'OK', 'Information')
        $script:SettingsWindow.Close()
    }
    elseif (-not $r.Cancelled){
        $ui.ProblemText.Text = $r.Message
        $ui.ProblemText.Visibility = 'Visible'
    }
}

function New-IDLTrayIcon {
    # Builds the NotifyIcon and its menu (WinForms). Returns @{ Icon; Menu; Items (Key -> ToolStripMenuItem) }.
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]$Branding
    )

    Add-Type -AssemblyName System.Windows.Forms, System.Drawing
    $icon = $null
    if ($Branding.IconPath){
        try{
            $icon = [System.Drawing.Icon]::new($Branding.IconPath, [System.Windows.Forms.SystemInformation]::SmallIconSize)
        }
        catch{
            $icon = $null
        }
    }

    if ($null -eq $icon){
        $icon = [System.Drawing.SystemIcons]::Application
    }

    $tray = [System.Windows.Forms.NotifyIcon]::new()
    $tray.Icon = $icon
    $tray.Text = $Branding.ProductName
    $menu = [System.Windows.Forms.ContextMenuStrip]::new()
    $tray.ContextMenuStrip = $menu
    return @{ Icon = $tray; Menu = $menu; Items = @{} }
}

function Update-IDLTrayMenu {
    # Rebuilds the menu from Get-IDLTrayMenuModel; a click runs OnClick with the item's key (its Tag).
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][hashtable]$Tray,
        [Parameter(Mandatory)][object[]]$Model,
        [Parameter(Mandatory)][scriptblock]$OnClick
    )

    $script:MenuClick = $OnClick
    $Tray.Menu.Items.Clear()
    $Tray.Items = @{}
    foreach ($m in $Model){
        if ($m.Key -eq '-'){
            $null = $Tray.Menu.Items.Add([System.Windows.Forms.ToolStripSeparator]::new())
            continue
        }

        $item = [System.Windows.Forms.ToolStripMenuItem]::new($m.Text)
        $item.Enabled = [bool]$m.Enabled
        $item.Tag = $m.Key
        if ($m.Bold){
            $item.Font = [System.Drawing.Font]::new($item.Font, [System.Drawing.FontStyle]::Bold)
        }

        $item.Add_Click({
                param($source, $clickArgs)
                & $script:MenuClick ([string]$source.Tag)
            })
        $null = $Tray.Menu.Items.Add($item)
        $Tray.Items[$m.Key] = $item
    }
}

function Start-IDLTray {
    # Runs the tray until Exit (or until tray.json disappears).
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$InfoPath
    )

    $script:InfoPath = $InfoPath
    $script:Info = Read-IDLTrayInfo -Path $InfoPath
    $script:Branding = Get-IDLBranding -DefaultDir $script:BrandDir
    $script:Tray = New-IDLTrayIcon -Branding $script:Branding
    $script:LastState = Get-IDLTrayServiceState -Name ([string]$script:Info['service_name'])
    $script:ExpectStop = $false
    $script:MenuSignature = ''

    $script:Refresh = {
        $script:LastState = Get-IDLTrayServiceState -Name ([string]$script:Info['service_name'])
        $script:Tray.Icon.Text = Get-IDLTrayTooltip -Product $script:Branding.ProductName -State $script:LastState
        $model = Get-IDLTrayMenuModel -Info $script:Info -State $script:LastState -LogFolder (Get-IDLTrayLogFolder -Info $script:Info)
        # Rebuild only when something changed, so an open menu is not torn down under the mouse.
        $signature = ($model | ForEach-Object { '{0}|{1}|{2}' -f $_.Key, $_.Text, $_.Enabled }) -join ';'
        if ($signature -ne $script:MenuSignature){
            $script:MenuSignature = $signature
            Update-IDLTrayMenu -Tray $script:Tray -Model $model -OnClick $script:OnClick
        }
    }

    $script:OnClick = {
        param($Key)
        try{
            switch ($Key){
                'open' { Open-IDLTrayTarget -Target ([string]$script:Info['console_url']) }
                'settings' {
                    Show-IDLTraySettings -Info $script:Info -Branding $script:Branding -State $script:LastState
                    try{
                        $script:Info = Read-IDLTrayInfo -Path $script:InfoPath
                    }
                    catch{
                        Write-Verbose $_.Exception.Message
                    }
                }
                'logs' {
                    $dir = Get-IDLTrayLogFolder -Info $script:Info
                    if ($dir){
                        Open-IDLTrayTarget -Target $dir
                    }
                }
                'exit' {
                    $script:Tray.Icon.Visible = $false
                    [System.Windows.Forms.Application]::Exit()
                    return
                }
                default {
                    $action = (Get-Culture).TextInfo.ToTitleCase($Key)
                    $script:ExpectStop = ($Key -in @('stop', 'restart'))
                    $r = Invoke-IDLTrayAction -Action $action -Info $script:Info
                    if (-not $r.Cancelled){
                        $script:Tray.Icon.ShowBalloonTip(5000, $script:Branding.ProductName, $r.Message, ((($r.Ok)) ? ([System.Windows.Forms.ToolTipIcon]::Info) : ([System.Windows.Forms.ToolTipIcon]::Warning)))
                    }

                    $script:ExpectStop = $false
                }
            }
        }
        catch{
            [void][System.Windows.Forms.MessageBox]::Show($_.Exception.Message, $script:Branding.ProductName, 'OK', 'Error')
        }

        & $script:Refresh
    }

    $script:Tray.Icon.Add_MouseDoubleClick({ & $script:OnClick 'open' })
    & $script:Refresh
    $script:Tray.Icon.Visible = $true
    if ($script:Branding.Warning){
        $script:Tray.Icon.ShowBalloonTip(8000, $script:Branding.ProductName, $script:Branding.Warning, [System.Windows.Forms.ToolTipIcon]::Warning)
    }

    $timer = [System.Windows.Forms.Timer]::new()
    $timer.Interval = $script:PollSeconds * 1000
    $timer.Add_Tick({
            if (-not (Test-Path -LiteralPath $script:InfoPath -PathType Leaf)){
                # Uninstalled: the tray goes away by itself.
                $script:Tray.Icon.Visible = $false
                [System.Windows.Forms.Application]::Exit()
                return
            }

            $before = $script:LastState
            & $script:Refresh
            if (
                ($before -eq 'Running') -and
                ($script:LastState -eq 'Stopped') -and
                (-not $script:ExpectStop)
            ){
                $script:Tray.Icon.ShowBalloonTip(8000, $script:Branding.ProductName, "The service '$($script:Info['service_name'])' has stopped.", [System.Windows.Forms.ToolTipIcon]::Warning)
            }
        })
    $timer.Start()
    try{
        [System.Windows.Forms.Application]::Run()
    }
    finally{
        $timer.Stop()
        $script:Tray.Icon.Visible = $false
        $script:Tray.Icon.Dispose()
    }
}

#endregion

if ($DefineOnly){
    return
}

$mutex = [System.Threading.Mutex]::new($false, 'Local\IDLifecycleTray')
$owned = $false
try{
    try{
        $owned = $mutex.WaitOne(0)
    }
    catch [System.Threading.AbandonedMutexException]{
        $owned = $true
    }

    if (-not $owned){
        exit 0 # already running in this session
    }

    if (-not (Test-Path -LiteralPath $TrayInfoPath -PathType Leaf)){
        [Console]::Error.WriteLine("Start-IDLifecycleTray: $TrayInfoPath not found; IDLifecycle is not installed here.")
        exit 2
    }

    try{
        $null = Read-IDLTrayInfo -Path $TrayInfoPath
    }
    catch{
        [Console]::Error.WriteLine("Start-IDLifecycleTray: $($_.Exception.Message)")
        exit 2
    }

    try{
        Add-Type -AssemblyName System.Windows.Forms, System.Drawing
    }
    catch{
        [Console]::Error.WriteLine('Start-IDLifecycleTray: Windows Forms is not available (Server Core has no desktop).')
        exit 3
    }

    if ([System.Threading.Thread]::CurrentThread.GetApartmentState() -ne [System.Threading.ApartmentState]::STA){
        [Console]::Error.WriteLine('Start-IDLifecycleTray: start it with pwsh -STA (the tray needs a single-threaded apartment).')
        exit 3
    }

    [System.Windows.Forms.Application]::EnableVisualStyles()
    Start-IDLTray -InfoPath $TrayInfoPath
    exit 0
}
catch{
    [Console]::Error.WriteLine("Start-IDLifecycleTray: $($_.Exception.Message)")
    exit 5
}
finally{
    if ($owned){
        $mutex.ReleaseMutex()
    }

    $mutex.Dispose()
}
