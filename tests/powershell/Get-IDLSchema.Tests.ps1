$here = Split-Path -Parent $MyInvocation.MyCommand.Path
. (Join-Path $here 'TestHelpers.ps1')

Describe 'Get-IDLSchema.ps1' {

    # A small user hierarchy: user -> organizationalPerson -> person -> top, with one auxiliary class and an
    # unrelated class whose attributes must not leak in.
    function New-SchemaState {
        param([object[]]$Faults = @())

        $schema = @{
            classes    = @(
                @{ lDAPDisplayName = 'top'; subClassOf = 'top'; systemMustContain = @('objectClass'); systemMayContain = @('cn', 'memberOf') },
                @{ lDAPDisplayName = 'person'; subClassOf = 'top'; mayContain = @('sn', 'telephoneNumber') },
                @{ lDAPDisplayName = 'organizationalPerson'; subClassOf = 'person'; mayContain = @('title', 'manager', 'directReports') },
                @{ lDAPDisplayName = 'user'; subClassOf = 'organizationalPerson'; auxiliaryClass = @('custAux'); systemAuxiliaryClass = @('securityPrincipal'); mayContain = @('employeeID', 'givenName', 'proxyAddresses', 'thumbnailPhoto', 'whenCreated', 'pwdLastSet') },
                @{ lDAPDisplayName = 'securityPrincipal'; subClassOf = 'top'; systemMustContain = @('objectSid'); systemMayContain = @('tokenGroups') },
                @{ lDAPDisplayName = 'custAux'; subClassOf = 'top'; mayContain = @('corpCostCenter') },
                @{ lDAPDisplayName = 'computer'; subClassOf = 'user'; mayContain = @('dNSHostName') }
            )
            attributes = @(
                @{ lDAPDisplayName = 'objectClass'; attributeSyntax = '2.5.5.2'; isSingleValued = $false; systemOnly = $true },
                @{ lDAPDisplayName = 'cn'; attributeSyntax = '2.5.5.12'; isSingleValued = $true; systemOnly = $true },
                @{ lDAPDisplayName = 'memberOf'; attributeSyntax = '2.5.5.1'; isSingleValued = $false; systemOnly = $true; linkID = 3 },
                @{ lDAPDisplayName = 'sn'; attributeSyntax = '2.5.5.12'; isSingleValued = $true; rangeUpper = 64; adminDescription = 'Surname' },
                @{ lDAPDisplayName = 'telephoneNumber'; attributeSyntax = '2.5.5.12'; isSingleValued = $true },
                @{ lDAPDisplayName = 'title'; attributeSyntax = '2.5.5.12'; isSingleValued = $true },
                @{ lDAPDisplayName = 'manager'; attributeSyntax = '2.5.5.1'; isSingleValued = $true; linkID = 42 },
                @{ lDAPDisplayName = 'directReports'; attributeSyntax = '2.5.5.1'; isSingleValued = $false; systemOnly = $true; linkID = 43 },
                @{ lDAPDisplayName = 'employeeID'; attributeSyntax = '2.5.5.12'; isSingleValued = $true },
                @{ lDAPDisplayName = 'givenName'; attributeSyntax = '2.5.5.12'; isSingleValued = $true },
                @{ lDAPDisplayName = 'proxyAddresses'; attributeSyntax = '2.5.5.12'; isSingleValued = $false },
                @{ lDAPDisplayName = 'thumbnailPhoto'; attributeSyntax = '2.5.5.10'; isSingleValued = $true },
                @{ lDAPDisplayName = 'whenCreated'; attributeSyntax = '2.5.5.11'; isSingleValued = $true; systemOnly = $true },
                @{ lDAPDisplayName = 'pwdLastSet'; attributeSyntax = '2.5.5.16'; isSingleValued = $true },
                @{ lDAPDisplayName = 'objectSid'; attributeSyntax = '2.5.5.17'; isSingleValued = $true; systemOnly = $true },
                @{ lDAPDisplayName = 'corpCostCenter'; attributeSyntax = '2.5.5.12'; isSingleValued = $true },
                @{ lDAPDisplayName = 'dNSHostName'; attributeSyntax = '2.5.5.12'; isSingleValued = $true },
                @{ lDAPDisplayName = 'tokenGroups'; attributeSyntax = '2.5.5.17'; isSingleValued = $false; systemFlags = 4 }
            )
        }
        return (New-StubStateFile -Schema $schema -Faults $Faults)
    }

    It 'follows subClassOf and auxiliary classes and reports each attribute once' {
        $r = Invoke-IDLScript -Script 'Get-IDLSchema' -Request @{ server = ''; className = 'user' } -StatePath (New-SchemaState)

        $r.ExitCode | Should Be 0
        $r.SingleJson | Should Be $true
        $r.Json.ok | Should Be $true
        $r.Json.className | Should Be 'user'

        $names = @($r.Json.attributes | ForEach-Object { $_.name })
        ($names -join ',') | Should Be 'cn,corpCostCenter,directReports,employeeID,givenName,manager,memberOf,objectClass,objectSid,proxyAddresses,pwdLastSet,sn,telephoneNumber,thumbnailPhoto,title,tokenGroups,whenCreated'
        $names -contains 'dNSHostName' | Should Be $false   # computer is a subclass of user, not a parent
        $names -contains 'corpCostCenter' | Should Be $true   # from the auxiliary class
    }

    It 'maps syntax, single-valued, system-only, link and range information' {
        $r = Invoke-IDLScript -Script 'Get-IDLSchema' -Request @{ className = 'user' } -StatePath (New-SchemaState)
        $by = @{}
        foreach ($a in $r.Json.attributes){
            $by[$a.name] = $a
        }

        $by['sn'].syntax | Should Be 'string'
        $by['sn'].singleValued | Should Be $true
        $by['sn'].rangeUpper | Should Be 64
        $by['sn'].description | Should Be 'Surname'
        $by['manager'].syntax | Should Be 'dn'
        $by['manager'].linkId | Should Be 42
        $by['directReports'].linkId | Should Be 43
        $by['proxyAddresses'].singleValued | Should Be $false
        $by['thumbnailPhoto'].syntax | Should Be 'binary'
        $by['whenCreated'].syntax | Should Be 'time'
        $by['whenCreated'].systemOnly | Should Be $true
        $by['pwdLastSet'].syntax | Should Be 'integer'
        $by['objectSid'].syntax | Should Be 'binary'
        $by['objectClass'].syntax | Should Be 'oid'
        $by['givenName'].constructed | Should Be $false
        $by['givenName'].systemOnly | Should Be $false
    }

    It 'marks constructed attributes from systemFlags' {
        $r = Invoke-IDLScript -Script 'Get-IDLSchema' -Request @{ className = 'securityPrincipal' } -StatePath (New-SchemaState)
        $r.ExitCode | Should Be 0
        (@($r.Json.attributes | ForEach-Object { $_.name }) -join ',') | Should Be 'cn,memberOf,objectClass,objectSid,tokenGroups'

        $tg = @($r.Json.attributes | Where-Object { $_.name -eq 'tokenGroups' })[0]
        $tg.constructed | Should Be $true
        $sid = @($r.Json.attributes | Where-Object { $_.name -eq 'objectSid' })[0]
        $sid.constructed | Should Be $false
    }

    It 'defaults className to user' {
        $r = Invoke-IDLScript -Script 'Get-IDLSchema' -Request @{ server = '' } -StatePath (New-SchemaState)
        $r.ExitCode | Should Be 0
        $r.Json.className | Should Be 'user'
    }

    It 'rejects an invalid or unknown class with exit code 2' {
        $r = Invoke-IDLScript -Script 'Get-IDLSchema' -Request @{ className = 'user)(x=1' } -StatePath (New-SchemaState)
        $r.ExitCode | Should Be 2
        $r.Json.ok | Should Be $false

        $r = Invoke-IDLScript -Script 'Get-IDLSchema' -Request @{ className = 'noSuchClass' } -StatePath (New-SchemaState)
        $r.ExitCode | Should Be 2
        $r.Json.error | Should Match 'noSuchClass'
    }

    It 'returns exit code 4 when AD cannot be reached' {
        $r = Invoke-IDLScript -Script 'Get-IDLSchema' -Request @{ className = 'user' } -StatePath (New-StubStateFile -FailConnect $true)
        $r.ExitCode | Should Be 4
        $r.SingleJson | Should Be $true
        $r.Json.ok | Should Be $false
    }

    It 'reports a failing schema read as a script error' {
        $state = New-SchemaState -Faults @(@{ cmdlet = 'Get-ADObject'; match = 'CN=Schema'; message = 'Access is denied' })
        $r = Invoke-IDLScript -Script 'Get-IDLSchema' -Request @{ className = 'user' } -StatePath $state
        $r.ExitCode | Should Not Be 0
        $r.Json.ok | Should Be $false
        $r.Json.error | Should Match 'Access is denied'
    }

    It 'changes nothing in AD' {
        $state = New-SchemaState
        $null = Invoke-IDLScript -Script 'Get-IDLSchema' -Request @{ className = 'user' } -StatePath $state
        @((Read-StubStateFile -Path $state).calls).Count | Should Be 0
    }
}
