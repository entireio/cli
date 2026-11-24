# Fixture parameters are consumed by Pester mock scriptblocks.
[Diagnostics.CodeAnalysis.SuppressMessageAttribute('PSReviewUnusedParameter', 'Failure')]
[Diagnostics.CodeAnalysis.SuppressMessageAttribute('PSReviewUnusedParameter', 'Expected')]
param()

Describe 'Release archive fallback' {
    BeforeAll {
        . (Join-Path (Split-Path -Parent $PSScriptRoot) 'install.ps1')
    }

    BeforeEach {
        Set-StrictMode -Version 2.0
        Mock Get-ReleaseOrigin { 'https://primary.invalid'; 'https://fallback.invalid' }
        Mock Invoke-WebRequest { throw 'unexpected network call' }
        $script:requests = @()
        $script:archive = Join-Path $TestDrive 'entire_windows_amd64.zip'
        $script:checksums = Join-Path $TestDrive 'checksums.txt'
        Mock Save-RemoteFile {
            $script:requests += $Uri
            if ($Uri.EndsWith('/checksums.txt')) {
                $hash = (Get-FileHash -LiteralPath $script:archive -Algorithm SHA256).Hash
                Set-Content -LiteralPath $Destination -Value "$hash  entire_windows_amd64.zip"
            }
            else {
                Set-Content -LiteralPath $Destination -Value $Uri
            }
        }
    }

    It 'downloads and verifies both files from the primary' {
        Save-ReleaseArchive '1.2.3' 'entire_windows_amd64.zip' $script:archive $script:checksums
        $script:requests | Should -HaveCount 2
        $script:requests[0] | Should -Be 'https://primary.invalid/v1.2.3/entire_windows_amd64.zip'
        $script:requests[1] | Should -Be 'https://primary.invalid/v1.2.3/checksums.txt'
    }

    It 're-downloads both files on fallback after <Failure>' -TestCases @(
        @{ Failure = 'archive' }
        @{ Failure = 'checksums' }
        @{ Failure = 'missing entry' }
        @{ Failure = 'malformed entry' }
    ) {
        param($Failure)
        Mock Save-RemoteFile {
            $script:requests += $Uri
            if ($Uri.EndsWith('.zip')) {
                Set-Content -LiteralPath $Destination -Value 'primary archive (possibly partial)'
                if ($Failure -eq 'archive') { throw 'archive unavailable' }
            }
            elseif ($Failure -eq 'checksums') { throw 'checksums unavailable' }
            elseif ($Failure -eq 'missing entry') {
                Set-Content -LiteralPath $Destination -Value (('a' * 64) + '  unrelated.zip')
            }
            else {
                Set-Content -LiteralPath $Destination -Value 'not-a-hash  entire_windows_amd64.zip'
            }
        } -ParameterFilter { $Uri -like 'https://primary.invalid/*' }

        Save-ReleaseArchive '1.2.3' 'entire_windows_amd64.zip' $script:archive $script:checksums
        $script:requests[-2] | Should -Be 'https://fallback.invalid/v1.2.3/entire_windows_amd64.zip'
        $script:requests[-1] | Should -Be 'https://fallback.invalid/v1.2.3/checksums.txt'
        (Get-Content -LiteralPath $script:archive -Raw).Trim() | Should -Be $script:requests[-2]
    }

    It 'aborts on a mismatch without contacting the fallback' {
        Mock Save-RemoteFile {
            $script:requests += $Uri
            Set-Content -LiteralPath $Destination -Value (('0' * 64) + '  entire_windows_amd64.zip')
        } -ParameterFilter { $Uri -eq 'https://primary.invalid/v1.2.3/checksums.txt' }
        { Save-ReleaseArchive '1.2.3' 'entire_windows_amd64.zip' $script:archive $script:checksums } |
            Should -Throw '*Checksum verification failed*'
        $script:requests | Should -HaveCount 2
        Should -Invoke Save-RemoteFile -Times 0 -Exactly -ParameterFilter { $Uri -like 'https://fallback.invalid/*' }
    }

    It 'reports every origin when no complete release can be downloaded' {
        Mock Save-RemoteFile { throw 'offline' }
        { Save-ReleaseArchive '1.2.3' 'entire_windows_amd64.zip' $script:archive $script:checksums } |
            Should -Throw '*Could not download release*primary.invalid: offline*fallback.invalid: offline*'
    }
}

Describe 'Pinned versions and direct-install selection' {
    BeforeAll {
        . (Join-Path (Split-Path -Parent $PSScriptRoot) 'install.ps1')
    }

    BeforeEach {
        $script:savedBaseUrl = $env:ENTIRE_INSTALL_BASE_URL
        $env:ENTIRE_INSTALL_BASE_URL = $null
        Mock Assert-Prerequisite {}
        Mock Get-PlatformArchitecture { 'amd64' }
        Mock Install-EntireWithScoop { throw 'unexpected Scoop call' }
        Mock Get-ReleaseVersion { throw 'discovery reached' }
        Mock Save-ReleaseArchive { throw 'download reached' }
        Mock Invoke-RestMethod { throw 'unexpected network call' }
        Mock Invoke-WebRequest { throw 'unexpected network call' }
        function scoop {}
        Set-Variable -Name Version -Value ''
        Set-Variable -Name Channel -Value 'stable'
        Set-Variable -Name ChannelExplicit -Value $false
        Set-Variable -Name InstallDirExplicit -Value $false
        Set-Variable -Name InstallDir -Value (Join-Path $TestDrive 'bin')
    }

    AfterEach {
        $env:ENTIRE_INSTALL_BASE_URL = $script:savedBaseUrl
    }

    It 'rejects invalid pins before discovery, downloads or Scoop: <Pin>' -TestCases @(
        @{ Pin = '' }
        @{ Pin = 'v' }
        @{ Pin = '1' }
        @{ Pin = '1.2' }
        @{ Pin = '../elsewhere' }
        @{ Pin = '1.2.3+build' }
        @{ Pin = "1.2.3`n" }
    ) {
        param($Pin)
        Set-Variable -Name Version -Value $Pin
        Set-Variable -Name VersionExplicit -Value $true
        { Install-Entire } | Should -Throw '*Invalid release version*'
        Should -Invoke Get-ReleaseVersion -Times 0 -Exactly
        Should -Invoke Save-ReleaseArchive -Times 0 -Exactly
        Should -Invoke Install-EntireWithScoop -Times 0 -Exactly
    }

    It 'pins <Pin> without discovery or Scoop, independently of the default channel' -TestCases @(
        @{ Pin = 'v1.2.3'; Expected = '1.2.3' }
        @{ Pin = '1.2.3-nightly.1'; Expected = '1.2.3-nightly.1' }
        @{ Pin = '1.2.3-rc.1'; Expected = '1.2.3-rc.1' }
    ) {
        param($Pin, $Expected)
        Set-Variable -Name Version -Value $Pin
        { Install-Entire } | Should -Throw '*download reached*'
        Should -Invoke Save-ReleaseArchive -Times 1 -Exactly -ParameterFilter { $ReleaseVersion -eq $Expected }
        Should -Invoke Get-ReleaseVersion -Times 0 -Exactly
        Should -Invoke Install-EntireWithScoop -Times 0 -Exactly
    }

    It 'refuses a pin from a different explicitly requested channel' {
        Set-Variable -Name Version -Value '1.2.3-nightly.1'
        Set-Variable -Name ChannelExplicit -Value $true
        { Install-Entire } | Should -Throw '*does not belong to channel stable*'
        Should -Invoke Save-ReleaseArchive -Times 0 -Exactly
        Should -Invoke Install-EntireWithScoop -Times 0 -Exactly
    }

    It 'accepts an explicit matching channel in mixed case' {
        Set-Variable -Name Version -Value '1.2.3-nightly.1'
        Set-Variable -Name ChannelExplicit -Value $true
        Set-Variable -Name Channel -Value 'NIGHTLY'
        { Install-Entire } | Should -Throw '*download reached*'
        Should -Invoke Get-ReleaseVersion -Times 0 -Exactly
    }

    It 'uses the archive path when a mirror is explicit even with Scoop present' {
        $env:ENTIRE_INSTALL_BASE_URL = 'https://custom.invalid'
        { Install-Entire } | Should -Throw '*discovery reached*'
        Should -Invoke Install-EntireWithScoop -Times 0 -Exactly
    }
}
