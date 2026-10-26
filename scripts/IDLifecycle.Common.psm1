<#
.SYNOPSIS
    Shared helpers for the IDLifecycle PowerShell scripts (logging, JSON stdin/stdout, AD lookups, reports).

.DESCRIPTION
    Imported by every script in this folder:
        Import-Module (Join-Path $PSScriptRoot 'IDLifecycle.Common.psm1') -Force

    Contract reminders (see docs/ARCHITECTURE.md section 6):
      * One JSON request document arrives on stdin (UTF-8, no BOM).
      * Exactly ONE compact JSON document is written to stdout (Write-IDLResponse). Nothing else may ever
        reach stdout, so all human logging goes to the log file, the information stream and (WARN/ERROR) stderr.
      * Passwords are never logged. Any password found in a request is registered as a secret and scrubbed
        from every log line, CSV cell, error message and response.

.NOTES
    Requires PowerShell 7+. AD cmdlets are called from here, so the scripts must import the ActiveDirectory
    module with -Global (otherwise these functions cannot see the cmdlets).
#>

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$script:IDL = $null
$script:IDLSecrets = [System.Collections.Generic.List[string]]::new()
$script:IDLResponseWritten = $false
$script:IDLContainerCache = [System.Collections.Generic.Dictionary[string, bool]]::new([System.StringComparer]::OrdinalIgnoreCase)
$script:IDLUtf8NoBom = [System.Text.UTF8Encoding]::new($false)

# Attributes that scripts must never write through the generic attribute plumbing.
# Keep this a plain list of lower-case single-quoted literals: the Go side parses it with a regex to stay in sync.
$script:IDLProtectedAttributes = @(
    'samaccountname', 'userprincipalname', 'cn', 'name', 'distinguishedname',
    'objectguid', 'objectsid', 'objectclass', 'objectcategory',
    'memberof', 'member',
    'unicodepwd', 'userpassword', 'unixuserpassword',
    'useraccountcontrol', 'primarygroupid', 'pwdlastset', 'lockouttime', 'accountexpires',
    'logonhours', 'userworkstations', 'altsecurityidentities', 'serviceprincipalname',
    'scriptpath', 'homedirectory', 'homedrive', 'profilepath',
    'admincount', 'sidhistory', 'ntsecuritydescriptor',
    'usercertificate', 'usersmimecertificate', 'proxyaddresses', 'msnpallowdialin'
)

# Attribute-name prefixes that are never writable through the generic attribute plumbing.
# (msExch has no hyphen in real attribute names such as msExchHideFromAddressLists, so the prefix is 'msexch'.)
$script:IDLProtectedAttributePrefixes = @(
    'msds-', 'ms-ds-', 'msexch', 'mspki-'
)

# Well-known RIDs of privileged groups: Domain Admins, Schema Admins, Enterprise Admins, Administrators,
# Account Operators, Server Operators, Print Operators, Backup Operators.
$script:IDLPrivilegedGroupRids = @('512', '518', '519', '544', '548', '549', '550', '551')

#region Exceptions and exit codes

function New-IDLException {
    <#
    .SYNOPSIS
        Builds an exception tagged with a failure kind (Input, Dependency, Connection, Fatal).
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][ValidateSet('Input', 'Dependency', 'Connection', 'Fatal')][string]$Kind,
        [Parameter(Mandatory)][string]$Message,
        [System.Exception]$InnerException
    )

    if ($InnerException){
        $ex = [System.InvalidOperationException]::new($Message, $InnerException)
    }
    else{
        $ex = [System.InvalidOperationException]::new($Message)
    }

    $ex.Data['IDLKind'] = $Kind
    return $ex
}

function Get-IDLExitCode {
    <#
    .SYNOPSIS
        Maps an exception to the contract exit code (2 input, 3 dependency, 4 connection, 5 fatal).
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][System.Exception]$Exception
    )

    $cursor = $Exception
    while ($null -ne $cursor){
        if ($cursor.Data.Contains('IDLKind')){
            switch ([string]$cursor.Data['IDLKind']){
                'Input' { return 2 }
                'Dependency' { return 3 }
                'Connection' { return 4 }
                default { return 5 }
            }
        }

        if (Test-IDLConnectionException -Exception $cursor){
            return 4
        }

        $cursor = $cursor.InnerException
    }

    return 5
}

function Test-IDLConnectionException {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][System.Exception]$Exception
    )

    if ($Exception.GetType().Name -in @('ADServerDownException', 'AuthenticationException')){
        return $true
    }

    return ($Exception.Message -match 'Unable to contact the server|server is not operational|server is down|The server is unavailable|Could not connect to')
}

#endregion

#region Secrets and text scrubbing

function Register-IDLSecret {
    <#
    .SYNOPSIS
        Registers a secret so it is scrubbed from all logs, reports, errors and responses.
    #>
    [CmdletBinding()]
    param(
        [AllowNull()][AllowEmptyString()][string]$Secret
    )

    if (
        ([string]::IsNullOrEmpty($Secret)) -or
        ($Secret.Length -lt 4)
    ){
        return
    }

    $variants = @($Secret)
    $json = (ConvertTo-Json -InputObject $Secret -Compress)
    $jsonInner = $json.Substring(1, $json.Length - 2)
    if ($jsonInner -ne $Secret){
        $variants += $jsonInner
    }

    foreach ($variant in $variants){
        if (-not $script:IDLSecrets.Contains($variant)){
            $script:IDLSecrets.Add($variant)
        }
    }
}

function Protect-IDLText {
    <#
    .SYNOPSIS
        Replaces every registered secret in the text with ***.
    #>
    [CmdletBinding()]
    param(
        [AllowNull()][AllowEmptyString()][Parameter(Position = 0)][string]$Text
    )

    if ([string]::IsNullOrEmpty($Text)){
        return $Text
    }

    foreach ($secret in $script:IDLSecrets){
        $Text = $Text.Replace($secret, '***')
    }

    return $Text
}

#endregion

#region Logging, counters, reports

function Write-IDLLog {
    <#
    .SYNOPSIS
        Writes a log line to the log file and the information stream (WARN/ERROR also go to stderr). Never stdout.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory, Position = 0)][AllowEmptyString()][string]$Message,
        [Parameter(Position = 1)][ValidateSet('INFO', 'WARN', 'ERROR', 'DEBUG')][string]$Level = 'INFO'
    )

    $text = Protect-IDLText -Text $Message
    $line = '{0} [{1}] {2}' -f (Get-Date -Format 'yyyy-MM-dd HH:mm:ss'), $Level, $text

    if (
        ($null -ne $script:IDL) -and
        ($script:IDL.LogPath)
    ){
        try{
            [System.IO.File]::AppendAllText($script:IDL.LogPath, $line + [Environment]::NewLine, $script:IDLUtf8NoBom)
        }
        catch{
            [Console]::Error.WriteLine(('{0} [WARN] Could not write log file: {1}' -f (Get-Date -Format 'yyyy-MM-dd HH:mm:ss'), $_.Exception.Message))
        }
    }

    if ([Console]::IsOutputRedirected){
        Write-Information -MessageData $line
    }
    else{
        Write-Information -MessageData $line -InformationAction Continue
    }

    if ($Level -in @('WARN', 'ERROR')){
        [Console]::Error.WriteLine($line)
    }
}

function Start-IDLRun {
    <#
    .SYNOPSIS
        Initialises logging, counters and report plumbing for a script run.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$ScriptName,
        [Parameter(Mandatory)][string]$ScriptRoot,
        [switch]$IsAction,
        [switch]$DryRun,
        [hashtable]$Parameters = @{},
        # Log/report folder; default <ScriptRoot>\Logs. The setup scripts pass a protected folder.
        [string]$LogDir = ''
    )

    # Nothing but the single response document may reach stdout, so silence the other host streams.
    Set-Variable -Name WarningPreference -Value 'SilentlyContinue' -Scope Global
    Set-Variable -Name VerbosePreference -Value 'SilentlyContinue' -Scope Global
    Set-Variable -Name DebugPreference -Value 'SilentlyContinue' -Scope Global
    Set-Variable -Name ProgressPreference -Value 'SilentlyContinue' -Scope Global
    Set-Variable -Name ConfirmPreference -Value 'None' -Scope Global

    $script:IDLResponseWritten = $false
    $script:IDLSecrets.Clear()
    $script:IDLContainerCache.Clear()

    $stamp = Get-Date -Format 'yyyyMMdd_HHmmss'
    $logDir = (([string]::IsNullOrWhiteSpace($LogDir)) ? (Join-Path -Path $ScriptRoot -ChildPath 'Logs') : ($LogDir))
    $logPath = Join-Path -Path $logDir -ChildPath ('{0}_{1}.log' -f $ScriptName, $stamp)
    $reportPath = Join-Path -Path $logDir -ChildPath ('{0}_Report_{1}.csv' -f $ScriptName, $stamp)

    try{
        if (-not (Test-Path -LiteralPath $logDir -PathType Container)){
            $null = New-Item -Path $logDir -ItemType Directory -Force
        }

        if (Test-Path -LiteralPath $logPath){
            $logPath = Join-Path -Path $logDir -ChildPath ('{0}_{1}_{2}.log' -f $ScriptName, $stamp, $PID)
        }

        if (Test-Path -LiteralPath $reportPath){
            $reportPath = Join-Path -Path $logDir -ChildPath ('{0}_Report_{1}_{2}.csv' -f $ScriptName, $stamp, $PID)
        }
    }
    catch{
        if ($IsAction){
            throw (New-IDLException -Kind Fatal -Message "Cannot create log folder '$logDir': $($_.Exception.Message)" -InnerException $_.Exception)
        }

        $logPath = $null
        [Console]::Error.WriteLine("[WARN] Cannot create log folder '$logDir': $($_.Exception.Message)")
    }

    $counters = [ordered]@{
        Total       = 0
        Resolved    = 0
        NotFound    = 0
        Ambiguous   = 0
        WouldChange = 0
        Changed     = 0
        Succeeded   = 0
        Failed      = 0
        Skipped     = 0
    }

    $script:IDL = @{
        ScriptName  = $ScriptName
        IsAction    = [bool]$IsAction
        DryRun      = [bool]$DryRun
        LogPath     = $logPath
        ReportPath  = $reportPath
        Counters    = $counters
        SkipReasons = [ordered]@{}
        Rows        = [System.Collections.Generic.List[object]]::new()
        Stopwatch   = [System.Diagnostics.Stopwatch]::StartNew()
    }

    Write-IDLLog "Script start: $ScriptName (PID $PID, PowerShell $($PSVersionTable.PSVersion))"
    $paramText = ($Parameters.GetEnumerator() | Sort-Object -Property Name | ForEach-Object { '{0}={1}' -f $_.Name, $_.Value }) -join '; '
    Write-IDLLog "Parameters: $paramText"
    $modeText = ($DryRun) ? 'DRY RUN (no changes will be made)' : 'LIVE'
    Write-IDLLog "Mode: $modeText"
}

function Add-IDLCounter {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Name,
        [int]$By = 1
    )

    if ($null -eq $script:IDL){
        return
    }

    if (-not $script:IDL.Counters.Contains($Name)){
        $script:IDL.Counters[$Name] = 0
    }

    $script:IDL.Counters[$Name] = [int]$script:IDL.Counters[$Name] + $By
}

function Add-IDLSkipReason {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Reason
    )

    if ($null -eq $script:IDL){
        return
    }

    if (-not $script:IDL.SkipReasons.Contains($Reason)){
        $script:IDL.SkipReasons[$Reason] = 0
    }

    $script:IDL.SkipReasons[$Reason] = [int]$script:IDL.SkipReasons[$Reason] + 1
}

function Add-IDLReportRow {
    <#
    .SYNOPSIS
        Adds a row to the CSV report (written by Stop-IDLRun for action scripts).
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Action,
        [Parameter(Mandatory)][string]$Result,
        [string]$Identity = '',
        [string]$ObjectIdOrDN = '',
        [string]$Details = '',
        [string]$ErrorText = ''
    )

    if ($null -eq $script:IDL){
        return
    }

    $script:IDL.Rows.Add([pscustomobject][ordered]@{
            Timestamp    = (Get-Date).ToString('yyyy-MM-dd HH:mm:ss')
            Action       = $Action
            Result       = $Result
            Identity     = $Identity
            ObjectIdOrDN = $ObjectIdOrDN
            Details      = $Details
            Error        = $ErrorText
        })
}

function ConvertTo-IDLCsvCell {
    [CmdletBinding()]
    param(
        [AllowNull()][AllowEmptyString()][string]$Value
    )

    if ([string]::IsNullOrEmpty($Value)){
        return ''
    }

    $safe = Protect-IDLText -Text $Value

    # Guard against spreadsheet formula injection when an admin opens the report in Excel.
    if ($safe -match '^[=+@\t\r]|^-[^\s\d]'){
        $safe = "'" + $safe
    }

    return $safe
}

function Start-IDLPhase {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Name
    )

    Write-IDLLog "Phase start: $Name"
    return @{
        Name      = $Name
        Stopwatch = [System.Diagnostics.Stopwatch]::StartNew()
    }
}

function Stop-IDLPhase {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]$Phase
    )

    $Phase.Stopwatch.Stop()
    Write-IDLLog ('Phase done: {0} ({1:N0} ms)' -f $Phase.Name, $Phase.Stopwatch.Elapsed.TotalMilliseconds)
}

function Stop-IDLRun {
    <#
    .SYNOPSIS
        Writes the end-of-run summary to the log and (for action scripts) the CSV report.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][int]$ExitCode
    )

    if ($null -eq $script:IDL){
        return
    }

    $state = $script:IDL

    Write-IDLLog '---- Summary ----'
    foreach ($entry in $state.Counters.GetEnumerator()){
        Write-IDLLog ('  {0,-12} {1}' -f $entry.Key, $entry.Value)
    }

    foreach ($entry in $state.SkipReasons.GetEnumerator()){
        Write-IDLLog ('  Skipped ({0}): {1}' -f $entry.Key, $entry.Value)
    }

    if ($state.IsAction){
        try{
            $header = 'Timestamp,Action,Result,Identity,ObjectIdOrDN,Details,Error'
            $lines = [System.Collections.Generic.List[string]]::new()
            $lines.Add($header)

            foreach ($row in $state.Rows){
                $cells = foreach ($name in @('Timestamp', 'Action', 'Result', 'Identity', 'ObjectIdOrDN', 'Details', 'Error')){
                    '"' + ((ConvertTo-IDLCsvCell -Value ([string]$row.$name)).Replace('"', '""')) + '"'
                }

                $lines.Add($cells -join ',')
            }

            [System.IO.File]::WriteAllLines($state.ReportPath, $lines, $script:IDLUtf8NoBom)
            Write-IDLLog "Report written: $($state.ReportPath)"
        }
        catch{
            Write-IDLLog "Could not write CSV report: $($_.Exception.Message)" 'ERROR'
        }
    }

    Write-IDLLog ('Script end: {0} ExitCode={1} Elapsed={2:N1}s' -f $state.ScriptName, $ExitCode, $state.Stopwatch.Elapsed.TotalSeconds)
}

#endregion

#region JSON in/out

function Read-IDLRequest {
    <#
    .SYNOPSIS
        Reads the UTF-8 JSON request from stdin and returns it as a case-insensitive hashtable.
    #>
    [CmdletBinding()]
    param()

    try{
        [Console]::InputEncoding = $script:IDLUtf8NoBom
        [Console]::OutputEncoding = $script:IDLUtf8NoBom
    }
    catch{
        Write-IDLLog "Could not set console encodings (continuing): $($_.Exception.Message)" 'DEBUG'
    }

    $raw = [Console]::In.ReadToEnd()
    if ($null -ne $raw){
        $raw = $raw.TrimStart([char]0xFEFF)
    }

    if ([string]::IsNullOrWhiteSpace($raw)){
        throw (New-IDLException -Kind Input -Message 'Request is empty: expected one JSON document on stdin.')
    }

    try{
        $request = ConvertFrom-Json -InputObject $raw -AsHashtable
    }
    catch{
        throw (New-IDLException -Kind Input -Message 'Request is not valid JSON.')
    }

    if ($request -isnot [System.Collections.IDictionary]){
        throw (New-IDLException -Kind Input -Message 'Request must be a JSON object.')
    }

    # Register passwords before anything else is logged.
    $items = Get-IDLField -Object $request -Name 'items'
    if ($items -is [System.Collections.IList]){
        foreach ($item in $items){
            if ($item -is [System.Collections.IDictionary]){
                Register-IDLSecret -Secret (ConvertTo-IDLString -Value (Get-IDLField -Object $item -Name 'password'))
            }
        }
    }

    return $request
}

function Write-IDLResponse {
    <#
    .SYNOPSIS
        Emits exactly one compact JSON document on stdout.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]$Response
    )

    if ($script:IDLResponseWritten){
        throw 'Write-IDLResponse called more than once.'
    }

    $script:IDLResponseWritten = $true
    $json = ConvertTo-Json -InputObject $Response -Depth 12 -Compress
    Write-Output (Protect-IDLText -Text $json)
}

function New-IDLErrorResponse {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Message
    )

    return [ordered]@{
        ok    = $false
        error = (Protect-IDLText -Text $Message)
    }
}

function Get-IDLField {
    <#
    .SYNOPSIS
        Case-insensitive, strict-mode-safe lookup of a key in a hashtable/dictionary.
    #>
    [CmdletBinding()]
    param(
        [AllowNull()]$Object,
        [Parameter(Mandatory)][string]$Name,
        $Default = $null
    )

    if ($Object -isnot [System.Collections.IDictionary]){
        return $Default
    }

    foreach ($key in $Object.Keys){
        if ([string]$key -ieq $Name){
            return , $Object[$key]
        }
    }

    return $Default
}

function ConvertTo-IDLString {
    <#
    .SYNOPSIS
        Renders a scalar value as an invariant string ($null stays $null).
    #>
    [CmdletBinding()]
    param(
        [AllowNull()]$Value
    )

    if ($null -eq $Value){
        return $null
    }

    if ($Value -is [string]){
        return $Value
    }

    if ($Value -is [bool]){
        return (($Value) ? ('TRUE') : ('FALSE'))
    }

    if ($Value -is [datetime]){
        return $Value.ToString('o', [System.Globalization.CultureInfo]::InvariantCulture)
    }

    if ($Value -is [byte[]]){
        return [System.Convert]::ToBase64String($Value)
    }

    if ($Value -is [System.IFormattable]){
        return $Value.ToString($null, [System.Globalization.CultureInfo]::InvariantCulture)
    }

    return [string]$Value
}

function Get-IDLRequestCommon {
    <#
    .SYNOPSIS
        Validates the common request envelope (server, idAttribute, items) and returns it.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]$Request,
        [switch]$RequireItems
    )

    $server = ConvertTo-IDLString -Value (Get-IDLField -Object $Request -Name 'server')
    if ($null -eq $server){
        $server = ''
    }

    $idAttribute = ConvertTo-IDLString -Value (Get-IDLField -Object $Request -Name 'idAttribute')
    if (-not (Test-IDLAttributeName -Name $idAttribute)){
        throw (New-IDLException -Kind Input -Message "idAttribute is missing or not a valid LDAP attribute name.")
    }

    $items = @()
    if ($RequireItems){
        $rawItems = Get-IDLField -Object $Request -Name 'items'
        if ($rawItems -isnot [System.Collections.IList]){
            throw (New-IDLException -Kind Input -Message "items is missing or not an array.")
        }

        $items = @($rawItems)
    }

    return @{
        Server      = $server.Trim()
        IdAttribute = $idAttribute
        Items       = $items
    }
}

function Test-IDLAttributeName {
    [CmdletBinding()]
    param(
        [AllowNull()][AllowEmptyString()][string]$Name
    )

    if ([string]::IsNullOrWhiteSpace($Name)){
        return $false
    }

    return ($Name -match '^[A-Za-z][A-Za-z0-9\-]{0,63}$')
}

function Get-IDLStringList {
    <#
    .SYNOPSIS
        Coerces a JSON array (or scalar) into a de-duplicated string[] of non-empty values.
    #>
    [CmdletBinding()]
    param(
        [AllowNull()]$Value
    )

    if ($null -eq $Value){
        return , [string[]]@()
    }

    $list = [System.Collections.Generic.List[string]]::new()
    $seen = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::OrdinalIgnoreCase)

    foreach ($entry in @($Value)){
        $text = ConvertTo-IDLString -Value $entry
        if (
            ([string]::IsNullOrWhiteSpace($text)) -or
            (-not $seen.Add($text.Trim()))
        ){
            continue
        }

        $list.Add($text.Trim())
    }

    return , [string[]]$list.ToArray()
}

#endregion

#region Results

function New-IDLResult {
    <#
    .SYNOPSIS
        Builds a per-item result object in the contract shape.
    #>
    [CmdletBinding()]
    param(
        [AllowNull()][AllowEmptyString()][string]$ChangeId,
        [AllowNull()][AllowEmptyString()][string]$ExternalId,
        [Parameter(Mandatory)][ValidateSet('applied', 'would', 'skipped', 'failed')][string]$Status,
        [string]$Message = '',
        [AllowNull()][string]$ObjectGuid,
        [AllowNull()][string]$Dn,
        [AllowNull()][string]$Sam,
        [AllowNull()][string]$Upn,
        [AllowNull()][string]$ErrorText
    )

    return [ordered]@{
        changeId   = $ChangeId
        externalId = $ExternalId
        status     = $Status
        message    = (Protect-IDLText -Text $Message)
        objectGuid = $ObjectGuid
        dn         = $Dn
        sam        = $Sam
        upn        = $Upn
        error      = (Protect-IDLText -Text $ErrorText)
    }
}

function Register-IDLResult {
    <#
    .SYNOPSIS
        Updates run counters for a finished item result and logs it.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]$Result
    )

    switch ($Result.status){
        'applied' {
            Add-IDLCounter -Name 'Changed'
            Add-IDLCounter -Name 'Succeeded'
        }
        'would' {
            Add-IDLCounter -Name 'WouldChange'
            Add-IDLCounter -Name 'Succeeded'
        }
        'skipped' {
            Add-IDLCounter -Name 'Skipped'
            Add-IDLSkipReason -Reason ([string]$Result.message)
        }
        'failed' {
            Add-IDLCounter -Name 'Failed'
        }
    }

    $level = (($Result.status -eq 'failed') ? ('ERROR') : ('INFO'))
    $line = ('Item {0} ({1}): {2} - {3} {4}' -f $Result.changeId, $Result.externalId, $Result.status, $Result.message, $Result.error).TrimEnd()
    Write-IDLLog $line $level
}

function Get-IDLResultSummary {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][AllowEmptyCollection()][object[]]$Results
    )

    $count = {
        param($status)
        @($Results | Where-Object { $_.status -eq $status }).Count
    }

    return [ordered]@{
        total   = $Results.Count
        applied = (& $count 'applied')
        would   = (& $count 'would')
        skipped = (& $count 'skipped')
        failed  = (& $count 'failed')
    }
}

function New-IDLActionResponse {
    <#
    .SYNOPSIS
        Builds the common action-script response document.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][AllowEmptyCollection()][object[]]$Results,
        [switch]$DryRun
    )

    return [ordered]@{
        ok      = $true
        dryRun  = [bool]$DryRun
        results = @($Results)
        summary = (Get-IDLResultSummary -Results $Results)
    }
}

#endregion

#region LDAP / AD helpers

function ConvertTo-IDLLdapFilterValue {
    <#
    .SYNOPSIS
        Escapes a value for use inside an LDAP filter (RFC 4515: \ * ( ) NUL).
    #>
    [CmdletBinding()]
    param(
        [AllowNull()][AllowEmptyString()][string]$Value
    )

    if ([string]::IsNullOrEmpty($Value)){
        return ''
    }

    $sb = [System.Text.StringBuilder]::new()
    foreach ($ch in $Value.ToCharArray()){
        switch ($ch){
            '\' { $null = $sb.Append('\5c') }
            '*' { $null = $sb.Append('\2a') }
            '(' { $null = $sb.Append('\28') }
            ')' { $null = $sb.Append('\29') }
            ([char]0) { $null = $sb.Append('\00') }
            default { $null = $sb.Append($ch) }
        }
    }

    return $sb.ToString()
}

function ConvertTo-IDLLdapGuidValue {
    <#
    .SYNOPSIS
        Renders a GUID as the escaped octet string AD expects in an objectGUID filter.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][guid]$Guid
    )

    return (($Guid.ToByteArray() | ForEach-Object { '\{0:x2}' -f $_ }) -join '')
}

function ConvertTo-IDLDnEscapedValue {
    <#
    .SYNOPSIS
        Escapes a value for use as an RDN value inside a distinguished name (RFC 4514).
    #>
    [CmdletBinding()]
    param(
        [AllowNull()][AllowEmptyString()][string]$Value
    )

    if ([string]::IsNullOrEmpty($Value)){
        return ''
    }

    $escaped = [regex]::Replace($Value, '([\\,+"<>;=])', '\$1')
    if ($escaped.StartsWith('#') -or $escaped.StartsWith(' ')){
        $escaped = '\' + $escaped
    }

    if ($escaped.EndsWith(' ') -and (-not $escaped.EndsWith('\ '))){
        $escaped = $escaped.Substring(0, $escaped.Length - 1) + '\ '
    }

    return $escaped
}

function Get-IDLServerParams {
    <#
    .SYNOPSIS
        Returns a splat hashtable carrying -Server when a domain controller was requested.
    #>
    [CmdletBinding()]
    param(
        [AllowNull()][AllowEmptyString()][string]$Server
    )

    if ([string]::IsNullOrWhiteSpace($Server)){
        return @{}
    }

    return @{ Server = $Server.Trim() }
}

function Get-IDLParentDn {
    [CmdletBinding()]
    param(
        [AllowNull()][AllowEmptyString()][string]$Dn
    )

    $match = [regex]::Match([string]$Dn, '^(?:\\.|[^,\\])*,(.+)$')
    if (-not $match.Success){
        return ''
    }

    return $match.Groups[1].Value
}

function Get-IDLLeafRdn {
    [CmdletBinding()]
    param(
        [AllowNull()][AllowEmptyString()][string]$Dn
    )

    $match = [regex]::Match([string]$Dn, '^((?:\\.|[^,\\])*),.+$')
    if (-not $match.Success){
        return [string]$Dn
    }

    return $match.Groups[1].Value
}

function Test-IDLDistinguishedName {
    [CmdletBinding()]
    param(
        [AllowNull()][AllowEmptyString()][string]$Value
    )

    # sAMAccountName cannot contain '=', so its presence marks a DN.
    return ((-not [string]::IsNullOrEmpty($Value)) -and ($Value.Contains('=')))
}

function Get-IDLAdValue {
    <#
    .SYNOPSIS
        Strict-mode-safe read of an AD property as an invariant string (or $null when unset).
    #>
    [CmdletBinding()]
    param(
        [AllowNull()]$Object,
        [Parameter(Mandatory)][string]$Name
    )

    if ($null -eq $Object){
        return $null
    }

    $value = $null
    $member = $Object.PSObject.Properties[$Name]
    if ($null -ne $member){
        $value = $member.Value
    }
    else{
        try{
            $value = $Object[$Name]
        }
        catch{
            $value = $null
        }
    }

    if ($null -eq $value){
        return $null
    }

    if (
        ($value -isnot [string]) -and
        ($value -isnot [byte[]]) -and
        ($value -is [System.Collections.IEnumerable])
    ){
        $values = @($value)
        if ($values.Count -eq 0){
            return $null
        }

        $value = $values[0]
    }

    $text = ConvertTo-IDLString -Value $value
    if ([string]::IsNullOrEmpty($text)){
        return $null
    }

    return $text
}

function Get-IDLAdValues {
    <#
    .SYNOPSIS
        Reads a multi-valued AD property as string[] (empty when unset).
    #>
    [CmdletBinding()]
    param(
        [AllowNull()]$Object,
        [Parameter(Mandatory)][string]$Name
    )

    if ($null -eq $Object){
        return , [string[]]@()
    }

    $member = $Object.PSObject.Properties[$Name]
    if (
        ($null -eq $member) -or
        ($null -eq $member.Value)
    ){
        return , [string[]]@()
    }

    return , [string[]]@(@($member.Value) | Where-Object { -not [string]::IsNullOrEmpty([string]$_) } | ForEach-Object { [string]$_ })
}

function Split-IDLChunk {
    <#
    .SYNOPSIS
        Splits an array into chunks of the given size (returned as an array of arrays).
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][AllowEmptyCollection()][object[]]$InputArray,
        [int]$Size = 40
    )

    $chunks = [System.Collections.Generic.List[object]]::new()
    for ($i = 0; $i -lt $InputArray.Count; $i += $Size){
        $end = [Math]::Min($i + $Size, $InputArray.Count) - 1
        $chunks.Add(@($InputArray[$i..$end]))
    }

    return , $chunks.ToArray()
}

function New-IDLOrFilter {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string[]]$Terms
    )

    if ($Terms.Count -eq 1){
        return $Terms[0]
    }

    return '(|' + ($Terms -join '') + ')'
}

function Test-IDLAdNotFound {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][System.Exception]$Exception
    )

    return (
        ($Exception.GetType().Name -eq 'ADIdentityNotFoundException') -or
        ($Exception.Message -match 'Cannot find an object with identity|Directory object not found|object not found')
    )
}

function Test-IDLAdConnection {
    <#
    .SYNOPSIS
        Verifies a domain controller is reachable; throws a Connection exception (exit code 4) when not.
    #>
    [CmdletBinding()]
    param(
        [AllowNull()][AllowEmptyString()][string]$Server
    )

    $serverParams = Get-IDLServerParams -Server $Server
    try{
        $rootDse = Get-ADRootDSE @serverParams
        Write-IDLLog "Connected to AD (default naming context: $($rootDse.defaultNamingContext))"
    }
    catch{
        throw (New-IDLException -Kind Connection -Message "Cannot connect to Active Directory: $($_.Exception.Message)" -InnerException $_.Exception)
    }
}

function Test-IDLContainerExists {
    <#
    .SYNOPSIS
        Returns $true when the OU/container DN exists (cached per run).
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Dn,
        [AllowNull()][AllowEmptyString()][string]$Server
    )

    $cached = $false
    if ($script:IDLContainerCache.TryGetValue($Dn, [ref]$cached)){
        return $cached
    }

    $serverParams = Get-IDLServerParams -Server $Server
    $exists = $false
    try{
        $null = Get-ADObject -Identity $Dn @serverParams
        $exists = $true
    }
    catch{
        if (-not (Test-IDLAdNotFound -Exception $_.Exception)){
            throw
        }
    }

    $script:IDLContainerCache[$Dn] = $exists
    return $exists
}

function Find-IDLUsersByGuid {
    <#
    .SYNOPSIS
        Bulk-fetches users by objectGUID (chunked OR filters). Returns a hashtable keyed by lower-case guid.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][AllowEmptyCollection()][guid[]]$Guids,
        [AllowNull()][AllowEmptyString()][string]$Server,
        [Parameter(Mandatory)][string[]]$Properties
    )

    $found = @{}
    $serverParams = Get-IDLServerParams -Server $Server
    if ($Guids.Count -eq 0){
        return $found
    }

    foreach ($chunk in (Split-IDLChunk -InputArray $Guids -Size 40)){
        $terms = @($chunk | ForEach-Object { '(objectGUID={0})' -f (ConvertTo-IDLLdapGuidValue -Guid $_) })
        $filter = New-IDLOrFilter -Terms $terms
        foreach ($user in @(Get-ADUser -LDAPFilter $filter -Properties $Properties @serverParams)){
            $found[([guid]$user.ObjectGUID).ToString()] = $user
        }
    }

    return $found
}

function Find-IDLUsersByAttribute {
    <#
    .SYNOPSIS
        Bulk-fetches users whose attribute equals any of the values. Returns hashtable value(lower) -> list of users.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Attribute,
        [Parameter(Mandatory)][AllowEmptyCollection()][string[]]$Values,
        [AllowNull()][AllowEmptyString()][string]$Server,
        [Parameter(Mandatory)][string[]]$Properties
    )

    $found = [hashtable]::new([System.StringComparer]::OrdinalIgnoreCase)
    $serverParams = Get-IDLServerParams -Server $Server
    if ($Values.Count -eq 0){
        return $found
    }

    foreach ($chunk in (Split-IDLChunk -InputArray $Values -Size 40)){
        $terms = @($chunk | ForEach-Object { '({0}={1})' -f $Attribute, (ConvertTo-IDLLdapFilterValue -Value $_) })
        $filter = New-IDLOrFilter -Terms $terms
        foreach ($user in @(Get-ADUser -LDAPFilter $filter -Properties $Properties @serverParams)){
            $value = Get-IDLAdValue -Object $user -Name $Attribute
            if ($null -eq $value){
                continue
            }

            if (-not $found.ContainsKey($value)){
                $found[$value] = [System.Collections.Generic.List[object]]::new()
            }

            $found[$value].Add($user)
        }
    }

    return $found
}

function Find-IDLTakenNames {
    <#
    .SYNOPSIS
        Looks up which account names (sAMAccountName, userPrincipalName, mail, cn) are already used in the directory.
    .DESCRIPTION
        Names is a list of entries with Field ('sam' | 'upn' | 'mail' | 'cn'), Value and, for cn, Ou. sam and upn are
        checked on ANY object (users, groups, computers ...) in the domain; mail against the mail attribute and the
        smtp: proxyAddresses of any object (contacts and groups included), case-insensitively; cn one level below Ou.
        Objects whose objectGUID is in Exclude never count as taken. LDAP filters are batched (at most 50 terms per
        query). Returns @{ Taken = [ordered]@{field; value; dn; objectGuid}[] (one entry per name and object, value as
        requested); MissingOus = string[] (cn target OUs that do not exist - nothing was checked there) }.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][AllowEmptyCollection()][object[]]$Names,
        [AllowNull()][AllowEmptyString()][string]$Server,
        [AllowEmptyCollection()][string[]]$Exclude = @()
    )

    $serverParams = Get-IDLServerParams -Server $Server
    $excluded = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::OrdinalIgnoreCase)
    foreach ($text in $Exclude){
        $guid = [guid]::Empty
        if ([guid]::TryParse([string]$text, [ref]$guid)){
            $null = $excluded.Add($guid.ToString())
        }
    }

    $wanted = @{}
    foreach ($field in @('sam', 'upn', 'mail')){
        $wanted[$field] = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::OrdinalIgnoreCase)
    }

    $cnByOu = [ordered]@{}
    foreach ($entry in $Names){
        $field = ([string]$entry.Field).ToLowerInvariant()
        $value = [string]$entry.Value
        if ([string]::IsNullOrWhiteSpace($value)){
            continue
        }

        if ($field -eq 'cn'){
            $ou = [string]$entry.Ou
            $key = $ou.ToLowerInvariant()
            if (-not $cnByOu.Contains($key)){
                $cnByOu[$key] = @{ Ou = $ou; Values = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::OrdinalIgnoreCase) }
            }

            $null = $cnByOu[$key].Values.Add($value)
            continue
        }

        if ($wanted.ContainsKey($field)){
            $null = $wanted[$field].Add($value)
        }
    }

    $taken = [System.Collections.Generic.List[object]]::new()
    $seen = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::OrdinalIgnoreCase)
    $record = {
        param([string]$Field, [string]$Value, $Object)

        $guid = ([guid]$Object.ObjectGUID).ToString()
        if (
            ($excluded.Contains($guid)) -or
            (-not $seen.Add("$Field|$Value|$guid"))
        ){
            return
        }

        $taken.Add([ordered]@{
                field      = $Field
                value      = $Value
                dn         = [string]$Object.DistinguishedName
                objectGuid = $guid
            })
    }

    # sam / upn / mail: directory-wide, any object class.
    $terms = [System.Collections.Generic.List[string]]::new()
    foreach ($value in $wanted['sam']){
        $terms.Add('(sAMAccountName={0})' -f (ConvertTo-IDLLdapFilterValue -Value $value))
    }

    foreach ($value in $wanted['upn']){
        $terms.Add('(userPrincipalName={0})' -f (ConvertTo-IDLLdapFilterValue -Value $value))
    }

    foreach ($value in $wanted['mail']){
        $escaped = ConvertTo-IDLLdapFilterValue -Value $value
        $terms.Add('(mail={0})' -f $escaped)
        $terms.Add('(proxyAddresses=smtp:{0})' -f $escaped)
    }

    if ($terms.Count -gt 0){
        foreach ($chunk in (Split-IDLChunk -InputArray $terms.ToArray() -Size 50)){
            $filter = New-IDLOrFilter -Terms ([string[]]$chunk)
            foreach ($object in @(Get-ADObject -LDAPFilter $filter -Properties 'sAMAccountName', 'userPrincipalName', 'mail', 'proxyAddresses' @serverParams)){
                $sam = Get-IDLAdValue -Object $object -Name 'sAMAccountName'
                if (($sam) -and ($wanted['sam'].Contains($sam))){
                    & $record 'sam' ($wanted['sam'] | Where-Object { $_ -ieq $sam } | Select-Object -First 1) $object
                }

                $upn = Get-IDLAdValue -Object $object -Name 'userPrincipalName'
                if (($upn) -and ($wanted['upn'].Contains($upn))){
                    & $record 'upn' ($wanted['upn'] | Where-Object { $_ -ieq $upn } | Select-Object -First 1) $object
                }

                $addresses = [System.Collections.Generic.List[string]]::new()
                $mail = Get-IDLAdValue -Object $object -Name 'mail'
                if ($mail){
                    $addresses.Add($mail)
                }

                foreach ($proxy in (Get-IDLAdValues -Object $object -Name 'proxyAddresses')){
                    if ($proxy -match '^(?i)smtp:(.+)$'){
                        $addresses.Add($Matches[1])
                    }
                }

                foreach ($address in $addresses){
                    if ($wanted['mail'].Contains($address)){
                        & $record 'mail' ($wanted['mail'] | Where-Object { $_ -ieq $address } | Select-Object -First 1) $object
                    }
                }
            }
        }
    }

    # cn: one level below each target OU.
    $missing = [System.Collections.Generic.List[string]]::new()
    foreach ($key in $cnByOu.Keys){
        $ou = $cnByOu[$key].Ou
        $values = $cnByOu[$key].Values
        if (-not (Test-IDLContainerExists -Dn $ou -Server $Server)){
            $missing.Add($ou)
            continue
        }

        $cnTerms = @($values | ForEach-Object { '(cn={0})' -f (ConvertTo-IDLLdapFilterValue -Value $_) })
        foreach ($chunk in (Split-IDLChunk -InputArray $cnTerms -Size 50)){
            $filter = New-IDLOrFilter -Terms ([string[]]$chunk)
            foreach ($object in @(Get-ADObject -LDAPFilter $filter -SearchBase $ou -SearchScope OneLevel @serverParams)){
                $name = [string]$object.Name
                if ($values.Contains($name)){
                    & $record 'cn' ($values | Where-Object { $_ -ieq $name } | Select-Object -First 1) $object
                }
            }
        }
    }

    return @{
        Taken      = $taken.ToArray()
        MissingOus = [string[]]$missing.ToArray()
    }
}

function Resolve-IDLTargetUsers {
    <#
    .SYNOPSIS
        Resolves every item to its AD account in bulk and verifies the idAttribute equals externalId.
    .DESCRIPTION
        objectGuid is REQUIRED on every item (a valid, non-empty GUID); items without one are Invalid and no lookup
        by idAttribute is ever made (that would resolve accounts directory-wide, outside every managed OU). All GUIDs
        are fetched in chunked bulk queries and the account's idAttribute must equal externalId (else Mismatch).
        Returns one object per item (same order) with Status Found | NotFound | Mismatch | Invalid, plus User and Message.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][AllowEmptyCollection()][object[]]$Items,
        [Parameter(Mandatory)][string]$IdAttribute,
        [AllowNull()][AllowEmptyString()][string]$Server,
        [string[]]$ExtraProperties = @()
    )

    $properties = [string[]]@(@($IdAttribute) + @($ExtraProperties) | Select-Object -Unique)
    $resolved = New-Object 'object[]' $Items.Count
    $guidList = [System.Collections.Generic.List[guid]]::new()
    $meta = New-Object 'object[]' $Items.Count

    for ($i = 0; $i -lt $Items.Count; $i++){
        $item = $Items[$i]
        $externalId = ConvertTo-IDLString -Value (Get-IDLField -Object $item -Name 'externalId')
        $guidText = ConvertTo-IDLString -Value (Get-IDLField -Object $item -Name 'objectGuid')
        $guid = [guid]::Empty

        if ($item -isnot [System.Collections.IDictionary]){
            $resolved[$i] = [pscustomobject]@{ Status = 'Invalid'; User = $null; Message = 'item is not a JSON object' }
            continue
        }

        if ([string]::IsNullOrWhiteSpace($externalId)){
            $resolved[$i] = [pscustomobject]@{ Status = 'Invalid'; User = $null; Message = 'externalId is required' }
            continue
        }

        if (
            ([string]::IsNullOrWhiteSpace($guidText)) -or
            (-not [guid]::TryParse($guidText.Trim(), [ref]$guid)) -or
            ($guid -eq [guid]::Empty)
        ){
            $resolved[$i] = [pscustomobject]@{ Status = 'Invalid'; User = $null; Message = 'objectGuid is required and must be a valid non-empty GUID' }
            continue
        }

        $meta[$i] = @{ ExternalId = $externalId.Trim(); Guid = $guid }
        $guidList.Add($guid)
    }

    $byGuid = Find-IDLUsersByGuid -Guids $guidList.ToArray() -Server $Server -Properties $properties

    for ($i = 0; $i -lt $Items.Count; $i++){
        if ($null -ne $resolved[$i]){
            continue
        }

        $externalId = $meta[$i].ExternalId
        $guid = [guid]$meta[$i].Guid

        $key = $guid.ToString()
        if (-not $byGuid.ContainsKey($key)){
            Add-IDLCounter -Name 'NotFound'
            $resolved[$i] = [pscustomobject]@{ Status = 'NotFound'; User = $null; Message = "no account found with objectGuid $key" }
            continue
        }

        $user = $byGuid[$key]
        $actual = Get-IDLAdValue -Object $user -Name $IdAttribute
        Add-IDLCounter -Name 'Resolved'
        if ($actual -ine $externalId){
            $shown = (($null -eq $actual) ? ('(unset)') : ("'$actual'"))
            $resolved[$i] = [pscustomobject]@{
                Status  = 'Mismatch'
                User    = $user
                Message = "externalId mismatch: account $($user.DistinguishedName) has $IdAttribute $shown but the change expects '$externalId'"
            }
            continue
        }

        $resolved[$i] = [pscustomobject]@{ Status = 'Found'; User = $user; Message = '' }
    }

    return , $resolved
}

function Get-IDLPrivilegedReason {
    <#
    .SYNOPSIS
        Returns why a group object is privileged (adminCount=1 or a well-known privileged RID), or $null.
    #>
    [CmdletBinding()]
    param(
        [AllowNull()]$Group
    )

    if ($null -eq $Group){
        return $null
    }

    $adminCount = Get-IDLAdValue -Object $Group -Name 'adminCount'
    if ($adminCount -eq '1'){
        return 'adminCount=1'
    }

    $sid = Get-IDLAdValue -Object $Group -Name 'objectSid'
    if (-not [string]::IsNullOrEmpty($sid)){
        $rid = ($sid -split '-')[-1]
        if ($rid -in $script:IDLPrivilegedGroupRids){
            return "well-known privileged RID $rid"
        }
    }

    return $null
}

function New-IDLGroupEntry {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Identity,
        [AllowEmptyCollection()][object[]]$Groups = @()
    )

    $group = $null
    if ($Groups.Count -eq 1){
        $group = $Groups[0]
    }

    $reason = Get-IDLPrivilegedReason -Group $group
    return [pscustomobject]@{
        Identity         = $Identity
        Dn               = (($null -ne $group) ? ([string]$group.DistinguishedName) : ($null))
        Found            = ($null -ne $group)
        Ambiguous        = ($Groups.Count -gt 1)
        MatchCount       = $Groups.Count
        Privileged       = ($null -ne $reason)
        PrivilegedReason = $reason
    }
}

function Resolve-IDLGroups {
    <#
    .SYNOPSIS
        Resolves group identities (sAMAccountName or DN) in bulk. Returns hashtable identity -> {Identity,Dn,Found}.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][AllowEmptyCollection()][string[]]$Identities,
        [AllowNull()][AllowEmptyString()][string]$Server
    )

    $map = [hashtable]::new([System.StringComparer]::OrdinalIgnoreCase)
    $serverParams = Get-IDLServerParams -Server $Server
    $unique = @($Identities | Where-Object { -not [string]::IsNullOrWhiteSpace($_) } | ForEach-Object { $_.Trim() } | Select-Object -Unique)

    $sams = @($unique | Where-Object { -not (Test-IDLDistinguishedName -Value $_) })
    $dns = @($unique | Where-Object { Test-IDLDistinguishedName -Value $_ })

    $groupProperties = @('adminCount', 'objectSid')
    $bySam = [hashtable]::new([System.StringComparer]::OrdinalIgnoreCase)
    if ($sams.Count -gt 0){
        foreach ($chunk in (Split-IDLChunk -InputArray $sams -Size 40)){
            $terms = @($chunk | ForEach-Object { '(sAMAccountName={0})' -f (ConvertTo-IDLLdapFilterValue -Value $_) })
            $filter = New-IDLOrFilter -Terms $terms
            foreach ($group in @(Get-ADGroup -LDAPFilter $filter -Properties $groupProperties @serverParams)){
                $key = [string]$group.SamAccountName
                if (-not $bySam.ContainsKey($key)){
                    $bySam[$key] = [System.Collections.Generic.List[object]]::new()
                }

                $bySam[$key].Add($group)
            }
        }
    }

    foreach ($sam in $sams){
        $matched = @()
        if ($bySam.ContainsKey($sam)){
            $matched = @($bySam[$sam])
        }

        $map[$sam] = New-IDLGroupEntry -Identity $sam -Groups $matched
    }

    foreach ($dn in $dns){
        $group = $null
        try{
            $group = Get-ADGroup -Identity $dn -Properties $groupProperties @serverParams
        }
        catch{
            if (-not (Test-IDLAdNotFound -Exception $_.Exception)){
                throw
            }
        }

        $map[$dn] = New-IDLGroupEntry -Identity $dn -Groups @($group | Where-Object { $null -ne $_ })
    }

    foreach ($entry in $map.Values){
        $state = 'NOT FOUND'
        if ($entry.Ambiguous){
            $state = "AMBIGUOUS ($($entry.MatchCount) matches)"
        }
        elseif ($entry.Found){
            $state = "found ($($entry.Dn))"
            if ($entry.Privileged){
                $state += " PRIVILEGED: $($entry.PrivilegedReason)"
            }
        }

        Write-IDLLog ("Group '{0}': {1}" -f $entry.Identity, $state)
    }

    return $map
}

function Update-IDLGroupMembership {
    <#
    .SYNOPSIS
        Adds/removes group memberships for a user (idempotent; honours DryRun). Returns the number of changes made/planned.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]$User,
        [string[]]$AddDns = @(),
        [string[]]$RemoveDns = @(),
        [AllowNull()][AllowEmptyString()][string]$Server,
        [switch]$DryRun,
        [Parameter(Mandatory)][AllowEmptyCollection()][System.Collections.Generic.List[string]]$Done
    )

    $serverParams = Get-IDLServerParams -Server $Server
    $current = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::OrdinalIgnoreCase)
    foreach ($dn in (Get-IDLAdValues -Object $User -Name 'MemberOf')){
        $null = $current.Add($dn)
    }

    $identity = [string]$User.SamAccountName
    $userDn = [string]$User.DistinguishedName
    $changes = 0

    foreach ($groupDn in @($AddDns | Select-Object -Unique)){
        if ($current.Contains($groupDn)){
            Write-IDLLog "  $identity already a member of $groupDn"
            continue
        }

        $changes++
        if ($DryRun){
            Write-IDLLog "  WOULD: add $identity to $groupDn"
            Add-IDLReportRow -Action 'AddGroupMember' -Result 'Would' -Identity $identity -ObjectIdOrDN $userDn -Details $groupDn
            $Done.Add("would add to $groupDn")
            continue
        }

        Add-ADGroupMember -Identity $groupDn -Members $userDn @serverParams
        Write-IDLLog "  Added $identity to $groupDn"
        Add-IDLReportRow -Action 'AddGroupMember' -Result 'Applied' -Identity $identity -ObjectIdOrDN $userDn -Details $groupDn
        $Done.Add("added to $groupDn")
    }

    foreach ($groupDn in @($RemoveDns | Select-Object -Unique)){
        if (-not $current.Contains($groupDn)){
            Write-IDLLog "  $identity is not a member of $groupDn (nothing to remove)"
            continue
        }

        $changes++
        if ($DryRun){
            Write-IDLLog "  WOULD: remove $identity from $groupDn"
            Add-IDLReportRow -Action 'RemoveGroupMember' -Result 'Would' -Identity $identity -ObjectIdOrDN $userDn -Details $groupDn
            $Done.Add("would remove from $groupDn")
            continue
        }

        Remove-ADGroupMember -Identity $groupDn -Members $userDn -Confirm:$false @serverParams
        Write-IDLLog "  Removed $identity from $groupDn"
        Add-IDLReportRow -Action 'RemoveGroupMember' -Result 'Applied' -Identity $identity -ObjectIdOrDN $userDn -Details $groupDn
        $Done.Add("removed from $groupDn")
    }

    return $changes
}

function Move-IDLUser {
    <#
    .SYNOPSIS
        Moves the user to the target OU (validates it exists; no-op if already there). Returns the new DN, or $null if no move was needed.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]$User,
        [Parameter(Mandatory)][string]$TargetOu,
        [AllowNull()][AllowEmptyString()][string]$Server,
        [switch]$DryRun,
        [Parameter(Mandatory)][AllowEmptyCollection()][System.Collections.Generic.List[string]]$Done
    )

    $serverParams = Get-IDLServerParams -Server $Server
    $oldDn = [string]$User.DistinguishedName
    $identity = [string]$User.SamAccountName

    if ((Get-IDLParentDn -Dn $oldDn) -ieq $TargetOu){
        Write-IDLLog "  $identity already in $TargetOu"
        return $null
    }

    if (-not (Test-IDLContainerExists -Dn $TargetOu -Server $Server)){
        throw "Target OU does not exist: $TargetOu"
    }

    $newDn = '{0},{1}' -f (Get-IDLLeafRdn -Dn $oldDn), $TargetOu
    if ($DryRun){
        Write-IDLLog "  WOULD: move $oldDn -> $newDn"
        Add-IDLReportRow -Action 'MoveObject' -Result 'Would' -Identity $identity -ObjectIdOrDN $oldDn -Details "-> $newDn"
        $Done.Add("would move to $TargetOu")
        return $newDn
    }

    Move-ADObject -Identity ([guid]$User.ObjectGUID) -TargetPath $TargetOu @serverParams
    Write-IDLLog "  Moved $oldDn -> $newDn"
    Add-IDLReportRow -Action 'MoveObject' -Result 'Applied' -Identity $identity -ObjectIdOrDN $oldDn -Details "-> $newDn"
    $Done.Add("moved to $TargetOu")
    return $newDn
}

function Get-IDLUserFields {
    <#
    .SYNOPSIS
        Returns a splat hashtable (ObjectGuid, Dn, Sam, Upn) describing an AD user object for New-IDLResult.
    #>
    [CmdletBinding()]
    param(
        [AllowNull()]$User,
        [AllowNull()][string]$DnOverride
    )

    if ($null -eq $User){
        return @{}
    }

    $dn = (([string]::IsNullOrEmpty($DnOverride)) ? ([string]$User.DistinguishedName) : ($DnOverride))
    return @{
        ObjectGuid = ([guid]$User.ObjectGUID).ToString()
        Dn         = $dn
        Sam        = [string]$User.SamAccountName
        Upn        = (Get-IDLAdValue -Object $User -Name 'UserPrincipalName')
    }
}

function Get-IDLProtectedAttributes {
    <#
    .SYNOPSIS
        Returns the lower-case names of attributes that may never be written by the scripts (prefixes: see Test-IDLProtectedAttribute).
    #>
    [CmdletBinding()]
    param()

    return $script:IDLProtectedAttributes
}

function Test-IDLProtectedAttribute {
    <#
    .SYNOPSIS
        True when the attribute is on the protected list or starts with a protected prefix (msDS-*, msExch-*). Case-insensitive.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][AllowEmptyString()][string]$Name
    )

    $lower = $Name.Trim().ToLowerInvariant()
    if ($lower -in $script:IDLProtectedAttributes){
        return $true
    }

    foreach ($prefix in $script:IDLProtectedAttributePrefixes){
        if ($lower.StartsWith($prefix)){
            return $true
        }
    }

    return $false
}

function Test-IDLDnShape {
    <#
    .SYNOPSIS
        True when the text looks like a well-formed distinguished name (RDNs of attr=value, escapes honoured, no control characters).
    #>
    [CmdletBinding()]
    param(
        [AllowNull()][AllowEmptyString()][string]$Value
    )

    if (
        ([string]::IsNullOrWhiteSpace($Value)) -or
        ($Value.Length -gt 2048) -or
        ($Value -cmatch '[\x00-\x1f\x7f]')
    ){
        return $false
    }

    $rdn = '[A-Za-z][A-Za-z0-9\-]*=(?:\\.|[^,\\+=])+'
    return ($Value -match ('^{0}(?:,{0})*$' -f $rdn))
}

function Get-IDLGroupDns {
    <#
    .SYNOPSIS
        Maps group identities to DNs using a Resolve-IDLGroups map. Returns {Dns; Missing; Skipped}.
    .DESCRIPTION
        Throws (so the item fails before any change) when a group is ambiguous or privileged. Missing groups are
        returned in .Missing for the caller to treat as it needs.
        With -SkipPrivileged (used by Disable-IDLUser, which must never be blocked) ambiguous and privileged groups
        are NOT thrown on but returned in .Skipped as {Identity; Kind ('privileged'|'ambiguous'); Reason}; they are
        never part of .Dns, so their membership is left alone.
    #>
    [CmdletBinding()]
    param(
        [AllowNull()][string[]]$Identities,
        [Parameter(Mandatory)]$GroupMap,
        [switch]$SkipPrivileged
    )

    $dns = [System.Collections.Generic.List[string]]::new()
    $missing = [System.Collections.Generic.List[string]]::new()
    $skipped = [System.Collections.Generic.List[object]]::new()

    foreach ($identity in @($Identities)){
        $entry = $GroupMap[$identity]
        if ($entry.Ambiguous){
            $reason = "it matches $($entry.MatchCount) groups"
            if ($SkipPrivileged){
                $skipped.Add([pscustomobject]@{ Identity = $identity; Kind = 'ambiguous'; Reason = $reason })
                continue
            }

            throw "Group '$identity' is ambiguous: $reason; refusing to guess."
        }

        if (-not $entry.Found){
            $missing.Add($identity)
            continue
        }

        if ($entry.Privileged){
            if ($SkipPrivileged){
                $skipped.Add([pscustomobject]@{ Identity = $identity; Kind = 'privileged'; Reason = [string]$entry.PrivilegedReason })
                continue
            }

            throw "Group '$identity' is privileged ($($entry.PrivilegedReason)); refusing to change its membership."
        }

        $dns.Add($entry.Dn)
    }

    return [pscustomobject]@{
        Dns     = [string[]]$dns.ToArray()
        Missing = [string[]]$missing.ToArray()
        Skipped = [object[]]$skipped.ToArray()
    }
}

#region Directory ACL lockdown (used by the installer)

function Get-IDLWellKnownSid {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Sid
    )

    return [System.Security.Principal.SecurityIdentifier]::new($Sid)
}

function ConvertTo-IDLSidSafe {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]$Identity
    )

    try{
        if ($Identity -is [System.Security.Principal.SecurityIdentifier]){
            return $Identity
        }

        if ($Identity -isnot [System.Security.Principal.NTAccount]){
            $Identity = [System.Security.Principal.NTAccount]::new([string]$Identity)
        }

        return $Identity.Translate([System.Security.Principal.SecurityIdentifier])
    }
    catch{
        return $null
    }
}

function Get-IDLDirectoryDacl {
    <#
    .SYNOPSIS
        Reads ONLY the DACL of a directory (Get-Acl/Set-Acl also touch owner/SACL, which needs SeSecurityPrivilege).
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Path
    )

    return [System.IO.FileSystemAclExtensions]::GetAccessControl([System.IO.DirectoryInfo]::new($Path), [System.Security.AccessControl.AccessControlSections]::Access)
}

function Get-IDLTrustedOwnerSids {
    <#
    .SYNOPSIS
        SIDs that may own IDLifecycle folders/files: SYSTEM, Administrators, TrustedInstaller and the current user
        (the same rule as the Go datadir package).
    #>
    [CmdletBinding()]
    param()

    return [string[]]@(
        'S-1-5-18',
        'S-1-5-32-544',
        'S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464',
        [System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value
    )
}

function Get-IDLOwnerSid {
    <#
    .SYNOPSIS
        Owner SID of a file or directory (reads only the owner; needs no privilege).
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Path
    )

    $item = Get-Item -LiteralPath $Path -Force
    $sections = [System.Security.AccessControl.AccessControlSections]::Owner
    if ($item -is [System.IO.DirectoryInfo]){
        $sd = [System.IO.FileSystemAclExtensions]::GetAccessControl([System.IO.DirectoryInfo]$item, $sections)
    }
    else{
        $sd = [System.IO.FileSystemAclExtensions]::GetAccessControl([System.IO.FileInfo]$item, $sections)
    }

    return $sd.GetOwner([System.Security.Principal.SecurityIdentifier]).Value
}

function Test-IDLTrustedOwner {
    <#
    .SYNOPSIS
        True when the path is owned by SYSTEM, Administrators, TrustedInstaller or the current user. An untrusted owner
        can always rewrite the ACL (implicit WRITE_DAC), so such a folder can never count as locked down.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Path
    )

    return ((Get-IDLOwnerSid -Path $Path) -in (Get-IDLTrustedOwnerSids))
}

function Test-IDLProcessElevated {
    [CmdletBinding()]
    param()

    return ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
}

function Set-IDLAdministratorsOwner {
    <#
    .SYNOPSIS
        Makes BUILTIN\Administrators the owner when elevated. Not elevated: keeps a trusted owner, throws for an
        untrusted one (it could rewrite the ACL at any time).
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Path
    )

    $admins = [System.Security.Principal.SecurityIdentifier]::new('S-1-5-32-544')
    if ((Get-IDLOwnerSid -Path $Path) -eq $admins.Value){
        return
    }

    if (Test-IDLProcessElevated){
        $item = Get-Item -LiteralPath $Path -Force
        $sections = [System.Security.AccessControl.AccessControlSections]::Owner
        if ($item -is [System.IO.DirectoryInfo]){
            $sd = [System.IO.FileSystemAclExtensions]::GetAccessControl([System.IO.DirectoryInfo]$item, $sections)
            $sd.SetOwner($admins)
            [System.IO.FileSystemAclExtensions]::SetAccessControl([System.IO.DirectoryInfo]$item, $sd)
        }
        else{
            $sd = [System.IO.FileSystemAclExtensions]::GetAccessControl([System.IO.FileInfo]$item, $sections)
            $sd.SetOwner($admins)
            [System.IO.FileSystemAclExtensions]::SetAccessControl([System.IO.FileInfo]$item, $sd)
        }

        return
    }

    if (-not (Test-IDLTrustedOwner -Path $Path)){
        throw "'$Path' is owned by $(Get-IDLOwnerSid -Path $Path), which is not trusted; take ownership as an administrator (takeown /a /r /f) or delete it."
    }
}

function Test-IDLDirectoryLockedDown {
    <#
    .SYNOPSIS
        True when the directory is owned by a trusted account (SYSTEM, Administrators, TrustedInstaller, current user),
        its ACL is protected (inheritance OFF), has no Users/Authenticated Users/CREATOR OWNER/Everyone ACE, and grants
        SYSTEM + Administrators FullControl and the service account Modify.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Path,
        [Parameter(Mandatory)][string]$ServiceAccount
    )

    if (-not (Test-Path -LiteralPath $Path -PathType Container)){
        return $false
    }

    if (-not (Test-IDLTrustedOwner -Path $Path)){
        return $false
    }

    $acl = Get-IDLDirectoryDacl -Path $Path
    if (-not $acl.AreAccessRulesProtected){
        return $false
    }

    $forbidden = @('S-1-5-32-545', 'S-1-5-11', 'S-1-3-0', 'S-1-1-0')
    $system = 'S-1-5-18'
    $admins = 'S-1-5-32-544'
    $serviceSid = ConvertTo-IDLSidSafe -Identity $ServiceAccount
    $hasSystem = $false
    $hasAdmins = $false
    $hasService = $false
    $serviceIsSystem = (($null -ne $serviceSid) -and ($serviceSid.Value -eq $system))

    foreach ($rule in @($acl.GetAccessRules($true, $true, [System.Security.Principal.SecurityIdentifier]))){
        $sid = ConvertTo-IDLSidSafe -Identity $rule.IdentityReference
        if ($null -eq $sid){
            continue
        }

        if ($sid.Value -in $forbidden){
            return $false
        }

        if ($rule.AccessControlType -ne 'Allow'){
            continue
        }

        $full = (($rule.FileSystemRights -band [System.Security.AccessControl.FileSystemRights]::FullControl) -eq [System.Security.AccessControl.FileSystemRights]::FullControl)
        $modify = (($rule.FileSystemRights -band [System.Security.AccessControl.FileSystemRights]::Modify) -eq [System.Security.AccessControl.FileSystemRights]::Modify)
        if (($sid.Value -eq $system) -and $full){
            $hasSystem = $true
        }

        if (($sid.Value -eq $admins) -and $full){
            $hasAdmins = $true
        }

        if (
            ($rule.IdentityReference.Value -ieq $ServiceAccount) -or
            (($null -ne $serviceSid) -and ($sid.Value -eq $serviceSid.Value))
        ){
            if ($modify){
                $hasService = $true
            }
        }
    }

    return ($hasSystem -and $hasAdmins -and ($hasService -or $serviceIsSystem))
}

function Set-IDLDirectoryLockdown {
    <#
    .SYNOPSIS
        Disables ACL inheritance WITHOUT copying inherited ACEs, clears explicit ACEs, then grants only SYSTEM and
        Administrators FullControl and the service account Modify (all inherited by children).
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Path,
        [Parameter(Mandatory)][string]$ServiceAccount
    )

    $inherit = [System.Security.AccessControl.InheritanceFlags]'ContainerInherit, ObjectInherit'
    $none = [System.Security.AccessControl.PropagationFlags]::None
    $allow = [System.Security.AccessControl.AccessControlType]::Allow

    $serviceSid = ConvertTo-IDLSidSafe -Identity $ServiceAccount
    if ($null -eq $serviceSid){
        throw "Cannot resolve service account '$ServiceAccount' to a SID; refusing to lock down '$Path'."
    }

    # Owner first: an untrusted owner could simply rewrite the ACL afterwards. Elevated sessions hand ownership to
    # Administrators; otherwise a trusted owner (e.g. the current user) is kept and an untrusted one is refused.
    Set-IDLAdministratorsOwner -Path $Path

    $acl = Get-IDLDirectoryDacl -Path $Path
    $acl.SetAccessRuleProtection($true, $false)
    foreach ($rule in @($acl.GetAccessRules($true, $true, [System.Security.Principal.SecurityIdentifier]))){
        $null = $acl.RemoveAccessRuleAll($rule)
    }

    $grants = [ordered]@{
        'S-1-5-18'     = [System.Security.AccessControl.FileSystemRights]::FullControl
        'S-1-5-32-544' = [System.Security.AccessControl.FileSystemRights]::FullControl
    }
    if (-not $grants.Contains($serviceSid.Value)){
        $grants[$serviceSid.Value] = [System.Security.AccessControl.FileSystemRights]::Modify
    }

    foreach ($sidText in $grants.Keys){
        $sid = [System.Security.Principal.SecurityIdentifier]::new($sidText)
        $acl.AddAccessRule([System.Security.AccessControl.FileSystemAccessRule]::new($sid, $grants[$sidText], $inherit, $none, $allow))
    }

    [System.IO.FileSystemAclExtensions]::SetAccessControl([System.IO.DirectoryInfo]::new($Path), $acl)
}

#endregion

#region Setup helpers (Install-IDLifecycle.ps1, Uninstall-IDLifecycle.ps1, New-IDLifecycleRelease.ps1)

$script:IDLFileWriteRights = [int64](2 -bor 4 -bor 16 -bor 64 -bor 256 -bor 65536 -bor 262144 -bor 524288 -bor 0x40000000 -bor 0x10000000)
$script:IDLParentWriteRights = [int64](64 -bor 262144 -bor 524288 -bor 0x10000000)

function Test-IDLPathWithin {
    <#
    .SYNOPSIS
        True when Child equals Parent or lies below it (both normalised, case-insensitive).
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Child,
        [Parameter(Mandatory)][string]$Parent
    )

    $c = [System.IO.Path]::GetFullPath($Child).TrimEnd('\')
    $p = [System.IO.Path]::GetFullPath($Parent).TrimEnd('\')
    return (
        ($c.Equals($p, [System.StringComparison]::OrdinalIgnoreCase)) -or
        ($c.StartsWith($p + '\', [System.StringComparison]::OrdinalIgnoreCase))
    )
}

function Get-IDLUnsafePathReason {
    <#
    .SYNOPSIS
        Path safety guard for folders the setup scripts install to, write logs/backups to or delete. Returns why the
        path is refused, or $null when it is acceptable.
    #>
    [CmdletBinding()]
    param(
        [AllowNull()][AllowEmptyString()][string]$Path
    )

    if ([string]::IsNullOrWhiteSpace($Path)){
        return 'the path is empty'
    }

    if ($Path -match '[\*\?"<>|]'){
        return 'the path contains wildcard or invalid characters'
    }

    if ($Path -notmatch '^[A-Za-z]:[\\/]'){
        return 'the path must be an absolute local path (X:\...)'
    }

    if ($Path.Substring(2).Contains(':')){
        return "the path contains ':' after the drive letter (alternate data streams are not accepted)"
    }

    if ($Path -match '~\d'){
        return '8.3 short names are not accepted; pass the full long path'
    }

    try{
        $full = [System.IO.Path]::GetFullPath($Path).TrimEnd('\')
    }
    catch{
        return "the path is not valid: $($_.Exception.Message)"
    }

    if ($full -match '^[A-Za-z]:$'){
        return 'the path is a drive root'
    }

    $profilesRoot = $null
    if ($env:USERPROFILE){
        $profilesRoot = Split-Path -Parent $env:USERPROFILE
    }

    $candidates = @(
        $env:SystemRoot, $env:windir, $env:ProgramFiles, ${env:ProgramFiles(x86)}, $env:ProgramW6432, $env:ProgramData,
        $env:CommonProgramFiles, ${env:CommonProgramFiles(x86)}, $env:CommonProgramW6432, $env:ALLUSERSPROFILE,
        $env:PUBLIC, $env:USERPROFILE, $env:APPDATA, $env:LOCALAPPDATA, $env:TEMP, $env:TMP, $profilesRoot,
        [Environment]::GetFolderPath('Windows'), [Environment]::GetFolderPath('System'),
        [Environment]::GetFolderPath('ProgramFiles'), [Environment]::GetFolderPath('CommonApplicationData'),
        [Environment]::GetFolderPath('UserProfile'), [Environment]::GetFolderPath('Desktop'),
        [Environment]::GetFolderPath('MyDocuments'),
        'C:\Windows', 'C:\Program Files', 'C:\Program Files (x86)', 'C:\ProgramData', 'C:\Users'
    )
    if ($env:SystemDrive){
        $candidates += (Join-Path -Path ($env:SystemDrive + '\') -ChildPath 'Users')
    }

    $protected = [System.Collections.Generic.List[string]]::new()
    foreach ($candidate in $candidates){
        if ([string]::IsNullOrWhiteSpace($candidate)){
            continue
        }

        try{
            $normal = [System.IO.Path]::GetFullPath($candidate).TrimEnd('\')
        }
        catch{
            continue
        }

        if (-not $protected.Contains($normal)){
            $protected.Add($normal)
        }
    }

    $cmp = [System.StringComparison]::OrdinalIgnoreCase
    foreach ($p in $protected){
        if ($full.Equals($p, $cmp)){
            return "the path is the protected folder '$p'"
        }

        if ($p.StartsWith($full + '\', $cmp)){
            return "the path contains the protected folder '$p'"
        }
    }

    # Folders whose whole subtree is off limits.
    $subtrees = @($env:SystemRoot, [Environment]::GetFolderPath('Windows'), $PSHOME)
    foreach ($base in @($env:ProgramData, 'C:\ProgramData')){
        if ($base){
            $subtrees += (Join-Path -Path $base -ChildPath 'Microsoft')
        }
    }

    foreach ($root in $subtrees){
        if ([string]::IsNullOrWhiteSpace($root)){
            continue
        }

        $r = [System.IO.Path]::GetFullPath($root).TrimEnd('\')
        if (
            ($full.Equals($r, $cmp)) -or
            ($full.StartsWith($r + '\', $cmp))
        ){
            return "the path is inside the protected folder '$r'"
        }
    }

    # System folders at any drive root.
    $firstSegment = ($full.Substring(3) -split '\\')[0]
    if ($firstSegment -in @('$Recycle.Bin', 'System Volume Information', 'Recovery', '$WinREAgent', 'Config.Msi')){
        return "the path is inside the system folder '$($full.Substring(0, 3))$firstSegment'"
    }

    # Nothing directly below the profiles root (C:\Users\<name>, Default, Public, All Users).
    foreach ($root in @($profilesRoot, 'C:\Users')){
        if (
            (-not [string]::IsNullOrWhiteSpace($root)) -and
            ((Split-Path -Parent $full).TrimEnd('\').Equals([System.IO.Path]::GetFullPath($root).TrimEnd('\'), $cmp))
        ){
            return "the path is directly below the profiles folder '$root'"
        }
    }

    # A junction/symbolic link (on the path itself or a parent) could point anywhere.
    $cursor = $full
    while (-not [string]::IsNullOrEmpty($cursor)){
        if (Test-Path -LiteralPath $cursor){
            $item = Get-Item -LiteralPath $cursor -Force
            if (($item.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0){
                return "the path (or its parent '$cursor') is a junction or symbolic link"
            }
        }

        $parent = Split-Path -Parent $cursor
        if (
            ([string]::IsNullOrEmpty($parent)) -or
            ($parent -eq $cursor)
        ){
            break
        }

        $cursor = $parent.TrimEnd('\')
        if ($cursor -match '^[A-Za-z]:$'){
            break
        }
    }

    return $null
}

function ConvertTo-IDLAccountName {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Sid
    )

    try{
        return ([System.Security.Principal.SecurityIdentifier]::new($Sid)).Translate([System.Security.Principal.NTAccount]).Value
    }
    catch{
        return $Sid
    }
}

function Get-IDLUntrustedAclReason {
    <#
    .SYNOPSIS
        Why a file/folder can be tampered with by someone other than SYSTEM, Administrators, TrustedInstaller, the
        current user (or -ExtraTrustedSids), or $null.
    .DESCRIPTION
        -Mode Full (the item itself): an untrusted owner, or an untrusted Allow ACE granting any write-type right
        (write/append data, write attributes, delete, delete child, change permissions, take ownership, generic write/all).
        -Mode Parent (folders ABOVE the item): an untrusted owner, or an untrusted ACE granting delete child, change
        permissions, take ownership or generic all - the rights that let someone delete/rename/replace the item or
        re-ACL the parent. Create-files/folders on a parent is accepted on purpose: C:\ grants Users "create folders",
        which only allows NEW siblings, never changing an existing child.
        Inherit-only ACEs (CREATOR OWNER templates, (IO) entries) do not apply to the item and are ignored; Deny ACEs
        are ignored (conservative).
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Path,
        [ValidateSet('Full', 'Parent')][string]$Mode = 'Full',
        [string]$OwnerSid = '',
        [string[]]$ExtraTrustedSids = @()
    )

    $item = Get-Item -LiteralPath $Path -Force
    $trusted = @(Get-IDLTrustedOwnerSids) + @($ExtraTrustedSids | Where-Object { $_ })
    if (-not $OwnerSid){
        $OwnerSid = Get-IDLOwnerSid -Path $Path
    }

    if ($OwnerSid -notin $trusted){
        return "'$Path' is owned by $(ConvertTo-IDLAccountName -Sid $OwnerSid), which is not SYSTEM, Administrators, TrustedInstaller or you"
    }

    $sections = [System.Security.AccessControl.AccessControlSections]::Access
    if ($item -is [System.IO.DirectoryInfo]){
        $acl = [System.IO.FileSystemAclExtensions]::GetAccessControl([System.IO.DirectoryInfo]$item, $sections)
    }
    else{
        $acl = [System.IO.FileSystemAclExtensions]::GetAccessControl([System.IO.FileInfo]$item, $sections)
    }

    $mask = (($Mode -eq 'Full') ? ($script:IDLFileWriteRights) : ($script:IDLParentWriteRights))
    foreach ($rule in @($acl.GetAccessRules($true, $true, [System.Security.Principal.SecurityIdentifier]))){
        if (
            ($rule.AccessControlType -ne [System.Security.AccessControl.AccessControlType]::Allow) -or
            (($rule.PropagationFlags -band [System.Security.AccessControl.PropagationFlags]::InheritOnly) -ne 0)
        ){
            continue
        }

        $sid = $rule.IdentityReference.Value
        if (
            ($sid -in $trusted) -or
            ($sid -eq 'S-1-3-4')
        ){
            continue
        }

        $granted = ([int64]$rule.FileSystemRights) -band $mask
        if ($granted -ne 0){
            return "'$Path' lets $(ConvertTo-IDLAccountName -Sid $sid) change it ($([System.Security.AccessControl.FileSystemRights]$rule.FileSystemRights))"
        }
    }

    return $null
}

function Get-IDLUntrustedTreeReason {
    <#
    .SYNOPSIS
        Get-IDLUntrustedAclReason over a whole tree: the parent chain (Mode Parent, optional), the folder and every file
        and folder below it (Mode Full). Returns the first reason or $null.
    .PARAMETER OwnerSidResolver
        Scriptblock { param($Path) <owner SID> } (lets the caller's test seam supply owners).
    .PARAMETER ExcludeRelative
        Sub-folders (relative to Path) that are skipped, e.g. scripts\Logs where the service account writes.
    .PARAMETER RefuseReparsePoints
        Junctions/symbolic links below Path are a failure (a release folder never contains any).
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Path,
        [switch]$ParentChain,
        [scriptblock]$OwnerSidResolver = $null,
        [string[]]$ExcludeRelative = @(),
        [switch]$RefuseReparsePoints,
        [string[]]$ExtraTrustedSids = @()
    )

    $root = [System.IO.Path]::GetFullPath($Path).TrimEnd('\')
    $owner = {
        param($p)
        if ($null -ne $OwnerSidResolver){
            return [string](& $OwnerSidResolver $p)
        }

        return (Get-IDLOwnerSid -Path $p)
    }

    if ($ParentChain){
        $cursor = Split-Path -Parent $root
        while (-not [string]::IsNullOrEmpty($cursor)){
            $reason = Get-IDLUntrustedAclReason -Path $cursor -Mode Parent -OwnerSid (& $owner $cursor) -ExtraTrustedSids $ExtraTrustedSids
            if ($reason){
                return "parent folder: $reason"
            }

            $next = Split-Path -Parent $cursor
            if (
                ([string]::IsNullOrEmpty($next)) -or
                ($next -eq $cursor)
            ){
                break
            }

            $cursor = $next
        }
    }

    $reason = Get-IDLUntrustedAclReason -Path $root -Mode Full -OwnerSid (& $owner $root) -ExtraTrustedSids $ExtraTrustedSids
    if ($reason){
        return $reason
    }

    $excluded = @($ExcludeRelative | Where-Object { $_ } | ForEach-Object { Join-Path -Path $root -ChildPath $_ })
    foreach ($entry in @(Get-ChildItem -LiteralPath $root -Recurse -Force -ErrorAction Stop)){
        if (@($excluded | Where-Object { Test-IDLPathWithin -Child $entry.FullName -Parent $_ }).Count -gt 0){
            continue
        }

        if (
            ($RefuseReparsePoints) -and
            (($entry.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0)
        ){
            return "'$($entry.FullName)' is a junction or symbolic link"
        }

        $reason = Get-IDLUntrustedAclReason -Path $entry.FullName -Mode Full -OwnerSid (& $owner $entry.FullName) -ExtraTrustedSids $ExtraTrustedSids
        if ($reason){
            return $reason
        }
    }

    return $null
}

function Read-IDLManifest {
    <#
    .SYNOPSIS
        Reads a SHA256SUMS manifest ("<sha256 hex>  <relative path>" per line, '#' comments allowed) and returns
        @{ Relative; Hash } entries. Throws on a malformed line or an unsafe path (absolute, '..', ':', Logs).
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Path
    )

    $entries = [System.Collections.Generic.List[object]]::new()
    $seen = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::OrdinalIgnoreCase)
    $n = 0
    foreach ($line in [System.IO.File]::ReadAllLines($Path)){
        $n++
        if (
            ([string]::IsNullOrWhiteSpace($line)) -or
            ($line.TrimStart().StartsWith('#'))
        ){
            continue
        }

        $m = [regex]::Match($line, '^(?<hash>[0-9A-Fa-f]{64}) [ *](?<path>.+)$')
        if (-not $m.Success){
            throw "SHA256SUMS line $n is malformed."
        }

        $rel = $m.Groups['path'].Value.Trim().Replace('/', '\')
        if (
            ($rel -match '^[\\]|:') -or
            (@($rel -split '\\') -contains '..') -or
            (@($rel -split '\\') -contains '.') -or
            (('\' + $rel + '\') -match '\\Logs\\')
        ){
            throw "SHA256SUMS line $n has an unsafe path '$rel'."
        }

        if (-not $seen.Add($rel)){
            throw "SHA256SUMS lists '$rel' twice."
        }

        $entries.Add(@{ Relative = $rel; Hash = $m.Groups['hash'].Value.ToUpperInvariant() })
    }

    return , $entries.ToArray()
}

function New-IDLManifestText {
    <#
    .SYNOPSIS
        Builds SHA256SUMS text for the given files (paths relative to Root).
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Root,
        [Parameter(Mandatory)][string[]]$Relative
    )

    $lines = foreach ($rel in ($Relative | Sort-Object)){
        '{0}  {1}' -f (Get-FileHash -LiteralPath (Join-Path -Path $Root -ChildPath $rel) -Algorithm SHA256).Hash.ToLowerInvariant(), $rel.Replace('\', '/')
    }

    return (($lines -join "`n") + "`n")
}

function Copy-IDLReleaseFiles {
    <#
    .SYNOPSIS
        Completes a release folder whose idlifecycled.exe is already in place: copies <SourceRoot>\scripts (every file,
        Logs folders excluded) to <OutDir>\scripts and <SourceRoot>\Setup.cmd (when present) to <OutDir>, then writes
        <OutDir>\SHA256SUMS. Returns the number of files in the manifest. Used by New-IDLifecycleRelease.ps1 and by
        Install-IDLifecycle.ps1 when it builds from source.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$SourceRoot,
        [Parameter(Mandatory)][string]$OutDir
    )

    $scriptsSource = Join-Path -Path $SourceRoot -ChildPath 'scripts'
    if (-not (Test-Path -LiteralPath $scriptsSource -PathType Container)){
        throw "'$SourceRoot' has no scripts folder."
    }

    if (-not (Test-Path -LiteralPath (Join-Path -Path $OutDir -ChildPath 'idlifecycled.exe') -PathType Leaf)){
        throw "'$OutDir' has no idlifecycled.exe."
    }

    $relative = [System.Collections.Generic.List[string]]::new()
    $relative.Add('idlifecycled.exe')
    foreach ($file in @(Get-ChildItem -LiteralPath $scriptsSource -Recurse -File -Force)){
        $rel = $file.FullName.Substring($scriptsSource.Length).TrimStart('\')
        if (('\' + $rel) -match '(^|\\)Logs\\'){
            continue
        }

        $target = Join-Path -Path (Join-Path -Path $OutDir -ChildPath 'scripts') -ChildPath $rel
        $parent = Split-Path -Parent $target
        if (-not (Test-Path -LiteralPath $parent -PathType Container)){
            $null = New-Item -Path $parent -ItemType Directory -Force
        }

        Copy-Item -LiteralPath $file.FullName -Destination $target
        $relative.Add((Join-Path -Path 'scripts' -ChildPath $rel))
    }

    # Double-click launcher for the setup window (scripts\Start-IDLifecycleSetup.ps1).
    $launcher = Join-Path -Path $SourceRoot -ChildPath 'Setup.cmd'
    if (Test-Path -LiteralPath $launcher -PathType Leaf){
        Copy-Item -LiteralPath $launcher -Destination (Join-Path -Path $OutDir -ChildPath 'Setup.cmd')
        $relative.Add('Setup.cmd')
    }

    [System.IO.File]::WriteAllText((Join-Path -Path $OutDir -ChildPath 'SHA256SUMS'), (New-IDLManifestText -Root $OutDir -Relative $relative.ToArray()), [System.Text.UTF8Encoding]::new($false))
    return $relative.Count
}

function Expand-IDLZipSafe {
    <#
    .SYNOPSIS
        Extracts a zip into an EMPTY destination folder, refusing any entry that would land outside it (absolute
        paths, '..', drive letters, alternate data streams) and archives over -MaxBytes uncompressed (default 2 GB).
        Returns the folder that holds the content: the destination, or its only sub-folder when the archive wraps
        everything in one top folder (as GitHub's source zips and zipped release folders do).
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$ZipPath,
        [Parameter(Mandatory)][string]$Destination,
        [int64]$MaxBytes = 2GB
    )

    Add-Type -AssemblyName System.IO.Compression, System.IO.Compression.FileSystem
    $dest = [System.IO.Path]::GetFullPath($Destination).TrimEnd('\')
    if (
        (Test-Path -LiteralPath $dest) -and
        (@(Get-ChildItem -LiteralPath $dest -Force).Count -gt 0)
    ){
        throw (New-IDLException -Kind Input -Message "'$dest' is not empty.")
    }

    $null = New-Item -Path $dest -ItemType Directory -Force
    $zip = [System.IO.Compression.ZipFile]::OpenRead($ZipPath)
    try{
        $total = [int64]0
        foreach ($entry in $zip.Entries){
            $total += $entry.Length
            if ($total -gt $MaxBytes){
                throw (New-IDLException -Kind Input -Message "'$ZipPath' expands to more than $([int]($MaxBytes / 1MB)) MB; refusing.")
            }

            $name = $entry.FullName.Replace('/', '\')
            if (
                ($name -match '(^|\\)\.\.(\\|$)') -or
                ($name -match ':') -or
                ([System.IO.Path]::IsPathRooted($name))
            ){
                throw (New-IDLException -Kind Input -Message "'$ZipPath' contains an unsafe entry '$($entry.FullName)'; refusing.")
            }

            $target = [System.IO.Path]::GetFullPath((Join-Path -Path $dest -ChildPath $name))
            if (-not (Test-IDLPathWithin -Child $target -Parent $dest)){
                throw (New-IDLException -Kind Input -Message "'$ZipPath' entry '$($entry.FullName)' points outside the destination; refusing.")
            }

            if ($name.EndsWith('\')){
                $null = New-Item -Path $target -ItemType Directory -Force
                continue
            }

            $parent = Split-Path -Parent $target
            if (-not (Test-Path -LiteralPath $parent -PathType Container)){
                $null = New-Item -Path $parent -ItemType Directory -Force
            }

            [System.IO.Compression.ZipFileExtensions]::ExtractToFile($entry, $target, $false)
        }
    }
    finally{
        $zip.Dispose()
    }

    $top = @(Get-ChildItem -LiteralPath $dest -Force)
    if (
        ($top.Count -eq 1) -and
        ($top[0] -is [System.IO.DirectoryInfo])
    ){
        return $top[0].FullName
    }

    return $dest
}

function Register-IDLDsnSecrets {
    <#
    .SYNOPSIS
        Registers a connection string AND its password component(s) as secrets, so neither the whole DSN nor a
        fragment of it can reach a log line, report cell or error text.
    #>
    [CmdletBinding()]
    param(
        [AllowNull()][AllowEmptyString()][string]$Dsn
    )

    if (
        ([string]::IsNullOrEmpty($Dsn)) -or
        ($Dsn -match '^env:[A-Za-z_][A-Za-z0-9_]*$')
    ){
        return
    }

    Register-IDLSecret -Secret $Dsn
    $patterns = @(
        '(?i)(?:^|[;\s])(?:password|pwd)\s*=\s*(?<v>\{[^}]*\}|[^;]+)',
        '^[a-zA-Z][a-zA-Z0-9+.\-]*://[^:/@]*:(?<v>.*)@[^@]*$',
        '^[^:@/]*:(?<v>.*)@(?:tcp|unix)\('
    )
    foreach ($pattern in $patterns){
        foreach ($m in [regex]::Matches($Dsn, $pattern)){
            $value = $m.Groups['v'].Value
            Register-IDLSecret -Secret $value
            Register-IDLSecret -Secret ([uri]::UnescapeDataString($value))
            Register-IDLSecret -Secret $value.Trim('{', '}')
        }
    }
}

function Get-IDLDsnTarget {
    <#
    .SYNOPSIS
        "database X on host Y" from a connection string, never including user names or passwords: URL userinfo is
        cut at the LAST '@' (so a '/' or '@' inside a password cannot leak), key/value DSNs only use database/host keys.
    #>
    [CmdletBinding()]
    param(
        [AllowNull()][AllowEmptyString()][string]$Dsn
    )

    if ([string]::IsNullOrWhiteSpace($Dsn)){
        return '(unknown database)'
    }

    $db = ''
    $server = ''
    $url = [regex]::Match($Dsn, '^[a-zA-Z][a-zA-Z0-9+.\-]*://(?<rest>.*)$')
    if ($url.Success){
        $rest = $url.Groups['rest'].Value
        $at = $rest.LastIndexOf('@')
        if ($at -ge 0){
            $rest = $rest.Substring($at + 1)
        }

        $hm = [regex]::Match($rest, '^(?<host>[^/?;]*)(?:/(?<db>[^?;]*))?(?:\?(?<q>.*))?$')
        if ($hm.Success){
            $server = $hm.Groups['host'].Value
            $db = $hm.Groups['db'].Value
            $q = [regex]::Match($hm.Groups['q'].Value, '(?i)(?:^|&)database=(?<v>[^&]+)')
            if ($q.Success){
                $db = $q.Groups['v'].Value
            }
        }
    }
    else{
        $at = $Dsn.LastIndexOf('@')
        $mysql = [regex]::Match((($at -ge 0) ? ($Dsn.Substring($at)) : ('')), '^@(?:tcp|unix)\((?<host>[^)]*)\)/(?<db>[^?]*)')
        if ($mysql.Success){
            $server = $mysql.Groups['host'].Value
            $db = $mysql.Groups['db'].Value
        }
        else{
            foreach ($key in @('database', 'dbname', 'initial catalog')){
                $m = [regex]::Match($Dsn, "(?i)(?:^|[;\s])$([regex]::Escape($key))\s*=\s*(?<v>[^;\s]+)")
                if ($m.Success){
                    $db = $m.Groups['v'].Value
                }
            }

            foreach ($key in @('hostname', 'host', 'server', 'data source')){
                $m = [regex]::Match($Dsn, "(?i)(?:^|[;\s])$([regex]::Escape($key))\s*=\s*(?<v>[^;\s]+)")
                if ($m.Success){
                    $server = $m.Groups['v'].Value
                    break
                }
            }
        }
    }

    return ("database '{0}' on '{1}'" -f (($db) ? ($db) : ('?')), (($server) ? ($server) : ('?')))
}

function Initialize-IDLProtectedDir {
    <#
    .SYNOPSIS
        Creates a folder for setup logs/backups and makes sure nobody else can tamper with it. Refuses an existing
        folder that is a junction/symbolic link or has an untrusted owner. With -Lock: owner Administrators (when
        elevated), inheritance off, SYSTEM + Administrators FullControl (+ the current user with -IncludeCurrentUser).
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Path,
        [switch]$Lock,
        [switch]$IncludeCurrentUser
    )

    $reason = Get-IDLUnsafePathReason -Path $Path
    if ($reason){
        throw "Folder '$Path' refused: $reason."
    }

    if (Test-Path -LiteralPath $Path){
        if (-not (Test-Path -LiteralPath $Path -PathType Container)){
            throw "'$Path' exists and is not a folder."
        }

        if (-not (Test-IDLTrustedOwner -Path $Path)){
            throw "'$Path' already exists and is owned by $(ConvertTo-IDLAccountName -Sid (Get-IDLOwnerSid -Path $Path)); delete it if you did not create it."
        }
    }
    else{
        $null = New-Item -Path $Path -ItemType Directory -Force
    }

    if (-not $Lock){
        return
    }

    Set-IDLAdministratorsOwner -Path $Path
    $inherit = [System.Security.AccessControl.InheritanceFlags]'ContainerInherit, ObjectInherit'
    $acl = Get-IDLDirectoryDacl -Path $Path
    $acl.SetAccessRuleProtection($true, $false)
    foreach ($rule in @($acl.GetAccessRules($true, $true, [System.Security.Principal.SecurityIdentifier]))){
        $null = $acl.RemoveAccessRuleAll($rule)
    }

    $sids = @('S-1-5-18', 'S-1-5-32-544')
    if ($IncludeCurrentUser){
        $sids += [System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value
    }

    foreach ($sid in $sids){
        $acl.AddAccessRule([System.Security.AccessControl.FileSystemAccessRule]::new(
                [System.Security.Principal.SecurityIdentifier]::new($sid),
                [System.Security.AccessControl.FileSystemRights]::FullControl,
                $inherit, [System.Security.AccessControl.PropagationFlags]::None,
                [System.Security.AccessControl.AccessControlType]::Allow))
    }

    [System.IO.FileSystemAclExtensions]::SetAccessControl([System.IO.DirectoryInfo]::new($Path), $acl)
}

function Get-IDLTestHookRefusal {
    <#
    .SYNOPSIS
        Why a setup script's -TestHook must NOT be loaded, or $null. Allowed only when IDL_SETUP_TEST=1, the hook is a
        plain file inside <repo>\tests\powershell (relative to the script folder, no junction/symlink anywhere on its
        path) and the script does not run from InstallDir or a Program Files folder.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$HookPath,
        [Parameter(Mandatory)][string]$ScriptRoot,
        [AllowEmptyString()][string]$InstallDir = ''
    )

    if ($env:IDL_SETUP_TEST -ne '1'){
        return 'IDL_SETUP_TEST is not set to 1'
    }

    $testsDir = [System.IO.Path]::GetFullPath((Join-Path -Path $ScriptRoot -ChildPath '..\tests\powershell')).TrimEnd('\')
    if (-not (Test-Path -LiteralPath $testsDir -PathType Container)){
        return "there is no tests folder next to this script ($testsDir)"
    }

    try{
        $hook = [System.IO.Path]::GetFullPath($HookPath)
    }
    catch{
        return 'the hook path is not valid'
    }

    if (
        (-not (Test-Path -LiteralPath $hook -PathType Leaf)) -or
        (-not (Test-IDLPathWithin -Child $hook -Parent $testsDir))
    ){
        return "the hook must be a file inside $testsDir"
    }

    $cursor = $hook
    while (-not [string]::IsNullOrEmpty($cursor)){
        if ((([System.IO.File]::GetAttributes($cursor)) -band [System.IO.FileAttributes]::ReparsePoint) -ne 0){
            return "'$cursor' is a junction or symbolic link"
        }

        $next = Split-Path -Parent $cursor
        if (
            ([string]::IsNullOrEmpty($next)) -or
            ($next -eq $cursor)
        ){
            break
        }

        $cursor = $next
    }

    # An InstallDir the path guard refuses (e.g. C:\) is rejected by the script's own validation right after.
    $forbidden = @($env:ProgramFiles, ${env:ProgramFiles(x86)}, $env:ProgramW6432) | Where-Object { -not [string]::IsNullOrWhiteSpace($_) }
    if (
        (-not [string]::IsNullOrWhiteSpace($InstallDir)) -and
        ($null -eq (Get-IDLUnsafePathReason -Path $InstallDir))
    ){
        $forbidden = @($InstallDir) + @($forbidden)
    }

    foreach ($dir in $forbidden){
        if (Test-IDLPathWithin -Child $ScriptRoot -Parent $dir){
            return "the script runs from '$dir'"
        }
    }

    return $null
}

#endregion

#region Mark-of-the-Web (used by the installer)

function Get-IDLBlockedFiles {
    <#
    .SYNOPSIS
        Returns the full paths of .ps1/.psm1/.psd1 files under the directory (recursive, Logs folders skipped) that
        carry a Zone.Identifier stream (Mark-of-the-Web). Under RemoteSigned such files refuse to load.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Path
    )

    if (-not (Test-Path -LiteralPath $Path -PathType Container)){
        return [string[]]@()
    }

    $blocked = [System.Collections.Generic.List[string]]::new()
    $root = (Get-Item -LiteralPath $Path).FullName.TrimEnd('\')
    $files = @(Get-ChildItem -LiteralPath $root -Recurse -File -Include '*.ps1', '*.psm1', '*.psd1' -ErrorAction SilentlyContinue |
            Where-Object { $_.FullName.Substring($root.Length) -notmatch '\\Logs\\' })
    foreach ($file in $files){
        if ($null -ne (Get-Item -LiteralPath $file.FullName -Stream 'Zone.Identifier' -ErrorAction SilentlyContinue)){
            $blocked.Add($file.FullName)
        }
    }

    return [string[]]$blocked.ToArray()
}

function Unblock-IDLScriptFiles {
    <#
    .SYNOPSIS
        Runs Unblock-File on every file Get-IDLBlockedFiles reports under the directory (logging each one) and
        returns the paths that are STILL blocked afterwards (empty when everything worked).
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Path
    )

    foreach ($file in (Get-IDLBlockedFiles -Path $Path)){
        try{
            Unblock-File -LiteralPath $file
            Write-IDLLog "  Unblocked $file"
        }
        catch{
            Write-IDLLog "  Could not unblock ${file}: $($_.Exception.Message)" 'WARN'
        }
    }

    return [string[]]@(Get-IDLBlockedFiles -Path $Path)
}

#endregion

#region Listen address

function Split-IDLListen {
    # 'host:port' / '[v6]:port' / ':port' -> @{ Host; Port } or $null.
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Value
    )

    $m = [regex]::Match($Value, '^(?:\[(?<h>[^\]]+)\]|(?<h>[^:\[\]]*)):(?<p>\d{1,5})$')
    if (-not $m.Success){
        return $null
    }

    $port = [int]$m.Groups['p'].Value
    if (
        ($port -lt 1) -or
        ($port -gt 65535)
    ){
        return $null
    }

    return @{ Host = $m.Groups['h'].Value; Port = $port }
}

function Test-IDLLoopbackHost {
    [CmdletBinding()]
    param(
        [AllowEmptyString()][string]$HostName
    )

    if ($HostName -ieq 'localhost'){
        return $true
    }

    $ip = $null
    if ([System.Net.IPAddress]::TryParse($HostName, [ref]$ip)){
        return [System.Net.IPAddress]::IsLoopback($ip)
    }

    return $false
}

#endregion

#region Service account

function Get-IDLServiceAccountKind {
    <#
    .SYNOPSIS
        Classifies a service log-on account name:
          BuiltIn  - LocalSystem, NT AUTHORITY\LocalService / NetworkService / SYSTEM, NT SERVICE\<name> (no password)
          Managed  - an MSA or gMSA: DOMAIN\name$ or name$@domain (Windows retrieves the password)
          Password - a normal domain or local user: DOMAIN\name, .\name or name@domain (needs its password)
        Returns '' for anything else.
    #>
    [CmdletBinding()]
    param(
        [AllowEmptyString()][string]$Account
    )

    $builtIn = @('LocalSystem', 'NT AUTHORITY\LocalService', 'NT AUTHORITY\NetworkService', 'NT AUTHORITY\SYSTEM')
    if (
        ($Account -in $builtIn) -or
        ($Account -match '^NT SERVICE\\[A-Za-z0-9_.\-]+$')
    ){
        return 'BuiltIn'
    }

    if (
        ($Account -match '^[^\\@\s]+\\[^\\@\s]+\$$') -or
        ($Account -match '^[^\\@\s]+\$@[^\\@\s]+$')
    ){
        return 'Managed'
    }

    if (
        ($Account -match '^[^\\@\s]+\\[^\\@\s]+$') -or
        ($Account -match '^[^\\@\s]+@[^\\@\s]+$')
    ){
        return 'Password'
    }

    return ''
}

function ConvertTo-IDLServiceAccountName {
    # '.\name' (a local account) -> '<COMPUTERNAME>\name': name-to-SID lookups (NTAccount, LookupAccountName) do not
    # understand '.'. Everything else is returned trimmed and unchanged.
    [CmdletBinding()]
    param(
        [AllowEmptyString()][string]$Account
    )

    $Account = $Account.Trim()
    if ($Account -match '^\.\\(.+)$'){
        return ('{0}\{1}' -f $env:COMPUTERNAME, $Matches[1])
    }

    return $Account
}

$script:LsaRightsSource = @'
using System;
using System.ComponentModel;
using System.Runtime.InteropServices;
using System.Security.Principal;

namespace IDLifecycle {
    public static class LsaRights {
        [StructLayout(LayoutKind.Sequential)]
        struct LSA_UNICODE_STRING { public ushort Length; public ushort MaximumLength; public IntPtr Buffer; }

        [StructLayout(LayoutKind.Sequential)]
        struct LSA_OBJECT_ATTRIBUTES {
            public int Length; public IntPtr RootDirectory; public IntPtr ObjectName; public uint Attributes;
            public IntPtr SecurityDescriptor; public IntPtr SecurityQualityOfService;
        }

        [DllImport("advapi32.dll")] static extern uint LsaOpenPolicy(IntPtr systemName, ref LSA_OBJECT_ATTRIBUTES attributes, uint access, out IntPtr handle);
        [DllImport("advapi32.dll")] static extern uint LsaEnumerateAccountRights(IntPtr handle, byte[] sid, out IntPtr rights, out uint count);
        [DllImport("advapi32.dll")] static extern uint LsaAddAccountRights(IntPtr handle, byte[] sid, LSA_UNICODE_STRING[] rights, uint count);
        [DllImport("advapi32.dll")] static extern uint LsaFreeMemory(IntPtr buffer);
        [DllImport("advapi32.dll")] static extern uint LsaClose(IntPtr handle);
        [DllImport("advapi32.dll")] static extern int LsaNtStatusToWinError(uint status);

        const uint POLICY_CREATE_ACCOUNT = 0x10;
        const uint POLICY_LOOKUP_NAMES = 0x800;
        const uint STATUS_OBJECT_NAME_NOT_FOUND = 0xC0000034;

        static IntPtr Open(uint access) {
            var oa = new LSA_OBJECT_ATTRIBUTES();
            oa.Length = Marshal.SizeOf(oa);
            IntPtr h;
            uint st = LsaOpenPolicy(IntPtr.Zero, ref oa, access, out h);
            if (st != 0) throw new Win32Exception(LsaNtStatusToWinError(st));
            return h;
        }

        static byte[] SidBytes(string sid) {
            var s = new SecurityIdentifier(sid);
            var b = new byte[s.BinaryLength];
            s.GetBinaryForm(b, 0);
            return b;
        }

        // Rights granted directly to the SID (not through a group).
        public static string[] Get(string sid) {
            IntPtr h = Open(POLICY_LOOKUP_NAMES);
            try {
                IntPtr p; uint n;
                uint st = LsaEnumerateAccountRights(h, SidBytes(sid), out p, out n);
                if (st == STATUS_OBJECT_NAME_NOT_FOUND) return new string[0];
                if (st != 0) throw new Win32Exception(LsaNtStatusToWinError(st));
                try {
                    var result = new string[n];
                    int size = Marshal.SizeOf(typeof(LSA_UNICODE_STRING));
                    for (int i = 0; i < n; i++) {
                        var u = (LSA_UNICODE_STRING)Marshal.PtrToStructure(p + i * size, typeof(LSA_UNICODE_STRING));
                        result[i] = Marshal.PtrToStringUni(u.Buffer, u.Length / 2);
                    }
                    return result;
                } finally { LsaFreeMemory(p); }
            } finally { LsaClose(h); }
        }

        public static void Add(string sid, string right) {
            IntPtr h = Open(POLICY_LOOKUP_NAMES | POLICY_CREATE_ACCOUNT);
            try {
                var u = new LSA_UNICODE_STRING();
                u.Buffer = Marshal.StringToHGlobalUni(right);
                u.Length = (ushort)(right.Length * 2);
                u.MaximumLength = (ushort)(u.Length + 2);
                try {
                    uint st = LsaAddAccountRights(h, SidBytes(sid), new[] { u }, 1);
                    if (st != 0) throw new Win32Exception(LsaNtStatusToWinError(st));
                } finally { Marshal.FreeHGlobal(u.Buffer); }
            } finally { LsaClose(h); }
        }
    }
}
'@

function Initialize-IDLLsaRights {
    if (-not ('IDLifecycle.LsaRights' -as [type])){
        Add-Type -TypeDefinition $script:LsaRightsSource -Language CSharp
    }
}

function Test-IDLServiceLogonRight {
    <#
    .SYNOPSIS
        True when the account holds SeServiceLogonRight ("Log on as a service") directly in this computer's local
        security policy. False when it does not, or when the account cannot be resolved or the policy cannot be read.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Account
    )

    $sid = ConvertTo-IDLSidSafe -Identity $Account
    if ($null -eq $sid){
        return $false
    }

    try{
        Initialize-IDLLsaRights
        return (@([IDLifecycle.LsaRights]::Get($sid.Value)) -contains 'SeServiceLogonRight')
    }
    catch{
        Write-IDLLog "Could not read the user rights of '$Account': $($_.Exception.Message)" 'WARN'
        return $false
    }
}

function Grant-IDLServiceLogonRight {
    # Adds SeServiceLogonRight for the account to the local security policy (elevation required). A domain GPO
    # that defines "Log on as a service" replaces the local list at its next refresh.
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Account
    )

    $sid = ConvertTo-IDLSidSafe -Identity $Account
    if ($null -eq $sid){
        throw "Cannot resolve service account '$Account' to a SID."
    }

    Initialize-IDLLsaRights
    [IDLifecycle.LsaRights]::Add($sid.Value, 'SeServiceLogonRight')
}

#endregion

#endregion

Export-ModuleMember -Function @(
    'New-IDLException', 'Get-IDLExitCode', 'Register-IDLSecret', 'Protect-IDLText',
    'Write-IDLLog', 'Start-IDLRun', 'Stop-IDLRun', 'Add-IDLCounter', 'Add-IDLSkipReason', 'Add-IDLReportRow',
    'Start-IDLPhase', 'Stop-IDLPhase',
    'Read-IDLRequest', 'Write-IDLResponse', 'New-IDLErrorResponse', 'Get-IDLField', 'ConvertTo-IDLString',
    'Get-IDLRequestCommon', 'Test-IDLAttributeName', 'Get-IDLStringList',
    'New-IDLResult', 'Register-IDLResult', 'Get-IDLResultSummary', 'New-IDLActionResponse',
    'ConvertTo-IDLLdapFilterValue', 'ConvertTo-IDLLdapGuidValue', 'ConvertTo-IDLDnEscapedValue',
    'Get-IDLServerParams', 'Get-IDLParentDn', 'Get-IDLLeafRdn', 'Test-IDLDistinguishedName',
    'Get-IDLAdValue', 'Get-IDLAdValues', 'Split-IDLChunk', 'New-IDLOrFilter', 'Test-IDLAdNotFound',
    'Test-IDLAdConnection', 'Test-IDLContainerExists', 'Find-IDLUsersByGuid', 'Find-IDLUsersByAttribute', 'Find-IDLTakenNames',
    'Resolve-IDLTargetUsers', 'Resolve-IDLGroups', 'Update-IDLGroupMembership', 'Move-IDLUser',
    'Get-IDLProtectedAttributes', 'Get-IDLUserFields', 'Test-IDLProtectedAttribute', 'Test-IDLDnShape', 'Get-IDLGroupDns',
    'Get-IDLPrivilegedReason', 'Test-IDLDirectoryLockedDown', 'Set-IDLDirectoryLockdown', 'Get-IDLDirectoryDacl',
    'Get-IDLTrustedOwnerSids', 'Get-IDLOwnerSid', 'Test-IDLTrustedOwner', 'Test-IDLProcessElevated', 'Set-IDLAdministratorsOwner',
    'Test-IDLPathWithin', 'Get-IDLUnsafePathReason', 'ConvertTo-IDLAccountName', 'Get-IDLUntrustedAclReason', 'Get-IDLUntrustedTreeReason',
    'Read-IDLManifest', 'New-IDLManifestText', 'Copy-IDLReleaseFiles', 'Expand-IDLZipSafe', 'Register-IDLDsnSecrets', 'Get-IDLDsnTarget', 'Initialize-IDLProtectedDir',
    'Get-IDLTestHookRefusal',
    'Get-IDLBlockedFiles', 'Unblock-IDLScriptFiles',
    'Split-IDLListen', 'Test-IDLLoopbackHost',
    'ConvertTo-IDLSidSafe', 'Get-IDLServiceAccountKind', 'ConvertTo-IDLServiceAccountName', 'Test-IDLServiceLogonRight', 'Grant-IDLServiceLogonRight'
)
