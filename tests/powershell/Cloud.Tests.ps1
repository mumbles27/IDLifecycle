$here = Split-Path -Parent $MyInvocation.MyCommand.Path
. (Join-Path $here 'TestHelpers.ps1')
. (Join-Path $here 'CloudTestHelpers.ps1')

# Pure helpers of IDLifecycle.Cloud.psm1, run in this process (no network, no AD).
Import-Module (Join-Path $script:ScriptsDir 'IDLifecycle.Common.psm1') -Force
Import-Module (Join-Path $script:ScriptsDir 'IDLifecycle.Cloud.psm1') -Force

Describe 'IDLifecycle.Cloud.psm1 client assertion' {

    $csp = New-TestCertificate
    $cng = New-TestCertificate -Cng
    try{
        It 'has the header and claims Microsoft documents, and its RS256 signature verifies with the certificate''s public key' {
            $cert = Get-IDLCloudCertificate -Thumbprint $csp.Thumbprint
            $aud = "https://login.microsoftonline.com/$script:TestTenant/oauth2/v2.0/token"
            $before = [System.DateTimeOffset]::UtcNow.ToUnixTimeSeconds()
            $jwt = New-IDLClientAssertion -Certificate $cert -ClientId $script:TestClient -Audience $aud
            $parts = $jwt.Split('.')
            $parts.Count | Should Be 3

            $header = ConvertFrom-Json ([System.Text.Encoding]::UTF8.GetString((ConvertFrom-IDLBase64Url -Text $parts[0])))
            $header.alg | Should Be 'RS256'
            $header.typ | Should Be 'JWT'
            # x5t = base64url of the SHA-1 certificate hash (the thumbprint's bytes); x5t#S256 = SHA-256.
            $thumbBytes = [byte[]]@(for ($i = 0; $i -lt 40; $i += 2){ [Convert]::ToByte($csp.Thumbprint.Substring($i, 2), 16) })
            $header.x5t | Should Be (ConvertTo-IDLBase64Url -Bytes $thumbBytes)
            $header.'x5t#S256' | Should Be (ConvertTo-IDLBase64Url -Bytes $cert.GetCertHash([System.Security.Cryptography.HashAlgorithmName]::SHA256))
            $parts[0] | Should Not Match '[=+/]'

            $claims = ConvertFrom-Json ([System.Text.Encoding]::UTF8.GetString((ConvertFrom-IDLBase64Url -Text $parts[1])))
            $claims.aud | Should Be $aud
            $claims.iss | Should Be $script:TestClient
            $claims.sub | Should Be $script:TestClient
            [guid]::Parse($claims.jti) | Should Not BeNullOrEmpty
            [long]$claims.nbf | Should BeGreaterThan ($before - 5)
            ([long]$claims.exp - [long]$claims.nbf) | Should Be 600

            # Verified with ONLY the exported public certificate (.cer), as Entra does.
            $public = [System.Security.Cryptography.X509Certificates.X509Certificate2]::new($csp.CerPath)
            $public.HasPrivateKey | Should Be $false
            $rsa = [System.Security.Cryptography.X509Certificates.RSACertificateExtensions]::GetRSAPublicKey($public)
            $data = [System.Text.Encoding]::ASCII.GetBytes($parts[0] + '.' + $parts[1])
            $sig = ConvertFrom-IDLBase64Url -Text $parts[2]
            $sig.Length | Should Be 256
            $rsa.VerifyData($data, $sig, [System.Security.Cryptography.HashAlgorithmName]::SHA256, [System.Security.Cryptography.RSASignaturePadding]::Pkcs1) | Should Be $true

            # Any change to the signed part breaks it.
            $tampered = [System.Text.Encoding]::ASCII.GetBytes($parts[0] + '.' + $parts[1] + 'x')
            $rsa.VerifyData($tampered, $sig, [System.Security.Cryptography.HashAlgorithmName]::SHA256, [System.Security.Cryptography.RSASignaturePadding]::Pkcs1) | Should Be $false

            # The assertion is a credential: registered for scrubbing.
            (Protect-IDLText -Text "x $jwt y") | Should Be 'x *** y'
        }

        It 'works with a CNG key too, and tells CSP (needed by Exchange Online) from CNG' {
            $cert = Get-IDLCloudCertificate -Thumbprint $cng.Thumbprint
            $jwt = New-IDLClientAssertion -Certificate $cert -ClientId $script:TestClient -Audience 'https://x/t/oauth2/v2.0/token'
            $parts = $jwt.Split('.')
            $public = [System.Security.Cryptography.X509Certificates.X509Certificate2]::new($cng.CerPath)
            $rsa = [System.Security.Cryptography.X509Certificates.RSACertificateExtensions]::GetRSAPublicKey($public)
            $rsa.VerifyData([System.Text.Encoding]::ASCII.GetBytes($parts[0] + '.' + $parts[1]), (ConvertFrom-IDLBase64Url -Text $parts[2]),
                [System.Security.Cryptography.HashAlgorithmName]::SHA256, [System.Security.Cryptography.RSASignaturePadding]::Pkcs1) | Should Be $true

            (Get-IDLCloudKeyInfo -Certificate $cert).IsCsp | Should Be $false
            (Get-IDLCloudKeyInfo -Certificate (Get-IDLCloudCertificate -Thumbprint $csp.Thumbprint)).IsCsp | Should Be $true
        }

        It 'a thumbprint that is not installed is a dependency error (exit code 3)' {
            $err = $null
            try{
                $null = Get-IDLCloudCertificate -Thumbprint ('AB' * 20)
            }
            catch{
                $err = $_.Exception
            }

            $err | Should Not BeNullOrEmpty
            Get-IDLExitCode -Exception $err | Should Be 3
        }
    }
    finally{
        Remove-TestCertificate $csp
        Remove-TestCertificate $cng
    }
}

Describe 'IDLifecycle.Cloud.psm1 group kinds' {

    function Kind {
        param([hashtable]$Group, [bool]$Exchange = $false)
        $base = @{ id = 'x'; displayName = 'G'; mailEnabled = $false; securityEnabled = $true; groupTypes = @(); isAssignableToRole = $false; onPremisesSyncEnabled = $null }
        foreach ($k in $Group.Keys){ $base[$k] = $Group[$k] }
        return (Get-IDLCloudGroupKind -Group $base -ExchangeEnabled $Exchange)
    }

    It 'classifies security, Microsoft 365, mail-enabled security and distribution groups and routes the last two to Exchange Online' {
        $k = Kind @{}
        $k.Kind | Should Be 'security'; $k.Manageable | Should Be $true; $k.ViaExchange | Should Be $false
        $k = Kind @{ groupTypes = @('Unified'); mailEnabled = $true; securityEnabled = $false }
        $k.Kind | Should Be 'm365'; $k.Manageable | Should Be $true; $k.ViaExchange | Should Be $false
        $k = Kind @{ mailEnabled = $true } -Exchange $true
        $k.Kind | Should Be 'mail_security'; $k.Manageable | Should Be $true; $k.ViaExchange | Should Be $true
        $k = Kind @{ mailEnabled = $true; securityEnabled = $false } -Exchange $true
        $k.Kind | Should Be 'distribution'; $k.Manageable | Should Be $true; $k.ViaExchange | Should Be $true
    }

    It 'refuses synced, role-assignable, dynamic, unverifiable and (Exchange off) mail groups' {
        (Kind @{ onPremisesSyncEnabled = $true }).Reason | Should Be 'synced'
        (Kind @{ isAssignableToRole = $true }).Reason | Should Be 'role_assignable'
        (Kind @{ groupTypes = @('DynamicMembership') }).Reason | Should Be 'dynamic'
        (Kind @{ groupTypes = @('Unified', 'DynamicMembership'); mailEnabled = $true }).Reason | Should Be 'dynamic'
        (Kind @{ isAssignableToRole = $null }).Reason | Should Be 'unverified'
        (Kind @{ mailEnabled = $true }).Reason | Should Be 'exchange_off'
        (Kind @{ mailEnabled = $true; securityEnabled = $false }).Reason | Should Be 'exchange_off'
        (Kind @{ mailEnabled = $false; securityEnabled = $false }).Reason | Should Be 'unsupported'
        # Synced wins over everything: such a group is changed in AD.
        (Kind @{ onPremisesSyncEnabled = $true; isAssignableToRole = $true }).Reason | Should Be 'synced'
        foreach ($r in @('synced', 'role_assignable', 'dynamic', 'unverified', 'exchange_off', 'unsupported')){
            Get-IDLCloudReasonText -Reason $r | Should Not Be $r
        }
    }
}

Describe 'IDLifecycle.Cloud.psm1 configuration and endpoints' {

    function Config {
        param([hashtable]$Cloud)
        return (Get-IDLCloudConfig -Request @{ cloud = $Cloud })
    }

    function Assert-InputError {
        param([scriptblock]$Block)
        $err = $null
        try{ & $Block } catch{ $err = $_.Exception }
        $err | Should Not BeNullOrEmpty
        Get-IDLExitCode -Exception $err | Should Be 2
    }

    $good = @{ tenantId = $script:TestTenant; clientId = $script:TestClient; auth = 'certificate'; certThumbprint = ('ab' * 20) }

    It 'accepts a valid block and normalises it' {
        $c = Config $good
        $c.Environment | Should Be 'global'
        $c.CertThumbprint | Should Be ('AB' * 20)
        $c.Endpoints.Graph | Should Be 'https://graph.microsoft.com'
        $c.Endpoints.Scope | Should Be 'https://graph.microsoft.com/.default'
        $c.ExchangeEnabled | Should Be $false
        (Config @{ tenantId = 'contoso.onmicrosoft.com'; clientId = $script:TestClient; auth = 'secret'; clientSecret = $script:TestSecret }).Auth | Should Be 'secret'
        (Protect-IDLText -Text "a $($script:TestSecret) b") | Should Be 'a *** b'
    }

    It 'refuses bad values with exit code 2' {
        Assert-InputError { Get-IDLCloudConfig -Request @{} }
        Assert-InputError { Config @{ tenantId = 'not a tenant'; clientId = $script:TestClient; certThumbprint = ('ab' * 20) } }
        Assert-InputError { Config @{ tenantId = $script:TestTenant; clientId = 'x'; certThumbprint = ('ab' * 20) } }
        Assert-InputError { Config @{ tenantId = $script:TestTenant; clientId = $script:TestClient; certThumbprint = 'abc' } }
        Assert-InputError { Config @{ tenantId = $script:TestTenant; clientId = $script:TestClient; auth = 'secret' } }
        Assert-InputError { Config @{ tenantId = $script:TestTenant; clientId = $script:TestClient; auth = 'password'; certThumbprint = ('ab' * 20) } }
        Assert-InputError { Config ($good + @{ environment = 'mars' }) }
        # Exchange Online: certificate only, and an organization is required.
        Assert-InputError { Config @{ tenantId = $script:TestTenant; clientId = $script:TestClient; auth = 'secret'; clientSecret = $script:TestSecret; exchange = @{ enabled = $true; organization = 'contoso.onmicrosoft.com' } } }
        Assert-InputError { Config ($good + @{ exchange = @{ enabled = $true; organization = '' } }) }
    }

    It 'knows the national clouds' {
        (Get-IDLCloudEndpoints -Environment usgov).Graph | Should Be 'https://graph.microsoft.us'
        (Get-IDLCloudEndpoints -Environment usgov).Exchange | Should Be 'O365USGovGCCHigh'
        (Get-IDLCloudEndpoints -Environment usgovdod).Graph | Should Be 'https://dod-graph.microsoft.us'
        (Get-IDLCloudEndpoints -Environment china).Login | Should Be 'https://login.chinacloudapi.cn'
    }

    It 'test overrides apply only with IDL_CLOUD_TEST=1 and only to loopback addresses' {
        $saved = @{ T = $env:IDL_CLOUD_TEST; G = $env:IDL_CLOUD_TEST_GRAPH_URL }
        try{
            $env:IDL_CLOUD_TEST_GRAPH_URL = 'http://127.0.0.1:1234'
            $env:IDL_CLOUD_TEST = ''
            (Get-IDLCloudEndpoints -Environment global).Graph | Should Be 'https://graph.microsoft.com'
            $env:IDL_CLOUD_TEST = '1'
            (Get-IDLCloudEndpoints -Environment global).Graph | Should Be 'http://127.0.0.1:1234'
            (Get-IDLCloudEndpoints -Environment global).Scope | Should Be 'https://graph.microsoft.com/.default'
            foreach ($bad in @('https://evil.example.com', 'http://10.0.0.1:80', 'http://127.0.0.1.evil.com:80')){
                $env:IDL_CLOUD_TEST_GRAPH_URL = $bad
                Assert-InputError { Get-IDLCloudEndpoints -Environment global }
            }
        }
        finally{
            $env:IDL_CLOUD_TEST = $saved.T
            $env:IDL_CLOUD_TEST_GRAPH_URL = $saved.G
        }
    }
}
