<#
.SYNOPSIS
    Adds Entra users to, and removes them from, cloud groups for IDLifecycle: security and Microsoft 365 groups through
    Microsoft Graph, mail-enabled security groups and distribution lists through Exchange Online.

.DESCRIPTION
    Reads ONE JSON request from stdin and writes ONE JSON response to stdout.

    Request:
      {"cloud":{...see IDLifecycle.Cloud.psm1...}, "items":[
         {"changeId":"c1","externalId":"E1001","userId":"<Entra user object id>",
          "add":["<groupId>"],"remove":["<groupId>"],"removeAll":false,"keep":["<groupId>"]} ]}

    Per item the script reads the user and every named group LIVE (never trusting the plan), then:
      * add: makes the user a DIRECT member of each group it is not yet a direct member of,
      * remove: removes each direct membership it has,
      * removeAll (a disable that strips groups): removes the user from every group it is a direct member of
        (found through transitiveMemberOf, then checked one by one) except the "keep" and "add" groups; memberships
        that are only inherited through nesting are left alone, and so are the groups below.
    Groups that may not be changed are refused - the group is reported as failed (nothing changed for it), the other
    groups of the item still run, and the item ends "failed": synced from AD (change the AD group instead),
    role-assignable (protected, like AD admin groups), dynamic membership, unverifiable properties, unknown kinds, and
    mail-enabled security / distribution groups while Exchange Online is not enabled. With removeAll such groups are
    simply left alone and named in the message.
    Security and Microsoft 365 groups are changed with Graph (POST/DELETE /groups/{id}/members/$ref); mail-enabled
    security groups and distribution lists with Exchange Online (Add-/Remove-DistributionGroupMember, the user must be
    a mail recipient there). Already a member / not a member is fine (idempotent); a group that no longer exists is a
    failure for "add" and nothing to do for "remove".

    Response: {"ok":true,"dryRun":false,"results":[{"changeId","externalId","status":"applied|would|skipped|failed",
    "message","objectGuid":null,"dn":null,"sam":null,"upn","error","before"}],"summary":{"total","applied","would","skipped","failed"}}
    "before" (undo): {"cloudAdded":[group ids actually added],"cloudRemoved":[group ids actually removed]} on applied and would
    items, and on a failed item that changed some groups before failing (no-ops are never listed).

.PARAMETER DryRun
    Do every lookup (Graph and Exchange Online reads) and log/report everything as WOULD: statements, but make NO
    changes. Items that would change come back with status "would".

.EXAMPLE
    Get-Content .\cloud-groups.json -Raw | pwsh -NoProfile -File .\Set-IDLCloudGroups.ps1 -DryRun | ConvertFrom-Json

.NOTES
    Requires PowerShell 7+, an Entra app registration with the application permissions GroupMember.ReadWrite.All,
    Group.Read.All and User.Read.All, and - for mail-enabled security groups and distribution lists - the
    ExchangeOnlineManagement module (3.x), Exchange.ManageAsApp and an Exchange role that includes Distribution Groups.
    Exit codes: 0 ok, 1 some items failed, 2 invalid input, 3 certificate missing, 4 sign-in failed / Graph
    unreachable, 5 unexpected error. Logs and the CSV report (Set-IDLCloudGroups_Report_<ts>.csv) go to $PSScriptRoot\Logs.
#>

#Requires -Version 7.0

[CmdletBinding()]
param(
    [switch]$DryRun
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

Import-Module (Join-Path $PSScriptRoot 'IDLifecycle.Common.psm1') -Force
Import-Module (Join-Path $PSScriptRoot 'IDLifecycle.Cloud.psm1') -Force

$exitCode = 5
$response = $null
$groupCache = @{}
$exchangeError = $null

function Get-CloudGroupLive {
    # The group as Graph sees it now ($null when it does not exist); cached for this run.
    param([string]$GroupId)

    if ($groupCache.ContainsKey($GroupId)){
        return $groupCache[$GroupId]
    }

    try{
        $g = Invoke-IDLGraph -Config $config -Path "/groups/$($GroupId)?`$select=$(Get-IDLGraphGroupSelect)"
    }
    catch{
        if ((Get-IDLGraphErrorStatus -Exception $_.Exception) -ne 404){
            throw
        }

        $g = $null
    }

    $groupCache[$GroupId] = $g
    return $g
}

function Test-CloudDirectMember {
    # True when the user is a DIRECT member of the group (inherited membership through nesting does not count).
    param([string]$GroupId, [string]$UserId)

    $filter = [uri]::EscapeDataString("id eq '$UserId'")
    $page = Invoke-IDLGraph -Config $config -Path "/groups/$GroupId/members/microsoft.graph.user?`$filter=$filter&`$count=true&`$select=id" -Eventual
    $members = Get-IDLCloudList -Object $page -Name 'value'
    return ($members.Count -gt 0)
}

function Get-ExchangeRecipients {
    # Exchange Online recipients of the group and the user (connects on first use; a failed connection is remembered).
    param([string]$GroupId, [string]$UserId, [string]$Kind)

    if ($null -ne $script:exchangeError){
        throw $script:exchangeError
    }

    try{
        Connect-IDLExchange -Config $config
    }
    catch{
        $script:exchangeError = $_.Exception.Message
        throw $script:exchangeError
    }

    $groupRecipient = Find-IDLExchangeRecipient -ObjectId $GroupId
    if ($null -eq $groupRecipient){
        throw "the group was not found in Exchange Online"
    }

    $userRecipient = Find-IDLExchangeRecipient -ObjectId $UserId
    if ($null -eq $userRecipient){
        throw "the user is not a mail recipient in Exchange Online (no mailbox or mail user), so it cannot be a member of a $(($Kind -eq 'distribution') ? ('distribution list') : ('mail-enabled security group'))"
    }

    return @{ Group = $groupRecipient; User = $userRecipient }
}

function Invoke-CloudMembership {
    <#
      One membership change. Returns @{ Changed = bool; Text = '...' } or throws (the group fails).
      $Operation is 'add' or 'remove'.
    #>
    param([string]$Operation, [string]$GroupId, $Group, [string]$UserId, [string]$UserLabel)

    $info = Get-IDLCloudGroupKind -Group $Group -ExchangeEnabled $config.ExchangeEnabled
    $name = [string](Get-IDLField -Object $Group -Name 'displayName')
    $label = "'$name'"
    if (-not $info.Manageable){
        throw ("refusing to {0} {1}: {2}" -f $Operation, $label, (Get-IDLCloudReasonText -Reason $info.Reason))
    }

    $via = (($info.ViaExchange) ? ('Exchange Online') : ('Graph'))
    $isMember = Test-CloudDirectMember -GroupId $GroupId -UserId $UserId
    if (
        (($Operation -eq 'add') -and $isMember) -or
        (($Operation -eq 'remove') -and (-not $isMember))
    ){
        $state = (($Operation -eq 'add') ? ('already a member of') : ('not a direct member of'))
        Write-IDLLog "  $UserLabel is $state $label"
        return @{ Changed = $false; Text = '' }
    }

    $recipients = $null
    if ($info.ViaExchange){
        # Resolved in DryRun too: a user without a mailbox is reported before the real run.
        $recipients = Get-ExchangeRecipients -GroupId $GroupId -UserId $UserId -Kind $info.Kind
    }

    $verb = (($Operation -eq 'add') ? ('add to') : ('remove from'))
    $past = (($Operation -eq 'add') ? ('added to') : ('removed from'))
    $action = (($Operation -eq 'add') ? ('AddCloudGroupMember') : ('RemoveCloudGroupMember'))
    if ($DryRun){
        Write-IDLLog "  WOULD: $verb $label ($($info.Kind), via $via)"
        Add-IDLReportRow -Action $action -Result 'Would' -Identity $UserLabel -ObjectIdOrDN $GroupId -Details "$name ($($info.Kind), via $via)"
        return @{ Changed = $true; Text = "would $verb $name" }
    }

    if ($info.ViaExchange){
        try{
            if ($Operation -eq 'add'){
                Add-DistributionGroupMember -Identity ([string]$recipients.Group.Guid) -Member ([string]$recipients.User.Guid) -BypassSecurityGroupManagerCheck -Confirm:$false -ErrorAction Stop
            }
            else{
                Remove-DistributionGroupMember -Identity ([string]$recipients.Group.Guid) -Member ([string]$recipients.User.Guid) -BypassSecurityGroupManagerCheck -Confirm:$false -ErrorAction Stop
            }
        }
        catch{
            $text = $_.Exception.Message
            if (
                (($Operation -eq 'add') -and ($text -match 'already a member')) -or
                (($Operation -eq 'remove') -and ($text -match "isn't a member|is not a member"))
            ){
                Write-IDLLog "  Exchange Online: $text (nothing to do)"
                return @{ Changed = $false; Text = '' }
            }

            throw
        }
    }
    else{
        try{
            if ($Operation -eq 'add'){
                $body = @{ '@odata.id' = "$($config.Endpoints.Graph)/v1.0/directoryObjects/$UserId" }
                $null = Invoke-IDLGraph -Config $config -Method POST -Path "/groups/$GroupId/members/`$ref" -Body $body
            }
            else{
                $null = Invoke-IDLGraph -Config $config -Method DELETE -Path "/groups/$GroupId/members/$UserId/`$ref"
            }
        }
        catch{
            $status = Get-IDLGraphErrorStatus -Exception $_.Exception
            $text = Get-IDLGraphErrorText -Exception $_.Exception
            if (
                (($Operation -eq 'add') -and ($status -eq 400) -and ($text -match 'already exist')) -or
                (($Operation -eq 'remove') -and ($status -eq 404))
            ){
                Write-IDLLog "  Graph: $text (nothing to do)"
                return @{ Changed = $false; Text = '' }
            }

            throw
        }
    }

    Write-IDLLog "  $(($Operation -eq 'add') ? ('Added to') : ('Removed from')) $label ($($info.Kind), via $via)"
    Add-IDLReportRow -Action $action -Result 'Applied' -Identity $UserLabel -ObjectIdOrDN $GroupId -Details "$name ($($info.Kind), via $via)"
    return @{ Changed = $true; Text = "$past $name" }
}

try{
    Start-IDLRun -ScriptName 'Set-IDLCloudGroups' -ScriptRoot $PSScriptRoot -IsAction -DryRun:$DryRun -Parameters @{ DryRun = [bool]$DryRun }

    # ---- Phase: read and validate the request ----
    $request = Read-IDLRequest
    $config = Get-IDLCloudConfig -Request $request
    $rawItems = Get-IDLField -Object $request -Name 'items'
    if ($rawItems -isnot [System.Collections.IList]){
        throw (New-IDLException -Kind Input -Message 'items is missing or not an array.')
    }

    $items = @($rawItems)
    Add-IDLCounter -Name 'Total' -By $items.Count
    Write-IDLLog "Request: items=$($items.Count); $(Get-IDLCloudConfigText -Config $config)"

    # ---- Phase: sign in (a failure here fails the whole run, exit 3/4) ----
    $null = Get-IDLCloudToken -Config $config

    # ---- Phase: process items ----
    $phase = Start-IDLPhase -Name 'Process items'
    $results = [System.Collections.Generic.List[object]]::new()
    foreach ($item in $items){
        $changeId = ConvertTo-IDLString -Value (Get-IDLField -Object $item -Name 'changeId')
        $externalId = ConvertTo-IDLString -Value (Get-IDLField -Object $item -Name 'externalId')
        $upn = $null
        $done = [System.Collections.Generic.List[string]]::new()
        $failures = [System.Collections.Generic.List[string]]::new()
        $notes = [System.Collections.Generic.List[string]]::new()
        # What this item actually changed (or would, in DryRun): the undo record.
        $before = [ordered]@{
            cloudAdded   = [System.Collections.Generic.List[string]]::new()
            cloudRemoved = [System.Collections.Generic.List[string]]::new()
        }
        $result = $null

        try{
            if ($item -isnot [System.Collections.IDictionary]){
                throw 'item is not an object'
            }

            $userId = ConvertTo-IDLString -Value (Get-IDLField -Object $item -Name 'userId')
            if (-not (Test-IDLCloudGuid -Value $userId)){
                throw 'userId must be the Entra user object id (GUID).'
            }

            $userId = $userId.ToLowerInvariant()
            $add = Get-IDLCloudGuidList -Value (Get-IDLField -Object $item -Name 'add') -Name 'add'
            $remove = Get-IDLCloudGuidList -Value (Get-IDLField -Object $item -Name 'remove') -Name 'remove'
            $keep = Get-IDLCloudGuidList -Value (Get-IDLField -Object $item -Name 'keep') -Name 'keep'
            $removeAll = [bool](Get-IDLField -Object $item -Name 'removeAll' -Default $false)
            $both = @($add | Where-Object { $remove -contains $_ })
            if ($both.Count -gt 0){
                throw "group(s) both in add and remove: $($both -join ', ')"
            }

            try{
                $user = Invoke-IDLGraph -Config $config -Path "/users/$($userId)?`$select=id,userPrincipalName"
            }
            catch{
                if ((Get-IDLGraphErrorStatus -Exception $_.Exception) -eq 404){
                    throw "Entra user $userId was not found"
                }

                throw
            }

            $upn = [string](Get-IDLField -Object $user -Name 'userPrincipalName')
            $userLabel = (([string]::IsNullOrEmpty($upn)) ? ($userId) : ($upn))
            Write-IDLLog "Processing $changeId ($externalId) -> $userLabel ($userId): add=$($add.Count) remove=$($remove.Count) removeAll=$removeAll keep=$($keep.Count)"

            # Operations in order: adds, explicit removals, then (removeAll) every other direct membership.
            $ops = [System.Collections.Generic.List[object]]::new()
            foreach ($gid in $add){
                $ops.Add(@{ Operation = 'add'; GroupId = $gid; FromAll = $false })
            }

            foreach ($gid in $remove){
                $ops.Add(@{ Operation = 'remove'; GroupId = $gid; FromAll = $false })
            }

            if ($removeAll){
                $path = "/users/$userId/transitiveMemberOf/microsoft.graph.group?`$select=$(Get-IDLGraphGroupSelect)&`$top=999&`$count=true"
                foreach ($g in (Invoke-IDLGraph -Config $config -Path $path -All -Eventual)){
                    $gid = ([string](Get-IDLField -Object $g -Name 'id')).ToLowerInvariant()
                    if (
                        ($keep -contains $gid) -or
                        ($add -contains $gid) -or
                        ($remove -contains $gid)
                    ){
                        continue
                    }

                    $groupCache[$gid] = $g
                    $info = Get-IDLCloudGroupKind -Group $g -ExchangeEnabled $config.ExchangeEnabled
                    if (-not $info.Manageable){
                        $name = [string](Get-IDLField -Object $g -Name 'displayName')
                        Write-IDLLog "  Leaving '$name' alone: $(Get-IDLCloudReasonText -Reason $info.Reason)"
                        Add-IDLReportRow -Action 'RemoveCloudGroupMember' -Result 'Skipped' -Identity $userLabel -ObjectIdOrDN $gid -Details "$($name): $($info.Reason)"
                        $notes.Add("$name ($($info.Reason))")
                        continue
                    }

                    $ops.Add(@{ Operation = 'remove'; GroupId = $gid; FromAll = $true })
                }
            }

            foreach ($op in $ops){
                try{
                    $group = Get-CloudGroupLive -GroupId $op.GroupId
                    if ($null -eq $group){
                        if ($op.Operation -eq 'add'){
                            throw "group $($op.GroupId) was not found"
                        }

                        Write-IDLLog "  Group $($op.GroupId) not found; nothing to remove" 'WARN'
                        continue
                    }

                    $outcome = Invoke-CloudMembership -Operation $op.Operation -GroupId $op.GroupId -Group $group -UserId $userId -UserLabel $userLabel
                    if ($outcome.Changed){
                        $done.Add($outcome.Text)
                        if ($op.Operation -eq 'add'){
                            $before.cloudAdded.Add($op.GroupId)
                        }
                        else{
                            $before.cloudRemoved.Add($op.GroupId)
                        }
                    }
                }
                catch{
                    $text = Protect-IDLText -Text $_.Exception.Message
                    Write-IDLLog "  $($op.Operation) $($op.GroupId) failed: $text" 'ERROR'
                    Add-IDLReportRow -Action (($op.Operation -eq 'add') ? ('AddCloudGroupMember') : ('RemoveCloudGroupMember')) -Result 'Failed' -Identity $userLabel -ObjectIdOrDN $op.GroupId -ErrorText $text
                    $failures.Add("$($op.Operation) $($op.GroupId): $text")
                }
            }

            $tail = [System.Collections.Generic.List[string]]::new()
            if ($notes.Count -gt 0){
                $tail.Add('left alone: ' + ($notes -join ', '))
            }

            if ($failures.Count -gt 0){
                $message = ((@($done) + @("failed: $($failures.Count) group(s)") + $tail) -join '; ')
                $partial = ((($before.cloudAdded.Count + $before.cloudRemoved.Count) -gt 0) ? ($before) : ($null))
                $result = New-IDLResult -ChangeId $changeId -ExternalId $externalId -Status failed -Message $message -Upn $upn -ErrorText ($failures -join '; ') -Before $partial
                Add-IDLReportRow -Action 'SetCloudGroups' -Result 'Failed' -Identity $userLabel -ObjectIdOrDN $userId -Details $message -ErrorText ($failures -join '; ')
            }
            elseif ($done.Count -eq 0){
                $message = ((@('nothing to do') + $tail) -join '; ')
                $result = New-IDLResult -ChangeId $changeId -ExternalId $externalId -Status skipped -Message $message -Upn $upn
                Add-IDLReportRow -Action 'SetCloudGroups' -Result 'Skipped' -Identity $userLabel -ObjectIdOrDN $userId -Details 'already in desired state'
            }
            else{
                $status = (($DryRun) ? ('would') : ('applied'))
                $result = New-IDLResult -ChangeId $changeId -ExternalId $externalId -Status $status -Message ((@($done) + $tail) -join '; ') -Upn $upn -Before $before
            }
        }
        catch{
            $errorText = Protect-IDLText -Text $_.Exception.Message
            Write-IDLLog "  Item $changeId failed: $errorText" 'ERROR'
            Write-IDLLog "  Stack: $($_.ScriptStackTrace)" 'DEBUG'
            $message = (($done.Count -gt 0) ? ('failed after: ' + ($done -join '; ')) : ('failed'))
            $result = New-IDLResult -ChangeId $changeId -ExternalId $externalId -Status failed -Message $message -Upn $upn -ErrorText $errorText
            Add-IDLReportRow -Action 'SetCloudGroups' -Result 'Failed' -Identity $externalId -Details $message -ErrorText $errorText
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
    Disconnect-IDLExchange
    Stop-IDLRun -ExitCode $exitCode
}

Write-IDLResponse -Response $response
exit $exitCode
