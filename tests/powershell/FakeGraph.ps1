<#
.SYNOPSIS
    FAKE Microsoft identity platform token endpoint + Microsoft Graph (v1.0 subset) used ONLY by the IDLifecycle
    Pester tests of the cloud scripts. Listens on 127.0.0.1:<random port> (plain TCP, no HTTP.sys reservation needed).

.DESCRIPTION
    Started by CloudTestHelpers.ps1 as a background pwsh. Writes its port to -PortFile, then serves one request per
    connection until -ParentPid exits or -StopFile appears. Every request re-reads the JSON state file and writes it
    back (calls appended, memberships changed), so tests can swap the state between script runs.

    State (property names case-insensitive on read):
    {
      "tenant": "<tenant id>", "clientId": "<client id>",
      "certPath": "<public .cer the assertion must be signed with>" | null,  "secret": "<client secret>" | null,
      "roles": ["GroupMember.ReadWrite.All", ...],           // roles claim of the issued access token
      "pageSize": 100,                                       // max entries per page (paging tests use 2)
      "groups": [ { "id", "displayName", "description", "mail", "mailEnabled", "securityEnabled", "groupTypes": [],
                    "isAssignableToRole", "onPremisesSyncEnabled", "members": ["<userId>"], "memberGroups": ["<groupId>"],
                    "hideRoleFlag": false } ],             // hideRoleFlag: isAssignableToRole is not returned
      "users":  [ { "id", "userPrincipalName", "onPremisesSecurityIdentifier", "onPremisesImmutableId",
                    "accountEnabled", "onPremisesSyncEnabled" } ],
      "faults": [ { "method": "GET", "path": "<regex on the path+query>", "status": 429, "retryAfter": 1, "times": 1 } ],
      "calls":  [ ... appended: {method, path, consistency, authorized, body} ; token calls carry assertion header/claims,
                  assertionValid, usedSecret (never the secret) ]
    }

    Graph behaviour modelled: $search/$filter/$count need "ConsistencyLevel: eventual" (400 otherwise); add member to a
    mail-enabled / dynamic group -> 400, to a role-assignable group -> 403; already a member -> 400 "already exist";
    remove a non-member -> 404; unknown objects -> 404; @odata.nextLink paging.
#>

param(
    [Parameter(Mandatory)][string]$StatePath,
    [Parameter(Mandatory)][string]$PortFile,
    [int]$ParentPid = 0,
    [string]$StopFile = ''
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$utf8 = [System.Text.UTF8Encoding]::new($false)

function Get-Key {
    param($Object, [string]$Name, $Default = $null)
    if ($null -eq $Object){ return $Default }
    if ($Object -is [System.Collections.IDictionary]){
        foreach ($k in $Object.Keys){ if ([string]$k -ieq $Name){ return $Object[$k] } }
        return $Default
    }
    $p = $Object.PSObject.Properties[$Name]
    if ($null -eq $p){ return $Default }
    return $p.Value
}

function ConvertTo-B64Url([byte[]]$Bytes){ [Convert]::ToBase64String($Bytes).TrimEnd('=').Replace('+', '-').Replace('/', '_') }
function ConvertFrom-B64Url([string]$Text){
    $b = $Text.Replace('-', '+').Replace('_', '/')
    switch ($b.Length % 4){ 2 { $b += '==' } 3 { $b += '=' } }
    return [Convert]::FromBase64String($b)
}

function Read-State { ConvertFrom-Json -InputObject ([System.IO.File]::ReadAllText($StatePath, $utf8)) -AsHashtable -Depth 30 }
function Write-State($State){ [System.IO.File]::WriteAllText($StatePath, (ConvertTo-Json -InputObject $State -Depth 30), $utf8) }

function New-Response([int]$Status, $Body = $null, [hashtable]$Headers = @{}){
    return @{ Status = $Status; Body = $Body; Headers = $Headers }
}

function New-GraphError([int]$Status, [string]$Code, [string]$Message){
    return (New-Response $Status @{ error = @{ code = $Code; message = $Message } })
}

function Get-QueryTable([string]$Query){
    $t = @{}
    if ([string]::IsNullOrEmpty($Query)){ return $t }
    foreach ($pair in $Query.Split('&')){
        if ($pair -eq ''){ continue }
        $i = $pair.IndexOf('=')
        $k = (($i -lt 0) ? ($pair) : ($pair.Substring(0, $i)))
        $v = (($i -lt 0) ? ('') : ($pair.Substring($i + 1)))
        $t[[uri]::UnescapeDataString($k)] = [uri]::UnescapeDataString($v.Replace('+', ' '))
    }
    return $t
}

function Select-Props($Object, [string]$Select){
    $out = [ordered]@{}
    $names = (([string]::IsNullOrEmpty($Select)) ? (@($Object.Keys)) : ($Select.Split(',')))
    foreach ($n in $names){
        if ($n -in @('members', 'memberGroups', 'hideRoleFlag')){ continue }
        if (($n -eq 'isAssignableToRole') -and [bool](Get-Key $Object 'hideRoleFlag' $false)){ continue }
        $v = $null
        foreach ($k in @($Object.Keys)){ if ([string]$k -ieq $n){ $v = $Object[$k] } }
        $out[$n] = $v
    }
    return $out
}

function New-Page($Items, $Query, [string]$BaseUrl, [string]$Path, $State){
    $size = [int](Get-Key $State 'pageSize' 100)
    if ($Query.ContainsKey('$top')){ $size = [math]::Min($size, [int]$Query['$top']) }
    $skip = (($Query.ContainsKey('$skiptoken')) ? ([int]$Query['$skiptoken']) : (0))
    $all = @($Items)
    $page = @($all | Select-Object -Skip $skip -First $size)
    $body = [ordered]@{ value = @($page | ForEach-Object { Select-Props $_ $Query['$select'] }) }
    if ($Query.ContainsKey('$count')){ $body['@odata.count'] = $all.Count }
    if (($skip + $size) -lt $all.Count){
        $q = @{}
        foreach ($k in $Query.Keys){ $q[$k] = $Query[$k] }
        $q['$skiptoken'] = [string]($skip + $size)
        $qs = ($q.Keys | Sort-Object | ForEach-Object { '{0}={1}' -f [uri]::EscapeDataString($_), [uri]::EscapeDataString([string]$q[$_]) }) -join '&'
        $body['@odata.nextLink'] = "$BaseUrl/v1.0$Path`?$qs"
    }
    return (New-Response 200 $body)
}

function Get-Group($State, [string]$Id){ @(Get-Key $State 'groups' @()) | Where-Object { ([string](Get-Key $_ 'id')) -ieq $Id } | Select-Object -First 1 }
function Get-User($State, [string]$Id){ @(Get-Key $State 'users' @()) | Where-Object { ([string](Get-Key $_ 'id')) -ieq $Id } | Select-Object -First 1 }

function Get-GroupKindFlags($Group){
    $types = @(Get-Key $Group 'groupTypes' @())
    return @{
        Unified = ($types -contains 'Unified')
        Dynamic = ($types -contains 'DynamicMembership')
        Mail    = [bool](Get-Key $Group 'mailEnabled' $false)
        Role    = [bool](Get-Key $Group 'isAssignableToRole' $false)
        Synced  = ($true -eq (Get-Key $Group 'onPremisesSyncEnabled'))
    }
}

function Get-TransitiveGroups($State, [string]$UserId){
    # Groups the user is in directly, plus every group that (transitively) contains one of those groups.
    $result = [System.Collections.Generic.List[object]]::new()
    $seen = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::OrdinalIgnoreCase)
    $queue = [System.Collections.Generic.Queue[string]]::new()
    foreach ($g in @(Get-Key $State 'groups' @())){
        if (@(Get-Key $g 'members' @()) -contains $UserId){ $queue.Enqueue([string]$g.id) }
    }
    while ($queue.Count -gt 0){
        $id = $queue.Dequeue()
        if (-not $seen.Add($id)){ continue }
        $g = Get-Group $State $id
        if ($null -eq $g){ continue }
        $result.Add($g)
        foreach ($parent in @(Get-Key $State 'groups' @())){
            if (@(Get-Key $parent 'memberGroups' @()) -contains $id){ $queue.Enqueue([string]$parent.id) }
        }
    }
    return , $result.ToArray()
}

function Invoke-Token($State, [string]$Path, [string]$Body, [int]$Port, $Call){
    $form = Get-QueryTable $Body
    $tenant = [string](Get-Key $State 'tenant')
    $Call.grant = $form['grant_type']
    $Call.scope = $form['scope']
    $Call.clientId = $form['client_id']
    $Call.usedSecret = $form.ContainsKey('client_secret')
    $Call.clientAssertionType = $form['client_assertion_type']
    if ($Path -ne "/$tenant/oauth2/v2.0/token"){
        return (New-Response 400 @{ error = 'invalid_request'; error_description = "AADSTS90002: Tenant '$Path' not found." })
    }

    if (($form['grant_type'] -ne 'client_credentials') -or ($form['client_id'] -ne [string](Get-Key $State 'clientId'))){
        return (New-Response 400 @{ error = 'unauthorized_client'; error_description = 'AADSTS700016: Application not found in the directory.' })
    }

    $valid = $false
    $why = ''
    if ($form.ContainsKey('client_secret')){
        $valid = ($form['client_secret'] -ceq [string](Get-Key $State 'secret'))
        $why = 'AADSTS7000215: Invalid client secret provided.'
    }
    elseif ($form['client_assertion_type'] -eq 'urn:ietf:params:oauth:client-assertion-type:jwt-bearer'){
        $parts = ([string]$form['client_assertion']).Split('.')
        if ($parts.Count -eq 3){
            $header = ConvertFrom-Json ($utf8.GetString((ConvertFrom-B64Url $parts[0]))) -AsHashtable
            $claims = ConvertFrom-Json ($utf8.GetString((ConvertFrom-B64Url $parts[1]))) -AsHashtable
            $Call.assertionHeader = $header
            $Call.assertionClaims = $claims
            $certPath = [string](Get-Key $State 'certPath')
            $cert = [System.Security.Cryptography.X509Certificates.X509Certificate2]::new($certPath)
            $rsa = [System.Security.Cryptography.X509Certificates.RSACertificateExtensions]::GetRSAPublicKey($cert)
            $sigOk = $rsa.VerifyData([System.Text.Encoding]::ASCII.GetBytes($parts[0] + '.' + $parts[1]), (ConvertFrom-B64Url $parts[2]),
                [System.Security.Cryptography.HashAlgorithmName]::SHA256, [System.Security.Cryptography.RSASignaturePadding]::Pkcs1)
            $now = [System.DateTimeOffset]::UtcNow.ToUnixTimeSeconds()
            $expectedAud = "http://127.0.0.1:$Port/$tenant/oauth2/v2.0/token"
            $checks = @(
                @($sigOk, 'signature'),
                @(($header.alg -eq 'RS256'), 'alg'),
                @(($header.typ -eq 'JWT'), 'typ'),
                @(($header.x5t -eq (ConvertTo-B64Url $cert.GetCertHash([System.Security.Cryptography.HashAlgorithmName]::SHA1))), 'x5t'),
                @(($claims.aud -eq $expectedAud), 'aud'),
                @(($claims.iss -eq $form['client_id']), 'iss'),
                @(($claims.sub -eq $form['client_id']), 'sub'),
                @((-not [string]::IsNullOrEmpty([string]$claims.jti)), 'jti'),
                @(([long]$claims.nbf -le ($now + 60)), 'nbf'),
                @(([long]$claims.exp -gt $now), 'exp'),
                @((([long]$claims.exp - [long]$claims.nbf) -le 600), 'lifetime')
            )
            $failed = @($checks | Where-Object { -not $_[0] } | ForEach-Object { $_[1] })
            $valid = ($failed.Count -eq 0)
            $why = 'AADSTS700027: Client assertion failed validation: ' + ($failed -join ',')
        }
        else{
            $why = 'AADSTS50027: Invalid JWT token.'
        }
    }

    $Call.assertionValid = $valid
    if (-not $valid){
        return (New-Response 401 @{ error = 'invalid_client'; error_description = $why })
    }

    $payload = @{ aud = 'https://graph.microsoft.com'; tid = $tenant; appid = $form['client_id']; roles = @(Get-Key $State 'roles' @()); fake = 'idl' }
    $token = 'eyJhbGciOiJub25lIn0.' + (ConvertTo-B64Url $utf8.GetBytes((ConvertTo-Json $payload -Compress))) + '.fakesig'
    return (New-Response 200 @{ token_type = 'Bearer'; expires_in = 3599; access_token = $token })
}

function Invoke-Graph($State, [string]$Method, [string]$Path, $Query, [string]$Body, [bool]$Eventual, [string]$BaseUrl){
    $advanced = ($Query.ContainsKey('$search') -or $Query.ContainsKey('$filter') -or $Query.ContainsKey('$count'))
    if ($advanced -and -not $Eventual){
        return (New-GraphError 400 'Request_UnsupportedQuery' 'Request with $search/$filter/$count requires ConsistencyLevel: eventual.')
    }

    $groups = @(Get-Key $State 'groups' @())
    $users = @(Get-Key $State 'users' @())

    if (($Method -eq 'GET') -and ($Path -eq '/groups')){
        $items = $groups
        if ($Query.ContainsKey('$search')){
            $terms = @([regex]::Matches($Query['$search'], '"(displayName|mail):((?:\\.|[^"\\])*)"') | ForEach-Object { @{ Field = $_.Groups[1].Value; Term = ($_.Groups[2].Value -replace '\\(.)', '$1') } })
            $items = @($groups | Where-Object {
                    $g = $_
                    @($terms | Where-Object { ([string](Get-Key $g $_.Field)).IndexOf($_.Term, [System.StringComparison]::OrdinalIgnoreCase) -ge 0 }).Count -gt 0
                })
        }
        return (New-Page $items $Query $BaseUrl $Path $State)
    }

    if (($Method -eq 'GET') -and ($Path -eq '/users')){
        $items = $users
        if ($Query.ContainsKey('$filter')){
            if ($Query['$filter'] -ne 'onPremisesSyncEnabled eq true'){ return (New-GraphError 400 'Request_UnsupportedQuery' 'unsupported filter') }
            $items = @($users | Where-Object { $true -eq (Get-Key $_ 'onPremisesSyncEnabled') })
        }
        return (New-Page $items $Query $BaseUrl $Path $State)
    }

    if ($Path -match '^/groups/([^/]+)$' -and $Method -eq 'GET'){
        $g = Get-Group $State $Matches[1]
        if ($null -eq $g){ return (New-GraphError 404 'Request_ResourceNotFound' "Resource '$($Matches[1])' does not exist.") }
        return (New-Response 200 (Select-Props $g $Query['$select']))
    }

    if ($Path -match '^/groups/([^/]+)/members/microsoft\.graph\.user$' -and $Method -eq 'GET'){
        $g = Get-Group $State $Matches[1]
        if ($null -eq $g){ return (New-GraphError 404 'Request_ResourceNotFound' 'group not found') }
        $members = @(@(Get-Key $g 'members' @()) | ForEach-Object { $u = Get-User $State $_; (($null -ne $u) ? ($u) : (@{ id = $_ })) })
        if ($Query.ContainsKey('$filter')){
            if ($Query['$filter'] -notmatch "^id eq '([0-9a-fA-F-]{36})'$"){ return (New-GraphError 400 'Request_UnsupportedQuery' 'unsupported filter') }
            $want = $Matches[1]
            $members = @($members | Where-Object { [string]$_.id -ieq $want })
        }
        return (New-Page $members $Query $BaseUrl $Path $State)
    }

    if ($Path -match '^/groups/([^/]+)/members/\$ref$' -and $Method -eq 'POST'){
        $g = Get-Group $State $Matches[1]
        if ($null -eq $g){ return (New-GraphError 404 'Request_ResourceNotFound' 'group not found') }
        $ref = [string](Get-Key (ConvertFrom-Json $Body -AsHashtable) '@odata.id')
        if ($ref -notmatch '/v1\.0/directoryObjects/([0-9a-fA-F-]{36})$'){ return (New-GraphError 400 'Request_BadRequest' 'Invalid @odata.id') }
        $uid = $Matches[1].ToLowerInvariant()
        if ($null -eq (Get-User $State $uid)){ return (New-GraphError 404 'Request_ResourceNotFound' "Resource '$uid' does not exist.") }
        $f = Get-GroupKindFlags $g
        if ($f.Role){ return (New-GraphError 403 'Authorization_RequestDenied' 'Insufficient privileges to complete the operation.') }
        if ($f.Synced -or $f.Dynamic -or ($f.Mail -and -not $f.Unified)){ return (New-GraphError 400 'Request_BadRequest' 'Cannot Update a mail-enabled security groups and or distribution list.') }
        if (@(Get-Key $g 'members' @()) -contains $uid){ return (New-GraphError 400 'Request_BadRequest' "One or more added object references already exist for the following modified properties: 'members'.") }
        $g['members'] = @(@(Get-Key $g 'members' @()) + $uid)
        return (New-Response 204)
    }

    if ($Path -match '^/groups/([^/]+)/members/([^/]+)/\$ref$' -and $Method -eq 'DELETE'){
        $g = Get-Group $State $Matches[1]
        $uid = $Matches[2].ToLowerInvariant()
        if ($null -eq $g){ return (New-GraphError 404 'Request_ResourceNotFound' 'group not found') }
        $f = Get-GroupKindFlags $g
        if ($f.Role){ return (New-GraphError 403 'Authorization_RequestDenied' 'Insufficient privileges to complete the operation.') }
        if ($f.Synced -or $f.Dynamic -or ($f.Mail -and -not $f.Unified)){ return (New-GraphError 400 'Request_BadRequest' 'Cannot Update a mail-enabled security groups and or distribution list.') }
        if (@(Get-Key $g 'members' @()) -notcontains $uid){ return (New-GraphError 404 'Request_ResourceNotFound' "Resource '$uid' does not exist or one of its queried reference-property objects are not present.") }
        $g['members'] = @(@(Get-Key $g 'members' @()) | Where-Object { $_ -ne $uid })
        return (New-Response 204)
    }

    if ($Path -match '^/users/([^/]+)$' -and $Method -eq 'GET'){
        $u = Get-User $State $Matches[1]
        if ($null -eq $u){ return (New-GraphError 404 'Request_ResourceNotFound' "Resource '$($Matches[1])' does not exist.") }
        return (New-Response 200 (Select-Props $u $Query['$select']))
    }

    if ($Path -match '^/users/([^/]+)/transitiveMemberOf/microsoft\.graph\.group$' -and $Method -eq 'GET'){
        $u = Get-User $State $Matches[1]
        if ($null -eq $u){ return (New-GraphError 404 'Request_ResourceNotFound' 'user not found') }
        return (New-Page (Get-TransitiveGroups $State ([string]$u.id)) $Query $BaseUrl $Path $State)
    }

    return (New-GraphError 400 'BadRequest' "Fake Graph does not implement $Method $Path")
}

function Send-Response($Stream, $Response){
    $reasons = @{ 200 = 'OK'; 204 = 'No Content'; 400 = 'Bad Request'; 401 = 'Unauthorized'; 403 = 'Forbidden'; 404 = 'Not Found'; 429 = 'Too Many Requests'; 500 = 'Internal Server Error'; 503 = 'Service Unavailable' }
    $text = (($null -eq $Response.Body) ? ('') : (ConvertTo-Json -InputObject $Response.Body -Depth 30 -Compress))
    $bytes = $utf8.GetBytes($text)
    $head = "HTTP/1.1 $($Response.Status) $($reasons[$Response.Status])`r`n"
    if ($bytes.Length -gt 0){ $head += "Content-Type: application/json`r`n" }
    foreach ($k in $Response.Headers.Keys){ $head += "${k}: $($Response.Headers[$k])`r`n" }
    $head += "Content-Length: $($bytes.Length)`r`nConnection: close`r`n`r`n"
    $hb = [System.Text.Encoding]::ASCII.GetBytes($head)
    $Stream.Write($hb, 0, $hb.Length)
    if ($bytes.Length -gt 0){ $Stream.Write($bytes, 0, $bytes.Length) }
    $Stream.Flush()
}

function Read-Request($Stream){
    # Request line + headers up to CRLFCRLF, then Content-Length bytes.
    $buffer = [System.IO.MemoryStream]::new()
    $one = [byte[]]::new(1)
    while ($true){
        $n = $Stream.Read($one, 0, 1)
        if ($n -le 0){ return $null }
        $buffer.WriteByte($one[0])
        $len = $buffer.Length
        if ($len -ge 4){
            $b = $buffer.GetBuffer()
            if (($b[$len - 4] -eq 13) -and ($b[$len - 3] -eq 10) -and ($b[$len - 2] -eq 13) -and ($b[$len - 1] -eq 10)){ break }
        }
    }
    $headText = [System.Text.Encoding]::ASCII.GetString($buffer.ToArray())
    $lines = $headText -split "`r`n"
    $first = $lines[0].Split(' ')
    $headers = @{}
    foreach ($l in ($lines | Select-Object -Skip 1)){
        $i = $l.IndexOf(':')
        if ($i -gt 0){ $headers[$l.Substring(0, $i).Trim().ToLowerInvariant()] = $l.Substring($i + 1).Trim() }
    }
    $body = ''
    if ($headers.ContainsKey('content-length')){
        $total = [int]$headers['content-length']
        $data = [byte[]]::new($total)
        $read = 0
        while ($read -lt $total){
            $n = $Stream.Read($data, $read, $total - $read)
            if ($n -le 0){ break }
            $read += $n
        }
        $body = $utf8.GetString($data, 0, $read)
    }
    return @{ Method = $first[0]; Target = $first[1]; Headers = $headers; Body = $body }
}

$listener = [System.Net.Sockets.TcpListener]::new([System.Net.IPAddress]::Loopback, 0)
$listener.Start()
$port = ([System.Net.IPEndPoint]$listener.LocalEndpoint).Port
$baseUrl = "http://127.0.0.1:$port"
[System.IO.File]::WriteAllText($PortFile, [string]$port, $utf8)

$pending = $listener.AcceptTcpClientAsync()
try{
    while ($true){
        if ($StopFile -and (Test-Path -LiteralPath $StopFile)){ break }
        if (($ParentPid -gt 0) -and ($null -eq (Get-Process -Id $ParentPid -ErrorAction SilentlyContinue))){ break }
        if (-not $pending.Wait(250)){ continue }
        $client = $pending.Result
        $pending = $listener.AcceptTcpClientAsync()
        try{
            $client.ReceiveTimeout = 10000
            $stream = $client.GetStream()
            $req = Read-Request $stream
            if ($null -eq $req){ continue }
            $state = Read-State
            $split = $req.Target.Split('?', 2)
            $path = $split[0]
            $query = Get-QueryTable ((($split.Count -gt 1)) ? ($split[1]) : (''))
            $call = [ordered]@{ method = $req.Method; path = $req.Target; consistency = ($req.Headers['consistencylevel'] -eq 'eventual'); authorized = $false; body = $null }

            $response = $null
            foreach ($fault in @(Get-Key $state 'faults' @())){
                if (([int](Get-Key $fault 'times' 0) -gt 0) -and ($req.Method -eq [string](Get-Key $fault 'method' $req.Method)) -and ($req.Target -match [string](Get-Key $fault 'path' '.'))){
                    $fault['times'] = [int]$fault['times'] - 1
                    $h = @{}
                    if ($null -ne (Get-Key $fault 'retryAfter')){ $h['Retry-After'] = [string]$fault['retryAfter'] }
                    $response = New-Response ([int]$fault['status']) @{ error = @{ code = 'Fault'; message = "injected HTTP $($fault['status'])" } } $h
                    $call.fault = $true
                    break
                }
            }

            if ($null -eq $response){
                if ($path -match '/oauth2/v2\.0/token$'){
                    $response = Invoke-Token $state $path $req.Body $port $call
                }
                elseif ($path.StartsWith('/v1.0/')){
                    $auth = [string]$req.Headers['authorization']
                    $tokenOk = $false
                    if ($auth -match '^Bearer ([^.]+)\.([^.]+)\.fakesig$'){
                        $claims = ConvertFrom-Json ($utf8.GetString((ConvertFrom-B64Url $Matches[2]))) -AsHashtable
                        $tokenOk = ($claims.fake -eq 'idl')
                    }
                    $call.authorized = $tokenOk
                    if (-not $tokenOk){
                        $response = New-GraphError 401 'InvalidAuthenticationToken' 'Access token is empty or invalid.'
                    }
                    else{
                        if ($req.Body){ $call.body = ConvertFrom-Json $req.Body -AsHashtable }
                        $response = Invoke-Graph $state $req.Method $path.Substring(5) $query $req.Body $call.consistency $baseUrl
                    }
                }
                else{
                    $response = New-GraphError 404 'NotFound' "no route $path"
                }
            }

            $call.status = $response.Status
            if ($null -eq $state['calls']){ $state['calls'] = @() }
            $state['calls'] = @(@($state['calls']) + $call)
            Write-State $state
            Send-Response $stream $response
        }
        catch{
            try{ Send-Response $stream (New-GraphError 500 'FakeError' $_.Exception.Message) } catch{ }
            [System.IO.File]::AppendAllText("$StatePath.errors.log", "$($_.Exception.Message) $($_.ScriptStackTrace)`r`n", $utf8)
        }
        finally{
            $client.Dispose()
        }
    }
}
finally{
    $listener.Stop()
}
