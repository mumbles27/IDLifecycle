$here = Split-Path -Parent $MyInvocation.MyCommand.Path
. (Join-Path $here 'TestHelpers.ps1')

Describe 'Stub ActiveDirectory module (test infrastructure)' {

    function New-StubFixture {
        $users = @(
            (New-StubUserEntry -Sam 'jdoe' -ExternalId 'E1001' -Cn 'Jane Doe' -Attributes @{ givenName = 'Jane'; title = 'Analyst' }),
            (New-StubUserEntry -Sam 'bsmith' -ExternalId 'E1002' -Cn 'Bob Smith' -Ou "OU=Sales,$script:Root"),
            (New-StubUserEntry -Sam 'star' -ExternalId 'E*9' -Cn 'Star Person')
        )
        return @{ State = (New-StandardState -Users $users); Users = $users }
    }

    function Invoke-Snippet {
        param([string]$Code, [string]$State)
        $r = Invoke-StubSnippet -Code ("`$ErrorActionPreference='Stop'; Import-Module ActiveDirectory -ErrorAction Stop; " + $Code) -StatePath $State
        return $r
    }

    It 'resolves ActiveDirectory to the stub (version 99.0.0) in the restricted child' {
        $f = New-StubFixture
        $r = Invoke-Snippet -State $f.State -Code '$m = Get-Module ActiveDirectory; "$($m.Version)|$($m.Path)"'
        $r.ExitCode | Should Be 0
        $r.StdOutRaw.Trim() | Should Match '^99\.0\.0\|'
        $r.StdOutRaw | Should Match 'StubAD'
    }

    It 'refuses to load when IDL_STUB_AD_STATE is not set' {
        $r = Invoke-StubSnippet -NoState -Code '$ErrorActionPreference = "Stop"; Import-Module ActiveDirectory -ErrorAction Stop; "loaded"'
        $r.ExitCode | Should Not Be 0
        $r.StdOutRaw | Should Not Match 'loaded'
        $r.StdErr | Should Match 'refuses to load'
    }

    It 'evaluates LDAP filters: equality, wildcards, presence, and/or/not, case-insensitive attribute names' {
        $f = New-StubFixture
        $code = @'
function S($filter) { (@(Get-ADUser -LDAPFilter $filter) | ForEach-Object { $_.SamAccountName } | Sort-Object) -join ',' }
"eq=" + (S '(employeeID=E1001)')
"ci=" + (S '(EMPLOYEEid=e1001)')
"prefix=" + (S '(employeeID=E10*)')
"suffix=" + (S '(sAMAccountName=*mith)')
"present=" + (S '(title=*)')
"and=" + (S '(&(employeeID=E*)(!(sAMAccountName=jdoe)))')
"or=" + (S '(|(sAMAccountName=jdoe)(sAMAccountName=bsmith))')
"escstar=" + (S '(employeeID=E\2a9)')
"wildstar=" + (S '(employeeID=E*9)')
"none=" + (S '(employeeID=nope)')
$bytes = (Get-ADUser -Identity jdoe).ObjectGUID.ToByteArray()
$esc = ($bytes | ForEach-Object { '\{0:x2}' -f $_ }) -join ''
"guid=" + (S "(objectGUID=$esc)")
'@
        $r = Invoke-Snippet -State $f.State -Code $code
        $r.ExitCode | Should Be 0
        $lines = @{}
        foreach ($l in ($r.StdOutRaw -split "`r?`n")){
            if ($l -match '^(\w+)=(.*)$'){
                $lines[$Matches[1]] = $Matches[2]
            }
        }
        $lines['eq'] | Should Be 'jdoe'
        $lines['ci'] | Should Be 'jdoe'
        $lines['prefix'] | Should Be 'bsmith,jdoe'
        $lines['suffix'] | Should Be 'bsmith'
        $lines['present'] | Should Be 'jdoe'
        $lines['and'] | Should Be 'bsmith,star'
        $lines['or'] | Should Be 'bsmith,jdoe'
        $lines['escstar'] | Should Be 'star'
        $lines['wildstar'] | Should Be 'star'
        $lines['none'] | Should Be ''
        $lines['guid'] | Should Be 'jdoe'
    }

    It 'throws ADIdentityNotFoundException for unknown identities but returns nothing for filters' {
        $f = New-StubFixture
        $code = @'
try { Get-ADUser -Identity 'nobody' } catch { "type=" + $_.Exception.GetType().Name }
"filter=" + @(Get-ADUser -LDAPFilter '(employeeID=none)').Count
"all=" + @(Get-ADUser -Filter *).Count
'@
        $r = Invoke-Snippet -State $f.State -Code $code
        $r.StdOutRaw | Should Match 'type=ADIdentityNotFoundException'
        $r.StdOutRaw | Should Match 'filter=0'
        $r.StdOutRaw | Should Match 'all=3'
    }

    It 'only returns requested -Properties (like real AD) plus the defaults' {
        $f = New-StubFixture
        $code = @'
$u = Get-ADUser -Identity jdoe
"noprops=" + [bool]$u.PSObject.Properties['title']
$u = Get-ADUser -Identity jdoe -Properties title, memberOf
"withprops=" + $u.title + "|" + [bool]$u.PSObject.Properties['title']
'@
        $r = Invoke-Snippet -State $f.State -Code $code
        $r.StdOutRaw | Should Match 'noprops=False'
        $r.StdOutRaw | Should Match 'withprops=Analyst\|True'
    }

    It 'New-ADUser assigns a fresh objectGuid and CN=<name>,<Path>; password is never stored or logged' {
        $f = New-StubFixture
        $code = @'
$pw = ConvertTo-SecureString 'Zz9!stubPassw0rd' -AsPlainText -Force
$a = New-ADUser -Name 'New One' -SamAccountName 'none1' -UserPrincipalName 'none1@stub.test' -Path 'OU=Staff,DC=stub,DC=test' -AccountPassword $pw -Enabled $true -OtherAttributes @{ employeeID = 'E7000' } -PassThru
$b = New-ADUser -Name 'New Two' -SamAccountName 'none2' -UserPrincipalName 'none2@stub.test' -Path 'OU=Staff,DC=stub,DC=test' -AccountPassword $pw -Enabled $true -PassThru
"dn=" + $a.DistinguishedName
"distinct=" + ($a.ObjectGUID -ne $b.ObjectGUID)
try { New-ADUser -Name 'Dup' -SamAccountName 'none1' -Path 'OU=Staff,DC=stub,DC=test' -AccountPassword $pw -Enabled $true } catch { "dup=" + $_.Exception.GetType().Name }
try { New-ADUser -Name 'BadPath' -SamAccountName 'bp' -Path 'OU=Nope,DC=stub,DC=test' -AccountPassword $pw -Enabled $true } catch { "badpath=1" }
'@
        $r = Invoke-Snippet -State $f.State -Code $code
        $r.ExitCode | Should Be 0
        $r.StdOutRaw | Should Match 'dn=CN=New One,OU=Staff,DC=stub,DC=test'
        $r.StdOutRaw | Should Match 'distinct=True'
        $r.StdOutRaw | Should Match 'dup=ADException'
        $r.StdOutRaw | Should Match 'badpath=1'
        $raw = [System.IO.File]::ReadAllText($f.State)
        $raw.Contains('Zz9!stubPassw0rd') | Should Be $false
        (Get-StubCalls -Path $f.State -Cmdlet 'New-ADUser').Count | Should Be 4
        (Get-StubCalls -Path $f.State -Cmdlet 'New-ADUser')[0].args.AccountPassword | Should Be '***'
    }

    It 'mimics AD membership errors (double add / remove of non-member) and records mutating calls only' {
        $f = New-StubFixture
        $code = @'
$g = 'CN=GG-AllStaff,OU=Groups,DC=stub,DC=test'
Add-ADGroupMember -Identity $g -Members 'jdoe'
try { Add-ADGroupMember -Identity $g -Members 'jdoe' } catch { "again=" + $_.Exception.Message }
Remove-ADGroupMember -Identity $g -Members 'jdoe' -Confirm:$false
try { Remove-ADGroupMember -Identity $g -Members 'jdoe' -Confirm:$false } catch { "notmember=1" }
$null = Get-ADUser -Identity jdoe
'@
        $r = Invoke-Snippet -State $f.State -Code $code
        $r.StdOutRaw | Should Match 'again=.*already a member'
        $r.StdOutRaw | Should Match 'notmember=1'
        $calls = Get-StubCalls -Path $f.State
        $calls.Count | Should Be 4
        @($calls | ForEach-Object { $_.cmdlet } | Select-Object -Unique) -join ',' | Should Be 'Add-ADGroupMember,Remove-ADGroupMember'
    }

    It 'models child objects: Remove-ADUser and non-recursive Remove-ADObject throw the leaf-object error, -Recursive deletes the subtree' {
        $jane = New-StubUserEntry -Sam 'jdoe' -ExternalId 'E1001' -Cn 'Jane Doe'
        $bob = New-StubUserEntry -Sam 'bsmith' -ExternalId 'E1002' -Cn 'Bob Smith'
        $eas = "CN=ExchangeActiveSyncDevices,$($jane.dn)"
        $objects = @(
            @{ dn = $eas; objectClass = 'msExchActiveSyncDevices' },
            @{ dn = "CN=Phone1,$eas"; objectClass = 'msExchActiveSyncDevice' }
        )
        $state = New-StandardState -Users @($jane, $bob) -Objects $objects
        $code = @"
`$one = @(Get-ADObject -LDAPFilter '(objectClass=*)' -SearchBase '$($jane.dn)' -SearchScope OneLevel)
"onelevel=" + `$one.Count + "|" + `$one[0].ObjectClass
`$sub = @(Get-ADObject -LDAPFilter '(objectClass=*)' -SearchBase '$($jane.dn)' -SearchScope Subtree)
"subtree=" + `$sub.Count
try { Remove-ADUser -Identity jdoe -Confirm:`$false } catch { "removeuser=" + `$_.Exception.Message }
try { Remove-ADObject -Identity '$($jane.objectGuid)' -Confirm:`$false } catch { "removeobject=" + `$_.Exception.Message }
Remove-ADObject -Identity '$($jane.objectGuid)' -Recursive -Confirm:`$false
Remove-ADUser -Identity bsmith -Confirm:`$false
"done=1"
"@
        $r = Invoke-Snippet -State $state -Code $code
        $r.StdOutRaw | Should Match 'onelevel=1\|msExchActiveSyncDevices'
        $r.StdOutRaw | Should Match 'subtree=3'
        $r.StdOutRaw | Should Match 'removeuser=.*only on a leaf object'
        $r.StdOutRaw | Should Match 'removeobject=.*only on a leaf object'
        $r.StdOutRaw | Should Match 'done=1'
        $after = Read-StubStateFile -Path $state
        @($after.users).Count | Should Be 0
        @($after.objects).Count | Should Be 0
        $calls = Get-StubCalls -Path $state
        @($calls | ForEach-Object { $_.cmdlet }) -join ',' | Should Be 'Remove-ADUser,Remove-ADObject,Remove-ADObject,Remove-ADUser'
        $calls[0].result | Should Match '^error'
        $calls[1].result | Should Match '^error'
        $calls[2].result | Should Be 'ok'
        $calls[2].args.Recursive | Should Be 'True'
    }

    It 'moves child objects together with their parent user' {
        $jane = New-StubUserEntry -Sam 'jdoe' -ExternalId 'E1001' -Cn 'Jane Doe'
        $state = New-StandardState -Users @($jane) -Objects @(@{ dn = "CN=ExchangeActiveSyncDevices,$($jane.dn)"; objectClass = 'msExchActiveSyncDevices' })
        $r = Invoke-Snippet -State $state -Code "Move-ADObject -Identity jdoe -TargetPath 'OU=Disabled,DC=stub,DC=test'; 'ok'"
        $r.StdOutRaw | Should Match 'ok'
        @((Read-StubStateFile -Path $state).objects)[0].dn | Should Be "CN=ExchangeActiveSyncDevices,CN=Jane Doe,OU=Disabled,$script:Root"
    }

    It 'throws a connection error from every cmdlet when failConnect is set' {
        $state = New-StubStateFile -FailConnect $true
        $r = Invoke-Snippet -State $state -Code 'try { Get-ADRootDSE } catch { "type=" + $_.Exception.GetType().Name }'
        $r.StdOutRaw | Should Match 'type=ADServerDownException'
    }
}
