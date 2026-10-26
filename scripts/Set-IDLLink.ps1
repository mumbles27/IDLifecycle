<#
.SYNOPSIS
    Links existing AD accounts to a (new) source identity for IDLifecycle: sets the ID attribute and optionally renames.

.DESCRIPTION
    Reads ONE JSON request from stdin and writes ONE JSON response to stdout.

    Request:
      {"server":"", "idAttribute":"employeeID", "items":[
         {"changeId":"c1","objectGuid":"<guid>","externalId":"E5678","expectedExternalId":"C-3F9A12",
          "rename":{"sam":"jdoe","upn":"jdoe@corp.example.com","cn":"Jane Doe","mail":"jane.doe@corp.example.com"}} ]}

    Used when a person who already has an account (a contractor entered by hand, a rehire, an account that existed
    before IDLifecycle) shows up with a new external id. Per item the script finds the account by objectGuid (all
    items in one bulk query) and validates everything BEFORE any change:
      * the account must not be privileged: adminCount=1, or a direct member of a group with adminCount=1 or a
        well-known privileged RID (512, 518, 519, 544, 548, 549, 550, 551) - such items always fail;
      * the account's idAttribute must equal expectedExternalId (case-insensitive; "" or null = must be unset),
        unless it already equals externalId (a retry: the link step is then skipped);
      * no OTHER object may already carry externalId in idAttribute (also not one linked earlier in this batch);
      * rename (optional, every field optional; only fields that differ are changed): sam 1-20 characters without
        " / \ [ ] : ; | = , + * ? < > @, upn local@domain, cn 1-64 characters, mail an address. New names must be
        free (sam/upn on any object, mail as mail or smtp: proxyAddresses on any object, cn in the account's OU);
        the account's own current names never block it, names taken earlier in this batch do.
    Then, in ONE Set-ADUser call: idAttribute = externalId, -SamAccountName, -UserPrincipalName, -EmailAddress and -
    when the account has proxyAddresses - the new address becomes the primary SMTP: entry and the old mail is kept as
    a secondary smtp: entry. A changed cn is applied last with Rename-ADObject. One failing item never stops the others.

    Response: {"ok":true,"dryRun":false,"results":[{"changeId","externalId","status":"applied|would|skipped|failed",
    "message","objectGuid","dn","sam","upn","error"}],"summary":{"total","applied","would","skipped","failed"}}
    "externalId" in a result is the NEW external id; dn/sam/upn are the values after the change (in DryRun: the
    values the change would produce). "skipped" = already linked and nothing to rename.

.PARAMETER DryRun
    Do every lookup and check and log/report WOULD: statements, but make NO changes. Successful items come back
    with status "would".

.EXAMPLE
    Get-Content .\link-request.json -Raw | pwsh -NoProfile -File .\Set-IDLLink.ps1 -DryRun

.NOTES
    Requires PowerShell 7+, the ActiveDirectory module, and delegated rights to write the ID attribute,
    sAMAccountName, userPrincipalName, mail and proxyAddresses and to rename the users.
    Exit codes: 0 ok, 1 some items failed, 2 invalid input, 3 module missing, 4 cannot reach AD, 5 unexpected error.
    Logs and the CSV report (Set-IDLLink_Report_<ts>.csv) are written to $PSScriptRoot\Logs.
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
    Start-IDLRun -ScriptName 'Set-IDLLink' -ScriptRoot $PSScriptRoot -IsAction -DryRun:$DryRun -Parameters @{ DryRun = [bool]$DryRun }

    # ---- Phase: read and validate the request ----
    $request = Read-IDLRequest
    $common = Get-IDLRequestCommon -Request $request -RequireItems
    $server = $common.Server
    $idAttribute = $common.IdAttribute
    $items = $common.Items
    Add-IDLCounter -Name 'Total' -By $items.Count
    Write-IDLLog ("Request: server='{0}' idAttribute={1} items={2}" -f $server, $idAttribute, $items.Count)

    if (
        (Test-IDLProtectedAttribute -Name $idAttribute) -or
        ($idAttribute -in @('mail', 'cn', 'name'))
    ){
        throw (New-IDLException -Kind Input -Message "idAttribute '$idAttribute' cannot be used as the link key.")
    }

    # Per-item parsing: bad items are marked, never fatal for the batch.
    $parsed = New-Object 'object[]' $items.Count
    for ($i = 0; $i -lt $items.Count; $i++){
        $item = $items[$i]
        $p = @{
            ChangeId   = ConvertTo-IDLString -Value (Get-IDLField -Object $item -Name 'changeId')
            ExternalId = ConvertTo-IDLString -Value (Get-IDLField -Object $item -Name 'externalId')
            Expected   = ''
            Guid       = [guid]::Empty
            Rename     = $null
            Error      = $null
        }

        try{
            if ($item -isnot [System.Collections.IDictionary]){
                throw 'item is not a JSON object'
            }

            if ([string]::IsNullOrWhiteSpace($p.ExternalId)){
                throw 'externalId is required'
            }

            $p.ExternalId = $p.ExternalId.Trim()
            if (($p.ExternalId.Length -gt 256) -or ($p.ExternalId -cmatch '[\x00-\x1f\x7f]')){
                throw 'externalId must be 1-256 printable characters'
            }

            $expected = ConvertTo-IDLString -Value (Get-IDLField -Object $item -Name 'expectedExternalId')
            $p.Expected = (($null -eq $expected) ? ('') : ($expected.Trim()))

            $guidText = ConvertTo-IDLString -Value (Get-IDLField -Object $item -Name 'objectGuid')
            $guid = [guid]::Empty
            if (
                ([string]::IsNullOrWhiteSpace($guidText)) -or
                (-not [guid]::TryParse($guidText.Trim(), [ref]$guid)) -or
                ($guid -eq [guid]::Empty)
            ){
                throw 'objectGuid is required and must be a valid non-empty GUID'
            }

            $p.Guid = $guid

            $rawRename = Get-IDLField -Object $item -Name 'rename'
            if ($null -ne $rawRename){
                if ($rawRename -isnot [System.Collections.IDictionary]){
                    throw "'rename' must be a JSON object or null"
                }

                $rename = @{}
                foreach ($field in @('sam', 'upn', 'cn', 'mail')){
                    $text = ConvertTo-IDLString -Value (Get-IDLField -Object $rawRename -Name $field)
                    $rename[$field] = (($null -eq $text) ? ('') : ($text.Trim()))
                }

                if (
                    ($rename.sam) -and
                    (
                        ($rename.sam.Length -gt 20) -or
                        ($rename.sam -match '["/\\\[\]:;|=,+*?<>@\s]') -or
                        ($rename.sam.EndsWith('.'))
                    )
                ){
                    throw "rename.sam '$($rename.sam)' is not a valid sAMAccountName (1-20 characters, no spaces or `" / \ [ ] : ; | = , + * ? < > @, not ending with a dot)"
                }

                if (($rename.upn) -and ($rename.upn -notmatch '^[^@\s]+@[^@\s]+$')){
                    throw "rename.upn '$($rename.upn)' is not a userPrincipalName (local@domain)"
                }

                if (($rename.cn) -and (($rename.cn.Length -gt 64) -or ($rename.cn -cmatch '[\x00-\x1f\x7f]'))){
                    throw 'rename.cn must be 1-64 printable characters'
                }

                if (($rename.mail) -and ($rename.mail -notmatch '^[^@\s]+@[^@\s]+\.[^@\s]+$')){
                    throw "rename.mail '$($rename.mail)' is not an e-mail address"
                }

                if (-not ($rename.sam -or $rename.upn -or $rename.cn -or $rename.mail)){
                    throw "'rename' has no names; send null to keep the current names"
                }

                $p.Rename = $rename
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

    # ---- Phase: bulk lookups ----
    $phase = Start-IDLPhase -Name 'Bulk lookups'
    $valid = @($parsed | Where-Object { $null -eq $_.Error })

    # 1. the accounts
    $byGuid = Find-IDLUsersByGuid -Guids ([guid[]]@($valid | ForEach-Object { $_.Guid })) -Server $server -Properties @($idAttribute, 'userPrincipalName', 'mail', 'proxyAddresses', 'adminCount', 'memberOf')

    # 1b. direct group memberships of those accounts, to refuse privileged accounts (defence in depth)
    $memberDns = [System.Collections.Generic.List[string]]::new()
    foreach ($user in $byGuid.Values){
        $memberDns.AddRange((Get-IDLAdValues -Object $user -Name 'memberOf'))
    }

    $memberGroups = Resolve-IDLGroups -Identities ([string[]]@($memberDns | Select-Object -Unique)) -Server $server

    # 2. objects that already carry one of the new external ids (any object class)
    $holders = [hashtable]::new([System.StringComparer]::OrdinalIgnoreCase)
    $extValues = [string[]]@($valid | ForEach-Object { $_.ExternalId } | Select-Object -Unique)
    if ($extValues.Count -gt 0){
        foreach ($chunk in (Split-IDLChunk -InputArray $extValues -Size 50)){
            $terms = [string[]]@($chunk | ForEach-Object { '({0}={1})' -f $idAttribute, (ConvertTo-IDLLdapFilterValue -Value $_) })
            foreach ($object in @(Get-ADObject -LDAPFilter (New-IDLOrFilter -Terms $terms) -Properties $idAttribute @serverParams)){
                $value = Get-IDLAdValue -Object $object -Name $idAttribute
                if ($null -eq $value){
                    continue
                }

                if (-not $holders.ContainsKey($value)){
                    $holders[$value] = [System.Collections.Generic.List[object]]::new()
                }

                $holders[$value].Add($object)
            }
        }
    }

    # 3. the new names
    $names = [System.Collections.Generic.List[object]]::new()
    foreach ($p in $valid){
        $user = $byGuid[$p.Guid.ToString()]
        if (($null -eq $p.Rename) -or ($null -eq $user)){
            continue
        }

        foreach ($field in @('sam', 'upn', 'mail')){
            if ($p.Rename[$field]){
                $names.Add(@{ Field = $field; Value = $p.Rename[$field]; Ou = '' })
            }
        }

        if ($p.Rename.cn){
            $names.Add(@{ Field = 'cn'; Value = $p.Rename.cn; Ou = (Get-IDLParentDn -Dn ([string]$user.DistinguishedName)) })
        }
    }

    $lookup = Find-IDLTakenNames -Names $names.ToArray() -Server $server
    Stop-IDLPhase -Phase $phase

    # ---- Phase: process items ----
    $phase = Start-IDLPhase -Name 'Process items'
    $results = [System.Collections.Generic.List[object]]::new()
    $usedExt = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::OrdinalIgnoreCase)
    $usedNames = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::OrdinalIgnoreCase)
    $labels = @{ sam = 'sAMAccountName'; upn = 'userPrincipalName'; mail = 'mail'; cn = 'cn' }

    for ($i = 0; $i -lt $items.Count; $i++){
        $p = $parsed[$i]
        $changeId = $p.ChangeId
        $externalId = $p.ExternalId
        $user = $null
        $userFields = @{}
        $done = [System.Collections.Generic.List[string]]::new()
        $result = $null

        try{
            if ($null -ne $p.Error){
                throw $p.Error
            }

            $guidKey = $p.Guid.ToString()
            if (-not $byGuid.ContainsKey($guidKey)){
                Add-IDLCounter -Name 'NotFound'
                throw "no account found with objectGuid $guidKey"
            }

            $user = $byGuid[$guidKey]
            Add-IDLCounter -Name 'Resolved'
            $userFields = Get-IDLUserFields -User $user
            $userDn = [string]$user.DistinguishedName
            $identity = [string]$user.SamAccountName
            Write-IDLLog "Processing $changeId ($externalId) -> $userDn"

            # ---- validate the item completely before any change ----
            # Privileged accounts are never linked or renamed (adminCount=1, or a direct member of a protected group).
            if ((Get-IDLAdValue -Object $user -Name 'adminCount') -eq '1'){
                throw "account $userDn is privileged (adminCount=1); privileged accounts are never linked or renamed"
            }

            foreach ($groupDn in (Get-IDLAdValues -Object $user -Name 'memberOf')){
                $entry = $memberGroups[$groupDn]
                if (($null -ne $entry) -and $entry.Privileged){
                    throw "account $userDn is a member of the privileged group $groupDn ($($entry.PrivilegedReason)); privileged accounts are never linked or renamed"
                }
            }

            $current = Get-IDLAdValue -Object $user -Name $idAttribute
            $alreadyLinked = ($null -ne $current) -and ($current -ieq $externalId)
            if (-not $alreadyLinked){
                $shown = (($null -eq $current) ? ('(unset)') : ("'$current'"))
                if (($p.Expected -eq '') -and ($null -ne $current)){
                    throw "$idAttribute mismatch: account $userDn has $idAttribute $shown but the change expects it to be unset"
                }

                if (($p.Expected -ne '') -and ($current -ine $p.Expected)){
                    throw "$idAttribute mismatch: account $userDn has $idAttribute $shown but the change expects '$($p.Expected)'"
                }
            }

            if ($holders.ContainsKey($externalId)){
                $others = @($holders[$externalId] | Where-Object { ([guid]$_.ObjectGUID).ToString() -ne $guidKey })
                if ($others.Count -gt 0){
                    throw "$idAttribute '$externalId' is already used by $($others[0].DistinguishedName)"
                }
            }

            if ($usedExt.Contains($externalId)){
                throw "$idAttribute '$externalId' was linked to another account earlier in this batch"
            }

            $currentNames = @{
                sam  = [string]$user.SamAccountName
                upn  = [string](Get-IDLAdValue -Object $user -Name 'userPrincipalName')
                mail = [string](Get-IDLAdValue -Object $user -Name 'mail')
                cn   = [string]$user.Name
            }
            $parent = Get-IDLParentDn -Dn $userDn
            $new = @{}
            if ($null -ne $p.Rename){
                foreach ($field in @('sam', 'upn', 'mail', 'cn')){
                    $wanted = $p.Rename[$field]
                    if ((-not $wanted) -or ($wanted -ceq $currentNames[$field])){
                        continue
                    }

                    $key = (($field -eq 'cn') ? ("cn|$($parent.ToLowerInvariant())|$wanted") : ("$field|$wanted"))
                    if ($usedNames.Contains($key)){
                        throw "rename: $($labels[$field]) '$wanted' is taken by another account earlier in this batch"
                    }

                    # Only the account itself may hold the name already (e.g. a change of case).
                    if ($wanted -ine $currentNames[$field]){
                        $holder = @($lookup.Taken | Where-Object { ($_.field -eq $field) -and ($_.value -ieq $wanted) -and ($_.objectGuid -ne $guidKey) } | Select-Object -First 1)
                        if ($holder.Count -gt 0){
                            throw "rename: $($labels[$field]) '$wanted' is used by $($holder[0].dn)"
                        }

                        if (($field -eq 'cn') -and ($lookup.MissingOus -contains $parent)){
                            throw "rename: cannot verify cn '$wanted' in $parent"
                        }
                    }

                    $new[$field] = $wanted
                }
            }

            if ($alreadyLinked -and ($new.Count -eq 0)){
                $result = New-IDLResult -ChangeId $changeId -ExternalId $externalId -Status skipped -Message 'already linked' @userFields
                Add-IDLReportRow -Action 'LinkUser' -Result 'Skipped' -Identity $identity -ObjectIdOrDN $userDn -Details 'already linked'
                $results.Add($result)
                Register-IDLResult -Result $result
                $null = $usedExt.Add($externalId)
                continue
            }

            # ---- one Set-ADUser call: link key, sam, upn, mail (+ proxyAddresses) ----
            $setParams = @{}
            $replace = @{}
            $steps = [System.Collections.Generic.List[string]]::new()
            if (-not $alreadyLinked){
                $replace[$idAttribute] = $externalId
                $steps.Add("set $idAttribute")
            }

            if ($new.ContainsKey('sam')){
                $setParams['SamAccountName'] = $new.sam
            }

            if ($new.ContainsKey('upn')){
                $setParams['UserPrincipalName'] = $new.upn
            }

            if ($new.ContainsKey('mail')){
                $setParams['EmailAddress'] = $new.mail
                $proxies = Get-IDLAdValues -Object $user -Name 'proxyAddresses'
                if ($proxies.Count -gt 0){
                    $oldMail = $currentNames.mail
                    $list = [System.Collections.Generic.List[string]]::new()
                    $list.Add('SMTP:' + $new.mail)
                    foreach ($proxy in $proxies){
                        if ($proxy -match '^(?i)smtp:(.+)$'){
                            $address = $Matches[1]
                            if (($address -ieq $new.mail) -or (($oldMail) -and ($address -ieq $oldMail))){
                                continue
                            }

                            # Any other primary address becomes a secondary one.
                            $list.Add('smtp:' + $address)
                            continue
                        }

                        $list.Add($proxy)
                    }

                    if ($oldMail){
                        $list.Add('smtp:' + $oldMail)
                    }

                    $replace['proxyAddresses'] = [string[]]$list.ToArray()
                }
            }

            if ($replace.Count -gt 0){
                $setParams['Replace'] = $replace
            }

            $renamed = @($new.Keys | Where-Object { $_ -ne 'cn' } | Sort-Object | ForEach-Object { "$($labels[$_]) '$($currentNames[$_])' -> '$($new[$_])'" })
            if ($renamed.Count -gt 0){
                $steps.Add('renamed ' + ($renamed -join ', '))
            }

            $finalSam = (($new.ContainsKey('sam')) ? ($new.sam) : ($currentNames.sam))
            $finalUpn = (($new.ContainsKey('upn')) ? ($new.upn) : ($currentNames.upn))
            $finalDn = $userDn

            if ($setParams.Count -gt 0){
                $detail = 'link: {0}; rename: {1}' -f (-not $alreadyLinked), ((@($new.Keys | Where-Object { $_ -ne 'cn' } | Sort-Object)) -join ',')
                if ($DryRun){
                    Write-IDLLog "  WOULD: update $identity ($detail)"
                    Add-IDLReportRow -Action 'LinkUser' -Result 'Would' -Identity $identity -ObjectIdOrDN $userDn -Details $detail
                    foreach ($step in $steps){
                        $done.Add("would $($step -replace '^renamed', 'rename')")
                    }
                }
                else{
                    Set-ADUser -Identity $p.Guid @setParams @serverParams
                    Write-IDLLog "  Updated $identity ($detail)"
                    Add-IDLReportRow -Action 'LinkUser' -Result 'Applied' -Identity $identity -ObjectIdOrDN $userDn -Details $detail
                    $done.AddRange($steps)
                }
            }

            # ---- cn last (the DN changes) ----
            if ($new.ContainsKey('cn')){
                $type = (Get-IDLLeafRdn -Dn $userDn) -replace '=.*$', ''
                $finalDn = '{0}={1},{2}' -f $type, (ConvertTo-IDLDnEscapedValue -Value $new.cn), $parent
                if ($DryRun){
                    Write-IDLLog "  WOULD: rename $userDn -> $finalDn"
                    Add-IDLReportRow -Action 'RenameObject' -Result 'Would' -Identity $finalSam -ObjectIdOrDN $userDn -Details "-> $finalDn"
                    $done.Add("would rename cn '$($currentNames.cn)' -> '$($new.cn)'")
                }
                else{
                    $renamedObject = Rename-ADObject -Identity $p.Guid -NewName $new.cn -PassThru @serverParams
                    if (($null -ne $renamedObject) -and ($renamedObject.PSObject.Properties['DistinguishedName'])){
                        $finalDn = [string]$renamedObject.DistinguishedName
                    }

                    Write-IDLLog "  Renamed $userDn -> $finalDn"
                    Add-IDLReportRow -Action 'RenameObject' -Result 'Applied' -Identity $finalSam -ObjectIdOrDN $userDn -Details "-> $finalDn"
                    $done.Add("renamed cn '$($currentNames.cn)' -> '$($new.cn)'")
                }
            }

            $userFields = @{ ObjectGuid = $guidKey; Dn = $finalDn; Sam = $finalSam; Upn = $finalUpn }
            $null = $usedExt.Add($externalId)
            foreach ($field in $new.Keys){
                $key = (($field -eq 'cn') ? ("cn|$($parent.ToLowerInvariant())|$($new[$field])") : ("$field|$($new[$field])"))
                $null = $usedNames.Add($key)
            }

            $status = (($DryRun) ? ('would') : ('applied'))
            $result = New-IDLResult -ChangeId $changeId -ExternalId $externalId -Status $status -Message ($done -join '; ') @userFields
        }
        catch{
            $errorText = Protect-IDLText -Text $_.Exception.Message
            Write-IDLLog "  Item $changeId failed: $errorText" 'ERROR'
            Write-IDLLog "  Stack: $($_.ScriptStackTrace)" 'DEBUG'
            $message = ((($done.Count -gt 0) -and (-not $DryRun)) ? ('failed after: ' + ($done -join '; ')) : ('failed'))
            $result = New-IDLResult -ChangeId $changeId -ExternalId $externalId -Status failed -Message $message -ErrorText $errorText @userFields
            $dnForReport = (($userFields.ContainsKey('Dn')) ? ([string]$userFields['Dn']) : (''))
            Add-IDLReportRow -Action 'LinkUser' -Result 'Failed' -Identity $externalId -ObjectIdOrDN $dnForReport -Details $message -ErrorText $errorText
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
