$here = Split-Path -Parent $MyInvocation.MyCommand.Path
. (Join-Path $here 'TestHelpers.ps1')

Describe 'Test-IDLNames.ps1' {

    function New-NamesState {
        param([bool]$FailConnect = $false)

        $users = @(
            (New-StubUserEntry -Sam 'jdoe' -ExternalId 'E1001' -Cn 'Jane Doe' -Guid '11111111-1111-1111-1111-111111111111' -Attributes @{ mail = 'jane.doe@stub.test'; proxyAddresses = @('SMTP:jane.doe@stub.test', 'smtp:jd@stub.test') }),
            (New-StubUserEntry -Sam 'bsmith' -ExternalId 'E1002' -Cn 'Bob Smith' -Ou "OU=Sales,$script:Root")
        )
        $groups = @(
            @{ dn = "CN=Finance,OU=Groups,$script:Root"; sam = 'finance'; attributes = @{ mail = 'finance@stub.test' } }
        )
        $objects = @(
            @{ dn = "CN=Ann Vendor,OU=Sales,$script:Root"; objectClass = 'contact'; attributes = @{ mail = 'ann@vendor.example'; proxyAddresses = @('SMTP:ann@vendor.example', 'smtp:ann.vendor@stub.test') } }
        )
        $ous = @("OU=Staff,$script:Root", "OU=Sales,$script:Root", "OU=Groups,$script:Root")
        return (New-StubStateFile -Users $users -Groups $groups -Ous $ous -Objects $objects -FailConnect $FailConnect)
    }

    function Get-Taken {
        param($Json, [string]$Field)
        return @($Json.taken | Where-Object { $_.field -eq $Field } | ForEach-Object { $_.value } | Sort-Object)
    }

    It 'reports taken sam/upn (any object, any case), mail (mail or smtp: proxyAddresses of users, groups, contacts) and cn per OU' {
        $state = New-NamesState
        $request = @{
            server = ''
            names  = @(
                @{ field = 'sam'; value = 'JDOE' },
                @{ field = 'sam'; value = 'finance' },
                @{ field = 'sam'; value = 'free1' },
                @{ field = 'upn'; value = 'bsmith@stub.test' },
                @{ field = 'upn'; value = 'free1@stub.test' },
                @{ field = 'mail'; value = 'Jane.Doe@stub.test' },
                @{ field = 'mail'; value = 'jd@stub.test' },
                @{ field = 'mail'; value = 'finance@stub.test' },
                @{ field = 'mail'; value = 'ann.vendor@stub.test' },
                @{ field = 'mail'; value = 'free1@stub.test' },
                @{ field = 'cn'; value = 'Bob Smith'; ou = "OU=Sales,$script:Root" },
                @{ field = 'cn'; value = 'Bob Smith'; ou = "OU=Staff,$script:Root" },
                @{ field = 'cn'; value = 'Ann Vendor'; ou = "OU=Sales,$script:Root" }
            )
        }
        $r = Invoke-IDLScript -Script 'Test-IDLNames' -Request $request -StatePath $state

        $r.ExitCode | Should Be 0
        $r.SingleJson | Should Be $true
        $r.Json.ok | Should Be $true
        ((Get-Taken $r.Json 'sam') -join ',') | Should Be 'finance,JDOE'
        ((Get-Taken $r.Json 'upn') -join ',') | Should Be 'bsmith@stub.test'
        ((Get-Taken $r.Json 'mail') -join ',') | Should Be 'ann.vendor@stub.test,finance@stub.test,Jane.Doe@stub.test,jd@stub.test'
        ((Get-Taken $r.Json 'cn') -join ',') | Should Be 'Ann Vendor,Bob Smith'
        $jane = @($r.Json.taken | Where-Object { $_.value -eq 'JDOE' })[0]
        $jane.dn | Should Be "CN=Jane Doe,OU=Staff,$script:Root"
        $jane.objectGuid | Should Be '11111111-1111-1111-1111-111111111111'
        @($r.Json.taken | Where-Object { ($_.field -eq 'cn') -and ($_.value -eq 'Bob Smith') })[0].dn | Should Be "CN=Bob Smith,OU=Sales,$script:Root"
        @($r.Json.missingOus).Count | Should Be 0
        (Get-StubCalls -Path $state).Count | Should Be 0
        $r.LogText | Should Not Match 'jane\.doe'
    }

    It 'ignores excluded objects, lists a missing cn OU, de-duplicates names and returns empty arrays when nothing is taken' {
        $state = New-NamesState
        $request = @{
            names   = @(
                @{ field = 'sam'; value = 'jdoe' },
                @{ field = 'sam'; value = 'jdoe' },
                @{ field = 'mail'; value = 'jane.doe@stub.test' },
                @{ field = 'cn'; value = 'Jane Doe'; ou = "OU=Staff,$script:Root" },
                @{ field = 'cn'; value = 'Someone'; ou = "OU=Nope,$script:Root" }
            )
            exclude = @('11111111-1111-1111-1111-111111111111')
        }
        $r = Invoke-IDLScript -Script 'Test-IDLNames' -Request $request -StatePath $state
        $r.ExitCode | Should Be 0
        @($r.Json.taken).Count | Should Be 0
        $r.StdOut | Should Match '"taken":\[\]'
        (@($r.Json.missingOus) -join ',') | Should Be "OU=Nope,$script:Root"

        $r = Invoke-IDLScript -Script 'Test-IDLNames' -Request @{ names = @() } -StatePath $state
        $r.ExitCode | Should Be 0
        $r.StdOut | Should Match '"taken":\[\]'
    }

    It 'escapes LDAP metacharacters in values (a * is not a wildcard)' {
        $state = New-NamesState
        $r = Invoke-IDLScript -Script 'Test-IDLNames' -Request @{ names = @(@{ field = 'sam'; value = 'j*' }, @{ field = 'mail'; value = '*@stub.test' }) } -StatePath $state
        $r.ExitCode | Should Be 0
        @($r.Json.taken).Count | Should Be 0
    }

    It 'rejects invalid input with exit code 2 and ok:false' {
        $state = New-NamesState
        $bad = @(
            @{ server = '' },
            @{ names = 'jdoe' },
            @{ names = @(@{ field = 'phone'; value = 'x' }) },
            @{ names = @(@{ field = 'sam'; value = '' }) },
            @{ names = @(@{ field = 'cn'; value = 'Jane Doe' }) },
            @{ names = @(@{ field = 'cn'; value = 'Jane Doe'; ou = 'not a dn' }) },
            @{ names = @(@{ field = 'sam'; value = 'jdoe' }); exclude = @('not-a-guid') },
            @{ names = @('jdoe') }
        )
        foreach ($request in $bad){
            $r = Invoke-IDLScript -Script 'Test-IDLNames' -Request $request -StatePath $state
            $r.ExitCode | Should Be 2
            $r.Json.ok | Should Be $false
            $r.SingleJson | Should Be $true
        }

        $r = Invoke-IDLScript -Script 'Test-IDLNames' -RawInput '{not json' -StatePath $state
        $r.ExitCode | Should Be 2
    }

    It 'exits 4 with ok:false when AD cannot be reached' {
        $state = New-NamesState -FailConnect $true
        $r = Invoke-IDLScript -Script 'Test-IDLNames' -Request @{ names = @(@{ field = 'sam'; value = 'jdoe' }) } -StatePath $state
        $r.ExitCode | Should Be 4
        $r.Json.ok | Should Be $false
    }
}
