@{
    RootModule            = 'ActiveDirectory.psm1'
    ModuleVersion         = '99.0.0'
    GUID                  = '5b1f7c64-9d2e-4c0a-8a43-1d5e6f0a9c11'
    Author                = 'IDLifecycle tests'
    CompanyName           = 'IDLifecycle'
    Description           = 'FAKE ActiveDirectory module for IDLifecycle Pester tests. NEVER touches a real directory.'
    PowerShellVersion     = '5.1'
    FunctionsToExport     = @(
        'Get-ADRootDSE', 'Get-ADUser', 'Get-ADGroup', 'Get-ADObject',
        'New-ADUser', 'Set-ADUser', 'Disable-ADAccount', 'Enable-ADAccount', 'Remove-ADUser',
        'Move-ADObject', 'Add-ADGroupMember', 'Remove-ADGroupMember'
    )
    CmdletsToExport       = @()
    VariablesToExport     = @()
    AliasesToExport       = @()
}
