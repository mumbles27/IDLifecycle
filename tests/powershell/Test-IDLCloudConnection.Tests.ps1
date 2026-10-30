$here = Split-Path -Parent $MyInvocation.MyCommand.Path
. (Join-Path $here 'TestHelpers.ps1')
. (Join-Path $here 'CloudTestHelpers.ps1')

Describe 'Test-IDLCloudConnection.ps1' {

    $groups = @((New-FakeGroup -Id '60000000-0000-0000-0000-000000000001' -Name 'Any' -Kind security))
    $users = @((New-FakeUser -Id '60000000-0000-0000-0000-000000000002' -Upn 'jane@contoso.test'))

    $fake = Start-FakeGraph
    $csp = New-TestCertificate
    $cng = New-TestCertificate -Cng
    $soon = New-TestCertificate -Days 10
    try{
        function Check {
            param($Cloud, [string]$Exo = '', [hashtable]$ExtraEnv = @{})
            return (Invoke-CloudScript -Script 'Test-IDLCloudConnection' -Fake $fake -ExoState $Exo -ExtraEnv $ExtraEnv -Request @{ cloud = $Cloud })
        }

        function Get-Check {
            param($Result, [string]$Name)
            return @($Result.Json.checks | Where-Object { $_.name -eq $Name })[0]
        }

        It 'all checks pass with a CSP certificate, the three Graph permissions and an Exchange role; reads only' {
            Set-FakeGraphState -Fake $fake -Users $users -Groups $groups -CertPath $csp.CerPath
            $exo = New-ExoStateFile -Recipients @((New-ExoRecipient -Id '60000000-0000-0000-0000-000000000002' -Name 'Jane'))
            $r = Check -Cloud (New-CloudBlock -Thumbprint $csp.Thumbprint -Exchange) -Exo $exo

            $r.ExitCode | Should Be 0
            $r.SingleJson | Should Be $true
            $r.Json.ok | Should Be $true
            $r.Json.healthy | Should Be $true
            (@($r.Json.checks | ForEach-Object { $_.name }) -join ',') | Should Be 'certificate,token,graph_permissions,graph_read,exchange_module,exchange_connect,exchange_permissions'
            foreach ($c in $r.Json.checks){
                $c.ok | Should Be $true
                $c.warning | Should Be $false
            }

            (Get-Check $r 'certificate').detail | Should Match '\(CSP\)'
            (Get-Check $r 'exchange_module').detail | Should Match '99\.0\.0'
            @(Get-FakeGraphCalls -Fake $fake | Where-Object { $_.method -ne 'GET' }).Count | Should Be 0
            @(Get-ExoCalls -Path $exo | Where-Object { $_.cmdlet -match '^(Add|Remove)-' }).Count | Should Be 0
        }

        It 'names missing Graph permissions, and warns about broader ones' {
            Set-FakeGraphState -Fake $fake -Users $users -Groups $groups -CertPath $csp.CerPath -Roles @('GroupMember.ReadWrite.All')
            $r = Check -Cloud (New-CloudBlock -Thumbprint $csp.Thumbprint)
            $r.ExitCode | Should Be 0
            $r.Json.healthy | Should Be $false
            $p = Get-Check $r 'graph_permissions'
            $p.ok | Should Be $false
            $p.detail | Should Match 'Group\.Read\.All, User\.Read\.All'
            @($r.Json.checks | Where-Object { $_.name -like 'exchange*' }).Count | Should Be 0

            # Directory.ReadWrite.All covers everything but is broader than needed.
            Set-FakeGraphState -Fake $fake -Users $users -Groups $groups -CertPath $csp.CerPath -Roles @('Directory.ReadWrite.All')
            $r = Check -Cloud (New-CloudBlock -Thumbprint $csp.Thumbprint)
            $r.Json.healthy | Should Be $true
            $p = Get-Check $r 'graph_permissions'
            $p.ok | Should Be $true
            $p.warning | Should Be $true
            $p.detail | Should Match 'broader than IDLifecycle needs: Directory\.ReadWrite\.All'
        }

        It 'a CNG key fails the certificate check when Exchange Online is on; an expiring certificate warns; a missing one skips the sign-in' {
            Set-FakeGraphState -Fake $fake -Users $users -Groups $groups -CertPath $cng.CerPath
            $r = Check -Cloud (New-CloudBlock -Thumbprint $cng.Thumbprint -Exchange)
            $c = Get-Check $r 'certificate'
            $c.ok | Should Be $false
            $c.detail | Should Match 'needs a CSP key'
            $c.detail | Should Match 'New-IDLCloudCertificate'
            # Graph itself accepts CNG keys.
            (Get-Check $r 'token').ok | Should Be $true
            $r = Check -Cloud (New-CloudBlock -Thumbprint $cng.Thumbprint)
            (Get-Check $r 'certificate').ok | Should Be $true

            Set-FakeGraphState -Fake $fake -Users $users -Groups $groups -CertPath $soon.CerPath
            $r = Check -Cloud (New-CloudBlock -Thumbprint $soon.Thumbprint)
            $c = Get-Check $r 'certificate'
            $c.ok | Should Be $true
            $c.warning | Should Be $true
            $c.detail | Should Match 'expires soon'

            $r = Check -Cloud (New-CloudBlock -Thumbprint ('EF' * 20))
            $r.ExitCode | Should Be 0
            (Get-Check $r 'certificate').ok | Should Be $false
            (Get-Check $r 'token').detail | Should Match 'skipped'
            (Get-Check $r 'graph_read').detail | Should Match 'skipped'
        }

        It 'reports a rejected sign-in without the secret, a missing Exchange module and a missing Exchange role' {
            Set-FakeGraphState -Fake $fake -Users $users -Groups $groups -Secret 'another-secret-value'
            $r = Check -Cloud (New-CloudBlock -Secret)
            $r.ExitCode | Should Be 0
            $t = Get-Check $r 'token'
            $t.ok | Should Be $false
            $t.detail | Should Match 'AADSTS7000215'
            foreach ($text in @($r.StdOut, $r.StdErr, $r.LogText)){
                $text | Should Not Match ([regex]::Escape($script:TestSecret))
            }

            Set-FakeGraphState -Fake $fake -Users $users -Groups $groups -CertPath $csp.CerPath
            $r = Check -Cloud (New-CloudBlock -Thumbprint $csp.Thumbprint -Exchange) -ExtraEnv @{ PSModulePath = $script:StubDir }
            (Get-Check $r 'exchange_module').ok | Should Be $false
            (Get-Check $r 'exchange_module').detail | Should Match 'Install-Module ExchangeOnlineManagement'
            (Get-Check $r 'exchange_connect').detail | Should Match 'skipped'

            $exo = New-ExoStateFile -MissingCommands @('Add-DistributionGroupMember', 'Remove-DistributionGroupMember')
            $r = Check -Cloud (New-CloudBlock -Thumbprint $csp.Thumbprint -Exchange) -Exo $exo
            (Get-Check $r 'exchange_connect').ok | Should Be $true
            $p = Get-Check $r 'exchange_permissions'
            $p.ok | Should Be $false
            $p.detail | Should Match 'Add-DistributionGroupMember, Remove-DistributionGroupMember'
            $p.detail | Should Match 'Distribution Groups role'

            $exo = New-ExoStateFile -FailConnect $true
            $r = Check -Cloud (New-CloudBlock -Thumbprint $csp.Thumbprint -Exchange) -Exo $exo
            (Get-Check $r 'exchange_connect').ok | Should Be $false
            (Get-Check $r 'exchange_permissions').detail | Should Match 'skipped'
        }

        It 'bad input is exit 2' {
            $r = Check -Cloud @{ tenantId = 'x' }
            $r.ExitCode | Should Be 2
            $r.Json.ok | Should Be $false
            $r = Invoke-CloudScript -Script 'Test-IDLCloudConnection' -Fake $fake -RawInput 'not json'
            $r.ExitCode | Should Be 2
        }
    }
    finally{
        Stop-FakeGraph $fake
        Remove-TestCertificate $csp
        Remove-TestCertificate $cng
        Remove-TestCertificate $soon
    }
}
