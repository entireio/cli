#Requires -Version 5.1

# Installs what scripts/test/run.ps1 needs, for the current user, once on a
# fresh machine. Safe to rerun: each step is skipped when already satisfied.
#
# Stock Windows PowerShell 5.1 ships PowerShellGet 1.0.0.1 without the NuGet
# package provider, and its bootstrap is an interactive prompt that -Force
# does not suppress, so a non-interactive Install-Module fails there until
# the provider is installed. pwsh ships a modern PowerShellGet and skips that
# step. GitHub's Windows runners preinstall both modules, so CI never runs this.
$ErrorActionPreference = 'Stop'

Write-Output "PowerShell $($PSVersionTable.PSVersion) ($($PSVersionTable.PSEdition))"

if ($PSVersionTable.PSEdition -eq 'Desktop') {
    $nuget = Get-PackageProvider -Name NuGet -ListAvailable -ErrorAction SilentlyContinue |
        Where-Object { $_.Version -ge [version]'2.8.5.201' } | Select-Object -First 1
    if ($null -ne $nuget) {
        Write-Output "NuGet provider $($nuget.Version) is already installed."
    }
    else {
        Write-Output 'Installing the NuGet package provider...'
        Install-PackageProvider -Name NuGet -MinimumVersion 2.8.5.201 -Scope CurrentUser -Force | Out-Null
    }
}

# Pester 5.2 is the floor run.ps1 imports; 6.x is also known to work, so
# there is no ceiling.
$pester = Get-Module -Name Pester -ListAvailable | Where-Object { $_.Version -ge [version]'5.2.0' } | Select-Object -First 1
if ($null -ne $pester) {
    Write-Output "Pester $($pester.Version) is already installed."
}
else {
    Write-Output 'Installing Pester...'
    Install-Module -Repository PSGallery -Scope CurrentUser -Force -Name Pester -MinimumVersion 5.2.0 -SkipPublisherCheck
}

$analyzer = Get-Module -Name PSScriptAnalyzer -ListAvailable | Where-Object { $_.Version -ge [version]'1.17.1' } | Select-Object -First 1
if ($null -ne $analyzer) {
    Write-Output "PSScriptAnalyzer $($analyzer.Version) is already installed."
}
else {
    Write-Output 'Installing PSScriptAnalyzer...'
    Install-Module -Repository PSGallery -Scope CurrentUser -Force -Name PSScriptAnalyzer -MinimumVersion 1.17.1 -SkipPublisherCheck
}

Write-Output 'Ready: run scripts/test/run.ps1 or `mise run test:ps1`.'
