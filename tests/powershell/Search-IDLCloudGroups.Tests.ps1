$here = Split-Path -Parent $MyInvocation.MyCommand.Path
. (Join-Path $here 'TestHelpers.ps1')
. (Join-Path $here 'CloudTestHelpers.ps1')

Describe 'Search-IDLCloudGroups.ps1' {

    $ids = @{
        Sec   = '10000000-0000-0000-0000-000000000001'
        M365  = '10000000-0000-0000-0000-000000000002'
        Mesg  = '10000000-0000-0000-0000-000000000003'
        Dl    = '10000000-0000-0000-0000-000000000004'
        Role  = '10000000-0000-0000-0000-000000000005'
        Dyn   = '10000000-0000-0000-0000-000000000006'
        Sync  = '10000000-0000-0000-0000-000000000007'
        Hide  = '10000000-0000-0000-0000-000000000008'
        Sales = '10000000-0000-0000-0000-000000000009'
    }
    $groups = @(
        (New-FakeGroup -Id $ids.Sec -Name 'Finance Readers' -Kind security -Description 'Finance share'),
        (New-FakeGroup -Id $ids.M365 -Name 'Finance Team' -Kind m365),
        (New-FakeGroup -Id $ids.Mesg -Name 'Finance Approvers' -Kind mail_security),
        (New-FakeGroup -Id $ids.Dl -Name 'Finance All' -Kind distribution),
        (New-FakeGroup -Id $ids.Role -Name 'Finance Admins' -Kind security -RoleAssignable $true),
        (New-FakeGroup -Id $ids.Dyn -Name 'Finance Dynamic' -Kind security -Dynamic $true),
        (New-FakeGroup -Id $ids.Sync -Name 'Finance Synced' -Kind security -Synced $true),
        (New-FakeGroup -Id $ids.Hide -Name 'Finance Hidden Flags' -Kind security -HideRoleFlag $true),
        (New-FakeGroup -Id $ids.Sales -Name 'Sales' -Kind security)
    )

    $fake = Start-FakeGraph
    $cert = New-TestCertificate
    try{
        function Search {
            param([hashtable]$Request, [switch]$Exchange, [hashtable]$ExtraEnv = @{})
            if (-not $Request.ContainsKey('cloud')){
                $Request.cloud = New-CloudBlock -Thumbprint $cert.Thumbprint -Exchange:$Exchange
            }
            return (Invoke-CloudScript -Script 'Search-IDLCloudGroups' -Fake $fake -Request $Request -ExtraEnv $ExtraEnv)
        }

        It 'finds groups by name, sorted, with kind and whether IDLifecycle may change them; reads only' {
            Set-FakeGraphState -Fake $fake -Groups $groups -CertPath $cert.CerPath
            $r = Search @{ query = 'finance' }

            $r.ExitCode | Should Be 0
            $r.SingleJson | Should Be $true
            $r.Json.ok | Should Be $true
            $r.Json.truncated | Should Be $false
            (@($r.Json.groups | ForEach-Object { $_.name }) -join '|') | Should Be 'Finance Admins|Finance All|Finance Approvers|Finance Dynamic|Finance Hidden Flags|Finance Readers|Finance Synced|Finance Team'
            $by = @{}
            foreach ($g in $r.Json.groups){ $by[$g.id] = $g }
            $by[$ids.Sec].kind | Should Be 'security'
            $by[$ids.Sec].manageable | Should Be $true
            $by[$ids.Sec].description | Should Be 'Finance share'
            $by[$ids.M365].kind | Should Be 'm365'
            $by[$ids.M365].manageable | Should Be $true
            $by[$ids.M365].mail | Should Be 'financeteam@contoso.test'
            $by[$ids.Mesg].kind | Should Be 'mail_security'
            $by[$ids.Mesg].reason | Should Be 'exchange_off'
            $by[$ids.Dl].kind | Should Be 'distribution'
            $by[$ids.Dl].reason | Should Be 'exchange_off'
            $by[$ids.Role].reason | Should Be 'role_assignable'
            $by[$ids.Dyn].reason | Should Be 'dynamic'
            $by[$ids.Sync].reason | Should Be 'synced'
            $by[$ids.Hide].reason | Should Be 'unverified'
            foreach ($g in $r.Json.groups){ $g.manageable | Should Be ($g.reason -eq '') }

            $calls = @(Get-FakeGraphCalls -Fake $fake)
            $calls.Count | Should Be 1
            $calls[0].method | Should Be 'GET'
            $calls[0].consistency | Should Be $true
            $calls[0].path | Should Match '\$count=true'
            $calls[0].path | Should Match '\$top=26'

            # With Exchange Online enabled mail-enabled security groups and distribution lists become manageable.
            $r = Search @{ query = 'finance' } -Exchange
            @($r.Json.groups | Where-Object { $_.id -eq $ids.Mesg })[0].manageable | Should Be $true
            @($r.Json.groups | Where-Object { $_.id -eq $ids.Dl })[0].manageable | Should Be $true
        }

        It 'escapes quotes and backslashes inside the $search phrase; "*" is ignored' {
            Set-FakeGraphState -Fake $fake -Groups $groups -CertPath $cert.CerPath
            $r = Search @{ query = 'a"b\c*' }
            $r.ExitCode | Should Be 0
            @($r.Json.groups).Count | Should Be 0
            $path = [uri]::UnescapeDataString(@(Get-FakeGraphCalls -Fake $fake)[0].path)
            $path | Should Match ([regex]::Escape('"displayName:a\"b\\c" OR "mail:a\"b\\c"'))

            $r = Search @{ query = 'Sal*' }
            (@($r.Json.groups | ForEach-Object { $_.name }) -join '|') | Should Be 'Sales'
        }

        It 'max limits the result and sets truncated' {
            Set-FakeGraphState -Fake $fake -Groups $groups -CertPath $cert.CerPath
            $r = Search @{ query = 'finance'; max = 3 }
            @($r.Json.groups).Count | Should Be 3
            $r.Json.truncated | Should Be $true
            @(Get-FakeGraphCalls -Fake $fake)[0].path | Should Match '\$top=4'
        }

        It 'looks groups up by id in the requested order; unknown ids are found:false' {
            Set-FakeGraphState -Fake $fake -Groups $groups -CertPath $cert.CerPath
            $missing = '10000000-0000-0000-0000-0000000000ff'
            $r = Search @{ ids = @($ids.Dl, $missing, $ids.Sec.ToUpperInvariant()) }
            $r.ExitCode | Should Be 0
            $list = @($r.Json.groups)
            $list.Count | Should Be 3
            $list[0].id | Should Be $ids.Dl
            $list[0].found | Should Be $true
            $list[0].name | Should Be 'Finance All'
            $list[1].id | Should Be $missing
            $list[1].found | Should Be $false
            $list[1].reason | Should Be 'not_found'
            $list[2].id | Should Be $ids.Sec
            $list[2].kind | Should Be 'security'
        }

        It 'retries a throttled request after Retry-After' {
            Set-FakeGraphState -Fake $fake -Groups $groups -CertPath $cert.CerPath -Faults @(@{ method = 'GET'; path = '^/v1\.0/groups\?'; status = 429; retryAfter = 1; times = 2 })
            $r = Search @{ query = 'sales' }
            $r.ExitCode | Should Be 0
            @($r.Json.groups).Count | Should Be 1
            @(Get-FakeGraphCalls -Fake $fake).Count | Should Be 3
            $r.LogText | Should Match 'HTTP 429; retry 1 of 5 in 1 s'
            $r.LogText | Should Match 'retry 2 of 5'
        }

        It 'refuses bad input with exit 2 and a loopback-only test override' {
            Set-FakeGraphState -Fake $fake -Groups $groups -CertPath $cert.CerPath
            foreach ($req in @(@{ query = 'a' }, @{ query = '***' }, @{ query = ('x' * 65) }, @{ query = "fi`nn" }, @{ query = 'fin'; max = 0 },
                    @{ query = 'fin'; max = 101 }, @{ query = 'fin'; ids = @($ids.Sec) }, @{ ids = @('nope') }, @{ ids = @() },
                    @{ ids = @(1..51 | ForEach-Object { '10000000-0000-0000-0000-{0:D12}' -f $_ }) })){
                $r = Search $req
                $r.ExitCode | Should Be 2
                $r.Json.ok | Should Be $false
                $r.SingleJson | Should Be $true
            }

            $r = Search @{ query = 'fin'; cloud = @{ tenantId = $script:TestTenant; clientId = $script:TestClient; certThumbprint = 'zz' } }
            $r.ExitCode | Should Be 2
            $r = Search @{ query = 'fin' } -ExtraEnv @{ IDL_CLOUD_TEST_GRAPH_URL = 'https://graph.example.com' }
            $r.ExitCode | Should Be 2
            @(Get-FakeGraphCalls -Fake $fake -IncludeToken).Count | Should Be 0
        }

        It 'a certificate that is not installed is exit 3; a rejected sign-in or denied access is exit 4; the secret never leaks' {
            Set-FakeGraphState -Fake $fake -Groups $groups -CertPath $cert.CerPath
            $r = Search @{ query = 'fin'; cloud = (New-CloudBlock -Thumbprint ('CD' * 20)) }
            $r.ExitCode | Should Be 3
            $r.Json.error | Should Match 'not found'

            Set-FakeGraphState -Fake $fake -Groups $groups -Secret 'the-right-secret'
            $r = Search @{ query = 'fin'; cloud = (New-CloudBlock -Secret) }
            $r.ExitCode | Should Be 4
            $r.Json.error | Should Match 'AADSTS7000215'
            foreach ($text in @($r.StdOut, $r.StdErr, $r.LogText)){
                $text | Should Not Match ([regex]::Escape($script:TestSecret))
            }

            Set-FakeGraphState -Fake $fake -Groups $groups -Secret $script:TestSecret
            $r = Search @{ query = 'sales'; cloud = (New-CloudBlock -Secret) }
            $r.ExitCode | Should Be 0
            @($r.Json.groups).Count | Should Be 1
            $token = @(Get-FakeGraphCalls -Fake $fake -IncludeToken | Where-Object { $_.path -match 'token' })[0]
            $token.usedSecret | Should Be $true
            $token.scope | Should Be 'https://graph.microsoft.com/.default'
            foreach ($text in @($r.StdOut, $r.StdErr, $r.LogText)){
                $text | Should Not Match ([regex]::Escape($script:TestSecret))
            }

            Set-FakeGraphState -Fake $fake -Groups $groups -CertPath $cert.CerPath -Faults @(@{ method = 'GET'; path = '^/v1\.0/groups'; status = 403; times = 1 })
            $r = Search @{ query = 'sales' }
            $r.ExitCode | Should Be 4
        }
    }
    finally{
        Stop-FakeGraph $fake
        Remove-TestCertificate $cert
    }
}
