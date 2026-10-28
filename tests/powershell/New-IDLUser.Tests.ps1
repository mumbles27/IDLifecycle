$here = Split-Path -Parent $MyInvocation.MyCommand.Path
. (Join-Path $here 'TestHelpers.ps1')

Describe 'New-IDLUser.ps1' {

    $secret = 'S3cr3t!Pa55w0rd#xyz'

    function New-Item1 {
        param(
            [string]$ExternalId = 'E3001',
            [string]$ChangeId = 'chg-1',
            [string]$Sam = 'jdoe',
            [string]$Cn = 'Jane Doe',
            [string[]]$Groups = @('GG-AllStaff'),
            [string]$Ou = "OU=Staff,$script:Root",
            [object[]]$Candidates = $null
        )

        if ($null -eq $Candidates){
            $Candidates = @(
                @{ sam = $Sam; upn = "$Sam@stub.test"; cn = $Cn },
                @{ sam = "${Sam}2"; upn = "${Sam}2@stub.test"; cn = "$Cn (2)" }
            )
        }

        return [ordered]@{
            changeId              = $ChangeId
            externalId            = $ExternalId
            ou                    = $Ou
            candidates            = $Candidates
            attributes            = [ordered]@{ givenName = 'Jane'; sn = 'Doe'; displayName = $Cn; title = 'Analyst' }
            groups                = $Groups
            password              = $secret
            enabled               = $true
            changePasswordAtLogon = $true
        }
    }

    function New-Request {
        param([object[]]$Items, [string]$Server = '')
        return [ordered]@{ server = $Server; idAttribute = 'employeeID'; items = @($Items) }
    }

    It 'creates the account with correct DN, attributes, groups and returns guid/dn/sam/upn' {
        $state = New-StandardState
        $r = Invoke-IDLScript -Script 'New-IDLUser' -Request (New-Request @(New-Item1)) -StatePath $state

        $r.ExitCode | Should Be 0
        $r.SingleJson | Should Be $true
        $r.Json.ok | Should Be $true
        $r.Json.dryRun | Should Be $false
        $r.Json.summary.total | Should Be 1
        $r.Json.summary.applied | Should Be 1
        $res = $r.Json.results[0]
        $res.changeId | Should Be 'chg-1'
        $res.externalId | Should Be 'E3001'
        $res.status | Should Be 'applied'
        $res.dn | Should Be "CN=Jane Doe,OU=Staff,$script:Root"
        $res.sam | Should Be 'jdoe'
        $res.upn | Should Be 'jdoe@stub.test'
        $res.objectGuid | Should Match '^[0-9a-f]{8}-[0-9a-f]{4}-'
        $res.error | Should BeNullOrEmpty

        $user = Get-StubUser -Path $state -Sam 'jdoe'
        $user | Should Not BeNullOrEmpty
        $user.objectGuid | Should Be $res.objectGuid
        $user.dn | Should Be $res.dn
        $user.enabled | Should Be $true
        $user.attributes.employeeID | Should Be 'E3001'
        $user.attributes.givenName | Should Be 'Jane'
        $user.attributes.title | Should Be 'Analyst'
        $user.attributes.pwdLastSet | Should Be '0'
        @($user.memberOf) -contains "CN=GG-AllStaff,OU=Groups,$script:Root" | Should Be $true
    }

    It 'writes a timestamped log and CSV report with the standard columns' {
        $state = New-StandardState
        $r = Invoke-IDLScript -Script 'New-IDLUser' -Request (New-Request @(New-Item1)) -StatePath $state -KeepFiles
        try{
            $r.LogFile | Should Not BeNullOrEmpty
            $r.ReportFile | Should Not BeNullOrEmpty
            (Split-Path -Leaf $r.LogFile) | Should Match '^New-IDLUser_\d{8}_\d{6}(_\d+)?\.log$'
            (Split-Path -Leaf $r.ReportFile) | Should Match '^New-IDLUser_Report_\d{8}_\d{6}(_\d+)?\.csv$'
            (Split-Path -Parent $r.LogFile) | Should Be (Join-Path $script:ScriptsDir 'Logs')

            $r.LogText | Should Match '\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2} \[INFO\] Script start'
            $r.LogText | Should Match 'Phase done: Process items'
            $r.LogText | Should Match '---- Summary ----'
            $r.LogText | Should Match 'ExitCode=0'

            ($r.ReportText -split "`r?`n")[0] | Should Be 'Timestamp,Action,Result,Identity,ObjectIdOrDN,Details,Error'
            $rows = @(Import-Csv -LiteralPath $r.ReportFile)
            @($rows | Where-Object { $_.Action -eq 'CreateUser' -and $_.Result -eq 'Applied' }).Count | Should Be 1
            @($rows | Where-Object { $_.Action -eq 'AddGroupMember' -and $_.Result -eq 'Applied' }).Count | Should Be 1
        }
        finally{
            foreach ($f in $r.NewFiles){
                Remove-Item -LiteralPath $f -Force -ErrorAction SilentlyContinue
            }
        }
    }

    It 'DryRun makes zero mutating calls, reports status would, and still validates' {
        $state = New-StandardState
        $r = Invoke-IDLScript -Script 'New-IDLUser' -Request (New-Request @(New-Item1)) -StatePath $state -DryRun

        $r.ExitCode | Should Be 0
        $r.Json.dryRun | Should Be $true
        $r.Json.results[0].status | Should Be 'would'
        $r.Json.results[0].dn | Should Be "CN=Jane Doe,OU=Staff,$script:Root"
        $r.Json.results[0].sam | Should Be 'jdoe'
        $r.Json.results[0].objectGuid | Should BeNullOrEmpty
        $r.Json.summary.would | Should Be 1
        $r.Json.summary.applied | Should Be 0
        (Get-StubCalls -Path $state).Count | Should Be 0
        (Read-StubStateFile -Path $state).users.Count | Should Be 0
        $r.LogText | Should Match 'WOULD: create'
        $r.ReportText | Should Match 'Would'
    }

    It 'is idempotent: a second run reports skipped/exists and creates no duplicate' {
        $state = New-StandardState
        $req = New-Request @(New-Item1)
        $first = Invoke-IDLScript -Script 'New-IDLUser' -Request $req -StatePath $state
        $second = Invoke-IDLScript -Script 'New-IDLUser' -Request $req -StatePath $state

        $first.Json.results[0].status | Should Be 'applied'
        $second.ExitCode | Should Be 0
        $second.Json.results[0].status | Should Be 'skipped'
        $second.Json.results[0].message | Should Be 'exists'
        $second.Json.results[0].objectGuid | Should Be $first.Json.results[0].objectGuid
        $second.Json.results[0].dn | Should Be $first.Json.results[0].dn
        $second.Json.results[0].sam | Should Be 'jdoe'
        (Get-StubCalls -Path $state -Cmdlet 'New-ADUser').Count | Should Be 1
        (Read-StubStateFile -Path $state).users.Count | Should Be 1
    }

    It 'DryRun of an existing account is skipped (exists), not would' {
        $existing = New-StubUserEntry -Sam 'already' -ExternalId 'E3001' -Cn 'Already Here'
        $state = New-StandardState -Users @($existing)
        $r = Invoke-IDLScript -Script 'New-IDLUser' -Request (New-Request @(New-Item1)) -StatePath $state -DryRun
        $r.Json.results[0].status | Should Be 'skipped'
        $r.Json.results[0].sam | Should Be 'already'
        $r.Json.results[0].objectGuid | Should Be $existing.objectGuid
    }

    It 'picks the 2nd candidate when the 1st sAMAccountName is taken' {
        $other = New-StubUserEntry -Sam 'jdoe' -ExternalId 'E0001' -Cn 'Somebody Else'
        $state = New-StandardState -Users @($other)
        $r = Invoke-IDLScript -Script 'New-IDLUser' -Request (New-Request @(New-Item1)) -StatePath $state
        $r.ExitCode | Should Be 0
        $r.Json.results[0].status | Should Be 'applied'
        $r.Json.results[0].sam | Should Be 'jdoe2'
        $r.Json.results[0].upn | Should Be 'jdoe2@stub.test'
        $r.Json.results[0].dn | Should Be "CN=Jane Doe (2),OU=Staff,$script:Root"
    }

    It 'sets the chosen candidate''s mail (it replaces a mail attribute); a malformed candidate mail fails the item' {
        $other = New-StubUserEntry -Sam 'jdoe' -ExternalId 'E0001' -Cn 'Somebody Else'
        $state = New-StandardState -Users @($other)
        $item = New-Item1 -Candidates @(
            @{ sam = 'jdoe'; upn = 'jdoe@stub.test'; cn = 'Jane Doe'; mail = 'jane.doe@mail.stub.test' },
            @{ sam = 'jdoe2'; upn = 'jdoe2@stub.test'; cn = 'Jane Doe (2)'; mail = 'jane.doe2@mail.stub.test' }
        )
        $item['attributes']['mail'] = 'from-mapping@stub.test'
        $r = Invoke-IDLScript -Script 'New-IDLUser' -Request (New-Request @($item)) -StatePath $state
        $r.ExitCode | Should Be 0
        $r.Json.results[0].sam | Should Be 'jdoe2'
        (Get-StubUser -Path $state -Sam 'jdoe2').attributes.mail | Should Be 'jane.doe2@mail.stub.test'
        $r.LogText | Should Match "attribute 'mail' replaced by the candidate's mail address"

        $state2 = New-StandardState
        $bad = New-Item1 -Candidates @(@{ sam = 'jdoe'; upn = 'jdoe@stub.test'; cn = 'Jane Doe'; mail = 'not-an-address' })
        $r = Invoke-IDLScript -Script 'New-IDLUser' -Request (New-Request @($bad)) -StatePath $state2
        $r.ExitCode | Should Be 1
        $r.Json.results[0].error | Should Match 'is not an e-mail address'
        (Read-StubStateFile -Path $state2).users.Count | Should Be 0
    }

    It 'picks the 2nd candidate when the 1st UPN is taken (even though sam is free)' {
        $other = New-StubUserEntry -Sam 'zzz' -ExternalId 'E0002' -Cn 'Zed'
        $other.upn = 'jdoe@stub.test'
        $state = New-StandardState -Users @($other)
        $r = Invoke-IDLScript -Script 'New-IDLUser' -Request (New-Request @(New-Item1)) -StatePath $state
        $r.Json.results[0].sam | Should Be 'jdoe2'
    }

    It 'treats a sAMAccountName used by a group as taken (directory-wide)' {
        $state = New-StubStateFile -Ous @("OU=Staff,$script:Root", "OU=Groups,$script:Root") -Groups @(
            @{ dn = "CN=jdoe,OU=Groups,$script:Root"; sam = 'jdoe' }
        )
        $item = New-Item1 -Groups @()
        $r = Invoke-IDLScript -Script 'New-IDLUser' -Request (New-Request @($item)) -StatePath $state
        $r.Json.results[0].sam | Should Be 'jdoe2'
    }

    It 'picks the 2nd candidate when the 1st CN is taken in the target OU, but not when taken in another OU' {
        $inOu = New-StubUserEntry -Sam 'other1' -ExternalId 'E0003' -Cn 'Jane Doe' -Ou "OU=Staff,$script:Root"
        $elsewhere = New-StubUserEntry -Sam 'other2' -ExternalId 'E0004' -Cn 'Jane Doe' -Ou "OU=Sales,$script:Root"

        $state = New-StandardState -Users @($inOu)
        $r = Invoke-IDLScript -Script 'New-IDLUser' -Request (New-Request @(New-Item1)) -StatePath $state
        $r.Json.results[0].sam | Should Be 'jdoe2'
        $r.Json.results[0].dn | Should Be "CN=Jane Doe (2),OU=Staff,$script:Root"

        $state2 = New-StandardState -Users @($elsewhere)
        $r2 = Invoke-IDLScript -Script 'New-IDLUser' -Request (New-Request @(New-Item1)) -StatePath $state2
        $r2.Json.results[0].sam | Should Be 'jdoe'
        $r2.Json.results[0].dn | Should Be "CN=Jane Doe,OU=Staff,$script:Root"
    }

    $mailCandidates = @(
        @{ sam = 'jdoe'; upn = 'jdoe@stub.test'; cn = 'Jane Doe'; mail = 'jane.doe@stub.test' },
        @{ sam = 'jdoe2'; upn = 'jdoe2@stub.test'; cn = 'Jane Doe (2)'; mail = 'jane.doe2@stub.test' }
    )

    It 'treats a candidate whose mail is used by another user (mail attribute, any case) as taken' {
        $other = New-StubUserEntry -Sam 'jdoe.old' -ExternalId 'E0001' -Cn 'Other Jane' -Attributes @{ mail = 'Jane.Doe@STUB.test' }
        $state = New-StandardState -Users @($other)
        $r = Invoke-IDLScript -Script 'New-IDLUser' -Request (New-Request @(New-Item1 -Candidates $mailCandidates -Groups @())) -StatePath $state
        $r.ExitCode | Should Be 0
        $r.Json.results[0].sam | Should Be 'jdoe2'
        (Get-StubUser -Path $state -Sam 'jdoe2').attributes.mail | Should Be 'jane.doe2@stub.test'
        $r.LogText | Should Match "mail='jane.doe@stub.test' is taken"
    }

    It 'treats a mail held as an smtp: proxyAddresses entry by a contact (not a user) as taken' {
        $contact = @{
            dn          = "CN=Jane Doe (vendor),OU=Sales,$script:Root"
            objectClass = 'contact'
            attributes  = @{ mail = 'jane@vendor.example'; proxyAddresses = @('SMTP:jane@vendor.example', 'smtp:jane.doe@stub.test') }
        }
        $state = New-StandardState -Objects @($contact)
        $r = Invoke-IDLScript -Script 'New-IDLUser' -Request (New-Request @(New-Item1 -Candidates $mailCandidates -Groups @())) -StatePath $state
        $r.ExitCode | Should Be 0
        $r.Json.results[0].sam | Should Be 'jdoe2'
    }

    It 'treats a mail consumed earlier in the same batch as taken; fails when every candidate mail is taken' {
        $state = New-StandardState
        $first = New-Item1 -ExternalId 'E3001' -ChangeId 'a' -Groups @() -Candidates @(@{ sam = 'jdoe'; upn = 'jdoe@stub.test'; cn = 'Jane Doe'; mail = 'jane.doe@stub.test' })
        $second = New-Item1 -ExternalId 'E3002' -ChangeId 'b' -Groups @() -Candidates @(
            @{ sam = 'jdoe9'; upn = 'jdoe9@stub.test'; cn = 'Jane Doe (9)'; mail = 'jane.doe@stub.test' },
            @{ sam = 'jdoe8'; upn = 'jdoe8@stub.test'; cn = 'Jane Doe (8)'; mail = 'jane.doe8@stub.test' }
        )
        $third = New-Item1 -ExternalId 'E3003' -ChangeId 'c' -Groups @() -Candidates @(@{ sam = 'jdoe7'; upn = 'jdoe7@stub.test'; cn = 'Jane Doe (7)'; mail = 'JANE.DOE8@stub.test' })
        $r = Invoke-IDLScript -Script 'New-IDLUser' -Request (New-Request @($first, $second, $third)) -StatePath $state
        $r.ExitCode | Should Be 1
        $r.Json.results[0].sam | Should Be 'jdoe'
        $r.Json.results[1].sam | Should Be 'jdoe8'
        $r.Json.results[2].status | Should Be 'failed'
        $r.Json.results[2].error | Should Match 'no free candidate.*mail'
        (Get-StubCalls -Path $state -Cmdlet 'New-ADUser').Count | Should Be 2
    }

    It 'fails an item when no candidate is free, without stopping the batch (exit 1)' {
        $other = New-StubUserEntry -Sam 'jdoe' -ExternalId 'E0001'
        $other2 = New-StubUserEntry -Sam 'jdoe2' -ExternalId 'E0002'
        $state = New-StandardState -Users @($other, $other2)
        $items = @(
            (New-Item1 -ExternalId 'E3001' -ChangeId 'a'),
            (New-Item1 -ExternalId 'E3002' -ChangeId 'b' -Sam 'asmith' -Cn 'Ann Smith')
        )
        $r = Invoke-IDLScript -Script 'New-IDLUser' -Request (New-Request $items) -StatePath $state
        $r.ExitCode | Should Be 1
        $r.Json.ok | Should Be $true
        $r.Json.results[0].status | Should Be 'failed'
        $r.Json.results[0].error | Should Match 'no free candidate'
        $r.Json.results[1].status | Should Be 'applied'
        $r.Json.summary.failed | Should Be 1
        $r.Json.summary.applied | Should Be 1
    }

    It 'treats candidates taken earlier in the same batch as taken' {
        $state = New-StandardState
        $items = @(
            (New-Item1 -ExternalId 'E3001' -ChangeId 'a' -Groups @()),
            (New-Item1 -ExternalId 'E3002' -ChangeId 'b' -Groups @())
        )
        $r = Invoke-IDLScript -Script 'New-IDLUser' -Request (New-Request $items) -StatePath $state
        $r.ExitCode | Should Be 0
        $r.Json.results[0].sam | Should Be 'jdoe'
        $r.Json.results[1].sam | Should Be 'jdoe2'
        $r.Json.results[1].dn | Should Be "CN=Jane Doe (2),OU=Staff,$script:Root"

        # same in DryRun
        $state2 = New-StandardState
        $r2 = Invoke-IDLScript -Script 'New-IDLUser' -Request (New-Request $items) -StatePath $state2 -DryRun
        $r2.Json.results[0].sam | Should Be 'jdoe'
        $r2.Json.results[1].sam | Should Be 'jdoe2'
    }

    It 'skips a duplicate externalId within one batch instead of creating two accounts' {
        $state = New-StandardState
        $items = @(
            (New-Item1 -ExternalId 'E3001' -ChangeId 'a' -Groups @()),
            (New-Item1 -ExternalId 'E3001' -ChangeId 'b' -Groups @())
        )
        $r = Invoke-IDLScript -Script 'New-IDLUser' -Request (New-Request $items) -StatePath $state
        $r.Json.results[0].status | Should Be 'applied'
        $r.Json.results[1].status | Should Be 'skipped'
        (Read-StubStateFile -Path $state).users.Count | Should Be 1
    }

    It 'adds groups given as sAMAccountName or DN' {
        $state = New-StandardState
        $item = New-Item1 -Groups @('GG-AllStaff', "CN=GG-Sales,OU=Groups,$script:Root")
        $r = Invoke-IDLScript -Script 'New-IDLUser' -Request (New-Request @($item)) -StatePath $state
        $r.Json.results[0].status | Should Be 'applied'
        $user = Get-StubUser -Path $state -Sam 'jdoe'
        @($user.memberOf).Count | Should Be 2
        (Get-StubCalls -Path $state -Cmdlet 'Add-ADGroupMember').Count | Should Be 2
    }

    It 'keeps the account but fails the item when a group cannot be resolved' {
        $state = New-StandardState
        $item = New-Item1 -Groups @('GG-AllStaff', 'GG-DoesNotExist')
        $r = Invoke-IDLScript -Script 'New-IDLUser' -Request (New-Request @($item)) -StatePath $state
        $r.ExitCode | Should Be 1
        $res = $r.Json.results[0]
        $res.status | Should Be 'failed'
        $res.error | Should Match 'GG-DoesNotExist'
        $res.message | Should Match 'GG-DoesNotExist'
        $res.objectGuid | Should Match '^[0-9a-f]{8}-'
        $res.dn | Should Be "CN=Jane Doe,OU=Staff,$script:Root"

        $user = Get-StubUser -Path $state -Sam 'jdoe'
        $user | Should Not BeNullOrEmpty
        @($user.memberOf) -contains "CN=GG-AllStaff,OU=Groups,$script:Root" | Should Be $true
    }

    It 'isolates per-item failures: one AD error does not stop the other items (exit 1)' {
        $fault = @{ cmdlet = 'New-ADUser'; match = 'badguy'; message = 'The server is unwilling to process the request.' }
        $state = New-StandardState -Faults @($fault)
        $items = @(
            (New-Item1 -ExternalId 'E3001' -ChangeId 'a' -Sam 'goodone' -Cn 'Good One' -Groups @()),
            (New-Item1 -ExternalId 'E3002' -ChangeId 'b' -Sam 'badguy' -Cn 'Bad Guy' -Groups @()),
            (New-Item1 -ExternalId 'E3003' -ChangeId 'c' -Sam 'goodtwo' -Cn 'Good Two' -Groups @())
        )
        $r = Invoke-IDLScript -Script 'New-IDLUser' -Request (New-Request $items) -StatePath $state
        $r.ExitCode | Should Be 1
        $r.Json.ok | Should Be $true
        $r.Json.results[0].status | Should Be 'applied'
        $r.Json.results[1].status | Should Be 'failed'
        $r.Json.results[1].error | Should Match 'unwilling'
        $r.Json.results[2].status | Should Be 'applied'
        $r.Json.summary.total | Should Be 3
        $r.Json.summary.failed | Should Be 1
        (Get-StubUser -Path $state -Sam 'goodtwo') | Should Not BeNullOrEmpty
        (Get-StubUser -Path $state -Sam 'badguy') | Should BeNullOrEmpty
    }

    It 'fails items with invalid fields individually (missing password, bad OU, no candidates, missing externalId)' {
        $state = New-StandardState
        $noPassword = New-Item1 -ExternalId 'E4001' -ChangeId 'np' -Sam 'nopass' -Cn 'No Pass' -Groups @()
        $noPassword.Remove('password')
        $badOu = New-Item1 -ExternalId 'E4002' -ChangeId 'bo' -Sam 'badou' -Cn 'Bad Ou' -Groups @() -Ou "OU=Nowhere,$script:Root"
        $noCand = New-Item1 -ExternalId 'E4003' -ChangeId 'nc' -Groups @()
        $noCand['candidates'] = @()
        $noExt = New-Item1 -ExternalId '' -ChangeId 'ne' -Groups @()
        $good = New-Item1 -ExternalId 'E4005' -ChangeId 'ok' -Sam 'fine' -Cn 'Fine User' -Groups @()

        $r = Invoke-IDLScript -Script 'New-IDLUser' -Request (New-Request @($noPassword, $badOu, $noCand, $noExt, $good)) -StatePath $state
        $r.ExitCode | Should Be 1
        $statuses = @($r.Json.results | ForEach-Object { $_.status })
        ($statuses -join ',') | Should Be 'failed,failed,failed,failed,applied'
        $r.Json.results[0].error | Should Match 'password'
        $r.Json.results[1].error | Should Match 'OU'
        $r.Json.results[2].error | Should Match 'candidates'
        $r.Json.results[3].error | Should Match 'externalId'
        (Get-StubCalls -Path $state -Cmdlet 'New-ADUser').Count | Should Be 1
    }

    It 'rejects invalid requests with exit code 2 and {"ok":false}' {
        $state = New-StandardState
        $cases = @(
            '{"idAttribute":"employeeID"}',
            '{"idAttribute":"employeeID","items":"nope"}',
            '{"items":[]}',
            '{"idAttribute":"bad name","items":[]}',
            '[1,2,3]',
            'not json at all'
        )
        foreach ($raw in $cases){
            $r = Invoke-IDLScript -Script 'New-IDLUser' -RawInput $raw -StatePath $state
            $r.ExitCode | Should Be 2
            $r.SingleJson | Should Be $true
            $r.Json.ok | Should Be $false
            $r.Json.error | Should Not BeNullOrEmpty
        }

        (Get-StubCalls -Path $state).Count | Should Be 0
    }

    It 'accepts an empty items array (exit 0, empty results)' {
        $state = New-StandardState
        $r = Invoke-IDLScript -Script 'New-IDLUser' -Request (New-Request @()) -StatePath $state
        $r.ExitCode | Should Be 0
        $r.Json.ok | Should Be $true
        $r.Json.summary.total | Should Be 0
        $r.StdOut | Should Match '"results":\[\]'
    }

    It 'exits 4 when AD is unreachable and 3 when the module cannot load' {
        $down = New-StubStateFile -FailConnect $true
        $r = Invoke-IDLScript -Script 'New-IDLUser' -Request (New-Request @(New-Item1)) -StatePath $down
        $r.ExitCode | Should Be 4
        $r.Json.ok | Should Be $false

        $r2 = Invoke-IDLScript -Script 'New-IDLUser' -Request (New-Request @(New-Item1)) -NoState
        $r2.ExitCode | Should Be 3
        $r2.Json.ok | Should Be $false
    }

    It 'never leaks the password: stdout, stderr, log, report and AD state stay clean' {
        $state = New-StandardState
        $r = Invoke-IDLScript -Script 'New-IDLUser' -Request (New-Request @(New-Item1)) -StatePath $state
        $r.ExitCode | Should Be 0
        $r.StdOut.Contains($secret) | Should Be $false
        $r.StdErr.Contains($secret) | Should Be $false
        $r.LogText.Contains($secret) | Should Be $false
        $r.ReportText.Contains($secret) | Should Be $false
        ([System.IO.File]::ReadAllText($state)).Contains($secret) | Should Be $false
        $call = (Get-StubCalls -Path $state -Cmdlet 'New-ADUser')[0]
        $call.args.AccountPassword | Should Be '***'
    }

    It 'scrubs the password even when an AD error message echoes it' {
        $fault = @{ cmdlet = 'New-ADUser'; match = ''; message = 'Password policy violation for value {password} on create' }
        $state = New-StandardState -Faults @($fault)
        $r = Invoke-IDLScript -Script 'New-IDLUser' -Request (New-Request @(New-Item1)) -StatePath $state
        $r.ExitCode | Should Be 1
        $r.Json.results[0].status | Should Be 'failed'
        $r.Json.results[0].error | Should Match 'Password policy violation for value \*\*\* on create'
        $r.StdOut.Contains($secret) | Should Be $false
        $r.StdErr.Contains($secret) | Should Be $false
        $r.LogText.Contains($secret) | Should Be $false
        $r.ReportText.Contains($secret) | Should Be $false
        ([System.IO.File]::ReadAllText($state)).Contains($secret) | Should Be $false
    }

    It 'round-trips non-ASCII names (UTF-8 stdin/stdout) and escapes RDN special characters' {
        $state = New-StandardState
        $item = New-Item1 -ExternalId 'E5001' -Sam 'jmunoz' -Cn 'José Muñoz, Jr.' -Groups @()
        $r = Invoke-IDLScript -Script 'New-IDLUser' -Request (New-Request @($item)) -StatePath $state
        $r.ExitCode | Should Be 0
        $r.Json.results[0].dn | Should Be "CN=José Muñoz\, Jr.,OU=Staff,$script:Root"
        (Get-StubUser -Path $state -Sam 'jmunoz').dn | Should Be "CN=José Muñoz\, Jr.,OU=Staff,$script:Root"
    }

    It 'escapes LDAP metacharacters: an externalId containing * does not match other accounts' {
        $existing = New-StubUserEntry -Sam 'e1' -ExternalId 'E1001' -Cn 'Existing One'
        $state = New-StandardState -Users @($existing)
        $item = New-Item1 -ExternalId 'E*' -Sam 'wild' -Cn 'Wild Card' -Groups @()
        $r = Invoke-IDLScript -Script 'New-IDLUser' -Request (New-Request @($item)) -StatePath $state
        $r.Json.results[0].status | Should Be 'applied'
        (Get-StubUser -Path $state -Sam 'wild').attributes.employeeID | Should Be 'E*'
    }

    It 'accepts the optional server field and ignores nothing else' {
        $state = New-StandardState
        $r = Invoke-IDLScript -Script 'New-IDLUser' -Request (New-Request @(New-Item1) -Server 'dc01.stub.test') -StatePath $state
        $r.ExitCode | Should Be 0
        $r.Json.results[0].status | Should Be 'applied'
        $r.LogText | Should Match "server='dc01.stub.test'"
    }

    It 'refuses to put a new account into a protected administrative group: the item fails and nothing is created' {
        $groups = @(
            @{ dn = "CN=GG-AllStaff,OU=Groups,$script:Root"; sam = 'GG-AllStaff' },
            @{ dn = "CN=Ops Admins,OU=Groups,$script:Root"; sam = 'Ops Admins'; adminCount = 1 }
        )
        $state = New-StubStateFile -Groups $groups -Ous @("OU=Staff,$script:Root", "OU=Groups,$script:Root")
        $r = Invoke-IDLScript -Script 'New-IDLUser' -Request (New-Request @(New-Item1 -Groups @('GG-AllStaff', 'Ops Admins'))) -StatePath $state

        $r.ExitCode | Should Be 1
        $r.Json.results[0].status | Should Be 'failed'
        $r.Json.results[0].error | Should Match 'refusing to add Ops Admins: it is a protected administrative group \(adminCount=1\)'
        (Get-StubCalls -Path $state).Count | Should Be 0
        ($r.StdOut + $r.StdErr + $r.LogText + $r.ReportText) | Should Not Match ([regex]::Escape($secret))
    }
}
