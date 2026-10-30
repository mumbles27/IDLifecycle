$here = Split-Path -Parent $MyInvocation.MyCommand.Path
. (Join-Path $here 'TestHelpers.ps1')

Describe 'Disable-IDLUser.ps1' {

    function New-Fixture {
        $g = "CN=GG-AllStaff,OU=Groups,$script:Root"
        $jane = New-StubUserEntry -Sam 'jdoe' -ExternalId 'E1001' -Cn 'Jane Doe' -MemberOf @($g, "CN=GG-Sales,OU=Groups,$script:Root")
        $bob = New-StubUserEntry -Sam 'bsmith' -ExternalId 'E1002' -Cn 'Bob Smith'
        $gone = New-StubUserEntry -Sam 'old1' -ExternalId 'E1003' -Cn 'Old One' -Enabled $false -Ou "OU=Disabled,$script:Root"
        $path = New-StandardState -Users @($jane, $bob, $gone)
        return @{ State = $path; Jane = $jane; Bob = $bob; Gone = $gone }
    }

    function New-DisableRequest {
        param([object[]]$Items)
        return [ordered]@{ server = ''; idAttribute = 'employeeID'; items = @($Items) }
    }

    It 'disables, stamps the description, removes groups and moves to the disabled OU' {
        $f = New-Fixture
        $item = [ordered]@{ changeId = 'c1'; externalId = 'E1001'; objectGuid = $f.Jane.objectGuid; moveTo = "OU=Disabled,$script:Root"; removeGroups = @('GG-AllStaff', "CN=GG-Sales,OU=Groups,$script:Root"); description = 'Disabled by IDLifecycle 2026-10-16' }
        $r = Invoke-IDLScript -Script 'Disable-IDLUser' -Request (New-DisableRequest @($item)) -StatePath $f.State

        $r.ExitCode | Should Be 0
        $r.SingleJson | Should Be $true
        $res = $r.Json.results[0]
        $res.status | Should Be 'applied'
        $res.dn | Should Be "CN=Jane Doe,OU=Disabled,$script:Root"
        $res.objectGuid | Should Be $f.Jane.objectGuid

        $u = Get-StubUser -Path $f.State -Sam 'jdoe'
        $u.enabled | Should Be $false
        $u.dn | Should Be "CN=Jane Doe,OU=Disabled,$script:Root"
        $u.attributes.description | Should Be 'Disabled by IDLifecycle 2026-10-16'
        @($u.memberOf).Count | Should Be 0
        (Get-StubCalls -Path $f.State -Cmdlet 'Disable-ADAccount').Count | Should Be 1
        (Get-StubCalls -Path $f.State -Cmdlet 'Remove-ADGroupMember').Count | Should Be 2
    }

    It 'DryRun makes zero mutating calls and reports would' {
        $f = New-Fixture
        $item = [ordered]@{ changeId = 'c1'; externalId = 'E1001'; objectGuid = $f.Jane.objectGuid; moveTo = "OU=Disabled,$script:Root"; removeGroups = @('GG-AllStaff'); description = 'x1234' }
        $r = Invoke-IDLScript -Script 'Disable-IDLUser' -Request (New-DisableRequest @($item)) -StatePath $f.State -DryRun

        $r.ExitCode | Should Be 0
        $r.Json.dryRun | Should Be $true
        $r.Json.results[0].status | Should Be 'would'
        (Get-StubCalls -Path $f.State).Count | Should Be 0
        (Get-StubUser -Path $f.State -Sam 'jdoe').enabled | Should Be $true
        $r.LogText | Should Match 'WOULD: disable'
    }

    It 'is idempotent: second run is skipped and makes no further calls' {
        $f = New-Fixture
        $item = [ordered]@{ changeId = 'c1'; externalId = 'E1001'; objectGuid = $f.Jane.objectGuid; moveTo = "OU=Disabled,$script:Root"; removeGroups = @('GG-AllStaff'); description = 'Disabled by IDLifecycle' }
        $null = Invoke-IDLScript -Script 'Disable-IDLUser' -Request (New-DisableRequest @($item)) -StatePath $f.State
        $before = (Get-StubCalls -Path $f.State).Count
        $r = Invoke-IDLScript -Script 'Disable-IDLUser' -Request (New-DisableRequest @($item)) -StatePath $f.State

        $r.ExitCode | Should Be 0
        $r.Json.results[0].status | Should Be 'skipped'
        (Get-StubCalls -Path $f.State).Count | Should Be $before
    }

    It 'an already-disabled account still gets the move/groups/description applied' {
        $f = New-Fixture
        $item = [ordered]@{ changeId = 'c1'; externalId = 'E1003'; objectGuid = $f.Gone.objectGuid; description = 'Tagged later' }
        $r = Invoke-IDLScript -Script 'Disable-IDLUser' -Request (New-DisableRequest @($item)) -StatePath $f.State
        $r.Json.results[0].status | Should Be 'applied'
        (Get-StubCalls -Path $f.State -Cmdlet 'Disable-ADAccount').Count | Should Be 0
        (Get-StubUser -Path $f.State -Sam 'old1').attributes.description | Should Be 'Tagged later'
    }

    It 'refuses on externalId mismatch and leaves the account untouched' {
        $f = New-Fixture
        $item = [ordered]@{ changeId = 'c1'; externalId = 'E4242'; objectGuid = $f.Jane.objectGuid; removeGroups = @('GG-AllStaff') }
        $r = Invoke-IDLScript -Script 'Disable-IDLUser' -Request (New-DisableRequest @($item)) -StatePath $f.State
        $r.ExitCode | Should Be 1
        $r.Json.results[0].status | Should Be 'failed'
        $r.Json.results[0].error | Should Match 'mismatch'
        (Get-StubCalls -Path $f.State).Count | Should Be 0
        (Get-StubUser -Path $f.State -Sam 'jdoe').enabled | Should Be $true
    }

    It 'isolates failures: unknown account and bad OU fail individually, the rest proceeds (exit 1)' {
        $f = New-Fixture
        $items = @(
            [ordered]@{ changeId = 'a'; externalId = 'E1001'; objectGuid = $f.Jane.objectGuid },
            [ordered]@{ changeId = 'b'; externalId = 'E7777'; objectGuid = [guid]::NewGuid().ToString() },
            [ordered]@{ changeId = 'c'; externalId = 'E1002'; objectGuid = $f.Bob.objectGuid; moveTo = "OU=Nowhere,$script:Root" },
            [ordered]@{ changeId = 'd'; externalId = ''; objectGuid = $f.Bob.objectGuid }
        )
        $r = Invoke-IDLScript -Script 'Disable-IDLUser' -Request (New-DisableRequest $items) -StatePath $f.State
        $r.ExitCode | Should Be 1
        @($r.Json.results | ForEach-Object { $_.status }) -join ',' | Should Be 'applied,failed,failed,failed'
        $r.Json.summary.failed | Should Be 3
        (Get-StubUser -Path $f.State -Sam 'jdoe').enabled | Should Be $false
        (Get-StubUser -Path $f.State -Sam 'bsmith').enabled | Should Be $false
        $r.Json.results[2].message | Should Match '^failed after: disabled'
    }

    It 'ignores removeGroups that do not exist and groups the user is not in' {
        $f = New-Fixture
        $item = [ordered]@{ changeId = 'c1'; externalId = 'E1002'; objectGuid = $f.Bob.objectGuid; removeGroups = @('GG-Ghost', 'GG-AllStaff') }
        $r = Invoke-IDLScript -Script 'Disable-IDLUser' -Request (New-DisableRequest @($item)) -StatePath $f.State
        $r.Json.results[0].status | Should Be 'applied'
        (Get-StubCalls -Path $f.State -Cmdlet 'Remove-ADGroupMember').Count | Should Be 0
        (Get-StubCalls -Path $f.State -Cmdlet 'Disable-ADAccount').Count | Should Be 1
    }

    function New-PrivilegedFixture {
        $ous = @("OU=Staff,$script:Root", "OU=Disabled,$script:Root", "OU=Groups,$script:Root")
        $groups = @(
            @{ dn = "CN=GG-AllStaff,OU=Groups,$script:Root"; sam = 'GG-AllStaff' },
            @{ dn = "CN=GG-Nested-AO,OU=Groups,$script:Root"; sam = 'GG-Nested-AO'; adminCount = 1; objectSid = 'S-1-5-21-1-2-3-4100' },
            @{ dn = "CN=Administrators,CN=Builtin,$script:Root"; sam = 'Administrators'; objectSid = 'S-1-5-32-544' },
            @{ dn = "CN=GG-Dup,OU=Groups,$script:Root"; sam = 'GG-Dup' },
            @{ dn = "CN=GG-Dup,OU=Staff,$script:Root"; sam = 'GG-Dup' }
        )
        $memberOf = @("CN=GG-AllStaff,OU=Groups,$script:Root", "CN=GG-Nested-AO,OU=Groups,$script:Root", "CN=Administrators,CN=Builtin,$script:Root")
        $term = New-StubUserEntry -Sam 'term1' -ExternalId 'E5001' -Cn 'Term One' -MemberOf $memberOf
        $path = New-StubStateFile -Users @($term) -Groups $groups -Ous $ous
        $item = [ordered]@{
            changeId = 'p1'; externalId = 'E5001'; objectGuid = $term.objectGuid; moveTo = "OU=Disabled,$script:Root"
            removeGroups = @('GG-AllStaff', "CN=GG-Nested-AO,OU=Groups,$script:Root", 'Administrators'); description = 'Terminated'
        }
        return @{ State = $path; User = $term; Item = $item }
    }

    It 'privileged groups never block the disable: disabled, normal group removed, privileged kept, status applied' {
        $f = New-PrivilegedFixture
        $r = Invoke-IDLScript -Script 'Disable-IDLUser' -Request (New-DisableRequest @($f.Item)) -StatePath $f.State

        $r.ExitCode | Should Be 0
        $res = $r.Json.results[0]
        $res.status | Should Be 'applied'
        $res.message | Should Match 'kept privileged group\(s\): .*GG-Nested-AO'
        $res.message | Should Match 'Administrators'
        $res.dn | Should Be "CN=Term One,OU=Disabled,$script:Root"

        $u = Get-StubUser -Path $f.State -Sam 'term1'
        $u.enabled | Should Be $false
        @($u.memberOf) -contains "CN=GG-AllStaff,OU=Groups,$script:Root" | Should Be $false
        @($u.memberOf) -contains "CN=GG-Nested-AO,OU=Groups,$script:Root" | Should Be $true
        @($u.memberOf) -contains "CN=Administrators,CN=Builtin,$script:Root" | Should Be $true
        (Get-StubCalls -Path $f.State -Cmdlet 'Disable-ADAccount').Count | Should Be 1
        $removes = @(Get-StubCalls -Path $f.State -Cmdlet 'Remove-ADGroupMember')
        $removes.Count | Should Be 1
        ($removes[0].args | ConvertTo-Json -Compress) | Should Match 'GG-AllStaff'
        $r.LogText | Should Match "WARN.*Group 'Administrators' is privileged group"
        $r.ReportText | Should Match '"RemoveGroupMember","Skipped"'
    }

    It 'privileged groups in DryRun: zero mutating calls, status would, kept groups reported' {
        $f = New-PrivilegedFixture
        $r = Invoke-IDLScript -Script 'Disable-IDLUser' -Request (New-DisableRequest @($f.Item)) -StatePath $f.State -DryRun

        $r.ExitCode | Should Be 0
        $r.Json.results[0].status | Should Be 'would'
        $r.Json.results[0].message | Should Match 'would disable'
        $r.Json.results[0].message | Should Match 'kept privileged group\(s\)'
        (Get-StubCalls -Path $f.State).Count | Should Be 0
        (Get-StubUser -Path $f.State -Sam 'term1').enabled | Should Be $true
    }

    It 'an already-disabled account with only privileged groups left is skipped and says what was kept' {
        $f = New-PrivilegedFixture
        $null = Invoke-IDLScript -Script 'Disable-IDLUser' -Request (New-DisableRequest @($f.Item)) -StatePath $f.State
        $before = (Get-StubCalls -Path $f.State).Count
        $r = Invoke-IDLScript -Script 'Disable-IDLUser' -Request (New-DisableRequest @($f.Item)) -StatePath $f.State
        $r.ExitCode | Should Be 0
        $r.Json.results[0].status | Should Be 'skipped'
        $r.Json.results[0].message | Should Match 'kept privileged group\(s\)'
        (Get-StubCalls -Path $f.State).Count | Should Be $before
    }

    It 'ambiguous removeGroups are skipped with a warning, not a failure' {
        $f = New-PrivilegedFixture
        $f.Item.removeGroups = @('GG-Dup', 'GG-AllStaff')
        $r = Invoke-IDLScript -Script 'Disable-IDLUser' -Request (New-DisableRequest @($f.Item)) -StatePath $f.State
        $r.ExitCode | Should Be 0
        $r.Json.results[0].status | Should Be 'applied'
        $r.Json.results[0].message | Should Match 'kept ambiguous group\(s\): GG-Dup'
        $r.LogText | Should Match "WARN.*Group 'GG-Dup' is ambiguous"
        (Get-StubUser -Path $f.State -Sam 'term1').enabled | Should Be $false
    }

    It 'an invalid or missing moveTo still disables the account but reports failed' {
        foreach ($bad in @('not a dn', "OU=Nowhere,$script:Root")){
            $f = New-PrivilegedFixture
            $f.Item.moveTo = $bad
            $r = Invoke-IDLScript -Script 'Disable-IDLUser' -Request (New-DisableRequest @($f.Item)) -StatePath $f.State
            $r.ExitCode | Should Be 1
            $res = $r.Json.results[0]
            $res.status | Should Be 'failed'
            $res.message | Should Match '^failed after: disabled'
            $res.error | Should Match '(not a distinguished name|does not exist)'
            (Get-StubUser -Path $f.State -Sam 'term1').enabled | Should Be $false
            (Get-StubCalls -Path $f.State -Cmdlet 'Disable-ADAccount').Count | Should Be 1
            (Get-StubCalls -Path $f.State -Cmdlet 'Move-ADObject').Count | Should Be 0
        }
    }

    It 'writes the log and the CSV report' {
        $f = New-Fixture
        $item = [ordered]@{ changeId = 'c1'; externalId = 'E1001'; objectGuid = $f.Jane.objectGuid }
        $r = Invoke-IDLScript -Script 'Disable-IDLUser' -Request (New-DisableRequest @($item)) -StatePath $f.State
        $r.LogFile | Should Not BeNullOrEmpty
        $r.ReportFile | Should Not BeNullOrEmpty
        ($r.ReportText -split "`r?`n")[0] | Should Be 'Timestamp,Action,Result,Identity,ObjectIdOrDN,Details,Error'
        $r.ReportText | Should Match 'DisableAccount'
        $r.LogText | Should Match 'ExitCode=0'
    }

    It 'rejects invalid input (2), AD unreachable (4), module missing (3)' {
        $f = New-Fixture
        foreach ($raw in @('{}', '{"idAttribute":"employeeID"}', '{"idAttribute":"employeeID","items":"x"}', 'garbage')){
            $r = Invoke-IDLScript -Script 'Disable-IDLUser' -RawInput $raw -StatePath $f.State
            $r.ExitCode | Should Be 2
            $r.Json.ok | Should Be $false
            $r.SingleJson | Should Be $true
        }

        $down = New-StubStateFile -FailConnect $true
        (Invoke-IDLScript -Script 'Disable-IDLUser' -Request (New-DisableRequest @()) -StatePath $down).ExitCode | Should Be 4
        (Invoke-IDLScript -Script 'Disable-IDLUser' -Request (New-DisableRequest @()) -NoState).ExitCode | Should Be 3
    }
}

Describe 'Disable-IDLUser.ps1 addGroups (lifecycle groups for disabled accounts)' {

    function New-AddFixture {
        $jane = New-StubUserEntry -Sam 'jdoe' -ExternalId 'E1001' -Cn 'Jane Doe' -MemberOf @("CN=GG-AllStaff,OU=Groups,$script:Root")
        $groups = @(
            @{ dn = "CN=GG-AllStaff,OU=Groups,$script:Root"; sam = 'GG-AllStaff' },
            @{ dn = "CN=GG-Disabled,OU=Groups,$script:Root"; sam = 'GG-Disabled' },
            @{ dn = "CN=Domain Admins,CN=Users,$script:Root"; sam = 'Domain Admins'; objectSid = 'S-1-5-21-1-2-3-512' }
        )
        $path = New-StubStateFile -Users @($jane) -Groups $groups -Ous @("OU=Staff,$script:Root", "OU=Disabled,$script:Root", "OU=Groups,$script:Root")
        return @{ State = $path; Jane = $jane }
    }

    function New-AddRequest {
        param($Item)
        return [ordered]@{ server = ''; idAttribute = 'employeeID'; items = @($Item) }
    }

    It 'adds the account to addGroups after disabling it; privileged and unknown groups are reported, never a failure' {
        $f = New-AddFixture
        $item = [ordered]@{ changeId = 'c1'; externalId = 'E1001'; objectGuid = $f.Jane.objectGuid; removeGroups = @('GG-AllStaff'); addGroups = @('GG-Disabled', 'Domain Admins', 'GG-Nope') }
        $r = Invoke-IDLScript -Script 'Disable-IDLUser' -Request (New-AddRequest $item) -StatePath $f.State

        $r.ExitCode | Should Be 0
        $res = $r.Json.results[0]
        $res.status | Should Be 'applied'
        $res.message | Should Match 'added to CN=GG-Disabled'
        $res.message | Should Match 'not added: .*Domain Admins \(privileged\)'
        $res.message | Should Match 'GG-Nope \(not found\)'
        $u = Get-StubUser -Path $f.State -Sam 'jdoe'
        $u.enabled | Should Be $false
        @($u.memberOf) -join '|' | Should Be "CN=GG-Disabled,OU=Groups,$script:Root"
        # Order: the disable comes first, the group change after it; nobody is added to the admin group.
        $calls = Get-StubCalls -Path $f.State
        $calls[0].cmdlet | Should Be 'Disable-ADAccount'
        @($calls | Where-Object { $_.cmdlet -eq 'Add-ADGroupMember' }).Count | Should Be 1
        ($calls | ConvertTo-Json -Depth 6) | Should Not Match 'Domain Admins'
        $r.LogText | Should Match "WARN.*Group 'Domain Admins' is a protected administrative group"
        $r.ReportText | Should Match '"AddGroupMember","Skipped"'

        # Idempotent: a second run has nothing to do (already disabled, already a member).
        $before = (Get-StubCalls -Path $f.State).Count
        $r = Invoke-IDLScript -Script 'Disable-IDLUser' -Request (New-AddRequest $item) -StatePath $f.State
        $r.Json.results[0].status | Should Be 'skipped'
        (Get-StubCalls -Path $f.State).Count | Should Be $before
    }

    It 'DryRun: WOULD lines for the disable and the group, zero mutating calls' {
        $f = New-AddFixture
        $item = [ordered]@{ changeId = 'c1'; externalId = 'E1001'; objectGuid = $f.Jane.objectGuid; addGroups = @('GG-Disabled') }
        $r = Invoke-IDLScript -Script 'Disable-IDLUser' -Request (New-AddRequest $item) -StatePath $f.State -DryRun

        $r.ExitCode | Should Be 0
        $r.Json.results[0].status | Should Be 'would'
        $r.LogText | Should Match 'WOULD: disable jdoe'
        $r.LogText | Should Match 'WOULD: add jdoe to CN=GG-Disabled'
        (Get-StubCalls -Path $f.State).Count | Should Be 0
    }
}

Describe 'Disable-IDLUser.ps1 undo support (before, set/clear, expect, force)' {

    function New-Fixture {
        $jane = New-StubUserEntry -Sam 'jdoe' -ExternalId 'E1001' -Cn 'Jane Doe' -Attributes @{ description = 'Analyst team'; title = 'Analyst' } -MemberOf @("CN=GG-AllStaff,OU=Groups,$script:Root", "CN=GG-Sales,OU=Groups,$script:Root")
        $path = New-StandardState -Users @($jane)
        return @{ State = $path; Jane = $jane }
    }

    function New-DisableRequest {
        param([object[]]$Items)
        return [ordered]@{ server = ''; idAttribute = 'employeeID'; items = @($Items) }
    }

    function New-JaneItem {
        param($Jane)
        return [ordered]@{
            changeId = 'c1'; externalId = 'E1001'; objectGuid = $Jane.objectGuid
            moveTo = "OU=Disabled,$script:Root"; removeGroups = @('GG-AllStaff', 'GG-Old'); addGroups = @('GG-Old')
            description = 'Disabled by IDLifecycle 2026-10-29'
        }
    }

    It 'returns before: enabled, DN, the old description and only the groups actually changed (applied and DryRun)' {
        foreach ($dry in @($true, $false)){
            $f = New-Fixture
            $r = Invoke-IDLScript -Script 'Disable-IDLUser' -Request (New-DisableRequest @(New-JaneItem $f.Jane)) -StatePath $f.State -DryRun:$dry

            $r.ExitCode | Should Be 0
            $b = $r.Json.results[0].before
            $b.enabled | Should Be $true
            $b.dn | Should Be "CN=Jane Doe,OU=Staff,$script:Root"
            $b.attributes.description | Should Be 'Analyst team'
            @($b.groupsRemoved) -join '|' | Should Be "CN=GG-AllStaff,OU=Groups,$script:Root"   # GG-Old: was not a member
            @($b.groupsAdded) -join '|' | Should Be "CN=GG-Old,OU=Groups,$script:Root"
            if ($dry){
                (Get-StubCalls -Path $f.State).Count | Should Be 0
            }
        }
    }

    It 'applies set/clear after disabling and records the old values' {
        $f = New-Fixture
        $item = [ordered]@{ changeId = 'c1'; externalId = 'E1001'; objectGuid = $f.Jane.objectGuid; set = @{ title = 'Former analyst' }; clear = @('description') }
        $r = Invoke-IDLScript -Script 'Disable-IDLUser' -Request (New-DisableRequest @($item)) -StatePath $f.State

        $r.Json.results[0].status | Should Be 'applied'
        $r.Json.results[0].before.attributes.title | Should Be 'Analyst'
        $r.Json.results[0].before.attributes.description | Should Be 'Analyst team'
        $u = Get-StubUser -Path $f.State -Sam 'jdoe'
        $u.enabled | Should Be $false
        $u.attributes.title | Should Be 'Former analyst'
        @($u.attributes.PSObject.Properties | ForEach-Object { $_.Name }) -contains 'description' | Should Be $false
        $calls = @(Get-StubCalls -Path $f.State | ForEach-Object { $_.cmdlet })
        $calls.IndexOf('Disable-ADAccount') | Should BeLessThan $calls.IndexOf('Set-ADUser')
    }

    It 'refuses before the disable: drift, a protected attribute in set, description given twice' {
        $f = New-Fixture
        $drift = New-JaneItem $f.Jane
        $drift['expect'] = [ordered]@{ enabled = $false; attributes = @{ description = 'Something else' } }
        $protected = [ordered]@{ changeId = 'c2'; externalId = 'E1001'; objectGuid = $f.Jane.objectGuid; set = @{ employeeID = 'X' } }
        $twice = New-JaneItem $f.Jane
        $twice.changeId = 'c3'
        $twice['set'] = @{ description = 'other' }
        $r = Invoke-IDLScript -Script 'Disable-IDLUser' -Request (New-DisableRequest @($drift, $protected, $twice)) -StatePath $f.State

        $r.ExitCode | Should Be 1
        $r.Json.results[0].error | Should Match "^drift: account is enabled, expected disabled; description is 'Analyst team', expected 'Something else'$"
        $r.Json.results[1].error | Should Match 'protected'
        $r.Json.results[2].error | Should Match 'both as a field and in set/clear'
        foreach ($res in $r.Json.results){
            $res.message | Should Be 'failed'
        }

        (Get-StubCalls -Path $f.State).Count | Should Be 0
        (Get-StubUser -Path $f.State -Sam 'jdoe').enabled | Should Be $true
    }

    It 'force overrides the expect check' {
        $f = New-Fixture
        $item = New-JaneItem $f.Jane
        $item['expect'] = [ordered]@{ enabled = $false }
        $item['force'] = $true
        $r = Invoke-IDLScript -Script 'Disable-IDLUser' -Request (New-DisableRequest @($item)) -StatePath $f.State
        $r.Json.results[0].status | Should Be 'applied'
        (Get-StubUser -Path $f.State -Sam 'jdoe').enabled | Should Be $false
    }

    It 'round trip: Enable-IDLUser with the inverse built from before puts the account back exactly' {
        $f = New-Fixture
        $original = Get-StubUserShape -Path $f.State -Guid $f.Jane.objectGuid
        $r = Invoke-IDLScript -Script 'Disable-IDLUser' -Request (New-DisableRequest @(New-JaneItem $f.Jane)) -StatePath $f.State
        $applied = $r.Json.results[0]
        $applied.status | Should Be 'applied'

        $expect = [ordered]@{
            enabled     = $false
            dn          = $applied.dn
            attributes  = @{ description = 'Disabled by IDLifecycle 2026-10-29' }
            memberOf    = @($applied.before.groupsAdded)
            notMemberOf = @($applied.before.groupsRemoved)
        }
        $undo = New-UndoItem -Result $applied -Expect $expect
        $r = Invoke-IDLScript -Script 'Enable-IDLUser' -Request (New-DisableRequest @($undo)) -StatePath $f.State

        $r.ExitCode | Should Be 0
        $r.Json.results[0].status | Should Be 'applied'
        (Get-StubUserShape -Path $f.State -Guid $f.Jane.objectGuid) | Should Be $original
    }
}
