# Test-only proxies guard the real cmdlets against accidental public requests;
# fixture writes are confined to Pester's disposable TestDrive.
[Diagnostics.CodeAnalysis.SuppressMessageAttribute('PSAvoidOverwritingBuiltInCmdlets', '')]
[Diagnostics.CodeAnalysis.SuppressMessageAttribute('PSUseShouldProcessForStateChangingFunctions', '')]
param()

Describe 'Release origins over local HTTP' {
    BeforeAll {
        . (Join-Path (Split-Path -Parent $PSScriptRoot) 'install.ps1')
        $script:savedBaseUrl = $env:ENTIRE_INSTALL_BASE_URL
        $script:savedToken = $env:GITHUB_TOKEN
        $env:ENTIRE_INSTALL_BASE_URL = $null
        $env:GITHUB_TOKEN = $null
        $script:fixtureRoot = Join-Path $TestDrive 'http'
        $ready = Join-Path $TestDrive 'port'
        $script:requestLog = Join-Path $TestDrive 'requests'
        New-Item -ItemType Directory -Path $script:fixtureRoot | Out-Null

        # TcpListener works without HTTP.sys URL reservations on Windows and
        # also runs on macOS/Linux. Bind an ephemeral loopback port, never a
        # public interface. The job is stopped in AfterAll, including on failure.
        $script:server = Start-Job -ScriptBlock {
            $listener = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, 0)
            $listener.Start()
            try {
                [IO.File]::WriteAllText($using:ready, [string] $listener.LocalEndpoint.Port)
                while ($true) {
                    # A blocking Accept prevents Stop-Job from interrupting the
                    # runspace. Poll so teardown cannot hang on an idle server.
                    if (-not $listener.Pending()) {
                        Start-Sleep -Milliseconds 10
                        continue
                    }
                    $client = $listener.AcceptTcpClient()
                    try {
                        $stream = $client.GetStream()
                        $stream.ReadTimeout = 5000
                        $reader = [IO.StreamReader]::new($stream)
                        $request = $reader.ReadLine()
                        while ($reader.ReadLine()) { }
                        $path = (($request -split ' ')[1] -split '\?')[0]
                        [IO.File]::AppendAllText($using:requestLog, "$path`n")
                        $file = Join-Path $using:fixtureRoot $path.TrimStart('/')
                        if ([IO.File]::Exists($file)) {
                            $bytes = [IO.File]::ReadAllBytes($file)
                            $status = '200 OK'
                        }
                        else {
                            $bytes = [Text.Encoding]::UTF8.GetBytes('not found')
                            $status = '404 Not Found'
                        }
                        $header = [Text.Encoding]::ASCII.GetBytes("HTTP/1.1 $status`r`nContent-Type: application/json`r`nContent-Length: $($bytes.Length)`r`nConnection: close`r`n`r`n")
                        $stream.Write($header, 0, $header.Length)
                        $stream.Write($bytes, 0, $bytes.Length)
                    }
                    finally { $client.Dispose() }
                }
            }
            finally { $listener.Stop() }
        }
        $deadline = [DateTime]::UtcNow.AddSeconds(20)
        while (-not (Test-Path -LiteralPath $ready)) {
            if ([DateTime]::UtcNow -gt $deadline -or $script:server.State -eq 'Failed') {
                throw 'Local fixture server did not start.'
            }
            Start-Sleep -Milliseconds 50
        }
        $script:localBase = 'http://127.0.0.1:' + (Get-Content -LiteralPath $ready -Raw)
        Set-Variable -Name DefaultReleaseOrigin -Value "$script:localBase/mirror"
        Set-Variable -Name GitHubReleaseOrigin -Value "$script:localBase/github"
        Set-Variable -Name GitHubApi -Value "$script:localBase/api"

        # Refuse any public URL even if a future installer edit reintroduces a
        # hard-coded endpoint. Call the real cmdlets only after that guard.
        function Invoke-RestMethod {
            [CmdletBinding()]
            param([string] $Uri, [hashtable] $Headers, [int] $TimeoutSec)
            if (-not $Uri.StartsWith("$script:localBase/")) { throw "Non-local metadata URL: $Uri" }
            Microsoft.PowerShell.Utility\Invoke-RestMethod @PSBoundParameters
        }
        function Invoke-WebRequest {
            [CmdletBinding()]
            param([string] $Uri, [string] $OutFile, [switch] $UseBasicParsing)
            if (-not $Uri.StartsWith("$script:localBase/")) { throw "Non-local download URL: $Uri" }
            Microsoft.PowerShell.Utility\Invoke-WebRequest @PSBoundParameters
        }
        function Set-HttpFixture {
            param([string] $RelativePath, [string] $Text)
            $file = Join-Path $script:fixtureRoot $RelativePath
            [IO.Directory]::CreateDirectory((Split-Path -Parent $file)) | Out-Null
            [IO.File]::WriteAllText($file, $Text)
        }
    }

    AfterAll {
        if ($script:server) {
            Stop-Job $script:server
            Remove-Job $script:server -Force
        }
        $env:ENTIRE_INSTALL_BASE_URL = $script:savedBaseUrl
        $env:GITHUB_TOKEN = $script:savedToken
    }

    BeforeEach {
        Set-StrictMode -Version 2.0
        Get-ChildItem -LiteralPath $script:fixtureRoot | Remove-Item -Recurse -Force
        [IO.File]::WriteAllText($script:requestLog, '')
        Set-HttpFixture 'mirror/channels/stable.json' '{"channel":"stable","tag":"v1.2.3","version":"9.9.9"}'
        Set-HttpFixture 'api/releases/latest' '{"tag_name":"v1.2.3"}'
        foreach ($origin in 'mirror', 'github') {
            Set-HttpFixture "$origin/v1.2.3/entire_windows_amd64.zip" "$origin archive bytes"
            $hash = (Get-FileHash -LiteralPath (Join-Path $script:fixtureRoot "$origin/v1.2.3/entire_windows_amd64.zip") -Algorithm SHA256).Hash
            Set-HttpFixture "$origin/v1.2.3/checksums.txt" "$hash  entire_windows_amd64.zip"
        }
        $script:archivePath = Join-Path $TestDrive 'download.zip'
        $script:checksumsPath = Join-Path $TestDrive 'download-checksums.txt'
    }

    It 'discovers and downloads from the mirror without GitHub' {
        $release = Get-ReleaseVersion stable
        $release | Should -Be '1.2.3'
        Save-ReleaseArchive $release 'entire_windows_amd64.zip' $script:archivePath $script:checksumsPath
        @(Get-Content -LiteralPath $script:requestLog) | Should -HaveCount 3
        Get-Content -LiteralPath $script:requestLog | Should -Not -Match '/github/|/api/'
        Get-Content -LiteralPath $script:archivePath -Raw | Should -Be 'mirror archive bytes'
    }

    It 'falls back on bad metadata and a missing checksum file, without mixing origins' {
        Set-HttpFixture 'mirror/channels/stable.json' '{"tag":"v1.2.3-rc.1"}'
        Remove-Item -LiteralPath (Join-Path $script:fixtureRoot 'mirror/v1.2.3/checksums.txt')
        $release = Get-ReleaseVersion stable
        $release | Should -Be '1.2.3'
        Save-ReleaseArchive $release 'entire_windows_amd64.zip' $script:archivePath $script:checksumsPath
        $requests = @(Get-Content -LiteralPath $script:requestLog)
        $requests | Should -HaveCount 6
        $requests[1] | Should -Be '/api/releases/latest'
        $requests[-2] | Should -Be '/github/v1.2.3/entire_windows_amd64.zip'
        $requests[-1] | Should -Be '/github/v1.2.3/checksums.txt'
        Get-Content -LiteralPath $script:archivePath -Raw | Should -Be 'github archive bytes'
    }

    It 'decodes a pretty-printed GitHub release list and skips near-nightly tags' {
        Remove-Item -LiteralPath (Join-Path $script:fixtureRoot 'api/releases') -Recurse -Force
        Set-HttpFixture 'api/releases' @'
[
  {
    "tag_name": "v1.2.3-prenightly"
  },
  {
    "tag_name": "v1.2.3-nightly"
  },
  {
    "tag_name": "v1.2.3-nightly.1"
  }
]
'@
        Get-ReleaseVersion nightly | Should -Be '1.2.3-nightly.1'
        @(Get-Content -LiteralPath $script:requestLog) | Should -HaveCount 2
    }

    It 'wires discovery, verified download and extraction into the direct installer' {
        $binary = Join-Path $TestDrive 'entire.exe'
        [IO.File]::WriteAllText($binary, 'fixture binary, never executed')
        $zip = Join-Path $script:fixtureRoot 'mirror/v1.2.3/entire_windows_amd64.zip'
        Remove-Item -LiteralPath $zip
        Compress-Archive -LiteralPath $binary -DestinationPath $zip
        $hash = (Get-FileHash -LiteralPath $zip -Algorithm SHA256).Hash
        Set-HttpFixture 'mirror/v1.2.3/checksums.txt' "$hash  entire_windows_amd64.zip"
        Mock Assert-Prerequisite {}
        Mock Get-PlatformArchitecture { 'amd64' }
        Mock Initialize-InstallDirectory {}
        Mock Install-BinaryFile {
            Get-Content -LiteralPath $Source -Raw | Should -Be 'fixture binary, never executed'
            throw 'verified binary ready to install'
        }
        Set-Variable -Name InstallDir -Value (Join-Path $TestDrive 'bin')
        Set-Variable -Name InstallDirExplicit -Value $true
        { Install-Entire } | Should -Throw '*verified binary ready to install*'
        Should -Invoke Install-BinaryFile -Times 1 -Exactly
        @(Get-Content -LiteralPath $script:requestLog) | Should -HaveCount 3
    }

    It 'does not conceal checksum corruption by fetching GitHub bytes' {
        Set-HttpFixture 'mirror/v1.2.3/checksums.txt' (('0' * 64) + '  entire_windows_amd64.zip')
        { Save-ReleaseArchive '1.2.3' 'entire_windows_amd64.zip' $script:archivePath $script:checksumsPath } |
            Should -Throw '*Checksum verification failed*'
        @(Get-Content -LiteralPath $script:requestLog) | Should -HaveCount 2
        Get-Content -LiteralPath $script:requestLog | Should -Not -Match '/github/'
    }
}
