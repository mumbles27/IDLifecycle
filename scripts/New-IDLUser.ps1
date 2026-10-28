<#
.SYNOPSIS
    Creates AD user accounts for IDLifecycle (picks a free sAMAccountName/UPN/CN candidate, sets groups).

.DESCRIPTION
    Reads ONE JSON request from stdin and writes ONE JSON response to stdout.

    Request:
      {"server":"", "idAttribute":"employeeID", "items":[
         {"changeId":"c1","externalId":"E1001","ou":"OU=Staff,DC=corp,DC=example,DC=com",
          "candidates":[{"sam":"jdoe","upn":"jdoe@corp.example.com","cn":"Jane Doe","mail":"jane.doe@corp.example.com"},
                        {"sam":"jdoe2","upn":"jdoe2@corp.example.com","cn":"Jane Doe (2)","mail":"jane.doe2@corp.example.com"}],
          "attributes":{"givenName":"Jane","sn":"Doe","displayName":"Jane Doe","title":"Analyst"},
          "groups":["GG-AllStaff"],"password":"<generated>","enabled":true,"changePasswordAtLogon":true} ]}

    Per item:
      * Idempotent: if an account whose idAttribute equals externalId already exists the item is "skipped"
        ("exists") and its guid/dn/sam/upn are returned; nothing is changed.
      * Otherwise the FIRST candidate whose sAMAccountName AND userPrincipalName are free directory-wide, whose
        cn is free in the target OU and whose optional "mail" is not used by any object (as mail or as an smtp:
        proxyAddresses entry, e.g. on a contact) is used. Existence is checked with a handful of bulk LDAP queries,
        and candidates taken earlier in the same batch count as taken. A candidate's "mail" is set as the account's
        mail attribute (replacing a "mail" in attributes).
      * The user is created with New-ADUser (attributes via -OtherAttributes, idAttribute = externalId, password as
        a SecureString), then added to the groups (sAMAccountName or DN). If a group cannot be resolved the account
        is KEPT and the item is reported "failed" with the missing groups listed (guid/dn still returned).
    One failing item never stops the others. The password is never logged or echoed (it is also scrubbed from errors).

    Response: {"ok":true,"dryRun":false,"results":[{"changeId","externalId","status":"applied|would|skipped|failed",
    "message","objectGuid","dn","sam","upn","error"}],"summary":{"total","applied","would","skipped","failed"}}

.PARAMETER DryRun
    Do every lookup and candidate selection and log/report WOULD: statements, but create nothing. Successful
    items come back with status "would" (objectGuid is null because nothing exists yet).

.EXAMPLE
    Get-Content .\new-request.json -Raw | pwsh -NoProfile -File .\New-IDLUser.ps1 -DryRun

.EXAMPLE
    Get-Content .\new-request.json -Raw | pwsh -NoProfile -File .\New-IDLUser.ps1 | ConvertFrom-Json

.NOTES
    Requires PowerShell 7+, the ActiveDirectory module, and delegated rights to create users in the target OUs and
    add group members. Exit codes: 0 ok, 1 some items failed, 2 invalid input, 3 module missing, 4 cannot reach AD,
    5 unexpected error. Logs and the CSV report (New-IDLUser_Report_<ts>.csv) are written to $PSScriptRoot\Logs.
    sAMAccountName candidates longer than 20 characters are skipped (the Go side truncates before adding a number).
    Attributes sAMAccountName/userPrincipalName/cn/name inside "attributes" are ignored in favour of the candidate.
#>

#Requires -Version 7.0

[CmdletBinding()]
param(
    [switch]$DryRun
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

Import-Module (Join-Path $PSScriptRoot 'IDLifecycle.Common.psm1') -Force

$exitCode = 5
$response = $null

try{
    Start-IDLRun -ScriptName 'New-IDLUser' -ScriptRoot $PSScriptRoot -IsAction -DryRun:$DryRun -Parameters @{ DryRun = [bool]$DryRun }

    # ---- Phase: read and validate the request ----
    $request = Read-IDLRequest
    $common = Get-IDLRequestCommon -Request $request -RequireItems
    $server = $common.Server
    $idAttribute = $common.IdAttribute
    $items = $common.Items
    Add-IDLCounter -Name 'Total' -By $items.Count
    Write-IDLLog ("Request: server='{0}' idAttribute={1} items={2}" -f $server, $idAttribute, $items.Count)

    $ignoredAttributes = @('samaccountname', 'userprincipalname', 'cn', 'name')

    # Per-item parsing: bad items are marked, never fatal for the batch.
    $parsed = New-Object 'object[]' $items.Count
    for ($i = 0; $i -lt $items.Count; $i++){
        $item = $items[$i]
        $p = @{
            ChangeId              = ConvertTo-IDLString -Value (Get-IDLField -Object $item -Name 'changeId')
            ExternalId            = (ConvertTo-IDLString -Value (Get-IDLField -Object $item -Name 'externalId'))
            Error                 = $null
            Ou                    = ''
            Candidates            = [System.Collections.Generic.List[object]]::new()
            Attributes            = [ordered]@{}
            Groups                = [string[]]@()
            Password              = $null
            Enabled               = $true
            ChangePasswordAtLogon = $true
        }

        try{
            if ($item -isnot [System.Collections.IDictionary]){
                throw 'item is not a JSON object'
            }

            if ([string]::IsNullOrWhiteSpace($p.ExternalId)){
                throw 'externalId is required'
            }

            $p.ExternalId = $p.ExternalId.Trim()

            $ou = ConvertTo-IDLString -Value (Get-IDLField -Object $item -Name 'ou')
            if (
                ([string]::IsNullOrWhiteSpace($ou)) -or
                (-not (Test-IDLDnShape -Value $ou.Trim()))
            ){
                throw 'ou is required and must be a well-formed distinguished name'
            }

            $p.Ou = $ou.Trim()

            $rawCandidates = Get-IDLField -Object $item -Name 'candidates'
            if (
                ($rawCandidates -isnot [System.Collections.IList]) -or
                ($rawCandidates.Count -eq 0)
            ){
                throw 'candidates must be a non-empty array'
            }

            foreach ($candidate in $rawCandidates){
                $sam = ConvertTo-IDLString -Value (Get-IDLField -Object $candidate -Name 'sam')
                $upn = ConvertTo-IDLString -Value (Get-IDLField -Object $candidate -Name 'upn')
                $cn = ConvertTo-IDLString -Value (Get-IDLField -Object $candidate -Name 'cn')
                if (
                    ([string]::IsNullOrWhiteSpace($sam)) -or
                    ([string]::IsNullOrWhiteSpace($upn)) -or
                    ([string]::IsNullOrWhiteSpace($cn))
                ){
                    throw 'every candidate needs non-empty sam, upn and cn'
                }

                # Optional: the mail address that goes with this candidate (naming.mail_template on the Go side).
                $mail = ([string](ConvertTo-IDLString -Value (Get-IDLField -Object $candidate -Name 'mail'))).Trim()
                if (
                    ($mail) -and
                    ($mail -notmatch '^[^@\s]+@[^@\s]+\.[^@\s]+$')
                ){
                    throw "candidate mail '$mail' is not an e-mail address"
                }

                $p.Candidates.Add(@{ Sam = $sam.Trim(); Upn = $upn.Trim(); Cn = $cn.Trim(); Mail = $mail })
            }

            $attributes = Get-IDLField -Object $item -Name 'attributes'
            if (
                ($null -ne $attributes) -and
                ($attributes -isnot [System.Collections.IDictionary])
            ){
                throw "'attributes' must be a JSON object"
            }

            if ($null -ne $attributes){
                foreach ($key in $attributes.Keys){
                    $name = [string]$key
                    if (-not (Test-IDLAttributeName -Name $name)){
                        throw "invalid attribute name: $name"
                    }

                    if ($name.ToLowerInvariant() -in $ignoredAttributes){
                        Write-IDLLog "  Item $($p.ChangeId): attribute '$name' ignored (candidate values are used)" 'WARN'
                        continue
                    }

                    if (Test-IDLProtectedAttribute -Name $name){
                        throw "attribute '$name' is protected and cannot be set by New-IDLUser"
                    }

                    $text = ConvertTo-IDLString -Value $attributes[$key]
                    if ([string]::IsNullOrEmpty($text)){
                        continue
                    }

                    if ($name -ieq $idAttribute){
                        if ($text -ine $p.ExternalId){
                            Write-IDLLog "  Item $($p.ChangeId): attribute '$name' differs from externalId; externalId wins" 'WARN'
                        }

                        continue
                    }

                    $p.Attributes[$name] = $text
                }
            }

            $p.Groups = Get-IDLStringList -Value (Get-IDLField -Object $item -Name 'groups')

            $enabledRaw = Get-IDLField -Object $item -Name 'enabled'
            if ($null -ne $enabledRaw){
                $p.Enabled = [bool]$enabledRaw
            }

            $changeRaw = Get-IDLField -Object $item -Name 'changePasswordAtLogon'
            if ($null -ne $changeRaw){
                $p.ChangePasswordAtLogon = [bool]$changeRaw
            }

            $p.Password = ConvertTo-IDLString -Value (Get-IDLField -Object $item -Name 'password')
            if (
                (-not [string]::IsNullOrEmpty($p.Password)) -and
                ($p.Password.Length -lt 8)
            ){
                throw 'password is too short (minimum 8 characters)'
            }

            if (
                $p.Enabled -and
                [string]::IsNullOrEmpty($p.Password)
            ){
                throw 'password is required when enabled is true'
            }
        }
        catch{
            $p.Error = $_.Exception.Message
        }

        $parsed[$i] = $p
    }

    # ---- Phase: dependencies and connection ----
    try{
        Import-Module ActiveDirectory -Global -ErrorAction Stop
        if (-not (Get-Module -Name ActiveDirectory)){
            throw 'ActiveDirectory module did not load.'
        }
    }
    catch{
        throw (New-IDLException -Kind Dependency -Message "ActiveDirectory module could not be loaded: $($_.Exception.Message)" -InnerException $_.Exception)
    }

    Write-IDLLog "ActiveDirectory module loaded (version $((Get-Module ActiveDirectory).Version))"
    Test-IDLAdConnection -Server $server
    $serverParams = Get-IDLServerParams -Server $server

    # ---- Phase: bulk existence checks ----
    $phase = Start-IDLPhase -Name 'Bulk lookups'
    $valid = @($parsed | Where-Object { $null -eq $_.Error })

    # 1. accounts that already carry the externalId (idempotency)
    $existing = Find-IDLUsersByAttribute -Attribute $idAttribute -Values ([string[]]@($valid | ForEach-Object { $_.ExternalId } | Select-Object -Unique)) -Server $server -Properties @($idAttribute, 'userPrincipalName')

    # 2. candidate sAMAccountName / UPN / mail already taken anywhere in the directory (mail: as mail or smtp:
    #    proxyAddresses of any object, e.g. a contact)
    $takenSam = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::OrdinalIgnoreCase)
    $takenUpn = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::OrdinalIgnoreCase)
    $takenMail = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::OrdinalIgnoreCase)
    $names = [System.Collections.Generic.List[object]]::new()
    foreach ($p in $valid){
        if ($existing.ContainsKey($p.ExternalId)){
            continue
        }

        foreach ($candidate in $p.Candidates){
            $names.Add(@{ Field = 'sam'; Value = $candidate.Sam })
            $names.Add(@{ Field = 'upn'; Value = $candidate.Upn })
            if ($candidate.Mail){
                $names.Add(@{ Field = 'mail'; Value = $candidate.Mail })
            }
        }
    }

    $sets = @{ sam = $takenSam; upn = $takenUpn; mail = $takenMail }
    foreach ($entry in (Find-IDLTakenNames -Names $names.ToArray() -Server $server).Taken){
        $null = $sets[$entry.field].Add($entry.value)
    }

    # 3. cn collisions inside each target OU (OU existence is validated first)
    $takenCn = @{}
    $badOu = @{}
    $cnByOu = @{}
    foreach ($p in $valid){
        if ($existing.ContainsKey($p.ExternalId)){
            continue
        }

        if (-not $cnByOu.ContainsKey($p.Ou.ToLowerInvariant())){
            $cnByOu[$p.Ou.ToLowerInvariant()] = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::OrdinalIgnoreCase)
        }

        foreach ($candidate in $p.Candidates){
            $null = $cnByOu[$p.Ou.ToLowerInvariant()].Add($candidate.Cn)
        }
    }

    foreach ($ouKey in $cnByOu.Keys){
        $ou = @($valid | Where-Object { $_.Ou.ToLowerInvariant() -eq $ouKey } | Select-Object -First 1)[0].Ou
        $takenCn[$ouKey] = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::OrdinalIgnoreCase)

        # A problem with one OU (missing, referral, access denied...) fails only the items that target it.
        try{
            if (-not (Test-IDLContainerExists -Dn $ou -Server $server)){
                $badOu[$ouKey] = "Target OU does not exist: $ou"
                Write-IDLLog "Target OU does not exist: $ou" 'WARN'
                continue
            }

            $cnTerms = @($cnByOu[$ouKey] | ForEach-Object { '(cn={0})' -f (ConvertTo-IDLLdapFilterValue -Value $_) })
            foreach ($chunk in (Split-IDLChunk -InputArray $cnTerms -Size 40)){
                $filter = New-IDLOrFilter -Terms ([string[]]$chunk)
                foreach ($object in @(Get-ADObject -LDAPFilter $filter -SearchBase $ou -SearchScope OneLevel @serverParams)){
                    $null = $takenCn[$ouKey].Add([string]$object.Name)
                }
            }
        }
        catch{
            $badOu[$ouKey] = "Cannot verify target OU '$ou': $(Protect-IDLText -Text $_.Exception.Message)"
            Write-IDLLog $badOu[$ouKey] 'WARN'
        }
    }

    # 4. groups, all at once
    $allGroups = [System.Collections.Generic.List[string]]::new()
    foreach ($p in $valid){
        $allGroups.AddRange([string[]]$p.Groups)
    }

    $groupMap = Resolve-IDLGroups -Identities $allGroups.ToArray() -Server $server
    Stop-IDLPhase -Phase $phase

    # ---- Phase: process items ----
    $phase = Start-IDLPhase -Name 'Process items'
    $results = [System.Collections.Generic.List[object]]::new()
    $createdInBatch = @{}

    for ($i = 0; $i -lt $items.Count; $i++){
        $p = $parsed[$i]
        $changeId = $p.ChangeId
        $externalId = $p.ExternalId
        $result = $null
        $userFields = @{}
        $created = $false

        try{
            if ($null -ne $p.Error){
                throw $p.Error
            }

            Write-IDLLog "Processing $changeId ($externalId)"

            # Idempotency: already exists in AD, or created earlier in this very batch.
            if ($existing.ContainsKey($externalId)){
                $matchesFound = $existing[$externalId]
                if ($matchesFound.Count -gt 1){
                    Add-IDLCounter -Name 'Ambiguous'
                    throw "ambiguous: $($matchesFound.Count) accounts already have $idAttribute '$externalId'"
                }

                Add-IDLCounter -Name 'Resolved'
                $userFields = Get-IDLUserFields -User $matchesFound[0]
                $result = New-IDLResult -ChangeId $changeId -ExternalId $externalId -Status skipped -Message 'exists' @userFields
                Add-IDLReportRow -Action 'CreateUser' -Result 'Skipped' -Identity $userFields['Sam'] -ObjectIdOrDN $userFields['Dn'] -Details 'exists'
                $results.Add($result)
                Register-IDLResult -Result $result
                continue
            }

            if ($createdInBatch.ContainsKey($externalId.ToLowerInvariant())){
                $userFields = $createdInBatch[$externalId.ToLowerInvariant()]
                $result = New-IDLResult -ChangeId $changeId -ExternalId $externalId -Status skipped -Message 'exists' @userFields
                Add-IDLReportRow -Action 'CreateUser' -Result 'Skipped' -Identity $userFields['Sam'] -ObjectIdOrDN $userFields['Dn'] -Details 'duplicate externalId in this batch'
                $results.Add($result)
                Register-IDLResult -Result $result
                continue
            }

            Add-IDLCounter -Name 'NotFound'

            $ouKey = $p.Ou.ToLowerInvariant()
            if ($badOu.ContainsKey($ouKey)){
                throw $badOu[$ouKey]
            }

            # Ambiguous or privileged groups fail the item BEFORE the account is created.
            $groupInfo = Get-IDLGroupDns -Identities $p.Groups -GroupMap $groupMap -Adding

            # Pick the first free candidate.
            $chosen = $null
            foreach ($candidate in $p.Candidates){
                if ($candidate.Sam.Length -gt 20){
                    Write-IDLLog "  Candidate '$($candidate.Sam)' skipped: sAMAccountName longer than 20 characters" 'WARN'
                    continue
                }

                if (
                    ($takenSam.Contains($candidate.Sam)) -or
                    ($takenUpn.Contains($candidate.Upn)) -or
                    ($takenCn[$ouKey].Contains($candidate.Cn)) -or
                    (($candidate.Mail) -and ($takenMail.Contains($candidate.Mail)))
                ){
                    Write-IDLLog "  Candidate sam='$($candidate.Sam)' upn='$($candidate.Upn)' cn='$($candidate.Cn)'$((($candidate.Mail)) ? (" mail='$($candidate.Mail)'") : ('')) is taken"
                    continue
                }

                $chosen = $candidate
                break
            }

            if ($null -eq $chosen){
                throw "no free candidate among $($p.Candidates.Count) (sAMAccountName, UPN, CN and mail must all be free)"
            }

            Write-IDLLog "  Selected candidate sam='$($chosen.Sam)' upn='$($chosen.Upn)' cn='$($chosen.Cn)'$((($chosen.Mail)) ? (" mail='$($chosen.Mail)'") : (''))"

            $other = @{ $idAttribute = $externalId }
            foreach ($name in $p.Attributes.Keys){
                $other[$name] = $p.Attributes[$name]
            }

            if ($chosen.Mail){
                foreach ($name in @($other.Keys | Where-Object { $_ -ieq 'mail' })){
                    Write-IDLLog "  Item $($p.ChangeId): attribute 'mail' replaced by the candidate's mail address" 'WARN'
                    $null = $other.Remove($name)
                }

                $other['mail'] = $chosen.Mail
            }

            $plannedDn = 'CN={0},{1}' -f (ConvertTo-IDLDnEscapedValue -Value $chosen.Cn), $p.Ou
            $userForGroups = [pscustomobject]@{
                SamAccountName    = $chosen.Sam
                DistinguishedName = $plannedDn
                MemberOf          = @()
            }
            $done = [System.Collections.Generic.List[string]]::new()

            if ($DryRun){
                Write-IDLLog "  WOULD: create $plannedDn (sam=$($chosen.Sam), upn=$($chosen.Upn), enabled=$($p.Enabled), attributes: $((@($other.Keys) | Sort-Object) -join ','))"
                Add-IDLReportRow -Action 'CreateUser' -Result 'Would' -Identity $chosen.Sam -ObjectIdOrDN $plannedDn -Details "upn=$($chosen.Upn); OU=$($p.Ou)"
                $userFields = @{ ObjectGuid = $null; Dn = $plannedDn; Sam = $chosen.Sam; Upn = $chosen.Upn }
                $done.Add('would create account')
            }
            else{
                $newParams = @{
                    Name                  = $chosen.Cn
                    SamAccountName        = $chosen.Sam
                    UserPrincipalName     = $chosen.Upn
                    Path                  = $p.Ou
                    Enabled               = $p.Enabled
                    ChangePasswordAtLogon = $p.ChangePasswordAtLogon
                    OtherAttributes       = $other
                    PassThru              = $true
                }

                if (-not [string]::IsNullOrEmpty($p.Password)){
                    $newParams['AccountPassword'] = ConvertTo-SecureString -String $p.Password -AsPlainText -Force
                }

                $newUser = New-ADUser @newParams @serverParams
                $created = $true
                $userFields = Get-IDLUserFields -User $newUser
                $userFields['Upn'] = $chosen.Upn
                $userForGroups = $newUser
                Write-IDLLog "  Created $($userFields['Dn']) (objectGuid $($userFields['ObjectGuid']))"
                Add-IDLReportRow -Action 'CreateUser' -Result 'Applied' -Identity $chosen.Sam -ObjectIdOrDN $userFields['Dn'] -Details "upn=$($chosen.Upn); OU=$($p.Ou)"
                $done.Add('created account')
            }

            # The candidate is now consumed (also in DryRun, so later items in the batch see it as taken).
            $null = $takenSam.Add($chosen.Sam)
            $null = $takenUpn.Add($chosen.Upn)
            $null = $takenCn[$ouKey].Add($chosen.Cn)
            if ($chosen.Mail){
                $null = $takenMail.Add($chosen.Mail)
            }

            $createdInBatch[$externalId.ToLowerInvariant()] = $userFields

            # Groups: add everything we can, then report the unresolvable ones.
            $addDns = $groupInfo.Dns
            $missing = $groupInfo.Missing

            if ($addDns.Count -gt 0){
                $null = Update-IDLGroupMembership -User $userForGroups -AddDns $addDns -Server $server -DryRun:$DryRun -Done $done
            }

            if ($missing.Count -gt 0){
                $verb = (($DryRun) ? ('would be created') : ('created'))
                $result = New-IDLResult -ChangeId $changeId -ExternalId $externalId -Status failed -Message "account $verb but group(s) not found: $($missing -join ', ')" -ErrorText "group(s) not found: $($missing -join ', ')" @userFields
                Add-IDLReportRow -Action 'CreateUser' -Result 'Failed' -Identity $chosen.Sam -ObjectIdOrDN $userFields['Dn'] -Details 'groups not found' -ErrorText "group(s) not found: $($missing -join ', ')"
            }
            elseif ($DryRun){
                $result = New-IDLResult -ChangeId $changeId -ExternalId $externalId -Status would -Message ($done -join '; ') @userFields
            }
            else{
                $result = New-IDLResult -ChangeId $changeId -ExternalId $externalId -Status applied -Message ($done -join '; ') @userFields
            }
        }
        catch{
            $errorText = Protect-IDLText -Text $_.Exception.Message
            Write-IDLLog "  Item $changeId failed: $errorText" 'ERROR'
            Write-IDLLog "  Stack: $($_.ScriptStackTrace)" 'DEBUG'
            $message = (($created) ? ('account created but a later step failed') : ('failed'))
            $result = New-IDLResult -ChangeId $changeId -ExternalId $externalId -Status failed -Message $message -ErrorText $errorText @userFields
            $dnForReport = (($userFields.ContainsKey('Dn')) ? ([string]$userFields['Dn']) : (''))
            Add-IDLReportRow -Action 'CreateUser' -Result 'Failed' -Identity $externalId -ObjectIdOrDN $dnForReport -Details $message -ErrorText $errorText
        }

        $results.Add($result)
        Register-IDLResult -Result $result
    }

    Stop-IDLPhase -Phase $phase

    $response = New-IDLActionResponse -Results $results.ToArray() -DryRun:$DryRun
    $exitCode = (($response.summary.failed -gt 0) ? (1) : (0))
}
catch{
    $exitCode = Get-IDLExitCode -Exception $_.Exception
    $message = Protect-IDLText -Text $_.Exception.Message
    Write-IDLLog "FATAL: $message" 'ERROR'
    Write-IDLLog "Stack: $($_.ScriptStackTrace)" 'DEBUG'
    $response = New-IDLErrorResponse -Message $message
}
finally{
    Stop-IDLRun -ExitCode $exitCode
}

Write-IDLResponse -Response $response
exit $exitCode
