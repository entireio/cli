# All network entry points are mocked: these tests must never fetch a release.
# Pester invokes these parameters through mock scriptblocks, which the analyzer
# does not follow when checking parameter use.
[Diagnostics.CodeAnalysis.SuppressMessageAttribute('PSReviewUnusedParameter', 'FixtureJson')]
[Diagnostics.CodeAnalysis.SuppressMessageAttribute('PSReviewUnusedParameter', 'Tag')]
param()

Describe 'Release origin metadata' {
    BeforeAll {
        . (Join-Path (Split-Path -Parent $PSScriptRoot) 'install.ps1')
    }

    BeforeEach {
        $script:savedBaseUrl = $env:ENTIRE_INSTALL_BASE_URL
        $env:ENTIRE_INSTALL_BASE_URL = $null
        Set-StrictMode -Version 2.0
        Mock Get-ReleaseOrigin { 'https://mirror.invalid'; $GitHubReleaseOrigin }
        Mock Invoke-RestMethod { throw 'offline' }
        Mock Invoke-WebRequest { throw 'unexpected download' }
        Mock Invoke-GitHubApi { '{"tag_name":"v1.2.3"}' | ConvertFrom-Json }
    }

    AfterEach {
        $env:ENTIRE_INSTALL_BASE_URL = $script:savedBaseUrl
    }

    It 'prefers tag over version and never consults GitHub on success' {
        Mock Invoke-RestMethod { '{"channel":"stable","tag":"v2.3.4","version":"9.9.9"}' | ConvertFrom-Json }
        Get-ReleaseVersion stable | Should -Be '2.3.4'
        Should -Invoke Invoke-GitHubApi -Times 0 -Exactly
        Should -Invoke Invoke-RestMethod -Times 1 -Exactly -ParameterFilter {
            $Uri -eq 'https://mirror.invalid/channels/stable.json' -and $TimeoutSec -eq 60 -and $null -eq $Headers
        }
    }

    It 'accepts a version-only pointer without a declared channel' {
        Mock Invoke-RestMethod { '{"version":"2.3.4"}' | ConvertFrom-Json }
        Get-ReleaseVersion stable | Should -Be '2.3.4'
        Should -Invoke Invoke-GitHubApi -Times 0 -Exactly
    }

    It 'falls back for unusable metadata: <FixtureJson>' -TestCases @(
        @{ FixtureJson = '{"tag":"v1.2"}' }
        @{ FixtureJson = '{"tag":"v1"}' }
        @{ FixtureJson = '{"tag":"v1.2.3+build"}' }
        @{ FixtureJson = '{"tag":"v1.2.3-rc.1"}' }
        @{ FixtureJson = '{"tag":"v1.2.3-nightly.1"}' }
        @{ FixtureJson = '{"tag":"bad","version":"1.2.3"}' }
        @{ FixtureJson = '{"tag":"v2.3.4","channel":"nightly"}' }
        @{ FixtureJson = '{"tag":"v2.3.4","channel":"Stable"}' }
        @{ FixtureJson = '{"tag":42}' }
        @{ FixtureJson = '{}' }
        @{ FixtureJson = 'null' }
        @{ FixtureJson = 'not json' }
    ) {
        param($FixtureJson)
        Mock Invoke-RestMethod { $FixtureJson | ConvertFrom-Json -ErrorAction Stop }
        Get-ReleaseVersion stable | Should -Be '1.2.3'
        Should -Invoke Invoke-GitHubApi -Times 1 -Exactly
    }

    It 'falls back when the mirror is unreachable' {
        Get-ReleaseVersion stable | Should -Be '1.2.3'
        Should -Invoke Invoke-RestMethod -Times 1 -Exactly
        Should -Invoke Invoke-GitHubApi -Times 1 -Exactly
    }

    It 'reports both failed origins instead of returning an empty version' {
        Mock Invoke-GitHubApi { throw 'GitHub offline' }
        { Get-ReleaseVersion stable } | Should -Throw '*mirror.invalid: offline*github.com*GitHub offline*'
    }

    It 'uses the literal nightly segment on both origins: <Tag>' -TestCases @(
        @{ Tag = 'v1.2.3-prenightly' }
        @{ Tag = 'v1.2.3-nightly' }
        @{ Tag = 'v1.2.3-NIGHTLY.1' }
        @{ Tag = 'v1.2-nightly.1' }
        @{ Tag = 'v1.2.3-nightly.1+build' }
    ) {
        param($Tag)
        Mock Invoke-RestMethod { [pscustomobject]@{ channel = 'nightly'; tag = $Tag } }
        # Pretty-printed list, as returned by the real API. A non-nightly tag
        # ahead of the good one must not mask the usable release behind it.
        Mock Invoke-GitHubApi {
            @"
[
  {
    "tag_name": "$Tag"
  },
  {
    "tag_name": "v2.3.4-nightly.1"
  }
]
"@ | ConvertFrom-Json
        }
        Get-ReleaseVersion nightly | Should -Be '2.3.4-nightly.1'
        Should -Invoke Invoke-GitHubApi -Times 1 -Exactly
    }

    It 'discovers nightly on the mirror without GitHub' {
        Mock Invoke-RestMethod { '{"channel":"nightly","tag":"v2.3.4-nightly.1"}' | ConvertFrom-Json }
        Get-ReleaseVersion nightly | Should -Be '2.3.4-nightly.1'
        Should -Invoke Invoke-GitHubApi -Times 0 -Exactly
    }

    It 'rejects invalid stable GitHub metadata: <Tag>' -TestCases @(
        @{ Tag = 'v1.2' }
        @{ Tag = 'v1.2.3-rc.1' }
        @{ Tag = 'v1.2.3-nightly.1' }
        @{ Tag = 'v1.2.3+build' }
        @{ Tag = 'v../../elsewhere' }
    ) {
        param($Tag)
        Mock Invoke-GitHubApi { [pscustomobject]@{ tag_name = $Tag } }
        { Get-ReleaseVersion stable } | Should -Throw '*Invalid release version*'
    }
}

Describe 'Origin selection' {
    BeforeAll {
        . (Join-Path (Split-Path -Parent $PSScriptRoot) 'install.ps1')
    }

    BeforeEach {
        $script:savedBaseUrl = $env:ENTIRE_INSTALL_BASE_URL
        $env:ENTIRE_INSTALL_BASE_URL = $null
    }

    AfterEach {
        $env:ENTIRE_INSTALL_BASE_URL = $script:savedBaseUrl
    }

    It 'orders Entire before GitHub by default' {
        $origins = @(Get-ReleaseOrigin)
        $origins | Should -HaveCount 2
        $origins[0] | Should -Be 'https://install.entire.com'
        $origins[1] | Should -Be 'https://github.com/entireio/cli/releases/download'
    }

    It 'treats even a GitHub-shaped override as an authoritative manifest origin' {
        $env:ENTIRE_INSTALL_BASE_URL = $GitHubReleaseOrigin
        Mock Invoke-RestMethod { '{"tag":"v2.3.4"}' | ConvertFrom-Json }
        Mock Invoke-GitHubApi { throw 'must not be reached' }
        Get-ReleaseVersion stable | Should -Be '2.3.4'
        Should -Invoke Invoke-RestMethod -Times 1 -Exactly -ParameterFilter {
            $Uri -eq "$GitHubReleaseOrigin/channels/stable.json"
        }
        Should -Invoke Invoke-GitHubApi -Times 0 -Exactly
    }

    It 'uses only the explicit mirror even when it fails' {
        $env:ENTIRE_INSTALL_BASE_URL = 'https://custom.invalid/'
        @(Get-ReleaseOrigin) | Should -HaveCount 1
        Get-ReleaseOrigin | Should -Be 'https://custom.invalid'
        Mock Invoke-RestMethod { throw 'offline' }
        Mock Invoke-GitHubApi { throw 'must not be reached' }
        { Get-ReleaseVersion stable } | Should -Throw '*custom.invalid: offline*'
        Should -Invoke Invoke-GitHubApi -Times 0 -Exactly
    }
}
