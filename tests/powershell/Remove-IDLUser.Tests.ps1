$here = Split-Path -Parent $MyInvocation.MyCommand.Path
. (Join-Path $here 'TestHelpers.ps1')

Describe 'Remove-IDLUser.ps1' {

    function New-Fixture {
        $off = New-StubUserEntry -Sam 'old1' -ExternalId 'E1003' -Cn 'Old One' -Enabled $false -Ou "OU=Disabled,$script:Root"
        $off2 = New-StubUserEntry -Sam 'old2' -ExternalId 'E1004' -Cn 'Old Two' -Enabled $false -Ou "OU=Disabled,$script:Root"
        $on = New-StubUserEntry -Sam 'jdoe' -ExternalId 'E1001' -Cn 'Jane Doe'
        $pc = New-StubUserEntry -Sam 'PC01$' -ExternalId 'E2001' -Cn 'PC01' -Enabled $false -ObjectClass 'computer'
        $path = New-StandardState -Users @($off, $off2, $on, $pc)
        return @{ State = $path; Off = $off; Off2 = $off2; On = $on; Pc = $pc }
    }

    function New-RemoveRequest {
        param([object[]]$Items)
        return [ordered]@{ server = ''; idAttribute = 'employeeID'; items = @($Items) }
    }

    It 'deletes a disabled account and returns its identifiers' {
        $f = New-Fixture
        $item = [ordered]@{ changeId = 'c1'; externalId = 'E1003'; objectGuid = $f.Off.objectGuid }
        $r = Invoke-IDLScript -Script 'Remove-IDLUser' -Request (New-RemoveRequest @($item)) -StatePath $f.State

        $r.ExitCode | Should Be 0
        $r.SingleJson | Should Be $true
        $r.Json.results[0].status | Should Be 'applied'
        $r.Json.results[0].sam | Should Be 'old1'
        $r.Json.results[0].objectGuid | Should Be $f.Off.objectGuid
        (Get-StubUser -Path $f.State -Sam 'old1') | Should BeNullOrEmpty
        (Get-StubUser -Path $f.State -Sam 'jdoe') | Should Not BeNullOrEmpty
        (Get-StubCalls -Path $f.State -Cmdlet 'Remove-ADObject').Count | Should Be 1
    }

    It 'refuses to delete an enabled account' {
        $f = New-Fixture
        $item = [ordered]@{ changeId = 'c1'; externalId = 'E1001'; objectGuid = $f.On.objectGuid }
        $r = Invoke-IDLScript -Script 'Remove-IDLUser' -Request (New-RemoveRequest @($item)) -StatePath $f.State

        $r.ExitCode | Should Be 1
        $r.Json.ok | Should Be $true
        $r.Json.results[0].status | Should Be 'failed'
        $r.Json.results[0].error | Should Match 'account is enabled'
        (Get-StubCalls -Path $f.State).Count | Should Be 0
        (Get-StubUser -Path $f.State -Sam 'jdoe') | Should Not BeNullOrEmpty
    }

    It 'refuses objects that are not users (computer)' {
        $f = New-Fixture
        $item = [ordered]@{ changeId = 'c1'; externalId = 'E2001'; objectGuid = $f.Pc.objectGuid }
        $r = Invoke-IDLScript -Script 'Remove-IDLUser' -Request (New-RemoveRequest @($item)) -StatePath $f.State
        $r.ExitCode | Should Be 1
        $r.Json.results[0].status | Should Be 'failed'
        $r.Json.results[0].error | Should Match 'not a user'
        (Get-StubCalls -Path $f.State).Count | Should Be 0
    }

    It 'refuses on externalId mismatch even for a disabled account' {
        $f = New-Fixture
        $item = [ordered]@{ changeId = 'c1'; externalId = 'E9999'; objectGuid = $f.Off.objectGuid }
        $r = Invoke-IDLScript -Script 'Remove-IDLUser' -Request (New-RemoveRequest @($item)) -StatePath $f.State
        $r.ExitCode | Should Be 1
        $r.Json.results[0].status | Should Be 'failed'
        $r.Json.results[0].error | Should Match 'mismatch'
        (Get-StubUser -Path $f.State -Sam 'old1') | Should Not BeNullOrEmpty
    }

    It 'DryRun deletes nothing, makes zero mutating calls and reports would' {
        $f = New-Fixture
        $item = [ordered]@{ changeId = 'c1'; externalId = 'E1003'; objectGuid = $f.Off.objectGuid }
        $r = Invoke-IDLScript -Script 'Remove-IDLUser' -Request (New-RemoveRequest @($item)) -StatePath $f.State -DryRun
        $r.ExitCode | Should Be 0
        $r.Json.dryRun | Should Be $true
        $r.Json.results[0].status | Should Be 'would'
        (Get-StubCalls -Path $f.State).Count | Should Be 0
        (Get-StubUser -Path $f.State -Sam 'old1') | Should Not BeNullOrEmpty
        $r.LogText | Should Match 'WOULD: delete'
    }

    It 'DryRun still enforces the safety checks (enabled account fails)' {
        $f = New-Fixture
        $item = [ordered]@{ changeId = 'c1'; externalId = 'E1001'; objectGuid = $f.On.objectGuid }
        $r = Invoke-IDLScript -Script 'Remove-IDLUser' -Request (New-RemoveRequest @($item)) -StatePath $f.State -DryRun
        $r.Json.results[0].status | Should Be 'failed'
    }

    It 'is idempotent: an account that is already gone is skipped' {
        $f = New-Fixture
        $item = [ordered]@{ changeId = 'c1'; externalId = 'E1003'; objectGuid = $f.Off.objectGuid }
        $first = Invoke-IDLScript -Script 'Remove-IDLUser' -Request (New-RemoveRequest @($item)) -StatePath $f.State
        $second = Invoke-IDLScript -Script 'Remove-IDLUser' -Request (New-RemoveRequest @($item)) -StatePath $f.State
        $first.Json.results[0].status | Should Be 'applied'
        $second.ExitCode | Should Be 0
        $second.Json.results[0].status | Should Be 'skipped'
        (Get-StubCalls -Path $f.State -Cmdlet 'Remove-ADObject').Count | Should Be 1
    }

    It 'requires objectGuid: an item with only externalId fails and deletes nothing' {
        $f = New-Fixture
        $item = [ordered]@{ changeId = 'c1'; externalId = 'E1004' }
        $r = Invoke-IDLScript -Script 'Remove-IDLUser' -Request (New-RemoveRequest @($item)) -StatePath $f.State
        $r.ExitCode | Should Be 1
        $r.Json.results[0].status | Should Be 'failed'
        $r.Json.results[0].message | Should Match 'objectGuid is required'
        (Get-StubCalls -Path $f.State).Count | Should Be 0
        (Get-StubUser -Path $f.State -Sam 'old2') | Should Not BeNullOrEmpty
    }

    It 'isolates failures across a batch (exit 1) and still deletes the eligible accounts' {
        $f = New-Fixture
        $items = @(
            [ordered]@{ changeId = 'a'; externalId = 'E1003'; objectGuid = $f.Off.objectGuid },
            [ordered]@{ changeId = 'b'; externalId = 'E1001'; objectGuid = $f.On.objectGuid },
            [ordered]@{ changeId = 'c'; externalId = 'E1004'; objectGuid = $f.Off2.objectGuid },
            [ordered]@{ changeId = 'd'; externalId = '' }
        )
        $r = Invoke-IDLScript -Script 'Remove-IDLUser' -Request (New-RemoveRequest $items) -StatePath $f.State
        $r.ExitCode | Should Be 1
        @($r.Json.results | ForEach-Object { $_.status }) -join ',' | Should Be 'applied,failed,applied,failed'
        $r.Json.summary.total | Should Be 4
        (Get-StubUser -Path $f.State -Sam 'old1') | Should BeNullOrEmpty
        (Get-StubUser -Path $f.State -Sam 'old2') | Should BeNullOrEmpty
        (Get-StubUser -Path $f.State -Sam 'jdoe') | Should Not BeNullOrEmpty
    }

    It 'reports an AD error on delete as a per-item failure' {
        $f = New-Fixture
        $state = New-StandardState -Users @($f.Off, $f.Off2) -Faults @(@{ cmdlet = 'Remove-ADObject'; match = $f.Off.objectGuid; message = 'Access is denied' })
        $items = @(
            [ordered]@{ changeId = 'a'; externalId = 'E1003'; objectGuid = $f.Off.objectGuid },
            [ordered]@{ changeId = 'b'; externalId = 'E1004'; objectGuid = $f.Off2.objectGuid }
        )
        $r = Invoke-IDLScript -Script 'Remove-IDLUser' -Request (New-RemoveRequest $items) -StatePath $state
        $r.ExitCode | Should Be 1
        $r.Json.results[0].status | Should Be 'failed'
        $r.Json.results[0].error | Should Match 'Access is denied'
        $r.Json.results[1].status | Should Be 'applied'
    }

    It 'deletes an account that has an Exchange ActiveSync child container (recursive delete, children go too)' {
        $f = New-Fixture
        $userDn = $f.Off.dn
        $objects = @(
            @{ dn = "CN=ExchangeActiveSyncDevices,$userDn"; objectClass = 'msExchActiveSyncDevices' },
            @{ dn = "CN=iPhone-ABC123,CN=ExchangeActiveSyncDevices,$userDn"; objectClass = 'msExchActiveSyncDevice' },
            @{ dn = "CN=Unrelated,OU=Disabled,$script:Root"; objectClass = 'container' }
        )
        $state = New-StandardState -Users @($f.Off, $f.On) -Objects $objects
        $item = [ordered]@{ changeId = 'c1'; externalId = 'E1003'; objectGuid = $f.Off.objectGuid }
        $r = Invoke-IDLScript -Script 'Remove-IDLUser' -Request (New-RemoveRequest @($item)) -StatePath $state

        $r.ExitCode | Should Be 0
        $r.Json.results[0].status | Should Be 'applied'
        $r.Json.results[0].message | Should Match '2 child object'
        $r.LogText | Should Match 'has 2 child object'
        (Get-StubUser -Path $state -Sam 'old1') | Should BeNullOrEmpty
        $left = @((Read-StubStateFile -Path $state).objects)
        $left.Count | Should Be 1
        $left[0].dn | Should Be "CN=Unrelated,OU=Disabled,$script:Root"
        $calls = @(Get-StubCalls -Path $state -Cmdlet 'Remove-ADObject')
        $calls.Count | Should Be 1
        $calls[0].args.Recursive | Should Be 'True'
        (Get-StubCalls -Path $state -Cmdlet 'Remove-ADUser').Count | Should Be 0
    }

    It 'refuses to delete an account whose child objects include a user or computer' {
        $f = New-Fixture
        $userDn = $f.Off.dn
        foreach ($cls in @('computer', 'user')){
            $objects = @(@{ dn = "CN=ExchangeActiveSyncDevices,$userDn"; objectClass = 'msExchActiveSyncDevices' })
            $users = @($f.Off)
            if ($cls -eq 'computer'){
                $objects += , @{ dn = "CN=PC99,$userDn"; objectClass = 'computer' }
            }
            else{
                $users += , (New-StubUserEntry -Sam 'nested1' -ExternalId 'E3001' -Cn 'Nested One' -Ou $userDn)
            }

            $state = New-StandardState -Users $users -Objects $objects
            $item = [ordered]@{ changeId = 'c1'; externalId = 'E1003'; objectGuid = $f.Off.objectGuid }
            foreach ($dry in @($true, $false)){
                $r = if ($dry) { Invoke-IDLScript -Script 'Remove-IDLUser' -Request (New-RemoveRequest @($item)) -StatePath $state -DryRun } else { Invoke-IDLScript -Script 'Remove-IDLUser' -Request (New-RemoveRequest @($item)) -StatePath $state }
                $r.ExitCode | Should Be 1
                $r.Json.results[0].status | Should Be 'failed'
                $r.Json.results[0].error | Should Match "child security principal.*\($cls\)"
            }

            (Get-StubCalls -Path $state).Count | Should Be 0
            (Get-StubUser -Path $state -Sam 'old1') | Should Not BeNullOrEmpty
        }
    }

    It 'DryRun with child objects makes zero mutating calls and reports would' {
        $f = New-Fixture
        $objects = @(@{ dn = "CN=ExchangeActiveSyncDevices,$($f.Off.dn)"; objectClass = 'msExchActiveSyncDevices' })
        $state = New-StandardState -Users @($f.Off) -Objects $objects
        $item = [ordered]@{ changeId = 'c1'; externalId = 'E1003'; objectGuid = $f.Off.objectGuid }
        $r = Invoke-IDLScript -Script 'Remove-IDLUser' -Request (New-RemoveRequest @($item)) -StatePath $state -DryRun
        $r.ExitCode | Should Be 0
        $r.Json.results[0].status | Should Be 'would'
        $r.Json.results[0].message | Should Match '1 child object'
        (Get-StubCalls -Path $state).Count | Should Be 0
        (Get-StubUser -Path $state -Sam 'old1') | Should Not BeNullOrEmpty
        @((Read-StubStateFile -Path $state).objects).Count | Should Be 1
    }

    It 'writes log + CSV report; rejects bad input (2); AD down (4); module missing (3)' {
        $f = New-Fixture
        $item = [ordered]@{ changeId = 'c1'; externalId = 'E1003'; objectGuid = $f.Off.objectGuid }
        $r = Invoke-IDLScript -Script 'Remove-IDLUser' -Request (New-RemoveRequest @($item)) -StatePath $f.State
        $r.LogFile | Should Not BeNullOrEmpty
        $r.ReportFile | Should Not BeNullOrEmpty
        ($r.ReportText -split "`r?`n")[0] | Should Be 'Timestamp,Action,Result,Identity,ObjectIdOrDN,Details,Error'
        $r.ReportText | Should Match 'RemoveUser'

        foreach ($raw in @('', '{"idAttribute":"employeeID"}', '{"idAttribute":"employeeID","items":7}', 'nope')){
            $bad = Invoke-IDLScript -Script 'Remove-IDLUser' -RawInput $raw -StatePath $f.State
            $bad.ExitCode | Should Be 2
            $bad.Json.ok | Should Be $false
        }

        $down = New-StubStateFile -FailConnect $true
        (Invoke-IDLScript -Script 'Remove-IDLUser' -Request (New-RemoveRequest @()) -StatePath $down).ExitCode | Should Be 4
        (Invoke-IDLScript -Script 'Remove-IDLUser' -Request (New-RemoveRequest @()) -NoState).ExitCode | Should Be 3
    }
}
