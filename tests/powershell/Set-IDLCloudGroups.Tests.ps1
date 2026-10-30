$here = Split-Path -Parent $MyInvocation.MyCommand.Path
. (Join-Path $here 'TestHelpers.ps1')
. (Join-Path $here 'CloudTestHelpers.ps1')

Describe 'Set-IDLCloudGroups.ps1' {

    $u = @{
        Jane = '40000000-0000-0000-0000-000000000001'
        Bob  = '40000000-0000-0000-0000-000000000002'    # no mailbox in Exchange Online
        Nope = '40000000-0000-0000-0000-0000000000ff'
    }
    $g = @{
        Sec    = '50000000-0000-0000-0000-000000000001'
        M365   = '50000000-0000-0000-0000-000000000002'
        Mesg   = '50000000-0000-0000-0000-000000000003'
        Dl     = '50000000-0000-0000-0000-000000000004'
        Role   = '50000000-0000-0000-0000-000000000005'
        Dyn    = '50000000-0000-0000-0000-000000000006'
        Sync   = '50000000-0000-0000-0000-000000000007'
        Hide   = '50000000-0000-0000-0000-000000000008'
        Keep   = '50000000-0000-0000-0000-000000000009'
        Nest   = '50000000-0000-0000-0000-00000000000a'
        Gone   = '50000000-0000-0000-0000-0000000000ff'
    }
    $users = @(
        (New-FakeUser -Id $u.Jane -Upn 'jane@contoso.test' -Sid 'S-1-5-21-1-2-3-1101'),
        (New-FakeUser -Id $u.Bob -Upn 'bob@contoso.test' -Sid 'S-1-5-21-1-2-3-1102')
    )

    function New-Groups {
        # Membership of Jane is set per test; Nest contains Sec (so a member of Sec is a member of Nest only through nesting).
        param([string[]]$JaneIn = @())
        $m = { param($id) ((($JaneIn -contains $id)) ? (@($u.Jane)) : (@())) }
        return @(
            (New-FakeGroup -Id $g.Sec -Name 'Finance Readers' -Kind security -Members (& $m $g.Sec)),
            (New-FakeGroup -Id $g.M365 -Name 'Finance Team' -Kind m365 -Members (& $m $g.M365)),
            (New-FakeGroup -Id $g.Mesg -Name 'Finance Approvers' -Kind mail_security -Members (& $m $g.Mesg)),
            (New-FakeGroup -Id $g.Dl -Name 'Finance All' -Kind distribution -Members (& $m $g.Dl)),
            (New-FakeGroup -Id $g.Role -Name 'Helpdesk Admins' -Kind security -RoleAssignable $true -Members (& $m $g.Role)),
            (New-FakeGroup -Id $g.Dyn -Name 'All Employees (dynamic)' -Kind security -Dynamic $true -Members (& $m $g.Dyn)),
            (New-FakeGroup -Id $g.Sync -Name 'GG-Finance (synced)' -Kind security -Synced $true -Members (& $m $g.Sync)),
            (New-FakeGroup -Id $g.Hide -Name 'Unreadable' -Kind security -HideRoleFlag $true -Members (& $m $g.Hide)),
            (New-FakeGroup -Id $g.Keep -Name 'Mail Retention' -Kind security -Members (& $m $g.Keep)),
            (New-FakeGroup -Id $g.Nest -Name 'All Finance (nested)' -Kind security -MemberGroups @($g.Sec))
        )
    }

    function New-Exo {
        param([hashtable]$Members = @{}, [bool]$FailConnect = $false, [object[]]$Faults = @())
        $recipients = @(
            (New-ExoRecipient -Id $g.Mesg -Name 'Finance Approvers' -Type 'MailUniversalSecurityGroup'),
            (New-ExoRecipient -Id $g.Dl -Name 'Finance All' -Type 'MailUniversalDistributionGroup'),
            (New-ExoRecipient -Id $u.Jane -Name 'Jane Doe' -Type 'UserMailbox')
        )
        return (New-ExoStateFile -Recipients $recipients -Members $Members -FailConnect $FailConnect -Faults $Faults)
    }

    function Get-Members {
        param([string]$GroupId)
        $group = @((Read-FakeGraphState -Fake $fake).groups | Where-Object { $_.id -eq $GroupId })[0]
        return , @($group.members | Where-Object { $_ })
    }

    $fake = Start-FakeGraph
    $cert = New-TestCertificate
    try{
        function SetGroups {
            param([object[]]$Items, [string]$Exo, [switch]$DryRun, [switch]$NoExchange, $Cloud = $null)
            if ($null -eq $Cloud){
                $Cloud = New-CloudBlock -Thumbprint $cert.Thumbprint -Exchange:(-not $NoExchange)
            }
            return (Invoke-CloudScript -Script 'Set-IDLCloudGroups' -Fake $fake -ExoState $Exo -DryRun:$DryRun -Request @{ cloud = $Cloud; items = @($Items) })
        }

        It 'adds through Graph (security, Microsoft 365) and Exchange Online (mail-enabled security, distribution); a re-run is skipped' {
            Set-FakeGraphState -Fake $fake -Users $users -Groups (New-Groups) -CertPath $cert.CerPath
            $exo = New-Exo
            $item = @{ changeId = 'c1'; externalId = 'E1001'; userId = $u.Jane; add = @($g.Sec, $g.M365, $g.Mesg, $g.Dl) }
            $r = SetGroups -Items @($item) -Exo $exo

            $r.ExitCode | Should Be 0
            $r.SingleJson | Should Be $true
            $res = @($r.Json.results)[0]
            $res.status | Should Be 'applied'
            $res.upn | Should Be 'jane@contoso.test'
            $res.message | Should Match 'added to Finance Readers'
            $res.message | Should Match 'added to Finance All'

            # Graph: exactly the two Graph-managed groups, each referencing the user as a directoryObject.
            $posts = @(Get-FakeGraphCalls -Fake $fake -Method POST)
            (@($posts | ForEach-Object { $_.path }) | Sort-Object) -join '|' | Should Be ("/v1.0/groups/$($g.Sec)/members/`$ref|/v1.0/groups/$($g.M365)/members/`$ref")
            $posts[0].body.'@odata.id' | Should Match "/v1\.0/directoryObjects/$($u.Jane)$"
            (Get-Members $g.Sec) -contains $u.Jane | Should Be $true
            (Get-Members $g.M365) -contains $u.Jane | Should Be $true

            # Exchange Online: app-only with the certificate OBJECT (works from LocalMachine\My), only the cmdlets needed.
            $connect = @(Get-ExoCalls -Path $exo -Cmdlet 'Connect-ExchangeOnline')
            $connect.Count | Should Be 1
            $connect[0].args.AppId | Should Be $script:TestClient
            $connect[0].args.Organization | Should Be 'contoso.onmicrosoft.com'
            $connect[0].args.CertificateObjectThumbprint | Should Be $cert.Thumbprint
            $connect[0].args.CertificateHasPrivateKey | Should Be $true
            (@($connect[0].args.CommandName) -join ',') | Should Be 'Get-Recipient,Get-DistributionGroupMember,Add-DistributionGroupMember,Remove-DistributionGroupMember'
            $adds = @(Get-ExoCalls -Path $exo -Cmdlet 'Add-DistributionGroupMember')
            $adds.Count | Should Be 2
            foreach ($a in $adds){
                $a.result | Should Be 'ok'
                $a.args.BypassSecurityGroupManagerCheck | Should Be $true
            }
            @((Read-ExoStateFile -Path $exo).members.$($g.Dl)) -contains $u.Jane | Should Be $true
            @(Get-ExoCalls -Path $exo -Cmdlet 'Disconnect-ExchangeOnline').Count | Should Be 1
            $r.ReportText | Should Match 'AddCloudGroupMember'

            # Same request again: nothing to do, nothing written anywhere.
            $before = @(Get-ExoCalls -Path $exo -Cmdlet 'Add-DistributionGroupMember').Count
            Set-FakeGraphState -Fake $fake -Users $users -Groups (New-Groups -JaneIn @($g.Sec, $g.M365, $g.Mesg, $g.Dl)) -CertPath $cert.CerPath
            $r = SetGroups -Items @($item) -Exo $exo
            $r.ExitCode | Should Be 0
            @($r.Json.results)[0].status | Should Be 'skipped'
            @(Get-FakeGraphCalls -Fake $fake -Method POST).Count | Should Be 0
            @(Get-ExoCalls -Path $exo -Cmdlet 'Add-DistributionGroupMember').Count | Should Be $before
        }

        It 'removes direct memberships; not being a member (or only through nesting) is fine' {
            Set-FakeGraphState -Fake $fake -Users $users -Groups (New-Groups -JaneIn @($g.Sec, $g.Dl)) -CertPath $cert.CerPath
            $exo = New-Exo -Members @{ $g.Dl = @($u.Jane) }
            $r = SetGroups -Exo $exo -Items @(@{ changeId = 'c2'; externalId = 'E1001'; userId = $u.Jane; remove = @($g.Sec, $g.M365, $g.Dl, $g.Nest, $g.Gone) })
            $r.ExitCode | Should Be 0
            $res = @($r.Json.results)[0]
            $res.status | Should Be 'applied'
            $res.message | Should Match 'removed from Finance Readers'
            $res.message | Should Match 'removed from Finance All'
            @(Get-FakeGraphCalls -Fake $fake -Method DELETE | ForEach-Object { $_.path }) -join '|' | Should Be "/v1.0/groups/$($g.Sec)/members/$($u.Jane)/`$ref"
            @(Get-ExoCalls -Path $exo -Cmdlet 'Remove-DistributionGroupMember').Count | Should Be 1
            (Get-Members $g.Sec).Count | Should Be 0
            $r.LogText | Should Match "Group $($g.Gone) not found; nothing to remove"
        }

        It 'refuses role-assignable, dynamic, synced, unverifiable and (Exchange off) mail groups; the other groups still change and the item fails' {
            Set-FakeGraphState -Fake $fake -Users $users -Groups (New-Groups) -CertPath $cert.CerPath
            $exo = New-Exo
            $r = SetGroups -NoExchange -Exo $exo -Items @(@{ changeId = 'c3'; externalId = 'E1001'; userId = $u.Jane; add = @($g.Role, $g.Dyn, $g.Sync, $g.Hide, $g.Dl, $g.Sec) })
            $r.ExitCode | Should Be 1
            $res = @($r.Json.results)[0]
            $res.status | Should Be 'failed'
            $res.message | Should Match 'added to Finance Readers'
            $res.message | Should Match 'failed: 5 group'
            $res.error | Should Match 'protected administrative group'
            $res.error | Should Match 'dynamic'
            $res.error | Should Match 'synchronised from Active Directory'
            $res.error | Should Match 'Group\.Read\.All'
            $res.error | Should Match 'Exchange Online, which is not enabled'
            # Only the allowed group was written.
            @(Get-FakeGraphCalls -Fake $fake -Method POST | ForEach-Object { $_.path }) -join '|' | Should Be "/v1.0/groups/$($g.Sec)/members/`$ref"
            @(Get-ExoCalls -Path $exo).Count | Should Be 0
            $r.ReportText | Should Match 'Failed'
        }

        It 'removeAll strips every direct membership except keep/add groups and leaves protected, dynamic, synced and nested ones alone' {
            $janeIn = @($g.Sec, $g.M365, $g.Dl, $g.Role, $g.Dyn, $g.Sync, $g.Keep)
            Set-FakeGraphState -Fake $fake -Users $users -Groups (New-Groups -JaneIn $janeIn) -CertPath $cert.CerPath -PageSize 2
            $exo = New-Exo -Members @{ $g.Dl = @($u.Jane) }
            $r = SetGroups -Exo $exo -Items @(@{ changeId = 'c4'; externalId = 'E1001'; userId = $u.Jane; removeAll = $true; keep = @($g.Keep) })
            $r.ExitCode | Should Be 0
            $res = @($r.Json.results)[0]
            $res.status | Should Be 'applied'
            $res.message | Should Match 'left alone: '
            $res.message | Should Match 'Helpdesk Admins \(role_assignable\)'
            $res.message | Should Match 'dynamic\)'
            $res.message | Should Match 'synced\)'

            (@(Get-FakeGraphCalls -Fake $fake -Method DELETE | ForEach-Object { $_.path }) | Sort-Object) -join '|' |
                Should Be ("/v1.0/groups/$($g.Sec)/members/$($u.Jane)/`$ref|/v1.0/groups/$($g.M365)/members/$($u.Jane)/`$ref")
            @(Get-ExoCalls -Path $exo -Cmdlet 'Remove-DistributionGroupMember').Count | Should Be 1
            (Get-Members $g.Keep) -contains $u.Jane | Should Be $true
            (Get-Members $g.Role) -contains $u.Jane | Should Be $true
            # transitiveMemberOf (User.Read.All) is used - not memberOf, which needs Directory.Read.All - and is paged.
            @(Get-FakeGraphCalls -Fake $fake | Where-Object { $_.path -match '/transitiveMemberOf/' }).Count | Should BeGreaterThan 1
            @(Get-FakeGraphCalls -Fake $fake | Where-Object { $_.path -match '/memberOf' -and $_.path -notmatch 'transitive' }).Count | Should Be 0
        }

        It 'DryRun reads everything, reports "would" and makes no change in Graph or Exchange Online' {
            Set-FakeGraphState -Fake $fake -Users $users -Groups (New-Groups -JaneIn @($g.M365, $g.Dl)) -CertPath $cert.CerPath
            $exo = New-Exo -Members @{ $g.Dl = @($u.Jane) }
            $r = SetGroups -DryRun -Exo $exo -Items @(
                @{ changeId = 'd1'; externalId = 'E1001'; userId = $u.Jane; add = @($g.Sec, $g.Mesg); remove = @($g.M365, $g.Dl) },
                @{ changeId = 'd2'; externalId = 'E1001'; userId = $u.Jane; removeAll = $true }
            )
            $r.ExitCode | Should Be 0
            $r.Json.dryRun | Should Be $true
            @($r.Json.results)[0].status | Should Be 'would'
            @($r.Json.results)[0].message | Should Match 'would add to Finance Readers'
            @($r.Json.results)[0].message | Should Match 'would remove from Finance All'
            @($r.Json.results)[1].status | Should Be 'would'
            @(Get-FakeGraphCalls -Fake $fake | Where-Object { $_.method -ne 'GET' }).Count | Should Be 0
            @(Get-ExoCalls -Path $exo | Where-Object { $_.cmdlet -match '^(Add|Remove)-' }).Count | Should Be 0
            $r.LogText | Should Match 'WOULD: add to'
            (Get-Members $g.Sec).Count | Should Be 0
        }

        It 'a user without a mailbox cannot join a distribution list; an unknown user or group fails its item; other items continue' {
            Set-FakeGraphState -Fake $fake -Users $users -Groups (New-Groups) -CertPath $cert.CerPath
            $exo = New-Exo
            $r = SetGroups -Exo $exo -Items @(
                @{ changeId = 'e1'; externalId = 'E1002'; userId = $u.Bob; add = @($g.Dl, $g.Sec) },
                @{ changeId = 'e2'; externalId = 'E9999'; userId = $u.Nope; add = @($g.Sec) },
                @{ changeId = 'e3'; externalId = 'E1001'; userId = $u.Jane; add = @($g.Gone, $g.M365) },
                @{ changeId = 'e4'; externalId = 'E1001'; userId = 'not-a-guid' },
                @{ changeId = 'e5'; externalId = 'E1001'; userId = $u.Jane; add = @($g.Sec); remove = @($g.Sec) }
            )
            $r.ExitCode | Should Be 1
            $res = @($r.Json.results)
            $res.Count | Should Be 5
            $res[0].status | Should Be 'failed'
            $res[0].error | Should Match 'not a mail recipient'
            $res[0].message | Should Match 'added to Finance Readers'
            $res[1].status | Should Be 'failed'
            $res[1].error | Should Match "Entra user $($u.Nope) was not found"
            $res[2].status | Should Be 'failed'
            $res[2].error | Should Match "group $($g.Gone) was not found"
            $res[2].message | Should Match 'added to Finance Team'
            $res[3].status | Should Be 'failed'
            $res[3].error | Should Match 'userId'
            $res[4].status | Should Be 'failed'
            $res[4].error | Should Match 'both in add and remove'
            $r.Json.summary.failed | Should Be 5
        }

        It 'an Exchange Online sign-in failure fails only the Exchange groups' {
            Set-FakeGraphState -Fake $fake -Users $users -Groups (New-Groups) -CertPath $cert.CerPath
            $exo = New-Exo -FailConnect $true
            $r = SetGroups -Exo $exo -Items @(@{ changeId = 'x1'; externalId = 'E1001'; userId = $u.Jane; add = @($g.Dl, $g.Mesg, $g.Sec) })
            $r.ExitCode | Should Be 1
            $res = @($r.Json.results)[0]
            $res.status | Should Be 'failed'
            $res.message | Should Match 'added to Finance Readers'
            $res.error | Should Match 'Could not connect to Exchange Online'
            # The failed connection is remembered: one attempt, not one per group.
            @(Get-ExoCalls -Path $exo -Cmdlet 'Connect-ExchangeOnline').Count | Should Be 1
        }

        It 'returns before.cloudAdded / cloudRemoved with only what changed (DryRun too); the inverse restores the memberships' {
            Set-FakeGraphState -Fake $fake -Users $users -Groups (New-Groups -JaneIn @($g.M365, $g.Mesg)) -CertPath $cert.CerPath
            $exo = New-Exo -Members @{ $g.Mesg = @($u.Jane) }
            $item = @{ changeId = 'c1'; externalId = 'E1001'; userId = $u.Jane; add = @($g.Sec, $g.Dl, $g.M365); remove = @($g.Mesg, $g.Keep) }

            $r = SetGroups -Items @($item) -Exo $exo -DryRun
            $b = @($r.Json.results)[0].before
            (@($b.cloudAdded) | Sort-Object) -join '|' | Should Be ((@($g.Sec, $g.Dl) | Sort-Object) -join '|')   # M365: already a member
            @($b.cloudRemoved) -join '|' | Should Be $g.Mesg                                                       # Keep: not a member
            @(Get-FakeGraphCalls -Fake $fake -Method POST).Count | Should Be 0

            $r = SetGroups -Items @($item) -Exo $exo
            $res = @($r.Json.results)[0]
            $res.status | Should Be 'applied'
            (@($res.before.cloudAdded) | Sort-Object) -join '|' | Should Be ((@($g.Sec, $g.Dl) | Sort-Object) -join '|')
            @($res.before.cloudRemoved) -join '|' | Should Be $g.Mesg
            (Get-Members $g.Sec) -contains $u.Jane | Should Be $true

            # The inverse: add what was removed, remove what was added. (The fake Graph does not see Exchange Online
            # writes, so its membership is brought in line with the first run first.)
            Set-FakeGraphState -Fake $fake -Users $users -Groups (New-Groups -JaneIn @($g.M365, $g.Sec, $g.Dl)) -CertPath $cert.CerPath
            $undo = @{ changeId = 'u1'; externalId = 'E1001'; userId = $u.Jane; add = @($res.before.cloudRemoved); remove = @($res.before.cloudAdded) }
            $r = SetGroups -Items @($undo) -Exo $exo
            @($r.Json.results)[0].status | Should Be 'applied'
            (Get-Members $g.Sec) -contains $u.Jane | Should Be $false
            (Get-Members $g.M365) -contains $u.Jane | Should Be $true
            @((Read-ExoStateFile -Path $exo).members.$($g.Mesg)) -contains $u.Jane | Should Be $true
            @((Read-ExoStateFile -Path $exo).members.$($g.Dl)) -contains $u.Jane | Should Be $false
        }
        It 'retries a throttled write; a client secret never reaches the log, the report or the output' {
            Set-FakeGraphState -Fake $fake -Users $users -Groups (New-Groups) -Secret $script:TestSecret -Faults @(@{ method = 'POST'; path = '/members/\$ref'; status = 429; retryAfter = 1; times = 1 })
            $exo = New-Exo
            $cloud = New-CloudBlock -Secret
            $r = SetGroups -Cloud $cloud -Exo $exo -Items @(@{ changeId = 's1'; externalId = 'E1001'; userId = $u.Jane; add = @($g.Sec); password = $script:TestSecret })
            $r.ExitCode | Should Be 0
            @($r.Json.results)[0].status | Should Be 'applied'
            @(Get-FakeGraphCalls -Fake $fake -Method POST).Count | Should Be 2
            foreach ($text in @($r.StdOut, $r.StdErr, $r.LogText, $r.ReportText)){
                $text | Should Not Match ([regex]::Escape($script:TestSecret))
            }

            # A wrong secret stops the run before any item (exit 4) - still without the secret anywhere.
            $r = SetGroups -Cloud (New-CloudBlock -Secret -SecretValue 'Wrong-Secret-Value-123') -Exo $exo -Items @(@{ changeId = 's2'; externalId = 'E1001'; userId = $u.Jane; add = @($g.Sec) })
            $r.ExitCode | Should Be 4
            $r.Json.ok | Should Be $false
            foreach ($text in @($r.StdOut, $r.StdErr, $r.LogText)){
                $text | Should Not Match 'Wrong-Secret-Value-123'
            }

            # Exchange Online with a client secret is refused up front.
            $bad = New-CloudBlock -Secret
            $bad.exchange.enabled = $true
            $r = SetGroups -Cloud $bad -Exo $exo -Items @()
            $r.ExitCode | Should Be 2
        }
    }
    finally{
        Stop-FakeGraph $fake
        Remove-TestCertificate $cert
    }
}
