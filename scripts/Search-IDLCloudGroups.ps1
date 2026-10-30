<#
.SYNOPSIS
    Searches Microsoft Entra ID for groups (security, Microsoft 365, mail-enabled security, distribution lists), or
    looks groups up by id, for the cloud group picker in the IDLifecycle GUI.

.DESCRIPTION
    Report-only script (it changes nothing, so there is no -DryRun). Reads ONE JSON request document from
    stdin and writes ONE JSON response document to stdout.

    Request - search:
      {"cloud":{...see IDLifecycle.Cloud.psm1...}, "query":"fin", "max":25}
    Request - look up by id (names and kinds for chips; at most 50 ids):
      {"cloud":{...}, "ids":["<objectId>", ...]}

    query: 2-64 characters after removing "*" (no control characters). It is sent to Graph as
    $search="displayName:<query>" OR "mail:<query>" (Graph matches words by prefix). max: 1-100, default 25.

    Response - search (sorted by name, case-insensitive):
      {"ok":true, "groups":[{"id","name","description","mail","kind","manageable","reason"}], "truncated":false}
    Response - ids (in the requested order; groups that do not exist have found:false):
      {"ok":true, "groups":[{"id","found":true,"name","description","mail","kind","manageable","reason"}], "truncated":false}

    kind: security | m365 | mail_security | distribution | unknown. manageable:false with reason synced (mastered in
    AD - pick the AD group), role_assignable (protected), dynamic (rule-based), unverified (properties unreadable -
    grant Group.Read.All), exchange_off (mail_security / distribution while Exchange Online is not enabled),
    unsupported, not_found (ids mode).
    On failure the response is {"ok":false,"error":"..."} with a non-zero exit code.

.EXAMPLE
    Get-Content .\search.json -Raw | pwsh -NoProfile -File .\Search-IDLCloudGroups.ps1 | ConvertFrom-Json

.NOTES
    Requires PowerShell 7+ and an Entra app registration with the application permissions Group.Read.All and
    GroupMember.ReadWrite.All (admin consent). Exit codes: 0 ok, 2 invalid input, 3 certificate missing,
    4 sign-in failed / Graph unreachable / access denied, 5 unexpected error.
    Logs: $PSScriptRoot\Logs\Search-IDLCloudGroups_<yyyyMMdd_HHmmss>.log. Nothing is ever written to stdout except the response.
#>

#Requires -Version 7.0

[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

Import-Module (Join-Path $PSScriptRoot 'IDLifecycle.Common.psm1') -Force
Import-Module (Join-Path $PSScriptRoot 'IDLifecycle.Cloud.psm1') -Force

$exitCode = 5
$response = $null
$maxIds = 50

try{
    Start-IDLRun -ScriptName 'Search-IDLCloudGroups' -ScriptRoot $PSScriptRoot

    # ---- Phase: read and validate the request ----
    $request = Read-IDLRequest
    $config = Get-IDLCloudConfig -Request $request
    $select = Get-IDLGraphGroupSelect

    $rawIds = Get-IDLField -Object $request -Name 'ids'
    $rawQuery = Get-IDLField -Object $request -Name 'query'
    if (
        ($null -ne $rawIds) -and
        ($null -ne $rawQuery)
    ){
        throw (New-IDLException -Kind Input -Message 'Send either query or ids, not both.')
    }

    if ($null -ne $rawIds){
        if ($rawIds -isnot [System.Collections.IList]){
            throw (New-IDLException -Kind Input -Message 'ids must be an array of object ids.')
        }

        $ids = Get-IDLCloudGuidList -Value $rawIds -Name 'ids'
        if (
            ($ids.Count -eq 0) -or
            ($ids.Count -gt $maxIds)
        ){
            throw (New-IDLException -Kind Input -Message "ids must hold 1-$maxIds object ids.")
        }

        Write-IDLLog "Request: look up $($ids.Count) group(s); $(Get-IDLCloudConfigText -Config $config)"
        $phase = Start-IDLPhase -Name 'Look up groups'
        $groups = [System.Collections.Generic.List[object]]::new()
        foreach ($id in $ids){
            try{
                $g = Invoke-IDLGraph -Config $config -Path "/groups/$($id)?`$select=$select"
                $entry = ConvertTo-IDLCloudGroup -Group $g -ExchangeEnabled $config.ExchangeEnabled
                $out = [ordered]@{ id = $entry.id; found = $true }
                foreach ($key in @('name', 'description', 'mail', 'kind', 'manageable', 'reason')){
                    $out[$key] = $entry[$key]
                }

                $groups.Add($out)
            }
            catch{
                if ((Get-IDLGraphErrorStatus -Exception $_.Exception) -ne 404){
                    throw
                }

                Write-IDLLog "Group $id not found" 'WARN'
                $groups.Add([ordered]@{ id = $id; found = $false; name = ''; description = ''; mail = ''; kind = ''; manageable = $false; reason = 'not_found' })
            }
        }

        Add-IDLCounter -Name 'Total' -By $groups.Count
        Stop-IDLPhase -Phase $phase
        $response = [ordered]@{ ok = $true; groups = [object[]]$groups.ToArray(); truncated = $false }
        $exitCode = 0
    }
    else{
        $query = ConvertTo-IDLString -Value $rawQuery
        $query = (($null -eq $query) ? ('') : (($query -replace '\*', '').Trim()))
        if (
            ($query.Length -lt 2) -or
            ($query.Length -gt 64)
        ){
            throw (New-IDLException -Kind Input -Message 'query must be 2-64 characters (not counting "*").')
        }

        if ($query -match '[\x00-\x1F\x7F]'){
            throw (New-IDLException -Kind Input -Message 'query must not contain control characters.')
        }

        $max = 25
        $rawMax = Get-IDLField -Object $request -Name 'max'
        if ($null -ne $rawMax){
            $parsed = 0
            if (
                (-not [int]::TryParse([string]$rawMax, [ref]$parsed)) -or
                ($parsed -lt 1) -or
                ($parsed -gt 100)
            ){
                throw (New-IDLException -Kind Input -Message 'max must be a whole number from 1 to 100.')
            }

            $max = $parsed
        }

        Write-IDLLog "Request: query='$query' max=$max; $(Get-IDLCloudConfigText -Config $config)"

        # ---- Phase: search ----
        $phase = Start-IDLPhase -Name 'Search groups'
        # Inside a $search phrase only " and \ are special; both are escaped with a backslash.
        $phrase = $query.Replace('\', '\\').Replace('"', '\"')
        $search = '"displayName:{0}" OR "mail:{0}"' -f $phrase
        Write-IDLLog "Graph search: $search"
        $path = '/groups?$search={0}&$select={1}&$top={2}&$count=true' -f [uri]::EscapeDataString($search), $select, ($max + 1)
        $page = Invoke-IDLGraph -Config $config -Path $path -Eventual
        $found = Get-IDLCloudList -Object $page -Name 'value'
        $more = -not [string]::IsNullOrEmpty([string](Get-IDLField -Object $page -Name '@odata.nextLink'))

        $groups = @($found | ForEach-Object { ConvertTo-IDLCloudGroup -Group $_ -ExchangeEnabled $config.ExchangeEnabled })
        $sorted = @($groups | Sort-Object -Property @{ Expression = { ([string]$_.name).ToLowerInvariant() } }, @{ Expression = { [string]$_.id } })
        $truncated = $more -or ($sorted.Count -gt $max)
        $result = @($sorted | Select-Object -First $max)
        Add-IDLCounter -Name 'Total' -By $result.Count
        Write-IDLLog "$($found.Count) group(s) returned by Graph; returning $($result.Count)$((($truncated)) ? (' (truncated)') : (''))"
        Stop-IDLPhase -Phase $phase

        $response = [ordered]@{ ok = $true; groups = [object[]]$result; truncated = [bool]$truncated }
        $exitCode = 0
    }
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
