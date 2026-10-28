$here = Split-Path -Parent $MyInvocation.MyCommand.Path
. (Join-Path $here 'TestHelpers.ps1')

Describe 'Search-IDLGroups.ps1' {

    $groups = @(
        @{ dn = "CN=GG-Finance,OU=Groups,$script:Root"; sam = 'GG-Finance'; attributes = @{ description = 'Finance staff'; groupType = '-2147483646' } },
        @{ dn = "CN=DL-Finance-Reports,OU=Groups,$script:Root"; sam = 'DL-Finance-Reports'; attributes = @{ description = 'Report mail'; groupType = '4' } },
        @{ dn = "CN=UG-Fin-All,OU=Groups,$script:Root"; sam = 'UG-Fin-All'; attributes = @{ groupType = '-2147483640' } },
        @{ dn = "CN=GG-Sales,OU=Groups,$script:Root"; sam = 'GG-Sales'; attributes = @{ displayName = 'Sales team'; groupType = '-2147483646' } },
        @{ dn = "CN=Domain Admins,CN=Users,$script:Root"; sam = 'Domain Admins'; objectSid = 'S-1-5-21-1-2-3-512'; attributes = @{ description = 'Designated administrators of the domain'; groupType = '-2147483646' } },
        @{ dn = "CN=Ops Team,OU=Groups,$script:Root"; sam = 'Ops Team'; adminCount = 1; attributes = @{ groupType = '-2147483640' } },
        @{ dn = "CN=Weird (x) group,OU=Groups,$script:Root"; sam = 'Weird-x'; attributes = @{ groupType = '-2147483646' } }
    )

    function Search {
        param($Request, [string]$State)
        return Invoke-IDLScript -Script 'Search-IDLGroups' -Request $Request -StatePath $State
    }

    It 'finds groups by name or description, sorted by name, with category, scope and the privileged flag; changes nothing' {
        $state = New-StubStateFile -Groups $groups
        $r = Search -Request @{ server = ''; query = 'fin'; max = 25 } -State $state

        $r.ExitCode | Should Be 0
        $r.SingleJson | Should Be $true
        $r.Json.ok | Should Be $true
        $r.Json.truncated | Should Be $false
        @($r.Json.groups | ForEach-Object { $_.sam }) -join ',' | Should Be 'DL-Finance-Reports,GG-Finance,UG-Fin-All'
        $fin = @($r.Json.groups | Where-Object { $_.sam -eq 'GG-Finance' })[0]
        $fin.dn | Should Be "CN=GG-Finance,OU=Groups,$script:Root"
        $fin.name | Should Be 'GG-Finance'
        $fin.description | Should Be 'Finance staff'
        $fin.category | Should Be 'security'
        $fin.scope | Should Be 'global'
        $fin.privileged | Should Be $false
        $dl = @($r.Json.groups | Where-Object { $_.sam -eq 'DL-Finance-Reports' })[0]
        $dl.category | Should Be 'distribution'
        $dl.scope | Should Be 'domainlocal'
        @($r.Json.groups | Where-Object { $_.sam -eq 'UG-Fin-All' })[0].scope | Should Be 'universal'
        (Get-StubCalls -Path $state).Count | Should Be 0

        # displayName and description are searched too; privileged = protected RID or adminCount=1.
        $r = Search -Request @{ query = 'Sales team' } -State $state
        @($r.Json.groups | ForEach-Object { $_.sam }) -join ',' | Should Be 'GG-Sales'
        $r = Search -Request @{ query = 'administrators' } -State $state
        @($r.Json.groups)[0].sam | Should Be 'Domain Admins'
        @($r.Json.groups)[0].privileged | Should Be $true
        $r = Search -Request @{ query = 'ops' } -State $state
        @($r.Json.groups)[0].privileged | Should Be $true
    }

    It 'max limits the result and sets truncated; the default is 25' {
        $state = New-StubStateFile -Groups $groups
        $r = Search -Request @{ query = 'fin'; max = 2 } -State $state
        $r.ExitCode | Should Be 0
        @($r.Json.groups).Count | Should Be 2
        @($r.Json.groups | ForEach-Object { $_.sam }) -join ',' | Should Be 'DL-Finance-Reports,GG-Finance'
        $r.Json.truncated | Should Be $true

        $many = @(1..30 | ForEach-Object { @{ dn = "CN=GG-Team$_,OU=Groups,$script:Root"; sam = "GG-Team$_"; attributes = @{ groupType = '-2147483646' } } })
        $r = Search -Request @{ query = 'team' } -State (New-StubStateFile -Groups $many)
        @($r.Json.groups).Count | Should Be 25
        $r.Json.truncated | Should Be $true
    }

    It '* is a wildcard; every other LDAP special character is matched literally (no filter injection)' {
        $state = New-StubStateFile -Groups $groups
        $r = Search -Request @{ query = 'GG-*nce' } -State $state
        @($r.Json.groups | ForEach-Object { $_.sam }) -join ',' | Should Be 'GG-Finance'

        $r = Search -Request @{ query = '(x)' } -State $state
        @($r.Json.groups | ForEach-Object { $_.sam }) -join ',' | Should Be 'Weird-x'

        foreach ($q in @('*)(objectClass=*', 'a)(|(cn=*', '\2a', 'fin\')){
            $r = Search -Request @{ query = $q } -State $state
            $r.ExitCode | Should Be 0
            @($r.Json.groups).Count | Should Be 0
        }

        $r.LogText | Should Match 'LDAP filter: .*\\5c'
    }

    It 'refuses bad input with exit 2; AD down is exit 4' {
        $state = New-StubStateFile -Groups $groups
        foreach ($req in @(@{ query = 'a' }, @{ query = '**a' }, @{ query = ('x' * 65) }, @{ query = '' }, @{ }, @{ query = "fi`nn" },
                @{ query = 'fin'; max = 0 }, @{ query = 'fin'; max = 201 }, @{ query = 'fin'; max = 'x' })){
            $r = Search -Request $req -State $state
            $r.ExitCode | Should Be 2
            $r.Json.ok | Should Be $false
            $r.SingleJson | Should Be $true
        }

        $r = Search -Request @{ query = 'fin' } -State (New-StubStateFile -FailConnect $true)
        $r.ExitCode | Should Be 4
        $r.Json.ok | Should Be $false
    }
}
