$here = Split-Path -Parent $MyInvocation.MyCommand.Path
. (Join-Path $here 'TestHelpers.ps1')

Describe 'Get-IDLOUs.ps1' {

    $ous = @("OU=Staff,$script:Root", "OU=Finance,OU=Staff,$script:Root", "OU=Payroll,OU=Finance,OU=Staff,$script:Root", "OU=Groups,$script:Root", "CN=Builtin,$script:Root")

    It 'lists the OUs below the search bases (each once, containers left out) with their names' {
        $state = New-StubStateFile -Ous $ous
        $r = Invoke-IDLScript -Script 'Get-IDLOUs' -Request @{ server = ''; searchBases = @("OU=Staff,$script:Root", "OU=Finance,OU=Staff,$script:Root") } -StatePath $state

        $r.ExitCode | Should Be 0
        $r.SingleJson | Should Be $true
        $r.Json.ok | Should Be $true
        $dns = @($r.Json.ous | ForEach-Object { $_.dn })
        $dns -contains "OU=Payroll,OU=Finance,OU=Staff,$script:Root" | Should Be $true
        $dns -contains "OU=Groups,$script:Root" | Should Be $false
        @($dns | Where-Object { $_ -eq "OU=Payroll,OU=Finance,OU=Staff,$script:Root" }).Count | Should Be 1
        @($r.Json.ous | Where-Object { $_.dn -eq "OU=Finance,OU=Staff,$script:Root" })[0].name | Should Be 'Finance'
        (Get-StubCalls -Path $state).Count | Should Be 0
    }

    It 'lists the whole domain without search bases; refuses a base that is not a DN (exit 2); AD down is exit 4' {
        $state = New-StubStateFile -Ous $ous
        $r = Invoke-IDLScript -Script 'Get-IDLOUs' -Request @{ searchBases = @() } -StatePath $state
        $r.ExitCode | Should Be 0
        @($r.Json.ous).Count | Should Be 4
        @($r.Json.ous | ForEach-Object { $_.dn }) -contains "CN=Builtin,$script:Root" | Should Be $false

        $r = Invoke-IDLScript -Script 'Get-IDLOUs' -Request @{ searchBases = @('Staff') } -StatePath $state
        $r.ExitCode | Should Be 2
        $r.Json.ok | Should Be $false

        $r = Invoke-IDLScript -Script 'Get-IDLOUs' -Request @{ searchBases = @() } -StatePath (New-StubStateFile -Ous $ous -FailConnect $true)
        $r.ExitCode | Should Be 4
    }
}
