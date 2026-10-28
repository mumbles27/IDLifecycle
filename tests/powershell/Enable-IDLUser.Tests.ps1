$here = Split-Path -Parent $MyInvocation.MyCommand.Path
. (Join-Path $here 'TestHelpers.ps1')

Describe 'Enable-IDLUser.ps1' {

    function New-Fixture {
        $off = New-StubUserEntry -Sam 'old1' -ExternalId 'E1003' -Cn 'Old One' -Enabled $false -Ou "OU=Disabled,$script:Root"
        $on = New-StubUserEntry -Sam 'jdoe' -ExternalId 'E1001' -Cn 'Jane Doe' -MemberOf @("CN=GG-AllStaff,OU=Groups,$script:Root")
        $path = New-StandardState -Users @($off, $on)
        return @{ State = $path; Off = $off; On = $on }
    }

    function New-EnableRequest {
        param([object[]]$Items)
        return [ordered]@{ server = ''; idAttribute = 'employeeID'; items = @($Items) }
    }

    It 'enables the account, adds groups (sam and DN) and moves it back' {
        $f = New-Fixture
        $item = [ordered]@{ changeId = 'c1'; externalId = 'E1003'; objectGuid = $f.Off.objectGuid; moveTo = "OU=Staff,$script:Root"; addGroups = @('GG-AllStaff', "CN=GG-Sales,OU=Groups,$script:Root") }
        $r = Invoke-IDLScript -Script 'Enable-IDLUser' -Request (New-EnableRequest @($item)) -StatePath $f.State

        $r.ExitCode | Should Be 0
        $r.SingleJson | Should Be $true
        $r.Json.results[0].status | Should Be 'applied'
        $r.Json.results[0].dn | Should Be "CN=Old One,OU=Staff,$script:Root"
        $u = Get-StubUser -Path $f.State -Sam 'old1'
        $u.enabled | Should Be $true
        $u.dn | Should Be "CN=Old One,OU=Staff,$script:Root"
        @($u.memberOf).Count | Should Be 2
    }

    It 'DryRun makes zero mutating calls and reports would' {
        $f = New-Fixture
        $item = [ordered]@{ changeId = 'c1'; externalId = 'E1003'; objectGuid = $f.Off.objectGuid; moveTo = "OU=Staff,$script:Root"; addGroups = @('GG-AllStaff') }
        $r = Invoke-IDLScript -Script 'Enable-IDLUser' -Request (New-EnableRequest @($item)) -StatePath $f.State -DryRun
        $r.ExitCode | Should Be 0
        $r.Json.dryRun | Should Be $true
        $r.Json.results[0].status | Should Be 'would'
        (Get-StubCalls -Path $f.State).Count | Should Be 0
        (Get-StubUser -Path $f.State -Sam 'old1').enabled | Should Be $false
    }

    It 'is idempotent: re-run is skipped; an already-enabled account is skipped' {
        $f = New-Fixture
        $item = [ordered]@{ changeId = 'c1'; externalId = 'E1003'; objectGuid = $f.Off.objectGuid; moveTo = "OU=Staff,$script:Root"; addGroups = @('GG-AllStaff') }
        $null = Invoke-IDLScript -Script 'Enable-IDLUser' -Request (New-EnableRequest @($item)) -StatePath $f.State
        $before = (Get-StubCalls -Path $f.State).Count
        $r = Invoke-IDLScript -Script 'Enable-IDLUser' -Request (New-EnableRequest @($item)) -StatePath $f.State
        $r.Json.results[0].status | Should Be 'skipped'

        $already = [ordered]@{ changeId = 'c2'; externalId = 'E1001'; objectGuid = $f.On.objectGuid }
        $r2 = Invoke-IDLScript -Script 'Enable-IDLUser' -Request (New-EnableRequest @($already)) -StatePath $f.State
        $r2.Json.results[0].status | Should Be 'skipped'
        (Get-StubCalls -Path $f.State).Count | Should Be $before
    }

    It 'refuses on externalId mismatch' {
        $f = New-Fixture
        $item = [ordered]@{ changeId = 'c1'; externalId = 'E0000'; objectGuid = $f.Off.objectGuid }
        $r = Invoke-IDLScript -Script 'Enable-IDLUser' -Request (New-EnableRequest @($item)) -StatePath $f.State
        $r.ExitCode | Should Be 1
        $r.Json.results[0].status | Should Be 'failed'
        $r.Json.results[0].error | Should Match 'mismatch'
        (Get-StubCalls -Path $f.State).Count | Should Be 0
    }

    It 'fails before any change when a group or the OU does not exist; other items continue' {
        $f = New-Fixture
        $items = @(
            [ordered]@{ changeId = 'a'; externalId = 'E1003'; objectGuid = $f.Off.objectGuid; addGroups = @('GG-Nope') },
            [ordered]@{ changeId = 'b'; externalId = 'E1003'; objectGuid = $f.Off.objectGuid; moveTo = "OU=Nowhere,$script:Root" },
            [ordered]@{ changeId = 'c'; externalId = 'E1003'; objectGuid = $f.Off.objectGuid }
        )
        $r = Invoke-IDLScript -Script 'Enable-IDLUser' -Request (New-EnableRequest $items) -StatePath $f.State
        $r.ExitCode | Should Be 1
        @($r.Json.results | ForEach-Object { $_.status }) -join ',' | Should Be 'failed,failed,applied'
        $r.Json.results[0].error | Should Match 'GG-Nope'
        (Get-StubCalls -Path $f.State -Cmdlet 'Enable-ADAccount').Count | Should Be 1
    }

    It 'writes log + report and validates input / dependency / connection exit codes' {
        $f = New-Fixture
        $item = [ordered]@{ changeId = 'c1'; externalId = 'E1003'; objectGuid = $f.Off.objectGuid }
        $r = Invoke-IDLScript -Script 'Enable-IDLUser' -Request (New-EnableRequest @($item)) -StatePath $f.State
        $r.LogFile | Should Not BeNullOrEmpty
        $r.ReportFile | Should Not BeNullOrEmpty
        $r.ReportText | Should Match 'EnableAccount'

        $bad = Invoke-IDLScript -Script 'Enable-IDLUser' -RawInput '{"items":[]}' -StatePath $f.State
        $bad.ExitCode | Should Be 2
        $bad.Json.ok | Should Be $false

        $down = New-StubStateFile -FailConnect $true
        (Invoke-IDLScript -Script 'Enable-IDLUser' -Request (New-EnableRequest @()) -StatePath $down).ExitCode | Should Be 4
        (Invoke-IDLScript -Script 'Enable-IDLUser' -Request (New-EnableRequest @()) -NoState).ExitCode | Should Be 3
    }
}

Describe 'Enable-IDLUser.ps1 removeGroups and privileged groups' {

    function New-EnFixture {
        $off = New-StubUserEntry -Sam 'old1' -ExternalId 'E1003' -Cn 'Old One' -Enabled $false -Ou "OU=Disabled,$script:Root" -MemberOf @("CN=GG-Disabled,OU=Groups,$script:Root", "CN=Domain Admins,CN=Users,$script:Root")
        $groups = @(
            @{ dn = "CN=GG-AllStaff,OU=Groups,$script:Root"; sam = 'GG-AllStaff' },
            @{ dn = "CN=GG-Disabled,OU=Groups,$script:Root"; sam = 'GG-Disabled' },
            @{ dn = "CN=Domain Admins,CN=Users,$script:Root"; sam = 'Domain Admins'; objectSid = 'S-1-5-21-1-2-3-512' }
        )
        $path = New-StubStateFile -Users @($off) -Groups $groups -Ous @("OU=Staff,$script:Root", "OU=Disabled,$script:Root", "OU=Groups,$script:Root")
        return @{ State = $path; Off = $off }
    }

    function New-EnRequest {
        param($Item)
        return [ordered]@{ server = ''; idAttribute = 'employeeID'; items = @($Item) }
    }

    It 'enables, adds the groups and leaves removeGroups; a privileged removeGroups entry is kept, an unknown one ignored' {
        $f = New-EnFixture
        $item = [ordered]@{ changeId = 'c1'; externalId = 'E1003'; objectGuid = $f.Off.objectGuid; addGroups = @('GG-AllStaff'); removeGroups = @('GG-Disabled', 'Domain Admins', 'GG-Nope') }
        $r = Invoke-IDLScript -Script 'Enable-IDLUser' -Request (New-EnRequest $item) -StatePath $f.State

        $r.ExitCode | Should Be 0
        $res = $r.Json.results[0]
        $res.status | Should Be 'applied'
        $res.message | Should Match 'removed from CN=GG-Disabled'
        $res.message | Should Match 'kept privileged group Domain Admins'
        $u = Get-StubUser -Path $f.State -Sam 'old1'
        $u.enabled | Should Be $true
        @($u.memberOf) -contains "CN=GG-AllStaff,OU=Groups,$script:Root" | Should Be $true
        @($u.memberOf) -contains "CN=GG-Disabled,OU=Groups,$script:Root" | Should Be $false
        @($u.memberOf) -contains "CN=Domain Admins,CN=Users,$script:Root" | Should Be $true
        $r.LogText | Should Match "Group 'GG-Nope' not found; nothing to remove"

        $before = (Get-StubCalls -Path $f.State).Count
        $r = Invoke-IDLScript -Script 'Enable-IDLUser' -Request (New-EnRequest $item) -StatePath $f.State
        $r.Json.results[0].status | Should Be 'skipped'
        (Get-StubCalls -Path $f.State).Count | Should Be $before
    }

    It 'DryRun: WOULD remove, zero mutating calls' {
        $f = New-EnFixture
        $item = [ordered]@{ changeId = 'c1'; externalId = 'E1003'; objectGuid = $f.Off.objectGuid; removeGroups = @('GG-Disabled') }
        $r = Invoke-IDLScript -Script 'Enable-IDLUser' -Request (New-EnRequest $item) -StatePath $f.State -DryRun
        $r.Json.results[0].status | Should Be 'would'
        $r.LogText | Should Match 'WOULD: remove old1 from CN=GG-Disabled'
        (Get-StubCalls -Path $f.State).Count | Should Be 0
    }

    It 'refuses to add the account to a privileged group: the item fails before any change' {
        $f = New-EnFixture
        $item = [ordered]@{ changeId = 'c1'; externalId = 'E1003'; objectGuid = $f.Off.objectGuid; addGroups = @('GG-AllStaff', 'Domain Admins') }
        $r = Invoke-IDLScript -Script 'Enable-IDLUser' -Request (New-EnRequest $item) -StatePath $f.State

        $r.ExitCode | Should Be 1
        $r.Json.results[0].status | Should Be 'failed'
        $r.Json.results[0].error | Should Match 'refusing to add Domain Admins: it is a protected administrative group'
        (Get-StubCalls -Path $f.State).Count | Should Be 0
        (Get-StubUser -Path $f.State -Sam 'old1').enabled | Should Be $false
    }
}
