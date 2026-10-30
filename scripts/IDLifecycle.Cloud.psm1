<#
.SYNOPSIS
    Shared helpers for the IDLifecycle cloud scripts (Microsoft Graph and Exchange Online): app-only tokens, Graph
    requests with paging and throttling, group kinds and the Exchange Online connection.

.DESCRIPTION
    Imported by the cloud scripts after IDLifecycle.Common.psm1:
        Import-Module (Join-Path $PSScriptRoot 'IDLifecycle.Common.psm1') -Force
        Import-Module (Join-Path $PSScriptRoot 'IDLifecycle.Cloud.psm1') -Force

    No Microsoft Graph SDK is needed: Graph is called over HTTPS with an app-only token from the client-credentials
    flow. The app authenticates with a certificate (a signed JWT client assertion; the private key never leaves the
    Windows certificate store) or with a client secret. Exchange Online is reached with the ExchangeOnlineManagement
    module (3.x), which only accepts certificates.

    Every request carries the connection in a "cloud" block (stdin only, never argv):
      {"cloud":{"environment":"global|usgov|usgovdod|china","tenantId":"...","clientId":"...",
                "auth":"certificate|secret","certThumbprint":"...","clientSecret":"...",
                "exchange":{"enabled":false,"organization":"contoso.onmicrosoft.com"}}}

    The client secret, the client assertion and the access token are registered as secrets, so they are scrubbed from
    every log line, report cell, error and response.

    Tests only: with IDL_CLOUD_TEST=1 the login and Graph base URLs can be pointed at a loopback fake with
    IDL_CLOUD_TEST_LOGIN_URL / IDL_CLOUD_TEST_GRAPH_URL (http://127.0.0.1:<port> or http://localhost:<port> only).

.NOTES
    Requires PowerShell 7+.
#>

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

Import-Module (Join-Path $PSScriptRoot 'IDLifecycle.Common.psm1')

# Properties read for every group: enough to tell its kind and whether it may be changed.
$script:IDLGroupSelect = 'id,displayName,description,mail,mailEnabled,securityEnabled,groupTypes,isAssignableToRole,onPremisesSyncEnabled'
$script:IDLHttp = $null
$script:IDLToken = $null
$script:IDLExchangeConnected = $false
$script:IDLRetryLimit = 5
$script:IDLRetryCapSeconds = 60

#region Configuration

function Test-IDLCloudGuid {
    [CmdletBinding()]
    param(
        [AllowNull()][AllowEmptyString()][string]$Value
    )

    if ([string]::IsNullOrWhiteSpace($Value)){
        return $false
    }

    return ($Value -match '^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$')
}

function Get-IDLCloudList {
    <#
    .SYNOPSIS
        A JSON array field as a flat object[] (empty when missing or null). Use it as $x = Get-IDLCloudList ...;
        Get-IDLField returns arrays wrapped, so @(Get-IDLField ...) would nest them.
    #>
    [CmdletBinding()]
    param(
        [AllowNull()]$Object,
        [Parameter(Mandatory)][string]$Name
    )

    $value = Get-IDLField -Object $Object -Name $Name
    if ($null -eq $value){
        return , [object[]]@()
    }

    return , [object[]]@($value)
}

function Get-IDLCloudGuidList {
    <#
    .SYNOPSIS
        Reads a JSON array of object ids as lower-case GUID strings (de-duplicated); throws on a value that is not a GUID.
    #>
    [CmdletBinding()]
    param(
        [AllowNull()]$Value,
        [Parameter(Mandatory)][string]$Name
    )

    $list = [System.Collections.Generic.List[string]]::new()
    foreach ($entry in (Get-IDLStringList -Value $Value)){
        if (-not (Test-IDLCloudGuid -Value $entry)){
            throw (New-IDLException -Kind Input -Message "$Name contains '$entry', which is not an object id (GUID).")
        }

        $id = $entry.ToLowerInvariant()
        if (-not $list.Contains($id)){
            $list.Add($id)
        }
    }

    return , [string[]]$list.ToArray()
}

function Get-IDLCloudEndpoints {
    <#
    .SYNOPSIS
        Login, Graph and Exchange Online environment names for a cloud environment.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][ValidateSet('global', 'usgov', 'usgovdod', 'china')][string]$Environment
    )

    $endpoints = switch ($Environment){
        'global' { @{ Login = 'https://login.microsoftonline.com'; Graph = 'https://graph.microsoft.com'; Exchange = 'O365Default' } }
        'usgov' { @{ Login = 'https://login.microsoftonline.us'; Graph = 'https://graph.microsoft.us'; Exchange = 'O365USGovGCCHigh' } }
        'usgovdod' { @{ Login = 'https://login.microsoftonline.us'; Graph = 'https://dod-graph.microsoft.us'; Exchange = 'O365USGovDoD' } }
        'china' { @{ Login = 'https://login.chinacloudapi.cn'; Graph = 'https://microsoftgraph.chinacloudapi.cn'; Exchange = 'O365China' } }
    }

    # The scope is always the real Graph resource; only the address requests go to can be redirected (tests).
    $endpoints.Scope = $endpoints.Graph + '/.default'
    if ($env:IDL_CLOUD_TEST -eq '1'){
        foreach ($pair in @(@('Login', 'IDL_CLOUD_TEST_LOGIN_URL'), @('Graph', 'IDL_CLOUD_TEST_GRAPH_URL'))){
            $override = [System.Environment]::GetEnvironmentVariable($pair[1])
            if ([string]::IsNullOrEmpty($override)){
                continue
            }

            if ($override -notmatch '^http://(127\.0\.0\.1|localhost):\d{1,5}$'){
                throw (New-IDLException -Kind Input -Message "$($pair[1]) must be http://127.0.0.1:<port> or http://localhost:<port>.")
            }

            $endpoints[$pair[0]] = $override
        }
    }

    return $endpoints
}

function Get-IDLCloudConfig {
    <#
    .SYNOPSIS
        Validates the "cloud" block of a request and returns it (secrets registered for scrubbing first).
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]$Request
    )

    $cloud = Get-IDLField -Object $Request -Name 'cloud'
    if ($cloud -isnot [System.Collections.IDictionary]){
        throw (New-IDLException -Kind Input -Message 'cloud is missing or not an object.')
    }

    $secret = ConvertTo-IDLString -Value (Get-IDLField -Object $cloud -Name 'clientSecret')
    Register-IDLSecret -Secret $secret

    $environment = ConvertTo-IDLString -Value (Get-IDLField -Object $cloud -Name 'environment')
    $environment = (([string]::IsNullOrWhiteSpace($environment)) ? ('global') : ($environment.Trim().ToLowerInvariant()))
    if ($environment -notin @('global', 'usgov', 'usgovdod', 'china')){
        throw (New-IDLException -Kind Input -Message 'cloud.environment must be global, usgov, usgovdod or china.')
    }

    $tenantId = ConvertTo-IDLString -Value (Get-IDLField -Object $cloud -Name 'tenantId')
    $tenantId = (($null -eq $tenantId) ? ('') : ($tenantId.Trim()))
    if (
        (-not (Test-IDLCloudGuid -Value $tenantId)) -and
        ($tenantId -notmatch '^(?=.{4,253}$)([A-Za-z0-9]([A-Za-z0-9\-]{0,61}[A-Za-z0-9])?\.)+[A-Za-z]{2,63}$')
    ){
        throw (New-IDLException -Kind Input -Message 'cloud.tenantId must be the tenant id (GUID) or a verified domain name.')
    }

    $clientId = ConvertTo-IDLString -Value (Get-IDLField -Object $cloud -Name 'clientId')
    $clientId = (($null -eq $clientId) ? ('') : ($clientId.Trim()))
    if (-not (Test-IDLCloudGuid -Value $clientId)){
        throw (New-IDLException -Kind Input -Message 'cloud.clientId must be the application (client) id, a GUID.')
    }

    $auth = ConvertTo-IDLString -Value (Get-IDLField -Object $cloud -Name 'auth')
    $auth = (([string]::IsNullOrWhiteSpace($auth)) ? ('certificate') : ($auth.Trim().ToLowerInvariant()))
    $thumbprint = ''
    switch ($auth){
        'certificate' {
            $thumbprint = ConvertTo-IDLString -Value (Get-IDLField -Object $cloud -Name 'certThumbprint')
            $thumbprint = (($null -eq $thumbprint) ? ('') : (($thumbprint -replace '[\s:]', '').ToUpperInvariant()))
            if ($thumbprint -notmatch '^[0-9A-F]{40}$'){
                throw (New-IDLException -Kind Input -Message 'cloud.certThumbprint must be the 40 hex digit thumbprint of the certificate.')
            }

            $secret = ''
        }
        'secret' {
            if ([string]::IsNullOrEmpty($secret)){
                throw (New-IDLException -Kind Input -Message 'cloud.clientSecret is required when cloud.auth is "secret".')
            }
        }
        default {
            throw (New-IDLException -Kind Input -Message 'cloud.auth must be "certificate" or "secret".')
        }
    }

    $exchange = Get-IDLField -Object $cloud -Name 'exchange'
    $exchangeEnabled = $false
    $organization = ''
    if ($exchange -is [System.Collections.IDictionary]){
        $exchangeEnabled = [bool](Get-IDLField -Object $exchange -Name 'enabled' -Default $false)
        $organization = ConvertTo-IDLString -Value (Get-IDLField -Object $exchange -Name 'organization')
        $organization = (($null -eq $organization) ? ('') : ($organization.Trim()))
    }

    if ($exchangeEnabled){
        if ($auth -ne 'certificate'){
            throw (New-IDLException -Kind Input -Message 'Exchange Online needs certificate authentication (cloud.auth "certificate"); it does not accept client secrets.')
        }

        if ($organization -notmatch '^(?=.{4,253}$)([A-Za-z0-9]([A-Za-z0-9\-]{0,61}[A-Za-z0-9])?\.)+[A-Za-z]{2,63}$'){
            throw (New-IDLException -Kind Input -Message 'cloud.exchange.organization must be the tenant''s initial domain, e.g. contoso.onmicrosoft.com.')
        }
    }

    return @{
        Environment     = $environment
        Endpoints       = (Get-IDLCloudEndpoints -Environment $environment)
        TenantId        = $tenantId
        ClientId        = $clientId.ToLowerInvariant()
        Auth            = $auth
        CertThumbprint  = $thumbprint
        ClientSecret    = $secret
        ExchangeEnabled = $exchangeEnabled
        Organization    = $organization
    }
}

function Get-IDLCloudConfigText {
    # One log line describing the connection (no secrets).
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]$Config
    )

    $credential = (($Config.Auth -eq 'certificate') ? ("certificate $($Config.CertThumbprint)") : ('client secret'))
    $exchange = (($Config.ExchangeEnabled) ? ("on ($($Config.Organization))") : ('off'))
    return "environment=$($Config.Environment) tenant=$($Config.TenantId) client=$($Config.ClientId) auth=$credential exchange=$exchange"
}

#endregion

#region Certificates and the client assertion

function Get-IDLCloudCertificate {
    <#
    .SYNOPSIS
        Finds the certificate by thumbprint in LocalMachine\My, then CurrentUser\My. Throws (dependency) when it is
        missing or has no private key the current account can use.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Thumbprint
    )

    foreach ($location in @('LocalMachine', 'CurrentUser')){
        $store = [System.Security.Cryptography.X509Certificates.X509Store]::new('My', $location)
        try{
            $store.Open([System.Security.Cryptography.X509Certificates.OpenFlags]::ReadOnly -bor [System.Security.Cryptography.X509Certificates.OpenFlags]::OpenExistingOnly)
            $found = $store.Certificates.Find([System.Security.Cryptography.X509Certificates.X509FindType]::FindByThumbprint, $Thumbprint, $false)
            if ($found.Count -gt 0){
                $cert = $found[0]
                if (-not $cert.HasPrivateKey){
                    throw (New-IDLException -Kind Dependency -Message "Certificate $Thumbprint ($location\My) has no private key.")
                }

                return $cert
            }
        }
        catch [System.Security.Cryptography.CryptographicException]{
            Write-IDLLog "Could not open the $location\My certificate store: $($_.Exception.Message)" 'DEBUG'
        }
        finally{
            $store.Dispose()
        }
    }

    throw (New-IDLException -Kind Dependency -Message "Certificate $Thumbprint was not found in LocalMachine\My or CurrentUser\My of the account running IDLifecycle.")
}

function Get-IDLCloudKeyInfo {
    <#
    .SYNOPSIS
        Describes a certificate's private key: provider name, whether it is a legacy CryptoAPI (CSP) key - which Exchange
        Online app-only authentication requires - and the key container's file name.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][System.Security.Cryptography.X509Certificates.X509Certificate2]$Certificate
    )

    $rsa = [System.Security.Cryptography.X509Certificates.RSACertificateExtensions]::GetRSAPrivateKey($Certificate)
    if ($null -eq $rsa){
        throw (New-IDLException -Kind Dependency -Message "Certificate $($Certificate.Thumbprint) does not have an RSA private key.")
    }

    try{
        $provider = ''
        $unique = ''
        if ($rsa -is [System.Security.Cryptography.RSACng]){
            $provider = [string]$rsa.Key.Provider
            $unique = [string]$rsa.Key.UniqueName
        }
        elseif ($rsa -is [System.Security.Cryptography.RSACryptoServiceProvider]){
            $provider = [string]$rsa.CspKeyContainerInfo.ProviderName
            $unique = [string]$rsa.CspKeyContainerInfo.UniqueKeyContainerName
        }

        # CryptoAPI providers are named "... Cryptographic Provider"; CNG key storage providers "... Key Storage Provider" /
        # "Microsoft Platform Crypto Provider" (TPM).
        return @{
            Provider  = $provider
            IsCsp     = ($provider -match 'Cryptographic (Service )?Provider$')
            Container = $unique
        }
    }
    finally{
        $rsa.Dispose()
    }
}

function ConvertTo-IDLBase64Url {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][byte[]]$Bytes
    )

    return [System.Convert]::ToBase64String($Bytes).TrimEnd('=').Replace('+', '-').Replace('/', '_')
}

function ConvertFrom-IDLBase64Url {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Text
    )

    $b64 = $Text.Replace('-', '+').Replace('_', '/')
    switch ($b64.Length % 4){
        2 { $b64 += '==' }
        3 { $b64 += '=' }
    }

    return [System.Convert]::FromBase64String($b64)
}

function New-IDLClientAssertion {
    <#
    .SYNOPSIS
        Builds the signed JWT client assertion for the client-credentials flow (Microsoft identity platform
        "certificate credentials"): header {alg RS256, typ JWT, x5t = base64url SHA-1 hash, x5t#S256 = base64url
        SHA-256 hash}; claims aud (token endpoint), iss = sub = client id, jti, nbf, iat, exp (10 minutes);
        signed RSASSA-PKCS1-v1_5 with SHA-256.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][System.Security.Cryptography.X509Certificates.X509Certificate2]$Certificate,
        [Parameter(Mandatory)][string]$ClientId,
        [Parameter(Mandatory)][string]$Audience,
        [datetime]$Now = [datetime]::UtcNow
    )

    $sha1 = $Certificate.GetCertHash([System.Security.Cryptography.HashAlgorithmName]::SHA1)
    $sha256 = $Certificate.GetCertHash([System.Security.Cryptography.HashAlgorithmName]::SHA256)
    $header = [ordered]@{
        alg        = 'RS256'
        typ        = 'JWT'
        x5t        = (ConvertTo-IDLBase64Url -Bytes $sha1)
        'x5t#S256' = (ConvertTo-IDLBase64Url -Bytes $sha256)
    }

    $epoch = [System.DateTimeOffset]::new($Now.ToUniversalTime()).ToUnixTimeSeconds()
    $claims = [ordered]@{
        aud = $Audience
        iss = $ClientId
        sub = $ClientId
        jti = ([guid]::NewGuid()).ToString()
        nbf = $epoch
        iat = $epoch
        exp = ($epoch + 600)
    }

    $encode = {
        param($object)
        ConvertTo-IDLBase64Url -Bytes ([System.Text.Encoding]::UTF8.GetBytes((ConvertTo-Json -InputObject $object -Compress)))
    }

    $unsigned = (& $encode $header) + '.' + (& $encode $claims)
    $rsa = [System.Security.Cryptography.X509Certificates.RSACertificateExtensions]::GetRSAPrivateKey($Certificate)
    if ($null -eq $rsa){
        throw (New-IDLException -Kind Dependency -Message "Certificate $($Certificate.Thumbprint) does not have an RSA private key this account can use.")
    }

    try{
        $signature = $rsa.SignData([System.Text.Encoding]::ASCII.GetBytes($unsigned), [System.Security.Cryptography.HashAlgorithmName]::SHA256, [System.Security.Cryptography.RSASignaturePadding]::Pkcs1)
    }
    catch{
        throw (New-IDLException -Kind Dependency -Message "Could not sign with the private key of certificate $($Certificate.Thumbprint) (does the service account have Read on the key?): $($_.Exception.Message)" -InnerException $_.Exception)
    }
    finally{
        $rsa.Dispose()
    }

    $assertion = $unsigned + '.' + (ConvertTo-IDLBase64Url -Bytes $signature)
    Register-IDLSecret -Secret $assertion
    return $assertion
}

function ConvertFrom-IDLJwtPayload {
    <#
    .SYNOPSIS
        Decodes the claims of a JWT (no signature check) as a hashtable; $null when it is not a JWT.
    #>
    [CmdletBinding()]
    param(
        [AllowNull()][AllowEmptyString()][string]$Token
    )

    $parts = (($null -eq $Token) ? (@()) : ($Token.Split('.')))
    if ($parts.Count -ne 3){
        return $null
    }

    try{
        $json = [System.Text.Encoding]::UTF8.GetString((ConvertFrom-IDLBase64Url -Text $parts[1]))
        return (ConvertFrom-Json -InputObject $json -AsHashtable)
    }
    catch{
        return $null
    }
}

#endregion

#region HTTP and Graph

function Invoke-IDLCloudHttp {
    # One HTTP exchange; returns {Status; Body; RetryAfter}. Transport failures throw (connection kind).
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Method,
        [Parameter(Mandatory)][string]$Uri,
        [hashtable]$Headers = @{},
        [AllowNull()][string]$Body = $null,
        [string]$ContentType = 'application/json'
    )

    if ($null -eq $script:IDLHttp){
        $script:IDLHttp = [System.Net.Http.HttpClient]::new()
        $script:IDLHttp.Timeout = [timespan]::FromSeconds(100)
    }

    $message = [System.Net.Http.HttpRequestMessage]::new([System.Net.Http.HttpMethod]::new($Method), $Uri)
    try{
        foreach ($key in $Headers.Keys){
            $null = $message.Headers.TryAddWithoutValidation([string]$key, [string]$Headers[$key])
        }

        if ($null -ne $Body){
            $message.Content = [System.Net.Http.StringContent]::new($Body, [System.Text.Encoding]::UTF8, $ContentType)
        }

        try{
            $response = $script:IDLHttp.SendAsync($message).GetAwaiter().GetResult()
        }
        catch{
            $inner = $_.Exception
            while ($null -ne $inner.InnerException){
                $inner = $inner.InnerException
            }

            throw (New-IDLException -Kind Connection -Message "Could not connect to $(([uri]$Uri).Host): $($inner.Message)" -InnerException $_.Exception)
        }

        try{
            $text = $response.Content.ReadAsStringAsync().GetAwaiter().GetResult()
            $retryAfter = $null
            if ($null -ne $response.Headers.RetryAfter){
                if ($null -ne $response.Headers.RetryAfter.Delta){
                    $retryAfter = [int][math]::Ceiling($response.Headers.RetryAfter.Delta.TotalSeconds)
                }
                elseif ($null -ne $response.Headers.RetryAfter.Date){
                    $retryAfter = [int][math]::Max(0, [math]::Ceiling(($response.Headers.RetryAfter.Date - [System.DateTimeOffset]::UtcNow).TotalSeconds))
                }
            }

            return @{
                Status     = [int]$response.StatusCode
                Body       = $text
                RetryAfter = $retryAfter
            }
        }
        finally{
            $response.Dispose()
        }
    }
    finally{
        $message.Dispose()
    }
}

function ConvertFrom-IDLJsonBody {
    [CmdletBinding()]
    param(
        [AllowNull()][AllowEmptyString()][string]$Text
    )

    if ([string]::IsNullOrWhiteSpace($Text)){
        return $null
    }

    try{
        return (ConvertFrom-Json -InputObject $Text -AsHashtable -Depth 20)
    }
    catch{
        return $null
    }
}

function Get-IDLCloudToken {
    <#
    .SYNOPSIS
        Returns an app-only access token for Microsoft Graph (client credentials; cached for this process).
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]$Config,
        [switch]$Force
    )

    if (
        (-not $Force) -and
        ($null -ne $script:IDLToken) -and
        ($script:IDLToken.ExpiresAt -gt [datetime]::UtcNow.AddMinutes(2))
    ){
        return $script:IDLToken.Value
    }

    $tokenUrl = '{0}/{1}/oauth2/v2.0/token' -f $Config.Endpoints.Login, [uri]::EscapeDataString($Config.TenantId)
    $form = [ordered]@{
        client_id  = $Config.ClientId
        scope      = $Config.Endpoints.Scope
        grant_type = 'client_credentials'
    }

    if ($Config.Auth -eq 'certificate'){
        $cert = Get-IDLCloudCertificate -Thumbprint $Config.CertThumbprint
        # The audience is the token endpoint the assertion is posted to (as Microsoft documents it).
        $audience = '{0}/{1}/oauth2/v2.0/token' -f $Config.Endpoints.Login, $Config.TenantId
        $form.client_assertion_type = 'urn:ietf:params:oauth:client-assertion-type:jwt-bearer'
        $form.client_assertion = New-IDLClientAssertion -Certificate $cert -ClientId $Config.ClientId -Audience $audience
    }
    else{
        $form.client_secret = $Config.ClientSecret
    }

    $body = ($form.GetEnumerator() | ForEach-Object { '{0}={1}' -f $_.Key, [uri]::EscapeDataString([string]$_.Value) }) -join '&'
    Write-IDLLog "Requesting a Graph token for client $($Config.ClientId) in tenant $($Config.TenantId)"
    $r = Invoke-IDLCloudHttp -Method POST -Uri $tokenUrl -Body $body -ContentType 'application/x-www-form-urlencoded'
    $json = ConvertFrom-IDLJsonBody -Text $r.Body
    if (
        ($r.Status -ne 200) -or
        ($null -eq $json) -or
        ([string]::IsNullOrEmpty([string](Get-IDLField -Object $json -Name 'access_token')))
    ){
        $code = [string](Get-IDLField -Object $json -Name 'error')
        $description = [string](Get-IDLField -Object $json -Name 'error_description')
        $description = ($description -split "`r?`n")[0]
        throw (New-IDLException -Kind Connection -Message ("Sign-in to Microsoft Entra failed (HTTP {0}{1}){2}" -f $r.Status, (($code) ? (" $code") : ('')), (($description) ? (": $description") : (''))))
    }

    $token = [string](Get-IDLField -Object $json -Name 'access_token')
    Register-IDLSecret -Secret $token
    $expiresIn = 3599
    $null = [int]::TryParse([string](Get-IDLField -Object $json -Name 'expires_in'), [ref]$expiresIn)
    $script:IDLToken = @{ Value = $token; ExpiresAt = [datetime]::UtcNow.AddSeconds($expiresIn) }
    Write-IDLLog "Graph token acquired (valid $expiresIn s)"
    return $token
}

function New-IDLGraphException {
    # A Graph error as an exception carrying the HTTP status and Graph error code in .Data.
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Method,
        [Parameter(Mandatory)][string]$Path,
        [Parameter(Mandatory)]$Response
    )

    $json = ConvertFrom-IDLJsonBody -Text $Response.Body
    $err = Get-IDLField -Object $json -Name 'error'
    $code = [string](Get-IDLField -Object $err -Name 'code')
    $text = [string](Get-IDLField -Object $err -Name 'message')
    $shortPath = ($Path -split '\?')[0]
    $message = 'Graph {0} {1} failed: HTTP {2}{3}{4}' -f $Method, $shortPath, $Response.Status, (($code) ? (" $code") : ('')), (($text) ? (": $text") : (''))
    # Denied or unavailable (5xx after the retries) = the service cannot be used: exit code 4 like an unreachable AD.
    $kind = ((($Response.Status -in @(401, 403)) -or ($Response.Status -ge 500)) ? ('Connection') : ('Fatal'))
    $ex = New-IDLException -Kind $kind -Message $message
    $ex.Data['IDLGraphStatus'] = [int]$Response.Status
    $ex.Data['IDLGraphCode'] = $code
    $ex.Data['IDLGraphMessage'] = $text
    return $ex
}

function Get-IDLGraphErrorStatus {
    # The HTTP status of a Graph error (0 when the exception is not one).
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][System.Exception]$Exception
    )

    $cursor = $Exception
    while ($null -ne $cursor){
        if ($cursor.Data.Contains('IDLGraphStatus')){
            return [int]$cursor.Data['IDLGraphStatus']
        }

        $cursor = $cursor.InnerException
    }

    return 0
}

function Get-IDLGraphErrorText {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][System.Exception]$Exception
    )

    $cursor = $Exception
    while ($null -ne $cursor){
        if ($cursor.Data.Contains('IDLGraphStatus')){
            return ('{0} {1}' -f [string]$cursor.Data['IDLGraphCode'], [string]$cursor.Data['IDLGraphMessage']).Trim()
        }

        $cursor = $cursor.InnerException
    }

    return $Exception.Message
}

function Invoke-IDLGraph {
    <#
    .SYNOPSIS
        Calls Microsoft Graph (v1.0) with the app-only token. Retries 429/503/504 honouring Retry-After (at most 5
        times, at most 60 s each) and a 401 once with a fresh token. -All follows @odata.nextLink and returns every
        "value" entry; otherwise the parsed body is returned ($null for 204). Errors throw (see New-IDLGraphException).
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]$Config,
        [ValidateSet('GET', 'POST', 'PATCH', 'DELETE')][string]$Method = 'GET',
        [Parameter(Mandatory)][string]$Path,
        $Body = $null,
        [switch]$All,
        [switch]$Eventual
    )

    $uri = $Config.Endpoints.Graph + '/v1.0' + $Path
    $bodyText = (($null -eq $Body) ? ($null) : (ConvertTo-Json -InputObject $Body -Depth 10 -Compress))
    $items = [System.Collections.Generic.List[object]]::new()
    $refreshed = $false

    while ($true){
        $attempt = 0
        while ($true){
            $headers = @{ Authorization = 'Bearer ' + (Get-IDLCloudToken -Config $Config) }
            if ($Eventual){
                $headers.ConsistencyLevel = 'eventual'
            }

            $logUri = $uri.Substring($Config.Endpoints.Graph.Length)
            Write-IDLLog "Graph $Method $logUri" 'DEBUG'
            $r = Invoke-IDLCloudHttp -Method $Method -Uri $uri -Headers $headers -Body $bodyText
            if (
                ($r.Status -eq 401) -and
                (-not $refreshed)
            ){
                $refreshed = $true
                $null = Get-IDLCloudToken -Config $Config -Force
                continue
            }

            if (
                ($r.Status -in @(429, 503, 504)) -and
                ($attempt -lt $script:IDLRetryLimit)
            ){
                $attempt++
                $wait = (($null -ne $r.RetryAfter) ? ($r.RetryAfter) : ([int][math]::Pow(2, $attempt)))
                $wait = [math]::Min([math]::Max($wait, 0), $script:IDLRetryCapSeconds)
                Write-IDLLog "Graph $Method $(($Path -split '\?')[0]) answered HTTP $($r.Status); retry $attempt of $script:IDLRetryLimit in $wait s" 'WARN'
                Start-Sleep -Seconds $wait
                continue
            }

            break
        }

        if (
            ($r.Status -lt 200) -or
            ($r.Status -ge 300)
        ){
            throw (New-IDLGraphException -Method $Method -Path $Path -Response $r)
        }

        $json = ConvertFrom-IDLJsonBody -Text $r.Body
        if (-not $All){
            return $json
        }

        $values = Get-IDLCloudList -Object $json -Name 'value'
        foreach ($entry in $values){
            $items.Add($entry)
        }

        $next = [string](Get-IDLField -Object $json -Name '@odata.nextLink')
        if ([string]::IsNullOrEmpty($next)){
            return , $items.ToArray()
        }

        # nextLink is absolute; it must stay on the Graph host we started on.
        if (-not $next.StartsWith($Config.Endpoints.Graph + '/', [System.StringComparison]::OrdinalIgnoreCase)){
            throw (New-IDLException -Kind Fatal -Message 'Graph returned a next page link on another host; refusing to follow it.')
        }

        $uri = $next
    }
}

function Get-IDLGraphGroupSelect {
    return $script:IDLGroupSelect
}

#endregion

#region Group kinds

function Get-IDLCloudGroupKind {
    <#
    .SYNOPSIS
        Kind of an Entra group and whether IDLifecycle may change its membership.
        kind: security | m365 | mail_security | distribution | unknown.
        reason (when manageable is false): synced (mastered in AD: use the AD group), role_assignable (protected, like
        AD admin groups), dynamic (rule-based membership), unverified (the group's properties could not be read -
        grant Group.Read.All), exchange_off (needs Exchange Online, which is not enabled), unsupported.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]$Group,
        [bool]$ExchangeEnabled = $false
    )

    $typeList = Get-IDLCloudList -Object $Group -Name 'groupTypes'
    $types = [string[]]@($typeList | ForEach-Object { [string]$_ })
    $mailEnabled = Get-IDLField -Object $Group -Name 'mailEnabled'
    $securityEnabled = Get-IDLField -Object $Group -Name 'securityEnabled'
    $roleAssignable = Get-IDLField -Object $Group -Name 'isAssignableToRole'
    $synced = Get-IDLField -Object $Group -Name 'onPremisesSyncEnabled'

    $kind = $(
        if ($types -contains 'Unified'){ 'm365' }
        elseif (([bool]$securityEnabled) -and ([bool]$mailEnabled)){ 'mail_security' }
        elseif ([bool]$securityEnabled){ 'security' }
        elseif ([bool]$mailEnabled){ 'distribution' }
        else{ 'unknown' }
    )

    $reason = $(
        if ($true -eq $synced){ 'synced' }
        elseif ($true -eq $roleAssignable){ 'role_assignable' }
        elseif ($types -contains 'DynamicMembership'){ 'dynamic' }
        elseif (($null -eq $roleAssignable) -or ($null -eq $mailEnabled) -or ($null -eq $securityEnabled)){ 'unverified' }
        elseif ($kind -eq 'unknown'){ 'unsupported' }
        elseif (($kind -in @('mail_security', 'distribution')) -and (-not $ExchangeEnabled)){ 'exchange_off' }
        else{ '' }
    )

    return @{
        Kind       = $kind
        Manageable = ($reason -eq '')
        Reason     = $reason
        ViaExchange = ($kind -in @('mail_security', 'distribution'))
    }
}

function Get-IDLCloudReasonText {
    # Plain-language text for a not-manageable reason code.
    [CmdletBinding()]
    param(
        [AllowEmptyString()][string]$Reason
    )

    switch ($Reason){
        'synced' { return 'the group is synchronised from Active Directory; change it as an AD group' }
        'role_assignable' { return 'the group can be assigned Entra roles (protected administrative group)' }
        'dynamic' { return 'the group has dynamic (rule-based) membership' }
        'unverified' { return 'the group''s properties could not be read (grant Group.Read.All)' }
        'exchange_off' { return 'the group is managed in Exchange Online, which is not enabled' }
        'unsupported' { return 'the group is neither a security, Microsoft 365, mail-enabled security nor distribution group' }
        'not_found' { return 'the group was not found' }
        default { return $Reason }
    }
}

function ConvertTo-IDLCloudGroup {
    # A Graph group as the contract shape {id,name,description,mail,kind,manageable,reason}.
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]$Group,
        [bool]$ExchangeEnabled = $false
    )

    $info = Get-IDLCloudGroupKind -Group $Group -ExchangeEnabled $ExchangeEnabled
    $description = [string](Get-IDLField -Object $Group -Name 'description')
    $mail = [string](Get-IDLField -Object $Group -Name 'mail')
    return [ordered]@{
        id          = ([string](Get-IDLField -Object $Group -Name 'id')).ToLowerInvariant()
        name        = [string](Get-IDLField -Object $Group -Name 'displayName')
        description = $description
        mail        = $mail
        kind        = $info.Kind
        manageable  = $info.Manageable
        reason      = $info.Reason
    }
}

#endregion

#region Exchange Online

function Import-IDLExchangeModule {
    [CmdletBinding()]
    param()

    try{
        Import-Module ExchangeOnlineManagement -MinimumVersion 3.0.0 -Global -ErrorAction Stop
    }
    catch{
        throw (New-IDLException -Kind Dependency -Message "The ExchangeOnlineManagement module (3.0 or later) could not be loaded - install it for all users: Install-Module ExchangeOnlineManagement -Scope AllUsers. ($($_.Exception.Message))" -InnerException $_.Exception)
    }

    return (Get-Module -Name ExchangeOnlineManagement)
}

function Connect-IDLExchange {
    <#
    .SYNOPSIS
        Connects to Exchange Online app-only with the certificate (passed as an object, so it may live in
        LocalMachine\My), loading only the cmdlets IDLifecycle uses.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]$Config
    )

    if ($script:IDLExchangeConnected){
        return
    }

    $null = Import-IDLExchangeModule
    $cert = Get-IDLCloudCertificate -Thumbprint $Config.CertThumbprint
    Write-IDLLog "Connecting to Exchange Online ($($Config.Organization), $($Config.Endpoints.Exchange))"
    try{
        Connect-ExchangeOnline -AppId $Config.ClientId -Certificate $cert -Organization $Config.Organization `
            -ExchangeEnvironmentName $Config.Endpoints.Exchange -ShowBanner:$false -SkipLoadingFormatData `
            -CommandName @('Get-Recipient', 'Get-DistributionGroupMember', 'Add-DistributionGroupMember', 'Remove-DistributionGroupMember') `
            -ErrorAction Stop | Out-Null
    }
    catch{
        throw (New-IDLException -Kind Connection -Message "Could not connect to Exchange Online: $($_.Exception.Message)" -InnerException $_.Exception)
    }

    $script:IDLExchangeConnected = $true
}

function Disconnect-IDLExchange {
    [CmdletBinding()]
    param()

    if (-not $script:IDLExchangeConnected){
        return
    }

    $script:IDLExchangeConnected = $false
    try{
        Disconnect-ExchangeOnline -Confirm:$false -ErrorAction Stop | Out-Null
        Write-IDLLog 'Disconnected from Exchange Online'
    }
    catch{
        Write-IDLLog "Disconnect from Exchange Online failed (ignored): $($_.Exception.Message)" 'WARN'
    }
}

function Find-IDLExchangeRecipient {
    # The Exchange Online recipient whose ExternalDirectoryObjectId is the Entra object id ($null when none).
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$ObjectId
    )

    if (-not (Test-IDLCloudGuid -Value $ObjectId)){
        throw "Not an object id: $ObjectId"
    }

    $found = @(Get-Recipient -Filter "ExternalDirectoryObjectId -eq '$ObjectId'" -ResultSize 2 -ErrorAction Stop)
    if ($found.Count -eq 0){
        return $null
    }

    return $found[0]
}

#endregion

Export-ModuleMember -Function @(
    'Test-IDLCloudGuid', 'Get-IDLCloudList', 'Get-IDLCloudGuidList', 'Get-IDLCloudEndpoints', 'Get-IDLCloudConfig', 'Get-IDLCloudConfigText',
    'Get-IDLCloudCertificate', 'Get-IDLCloudKeyInfo', 'ConvertTo-IDLBase64Url', 'ConvertFrom-IDLBase64Url',
    'New-IDLClientAssertion', 'ConvertFrom-IDLJwtPayload', 'Get-IDLCloudToken', 'Invoke-IDLGraph',
    'Get-IDLGraphErrorStatus', 'Get-IDLGraphErrorText', 'Get-IDLGraphGroupSelect', 'Get-IDLCloudGroupKind',
    'Get-IDLCloudReasonText', 'ConvertTo-IDLCloudGroup', 'Import-IDLExchangeModule', 'Connect-IDLExchange',
    'Disconnect-IDLExchange', 'Find-IDLExchangeRecipient'
)
