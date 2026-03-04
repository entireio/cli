#requires -Version 5.1

$ErrorActionPreference = 'Stop'

$githubRepo = if ($env:ENTIRE_INSTALL_REPO) { $env:ENTIRE_INSTALL_REPO } else { 'entireio/cli' }
$defaultInstallDir = if ($env:ENTIRE_INSTALL_DIR) { $env:ENTIRE_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'Programs\entire\bin' }
$requestedVersion = if ($env:ENTIRE_INSTALL_VERSION) { $env:ENTIRE_INSTALL_VERSION } else { '' }
$baseUrlOverride = if ($env:ENTIRE_INSTALL_BASE_URL) { $env:ENTIRE_INSTALL_BASE_URL } else { '' }

function Fail([string]$message) {
  Write-Error $message
}

function Info([string]$message) {
  Write-Host ("==> {0}" -f $message)
}

function Success([string]$message) {
  Write-Host ("==> {0}" -f $message)
}

function Detect-Arch {
  $arch = $env:PROCESSOR_ARCHITECTURE
  if (-not $arch) {
    Fail 'Unable to detect architecture (PROCESSOR_ARCHITECTURE is empty).'
  }

  switch ($arch.ToUpperInvariant()) {
    'AMD64' { return 'amd64' }
    'ARM64' { return 'arm64' }
    default { Fail ("Unsupported architecture: {0}" -f $arch) }
  }
}

function Get-Latest-Version([string]$repo) {
  $url = "https://api.github.com/repos/$repo/releases/latest"
  $headers = @{
    Accept     = 'application/vnd.github+json'
    'User-Agent' = 'entire-install-script'
  }
  if ($env:GITHUB_TOKEN) {
    $headers.Authorization = "Bearer $($env:GITHUB_TOKEN)"
  }

  try {
    $release = Invoke-RestMethod -Uri $url -Headers $headers -Method Get
  } catch {
    Fail 'Failed to fetch latest version from GitHub. Please check your internet connection.'
  }

  $tag = [string]$release.tag_name
  if (-not $tag) {
    Fail 'Failed to determine latest version (tag_name missing).'
  }

  return $tag.TrimStart('v')
}

function Download-File([string]$url, [string]$outputPath) {
  Invoke-WebRequest -Uri $url -OutFile $outputPath -UseBasicParsing
}

function Get-Expected-Checksum([string]$checksumsPath, [string]$archiveName) {
  foreach ($line in Get-Content -LiteralPath $checksumsPath) {
    $trimmed = $line.Trim()
    if (-not $trimmed) {
      continue
    }

    $parts = $trimmed -split '\s+'
    if ($parts.Count -lt 2) {
      continue
    }

    $hash = $parts[0]
    $file = $parts[-1]
    if ($file -ieq $archiveName) {
      return $hash
    }
  }

  return ''
}

function Verify-Checksum([string]$filePath, [string]$expectedChecksum) {
  $actual = (Get-FileHash -Algorithm SHA256 -LiteralPath $filePath).Hash.ToLowerInvariant()
  $expected = $expectedChecksum.ToLowerInvariant()
  if ($actual -ne $expected) {
    Fail ("Checksum verification failed! Expected: {0}, actual: {1}" -f $expected, $actual)
  }
}

function Ensure-Install-Dir([string]$installDir) {
  if (-not (Test-Path -LiteralPath $installDir)) {
    New-Item -ItemType Directory -Path $installDir -Force | Out-Null
  }
}

function Normalize-PathForCompare([string]$pathValue) {
  if (-not $pathValue) {
    return ''
  }

  $normalized = $pathValue.Trim().TrimEnd([char]92, [char]47)
  return $normalized.ToLowerInvariant()
}

function UserPathContains([string]$targetDir) {
  $pathUser = [Environment]::GetEnvironmentVariable('Path', 'User')
  if (-not $pathUser) {
    return $false
  }

  $needle = Normalize-PathForCompare $targetDir
  foreach ($entry in ($pathUser -split ';')) {
    if ((Normalize-PathForCompare $entry) -eq $needle) {
      return $true
    }
  }

  return $false
}

function Add-InstallDirToUserPath([string]$installDir) {
  $pathUser = [Environment]::GetEnvironmentVariable('Path', 'User')
  if ($pathUser) {
    [Environment]::SetEnvironmentVariable('Path', ("{0};{1}" -f $installDir, $pathUser), 'User')
  } else {
    [Environment]::SetEnvironmentVariable('Path', $installDir, 'User')
  }
}

function Main {
  Info 'Installing Entire CLI...'

  $arch = Detect-Arch
  $os = 'windows'
  Info ("Detected platform: {0}/{1}" -f $os, $arch)

  $version = $requestedVersion
  if (-not $version) {
    Info 'Fetching latest version...'
    $version = Get-Latest-Version -repo $githubRepo
  }
  $version = $version.TrimStart('v')
  Info ("Installing version: {0}" -f $version)

  $archiveName = "entire_${os}_${arch}.zip"

  $baseUrl = $baseUrlOverride
  if (-not $baseUrl) {
    $baseUrl = "https://github.com/$githubRepo/releases/download/v$version"
  }
  $downloadUrl = "$baseUrl/$archiveName"
  $checksumsUrl = "$baseUrl/checksums.txt"

  $tmpDir = Join-Path ([System.IO.Path]::GetTempPath()) ([System.Guid]::NewGuid().ToString('N'))
  New-Item -ItemType Directory -Path $tmpDir -Force | Out-Null

  try {
    $archivePath = Join-Path $tmpDir $archiveName
    Info ("Downloading {0}..." -f $archiveName)
    try {
      Download-File -url $downloadUrl -outputPath $archivePath
    } catch {
      Fail ("Failed to download from {0}. Please check that the version exists and try again." -f $downloadUrl)
    }

    Info 'Downloading checksums...'
    $checksumsPath = Join-Path $tmpDir 'checksums.txt'
    try {
      Download-File -url $checksumsUrl -outputPath $checksumsPath
    } catch {
      Fail ("Failed to download checksums from {0}" -f $checksumsUrl)
    }

    Info 'Verifying checksum...'
    $expectedChecksum = Get-Expected-Checksum -checksumsPath $checksumsPath -archiveName $archiveName
    if (-not $expectedChecksum) {
      Fail ("Checksum for {0} not found in checksums.txt" -f $archiveName)
    }
    Verify-Checksum -filePath $archivePath -expectedChecksum $expectedChecksum
    Success 'Checksum verified'

    Info 'Extracting...'
    Expand-Archive -LiteralPath $archivePath -DestinationPath $tmpDir -Force

    $installDir = $defaultInstallDir
    Ensure-Install-Dir -installDir $installDir
    Info ("Installing to {0}..." -f $installDir)

    $binaryPath = Join-Path $tmpDir 'entire.exe'
    if (-not (Test-Path -LiteralPath $binaryPath)) {
      Fail 'Extraction completed but entire.exe was not found in the archive.'
    }

    $installPath = Join-Path $installDir 'entire.exe'
    Move-Item -LiteralPath $binaryPath -Destination $installPath -Force

    try {
      & $installPath version | Out-Null
    } catch {
      Fail 'Installation completed but the binary failed to execute. Please check the installation.'
    }

    Success ("Entire CLI installed to {0}" -f $installPath)

    $cmd = Get-Command entire -ErrorAction SilentlyContinue
    if ($cmd -and $cmd.Source -and ($cmd.Source -ne $installPath)) {
      Write-Host ''
      Write-Host 'WARNING: PATH conflict detected'
      Write-Host ("Installed to: {0}" -f $installPath)
      Write-Host ("But 'entire' resolves to: {0}" -f $cmd.Source)
      Write-Host ''
      Fail 'Installation completed but PATH needs adjustment. Then, rerun the installation.'
    }

    if (-not (UserPathContains -targetDir $installDir)) {
      Add-InstallDirToUserPath -installDir $installDir
      Success ("Added {0} to your user PATH" -f $installDir)
      Write-Host ''
      Write-Host '  Restart your terminal, then run entire to get started.'
      return
    }

    Info 'Running post-install actions...'
    & $installPath curl-bash-post-install
  } finally {
    if (Test-Path -LiteralPath $tmpDir) {
      Remove-Item -LiteralPath $tmpDir -Recurse -Force -ErrorAction SilentlyContinue
    }
  }
}

Main
