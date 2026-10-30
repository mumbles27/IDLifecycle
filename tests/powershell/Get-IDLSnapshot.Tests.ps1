$here = Split-Path -Parent $MyInvocation.MyCommand.Path
. (Join-Path $here 'TestHelpers.ps1')

Describe 'Get-IDLSnapshot.ps1' {

    function New-SnapshotState {
        $users = @(
            (New-StubUserEntry -Sam 'jdoe' -ExternalId 'E1001' -Attributes @{ givenName = 'Jane'; sn = 'Doe'; title = 'Analyst' } -MemberOf @("CN=GG-AllStaff,OU=Groups,$script:Root")),
            (New-StubUserEntry -Sam 'bsmith' -ExternalId 'E1002' -Enabled $false -Attributes @{ givenName = 'Bob'; sn = 'Smith' }),
            (New-StubUserEntry -Sam 'svc-noid' -ExternalId '' -Attributes @{ givenName = 'Service' }),
            (New-StubUserEntry -Sam 'elsewhere' -ExternalId 'E9999' -Ou "OU=Sales,$script:Root" -Attributes @{ givenName = 'Out' }),
            (New-StubUserEntry -Sam 'sub1' -ExternalId 'E2000' -Ou "OU=Deep,OU=Staff,$script:Root" -Attributes @{ title = 'Nested' })
        )
        $groups = @(
            @{ dn = "CN=GG-AllStaff,OU=Groups,$script:Root"; sam = 'GG-AllStaff' },
            @{ dn = "CN=Marketing,OU=Groups,$script:Root"; sam = 'Marketing' }
        )
        $ous = @("OU=Staff,$script:Root", "OU=Deep,OU=Staff,$script:Root", "OU=Sales,$script:Root", "OU=Groups,$script:Root")
        return (New-StubStateFile -Users $users -Groups $groups -Ous $ous)
    }

    It 'returns only idAttribute-populated users under the search bases, with attributes, memberOf and parentOu' {
        $state = New-SnapshotState
        $request = @{
            server      = ''
            searchBases = @("OU=Staff,$script:Root")
            idAttribute = 'employeeID'
            properties  = @('givenName', 'sn', 'title')
            groups      = @()
        }
        $r = Invoke-IDLScript -Script 'Get-IDLSnapshot' -Request $request -StatePath $state

        $r.ExitCode | Should Be 0
        $r.SingleJson | Should Be $true
        $r.Json.ok | Should Be $true

        $sams = @($r.Json.users | ForEach-Object { $_.sam } | Sort-Object)
        ($sams -join ',') | Should Be 'bsmith,jdoe,sub1'

        $jane = @($r.Json.users | Where-Object { $_.sam -eq 'jdoe' })[0]
        $jane.externalId | Should Be 'E1001'
        $jane.enabled | Should Be $true
        $jane.parentOu | Should Be "OU=Staff,$script:Root"
        $jane.dn | Should Be "CN=jdoe,OU=Staff,$script:Root"
        $jane.upn | Should Be 'jdoe@stub.test'
        $jane.attributes.givenName | Should Be 'Jane'
        $jane.attributes.sn | Should Be 'Doe'
        $jane.attributes.title | Should Be 'Analyst'
        @($jane.memberOf).Count | Should Be 1
        $jane.memberOf[0] | Should Be "CN=GG-AllStaff,OU=Groups,$script:Root"
        $jane.objectGuid | Should Match '^[0-9a-f]{8}-'

        $bob = @($r.Json.users | Where-Object { $_.sam -eq 'bsmith' })[0]
        $bob.enabled | Should Be $false
        $bob.attributes.title | Should BeNullOrEmpty
        $bob.attributes.PSObject.Properties['title'] | Should Not BeNullOrEmpty   # key present with null value
        @($bob.memberOf).Count | Should Be 0

        $nested = @($r.Json.users | Where-Object { $_.sam -eq 'sub1' })[0]
        $nested.parentOu | Should Be "OU=Deep,OU=Staff,$script:Root"
    }

    It 'includeUnlinked also returns users without idAttribute (externalId ""), never computers; a non-boolean is exit 2' {
        $users = @(
            (New-StubUserEntry -Sam 'jdoe' -ExternalId 'E1001' -Attributes @{ givenName = 'Jane' }),
            (New-StubUserEntry -Sam 'old.jane' -ExternalId '' -Attributes @{ givenName = 'Jane'; sn = 'Doe' } -Enabled $false),
            (New-StubUserEntry -Sam 'PC01$' -ExternalId '' -ObjectClass 'computer'),
            (New-StubUserEntry -Sam 'outside' -ExternalId '' -Ou "OU=Sales,$script:Root")
        )
        $state = New-StubStateFile -Users $users -Ous @("OU=Staff,$script:Root", "OU=Sales,$script:Root")
        $request = @{ searchBases = @("OU=Staff,$script:Root"); idAttribute = 'employeeID'; properties = @('givenName', 'sn'); groups = @(); includeUnlinked = $true }
        $r = Invoke-IDLScript -Script 'Get-IDLSnapshot' -Request $request -StatePath $state

        $r.ExitCode | Should Be 0
        (@($r.Json.users | ForEach-Object { $_.sam } | Sort-Object) -join ',') | Should Be 'jdoe,old.jane'
        $old = @($r.Json.users | Where-Object { $_.sam -eq 'old.jane' })[0]
        $old.externalId | Should Be ''
        $old.enabled | Should Be $false
        $old.attributes.sn | Should Be 'Doe'
        @($r.Json.users | Where-Object { $_.sam -eq 'jdoe' })[0].externalId | Should Be 'E1001'
        $r.LogText | Should Match '1 user\(s\) without employeeID included'

        $request.includeUnlinked = $false
        $r = Invoke-IDLScript -Script 'Get-IDLSnapshot' -Request $request -StatePath $state
        (@($r.Json.users | ForEach-Object { $_.sam }) -join ',') | Should Be 'jdoe'

        $request.includeUnlinked = 'yes'
        $r = Invoke-IDLScript -Script 'Get-IDLSnapshot' -Request $request -StatePath $state
        $r.ExitCode | Should Be 2
        $r.Json.ok | Should Be $false
    }

    It 'resolves groups (sAMAccountName and DN), reporting found and not found' {
        $state = New-SnapshotState
        $request = @{
            searchBases = @("OU=Staff,$script:Root")
            idAttribute = 'employeeID'
            properties  = @()
            groups      = @('GG-AllStaff', "CN=Marketing,OU=Groups,$script:Root", 'GG-Missing', "CN=Nope,OU=Groups,$script:Root")
        }
        $r = Invoke-IDLScript -Script 'Get-IDLSnapshot' -Request $request -StatePath $state

        $r.ExitCode | Should Be 0
        @($r.Json.groups).Count | Should Be 4
        $byId = @{}
        foreach ($g in $r.Json.groups){
            $byId[$g.identity] = $g
        }

        $byId['GG-AllStaff'].found | Should Be $true
        $byId['GG-AllStaff'].dn | Should Be "CN=GG-AllStaff,OU=Groups,$script:Root"
        $byId["CN=Marketing,OU=Groups,$script:Root"].found | Should Be $true
        $byId['GG-Missing'].found | Should Be $false
        $byId['GG-Missing'].dn | Should BeNullOrEmpty
        $byId["CN=Nope,OU=Groups,$script:Root"].found | Should Be $false
    }

    It 'handles multiple search bases without duplicating users and writes a log (no CSV, no mutations)' {
        $state = New-SnapshotState
        $request = @{
            searchBases = @("OU=Staff,$script:Root", "OU=Deep,OU=Staff,$script:Root", "OU=Sales,$script:Root")
            idAttribute = 'employeeID'
            properties  = @('title')
            groups      = @()
        }
        $r = Invoke-IDLScript -Script 'Get-IDLSnapshot' -Request $request -StatePath $state

        $r.ExitCode | Should Be 0
        @($r.Json.users).Count | Should Be 4
        $r.LogFile | Should Not BeNullOrEmpty
        $r.LogText | Should Match '\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2} \[INFO\] Script start: Get-IDLSnapshot'
        $r.LogText | Should Match 'ExitCode=0'
        (Get-StubCalls -Path $state).Count | Should Be 0
    }

    It 'returns empty arrays (not null) when nothing matches' {
        $state = New-SnapshotState
        $r = Invoke-IDLScript -Script 'Get-IDLSnapshot' -StatePath $state -Request @{ searchBases = @("OU=Groups,$script:Root"); idAttribute = 'employeeID' }
        $r.ExitCode | Should Be 0
        $r.StdOut | Should Match '"users":\[\]'
        $r.StdOut | Should Match '"groups":\[\]'
    }

    It 'rejects invalid input with exit code 2 and ok:false' {
        $state = New-SnapshotState
        $bad = @(
            @{ idAttribute = 'employeeID' },
            @{ searchBases = @(); idAttribute = 'employeeID' },
            @{ searchBases = @("OU=Staff,$script:Root") },
            @{ searchBases = @("OU=Staff,$script:Root"); idAttribute = 'employeeID)(objectClass=*' },
            @{ searchBases = @("OU=Staff,$script:Root"); idAttribute = 'employeeID'; properties = @('bad name') }
        )
        foreach ($request in $bad){
            $r = Invoke-IDLScript -Script 'Get-IDLSnapshot' -Request $request -StatePath $state
            $r.ExitCode | Should Be 2
            $r.SingleJson | Should Be $true
            $r.Json.ok | Should Be $false
            $r.Json.error | Should Not BeNullOrEmpty
        }

        $r = Invoke-IDLScript -Script 'Get-IDLSnapshot' -RawInput '{not json' -StatePath $state
        $r.ExitCode | Should Be 2
        $r.Json.ok | Should Be $false

        $r = Invoke-IDLScript -Script 'Get-IDLSnapshot' -RawInput '' -StatePath $state
        $r.ExitCode | Should Be 2
    }

    It 'exits 4 with ok:false when the domain controller cannot be reached' {
        $state = New-StubStateFile -FailConnect $true
        $r = Invoke-IDLScript -Script 'Get-IDLSnapshot' -StatePath $state -Request @{ searchBases = @("OU=Staff,$script:Root"); idAttribute = 'employeeID' }
        $r.ExitCode | Should Be 4
        $r.Json.ok | Should Be $false
    }

    It 'exits 3 with ok:false when the ActiveDirectory module cannot be loaded' {
        # With no IDL_STUB_AD_STATE the stub refuses to load, which the script must map to "dependency missing".
        $r = Invoke-IDLScript -Script 'Get-IDLSnapshot' -NoState -Request @{ searchBases = @("OU=Staff,$script:Root"); idAttribute = 'employeeID' }
        $r.ExitCode | Should Be 3
        $r.SingleJson | Should Be $true
        $r.Json.ok | Should Be $false
    }

    It 'exits 5 with ok:false when the search base does not exist' {
        $state = New-SnapshotState
        $r = Invoke-IDLScript -Script 'Get-IDLSnapshot' -StatePath $state -Request @{ searchBases = @("OU=Missing,$script:Root"); idAttribute = 'employeeID' }
        $r.ExitCode | Should Be 5
        $r.Json.ok | Should Be $false
    }

    It 'treats LDAP metacharacters in the idAttribute as invalid input rather than injecting a filter' {
        $state = New-SnapshotState
        $r = Invoke-IDLScript -Script 'Get-IDLSnapshot' -StatePath $state -Request @{ searchBases = @("OU=Staff,$script:Root"); idAttribute = '*' }
        $r.ExitCode | Should Be 2
    }
}

Describe 'Get-IDLSnapshot.ps1 sid' {

    It 'returns every user''s objectSid as an S-1-5-... string (matched against Entra onPremisesSecurityIdentifier)' {
        $jane = New-StubUserEntry -Sam 'jdoe' -ExternalId 'E1001'
        $jane['objectSid'] = 'S-1-5-21-100-200-300-1105'
        $bob = New-StubUserEntry -Sam 'bsmith' -ExternalId 'E1002'
        $state = New-StubStateFile -Users @($jane, $bob) -Ous @("OU=Staff,$script:Root")
        $request = @{ searchBases = @("OU=Staff,$script:Root"); idAttribute = 'employeeID'; properties = @(); groups = @() }
        $r = Invoke-IDLScript -Script 'Get-IDLSnapshot' -Request $request -StatePath $state

        $r.ExitCode | Should Be 0
        @($r.Json.users | Where-Object { $_.sam -eq 'jdoe' })[0].sid | Should Be 'S-1-5-21-100-200-300-1105'
        @($r.Json.users | Where-Object { $_.sam -eq 'bsmith' })[0].sid | Should Match '^S-1-5-21-\d+-\d+-\d+-\d+$'
    }
}
