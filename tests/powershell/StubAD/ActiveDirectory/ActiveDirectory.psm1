<#
.SYNOPSIS
    FAKE 'ActiveDirectory' module (version 99.0.0) used ONLY by the IDLifecycle Pester tests.

.DESCRIPTION
    Implements just the cmdlets/parameters the IDLifecycle scripts use, against a JSON state file whose path is
    in the environment variable IDL_STUB_AD_STATE. Every cmdlet reads the file, does its work and (for mutating
    cmdlets) writes it back, because each script invocation is a separate process.

    SAFETY: this module throws on import when IDL_STUB_AD_STATE is not set. Tests always run the scripts in a
    child pwsh whose PSModulePath contains ONLY the directory holding this module, so the real domain can never
    be reached.

    State file schema (all property names are case-insensitive on read):
    {
      "users": [ { "objectGuid": "guid", "dn": "CN=Jane Doe,OU=Staff,DC=stub,DC=test", "sam": "jdoe",
                   "upn": "jdoe@stub.test", "enabled": true, "objectClass": "user",
                   "attributes": { "employeeID": "E1001", "givenName": "Jane", "title": "Analyst" },
                   "memberOf": [ "CN=GG-AllStaff,OU=Groups,DC=stub,DC=test" ] } ],
      "groups": [ { "dn": "CN=GG-AllStaff,OU=Groups,DC=stub,DC=test", "sam": "GG-AllStaff",
                    "adminCount": 1, "objectSid": "S-1-5-21-1-2-3-512" } ],
      "ous":    [ "OU=Staff,DC=stub,DC=test", "OU=Groups,DC=stub,DC=test" ],
      "objects": [ { "dn": "CN=ExchangeActiveSyncDevices,CN=Jane Doe,OU=Staff,DC=stub,DC=test", "objectClass": "msExchActiveSyncDevices" } ],
      "calls":  [ { "cmdlet": "New-ADUser", "args": { "Name": "Jane Doe", "AccountPassword": "***" }, "result": "ok" } ],
      "faults": [ { "cmdlet": "Add-ADGroupMember", "match": "jdoe", "message": "boom {password}" } ],
      "failConnect": false,
      "schema": { "classes":    [ { "lDAPDisplayName": "user", "subClassOf": "organizationalPerson", "mayContain": ["employeeID"] } ],
                  "attributes": [ { "lDAPDisplayName": "employeeID", "attributeSyntax": "2.5.5.12", "isSingleValued": true } ] }
    }

      * users[].objectClass defaults to "user" (use "computer" to test the not-a-user refusal).
      * users[].objectSid (optional, "S-1-5-21-...") is returned as the SID property; without it a stable SID is derived from the objectGUID.
      * groups[] and objects[] may carry an "attributes" table too (e.g. a contact with mail / proxyAddresses);
        multi-valued attributes are JSON arrays and match filters value by value, case-insensitively.
      * Set-ADUser supports -Replace/-Clear/-Add/-Remove (applied Remove, Add, Replace, Clear like the real cmdlet; adding
        an existing or removing a missing value throws) and -SamAccountName/-UserPrincipalName (uniqueness enforced)/
        -EmailAddress. Rename-ADObject changes the RDN value (children move along, the DN must stay unique).
      * groups[].adminCount / objectSid (both optional) are returned by Get-ADGroup -Properties adminCount,objectSid.
      * faults[] with cmdlet "Get-ADObject" also make READS throw when "match" is found in the Identity/filter/
        search base (reads are not recorded in calls).
      * objects[] are generic child objects; one whose DN ends with ",<user DN>" makes that user a non-leaf:
        Remove-ADUser and Remove-ADObject without -Recursive throw the leaf-object error, -Recursive deletes the subtree.
      * calls[] is appended to by every MUTATING cmdlet (New/Set/Remove-ADUser, Enable/Disable-ADAccount,
        Move-ADObject, Rename-ADObject, Remove-ADObject, Add/Remove-ADGroupMember), including attempts that fail (result starts with "error").
        Passwords are never recorded (AccountPassword is "***").
      * faults[] makes a matching mutating cmdlet throw ADException(message). "match" is a case-insensitive
        substring searched in the call's JSON args ("" matches all). "{password}" in message is replaced with the
        plaintext AccountPassword (only used to prove the scripts scrub secrets from errors).
      * schema (optional) holds classSchema / attributeSchema objects with their LDAP property names. Get-ADObject
        with -SearchBase <schemaNamingContext> returns them (the filter only picks classSchema vs attributeSchema).
      * failConnect=true makes every cmdlet throw ADServerDownException (exit code 4 tests).
      * Domain root DNs (starting with "DC=") are always valid search bases/paths. Other containers must be listed in ous.

    Supported LDAP filter syntax: (attr=value), (&...), (|...), (!...), presence (attr=*), * wildcards inside
    values, RFC 4515 \hh escapes (objectGUID values are matched as raw octets).
#>

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

if ([string]::IsNullOrEmpty($env:IDL_STUB_AD_STATE)){
    throw 'IDLifecycle STUB ActiveDirectory module refuses to load: environment variable IDL_STUB_AD_STATE is not set. This module is for tests only.'
}

class ADException : System.Exception {
    ADException([string]$message) : base($message) {}
}

class ADIdentityNotFoundException : ADException {
    ADIdentityNotFoundException([string]$message) : base($message) {}
}

class ADServerDownException : ADException {
    ADServerDownException([string]$message) : base($message) {}
}

$script:StubRoot = 'DC=stub,DC=test'
$script:StubSchemaNc = 'CN=Schema,CN=Configuration,DC=stub,DC=test'
$script:Utf8NoBom = [System.Text.UTF8Encoding]::new($false)
$script:StubLeafError = 'The directory service can perform the requested operation only on a leaf object.'

#region State file

function Get-StubKey {
    param($Object, [string]$Name, $Default = $null)

    if ($Object -isnot [System.Collections.IDictionary]){
        return $Default
    }

    foreach ($key in $Object.Keys){
        if ([string]$key -ieq $Name){
            return $Object[$key]
        }
    }

    return $Default
}

function New-StubDeterministicGuid {
    param([string]$Text)

    $md5 = [System.Security.Cryptography.MD5]::Create()
    try{
        return ([guid]::new($md5.ComputeHash([System.Text.Encoding]::UTF8.GetBytes($Text.ToLowerInvariant())))).ToString()
    }
    finally{
        $md5.Dispose()
    }
}

function Get-StubUserSid {
    # users[].objectSid when set, else a stable domain SID derived from the objectGUID (real Get-ADUser always returns SID).
    param($Entity)

    $sid = [string]$Entity.Source['objectSid']
    if ([string]::IsNullOrEmpty($sid)){
        $rid = [System.BitConverter]::ToUInt32(([guid]$Entity.Guid).ToByteArray(), 0) % 100000000
        $sid = 'S-1-5-21-1111111111-2222222222-3333333333-{0}' -f ($rid + 1000)
    }

    return [System.Security.Principal.SecurityIdentifier]::new($sid)
}

function ConvertTo-StubAttributeTable {
    param($Source)

    $table = @{}
    if ($Source -is [System.Collections.IDictionary]){
        foreach ($key in $Source.Keys){
            $table[[string]$key] = $Source[$key]
        }
    }

    return $table
}

function Read-StubState {
    $path = $env:IDL_STUB_AD_STATE
    $raw = $null
    if (Test-Path -LiteralPath $path){
        $raw = [System.IO.File]::ReadAllText($path, $script:Utf8NoBom)
    }

    $source = @{}
    if (-not [string]::IsNullOrWhiteSpace($raw)){
        $source = ConvertFrom-Json -InputObject $raw -AsHashtable
    }

    $state = @{
        users       = [System.Collections.Generic.List[object]]::new()
        groups      = [System.Collections.Generic.List[object]]::new()
        ous         = [System.Collections.Generic.List[object]]::new()
        objects     = [System.Collections.Generic.List[object]]::new()
        calls       = [System.Collections.Generic.List[object]]::new()
        faults      = [System.Collections.Generic.List[object]]::new()
        failConnect = [bool](Get-StubKey $source 'failConnect' $false)
        schema      = (Get-StubKey $source 'schema' @{})
    }

    if ($state.failConnect){
        throw [ADServerDownException]::new('Unable to contact the server. This may be because this server does not exist, it is currently down, or it does not have the Active Directory Web Services running.')
    }

    foreach ($u in @(Get-StubKey $source 'users' @())){
        $dn = [string](Get-StubKey $u 'dn')
        $guid = [string](Get-StubKey $u 'objectGuid')
        if ([string]::IsNullOrEmpty($guid)){
            $guid = New-StubDeterministicGuid -Text $dn
        }

        $state.users.Add(@{
                objectGuid  = ([guid]$guid).ToString()
                dn          = $dn
                sam         = [string](Get-StubKey $u 'sam')
                upn         = [string](Get-StubKey $u 'upn')
                enabled     = [bool](Get-StubKey $u 'enabled' $true)
                objectClass = [string](Get-StubKey $u 'objectClass' 'user')
                objectSid   = (Get-StubKey $u 'objectSid')
                attributes  = (ConvertTo-StubAttributeTable (Get-StubKey $u 'attributes'))
                memberOf    = [string[]]@(@(Get-StubKey $u 'memberOf' @()) | Where-Object { $_ } | ForEach-Object { [string]$_ })
            })
    }

    foreach ($g in @(Get-StubKey $source 'groups' @())){
        $dn = [string](Get-StubKey $g 'dn')
        $guid = [string](Get-StubKey $g 'objectGuid')
        if ([string]::IsNullOrEmpty($guid)){
            $guid = New-StubDeterministicGuid -Text $dn
        }

        $state.groups.Add(@{
                objectGuid = ([guid]$guid).ToString()
                dn         = $dn
                sam        = [string](Get-StubKey $g 'sam')
                adminCount = (Get-StubKey $g 'adminCount')
                objectSid  = (Get-StubKey $g 'objectSid')
                attributes = (ConvertTo-StubAttributeTable (Get-StubKey $g 'attributes'))
            })
    }

    foreach ($ou in @(Get-StubKey $source 'ous' @())){
        $state.ous.Add([string]$ou)
    }

    # Generic child objects (e.g. CN=ExchangeActiveSyncDevices,CN=<user>,...) that make a user a non-leaf.
    foreach ($o in @(Get-StubKey $source 'objects' @())){
        $dn = [string](Get-StubKey $o 'dn')
        $guid = [string](Get-StubKey $o 'objectGuid')
        if ([string]::IsNullOrEmpty($guid)){
            $guid = New-StubDeterministicGuid -Text $dn
        }

        $state.objects.Add(@{
                objectGuid  = ([guid]$guid).ToString()
                dn          = $dn
                objectClass = [string](Get-StubKey $o 'objectClass' 'container')
                attributes  = (ConvertTo-StubAttributeTable (Get-StubKey $o 'attributes'))
            })
    }

    foreach ($c in @(Get-StubKey $source 'calls' @())){
        $state.calls.Add($c)
    }

    foreach ($f in @(Get-StubKey $source 'faults' @())){
        $state.faults.Add($f)
    }

    return $state
}

function Save-StubState {
    param($State)

    $path = $env:IDL_STUB_AD_STATE
    $json = ConvertTo-Json -InputObject $State -Depth 20
    $tmp = $path + '.tmp'
    [System.IO.File]::WriteAllText($tmp, $json, $script:Utf8NoBom)
    Move-Item -LiteralPath $tmp -Destination $path -Force
}

function Add-StubCall {
    param($State, [string]$Cmdlet, [hashtable]$CallArgs)

    $call = @{
        cmdlet = $Cmdlet
        args   = $CallArgs
        result = 'pending'
    }
    $State.calls.Add($call)
    return $call
}

function Invoke-StubFault {
    param($State, [string]$Cmdlet, $Call, [string]$PlainPassword)

    $argsJson = ConvertTo-Json -InputObject $Call.args -Depth 10 -Compress
    foreach ($fault in $State.faults){
        if ([string](Get-StubKey $fault 'cmdlet') -ine $Cmdlet){
            continue
        }

        $match = [string](Get-StubKey $fault 'match' '')
        if (
            ($match -ne '') -and
            ($argsJson.IndexOf($match, [System.StringComparison]::OrdinalIgnoreCase) -lt 0)
        ){
            continue
        }

        $message = [string](Get-StubKey $fault 'message' 'stub fault')
        throw [ADException]::new($message.Replace('{password}', $PlainPassword))
    }
}

function ConvertTo-StubSummaryValue {
    param($Value, [string]$Name = '')

    if ($Name -match '(?i)password|unicodePwd|userPassword'){
        return '***'
    }

    if ($null -eq $Value){
        return $null
    }

    if ($Value -is [System.Security.SecureString]){
        return '***'
    }

    if ($Value -is [System.Collections.IDictionary]){
        $copy = [ordered]@{}
        foreach ($key in $Value.Keys){
            $copy[[string]$key] = ConvertTo-StubSummaryValue -Value $Value[$key] -Name ([string]$key)
        }

        return $copy
    }

    if (
        ($Value -isnot [string]) -and
        ($Value -is [System.Collections.IEnumerable])
    ){
        return @(@($Value) | ForEach-Object { ConvertTo-StubSummaryValue -Value $_ })
    }

    if ($Value -is [pscustomobject]){
        $dn = $Value.PSObject.Properties['DistinguishedName']
        if ($dn){
            return [string]$dn.Value
        }
    }

    return [string]$Value
}

function ConvertTo-StubCallArgs {
    param([System.Collections.IDictionary]$Bound)

    $summary = [ordered]@{}
    foreach ($name in $Bound.Keys){
        if ($name -in @('Confirm', 'WhatIf', 'Verbose', 'Debug', 'ErrorAction', 'WarningAction', 'InformationAction', 'ErrorVariable', 'WarningVariable', 'InformationVariable', 'OutVariable', 'OutBuffer', 'PipelineVariable', 'Server', 'Credential')){
            continue
        }

        $summary[$name] = ConvertTo-StubSummaryValue -Value $Bound[$name] -Name $name
    }

    return $summary
}

function ConvertTo-StubPlainPassword {
    param($Secure)

    if ($null -eq $Secure){
        return ''
    }

    return [System.Net.NetworkCredential]::new('', [System.Security.SecureString]$Secure).Password
}

#endregion

#region DN helpers

function Get-StubParentDn {
    param([string]$Dn)

    $m = [regex]::Match($Dn, '^(?:\\.|[^,\\])*,(.+)$')
    if ($m.Success){
        return $m.Groups[1].Value
    }

    return ''
}

function Get-StubLeafRdn {
    param([string]$Dn)

    $m = [regex]::Match($Dn, '^((?:\\.|[^,\\])*),.+$')
    if ($m.Success){
        return $m.Groups[1].Value
    }

    return $Dn
}

function Get-StubRdnValue {
    param([string]$Dn)

    $leaf = Get-StubLeafRdn -Dn $Dn
    $idx = $leaf.IndexOf('=')
    if ($idx -lt 0){
        return $leaf
    }

    return [regex]::Replace($leaf.Substring($idx + 1), '\\(.)', '$1')
}

function ConvertTo-StubEscapedRdn {
    param([string]$Value)

    $escaped = [regex]::Replace($Value, '([\\,+"<>;=])', '\$1')
    if ($escaped.StartsWith('#') -or $escaped.StartsWith(' ')){
        $escaped = '\' + $escaped
    }

    return $escaped
}

function Test-StubContainer {
    param($State, [string]$Dn)

    if ($Dn -match '^(?i)DC=[^,]+(,DC=[^,]+)*$'){
        return $true
    }

    foreach ($ou in $State.ous){
        if ([string]$ou -ieq $Dn){
            return $true
        }
    }

    return $false
}

#endregion

#region Entities and LDAP filter

function Get-StubEntities {
    param($State, [string[]]$Kinds)

    $all = [System.Collections.Generic.List[object]]::new()

    if ('user' -in $Kinds){
        foreach ($u in $State.users){
            $all.Add(@{ Kind = 'user'; Source = $u; Dn = $u.dn; Guid = $u.objectGuid; Sam = $u.sam; Upn = $u.upn; ObjectClass = $u.objectClass })
        }
    }

    if ('group' -in $Kinds){
        foreach ($g in $State.groups){
            $all.Add(@{ Kind = 'group'; Source = $g; Dn = $g.dn; Guid = $g.objectGuid; Sam = $g.sam; Upn = ''; ObjectClass = 'group' })
        }
    }

    if ('ou' -in $Kinds){
        foreach ($ou in $State.ous){
            $all.Add(@{ Kind = 'ou'; Source = $null; Dn = [string]$ou; Guid = (New-StubDeterministicGuid -Text ([string]$ou)); Sam = ''; Upn = ''; ObjectClass = ((([string]$ou).StartsWith('CN=', 'OrdinalIgnoreCase')) ? ('container') : ('organizationalUnit')) })
        }
    }

    if ('object' -in $Kinds){
        foreach ($o in $State.objects){
            $all.Add(@{ Kind = 'object'; Source = $o; Dn = $o.dn; Guid = $o.objectGuid; Sam = ''; Upn = ''; ObjectClass = $o.objectClass })
        }
    }

    return $all
}

function Get-StubDescendants {
    # Every user/group/object entity whose DN lies strictly below $Dn.
    param($State, [string]$Dn)

    $suffix = ',' + $Dn
    return @(Get-StubEntities -State $State -Kinds @('user', 'group', 'object') | Where-Object { $_.Dn.EndsWith($suffix, [System.StringComparison]::OrdinalIgnoreCase) })
}

function Test-StubSearchBase {
    # A search base is valid when it is a container or any existing object (e.g. a user with child objects).
    param($State, [string]$Dn)

    if (Test-StubContainer -State $State -Dn $Dn){
        return $true
    }

    foreach ($entity in (Get-StubEntities -State $State -Kinds @('user', 'group', 'object'))){
        if ($entity.Dn -ieq $Dn){
            return $true
        }
    }

    return $false
}

function Get-StubEntityValues {
    param($Entity, [string]$Attribute)

    $user = $Entity.Source
    switch ($Attribute.ToLowerInvariant()){
        'samaccountname' {
            if ($Entity.Sam){
                return @($Entity.Sam)
            }

            return @()
        }
        'userprincipalname' {
            if ($Entity.Upn){
                return @($Entity.Upn)
            }

            return @()
        }
        'distinguishedname' { return @($Entity.Dn) }
        'objectguid' { return @($Entity.Guid) }
        'cn' { return @(Get-StubRdnValue -Dn $Entity.Dn) }
        'name' { return @(Get-StubRdnValue -Dn $Entity.Dn) }
        'objectclass' {
            switch ($Entity.Kind){
                'user' { return @('top', 'person', 'organizationalPerson', $Entity.ObjectClass) }
                'group' { return @('top', 'group') }
                default { return @('top', $Entity.ObjectClass) }
            }
        }
        'objectcategory' {
            switch ($Entity.Kind){
                'user' {
                    if ($Entity.ObjectClass -eq 'user'){
                        return @('person')
                    }

                    return @($Entity.ObjectClass)
                }
                'group' { return @('group') }
                default { return @($Entity.ObjectClass) }
            }
        }
        'memberof' {
            if ($Entity.Kind -eq 'user'){
                return [string[]]$user.memberOf
            }

            return @()
        }
        default {
            if ($Entity.Kind -eq 'group'){
                $groupValue = $null
                foreach ($key in $Entity.Source.Keys){
                    if (
                        ([string]$key -ieq $Attribute) -and
                        ([string]$key -in @('adminCount', 'objectSid'))
                    ){
                        $groupValue = $Entity.Source[$key]
                    }
                }

                if ($null -ne $groupValue){
                    return @([string]$groupValue)
                }
            }

            # users, groups and generic objects (e.g. contacts) may carry an attributes table.
            if (
                ($Entity.Kind -notin @('user', 'group', 'object')) -or
                ($null -eq $Entity.Source) -or
                (-not $Entity.Source.ContainsKey('attributes')) -or
                ($null -eq $Entity.Source.attributes)
            ){
                return @()
            }

            foreach ($key in $Entity.Source.attributes.Keys){
                if ([string]$key -ieq $Attribute){
                    $value = $Entity.Source.attributes[$key]
                    if ($null -eq $value){
                        return @()
                    }

                    return @(@($value) | ForEach-Object { [string]$_ })
                }
            }

            return @()
        }
    }
}

function ConvertFrom-StubLdapBytes {
    param([string]$Raw)

    $bytes = [System.Collections.Generic.List[byte]]::new()
    $i = 0
    while ($i -lt $Raw.Length){
        $next = $Raw.IndexOf('\', $i)
        if ($next -lt 0){
            $bytes.AddRange($script:Utf8NoBom.GetBytes($Raw.Substring($i)))
            break
        }

        if ($next -gt $i){
            $bytes.AddRange($script:Utf8NoBom.GetBytes($Raw.Substring($i, $next - $i)))
        }

        if (
            ($next + 2 -lt $Raw.Length + 0) -and
            ($Raw.Substring($next + 1, 2) -match '^[0-9a-fA-F]{2}$')
        ){
            $bytes.Add([System.Convert]::ToByte($Raw.Substring($next + 1, 2), 16))
            $i = $next + 3
        }
        else{
            throw [ADException]::new("Stub: invalid escape in LDAP filter value '$Raw'")
        }
    }

    return , $bytes.ToArray()
}

function ConvertFrom-StubLdapFilter {
    param([string]$Filter)

    $text = $Filter.Trim()
    $pos = [ref]0
    $ast = Read-StubFilterNode -Text $text -Pos $pos
    if ($pos.Value -ne $text.Length){
        throw [ADException]::new("Stub: unexpected trailing characters in LDAP filter '$Filter'")
    }

    return $ast
}

function Read-StubFilterNode {
    param([string]$Text, [ref]$Pos)

    if (
        ($Pos.Value -ge $Text.Length) -or
        ($Text[$Pos.Value] -ne '(')
    ){
        throw [ADException]::new("Stub: malformed LDAP filter (expected '(' at $($Pos.Value)): '$Text'")
    }

    $Pos.Value++
    $c = $Text[$Pos.Value]

    if ($c -in @('&', '|')){
        $Pos.Value++
        $children = [System.Collections.Generic.List[object]]::new()
        while (
            ($Pos.Value -lt $Text.Length) -and
            ($Text[$Pos.Value] -eq '(')
        ){
            $children.Add((Read-StubFilterNode -Text $Text -Pos $Pos))
        }

        if (
            ($Pos.Value -ge $Text.Length) -or
            ($Text[$Pos.Value] -ne ')')
        ){
            throw [ADException]::new("Stub: unbalanced LDAP filter '$Text'")
        }

        $Pos.Value++
        return @{ Op = (($c -eq '&') ? ('and') : ('or')); Children = $children.ToArray() }
    }

    if ($c -eq '!'){
        $Pos.Value++
        $child = Read-StubFilterNode -Text $Text -Pos $Pos
        if (
            ($Pos.Value -ge $Text.Length) -or
            ($Text[$Pos.Value] -ne ')')
        ){
            throw [ADException]::new("Stub: unbalanced LDAP filter '$Text'")
        }

        $Pos.Value++
        return @{ Op = 'not'; Children = @($child) }
    }

    $close = $Text.IndexOf(')', $Pos.Value)
    if ($close -lt 0){
        throw [ADException]::new("Stub: unbalanced LDAP filter '$Text'")
    }

    $item = $Text.Substring($Pos.Value, $close - $Pos.Value)
    $Pos.Value = $close + 1

    if ($item -match '^[^=]*[<>~]='){
        throw [ADException]::new("Stub: unsupported LDAP operator in '($item)'")
    }

    $eq = $item.IndexOf('=')
    if ($eq -lt 1){
        throw [ADException]::new("Stub: malformed LDAP filter item '($item)'")
    }

    return @{ Op = 'eq'; Attr = $item.Substring(0, $eq); Value = $item.Substring($eq + 1) }
}

function Test-StubFilter {
    param($Ast, $Entity)

    switch ($Ast.Op){
        'and' {
            foreach ($child in $Ast.Children){
                if (-not (Test-StubFilter -Ast $child -Entity $Entity)){
                    return $false
                }
            }

            return $true
        }
        'or' {
            foreach ($child in $Ast.Children){
                if (Test-StubFilter -Ast $child -Entity $Entity){
                    return $true
                }
            }

            return $false
        }
        'not' {
            return (-not (Test-StubFilter -Ast $Ast.Children[0] -Entity $Entity))
        }
    }

    $values = @(Get-StubEntityValues -Entity $Entity -Attribute $Ast.Attr)
    $raw = [string]$Ast.Value
    if ($raw -eq '*'){
        return ($values.Count -gt 0)
    }

    if ($Ast.Attr -ieq 'objectGUID'){
        $bytes = ConvertFrom-StubLdapBytes -Raw $raw
        if ($bytes.Count -ne 16){
            return $false
        }

        return (([guid]::new([byte[]]$bytes)).ToString() -ieq $Entity.Guid)
    }

    $parts = $raw.Split('*')
    $pattern = '^' + (($parts | ForEach-Object { [regex]::Escape($script:Utf8NoBom.GetString((ConvertFrom-StubLdapBytes -Raw $_))) }) -join '.*') + '$'
    foreach ($value in $values){
        if ([regex]::IsMatch($value, $pattern, 'IgnoreCase')){
            return $true
        }
    }

    return $false
}

function ConvertFrom-StubPowerShellFilter {
    param([string]$Filter)

    $f = $Filter.Trim()
    if ($f -eq '*'){
        return $null
    }

    $m = [regex]::Match($f, "^(\w+)\s+-(eq|like)\s+'(.*)'$")
    if (-not $m.Success){
        throw [ADException]::new("Stub: unsupported -Filter '$Filter' (use '*', attr -eq 'v', or -LDAPFilter)")
    }

    return @{ Op = 'eq'; Attr = $m.Groups[1].Value; Value = $m.Groups[3].Value }
}

function Search-StubEntities {
    param($State, [string[]]$Kinds, $Ast, [string]$SearchBase, [string]$SearchScope = 'Subtree')

    if (
        ($SearchBase) -and
        (-not (Test-StubSearchBase -State $State -Dn $SearchBase))
    ){
        throw [ADException]::new("Directory object not found (search base '$SearchBase')")
    }

    $hits = [System.Collections.Generic.List[object]]::new()
    foreach ($entity in (Get-StubEntities -State $State -Kinds $Kinds)){
        if ($SearchBase){
            switch ($SearchScope){
                'Base' { $inScope = ($entity.Dn -ieq $SearchBase) }
                'OneLevel' { $inScope = ((Get-StubParentDn -Dn $entity.Dn) -ieq $SearchBase) }
                default { $inScope = (($entity.Dn -ieq $SearchBase) -or $entity.Dn.EndsWith(',' + $SearchBase, [System.StringComparison]::OrdinalIgnoreCase)) }
            }

            if (-not $inScope){
                continue
            }
        }

        if (
            ($null -ne $Ast) -and
            (-not (Test-StubFilter -Ast $Ast -Entity $entity))
        ){
            continue
        }

        $hits.Add($entity)
    }

    return , $hits.ToArray()
}

function Resolve-StubIdentity {
    param($State, $Identity, [string[]]$Kinds)

    $dn = $null
    $guid = $null
    $sam = $null

    if ($Identity -is [guid]){
        $guid = $Identity.ToString()
    }
    elseif ($Identity -isnot [string]){
        $dnProp = $Identity.PSObject.Properties['DistinguishedName']
        $guidProp = $Identity.PSObject.Properties['ObjectGUID']
        if ($guidProp){
            $guid = ([guid]$guidProp.Value).ToString()
        }
        elseif ($dnProp){
            $dn = [string]$dnProp.Value
        }
        else{
            $sam = [string]$Identity
        }
    }
    else{
        $text = [string]$Identity
        $parsed = [guid]::Empty
        if ([guid]::TryParse($text, [ref]$parsed)){
            $guid = $parsed.ToString()
        }
        elseif ($text.Contains('=')){
            $dn = $text
        }
        else{
            $sam = $text
        }
    }

    foreach ($entity in (Get-StubEntities -State $State -Kinds $Kinds)){
        if (
            ($guid -and ($entity.Guid -ieq $guid)) -or
            ($dn -and ($entity.Dn -ieq $dn)) -or
            ($sam -and ($entity.Sam) -and ($entity.Sam -ieq $sam))
        ){
            return $entity
        }
    }

    $shown = $sam
    if ($guid){
        $shown = $guid
    }
    elseif ($dn){
        $shown = $dn
    }

    throw [ADIdentityNotFoundException]::new("Cannot find an object with identity: '$shown' under: '$script:StubRoot'.")
}

function New-StubAdObject {
    param($Entity, [string[]]$Properties = @())

    $o = [ordered]@{}
    $o['DistinguishedName'] = $Entity.Dn
    switch ($Entity.Kind){
        'user' {
            $o['Enabled'] = [bool]$Entity.Source.enabled
            $o['SID'] = Get-StubUserSid -Entity $Entity
            $given = @(Get-StubEntityValues -Entity $Entity -Attribute 'givenName')
            if ($given.Count -gt 0){
                $o['GivenName'] = $given[0]
            }
        }
        'group' {
            $o['GroupCategory'] = 'Security'
            $o['GroupScope'] = 'Global'
        }
    }

    $o['Name'] = Get-StubRdnValue -Dn $Entity.Dn
    $o['ObjectClass'] = $Entity.ObjectClass
    $o['ObjectGUID'] = [guid]$Entity.Guid
    if ($Entity.Kind -notin @('ou', 'object')){
        $o['SamAccountName'] = $Entity.Sam
    }

    if ($Entity.Kind -eq 'user'){
        $sn = @(Get-StubEntityValues -Entity $Entity -Attribute 'sn')
        if ($sn.Count -gt 0){
            $o['Surname'] = $sn[0]
        }

        $o['UserPrincipalName'] = $Entity.Upn
    }

    $requested = [System.Collections.Generic.List[string]]::new()
    foreach ($p in $Properties){
        if ($p -eq '*'){
            if ($Entity.Kind -eq 'user'){
                foreach ($k in $Entity.Source.attributes.Keys){
                    $requested.Add([string]$k)
                }

                $requested.Add('memberOf')
            }
        }
        else{
            $requested.Add($p)
        }
    }

    foreach ($p in $requested){
        $exists = $false
        foreach ($k in $o.Keys){
            if ([string]$k -ieq $p){
                $exists = $true
            }
        }

        if ($exists){
            continue
        }

        $values = @(Get-StubEntityValues -Entity $Entity -Attribute $p)
        if ($values.Count -eq 0){
            continue
        }

        if ($p -ieq 'memberOf'){
            $o[$p] = [string[]]$values
        }
        elseif ($values.Count -eq 1){
            $o[$p] = $values[0]
        }
        else{
            $o[$p] = [string[]]$values
        }
    }

    return [pscustomobject]$o
}

#endregion

#region Read cmdlets

function Get-ADRootDSE {
    [CmdletBinding()]
    param([string]$Server, [pscredential]$Credential)

    $null = Read-StubState
    return [pscustomobject]@{
        defaultNamingContext = $script:StubRoot
        rootDomainNamingContext = $script:StubRoot
        schemaNamingContext = $script:StubSchemaNc
        dnsHostName = 'stub-dc.stub.test'
    }
}

function Invoke-StubGet {
    param($Kinds, $Identity, [string]$LDAPFilter, [string]$Filter, [string]$SearchBase, [string]$SearchScope, [string[]]$Properties, [bool]$HasIdentity, [string]$CmdletName = '')

    $state = Read-StubState

    # Optional read-side fault injection (not recorded in calls): faults[] entries whose cmdlet is this
    # cmdlet and whose "match" is a substring of Identity/LDAPFilter/SearchBase make the read throw.
    if ($CmdletName){
        $probe = "$Identity|$LDAPFilter|$Filter|$SearchBase"
        foreach ($fault in $state.faults){
            if ([string](Get-StubKey $fault 'cmdlet') -ine $CmdletName){
                continue
            }

            $match = [string](Get-StubKey $fault 'match' '')
            if (
                ($match -eq '') -or
                ($probe.IndexOf($match, [System.StringComparison]::OrdinalIgnoreCase) -ge 0)
            ){
                throw [ADException]::new([string](Get-StubKey $fault 'message' 'stub fault'))
            }
        }
    }

    if (
        (-not $HasIdentity) -and
        ($SearchBase -ieq $script:StubSchemaNc)
    ){
        $kind = (($LDAPFilter -match 'classSchema') ? ('classes') : ('attributes'))
        foreach ($o in @(Get-StubKey $state.schema $kind @())){
            [pscustomobject]$o
        }

        return
    }

    if ($HasIdentity){
        $entity = Resolve-StubIdentity -State $state -Identity $Identity -Kinds $Kinds
        return (New-StubAdObject -Entity $entity -Properties $Properties)
    }

    if ($LDAPFilter){
        $ast = ConvertFrom-StubLdapFilter -Filter $LDAPFilter
    }
    else{
        $ast = ConvertFrom-StubPowerShellFilter -Filter $Filter
    }

    $scope = (($SearchScope) ? ($SearchScope) : ('Subtree'))
    foreach ($entity in (Search-StubEntities -State $state -Kinds $Kinds -Ast $ast -SearchBase $SearchBase -SearchScope $scope)){
        New-StubAdObject -Entity $entity -Properties $Properties
    }
}

function Get-ADUser {
    [CmdletBinding(DefaultParameterSetName = 'Filter')]
    param(
        [Parameter(ParameterSetName = 'Identity', Mandatory, Position = 0)]$Identity,
        [Parameter(ParameterSetName = 'LDAPFilter', Mandatory)][string]$LDAPFilter,
        [Parameter(ParameterSetName = 'Filter', Mandatory)][string]$Filter,
        [string]$SearchBase,
        [ValidateSet('Base', 'OneLevel', 'Subtree')][string]$SearchScope,
        [string[]]$Properties = @(),
        [int]$ResultPageSize,
        [int]$ResultSetSize,
        [string]$Server,
        [pscredential]$Credential
    )

    Invoke-StubGet -Kinds @('user') -Identity $Identity -LDAPFilter $LDAPFilter -Filter $Filter -SearchBase $SearchBase -SearchScope $SearchScope -Properties $Properties -HasIdentity ($PSCmdlet.ParameterSetName -eq 'Identity')
}

function Get-ADGroup {
    [CmdletBinding(DefaultParameterSetName = 'Filter')]
    param(
        [Parameter(ParameterSetName = 'Identity', Mandatory, Position = 0)]$Identity,
        [Parameter(ParameterSetName = 'LDAPFilter', Mandatory)][string]$LDAPFilter,
        [Parameter(ParameterSetName = 'Filter', Mandatory)][string]$Filter,
        [string]$SearchBase,
        [ValidateSet('Base', 'OneLevel', 'Subtree')][string]$SearchScope,
        [string[]]$Properties = @(),
        [int]$ResultPageSize,
        [int]$ResultSetSize,
        [string]$Server,
        [pscredential]$Credential
    )

    $found = Invoke-StubGet -Kinds @('group') -Identity $Identity -LDAPFilter $LDAPFilter -Filter $Filter -SearchBase $SearchBase -SearchScope $SearchScope -Properties $Properties -HasIdentity ($PSCmdlet.ParameterSetName -eq 'Identity') -CmdletName 'Get-ADGroup'
    if ($ResultSetSize -gt 0){
        $found = @($found) | Select-Object -First $ResultSetSize
    }

    $found
}

function Get-ADObject {
    [CmdletBinding(DefaultParameterSetName = 'Filter')]
    param(
        [Parameter(ParameterSetName = 'Identity', Mandatory, Position = 0)]$Identity,
        [Parameter(ParameterSetName = 'LDAPFilter', Mandatory)][string]$LDAPFilter,
        [Parameter(ParameterSetName = 'Filter', Mandatory)][string]$Filter,
        [string]$SearchBase,
        [ValidateSet('Base', 'OneLevel', 'Subtree')][string]$SearchScope,
        [string[]]$Properties = @(),
        [int]$ResultPageSize,
        [string]$Server,
        [pscredential]$Credential
    )

    Invoke-StubGet -CmdletName 'Get-ADObject' -Kinds @('user', 'group', 'ou', 'object') -Identity $Identity -LDAPFilter $LDAPFilter -Filter $Filter -SearchBase $SearchBase -SearchScope $SearchScope -Properties $Properties -HasIdentity ($PSCmdlet.ParameterSetName -eq 'Identity')
}

#endregion

#region Mutating cmdlets

function Test-StubUnique {
    param($State, [string]$Sam, [string]$Upn, [string]$Dn, $ExceptGuid = $null)

    foreach ($entity in (Get-StubEntities -State $State -Kinds @('user', 'group', 'ou'))){
        if ($ExceptGuid -and ($entity.Guid -ieq [string]$ExceptGuid)){
            continue
        }

        if (
            ($Sam) -and
            ($entity.Sam) -and
            ($entity.Sam -ieq $Sam)
        ){
            throw [ADException]::new("The specified account already exists (sAMAccountName '$Sam').")
        }

        if (
            ($Upn) -and
            ($entity.Upn) -and
            ($entity.Upn -ieq $Upn)
        ){
            throw [ADException]::new("The specified userPrincipalName already exists ('$Upn').")
        }

        if (
            ($Dn) -and
            ($entity.Dn -ieq $Dn)
        ){
            throw [ADException]::new("The object already exists ('$Dn').")
        }
    }
}

function New-ADUser {
    [CmdletBinding(SupportsShouldProcess)]
    param(
        [Parameter(Mandatory, Position = 0)][string]$Name,
        [string]$SamAccountName,
        [string]$UserPrincipalName,
        [string]$Path,
        [securestring]$AccountPassword,
        [bool]$Enabled,
        [bool]$ChangePasswordAtLogon,
        [hashtable]$OtherAttributes,
        [string]$GivenName,
        [string]$Surname,
        [string]$DisplayName,
        [string]$Description,
        [switch]$PassThru,
        [string]$Server,
        [pscredential]$Credential
    )

    $state = Read-StubState
    $call = Add-StubCall -State $state -Cmdlet 'New-ADUser' -CallArgs (ConvertTo-StubCallArgs -Bound $PSBoundParameters)
    $plainPassword = ConvertTo-StubPlainPassword -Secure $AccountPassword
    try{
        Invoke-StubFault -State $state -Cmdlet 'New-ADUser' -Call $call -PlainPassword $plainPassword

        if (-not $Path){
            $Path = "CN=Users,$script:StubRoot"
        }

        if (-not (Test-StubContainer -State $state -Dn $Path)){
            throw [ADException]::new("Directory object not found: the path '$Path' does not exist.")
        }

        if (-not $SamAccountName){
            $SamAccountName = $Name
        }

        if ($SamAccountName.Length -gt 20){
            throw [ADException]::new("The specified sAMAccountName '$SamAccountName' is longer than 20 characters.")
        }

        $enabledValue = $PSBoundParameters.ContainsKey('Enabled') -and $Enabled
        if (
            $enabledValue -and
            (-not $AccountPassword)
        ){
            throw [ADException]::new('The server is unwilling to process the request (an enabled account requires a password).')
        }

        $dn = 'CN={0},{1}' -f (ConvertTo-StubEscapedRdn -Value $Name), $Path
        Test-StubUnique -State $state -Sam $SamAccountName -Upn $UserPrincipalName -Dn $dn

        $attributes = @{}
        if ($OtherAttributes){
            foreach ($key in $OtherAttributes.Keys){
                $attributes[[string]$key] = $OtherAttributes[$key]
            }
        }

        foreach ($pair in @(@('givenName', $GivenName), @('sn', $Surname), @('displayName', $DisplayName), @('description', $Description))){
            if ($pair[1]){
                $attributes[$pair[0]] = $pair[1]
            }
        }

        if ($PSBoundParameters.ContainsKey('ChangePasswordAtLogon') -and $ChangePasswordAtLogon){
            $attributes['pwdLastSet'] = '0'
        }

        $user = @{
            objectGuid  = ([guid]::NewGuid()).ToString()
            dn          = $dn
            sam         = $SamAccountName
            upn         = $UserPrincipalName
            enabled     = $enabledValue
            objectClass = 'user'
            attributes  = $attributes
            objectSid   = $null
            memberOf    = [string[]]@()
        }
        $state.users.Add($user)
        $call.result = 'ok'
    }
    catch{
        $call.result = 'error: ' + (($plainPassword) ? ($_.Exception.Message.Replace($plainPassword, '***')) : ($_.Exception.Message))
        throw
    }
    finally{
        Save-StubState -State $state
    }

    if ($PassThru){
        $entity = Resolve-StubIdentity -State $state -Identity $user.objectGuid -Kinds @('user')
        New-StubAdObject -Entity $entity
    }
}

function Set-ADUser {
    [CmdletBinding(SupportsShouldProcess)]
    param(
        [Parameter(Mandatory, Position = 0)]$Identity,
        [hashtable]$Replace,
        [string[]]$Clear,
        [hashtable]$Add,
        [hashtable]$Remove,
        [string]$Description,
        [string]$SamAccountName,
        [string]$UserPrincipalName,
        [string]$EmailAddress,
        [bool]$ChangePasswordAtLogon,
        [bool]$Enabled,
        [switch]$PassThru,
        [string]$Server,
        [pscredential]$Credential
    )

    $state = Read-StubState
    $call = Add-StubCall -State $state -Cmdlet 'Set-ADUser' -CallArgs (ConvertTo-StubCallArgs -Bound $PSBoundParameters)
    $user = $null
    $original = $null
    try{
        Invoke-StubFault -State $state -Cmdlet 'Set-ADUser' -Call $call -PlainPassword ''

        $entity = Resolve-StubIdentity -State $state -Identity $Identity -Kinds @('user')
        $user = $entity.Source
        $originalSam = $user.sam
        $originalUpn = $user.upn
        $readOnly = @('distinguishedname', 'objectguid', 'objectsid', 'memberof', 'cn', 'name', 'unicodepwd', 'userpassword', 'useraccountcontrol', 'samaccountname', 'userprincipalname')

        # Like the real cmdlet: -Remove, then -Add, then -Replace, then -Clear (multi-valued values compare case-insensitively).
        # Worked out on a copy first, so a refused value leaves the object untouched (an LDAP modify is atomic).
        $original = $user.attributes
        $user.attributes = @{}
        foreach ($k in $original.Keys){
            $user.attributes[$k] = $original[$k]
        }

        foreach ($pair in @(@('Remove', $Remove), @('Add', $Add))){
            $operation = $pair[0]
            $table = $pair[1]
            if ($null -eq $table){
                continue
            }

            foreach ($key in $table.Keys){
                if (([string]$key).ToLowerInvariant() -in $readOnly){
                    throw [ADException]::new("Stub: attribute '$key' cannot be modified through Set-ADUser -$operation (constraint violation).")
                }

                $existingKey = $null
                foreach ($k in @($user.attributes.Keys)){
                    if ([string]$k -ieq [string]$key){
                        $existingKey = $k
                    }
                }

                $current = [System.Collections.Generic.List[string]]::new()
                if ($null -ne $existingKey){
                    foreach ($v in @($user.attributes[$existingKey])){
                        if ($null -ne $v){
                            $current.Add([string]$v)
                        }
                    }

                    $user.attributes.Remove($existingKey)
                }

                foreach ($v in @($table[$key])){
                    $text = [string]$v
                    $index = -1
                    for ($j = 0; $j -lt $current.Count; $j++){
                        if ($current[$j] -ieq $text){
                            $index = $j
                        }
                    }

                    if ($operation -eq 'Add'){
                        if ($index -ge 0){
                            throw [ADException]::new("The specified value already exists ('$key').")
                        }

                        $current.Add($text)
                    }
                    else{
                        if ($index -lt 0){
                            throw [ADException]::new("The specified value does not exist ('$key').")
                        }

                        $current.RemoveAt($index)
                    }
                }

                if ($current.Count -gt 0){
                    $user.attributes[[string]$key] = (($current.Count -eq 1) ? ($current[0]) : ([string[]]$current.ToArray()))
                }
            }
        }

        if ($PSBoundParameters.ContainsKey('SamAccountName')){
            if ([string]::IsNullOrWhiteSpace($SamAccountName) -or ($SamAccountName.Length -gt 20)){
                throw [ADException]::new("The specified sAMAccountName '$SamAccountName' is empty or longer than 20 characters.")
            }

            Test-StubUnique -State $state -Sam $SamAccountName -Upn '' -Dn '' -ExceptGuid $user.objectGuid
            $user.sam = $SamAccountName
        }

        if ($PSBoundParameters.ContainsKey('UserPrincipalName')){
            Test-StubUnique -State $state -Sam '' -Upn $UserPrincipalName -Dn '' -ExceptGuid $user.objectGuid
            $user.upn = $UserPrincipalName
        }

        $replacements = @{}
        if ($PSBoundParameters.ContainsKey('EmailAddress')){
            $replacements['mail'] = $EmailAddress
        }

        if ($Replace){
            foreach ($key in $Replace.Keys){
                $replacements[[string]$key] = $Replace[$key]
            }
        }

        if ($PSBoundParameters.ContainsKey('Description')){
            $replacements['description'] = $Description
        }

        foreach ($key in $replacements.Keys){
            $value = $replacements[$key]
            switch ($key.ToLowerInvariant()){
                { $_ -in @('distinguishedname', 'objectguid', 'objectsid', 'memberof', 'cn', 'name', 'unicodepwd', 'userpassword', 'useraccountcontrol') } {
                    throw [ADException]::new("Stub: attribute '$key' cannot be modified through Set-ADUser -Replace (constraint violation).")
                }
                'samaccountname' {
                    Test-StubUnique -State $state -Sam ([string]$value) -Upn '' -Dn '' -ExceptGuid $user.objectGuid
                    $user.sam = [string]$value
                }
                'userprincipalname' {
                    Test-StubUnique -State $state -Sam '' -Upn ([string]$value) -Dn '' -ExceptGuid $user.objectGuid
                    $user.upn = [string]$value
                }
                default {
                    foreach ($existing in @($user.attributes.Keys)){
                        if ([string]$existing -ieq $key){
                            $user.attributes.Remove($existing)
                        }
                    }

                    if (
                        ($null -ne $value) -and
                        ([string]$value -ne '')
                    ){
                        $user.attributes[$key] = $value
                    }
                }
            }
        }

        foreach ($key in @($Clear)){
            if (-not $key){
                continue
            }

            switch ($key.ToLowerInvariant()){
                { $_ -in @('samaccountname', 'distinguishedname', 'objectguid', 'cn', 'name') } {
                    throw [ADException]::new("Stub: attribute '$key' cannot be cleared.")
                }
                'userprincipalname' { $user.upn = '' }
                default {
                    foreach ($existing in @($user.attributes.Keys)){
                        if ([string]$existing -ieq $key){
                            $user.attributes.Remove($existing)
                        }
                    }
                }
            }
        }

        if ($PSBoundParameters.ContainsKey('ChangePasswordAtLogon')){
            if ($ChangePasswordAtLogon){
                $user.attributes['pwdLastSet'] = '0'
            }
            else{
                $user.attributes['pwdLastSet'] = '1'
            }
        }

        if ($PSBoundParameters.ContainsKey('Enabled')){
            $user.enabled = $Enabled
        }

        $call.result = 'ok'
    }
    catch{
        $call.result = 'error: ' + $_.Exception.Message
        if (($null -ne $user) -and ($null -ne $original)){
            $user.attributes = $original
            $user.sam = $originalSam
            $user.upn = $originalUpn
        }

        throw
    }
    finally{
        Save-StubState -State $state
    }

    if ($PassThru){
        New-StubAdObject -Entity (Resolve-StubIdentity -State $state -Identity $Identity -Kinds @('user'))
    }
}

function Set-StubAccountEnabled {
    param([string]$Cmdlet, $Identity, [bool]$Value, [System.Collections.IDictionary]$Bound)

    $state = Read-StubState
    $call = Add-StubCall -State $state -Cmdlet $Cmdlet -CallArgs (ConvertTo-StubCallArgs -Bound $Bound)
    try{
        Invoke-StubFault -State $state -Cmdlet $Cmdlet -Call $call -PlainPassword ''
        $entity = Resolve-StubIdentity -State $state -Identity $Identity -Kinds @('user')
        $entity.Source.enabled = $Value
        $call.result = 'ok'
    }
    catch{
        $call.result = 'error: ' + $_.Exception.Message
        throw
    }
    finally{
        Save-StubState -State $state
    }
}

function Disable-ADAccount {
    [CmdletBinding(SupportsShouldProcess)]
    param(
        [Parameter(Mandatory, Position = 0)]$Identity,
        [switch]$PassThru,
        [string]$Server,
        [pscredential]$Credential
    )

    Set-StubAccountEnabled -Cmdlet 'Disable-ADAccount' -Identity $Identity -Value $false -Bound $PSBoundParameters
}

function Enable-ADAccount {
    [CmdletBinding(SupportsShouldProcess)]
    param(
        [Parameter(Mandatory, Position = 0)]$Identity,
        [switch]$PassThru,
        [string]$Server,
        [pscredential]$Credential
    )

    Set-StubAccountEnabled -Cmdlet 'Enable-ADAccount' -Identity $Identity -Value $true -Bound $PSBoundParameters
}

function Remove-ADUser {
    [CmdletBinding(SupportsShouldProcess, ConfirmImpact = 'High')]
    param(
        [Parameter(Mandatory, Position = 0)]$Identity,
        [string]$Server,
        [pscredential]$Credential
    )

    $state = Read-StubState
    $call = Add-StubCall -State $state -Cmdlet 'Remove-ADUser' -CallArgs (ConvertTo-StubCallArgs -Bound $PSBoundParameters)
    try{
        Invoke-StubFault -State $state -Cmdlet 'Remove-ADUser' -Call $call -PlainPassword ''
        $entity = Resolve-StubIdentity -State $state -Identity $Identity -Kinds @('user')
        # Like real AD: Remove-ADUser only deletes leaf objects (use Remove-ADObject -Recursive otherwise).
        if (@(Get-StubDescendants -State $state -Dn $entity.Dn).Count -gt 0){
            throw [ADException]::new($script:StubLeafError)
        }

        if ($PSCmdlet.ShouldProcess($entity.Dn, 'Remove')){
            $null = $state.users.Remove($entity.Source)
        }

        $call.result = 'ok'
    }
    catch{
        $call.result = 'error: ' + $_.Exception.Message
        throw
    }
    finally{
        Save-StubState -State $state
    }
}

function Remove-ADObject {
    [CmdletBinding(SupportsShouldProcess, ConfirmImpact = 'High')]
    param(
        [Parameter(Mandatory, Position = 0)]$Identity,
        [switch]$Recursive,
        [string]$Server,
        [pscredential]$Credential
    )

    $state = Read-StubState
    $call = Add-StubCall -State $state -Cmdlet 'Remove-ADObject' -CallArgs (ConvertTo-StubCallArgs -Bound $PSBoundParameters)
    try{
        Invoke-StubFault -State $state -Cmdlet 'Remove-ADObject' -Call $call -PlainPassword ''
        $entity = Resolve-StubIdentity -State $state -Identity $Identity -Kinds @('user', 'group', 'object')
        $children = @(Get-StubDescendants -State $state -Dn $entity.Dn)
        if (
            ($children.Count -gt 0) -and
            (-not $Recursive)
        ){
            throw [ADException]::new($script:StubLeafError)
        }

        if ($PSCmdlet.ShouldProcess($entity.Dn, 'Remove')){
            foreach ($victim in @($children) + @($entity)){
                switch ($victim.Kind){
                    'user' { $null = $state.users.Remove($victim.Source) }
                    'group' { $null = $state.groups.Remove($victim.Source) }
                    'object' { $null = $state.objects.Remove($victim.Source) }
                }
            }
        }

        $call.result = 'ok'
    }
    catch{
        $call.result = 'error: ' + $_.Exception.Message
        throw
    }
    finally{
        Save-StubState -State $state
    }
}

function Move-ADObject {
    [CmdletBinding(SupportsShouldProcess)]
    param(
        [Parameter(Mandatory, Position = 0)]$Identity,
        [Parameter(Mandatory)][string]$TargetPath,
        [switch]$PassThru,
        [string]$Server,
        [pscredential]$Credential
    )

    $state = Read-StubState
    $call = Add-StubCall -State $state -Cmdlet 'Move-ADObject' -CallArgs (ConvertTo-StubCallArgs -Bound $PSBoundParameters)
    try{
        Invoke-StubFault -State $state -Cmdlet 'Move-ADObject' -Call $call -PlainPassword ''
        $entity = Resolve-StubIdentity -State $state -Identity $Identity -Kinds @('user', 'group')
        if (-not (Test-StubContainer -State $state -Dn $TargetPath)){
            throw [ADException]::new("Directory object not found: target path '$TargetPath' does not exist.")
        }

        $newDn = '{0},{1}' -f (Get-StubLeafRdn -Dn $entity.Dn), $TargetPath
        Test-StubUnique -State $state -Sam '' -Upn '' -Dn $newDn -ExceptGuid $entity.Guid
        # Child objects move with their parent.
        foreach ($child in (Get-StubDescendants -State $state -Dn $entity.Dn)){
            $child.Source.dn = $child.Dn.Substring(0, $child.Dn.Length - $entity.Dn.Length) + $newDn
        }

        $entity.Source.dn = $newDn
        $call.result = 'ok'
    }
    catch{
        $call.result = 'error: ' + $_.Exception.Message
        throw
    }
    finally{
        Save-StubState -State $state
    }

    if ($PassThru){
        New-StubAdObject -Entity (Resolve-StubIdentity -State $state -Identity $Identity -Kinds @('user', 'group'))
    }
}

function Rename-ADObject {
    [CmdletBinding(SupportsShouldProcess)]
    param(
        [Parameter(Mandatory, Position = 0)]$Identity,
        [Parameter(Mandatory, Position = 1)][string]$NewName,
        [switch]$PassThru,
        [string]$Server,
        [pscredential]$Credential
    )

    $state = Read-StubState
    $call = Add-StubCall -State $state -Cmdlet 'Rename-ADObject' -CallArgs (ConvertTo-StubCallArgs -Bound $PSBoundParameters)
    try{
        Invoke-StubFault -State $state -Cmdlet 'Rename-ADObject' -Call $call -PlainPassword ''
        $entity = Resolve-StubIdentity -State $state -Identity $Identity -Kinds @('user', 'group', 'object')
        if ([string]::IsNullOrWhiteSpace($NewName) -or ($NewName.Length -gt 64)){
            throw [ADException]::new("Stub: the new name '$NewName' is empty or longer than 64 characters.")
        }

        $leaf = Get-StubLeafRdn -Dn $entity.Dn
        $type = $leaf.Substring(0, $leaf.IndexOf('='))
        $newDn = '{0}={1},{2}' -f $type, (ConvertTo-StubEscapedRdn -Value $NewName), (Get-StubParentDn -Dn $entity.Dn)
        if ($newDn -cne $entity.Dn){
            Test-StubUnique -State $state -Sam '' -Upn '' -Dn $newDn -ExceptGuid $entity.Guid
            # Child objects keep their place below the renamed object.
            foreach ($child in (Get-StubDescendants -State $state -Dn $entity.Dn)){
                $child.Source.dn = $child.Dn.Substring(0, $child.Dn.Length - $entity.Dn.Length) + $newDn
            }

            $entity.Source.dn = $newDn
        }

        $call.result = 'ok'
    }
    catch{
        $call.result = 'error: ' + $_.Exception.Message
        throw
    }
    finally{
        Save-StubState -State $state
    }

    if ($PassThru){
        New-StubAdObject -Entity (Resolve-StubIdentity -State $state -Identity $entity.Guid -Kinds @('user', 'group', 'object'))
    }
}

function Add-ADGroupMember {
    [CmdletBinding(SupportsShouldProcess)]
    param(
        [Parameter(Mandatory, Position = 0)]$Identity,
        [Parameter(Mandatory, Position = 1)][object[]]$Members,
        [string]$Server,
        [pscredential]$Credential
    )

    $state = Read-StubState
    $call = Add-StubCall -State $state -Cmdlet 'Add-ADGroupMember' -CallArgs (ConvertTo-StubCallArgs -Bound $PSBoundParameters)
    try{
        Invoke-StubFault -State $state -Cmdlet 'Add-ADGroupMember' -Call $call -PlainPassword ''
        $group = Resolve-StubIdentity -State $state -Identity $Identity -Kinds @('group')
        foreach ($member in $Members){
            $entity = Resolve-StubIdentity -State $state -Identity $member -Kinds @('user')
            if ($entity.Source.memberOf | Where-Object { $_ -ieq $group.Dn }){
                throw [ADException]::new('The specified account name is already a member of the group.')
            }

            $entity.Source.memberOf = [string[]]@(@($entity.Source.memberOf) + $group.Dn)
        }

        $call.result = 'ok'
    }
    catch{
        $call.result = 'error: ' + $_.Exception.Message
        throw
    }
    finally{
        Save-StubState -State $state
    }
}

function Remove-ADGroupMember {
    [CmdletBinding(SupportsShouldProcess, ConfirmImpact = 'High')]
    param(
        [Parameter(Mandatory, Position = 0)]$Identity,
        [Parameter(Mandatory, Position = 1)][object[]]$Members,
        [string]$Server,
        [pscredential]$Credential
    )

    $state = Read-StubState
    $call = Add-StubCall -State $state -Cmdlet 'Remove-ADGroupMember' -CallArgs (ConvertTo-StubCallArgs -Bound $PSBoundParameters)
    try{
        Invoke-StubFault -State $state -Cmdlet 'Remove-ADGroupMember' -Call $call -PlainPassword ''
        $group = Resolve-StubIdentity -State $state -Identity $Identity -Kinds @('group')
        foreach ($member in $Members){
            $entity = Resolve-StubIdentity -State $state -Identity $member -Kinds @('user')
            if (-not ($entity.Source.memberOf | Where-Object { $_ -ieq $group.Dn })){
                throw [ADException]::new('The specified account name is not a member of the group.')
            }

            if ($PSCmdlet.ShouldProcess($group.Dn, 'Remove member')){
                $entity.Source.memberOf = [string[]]@(@($entity.Source.memberOf) | Where-Object { $_ -ine $group.Dn })
            }
        }

        $call.result = 'ok'
    }
    catch{
        $call.result = 'error: ' + $_.Exception.Message
        throw
    }
    finally{
        Save-StubState -State $state
    }
}

#endregion

Export-ModuleMember -Function @(
    'Get-ADRootDSE', 'Get-ADUser', 'Get-ADGroup', 'Get-ADObject',
    'New-ADUser', 'Set-ADUser', 'Disable-ADAccount', 'Enable-ADAccount', 'Remove-ADUser',
    'Remove-ADObject', 'Move-ADObject', 'Rename-ADObject', 'Add-ADGroupMember', 'Remove-ADGroupMember'
)
