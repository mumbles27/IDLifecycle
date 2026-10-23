$here = Split-Path -Parent $MyInvocation.MyCommand.Path
. (Join-Path $here 'TestHelpers.ps1')

# The common module has no import-time AD dependency, so it is safe to load in this process
# (only pure helper functions are exercised; nothing here calls an AD cmdlet).
Import-Module (Join-Path $script:ScriptsDir 'IDLifecycle.Common.psm1') -Force

Describe 'IDLifecycle.Common.psm1 helpers' {

    It 'escapes RFC 4515 special characters in LDAP filter values' {
        ConvertTo-IDLLdapFilterValue -Value 'plain' | Should Be 'plain'
        ConvertTo-IDLLdapFilterValue -Value 'a*b' | Should Be 'a\2ab'
        ConvertTo-IDLLdapFilterValue -Value '(x)' | Should Be '\28x\29'
        ConvertTo-IDLLdapFilterValue -Value 'a\b' | Should Be 'a\5cb'
        ConvertTo-IDLLdapFilterValue -Value ("a" + [char]0 + "b") | Should Be 'a\00b'
        ConvertTo-IDLLdapFilterValue -Value '*)(uid=*))(|(uid=*' | Should Be '\2a\29\28uid=\2a\29\29\28|\28uid=\2a'
        ConvertTo-IDLLdapFilterValue -Value '' | Should Be ''
        ConvertTo-IDLLdapFilterValue -Value 'José' | Should Be 'José'
    }

    It 'renders GUIDs as escaped octets for objectGUID filters' {
        $g = [guid]'00112233-4455-6677-8899-aabbccddeeff'
        # .NET byte order for the first three groups is little-endian, exactly what AD stores.
        ConvertTo-IDLLdapGuidValue -Guid $g | Should Be '\33\22\11\00\55\44\77\66\88\99\aa\bb\cc\dd\ee\ff'
    }

    It 'escapes RDN values and splits DNs with escaped commas' {
        ConvertTo-IDLDnEscapedValue -Value 'Doe, Jane' | Should Be 'Doe\, Jane'
        ConvertTo-IDLDnEscapedValue -Value '#hash' | Should Be '\#hash'
        ConvertTo-IDLDnEscapedValue -Value 'trail ' | Should Be 'trail\ '
        Get-IDLParentDn -Dn 'CN=Doe\, Jane,OU=Staff,DC=x,DC=y' | Should Be 'OU=Staff,DC=x,DC=y'
        Get-IDLLeafRdn -Dn 'CN=Doe\, Jane,OU=Staff,DC=x,DC=y' | Should Be 'CN=Doe\, Jane'
        Get-IDLParentDn -Dn 'DC=x' | Should Be ''
    }

    It 'distinguishes DNs from sAMAccountNames and validates attribute names' {
        Test-IDLDistinguishedName -Value 'CN=G,OU=X,DC=a' | Should Be $true
        Test-IDLDistinguishedName -Value 'GG-AllStaff' | Should Be $false
        Test-IDLAttributeName -Name 'employeeID' | Should Be $true
        Test-IDLAttributeName -Name 'msDS-cloudExtensionAttribute1' | Should Be $true
        Test-IDLAttributeName -Name 'a)(b' | Should Be $false
        Test-IDLAttributeName -Name '*' | Should Be $false
        Test-IDLAttributeName -Name '' | Should Be $false
    }

    It 'scrubs registered secrets (and their JSON-escaped form) from text' {
        Register-IDLSecret -Secret 'Sup3r"Secret\Pass'
        Protect-IDLText -Text 'boom Sup3r"Secret\Pass end' | Should Be 'boom *** end'
        Protect-IDLText -Text 'json Sup3r\"Secret\\Pass end' | Should Be 'json *** end'
        Protect-IDLText -Text 'nothing here' | Should Be 'nothing here'
    }

    It 'maps exceptions to the contract exit codes' {
        Get-IDLExitCode -Exception (New-IDLException -Kind Input -Message 'x') | Should Be 2
        Get-IDLExitCode -Exception (New-IDLException -Kind Dependency -Message 'x') | Should Be 3
        Get-IDLExitCode -Exception (New-IDLException -Kind Connection -Message 'x') | Should Be 4
        Get-IDLExitCode -Exception (New-IDLException -Kind Fatal -Message 'x') | Should Be 5
        Get-IDLExitCode -Exception ([System.Exception]::new('Unable to contact the server.')) | Should Be 4
        Get-IDLExitCode -Exception ([System.Exception]::new('whatever')) | Should Be 5
    }

    It 'coerces lists, strings and chunks predictably' {
        $list = Get-IDLStringList -Value @('a', ' b ', 'A', '', $null, 3)
        $list.Count | Should Be 3
        $solo = Get-IDLStringList -Value 'solo'
        $solo.GetType().Name | Should Be 'String[]'
        $empty = Get-IDLStringList -Value $null
        $empty.Count | Should Be 0
        ConvertTo-IDLString -Value $true | Should Be 'TRUE'
        ConvertTo-IDLString -Value 42 | Should Be '42'
        ConvertTo-IDLString -Value 1.5 | Should Be '1.5'
        $chunks = Split-IDLChunk -InputArray @(1..5) -Size 2
        @($chunks).Count | Should Be 3
        @($chunks[2]).Count | Should Be 1
        New-IDLOrFilter -Terms @('(a=1)') | Should Be '(a=1)'
        New-IDLOrFilter -Terms @('(a=1)', '(b=2)') | Should Be '(|(a=1)(b=2))'
    }

    It 'Get-IDLField is case-insensitive and keeps arrays intact' {
        $h = @{ Items = @(@{ a = 1 }) }
        $v = Get-IDLField -Object $h -Name 'items'
        $v.GetType().Name | Should Be 'Object[]'
        $v.Count | Should Be 1
        Get-IDLField -Object $h -Name 'missing' -Default 'd' | Should Be 'd'
        Get-IDLField -Object $null -Name 'x' | Should BeNullOrEmpty
    }
}
