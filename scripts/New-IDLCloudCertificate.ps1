<#
.SYNOPSIS
    Creates the certificate IDLifecycle signs in to Microsoft Entra ID (Graph) and Exchange Online with, lets the
    service account use its private key, and exports the public part (.cer) to upload to the app registration.

.DESCRIPTION
    Run once, by an administrator, in an ELEVATED PowerShell 7 on the IDLifecycle server:

      1. creates a self-signed RSA-2048 certificate (SHA-256) in LocalMachine\My whose private key lives in the
         "Microsoft Enhanced RSA and AES Cryptographic Provider" (a CSP key - Exchange Online app-only sign-in does
         not accept CNG keys) and can not be exported;
      2. gives the service account Read on the private key (the account is read from the installed IDLifecycle
         service unless -ServiceAccount is given; LocalSystem needs nothing);
      3. writes the PUBLIC certificate to <OutDir>\IDLifecycle-<thumbprint>.cer. The private key is never exported;
      4. prints the thumbprint and the next steps (upload the .cer, permissions, IDLifecycle settings).

    -StoreLocation CurrentUser creates the certificate in the current user's store instead (no key permission is set;
    for a service that runs as this user, and for tests). -DryRun only reports what would be done.

.PARAMETER ServiceAccount
    Account that runs the IDLifecycle service (DOMAIN\name, DOMAIN\gmsa$, NT SERVICE\IDLifecycle, LocalSystem).
    Default: the log-on account of the installed service -ServiceName.

.PARAMETER ServiceName
    Name of the installed IDLifecycle service. Default IDLifecycle.

.PARAMETER OutDir
    Folder for the .cer file. Default: the current folder.

.PARAMETER Subject
    Certificate subject. Default CN=IDLifecycle-<COMPUTERNAME>.

.PARAMETER Years
    Validity in years, 1-5. Default 2.

.PARAMETER StoreLocation
    LocalMachine (default, needs elevation) or CurrentUser.

.PARAMETER DryRun
    Validate everything and report WOULD: lines; create nothing.

.PARAMETER Json
    Write one JSON summary document to stdout instead of the text summary:
    {"ok":true,"dryRun":false,"thumbprint","subject","notAfter","storeLocation","keyProvider","cerPath","grantedTo"}.

.EXAMPLE
    .\New-IDLCloudCertificate.ps1 -OutDir C:\Temp
    Creates the certificate for the installed service's account and writes C:\Temp\IDLifecycle-<thumbprint>.cer.

.NOTES
    Requires PowerShell 7+ on Windows (PKI module). Exit codes: 0 ok, 2 invalid input (or not elevated), 3 PKI module
    missing, 5 unexpected error. Log: $PSScriptRoot\Logs\New-IDLCloudCertificate_<yyyyMMdd_HHmmss>.log.
#>

#Requires -Version 7.0

[CmdletBinding()]
param(
    [string]$ServiceAccount = '',
    [string]$ServiceName = 'IDLifecycle',
    [string]$OutDir = '',
    [string]$Subject = '',
    [int]$Years = 2,
    [string]$StoreLocation = 'LocalMachine',
    [switch]$DryRun,
    [switch]$Json
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

Import-Module (Join-Path $PSScriptRoot 'IDLifecycle.Common.psm1') -Force
Import-Module (Join-Path $PSScriptRoot 'IDLifecycle.Cloud.psm1') -Force

$exitCode = 5
$summary = $null
$lines = [System.Collections.Generic.List[string]]::new()

try{
    Start-IDLRun -ScriptName 'New-IDLCloudCertificate' -ScriptRoot $PSScriptRoot -DryRun:$DryRun -Parameters @{
        ServiceAccount = $ServiceAccount; ServiceName = $ServiceName; OutDir = $OutDir; Subject = $Subject; Years = $Years
        StoreLocation = $StoreLocation; DryRun = [bool]$DryRun
    }

    # ---- Validate ----
    if ($StoreLocation -notin @('LocalMachine', 'CurrentUser')){
        throw (New-IDLException -Kind Input -Message 'StoreLocation must be LocalMachine or CurrentUser.')
    }

    if (
        ($Years -lt 1) -or
        ($Years -gt 5)
    ){
        throw (New-IDLException -Kind Input -Message 'Years must be 1-5.')
    }

    if ([string]::IsNullOrWhiteSpace($Subject)){
        $Subject = "CN=IDLifecycle-$env:COMPUTERNAME"
    }

    if ($Subject -notmatch '^CN=[^,=+<>#;"\\]{1,64}$'){
        throw (New-IDLException -Kind Input -Message 'Subject must look like CN=<name> (1-64 characters, no , = + < > # ; " \).')
    }

    if ([string]::IsNullOrWhiteSpace($OutDir)){
        $OutDir = (Get-Location).ProviderPath
    }

    $OutDir = [System.IO.Path]::GetFullPath($OutDir)
    if ($ServiceName -notmatch '^[A-Za-z0-9_.\-]{1,80}$'){
        throw (New-IDLException -Kind Input -Message 'ServiceName is not valid.')
    }

    if (
        ($StoreLocation -eq 'LocalMachine') -and
        (-not $DryRun) -and
        (-not (Test-IDLProcessElevated))
    ){
        throw (New-IDLException -Kind Input -Message 'Run this from an elevated PowerShell 7 (Run as administrator): it writes to the LocalMachine certificate store.')
    }

    # ---- Who needs the key ----
    $grantSid = $null
    $grantName = ''
    if ($StoreLocation -eq 'LocalMachine'){
        $account = $ServiceAccount.Trim()
        if ($account -eq ''){
            $service = Get-CimInstance -ClassName Win32_Service -Filter ("Name='{0}'" -f $ServiceName) -ErrorAction SilentlyContinue
            if ($null -eq $service){
                throw (New-IDLException -Kind Input -Message "The service '$ServiceName' is not installed; pass -ServiceAccount with the account that will run IDLifecycle.")
            }

            $account = [string]$service.StartName
            Write-IDLLog "Service '$ServiceName' runs as '$account'"
        }

        if ((Get-IDLServiceAccountKind -Account $account) -eq ''){
            throw (New-IDLException -Kind Input -Message "ServiceAccount '$account' is not an account name (DOMAIN\name, DOMAIN\gmsa$, NT SERVICE\name or LocalSystem).")
        }

        if ($account -in @('LocalSystem', 'NT AUTHORITY\SYSTEM')){
            Write-IDLLog 'The service runs as LocalSystem, which can already use machine keys; no key permission needed'
        }
        else{
            $grantName = ConvertTo-IDLServiceAccountName -Account $account
            try{
                $grantSid = ([System.Security.Principal.NTAccount]::new($grantName)).Translate([System.Security.Principal.SecurityIdentifier])
            }
            catch{
                throw (New-IDLException -Kind Input -Message "Account '$grantName' could not be resolved to a SID: $($_.Exception.Message)")
            }
        }
    }

    $notAfter = (Get-Date).AddYears($Years)
    $provider = 'Microsoft Enhanced RSA and AES Cryptographic Provider'
    if ($DryRun){
        Write-IDLLog "WOULD: create $Subject in $StoreLocation\My (RSA 2048, $provider, not exportable, valid until $($notAfter.ToString('yyyy-MM-dd')))"
        if ($null -ne $grantSid){
            Write-IDLLog "WOULD: give $grantName ($($grantSid.Value)) Read on the private key"
        }

        Write-IDLLog "WOULD: write the public certificate to $OutDir\IDLifecycle-<thumbprint>.cer"
        $lines.Add("DRY RUN - nothing was created.")
        $lines.Add("Would create $Subject in $StoreLocation\My, valid until $($notAfter.ToString('yyyy-MM-dd')), key in '$provider'.")
        if ($null -ne $grantSid){
            $lines.Add("Would give $grantName Read on the private key.")
        }

        $lines.Add("Would write the public certificate to $OutDir.")
        $summary = [ordered]@{
            ok = $true; dryRun = $true; thumbprint = ''; subject = $Subject; notAfter = $notAfter.ToString('yyyy-MM-dd')
            storeLocation = $StoreLocation; keyProvider = $provider; cerPath = ''; grantedTo = $grantName
        }
        $exitCode = 0
    }
    else{
        try{
            try{
                Import-Module PKI -ErrorAction Stop
            }
            catch{
                # A service account or a locked-down session may not have the Windows PowerShell module folder on its path.
                Import-Module (Join-Path $env:SystemRoot 'System32\WindowsPowerShell\v1.0\Modules\PKI\PKI.psd1') -ErrorAction Stop
            }
        }
        catch{
            throw (New-IDLException -Kind Dependency -Message "The Windows PKI module could not be loaded: $($_.Exception.Message)" -InnerException $_.Exception)
        }

        # ---- Create (CSP key, not exportable) ----
        $cert = New-SelfSignedCertificate -Subject $Subject -CertStoreLocation "Cert:\$StoreLocation\My" -KeyAlgorithm RSA -KeyLength 2048 `
            -Provider $provider -KeySpec KeyExchange -KeyExportPolicy NonExportable -HashAlgorithm SHA256 -NotAfter $notAfter `
            -FriendlyName 'IDLifecycle Microsoft Entra app'
        $thumbprint = [string]$cert.Thumbprint
        Write-IDLLog "Created certificate $thumbprint ($Subject) in $StoreLocation\My, valid until $($cert.NotAfter.ToString('yyyy-MM-dd'))"
        $key = Get-IDLCloudKeyInfo -Certificate $cert
        Write-IDLLog "Private key: provider '$($key.Provider)', container '$($key.Container)'"

        # ---- Let the service account use the key ----
        if ($null -ne $grantSid){
            $keyFile = Join-Path $env:ProgramData ("Microsoft\Crypto\RSA\MachineKeys\{0}" -f $key.Container)
            if (-not (Test-Path -LiteralPath $keyFile -PathType Leaf)){
                $keyFile = Join-Path $env:ProgramData ("Microsoft\Crypto\Keys\{0}" -f $key.Container)
            }

            if (-not (Test-Path -LiteralPath $keyFile -PathType Leaf)){
                throw "The certificate was created ($thumbprint) but its private key file was not found, so $grantName could not be given access. Grant it in certlm.msc: the certificate > All Tasks > Manage Private Keys > Read."
            }

            $acl = Get-Acl -LiteralPath $keyFile
            $rule = [System.Security.AccessControl.FileSystemAccessRule]::new($grantSid, [System.Security.AccessControl.FileSystemRights]::Read, [System.Security.AccessControl.AccessControlType]::Allow)
            $acl.AddAccessRule($rule)
            Set-Acl -LiteralPath $keyFile -AclObject $acl
            Write-IDLLog "Gave $grantName ($($grantSid.Value)) Read on $keyFile"
        }

        # ---- Export the public part only ----
        if (-not (Test-Path -LiteralPath $OutDir -PathType Container)){
            $null = New-Item -ItemType Directory -Path $OutDir -Force
        }

        $cerPath = Join-Path $OutDir ("IDLifecycle-{0}.cer" -f $thumbprint)
        [System.IO.File]::WriteAllBytes($cerPath, $cert.Export([System.Security.Cryptography.X509Certificates.X509ContentType]::Cert))
        Write-IDLLog "Public certificate written to $cerPath"

        $lines.Add("Certificate created: $thumbprint")
        $lines.Add("  Subject     : $Subject")
        $lines.Add("  Valid until : $($cert.NotAfter.ToString('yyyy-MM-dd'))")
        $lines.Add("  Store       : $StoreLocation\My (private key: $($key.Provider), not exportable)")
        if ($null -ne $grantSid){
            $lines.Add("  Key access  : $grantName (Read)")
        }

        $lines.Add("  Public file : $cerPath")
        $lines.Add('')
        $lines.Add('Next steps:')
        $lines.Add('  1. Entra admin center > App registrations > your IDLifecycle app > Certificates & secrets > Upload certificate: the .cer above.')
        $lines.Add('  2. API permissions (Application, then Grant admin consent): Microsoft Graph GroupMember.ReadWrite.All, Group.Read.All, User.Read.All;')
        $lines.Add('     for Exchange Online groups also Office 365 Exchange Online Exchange.ManageAsApp and an Exchange role (see the setup guide).')
        $lines.Add("  3. IDLifecycle > Microsoft 365 / Entra ID settings: certificate thumbprint $thumbprint, then Test connection.")
        $summary = [ordered]@{
            ok = $true; dryRun = $false; thumbprint = $thumbprint; subject = $Subject; notAfter = $cert.NotAfter.ToString('yyyy-MM-dd')
            storeLocation = $StoreLocation; keyProvider = $key.Provider; cerPath = $cerPath; grantedTo = $grantName
        }
        $exitCode = 0
    }
}
catch{
    $exitCode = Get-IDLExitCode -Exception $_.Exception
    $message = Protect-IDLText -Text $_.Exception.Message
    Write-IDLLog "FATAL: $message" 'ERROR'
    Write-IDLLog "Stack: $($_.ScriptStackTrace)" 'DEBUG'
    $summary = New-IDLErrorResponse -Message $message
    $lines.Clear()
    $lines.Add("FAILED: $message")
}
finally{
    Stop-IDLRun -ExitCode $exitCode
}

if ($Json){
    Write-IDLResponse -Response $summary
}
else{
    foreach ($line in $lines){
        Write-Output $line
    }
}

exit $exitCode
