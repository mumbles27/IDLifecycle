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
