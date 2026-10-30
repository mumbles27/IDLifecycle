$here = Split-Path -Parent $MyInvocation.MyCommand.Path
. (Join-Path $here 'TestHelpers.ps1')

# Live creation is tested in CurrentUser\My only (the tests are not elevated and must not touch the machine store);
# the LocalMachine path is covered by -DryRun and the refusal without elevation.
Describe 'New-IDLCloudCertificate.ps1' {

    function NewCert {
        param([string[]]$Arguments)
        return (Invoke-IDLScript -Script 'New-IDLCloudCertificate' -NoState -ScriptArguments $Arguments)
    }

    function Get-StoreCount {
        param([string]$Subject)
        return @(Get-ChildItem Cert:\CurrentUser\My | Where-Object { $_.Subject -eq $Subject }).Count + @(Get-ChildItem Cert:\LocalMachine\My | Where-Object { $_.Subject -eq $Subject }).Count
    }

    It 'creates a non-exportable CSP certificate, writes only the public .cer and reports the thumbprint' {
        $out = Join-Path $script:StateDir ('cert_' + [guid]::NewGuid().ToString('N'))
        $subject = 'CN=IDL-Pester-New-' + [guid]::NewGuid().ToString('N').Substring(0, 8)
        $thumb = $null
        try{
            $r = NewCert -Arguments @('-StoreLocation', 'CurrentUser', '-OutDir', $out, '-Subject', $subject, '-Years', '1', '-Json')
            $r.ExitCode | Should Be 0
            $r.SingleJson | Should Be $true
            $r.Json.ok | Should Be $true
            $r.Json.dryRun | Should Be $false
            $thumb = $r.Json.thumbprint
            $thumb | Should Match '^[0-9A-F]{40}$'
            $r.Json.keyProvider | Should Be 'Microsoft Enhanced RSA and AES Cryptographic Provider'
            $r.Json.grantedTo | Should Be ''

            $cert = Get-Item "Cert:\CurrentUser\My\$thumb"
            $cert.Subject | Should Be $subject
            $cert.HasPrivateKey | Should Be $true
            $cert.NotAfter | Should BeGreaterThan (Get-Date).AddDays(360)
            $cert.NotAfter | Should BeLessThan (Get-Date).AddDays(370)
            $exported = $true
            try{
                $null = $cert.Export([System.Security.Cryptography.X509Certificates.X509ContentType]::Pfx, 'x')
            }
            catch{
                $exported = $false
            }
            $exported | Should Be $false

            @(Get-ChildItem -LiteralPath $out).Count | Should Be 1
            $r.Json.cerPath | Should Be (Join-Path $out "IDLifecycle-$thumb.cer")
            $public = [System.Security.Cryptography.X509Certificates.X509Certificate2]::new($r.Json.cerPath)
            $public.Thumbprint | Should Be $thumb
            $public.HasPrivateKey | Should Be $false

            # Text mode prints the next steps.
            $r2 = NewCert -Arguments @('-StoreLocation', 'CurrentUser', '-OutDir', $out, '-Subject', $subject, '-DryRun')
            $r2.ExitCode | Should Be 0
            $r2.StdOut | Should Match 'DRY RUN'
        }
        finally{
            if ($thumb -and (Test-Path "Cert:\CurrentUser\My\$thumb")){
                Remove-Item "Cert:\CurrentUser\My\$thumb" -DeleteKey
            }
            Remove-Item -LiteralPath $out -Recurse -Force -ErrorAction SilentlyContinue
        }
    }

    It 'DryRun for the machine store creates nothing and names the account that would get the key' {
        $subject = 'CN=IDL-Pester-Dry-' + [guid]::NewGuid().ToString('N').Substring(0, 8)
        $r = NewCert -Arguments @('-ServiceAccount', 'NT SERVICE\IDLifecycle', '-Subject', $subject, '-DryRun', '-Json')
        $r.ExitCode | Should Be 0
        $r.Json.dryRun | Should Be $true
        $r.Json.storeLocation | Should Be 'LocalMachine'
        $r.Json.grantedTo | Should Be 'NT SERVICE\IDLifecycle'
        $r.Json.thumbprint | Should Be ''
        $r.LogText | Should Match 'WOULD: create'
        $r.LogText | Should Match 'WOULD: give NT SERVICE\\IDLifecycle'
        Get-StoreCount -Subject $subject | Should Be 0

        # LocalSystem already has access to machine keys.
        $r = NewCert -Arguments @('-ServiceAccount', 'LocalSystem', '-Subject', $subject, '-DryRun', '-Json')
        $r.ExitCode | Should Be 0
        $r.Json.grantedTo | Should Be ''
    }

    It 'refuses bad input, an unknown service and (without elevation) the machine store with exit 2' {
        foreach ($a in @(@('-Years', '0', '-DryRun'), @('-Years', '6', '-DryRun'), @('-StoreLocation', 'Elsewhere', '-DryRun'),
                @('-Subject', 'not a cn', '-DryRun'), @('-ServiceAccount', 'bad name with spaces', '-DryRun'),
                @('-ServiceName', 'IDL-NoSuchService-xyz', '-DryRun'), @('-ServiceName', 'bad/name', '-DryRun'))){
            $r = NewCert -Arguments (@($a) + '-Json')
            $r.ExitCode | Should Be 2
            $r.Json.ok | Should Be $false
        }

        $elevated = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
        if (-not $elevated){
            $subject = 'CN=IDL-Pester-NoElev-' + [guid]::NewGuid().ToString('N').Substring(0, 8)
            $r = NewCert -Arguments @('-ServiceAccount', 'NT SERVICE\IDLifecycle', '-Subject', $subject, '-Json')
            $r.ExitCode | Should Be 2
            $r.Json.error | Should Match 'elevated'
            Get-StoreCount -Subject $subject | Should Be 0
        }
    }
}
