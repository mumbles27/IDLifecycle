$here = Split-Path -Parent $MyInvocation.MyCommand.Path
. (Join-Path $here 'TestHelpers.ps1')

# Pure helper functions only (no AD cmdlets are called in this process).
Import-Module (Join-Path $script:ScriptsDir 'IDLifecycle.Common.psm1') -Force

Describe 'Security hardening' {

    $root = 'DC=stub,DC=test'

    function New-SecGroups {
        $list = @(
            @{ sam = 'GG-Plain'; adminCount = 0; objectSid = 'S-1-5-21-1-2-3-1234' },
            @{ sam = 'GG-Flagged'; adminCount = 1; objectSid = 'S-1-5-21-1-2-3-2000' },
            @{ sam = 'Domain Admins'; objectSid = 'S-1-5-21-1-2-3-512' },
            @{ sam = 'Schema Admins'; objectSid = 'S-1-5-21-1-2-3-518' },
            @{ sam = 'Enterprise Admins'; objectSid = 'S-1-5-21-1-2-3-519' },
            @{ sam = 'Administrators'; objectSid = 'S-1-5-32-544' },
            @{ sam = 'Account Operators'; objectSid = 'S-1-5-32-548' },
            @{ sam = 'Server Operators'; objectSid = 'S-1-5-32-549' },
            @{ sam = 'Print Operators'; objectSid = 'S-1-5-32-550' },
            @{ sam = 'Backup Operators'; objectSid = 'S-1-5-32-551' },
            @{ sam = 'GG-Dup'; objectSid = 'S-1-5-21-1-2-3-3001'; ou = 'OU=Groups' },
            @{ sam = 'GG-Dup'; objectSid = 'S-1-5-21-1-2-3-3002'; ou = 'OU=Sales' }
        )
        $out = @()
        foreach ($g in $list){
            $ou = if ($g.ContainsKey('ou')) { $g.ou } else { 'OU=Groups' }
            $entry = [ordered]@{ dn = "CN=$($g.sam),$ou,$root"; sam = $g.sam; objectSid = $g.objectSid }
            if ($g.ContainsKey('adminCount')){
                $entry['adminCount'] = $g.adminCount
            }

            $out += , $entry
        }

        return $out
    }

    function New-SecState {
        param([object[]]$Users = @(), [object[]]$Faults = @())
        $ous = @("OU=Staff,$root", "OU=Sales,$root", "OU=Disabled,$root", "OU=Groups,$root")
        return (New-StubStateFile -Users $Users -Groups (New-SecGroups) -Ous $ous -Faults $Faults)
    }

    function Copy-Item2 {
        param([hashtable]$Source, [hashtable]$Extra = @{})
        $copy = [ordered]@{}
        foreach ($k in $Source.Keys){
            $copy[$k] = $Source[$k]
        }

        foreach ($k in $Extra.Keys){
            $copy[$k] = $Extra[$k]
        }

        return $copy
    }

    function New-Req {
        param([object[]]$Items)
        return [ordered]@{ server = ''; idAttribute = 'employeeID'; items = @($Items) }
    }

    function New-NewItem {
        param([string]$Ext = 'E8001', [string]$Sam = 'secuser', [string]$Cn = 'Sec User', [string]$Ou = "OU=Staff,$root", [string[]]$Groups = @(), [string]$Password = 'Str0ng!Passw0rd#1', $Attributes = $null)
        if ($null -eq $Attributes){
            $Attributes = [ordered]@{ givenName = 'Sec'; sn = 'User' }
        }

        return [ordered]@{
            changeId = "n-$Ext"; externalId = $Ext; ou = $Ou
            candidates = @(@{ sam = $Sam; upn = "$Sam@stub.test"; cn = $Cn })
            attributes = $Attributes; groups = $Groups; password = $Password; enabled = $true; changePasswordAtLogon = $true
        }
    }

    #region 1 - installer lockdown

    It 'Set-IDLDirectoryLockdown removes inheritance without copying ACEs, then Test-IDLDirectoryLockedDown agrees (idempotent)' {
        $dir = Join-Path $script:StateDir ('acl_' + [guid]::NewGuid().ToString('N'))
        $null = New-Item -ItemType Directory -Force -Path $dir
        $me = [System.Security.Principal.WindowsIdentity]::GetCurrent().Name
        try{
            # Fresh temp dir inherits its parent's ACL, so it must NOT count as locked down.
            Test-IDLDirectoryLockedDown -Path $dir -ServiceAccount $me | Should Be $false
            Test-IDLDirectoryLockedDown -Path (Join-Path $dir 'missing') -ServiceAccount $me | Should Be $false

            Set-IDLDirectoryLockdown -Path $dir -ServiceAccount $me
            Test-IDLDirectoryLockedDown -Path $dir -ServiceAccount $me | Should Be $true

            $acl = Get-Acl -LiteralPath $dir
            $acl.AreAccessRulesProtected | Should Be $true
            $sids = @($acl.Access | ForEach-Object { $_.IdentityReference.Translate([System.Security.Principal.SecurityIdentifier]).Value })
            foreach ($bad in @('S-1-5-32-545', 'S-1-5-11', 'S-1-3-0', 'S-1-1-0')){
                ($sids -contains $bad) | Should Be $false
            }

            ($sids -contains 'S-1-5-18') | Should Be $true
            ($sids -contains 'S-1-5-32-544') | Should Be $true
            @($acl.Access | Where-Object { $_.IsInherited }).Count | Should Be 0

            # children inherit only the locked-down entries
            $file = Join-Path $dir 'child.txt'
            Set-Content -LiteralPath $file -Value 'x'
            $childSids = @((Get-Acl -LiteralPath $file).Access | ForEach-Object { $_.IdentityReference.Translate([System.Security.Principal.SecurityIdentifier]).Value })
            ($childSids -contains 'S-1-5-32-545') | Should Be $false

            Set-IDLDirectoryLockdown -Path $dir -ServiceAccount $me
            Test-IDLDirectoryLockedDown -Path $dir -ServiceAccount $me | Should Be $true
        }
        finally{
            Remove-Item -LiteralPath $dir -Recurse -Force -ErrorAction SilentlyContinue
        }
    }

    It 'Test-IDLDirectoryLockedDown is false when a protected ACL still carries a Users ACE' {
        $dir = Join-Path $script:StateDir ('acl_' + [guid]::NewGuid().ToString('N'))
        $null = New-Item -ItemType Directory -Force -Path $dir
        $me = [System.Security.Principal.WindowsIdentity]::GetCurrent().Name
        try{
            Set-IDLDirectoryLockdown -Path $dir -ServiceAccount $me
            $acl = Get-Acl -LiteralPath $dir
            $users = [System.Security.Principal.SecurityIdentifier]::new('S-1-5-32-545')
            $acl.AddAccessRule([System.Security.AccessControl.FileSystemAccessRule]::new($users, 'ReadAndExecute', 'ContainerInherit, ObjectInherit', 'None', 'Allow'))
            [System.IO.FileSystemAclExtensions]::SetAccessControl([System.IO.DirectoryInfo]::new($dir), $acl)
            Test-IDLDirectoryLockedDown -Path $dir -ServiceAccount $me | Should Be $false
        }
        finally{
            Remove-Item -LiteralPath $dir -Recurse -Force -ErrorAction SilentlyContinue
        }
    }

    It 'installer DryRun plans a lockdown for existing inheriting dirs and changes no ACL' {
        $dir = Join-Path $script:StateDir ('inst_' + [guid]::NewGuid().ToString('N'))
        $data = Join-Path $dir 'data'
        $null = New-Item -ItemType Directory -Force -Path $data
        $exe = Join-Path $dir 'idlifecycled.exe'
        Set-Content -LiteralPath $exe -Value 'x'
        $cfg = Join-Path $dir 'config.json'
        [System.IO.File]::WriteAllText($cfg, (ConvertTo-Json -InputObject @{ data_dir = $data; powershell = @{ scripts_dir = $script:ScriptsDir } } -Compress), $script:Utf8NoBom)
        $beforeProtected = (Get-Acl -LiteralPath $data).AreAccessRulesProtected
        $beforeCount = @((Get-Acl -LiteralPath $data).Access).Count
        try{
            $state = New-StubStateFile
            $a = @('-BinaryPath', $exe, '-ConfigPath', $cfg, '-ServiceAccount', 'TESTDOM\svc-idl$', '-ServiceName', 'IDLifecycleDryRunTest2')
            $r = Invoke-IDLScript -Script 'Install-IDLifecycleService' -StatePath $state -ScriptArguments $a -DryRun
            $r.ExitCode | Should Be 0
            $r.LogText.Contains("WOULD: LockDownDirectory $data") | Should Be $true
            $r.LogText.Contains('WOULD: LockDownDirectory ' + (Join-Path $script:ScriptsDir 'Logs')) | Should Be $true
            $r.LogText | Should Not Match 'GrantDataDir'
            (Get-Acl -LiteralPath $data).AreAccessRulesProtected | Should Be $beforeProtected
            @((Get-Acl -LiteralPath $data).Access).Count | Should Be $beforeCount
        }
        finally{
            Remove-Item -LiteralPath $dir -Recurse -Force -ErrorAction SilentlyContinue
        }
    }

    #endregion

    #region 2 - objectGuid required

    foreach ($kind in @('Set', 'Disable', 'Enable', 'Remove')){
        It "$kind-IDLUser fails an item without (or with an empty/invalid) objectGuid and never resolves by idAttribute" {
            $off = New-StubUserEntry -Sam 'victim' -ExternalId 'E1004' -Cn 'Vic Tim' -Enabled $false -Ou "OU=Disabled,$root"
            $on = New-StubUserEntry -Sam 'other' -ExternalId 'E1005' -Cn 'Other One' -Enabled $true -Ou "OU=Staff,$root"
            $state = New-SecState -Users @($off, $on)
            $base = @{ changeId = 'x'; externalId = 'E1004' }
            if ($kind -eq 'Set'){
                $base['set'] = @{ title = 'Pwn' }
            }

            $noGuid = Copy-Item2 -Source $base
            $empty = Copy-Item2 -Source $base -Extra @{ objectGuid = '' }
            $zero = Copy-Item2 -Source $base -Extra @{ objectGuid = '00000000-0000-0000-0000-000000000000' }
            $bad = Copy-Item2 -Source $base -Extra @{ objectGuid = 'nope' }
            $r = Invoke-IDLScript -Script "$kind-IDLUser" -Request (New-Req @($noGuid, $empty, $zero, $bad)) -StatePath $state
            $r.ExitCode | Should Be 1
            $r.Json.ok | Should Be $true
            @($r.Json.results | Where-Object { $_.status -eq 'failed' }).Count | Should Be 4
            $r.Json.results[0].message | Should Match 'objectGuid is required'
            $r.Json.results[0].error | Should Match 'objectGuid is required'
            $r.Json.results[1].error | Should Match 'objectGuid is required'
            $r.Json.results[2].error | Should Match 'objectGuid is required'
            (Get-StubCalls -Path $state).Count | Should Be 0
            (Get-StubUser -Path $state -Sam 'victim') | Should Not BeNullOrEmpty
        }
    }

    #endregion

    #region 3 - protected attributes

    It 'Test-IDLProtectedAttribute covers the list and the msDS-/msExch- prefixes case-insensitively' {
        foreach ($n in @('sAMAccountName', 'USERPRINCIPALNAME', 'altSecurityIdentities', 'servicePrincipalName', 'scriptPath', 'userWorkstations', 'adminCount', 'primaryGroupId', 'sIDHistory', 'nTSecurityDescriptor', 'msDS-AllowedToDelegateTo', 'MSDS-Foo', 'msExchHideFromAddressLists', 'msExch-Whatever', 'memberOf', 'objectSid', 'homeDirectory', 'profilePath', 'proxyAddresses', 'accountExpires', 'msPKI-Foo', 'ms-DS-Bar', 'cn', 'name', 'member', 'objectClass', 'unixUserPassword', 'msNPAllowDialin', 'logonHours')){
            Test-IDLProtectedAttribute -Name $n | Should Be $true
        }

        foreach ($n in @('title', 'department', 'telephoneNumber', 'description', 'manager', 'extensionAttribute1')){
            Test-IDLProtectedAttribute -Name $n | Should Be $false
        }

        @(Get-IDLProtectedAttributes).Count | Should BeGreaterThan 15
    }

    It 'Set-IDLUser refuses every protected attribute in set and clear, naming the attribute, changing nothing' {
        $jane = New-StubUserEntry -Sam 'jdoe' -ExternalId 'E1001' -Cn 'Jane Doe'
        $state = New-SecState -Users @($jane)
        $names = @('samaccountname', 'userPrincipalName', 'altSecurityIdentities', 'servicePrincipalName', 'scriptPath', 'userWorkstations', 'adminCount', 'primaryGroupId', 'sIDHistory', 'nTSecurityDescriptor', 'msDS-AllowedToDelegateTo', 'MSDS-Foo', 'msExchHideFromAddressLists', 'MSEXCH-Foo', 'homeDirectory', 'profilePath', 'proxyAddresses', 'accountExpires', 'mspki-foo', 'ms-DS-Bar', 'homeDrive', 'lockoutTime', 'userCertificate')
        $items = @()
        foreach ($n in $names){
            $items += , ([ordered]@{ changeId = "s-$n"; externalId = 'E1001'; objectGuid = $jane.objectGuid; set = @{ $n = 'x' } })
            $items += , ([ordered]@{ changeId = "c-$n"; externalId = 'E1001'; objectGuid = $jane.objectGuid; clear = @($n) })
        }

        $r = Invoke-IDLScript -Script 'Set-IDLUser' -Request (New-Req $items) -StatePath $state
        $r.ExitCode | Should Be 1
        @($r.Json.results | Where-Object { $_.status -eq 'failed' }).Count | Should Be ($names.Count * 2)
        foreach ($res in $r.Json.results){
            $res.error | Should Match 'protected'
        }

        $r.Json.results[0].error | Should Match 'samaccountname'
        (Get-StubCalls -Path $state).Count | Should Be 0
    }

    It 'New-IDLUser refuses protected attributes in "attributes" (no account created) but still ignores sAMAccountName/UPN/cn' {
        $state = New-SecState
        $bad = New-NewItem -Ext 'E8101' -Sam 'badattr' -Attributes @{ givenName = 'A'; 'msDS-AllowedToDelegateTo' = 'x' }
        $adm = New-NewItem -Ext 'E8102' -Sam 'admc' -Cn 'Adm C' -Attributes @{ adminCount = '1' }
        $ign = New-NewItem -Ext 'E8103' -Sam 'okuser' -Cn 'Ok User' -Attributes @{ givenName = 'Ok'; sAMAccountName = 'hacker'; cn = 'zzz' }
        $r = Invoke-IDLScript -Script 'New-IDLUser' -Request (New-Req @($bad, $adm, $ign)) -StatePath $state
        $r.ExitCode | Should Be 1
        $r.Json.results[0].status | Should Be 'failed'
        $r.Json.results[0].error | Should Match 'protected'
        $r.Json.results[1].status | Should Be 'failed'
        $r.Json.results[2].status | Should Be 'applied'
        $r.Json.results[2].sam | Should Be 'okuser'
        (Get-StubCalls -Path $state -Cmdlet 'New-ADUser').Count | Should Be 1
    }

    It 'New-IDLUser refuses the extended protected list (home/profile paths, proxyAddresses, accountExpires, msPKI-*) and cannot override the id attribute' {
        $state = New-SecState
        $names = @('homeDirectory', 'profilePath', 'proxyAddresses', 'accountExpires', 'mspki-foo', 'homeDrive', 'ms-DS-Bar')
        $items = @()
        $n = 0
        foreach ($name in $names){
            $n++
            $items += , (New-NewItem -Ext ('E86{0:00}' -f $n) -Sam "prot$n" -Cn "Prot $n" -Attributes @{ givenName = 'P'; $name = 'x' })
        }

        $id = New-NewItem -Ext 'E8699' -Sam 'idover' -Cn 'Id Over' -Attributes @{ givenName = 'I'; employeeID = 'E0000' }
        $r = Invoke-IDLScript -Script 'New-IDLUser' -Request (New-Req ($items + , $id)) -StatePath $state
        $r.ExitCode | Should Be 1
        for ($i = 0; $i -lt $names.Count; $i++){
            $r.Json.results[$i].status | Should Be 'failed'
            $r.Json.results[$i].error | Should Match 'protected'
            $r.Json.results[$i].error | Should Match ([regex]::Escape($names[$i]))
        }

        $r.Json.results[$names.Count].status | Should Be 'applied'
        (Get-StubUser -Path $state -Sam 'idover').attributes.employeeID | Should Be 'E8699'
        (Get-StubCalls -Path $state -Cmdlet 'New-ADUser').Count | Should Be 1
    }

    #endregion

    #region 4 - privileged groups

    It 'Get-IDLPrivilegedReason flags adminCount=1 and the privileged RIDs only' {
        Get-IDLPrivilegedReason -Group ([pscustomobject]@{ adminCount = 1 }) | Should Match 'adminCount'
        Get-IDLPrivilegedReason -Group ([pscustomobject]@{ objectSid = 'S-1-5-21-9-9-9-519' }) | Should Match '519'
        Get-IDLPrivilegedReason -Group ([pscustomobject]@{ adminCount = 0; objectSid = 'S-1-5-21-9-9-9-1519' }) | Should BeNullOrEmpty
        Get-IDLPrivilegedReason -Group ([pscustomobject]@{ adminCount = 0; objectSid = 'S-1-5-21-9-9-9-1234' }) | Should BeNullOrEmpty
        Get-IDLPrivilegedReason -Group $null | Should BeNullOrEmpty
    }

    It 'Set-IDLUser refuses to add or remove membership in any privileged group, with no partial change' {
        $privileged = @('GG-Flagged', 'Domain Admins', 'Schema Admins', 'Enterprise Admins', 'Administrators', 'Account Operators', 'Server Operators', 'Print Operators', 'Backup Operators')
        $memberOf = @($privileged | ForEach-Object { "CN=$_,OU=Groups,$root" })
        $jane = New-StubUserEntry -Sam 'jdoe' -ExternalId 'E1001' -Cn 'Jane Doe' -MemberOf $memberOf
        $state = New-SecState -Users @($jane)
        $items = @()
        foreach ($g in $privileged){
            $items += , ([ordered]@{ changeId = "a-$g"; externalId = 'E1001'; objectGuid = $jane.objectGuid; set = @{ title = 'Partial' }; addGroups = @($g) })
            $items += , ([ordered]@{ changeId = "r-$g"; externalId = 'E1001'; objectGuid = $jane.objectGuid; set = @{ title = 'Partial' }; removeGroups = @($g) })
        }

        $items += , ([ordered]@{ changeId = 'plain'; externalId = 'E1001'; objectGuid = $jane.objectGuid; addGroups = @('GG-Plain') })
        $r = Invoke-IDLScript -Script 'Set-IDLUser' -Request (New-Req $items) -StatePath $state
        $r.ExitCode | Should Be 1
        $n = $privileged.Count * 2
        @($r.Json.results | Select-Object -First $n | Where-Object { $_.status -eq 'failed' }).Count | Should Be $n
        foreach ($res in @($r.Json.results | Select-Object -First $n)){
            $res.error | Should Match 'privileged'
        }

        $r.Json.results[$n].status | Should Be 'applied'
        (Get-StubCalls -Path $state -Cmdlet 'Set-ADUser').Count | Should Be 0
        (Get-StubCalls -Path $state -Cmdlet 'Remove-ADGroupMember').Count | Should Be 0
        $u = Get-StubUser -Path $state -Sam 'jdoe'
        $u.attributes.PSObject.Properties['title'] | Should BeNullOrEmpty
        @($u.memberOf).Count | Should Be ($privileged.Count + 1)
    }

    It 'Enable/New refuse privileged groups (also in DryRun) before changing anything; Disable keeps them and still disables' {
        $off = New-StubUserEntry -Sam 'off1' -ExternalId 'E2001' -Cn 'Off One' -Enabled $false -Ou "OU=Disabled,$root"
        $on = New-StubUserEntry -Sam 'on1' -ExternalId 'E2002' -Cn 'On One' -MemberOf @("CN=Administrators,OU=Groups,$root")
        $state = New-SecState -Users @($off, $on)

        $e = [ordered]@{ changeId = 'e'; externalId = 'E2001'; objectGuid = $off.objectGuid; addGroups = @('Domain Admins'); moveTo = "OU=Staff,$root" }
        $r = Invoke-IDLScript -Script 'Enable-IDLUser' -Request (New-Req @($e)) -StatePath $state
        $r.Json.results[0].status | Should Be 'failed'
        $r.Json.results[0].error | Should Match 'privileged'

        # Disable is lifecycle-critical: a privileged group is KEPT (warned), never a reason to leave the account enabled.
        $d = [ordered]@{ changeId = 'd'; externalId = 'E2002'; objectGuid = $on.objectGuid; removeGroups = @('Administrators'); moveTo = "OU=Disabled,$root" }
        $r = Invoke-IDLScript -Script 'Disable-IDLUser' -Request (New-Req @($d)) -StatePath $state -DryRun
        $r.Json.results[0].status | Should Be 'would'
        $r.Json.results[0].message | Should Match 'kept privileged group\(s\): Administrators'
        (Get-StubCalls -Path $state).Count | Should Be 0
        $r = Invoke-IDLScript -Script 'Disable-IDLUser' -Request (New-Req @($d)) -StatePath $state
        $r.Json.results[0].status | Should Be 'applied'
        $r.Json.results[0].message | Should Match 'kept privileged group\(s\): Administrators'
        (Get-StubCalls -Path $state -Cmdlet 'Remove-ADGroupMember').Count | Should Be 0
        $u = Get-StubUser -Path $state -Sam 'on1'
        $u.enabled | Should Be $false
        @($u.memberOf) -contains "CN=Administrators,OU=Groups,$root" | Should Be $true

        $n1 = New-NewItem -Ext 'E8201' -Sam 'privnew' -Cn 'Priv New' -Groups @('GG-Plain', 'GG-Flagged')
        $n2 = New-NewItem -Ext 'E8202' -Sam 'privnew2' -Cn 'Priv New2' -Groups @('Backup Operators')
        $n3 = New-NewItem -Ext 'E8203' -Sam 'plainnew' -Cn 'Plain New' -Groups @('GG-Plain')
        foreach ($dry in @($true, $false)){
            $r = if ($dry) { Invoke-IDLScript -Script 'New-IDLUser' -Request (New-Req @($n1, $n2, $n3)) -StatePath $state -DryRun } else { Invoke-IDLScript -Script 'New-IDLUser' -Request (New-Req @($n1, $n2, $n3)) -StatePath $state }
            $r.ExitCode | Should Be 1
            $r.Json.results[0].status | Should Be 'failed'
            $r.Json.results[0].error | Should Match 'privileged'
            $r.Json.results[1].status | Should Be 'failed'
            $r.Json.results[2].status | Should Be ($dry ? 'would' : 'applied')
        }

        (Get-StubUser -Path $state -Sam 'privnew') | Should BeNullOrEmpty
        (Get-StubUser -Path $state -Sam 'privnew2') | Should BeNullOrEmpty
        (Get-StubUser -Path $state -Sam 'plainnew') | Should Not BeNullOrEmpty
        (Get-StubCalls -Path $state -Cmdlet 'Enable-ADAccount').Count | Should Be 0
        (Get-StubCalls -Path $state -Cmdlet 'Disable-ADAccount').Count | Should Be 1
        (Get-StubCalls -Path $state -Cmdlet 'New-ADUser').Count | Should Be 1
    }

    #endregion

    #region 5 - ambiguous groups

    It 'fails (not last-wins) when a group name matches more than one group, and does not ignore it on removal' {
        $jane = New-StubUserEntry -Sam 'jdoe' -ExternalId 'E1001' -Cn 'Jane Doe'
        $state = New-SecState -Users @($jane)
        $s = [ordered]@{ changeId = 's'; externalId = 'E1001'; objectGuid = $jane.objectGuid; addGroups = @('GG-Dup') }
        $r = Invoke-IDLScript -Script 'Set-IDLUser' -Request (New-Req @($s)) -StatePath $state
        $r.Json.results[0].status | Should Be 'failed'
        $r.Json.results[0].error | Should Match 'ambiguous'

        # Disable never guesses either, but an ambiguous group must not keep a leaver enabled: kept + warned.
        $d = [ordered]@{ changeId = 'd'; externalId = 'E1001'; objectGuid = $jane.objectGuid; removeGroups = @('GG-Dup') }
        $r = Invoke-IDLScript -Script 'Disable-IDLUser' -Request (New-Req @($d)) -StatePath $state
        $r.Json.results[0].status | Should Be 'applied'
        $r.Json.results[0].message | Should Match 'kept ambiguous group\(s\): GG-Dup'
        (Get-StubCalls -Path $state -Cmdlet 'Remove-ADGroupMember').Count | Should Be 0

        $n = New-NewItem -Ext 'E8301' -Sam 'dupnew' -Cn 'Dup New' -Groups @('GG-Dup')
        $ok = New-NewItem -Ext 'E8302' -Sam 'dupok' -Cn 'Dup Ok' -Groups @('GG-Plain')
        $r = Invoke-IDLScript -Script 'New-IDLUser' -Request (New-Req @($n, $ok)) -StatePath $state
        $r.ExitCode | Should Be 1
        $r.Json.results[0].status | Should Be 'failed'
        $r.Json.results[0].error | Should Match 'ambiguous'
        $r.Json.results[1].status | Should Be 'applied'
        (Get-StubUser -Path $state -Sam 'dupnew') | Should BeNullOrEmpty
        (Get-StubCalls -Path $state -Cmdlet 'Add-ADGroupMember').Count | Should Be 1
        (Get-StubCalls -Path $state -Cmdlet 'Disable-ADAccount').Count | Should Be 1
    }

    #endregion

    #region 6 - New-IDLUser malformed / unverifiable OU

    It 'Test-IDLDnShape accepts well-formed DNs and rejects malformed ones' {
        foreach ($ok in @('OU=Staff,DC=corp,DC=example,DC=com', 'CN=Doe\, Jane,OU=a b,DC=x', 'DC=x')){
            Test-IDLDnShape -Value $ok | Should Be $true
        }

        foreach ($bad in @('', 'garbage', 'OU=,DC=x', 'OU=a,,DC=b', 'OU=a,', ',OU=a', '=OU,DC=x', "OU=a`n,DC=x", 'OU=a=b,DC=x')){
            Test-IDLDnShape -Value $bad | Should Be $false
        }
    }

    It 'a malformed ou fails only that item; the rest of the batch is processed (exit 1)' {
        $state = New-SecState
        $items = @(
            (New-NewItem -Ext 'E8401' -Sam 'u1' -Cn 'U One' -Ou 'garbage'),
            (New-NewItem -Ext 'E8402' -Sam 'u2' -Cn 'U Two' -Ou "OU=Staff,$root,"),
            (New-NewItem -Ext 'E8403' -Sam 'u3' -Cn 'U Three' -Ou 'OU=,DC=stub,DC=test'),
            (New-NewItem -Ext 'E8404' -Sam 'u4' -Cn 'U Four')
        )
        $r = Invoke-IDLScript -Script 'New-IDLUser' -Request (New-Req $items) -StatePath $state
        $r.ExitCode | Should Be 1
        $r.Json.ok | Should Be $true
        @($r.Json.results | ForEach-Object { $_.status }) -join ',' | Should Be 'failed,failed,failed,applied'
        $r.Json.results[0].error | Should Match 'well-formed'
        (Get-StubCalls -Path $state -Cmdlet 'New-ADUser').Count | Should Be 1
    }

    It 'an AD error while verifying one ou (not "not found") fails only the items for that ou' {
        $fault = @{ cmdlet = 'Get-ADObject'; match = 'OU=Broken'; message = 'A referral was returned from the server.' }
        $state = New-SecState -Faults @($fault)
        $items = @(
            (New-NewItem -Ext 'E8501' -Sam 'b1' -Cn 'B One' -Ou "OU=Broken,$root"),
            (New-NewItem -Ext 'E8502' -Sam 'b2' -Cn 'B Two' -Ou "OU=Nowhere,$root"),
            (New-NewItem -Ext 'E8503' -Sam 'b3' -Cn 'B Three')
        )
        $r = Invoke-IDLScript -Script 'New-IDLUser' -Request (New-Req $items) -StatePath $state
        $r.ExitCode | Should Be 1
        $r.Json.ok | Should Be $true
        @($r.Json.results | ForEach-Object { $_.status }) -join ',' | Should Be 'failed,failed,applied'
        $r.Json.results[0].error | Should Match 'Cannot verify target OU'
        $r.Json.results[0].error | Should Match 'referral'
        $r.Json.results[1].error | Should Match 'does not exist'
        (Get-StubUser -Path $state -Sam 'b3') | Should Not BeNullOrEmpty
    }

    #endregion

    #region 7 - short passwords

    It 'rejects passwords shorter than 8 characters per item, never creating the account or echoing the password' {
        $state = New-SecState
        $items = @(
            (New-NewItem -Ext 'E8601' -Sam 'p1' -Cn 'P One' -Password 'Zq9'),
            (New-NewItem -Ext 'E8602' -Sam 'p2' -Cn 'P Two' -Password 'Zq9!xY7'),
            (New-NewItem -Ext 'E8603' -Sam 'p3' -Cn 'P Three' -Password 'Zq9!xY7w'),
            (New-NewItem -Ext 'E8604' -Sam 'p4' -Cn 'P Four')
        )
        $r = Invoke-IDLScript -Script 'New-IDLUser' -Request (New-Req $items) -StatePath $state
        $r.ExitCode | Should Be 1
        @($r.Json.results | ForEach-Object { $_.status }) -join ',' | Should Be 'failed,failed,applied,applied'
        $r.Json.results[0].error | Should Match 'too short'
        $r.Json.results[1].error | Should Match 'too short'
        (Get-StubUser -Path $state -Sam 'p1') | Should BeNullOrEmpty
        (Get-StubUser -Path $state -Sam 'p2') | Should BeNullOrEmpty
        foreach ($text in @($r.StdOut, $r.StdErr, $r.LogText, $r.ReportText, [System.IO.File]::ReadAllText($state))){
            $text.Contains('Zq9!xY7') | Should Be $false
            $text.Contains('"Zq9"') | Should Be $false
        }
    }

    #endregion
}
