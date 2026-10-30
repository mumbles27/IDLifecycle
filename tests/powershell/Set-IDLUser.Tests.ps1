$here = Split-Path -Parent $MyInvocation.MyCommand.Path
. (Join-Path $here 'TestHelpers.ps1')

Describe 'Set-IDLUser.ps1' {

    function New-Fixture {
        $jane = New-StubUserEntry -Sam 'jdoe' -ExternalId 'E1001' -Cn 'Jane Doe' -Attributes @{ title = 'Old'; telephoneNumber = '555-0100'; department = 'Ops' } -MemberOf @("CN=GG-Old,OU=Groups,$script:Root")
        $bob = New-StubUserEntry -Sam 'bsmith' -ExternalId 'E1002' -Cn 'Bob Smith' -Attributes @{ title = 'Clerk' }
        $path = New-StandardState -Users @($jane, $bob)
        return @{ State = $path; Jane = $jane; Bob = $bob }
    }

    function New-SetRequest {
        param([object[]]$Items)
        return [ordered]@{ server = ''; idAttribute = 'employeeID'; items = @($Items) }
    }

    It 'applies set/clear/addGroups/removeGroups/moveTo and returns the new DN' {
        $f = New-Fixture
        $item = [ordered]@{
            changeId = 'c1'; externalId = 'E1001'; objectGuid = $f.Jane.objectGuid
            set = [ordered]@{ title = 'New'; displayName = 'Jane Q. Doe' }
            clear = @('telephoneNumber')
            addGroups = @('GG-AllStaff', "CN=GG-Sales,OU=Groups,$script:Root")
            removeGroups = @('GG-Old')
            moveTo = "OU=Sales,$script:Root"
        }
        $r = Invoke-IDLScript -Script 'Set-IDLUser' -Request (New-SetRequest @($item)) -StatePath $f.State

        $r.ExitCode | Should Be 0
        $r.SingleJson | Should Be $true
        $res = $r.Json.results[0]
        $res.status | Should Be 'applied'
        $res.changeId | Should Be 'c1'
        $res.dn | Should Be "CN=Jane Doe,OU=Sales,$script:Root"
        $res.objectGuid | Should Be $f.Jane.objectGuid
        $res.sam | Should Be 'jdoe'
        $res.upn | Should Be 'jdoe@stub.test'

        $u = Get-StubUser -Path $f.State -Sam 'jdoe'
        $u.dn | Should Be "CN=Jane Doe,OU=Sales,$script:Root"
        $u.attributes.title | Should Be 'New'
        $u.attributes.displayName | Should Be 'Jane Q. Doe'
        $u.attributes.PSObject.Properties['telephoneNumber'] | Should BeNullOrEmpty
        $u.attributes.department | Should Be 'Ops'
        $members = @($u.memberOf)
        $members.Count | Should Be 2
        $members -contains "CN=GG-AllStaff,OU=Groups,$script:Root" | Should Be $true
        $members -contains "CN=GG-Sales,OU=Groups,$script:Root" | Should Be $true
        $members -contains "CN=GG-Old,OU=Groups,$script:Root" | Should Be $false

        # one Set-ADUser call carrying both -Replace and -Clear (LDAP names)
        $setCalls = Get-StubCalls -Path $f.State -Cmdlet 'Set-ADUser'
        $setCalls.Count | Should Be 1
        $setCalls[0].args.Replace.title | Should Be 'New'
        @($setCalls[0].args.Clear) -contains 'telephoneNumber' | Should Be $true
        (Get-StubCalls -Path $f.State -Cmdlet 'Move-ADObject').Count | Should Be 1
    }

    It 'DryRun makes zero mutating calls and reports would' {
        $f = New-Fixture
        $item = [ordered]@{ changeId = 'c1'; externalId = 'E1001'; objectGuid = $f.Jane.objectGuid; set = @{ title = 'New' }; clear = @('telephoneNumber'); addGroups = @('GG-AllStaff'); removeGroups = @('GG-Old'); moveTo = "OU=Sales,$script:Root" }
        $r = Invoke-IDLScript -Script 'Set-IDLUser' -Request (New-SetRequest @($item)) -StatePath $f.State -DryRun

        $r.ExitCode | Should Be 0
        $r.Json.dryRun | Should Be $true
        $r.Json.results[0].status | Should Be 'would'
        $r.Json.results[0].dn | Should Be "CN=Jane Doe,OU=Sales,$script:Root"
        $r.Json.summary.would | Should Be 1
        (Get-StubCalls -Path $f.State).Count | Should Be 0
        (Get-StubUser -Path $f.State -Sam 'jdoe').attributes.title | Should Be 'Old'
        $r.LogText | Should Match 'WOULD:'
    }

    It 'is idempotent: re-running the same request reports skipped and makes no calls' {
        $f = New-Fixture
        $item = [ordered]@{ changeId = 'c1'; externalId = 'E1001'; objectGuid = $f.Jane.objectGuid; set = @{ title = 'New' }; clear = @('telephoneNumber'); addGroups = @('GG-AllStaff'); removeGroups = @('GG-Old'); moveTo = "OU=Sales,$script:Root" }
        $first = Invoke-IDLScript -Script 'Set-IDLUser' -Request (New-SetRequest @($item)) -StatePath $f.State
        $callsAfterFirst = (Get-StubCalls -Path $f.State).Count
        $second = Invoke-IDLScript -Script 'Set-IDLUser' -Request (New-SetRequest @($item)) -StatePath $f.State

        $first.Json.results[0].status | Should Be 'applied'
        $second.ExitCode | Should Be 0
        $second.Json.results[0].status | Should Be 'skipped'
        $second.Json.summary.skipped | Should Be 1
        (Get-StubCalls -Path $f.State).Count | Should Be $callsAfterFirst
        $second.Json.results[0].dn | Should Be "CN=Jane Doe,OU=Sales,$script:Root"
    }

    It 'skips no-ops (values already equal, nothing to clear, empty request)' {
        $f = New-Fixture
        $items = @(
            [ordered]@{ changeId = 'a'; externalId = 'E1001'; objectGuid = $f.Jane.objectGuid; set = @{ title = 'Old' } },
            [ordered]@{ changeId = 'b'; externalId = 'E1001'; objectGuid = $f.Jane.objectGuid; clear = @('mobile') },
            [ordered]@{ changeId = 'c'; externalId = 'E1002'; objectGuid = $f.Bob.objectGuid }
        )
        $r = Invoke-IDLScript -Script 'Set-IDLUser' -Request (New-SetRequest $items) -StatePath $f.State
        $r.ExitCode | Should Be 0
        @($r.Json.results | ForEach-Object { $_.status }) -join ',' | Should Be 'skipped,skipped,skipped'
        (Get-StubCalls -Path $f.State).Count | Should Be 0
    }

    It 'requires objectGuid: an item with only externalId fails and nothing is changed' {
        $f = New-Fixture
        $item = [ordered]@{ changeId = 'c1'; externalId = 'E1002'; set = @{ title = 'Lead' } }
        $r = Invoke-IDLScript -Script 'Set-IDLUser' -Request (New-SetRequest @($item)) -StatePath $f.State
        $r.ExitCode | Should Be 1
        $r.Json.results[0].status | Should Be 'failed'
        $r.Json.results[0].message | Should Match 'objectGuid is required'
        (Get-StubCalls -Path $f.State).Count | Should Be 0
        (Get-StubUser -Path $f.State -Sam 'bsmith').attributes.title | Should Be 'Clerk'
    }

    It 'refuses when the account idAttribute does not equal externalId (nothing is touched)' {
        $f = New-Fixture
        $item = [ordered]@{ changeId = 'c1'; externalId = 'E9999'; objectGuid = $f.Jane.objectGuid; set = @{ title = 'Hijack' } }
        $r = Invoke-IDLScript -Script 'Set-IDLUser' -Request (New-SetRequest @($item)) -StatePath $f.State

        $r.ExitCode | Should Be 1
        $r.Json.ok | Should Be $true
        $r.Json.results[0].status | Should Be 'failed'
        $r.Json.results[0].error | Should Match 'mismatch'
        (Get-StubCalls -Path $f.State).Count | Should Be 0
        (Get-StubUser -Path $f.State -Sam 'jdoe').attributes.title | Should Be 'Old'
    }

    It 'fails one bad item (unknown guid) but still processes the others (exit 1)' {
        $f = New-Fixture
        $items = @(
            [ordered]@{ changeId = 'a'; externalId = 'E1001'; objectGuid = $f.Jane.objectGuid; set = @{ title = 'A' } },
            [ordered]@{ changeId = 'b'; externalId = 'E5555'; objectGuid = [guid]::NewGuid().ToString(); set = @{ title = 'B' } },
            [ordered]@{ changeId = 'c'; externalId = 'E1002'; objectGuid = $f.Bob.objectGuid; set = @{ title = 'C' } }
        )
        $r = Invoke-IDLScript -Script 'Set-IDLUser' -Request (New-SetRequest $items) -StatePath $f.State
        $r.ExitCode | Should Be 1
        @($r.Json.results | ForEach-Object { $_.status }) -join ',' | Should Be 'applied,failed,applied'
        $r.Json.results[1].error | Should Match 'no account found'
        (Get-StubUser -Path $f.State -Sam 'bsmith').attributes.title | Should Be 'C'
    }

    It 'isolates an AD error on one item (fault injection)' {
        $f = New-Fixture
        $state = New-StandardState -Users @($f.Jane, $f.Bob) -Faults @(@{ cmdlet = 'Set-ADUser'; match = 'BOOM'; message = 'Insufficient access rights' })
        $items = @(
            [ordered]@{ changeId = 'a'; externalId = 'E1002'; objectGuid = $f.Bob.objectGuid; set = @{ title = 'BOOM' } },
            [ordered]@{ changeId = 'b'; externalId = 'E1001'; objectGuid = $f.Jane.objectGuid; set = @{ title = 'Fine' } }
        )
        $r = Invoke-IDLScript -Script 'Set-IDLUser' -Request (New-SetRequest $items) -StatePath $state
        $r.ExitCode | Should Be 1
        $r.Json.results[0].status | Should Be 'failed'
        $r.Json.results[0].error | Should Match 'Insufficient access'
        $r.Json.results[1].status | Should Be 'applied'
    }

    It 'validates before changing anything: protected attrs, empty values, bad names, unknown add-group, bad OU' {
        $f = New-Fixture
        $guid = $f.Jane.objectGuid
        $bad = @(
            [ordered]@{ changeId = '1'; externalId = 'E1001'; objectGuid = $guid; set = @{ title = 'X'; cn = 'Renamed' } },
            [ordered]@{ changeId = '2'; externalId = 'E1001'; objectGuid = $guid; set = @{ title = 'X'; employeeID = 'E7' } },
            [ordered]@{ changeId = '3'; externalId = 'E1001'; objectGuid = $guid; set = @{ title = '' } },
            [ordered]@{ changeId = '4'; externalId = 'E1001'; objectGuid = $guid; set = @{ 'bad name' = 'x' } },
            [ordered]@{ changeId = '5'; externalId = 'E1001'; objectGuid = $guid; set = @{ title = 'X' }; addGroups = @('GG-Nope') },
            [ordered]@{ changeId = '6'; externalId = 'E1001'; objectGuid = $guid; set = @{ title = 'X' }; moveTo = "OU=Nowhere,$script:Root" },
            [ordered]@{ changeId = '7'; externalId = 'E1001'; objectGuid = $guid; set = @{ title = 'X' }; clear = @('title') },
            [ordered]@{ changeId = '8'; externalId = 'E1001'; objectGuid = $guid; set = @{ memberOf = 'x' } },
            [ordered]@{ changeId = '9'; externalId = 'E1001'; objectGuid = 'not-a-guid'; set = @{ title = 'X' } }
        )
        $r = Invoke-IDLScript -Script 'Set-IDLUser' -Request (New-SetRequest $bad) -StatePath $f.State
        $r.ExitCode | Should Be 1
        @($r.Json.results | Where-Object { $_.status -eq 'failed' }).Count | Should Be 9
        (Get-StubCalls -Path $f.State).Count | Should Be 0
        $r.Json.results[4].error | Should Match 'GG-Nope'
        $r.Json.results[5].error | Should Match 'Target OU'
    }

    It 'ignores removeGroups that do not exist (nothing to remove) and adds groups by DN' {
        $f = New-Fixture
        $item = [ordered]@{ changeId = 'c1'; externalId = 'E1002'; objectGuid = $f.Bob.objectGuid; addGroups = @("CN=GG-Sales,OU=Groups,$script:Root"); removeGroups = @('GG-Ghost') }
        $r = Invoke-IDLScript -Script 'Set-IDLUser' -Request (New-SetRequest @($item)) -StatePath $f.State
        $r.ExitCode | Should Be 0
        $r.Json.results[0].status | Should Be 'applied'
        @((Get-StubUser -Path $f.State -Sam 'bsmith').memberOf) -contains "CN=GG-Sales,OU=Groups,$script:Root" | Should Be $true
        $r.LogText | Should Match "WARN\] .*GG-Ghost"
    }

    It 'moves a user only when not already in the target OU' {
        $f = New-Fixture
        $item = [ordered]@{ changeId = 'c1'; externalId = 'E1001'; objectGuid = $f.Jane.objectGuid; moveTo = "OU=Staff,$script:Root" }
        $r = Invoke-IDLScript -Script 'Set-IDLUser' -Request (New-SetRequest @($item)) -StatePath $f.State
        $r.Json.results[0].status | Should Be 'skipped'
        (Get-StubCalls -Path $f.State -Cmdlet 'Move-ADObject').Count | Should Be 0
    }

    It 'writes the log and CSV report with the standard columns' {
        $f = New-Fixture
        $item = [ordered]@{ changeId = 'c1'; externalId = 'E1001'; objectGuid = $f.Jane.objectGuid; set = @{ title = 'New' } }
        $r = Invoke-IDLScript -Script 'Set-IDLUser' -Request (New-SetRequest @($item)) -StatePath $f.State
        $r.LogFile | Should Not BeNullOrEmpty
        $r.ReportFile | Should Not BeNullOrEmpty
        ($r.ReportText -split "`r?`n")[0] | Should Be 'Timestamp,Action,Result,Identity,ObjectIdOrDN,Details,Error'
        $r.ReportText | Should Match 'SetAttributes'
        $r.LogText | Should Match 'Phase done: Process items'
        $r.LogText | Should Match 'ExitCode=0'
    }

    It 'rejects invalid input with exit 2, AD down with 4, module missing with 3' {
        $f = New-Fixture
        foreach ($raw in @('{"idAttribute":"employeeID"}', '{"items":[]}', '{"idAttribute":"employeeID","items":{}}', '<xml/>')){
            $r = Invoke-IDLScript -Script 'Set-IDLUser' -RawInput $raw -StatePath $f.State
            $r.ExitCode | Should Be 2
            $r.Json.ok | Should Be $false
            $r.SingleJson | Should Be $true
        }

        $down = New-StubStateFile -FailConnect $true
        (Invoke-IDLScript -Script 'Set-IDLUser' -Request (New-SetRequest @()) -StatePath $down).ExitCode | Should Be 4
        (Invoke-IDLScript -Script 'Set-IDLUser' -Request (New-SetRequest @()) -NoState).ExitCode | Should Be 3
    }

    It 'refuses to add an account to a protected administrative group: the item fails before any change' {
        $jane = New-StubUserEntry -Sam 'jdoe' -ExternalId 'E1001' -Cn 'Jane Doe' -Attributes @{ title = 'Old' }
        $groups = @(
            @{ dn = "CN=GG-AllStaff,OU=Groups,$script:Root"; sam = 'GG-AllStaff' },
            @{ dn = "CN=Administrators,CN=Builtin,$script:Root"; sam = 'Administrators'; objectSid = 'S-1-5-32-544' }
        )
        $state = New-StubStateFile -Users @($jane) -Groups $groups -Ous @("OU=Staff,$script:Root", "OU=Groups,$script:Root")
        $item = [ordered]@{ changeId = 'c1'; externalId = 'E1001'; objectGuid = $jane.objectGuid; set = @{ title = 'New' }; addGroups = @('GG-AllStaff', 'Administrators') }
        $r = Invoke-IDLScript -Script 'Set-IDLUser' -Request (New-SetRequest @($item)) -StatePath $state

        $r.ExitCode | Should Be 1
        $r.Json.results[0].status | Should Be 'failed'
        $r.Json.results[0].error | Should Match 'refusing to add Administrators: it is a protected administrative group \(well-known privileged RID 544\)'
        (Get-StubCalls -Path $state).Count | Should Be 0
        (Get-StubUser -Path $state -Sam 'jdoe').attributes.title | Should Be 'Old'
    }
}

Describe 'Set-IDLUser.ps1 undo support (before, expect, force)' {

    function New-Fixture {
        $jane = New-StubUserEntry -Sam 'jdoe' -ExternalId 'E1001' -Cn 'Jane Doe' -Attributes @{ title = 'Old'; telephoneNumber = '555-0100'; department = 'Ops' } -MemberOf @("CN=GG-Old,OU=Groups,$script:Root")
        $bob = New-StubUserEntry -Sam 'bsmith' -ExternalId 'E1002' -Cn 'Bob Smith' -Attributes @{ title = 'Clerk' }
        $path = New-StandardState -Users @($jane, $bob)
        return @{ State = $path; Jane = $jane; Bob = $bob }
    }

    function New-SetRequest {
        param([object[]]$Items)
        return [ordered]@{ server = ''; idAttribute = 'employeeID'; items = @($Items) }
    }

    function New-JaneItem {
        param($Jane)
        return [ordered]@{
            changeId = 'c1'; externalId = 'E1001'; objectGuid = $Jane.objectGuid
            set = [ordered]@{ title = 'New'; department = 'Ops'; pager = '123' }
            clear = @('telephoneNumber')
            addGroups = @('GG-AllStaff', 'GG-Old')
            removeGroups = @('GG-Sales')
            moveTo = "OU=Sales,$script:Root"
        }
    }

    It 'returns before: old values of the changed attributes only, the groups actually added or removed, the old DN' {
        $f = New-Fixture
        $r = Invoke-IDLScript -Script 'Set-IDLUser' -Request (New-SetRequest @(New-JaneItem $f.Jane)) -StatePath $f.State

        $r.ExitCode | Should Be 0
        $res = $r.Json.results[0]
        $res.status | Should Be 'applied'
        $b = $res.before
        $b.enabled | Should Be $true
        $b.dn | Should Be "CN=Jane Doe,OU=Staff,$script:Root"
        $b.attributes.title | Should Be 'Old'
        $b.attributes.telephoneNumber | Should Be '555-0100'
        @($b.attributes.PSObject.Properties | ForEach-Object { $_.Name }) -contains 'pager' | Should Be $true
        $b.attributes.pager | Should Be $null
        @($b.attributes.PSObject.Properties | ForEach-Object { $_.Name }) -contains 'department' | Should Be $false   # unchanged: not recorded
        @($b.groupsAdded) -join '|' | Should Be "CN=GG-AllStaff,OU=Groups,$script:Root"    # GG-Old: already a member
        @($b.groupsRemoved).Count | Should Be 0                                             # GG-Sales: was not a member
        $res.dn | Should Be "CN=Jane Doe,OU=Sales,$script:Root"
    }

    It 'DryRun returns the same before with zero mutating calls; a skipped item has none' {
        $f = New-Fixture
        $r = Invoke-IDLScript -Script 'Set-IDLUser' -Request (New-SetRequest @(New-JaneItem $f.Jane)) -StatePath $f.State -DryRun

        $r.ExitCode | Should Be 0
        $res = $r.Json.results[0]
        $res.status | Should Be 'would'
        $res.before.attributes.title | Should Be 'Old'
        @($res.before.groupsAdded) -join '|' | Should Be "CN=GG-AllStaff,OU=Groups,$script:Root"
        (Get-StubCalls -Path $f.State).Count | Should Be 0

        $noop = [ordered]@{ changeId = 'c2'; externalId = 'E1002'; objectGuid = $f.Bob.objectGuid; set = @{ title = 'Clerk' } }
        $r = Invoke-IDLScript -Script 'Set-IDLUser' -Request (New-SetRequest @($noop)) -StatePath $f.State
        $r.Json.results[0].status | Should Be 'skipped'
        @($r.Json.results[0].PSObject.Properties | ForEach-Object { $_.Name }) -contains 'before' | Should Be $false
    }

    It 'drift: an account that changed since fails before any change, with every difference listed; other items continue' {
        $f = New-Fixture
        $item = New-JaneItem $f.Jane
        $item['expect'] = [ordered]@{
            enabled     = $false
            dn          = "CN=Jane Doe,OU=Sales,$script:Root"
            attributes  = [ordered]@{ title = 'Senior'; telephoneNumber = $null }
            memberOf    = @("CN=GG-Sales,OU=Groups,$script:Root")
            notMemberOf = @('GG-Old')
        }
        $bob = [ordered]@{ changeId = 'c2'; externalId = 'E1002'; objectGuid = $f.Bob.objectGuid; set = @{ title = 'Lead' } }
        $r = Invoke-IDLScript -Script 'Set-IDLUser' -Request (New-SetRequest @($item, $bob)) -StatePath $f.State

        $r.ExitCode | Should Be 1
        $res = $r.Json.results[0]
        $res.status | Should Be 'failed'
        $res.message | Should Be 'failed'
        $res.error | Should Match '^drift: '
        $res.error | Should Match 'account is enabled, expected disabled'
        $res.error | Should Match "account is at CN=Jane Doe,OU=Staff,DC=stub,DC=test, expected CN=Jane Doe,OU=Sales"
        $res.error | Should Match "title is 'Old', expected 'Senior'"
        $res.error | Should Match "telephoneNumber is '555-0100', expected empty"
        $res.error | Should Match 'no longer a member of CN=GG-Sales'
        $res.error | Should Match 'is a member of GG-Old again'
        @($res.PSObject.Properties | ForEach-Object { $_.Name }) -contains 'before' | Should Be $false
        $r.Json.results[1].status | Should Be 'applied'
        @(Get-StubCalls -Path $f.State | Where-Object { $_.args.Identity -eq $f.Jane.objectGuid -or ($_.args | ConvertTo-Json -Compress) -match 'Jane Doe' }).Count | Should Be 0
        (Get-StubUser -Path $f.State -Sam 'jdoe').attributes.title | Should Be 'Old'
    }

    It 'expect that matches passes; force skips the check (WARN) and applies' {
        $f = New-Fixture
        $item = New-JaneItem $f.Jane
        $item['expect'] = [ordered]@{ enabled = $true; dn = "cn=jane doe,ou=staff,$script:Root"; attributes = @{ title = 'Old'; pager = $null }; memberOf = @('GG-Old'); notMemberOf = @('GG-AllStaff') }
        $r = Invoke-IDLScript -Script 'Set-IDLUser' -Request (New-SetRequest @($item)) -StatePath $f.State -DryRun
        $r.Json.results[0].status | Should Be 'would'

        $f = New-Fixture
        $item = New-JaneItem $f.Jane
        $item['expect'] = [ordered]@{ attributes = @{ title = 'Senior' } }
        $item['force'] = $true
        $r = Invoke-IDLScript -Script 'Set-IDLUser' -Request (New-SetRequest @($item)) -StatePath $f.State
        $r.Json.results[0].status | Should Be 'applied'
        $r.LogText | Should Match 'expect check skipped \(force\)'
        (Get-StubUser -Path $f.State -Sam 'jdoe').attributes.title | Should Be 'New'
    }

    It 'rejects a malformed expect before any change' {
        $f = New-Fixture
        $item = New-JaneItem $f.Jane
        $item['expect'] = [ordered]@{ enabled = 'yes' }
        $r = Invoke-IDLScript -Script 'Set-IDLUser' -Request (New-SetRequest @($item)) -StatePath $f.State
        $r.Json.results[0].status | Should Be 'failed'
        $r.Json.results[0].error | Should Match 'expect.enabled must be true or false'
        (Get-StubCalls -Path $f.State).Count | Should Be 0
    }

    It 'round trip: the inverse built from before (with expect) puts the account back exactly' {
        $f = New-Fixture
        $original = Get-StubUserShape -Path $f.State -Guid $f.Jane.objectGuid
        $r = Invoke-IDLScript -Script 'Set-IDLUser' -Request (New-SetRequest @(New-JaneItem $f.Jane)) -StatePath $f.State
        $applied = $r.Json.results[0]
        (Get-StubUserShape -Path $f.State -Guid $f.Jane.objectGuid) | Should Not Be $original

        $expect = [ordered]@{
            dn          = $applied.dn
            attributes  = [ordered]@{ title = 'New'; pager = '123'; telephoneNumber = $null }
            memberOf    = @($applied.before.groupsAdded)
            notMemberOf = @($applied.before.groupsRemoved)
        }
        $undo = New-UndoItem -Result $applied -Expect $expect
        $r = Invoke-IDLScript -Script 'Set-IDLUser' -Request (New-SetRequest @($undo)) -StatePath $f.State

        $r.ExitCode | Should Be 0
        $r.Json.results[0].status | Should Be 'applied'
        (Get-StubUserShape -Path $f.State -Guid $f.Jane.objectGuid) | Should Be $original
        # The undo's own before is the redo record.
        $r.Json.results[0].before.attributes.title | Should Be 'New'
    }
}
