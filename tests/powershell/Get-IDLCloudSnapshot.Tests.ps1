$here = Split-Path -Parent $MyInvocation.MyCommand.Path
. (Join-Path $here 'TestHelpers.ps1')
. (Join-Path $here 'CloudTestHelpers.ps1')

Describe 'Get-IDLCloudSnapshot.ps1' {

    $u = @{
        Jane = '20000000-0000-0000-0000-000000000001'
        Bob  = '20000000-0000-0000-0000-000000000002'
        Amy  = '20000000-0000-0000-0000-000000000003'
        Cara = '20000000-0000-0000-0000-000000000004'
        Dan  = '20000000-0000-0000-0000-000000000005'
        Guest = '20000000-0000-0000-0000-000000000006'
    }
    $g = @{
        Sec    = '30000000-0000-0000-0000-000000000001'
        Parent = '30000000-0000-0000-0000-000000000002'
        Dl     = '30000000-0000-0000-0000-000000000003'
        Gone   = '30000000-0000-0000-0000-0000000000ff'
    }
    $users = @(
        (New-FakeUser -Id $u.Jane -Upn 'jane@contoso.test' -Sid 'S-1-5-21-1-2-3-1101' -ImmutableId 'AAECAwQFBgcICQoLDA0ODw=='),
        (New-FakeUser -Id $u.Bob -Upn 'bob@contoso.test' -Sid 'S-1-5-21-1-2-3-1102' -Enabled $false),
        (New-FakeUser -Id $u.Amy -Upn 'amy@contoso.test' -Sid 'S-1-5-21-1-2-3-1103'),
        (New-FakeUser -Id $u.Cara -Upn 'cara@contoso.test' -Sid 'S-1-5-21-1-2-3-1104'),
        (New-FakeUser -Id $u.Dan -Upn 'dan@contoso.test' -Sid 'S-1-5-21-1-2-3-1105'),
        (New-FakeUser -Id $u.Guest -Upn 'guest_x#EXT#@contoso.test' -Synced $false)
    )
    $groups = @(
        (New-FakeGroup -Id $g.Sec -Name 'Finance Readers' -Kind security -Members @($u.Jane, $u.Bob, $u.Amy)),
        (New-FakeGroup -Id $g.Parent -Name 'All Finance' -Kind security -Members @($u.Cara) -MemberGroups @($g.Sec)),
        (New-FakeGroup -Id $g.Dl -Name 'Finance DL' -Kind distribution -Members @($u.Dan))
    )

    $fake = Start-FakeGraph
    $cert = New-TestCertificate
    try{
        function Snapshot {
            param([hashtable]$Request)
            if (-not $Request.ContainsKey('cloud')){
                $Request.cloud = New-CloudBlock -Thumbprint $cert.Thumbprint
            }
            return (Invoke-CloudScript -Script 'Get-IDLCloudSnapshot' -Fake $fake -Request $Request)
        }

        It 'returns every synced user (paged) and the DIRECT user members of each group; reads only' {
            Set-FakeGraphState -Fake $fake -Users $users -Groups $groups -CertPath $cert.CerPath -PageSize 2
            $r = Snapshot @{ groups = @($g.Sec, $g.Parent.ToUpperInvariant(), $g.Dl, $g.Gone); match = 'sid' }

            $r.ExitCode | Should Be 0
            $r.SingleJson | Should Be $true
            $r.Json.ok | Should Be $true

            $list = @($r.Json.users)
            $list.Count | Should Be 5     # the cloud-only guest is not synced
            $jane = @($list | Where-Object { $_.id -eq $u.Jane })[0]
            $jane.upn | Should Be 'jane@contoso.test'
            $jane.sid | Should Be 'S-1-5-21-1-2-3-1101'
            $jane.immutableId | Should Be 'AAECAwQFBgcICQoLDA0ODw=='
            $jane.enabled | Should Be $true
            @($list | Where-Object { $_.id -eq $u.Bob })[0].enabled | Should Be $false

            $byId = @{}
            foreach ($x in $r.Json.groups){ $byId[$x.id] = $x }
            $byId.Count | Should Be 4
            $byId[$g.Sec].found | Should Be $true
            $byId[$g.Sec].name | Should Be 'Finance Readers'
            $byId[$g.Sec].kind | Should Be 'security'
            $byId[$g.Sec].manageable | Should Be $true
            (@($byId[$g.Sec].members) | Sort-Object) -join ',' | Should Be ((@($u.Jane, $u.Bob, $u.Amy) | Sort-Object) -join ',')
            # Nested group members are not expanded: only Cara is a direct member of All Finance.
            @($byId[$g.Parent].members) -join ',' | Should Be $u.Cara
            $byId[$g.Dl].kind | Should Be 'distribution'
            $byId[$g.Dl].manageable | Should Be $false
            $byId[$g.Dl].reason | Should Be 'exchange_off'
            @($byId[$g.Dl].members) -join ',' | Should Be $u.Dan
            $byId[$g.Gone].found | Should Be $false
            $byId[$g.Gone].reason | Should Be 'not_found'
            @($byId[$g.Gone].members).Count | Should Be 0

            $calls = @(Get-FakeGraphCalls -Fake $fake)
            @($calls | Where-Object { $_.method -ne 'GET' }).Count | Should Be 0
            # $filter / $count / casts are advanced queries: every such request carries ConsistencyLevel: eventual.
            @($calls | Where-Object { ($_.path -match '\$filter|\$count') -and (-not $_.consistency) }).Count | Should Be 0
            # Paging: 5 users at 2 per page = 3 user requests.
            @($calls | Where-Object { $_.path -match '^/v1\.0/users\?' }).Count | Should Be 3
            $r.LogText | Should Match "Group $($g.Gone) not found"
        }

        It 'includeUsers:false skips the users; an empty group list is fine' {
            Set-FakeGraphState -Fake $fake -Users $users -Groups $groups -CertPath $cert.CerPath
            $r = Snapshot @{ groups = @($g.Sec); includeUsers = $false }
            $r.ExitCode | Should Be 0
            @($r.Json.users).Count | Should Be 0
            @($r.Json.groups).Count | Should Be 1
            @(Get-FakeGraphCalls -Fake $fake | Where-Object { $_.path -match '^/v1\.0/users' }).Count | Should Be 0

            $r = Snapshot @{ groups = @() }
            $r.ExitCode | Should Be 0
            @($r.Json.users).Count | Should Be 5
            @($r.Json.groups).Count | Should Be 0
        }

        It 'refuses bad input with exit 2; Graph that stays unavailable is exit 4' {
            Set-FakeGraphState -Fake $fake -Users $users -Groups $groups -CertPath $cert.CerPath
            foreach ($req in @(@{ groups = 'x' }, @{ groups = @('not-a-guid') }, @{ groups = @($g.Sec); match = 'email' },
                    @{ groups = @(1..1001 | ForEach-Object { '30000000-0000-0000-0000-{0:D12}' -f $_ }) })){
                $r = Snapshot $req
                $r.ExitCode | Should Be 2
                $r.Json.ok | Should Be $false
            }

            Set-FakeGraphState -Fake $fake -Users $users -Groups $groups -CertPath $cert.CerPath -Faults @(@{ method = 'GET'; path = '^/v1\.0/users'; status = 503; retryAfter = 0; times = 10 })
            $r = Snapshot @{ groups = @($g.Sec) }
            $r.ExitCode | Should Be 4
            $r.Json.error | Should Match 'HTTP 503'
            @(Get-FakeGraphCalls -Fake $fake | Where-Object { $_.path -match '^/v1\.0/users' }).Count | Should Be 6   # 1 + 5 retries
        }
    }
    finally{
        Stop-FakeGraph $fake
        Remove-TestCertificate $cert
    }
}
