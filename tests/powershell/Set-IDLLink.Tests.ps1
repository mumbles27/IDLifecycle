$here = Split-Path -Parent $MyInvocation.MyCommand.Path
. (Join-Path $here 'TestHelpers.ps1')

Describe 'Set-IDLLink.ps1' {

    $contractorGuid = '22222222-2222-2222-2222-222222222222'
    $oldGuid = '33333333-3333-3333-3333-333333333333'

    function New-LinkState {
        param([object[]]$Extra = @(), [object[]]$Objects = @(), [object[]]$Faults = @(), [bool]$FailConnect = $false)

        $contractor = New-StubUserEntry -Sam 'jdoe.c' -ExternalId 'C-100' -Cn 'Jane Doe (Contractor)' -Guid $contractorGuid -Attributes @{
            mail           = 'jdoe.c@stub.test'
            proxyAddresses = @('SMTP:jdoe.c@stub.test', 'smtp:jdoe.contractor@vendor.example', 'X500:/o=Stub/cn=jdoe.c')
        }
        $unlinked = New-StubUserEntry -Sam 'old.bob' -ExternalId '' -Cn 'Bob Old' -Guid $oldGuid
        return (New-StandardState -Users (@($contractor, $unlinked) + $Extra) -Objects $Objects -Faults $Faults -FailConnect $FailConnect)
    }

    function New-LinkItem {
        param(
            [string]$ChangeId = 'chg-1',
            [string]$Guid = $contractorGuid,
            [string]$ExternalId = 'E5678',
            $Expected = 'C-100',
            $Rename = $null
        )

        return [ordered]@{ changeId = $ChangeId; objectGuid = $Guid; externalId = $ExternalId; expectedExternalId = $Expected; rename = $Rename }
    }

    function New-Request {
        param([object[]]$Items)
        return [ordered]@{ server = ''; idAttribute = 'employeeID'; items = @($Items) }
    }

    $fullRename = [ordered]@{ sam = 'jdoe'; upn = 'jane.doe@stub.test'; cn = 'Jane Doe'; mail = 'jane.doe@stub.test' }

    It 'links a contractor account to the new id and renames sam, upn, mail (proxyAddresses primary) and cn' {
        $state = New-LinkState
        $r = Invoke-IDLScript -Script 'Set-IDLLink' -Request (New-Request @(New-LinkItem -Rename $fullRename)) -StatePath $state

        $r.ExitCode | Should Be 0
        $r.SingleJson | Should Be $true
        $r.Json.ok | Should Be $true
        $r.Json.dryRun | Should Be $false
        $res = $r.Json.results[0]
        $res.status | Should Be 'applied'
        $res.changeId | Should Be 'chg-1'
        $res.externalId | Should Be 'E5678'
        $res.objectGuid | Should Be $contractorGuid
        $res.sam | Should Be 'jdoe'
        $res.upn | Should Be 'jane.doe@stub.test'
        $res.dn | Should Be "CN=Jane Doe,OU=Staff,$script:Root"
        $res.message | Should Match 'set employeeID'
        $res.message | Should Match "renamed .*sAMAccountName 'jdoe.c' -> 'jdoe'"

        $user = Get-StubUser -Path $state -Sam 'jdoe'
        $user.attributes.employeeID | Should Be 'E5678'
        $user.upn | Should Be 'jane.doe@stub.test'
        $user.dn | Should Be "CN=Jane Doe,OU=Staff,$script:Root"
        $user.attributes.mail | Should Be 'jane.doe@stub.test'
        (@($user.attributes.proxyAddresses) -join '|') | Should Be 'SMTP:jane.doe@stub.test|smtp:jdoe.contractor@vendor.example|X500:/o=Stub/cn=jdoe.c|smtp:jdoe.c@stub.test'

        $calls = Get-StubCalls -Path $state
        (@($calls | ForEach-Object { $_.cmdlet }) -join ',') | Should Be 'Set-ADUser,Rename-ADObject'
        $calls[0].args.SamAccountName | Should Be 'jdoe'
        $calls[0].args.Replace.employeeID | Should Be 'E5678'
    }

    It 'links an account whose id attribute is unset (expectedExternalId "" or null) without renaming' {
        $state = New-LinkState
        $items = @(
            (New-LinkItem -ChangeId 'a' -Guid $oldGuid -ExternalId 'E2001' -Expected '')
        )
        $r = Invoke-IDLScript -Script 'Set-IDLLink' -Request (New-Request $items) -StatePath $state
        $r.ExitCode | Should Be 0
        $r.Json.results[0].status | Should Be 'applied'
        $r.Json.results[0].sam | Should Be 'old.bob'
        (Get-StubUser -Path $state -Sam 'old.bob').attributes.employeeID | Should Be 'E2001'
        (Get-StubCalls -Path $state -Cmdlet 'Rename-ADObject').Count | Should Be 0

        $state = New-LinkState
        $r = Invoke-IDLScript -Script 'Set-IDLLink' -Request (New-Request @(New-LinkItem -Guid $oldGuid -ExternalId 'E2001' -Expected $null)) -StatePath $state
        $r.Json.results[0].status | Should Be 'applied'
    }

    It 'DryRun makes zero mutating calls and reports the names and DN the change would produce' {
        $state = New-LinkState
        $r = Invoke-IDLScript -Script 'Set-IDLLink' -Request (New-Request @(New-LinkItem -Rename $fullRename)) -StatePath $state -DryRun
        $r.ExitCode | Should Be 0
        $r.Json.dryRun | Should Be $true
        $res = $r.Json.results[0]
        $res.status | Should Be 'would'
        $res.sam | Should Be 'jdoe'
        $res.dn | Should Be "CN=Jane Doe,OU=Staff,$script:Root"
        $res.message | Should Match 'would set employeeID'
        $res.message | Should Match 'would rename cn'
        (Get-StubCalls -Path $state).Count | Should Be 0
        (Get-StubUser -Path $state -Sam 'jdoe.c').attributes.employeeID | Should Be 'C-100'
        $r.LogText | Should Match 'WOULD:'
    }

    It 'refuses a mismatching current id attribute, and an externalId another object already carries' {
        $holder = New-StubUserEntry -Sam 'real.jane' -ExternalId 'E5678' -Cn 'Real Jane'
        $state = New-LinkState -Extra @($holder)
        $items = @(
            (New-LinkItem -ChangeId 'wrong-expected' -Expected 'C-999'),
            (New-LinkItem -ChangeId 'expected-unset' -Expected ''),
            (New-LinkItem -ChangeId 'taken' -Guid $oldGuid -Expected '')
        )
        $r = Invoke-IDLScript -Script 'Set-IDLLink' -Request (New-Request $items) -StatePath $state
        $r.ExitCode | Should Be 1
        $r.Json.summary.failed | Should Be 3
        $r.Json.results[0].error | Should Match "employeeID mismatch: .*'C-100' but the change expects 'C-999'"
        $r.Json.results[1].error | Should Match 'expects it to be unset'
        $r.Json.results[2].error | Should Match "employeeID 'E5678' is already used by CN=Real Jane"
        (Get-StubCalls -Path $state).Count | Should Be 0
    }

    It 'refuses rename names used by other objects (sam, upn, mail via a contact proxyAddresses, cn in the OU) but not by the account itself' {
        $other = New-StubUserEntry -Sam 'jdoe' -ExternalId 'E0001' -Cn 'John Doe'
        $contact = @{ dn = "CN=Jane Vendor,OU=Sales,$script:Root"; objectClass = 'contact'; attributes = @{ proxyAddresses = @('SMTP:jane@vendor.example', 'smtp:jane.doe@stub.test') } }
        $cnHolder = New-StubUserEntry -Sam 'jd2' -ExternalId 'E0002' -Cn 'Jane Doe'
        $state = New-LinkState -Extra @($other, $cnHolder) -Objects @($contact)
        $items = @(
            (New-LinkItem -ChangeId 'sam' -Rename @{ sam = 'jdoe' }),
            (New-LinkItem -ChangeId 'upn' -Rename @{ upn = 'jd2@stub.test' }),
            (New-LinkItem -ChangeId 'mail' -Rename @{ mail = 'Jane.Doe@stub.test' }),
            (New-LinkItem -ChangeId 'cn' -Rename @{ cn = 'Jane Doe' })
        )
        $r = Invoke-IDLScript -Script 'Set-IDLLink' -Request (New-Request $items) -StatePath $state
        $r.ExitCode | Should Be 1
        $r.Json.summary.failed | Should Be 4
        $r.Json.results[0].error | Should Match "sAMAccountName 'jdoe' is used by CN=John Doe"
        $r.Json.results[1].error | Should Match "userPrincipalName 'jd2@stub.test' is used by CN=Jane Doe"
        $r.Json.results[2].error | Should Match "mail 'Jane.Doe@stub.test' is used by CN=Jane Vendor"
        $r.Json.results[3].error | Should Match "cn 'Jane Doe' is used by CN=Jane Doe,OU=Staff"
        (Get-StubCalls -Path $state).Count | Should Be 0

        # The account's own names (a change of case) never block it.
        $state = New-LinkState
        $r = Invoke-IDLScript -Script 'Set-IDLLink' -Request (New-Request @(New-LinkItem -Rename @{ sam = 'JDoe.C'; mail = 'JDOE.C@stub.test' })) -StatePath $state
        $r.ExitCode | Should Be 0
        $r.Json.results[0].sam | Should Be 'JDoe.C'
        (Get-StubUser -Path $state -Sam 'JDoe.C').attributes.mail | Should Be 'JDOE.C@stub.test'
    }

    It 'is idempotent: an already linked account with nothing to rename is skipped; a retry only renames' {
        $state = New-LinkState
        $r = Invoke-IDLScript -Script 'Set-IDLLink' -Request (New-Request @(New-LinkItem -ExternalId 'C-100' -Expected 'C-100')) -StatePath $state
        $r.ExitCode | Should Be 0
        $r.Json.results[0].status | Should Be 'skipped'
        $r.Json.results[0].message | Should Be 'already linked'

        # The link step was applied before (id already E5678), the expected value is stale: only the rename is done.
        $first = Invoke-IDLScript -Script 'Set-IDLLink' -Request (New-Request @(New-LinkItem)) -StatePath $state
        $first.Json.results[0].status | Should Be 'applied'
        $r = Invoke-IDLScript -Script 'Set-IDLLink' -Request (New-Request @(New-LinkItem -Rename @{ sam = 'jdoe' })) -StatePath $state
        $r.Json.results[0].status | Should Be 'applied'
        $r.Json.results[0].message | Should Not Match 'set employeeID'
        $last = (Get-StubCalls -Path $state -Cmdlet 'Set-ADUser')[-1]
        [bool]$last.args.PSObject.Properties['Replace'] | Should Be $false
        $last.args.SamAccountName | Should Be 'jdoe'
    }

    It 'refuses privileged accounts: adminCount=1, or a direct member of a protected group (RID or adminCount)' {
        $admin = New-StubUserEntry -Sam 'adm.jane' -ExternalId '' -Cn 'Jane Admin' -Guid '55555555-5555-5555-5555-555555555555' -Attributes @{ adminCount = '1' }
        $da = New-StubUserEntry -Sam 'da.jane' -ExternalId '' -Cn 'Jane DA' -Guid '66666666-6666-6666-6666-666666666666' -MemberOf @("CN=Domain Admins,CN=Users,$script:Root")
        $ops = New-StubUserEntry -Sam 'ops.jane' -ExternalId '' -Cn 'Jane Ops' -Guid '77777777-7777-7777-7777-777777777777' -MemberOf @("CN=GG-Tier0,OU=Groups,$script:Root")
        $plain = New-StubUserEntry -Sam 'plain.jane' -ExternalId '' -Cn 'Jane Plain' -Guid '88888888-8888-8888-8888-888888888888' -MemberOf @("CN=GG-AllStaff,OU=Groups,$script:Root")
        $groups = @(
            @{ dn = "CN=Domain Admins,CN=Users,$script:Root"; sam = 'Domain Admins'; objectSid = 'S-1-5-21-1-2-3-512' },
            @{ dn = "CN=GG-Tier0,OU=Groups,$script:Root"; sam = 'GG-Tier0'; adminCount = 1 },
            @{ dn = "CN=GG-AllStaff,OU=Groups,$script:Root"; sam = 'GG-AllStaff' }
        )
        $state = New-StubStateFile -Users @($admin, $da, $ops, $plain) -Groups $groups -Ous @("OU=Staff,$script:Root", "OU=Groups,$script:Root", "CN=Users,$script:Root")
        $items = @(
            (New-LinkItem -ChangeId 'admincount' -Guid '55555555-5555-5555-5555-555555555555' -Expected '' -ExternalId 'E9001'),
            (New-LinkItem -ChangeId 'rid' -Guid '66666666-6666-6666-6666-666666666666' -Expected '' -ExternalId 'E9002' -Rename @{ sam = 'jane9' }),
            (New-LinkItem -ChangeId 'groupadmincount' -Guid '77777777-7777-7777-7777-777777777777' -Expected '' -ExternalId 'E9003'),
            (New-LinkItem -ChangeId 'plain' -Guid '88888888-8888-8888-8888-888888888888' -Expected '' -ExternalId 'E9004')
        )
        $r = Invoke-IDLScript -Script 'Set-IDLLink' -Request (New-Request $items) -StatePath $state
        $r.ExitCode | Should Be 1
        $r.Json.results[0].status | Should Be 'failed'
        $r.Json.results[0].error | Should Match 'is privileged \(adminCount=1\)'
        $r.Json.results[1].error | Should Match 'member of the privileged group CN=Domain Admins.*RID 512'
        $r.Json.results[2].error | Should Match 'member of the privileged group CN=GG-Tier0.*adminCount=1'
        $r.Json.results[3].status | Should Be 'applied'
        $calls = Get-StubCalls -Path $state
        $calls.Count | Should Be 1
        $calls[0].args.Identity | Should Be '88888888-8888-8888-8888-888888888888'

        $r = Invoke-IDLScript -Script 'Set-IDLLink' -Request (New-Request @($items[0])) -StatePath $state -DryRun
        $r.Json.results[0].status | Should Be 'failed'
    }

    It 'counts external ids and names used earlier in the same batch' {
        $third = New-StubUserEntry -Sam 'third' -ExternalId '' -Cn 'Third' -Guid '44444444-4444-4444-4444-444444444444'
        $state = New-LinkState -Extra @($third)
        $items = @(
            (New-LinkItem -ChangeId 'a' -Rename @{ sam = 'jdoe' }),
            (New-LinkItem -ChangeId 'b' -Guid $oldGuid -Expected '' -ExternalId 'E5678'),
            (New-LinkItem -ChangeId 'c' -Guid '44444444-4444-4444-4444-444444444444' -Expected '' -ExternalId 'E7777' -Rename @{ sam = 'jdoe' })
        )
        $r = Invoke-IDLScript -Script 'Set-IDLLink' -Request (New-Request $items) -StatePath $state
        $r.ExitCode | Should Be 1
        $r.Json.results[0].status | Should Be 'applied'
        $r.Json.results[1].error | Should Match 'earlier in this batch'
        $r.Json.results[2].error | Should Match "sAMAccountName 'jdoe' is taken by another account earlier in this batch"
    }

    It 'fails invalid or unknown items individually and isolates an AD error (exit 1)' {
        $state = New-LinkState -Faults @(@{ cmdlet = 'Set-ADUser'; match = 'E2001'; message = 'insufficient access rights' })
        $items = @(
            (New-LinkItem -ChangeId 'bad-guid' -Guid 'nope'),
            (New-LinkItem -ChangeId 'unknown' -Guid '99999999-9999-9999-9999-999999999999'),
            (New-LinkItem -ChangeId 'bad-sam' -Rename @{ sam = 'way-too-long-for-a-sam-account' }),
            (New-LinkItem -ChangeId 'bad-sam2' -Rename @{ sam = 'j doe' }),
            (New-LinkItem -ChangeId 'empty-rename' -Rename @{ sam = '' }),
            (New-LinkItem -ChangeId 'no-ext' -ExternalId '' -Expected ''),
            (New-LinkItem -ChangeId 'ad-error' -Guid $oldGuid -Expected '' -ExternalId 'E2001'),
            (New-LinkItem -ChangeId 'ok')
        )
        $r = Invoke-IDLScript -Script 'Set-IDLLink' -Request (New-Request $items) -StatePath $state
        $r.ExitCode | Should Be 1
        $r.Json.results[0].error | Should Match 'objectGuid is required'
        $r.Json.results[1].error | Should Match 'no account found'
        $r.Json.results[2].error | Should Match 'not a valid sAMAccountName'
        $r.Json.results[3].error | Should Match 'not a valid sAMAccountName'
        $r.Json.results[4].error | Should Match 'has no names'
        $r.Json.results[5].error | Should Match 'externalId is required'
        $r.Json.results[6].error | Should Match 'insufficient access rights'
        $r.Json.results[6].message | Should Be 'failed'
        $r.Json.results[7].status | Should Be 'applied'
        $r.Json.summary.failed | Should Be 7
        $r.ReportText | Should Match 'LinkUser'
    }

    It 'rejects invalid requests with exit code 2 and exits 4 when AD is unreachable' {
        $state = New-LinkState
        foreach ($request in @(
                @{ idAttribute = 'employeeID' },
                @{ idAttribute = 'employeeID'; items = 'x' },
                @{ idAttribute = 'bad attr'; items = @() },
                @{ idAttribute = 'sAMAccountName'; items = @() },
                @{ idAttribute = 'mail'; items = @() }
            )){
            $r = Invoke-IDLScript -Script 'Set-IDLLink' -Request $request -StatePath $state
            $r.ExitCode | Should Be 2
            $r.Json.ok | Should Be $false
        }

        $r = Invoke-IDLScript -Script 'Set-IDLLink' -Request (New-Request @()) -StatePath $state
        $r.ExitCode | Should Be 0
        @($r.Json.results).Count | Should Be 0

        $down = New-LinkState -FailConnect $true
        $r = Invoke-IDLScript -Script 'Set-IDLLink' -Request (New-Request @(New-LinkItem)) -StatePath $down
        $r.ExitCode | Should Be 4
        $r.Json.ok | Should Be $false
    }
}

Describe 'Set-IDLLink.ps1 undo support (before, unlink, proxyAddresses, expect)' {

    $contractorGuid = '22222222-2222-2222-2222-222222222222'
    $oldGuid = '33333333-3333-3333-3333-333333333333'

    function New-LinkState {
        $contractor = New-StubUserEntry -Sam 'jdoe.c' -ExternalId 'C-100' -Cn 'Jane Doe (Contractor)' -Guid $contractorGuid -Attributes @{
            mail           = 'jdoe.c@stub.test'
            proxyAddresses = @('SMTP:jdoe.c@stub.test', 'smtp:jdoe.contractor@vendor.example', 'X500:/o=Stub/cn=jdoe.c')
        }
        $unlinked = New-StubUserEntry -Sam 'old.bob' -ExternalId '' -Cn 'Bob Old' -Guid $oldGuid
        return (New-StandardState -Users @($contractor, $unlinked))
    }

    function New-Request {
        param([object[]]$Items)
        return [ordered]@{ server = ''; idAttribute = 'employeeID'; items = @($Items) }
    }

    $rename = [ordered]@{ sam = 'jdoe'; upn = 'jdoe@stub.test'; cn = 'Jane Doe'; mail = 'jane.doe@stub.test' }

    It 'returns before: the old ID value and DN; names with the full proxyAddresses list only when it renames' {
        foreach ($dry in @($true, $false)){
            $state = New-LinkState
            $item = [ordered]@{ changeId = 'c1'; objectGuid = $contractorGuid; externalId = 'E5678'; expectedExternalId = 'C-100'; rename = $rename }
            $r = Invoke-IDLScript -Script 'Set-IDLLink' -Request (New-Request @($item)) -StatePath $state -DryRun:$dry

            $r.ExitCode | Should Be 0
            $b = $r.Json.results[0].before
            $b.idValue | Should Be 'C-100'
            $b.dn | Should Be "CN=Jane Doe (Contractor),OU=Staff,$script:Root"
            $b.names.sam | Should Be 'jdoe.c'
            $b.names.upn | Should Be 'jdoe.c@stub.test'
            $b.names.cn | Should Be 'Jane Doe (Contractor)'
            $b.names.mail | Should Be 'jdoe.c@stub.test'
            @($b.names.proxyAddresses) -join '|' | Should Be 'SMTP:jdoe.c@stub.test|smtp:jdoe.contractor@vendor.example|X500:/o=Stub/cn=jdoe.c'
        }

        $state = New-LinkState
        $item = [ordered]@{ changeId = 'c2'; objectGuid = $oldGuid; externalId = 'E2001'; expectedExternalId = '' }
        $r = Invoke-IDLScript -Script 'Set-IDLLink' -Request (New-Request @($item)) -StatePath $state
        $b = $r.Json.results[0].before
        $b.idValue | Should Be ''
        @($b.PSObject.Properties | ForEach-Object { $_.Name }) -contains 'names' | Should Be $false
    }

    It 'unlinks: an empty externalId clears the ID attribute when it still has expectedExternalId; refuses otherwise' {
        $state = New-LinkState
        $r = Invoke-IDLScript -Script 'Set-IDLLink' -Request (New-Request @([ordered]@{ changeId = 'c1'; objectGuid = $oldGuid; externalId = 'E2001'; expectedExternalId = '' })) -StatePath $state
        $r.Json.results[0].status | Should Be 'applied'

        $wrong = [ordered]@{ changeId = 'u1'; objectGuid = $oldGuid; externalId = ''; expectedExternalId = 'E9999' }
        $r = Invoke-IDLScript -Script 'Set-IDLLink' -Request (New-Request @($wrong)) -StatePath $state
        $r.Json.results[0].status | Should Be 'failed'
        $r.Json.results[0].error | Should Match "employeeID mismatch.*expects 'E9999'"

        $dry = [ordered]@{ changeId = 'u2'; objectGuid = $oldGuid; externalId = ''; expectedExternalId = 'E2001' }
        $r = Invoke-IDLScript -Script 'Set-IDLLink' -Request (New-Request @($dry)) -StatePath $state -DryRun
        $r.Json.results[0].status | Should Be 'would'
        $r.Json.results[0].message | Should Match 'would clear employeeID'
        (Get-StubUser -Path $state -Sam 'old.bob').attributes.employeeID | Should Be 'E2001'

        $r = Invoke-IDLScript -Script 'Set-IDLLink' -Request (New-Request @($dry)) -StatePath $state
        $r.Json.results[0].status | Should Be 'applied'
        $r.Json.results[0].externalId | Should Be ''
        $r.Json.results[0].before.idValue | Should Be 'E2001'
        @((Get-StubUser -Path $state -Sam 'old.bob').attributes.PSObject.Properties | ForEach-Object { $_.Name }) -contains 'employeeID' | Should Be $false

        # Already unset: nothing to do.
        $r = Invoke-IDLScript -Script 'Set-IDLLink' -Request (New-Request @($dry)) -StatePath $state
        $r.Json.results[0].status | Should Be 'skipped'
    }

    It 'drift (ID value or names changed since) fails before any change; a bad proxyAddresses entry is refused' {
        $state = New-LinkState
        $item = [ordered]@{ changeId = 'c1'; objectGuid = $contractorGuid; externalId = 'E5678'; expectedExternalId = 'C-100'; expect = [ordered]@{ idValue = 'C-999'; names = @{ sam = 'someone' } } }
        $bad = [ordered]@{ changeId = 'c2'; objectGuid = $contractorGuid; externalId = 'E5678'; expectedExternalId = 'C-100'; proxyAddresses = @('not-an-address') }
        $r = Invoke-IDLScript -Script 'Set-IDLLink' -Request (New-Request @($item, $bad)) -StatePath $state
        $r.Json.results[0].error | Should Be "drift: employeeID is 'C-100', expected 'C-999'; sam is 'jdoe.c', expected 'someone'"
        $r.Json.results[1].error | Should Match 'not of the form type:address'
        (Get-StubCalls -Path $state).Count | Should Be 0
    }

    It 'round trip: link with rename, then the inverse (old id, old names, exact proxyAddresses) puts the account back' {
        $state = New-LinkState
        $original = Get-StubUserShape -Path $state -Guid $contractorGuid
        $item = [ordered]@{ changeId = 'c1'; objectGuid = $contractorGuid; externalId = 'E5678'; expectedExternalId = 'C-100'; rename = $rename }
        $r = Invoke-IDLScript -Script 'Set-IDLLink' -Request (New-Request @($item)) -StatePath $state
        $applied = $r.Json.results[0]
        $applied.status | Should Be 'applied'
        (Get-StubUserShape -Path $state -Guid $contractorGuid) | Should Not Be $original

        $b = $applied.before
        $undo = [ordered]@{
            changeId = 'u1'; objectGuid = $contractorGuid; externalId = $b.idValue; expectedExternalId = 'E5678'
            rename = [ordered]@{ sam = $b.names.sam; upn = $b.names.upn; cn = $b.names.cn; mail = $b.names.mail }
            proxyAddresses = @($b.names.proxyAddresses)
            expect = [ordered]@{ idValue = 'E5678'; dn = $applied.dn; names = $rename }
        }
        $r = Invoke-IDLScript -Script 'Set-IDLLink' -Request (New-Request @($undo)) -StatePath $state
        $r.ExitCode | Should Be 0
        $r.Json.results[0].status | Should Be 'applied'
        (Get-StubUserShape -Path $state -Guid $contractorGuid) | Should Be $original

        # Unlinked account: link, then unlink.
        $original = Get-StubUserShape -Path $state -Guid $oldGuid
        $r = Invoke-IDLScript -Script 'Set-IDLLink' -Request (New-Request @([ordered]@{ changeId = 'c2'; objectGuid = $oldGuid; externalId = 'E2001'; expectedExternalId = '' })) -StatePath $state
        $b = $r.Json.results[0].before
        $r = Invoke-IDLScript -Script 'Set-IDLLink' -Request (New-Request @([ordered]@{ changeId = 'u2'; objectGuid = $oldGuid; externalId = $b.idValue; expectedExternalId = 'E2001'; expect = @{ idValue = 'E2001' } })) -StatePath $state
        $r.Json.results[0].status | Should Be 'applied'
        (Get-StubUserShape -Path $state -Guid $oldGuid) | Should Be $original
    }
}
