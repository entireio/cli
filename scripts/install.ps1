[CmdletBinding()]
# The parameters are read inside functions, which PSScriptAnalyzer does not
# follow when deciding whether a parameter is used. One suppression per
# parameter, so a new unused parameter still reports.
[Diagnostics.CodeAnalysis.SuppressMessageAttribute('PSReviewUnusedParameter', 'Channel')]
[Diagnostics.CodeAnalysis.SuppressMessageAttribute('PSReviewUnusedParameter', 'InstallDir')]
[Diagnostics.CodeAnalysis.SuppressMessageAttribute('PSReviewUnusedParameter', 'NoPathUpdate')]
[Diagnostics.CodeAnalysis.SuppressMessageAttribute('PSReviewUnusedParameter', 'Help')]
param(
    [ValidateSet("stable", "nightly")]
    [string] $Channel = "stable",

    [ValidateNotNullOrEmpty()]
    [string] $InstallDir = (Join-Path $HOME ".local\bin"),

    [switch] $NoPathUpdate,

    [Alias("h")]
    [switch] $Help
)

$GitHubRepo = "entireio/cli"
$ScoopBucketUrl = "https://github.com/entireio/scoop-bucket.git"
# Whether the caller chose the install directory. Read here because
# $PSBoundParameters inside a function is that function's own; the functions
# read this the way they read $InstallDir.
$InstallDirExplicit = $PSBoundParameters.ContainsKey('InstallDir')

# Two limits, deliberately not one: the API calls get a whole-request cap,
# the archive download gets only a stall guard. See Save-RemoteFile.
$ApiTimeoutSec = 60
$DownloadStallTimeoutSec = 60

function Write-Info {
    param([string] $Message)
    Write-Host "==> $Message" -ForegroundColor Cyan
}

function Write-Success {
    param([string] $Message)
    Write-Host "==> $Message" -ForegroundColor Green
}

function Write-InstallerWarning {
    param([string] $Message)
    Write-Host "Warning: $Message" -ForegroundColor Yellow
}

function Write-Usage {
    Write-Host @"
Usage: install.ps1 [-Channel stable|nightly] [-InstallDir <path>] [-NoPathUpdate]

Options:
  -Channel       Release channel to install (default: stable)
  -InstallDir    Direct-install destination (default: `$HOME\.local\bin)
  -NoPathUpdate  Do not add a direct-install destination to the user PATH
  -Help, -h      Show this help message

Stable installs use Scoop when it is available. Nightly installs use the
verified release archive because the Scoop bucket only publishes stable builds.

Scoop chooses its own install location and manages its own PATH entry.
An explicit -InstallDir selects a release-archive install even when Scoop
is available; -NoPathUpdate applies only to release-archive installs.
"@
}

# $env:OS is "Windows_NT" in both Windows PowerShell and pwsh on Windows and
# unset elsewhere; $IsWindows does not exist in Windows PowerShell 5.1.
function Test-IsWindows {
    $env:OS -eq 'Windows_NT'
}

# A runtime check, not `#Requires -Version`: that line is honoured when the
# script runs from its file and ignored when the text arrives through
# irm | iex or [scriptblock]::Create.
function Test-MinimumPowerShell {
    $PSVersionTable.PSVersion -ge [version]'5.1'
}

function Test-FullLanguageMode {
    $ExecutionContext.SessionState.LanguageMode -eq 'FullLanguage'
}

function Assert-Prerequisite {
    if (-not (Test-IsWindows)) {
        throw "install.ps1 supports Windows only. On macOS and Linux, run: curl -fsSL https://entire.io/install.sh | bash"
    }
    # No TLS 1.2 check follows: Windows PowerShell 5.1 needs .NET Framework
    # 4.5.2 or later, which has had Tls12 since 4.5, so a host that passes this
    # check always has it.
    if (-not (Test-MinimumPowerShell)) {
        throw "Windows PowerShell 5.1 or later is required; this is $($PSVersionTable.PSVersion). Get a newer PowerShell at https://aka.ms/powershell"
    }
    # Later steps use registry types and Add-Type, which ConstrainedLanguage
    # rejects with errors that do not name the cause.
    if (-not (Test-FullLanguageMode)) {
        throw "PowerShell FullLanguage mode is required; this session is in $($ExecutionContext.SessionState.LanguageMode) mode (usually set by an AppLocker or WDAC policy)."
    }
}

function Invoke-Scoop {
    param(
        [Parameter(Mandatory = $true, ValueFromRemainingArguments = $true)]
        [string[]] $ScoopArgs
    )

    # Native stderr redirected with 2>&1 becomes ErrorRecord. With
    # $ErrorActionPreference Stop that is terminating even when scoop
    # exits 0 (update notices, deprecation warnings).
    $previous = $ErrorActionPreference
    $ErrorActionPreference = "Continue"
    try {
        $output = & "scoop" @ScoopArgs 2>&1
        return @{
            ExitCode = [int] $LASTEXITCODE
            Output   = $output
        }
    }
    finally {
        $ErrorActionPreference = $previous
    }
}

function Test-ScoopAppInstalled {
    param([string] $AppName)

    # Only the exit code matters, so discard every stream. scoop's abort /
    # warn / info all use Write-Host, i.e. the information stream, which
    # 2>&1 neither captures nor suppresses -- a failed probe would
    # otherwise print "Could not find app path for '<app>'" straight to the
    # console and read as a failure during a successful first install.
    # Deliberately not folded into Invoke-Scoop: a blanket redirect there
    # would also hide scoop's install and update progress, which is useful.
    $previous = $ErrorActionPreference
    $ErrorActionPreference = "Continue"
    try {
        & "scoop" prefix $AppName *> $null
        return $LASTEXITCODE -eq 0
    }
    finally {
        $ErrorActionPreference = $previous
    }
}

function Format-ScoopOutput {
    param($Output)
    ($Output | Out-String).Trim()
}

# `scoop bucket add` exits 0 when it adds the bucket and 2 when the bucket
# is already there (0 in Scoop releases before v0.3.0); every real failure
# exits 1. Asking first with `bucket list` is not an option: its output is
# rendered text whose shape changes with the number of columns, and on a
# Scoop with no buckets at all it exits 2 itself.
function Add-EntireScoopBucket {
    Write-Info "Adding the Entire Scoop bucket..."
    $added = Invoke-Scoop bucket add entire $ScoopBucketUrl
    if ($added.ExitCode -notin 0, 2) {
        throw "Failed to add the Entire Scoop bucket (scoop exited $($added.ExitCode)). $(Format-ScoopOutput $added.Output)"
    }
}

function Install-EntireWithScoop {
    Write-Info "Scoop detected; installing Entire CLI with Scoop..."

    Add-EntireScoopBucket

    $isInstalled = Test-ScoopAppInstalled -AppName "entire"
    if ($isInstalled) {
        Write-Info "Updating Entire CLI with Scoop..."
        $updated = Invoke-Scoop update entire/entire
        if ($updated.ExitCode -ne 0) {
            throw "Scoop failed to update Entire CLI (scoop exited $($updated.ExitCode)). $(Format-ScoopOutput $updated.Output)"
        }
    }
    else {
        Write-Info "Installing Entire CLI with Scoop..."
        $installed = Invoke-Scoop install entire/entire
        if ($installed.ExitCode -ne 0) {
            throw "Scoop failed to install Entire CLI (scoop exited $($installed.ExitCode)). $(Format-ScoopOutput $installed.Output)"
        }
    }

    Write-Success "Entire CLI installed with Scoop"
}

function Get-NativeArchitectureName {
    $environmentKey = "HKLM:\SYSTEM\CurrentControlSet\Control\Session Manager\Environment"
    (Get-ItemProperty -LiteralPath $environmentKey -Name "PROCESSOR_ARCHITECTURE").PROCESSOR_ARCHITECTURE
}

function Get-PlatformArchitecture {
    # The machine-level value is the native OS architecture. The
    # $env:PROCESSOR_ARCHITECTURE seen by the script is the process's, so
    # x64 PowerShell on ARM64 Windows reports AMD64. RuntimeInformation is
    # not an option either: it needs .NET 4.7.1+, so it is missing on
    # stock Windows PowerShell 5.1 installs.
    $architecture = Get-NativeArchitectureName

    if ([string]::IsNullOrWhiteSpace($architecture)) {
        throw "Cannot determine the Windows architecture."
    }

    switch ($architecture.ToUpperInvariant()) {
        "AMD64" { return "amd64" }
        "ARM64" { return "arm64" }
        default { throw "Unsupported architecture: $architecture" }
    }
}

function Invoke-GitHubApi {
    param([string] $Uri)

    $headers = @{
        "Accept"     = "application/vnd.github+json"
        "User-Agent" = "entire-install.ps1"
    }
    if (-not [string]::IsNullOrWhiteSpace($env:GITHUB_TOKEN)) {
        $headers["Authorization"] = "Bearer $($env:GITHUB_TOKEN)"
    }

    # No -UseBasicParsing here: only Invoke-WebRequest runs the Internet
    # Explorer HTML parser on Windows PowerShell 5.1; Invoke-RestMethod
    # decodes the body itself. See Save-RemoteFile for the one that needs it.
    try {
        Invoke-RestMethod -Uri $Uri -Headers $headers -TimeoutSec $ApiTimeoutSec
    }
    catch {
        # Only WebException (5.1) and HttpResponseException (pwsh) carry a
        # Response; a timeout surfaces as TaskCanceledException, which has no
        # such property, and under StrictMode reading it would throw.
        $response = $_.Exception.PSObject.Properties['Response']
        if ($null -ne $response -and $null -ne $response.Value) {
            throw "GitHub returned HTTP $([int] $response.Value.StatusCode) for $Uri. $($_.Exception.Message)"
        }
        throw "Could not reach GitHub at $Uri. $($_.Exception.Message) Check your internet connection."
    }
}

function Get-ReleaseVersion {
    param([string] $ReleaseChannel)

    if ($ReleaseChannel -eq "nightly") {
        $uri = "https://api.github.com/repos/$GitHubRepo/releases?per_page=20"
        $releases = Invoke-GitHubApi -Uri $uri
        $release = $null
        $checked = 0

        # GitHub returns created_at descending. The first *nightly* tag is
        # the latest nightly. Windows PowerShell 5.1 returns a JSON array
        # from Invoke-RestMethod as one pipeline object, so enumerate it
        # explicitly instead of piping to Where-Object.
        foreach ($candidate in $releases) {
            $checked++
            if ($candidate.tag_name -like "*nightly*") {
                $release = $candidate
                break
            }
        }
        # Invoke-GitHubApi has already thrown for any network or HTTP
        # failure, so an empty result here means GitHub answered and nothing
        # in the answer was a nightly.
        if ($null -eq $release) {
            throw "No nightly release found among the $checked most recent releases of $GitHubRepo. Try -Channel stable."
        }
    }
    else {
        $uri = "https://api.github.com/repos/$GitHubRepo/releases/latest"
        $release = Invoke-GitHubApi -Uri $uri
    }

    if ($null -eq $release -or [string]::IsNullOrWhiteSpace([string] $release.tag_name)) {
        throw "GitHub returned the latest release of $GitHubRepo without a tag name, so the version cannot be determined."
    }

    $version = ([string] $release.tag_name) -replace "^v", ""
    if ($version -notmatch "^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$") {
        throw "GitHub returned an invalid release version: $version"
    }

    return $version
}

function Save-RemoteFile {
    param(
        [string] $Uri,
        [string] $Destination
    )

    # No -TimeoutSec on the download. What it means depends on the host:
    # Windows PowerShell 5.1 applies it to the response headers only (the body
    # read has its own 5-minute stall default); PowerShell 7.0-7.3 applied it
    # to the whole transfer, so a 21 MB archive on a link below ~3 Mbit/s was
    # cancelled at 60 s with "The operation was canceled."; PowerShell 7.4+
    # made it an alias of -ConnectionTimeoutSeconds, which caps the connection
    # only. Instead, where the host has -OperationTimeoutSeconds (7.4+), the
    # download is cut when no data arrives for $DownloadStallTimeoutSec: a slow
    # link finishes, a dead connection is reported instead of hanging, since
    # the host has no stall detector of its own.
    #
    # -UseBasicParsing is required for Windows PowerShell 5.1 (skips the IE
    # DOM parser). In PowerShell 7+ it is accepted and silently ignored.
    $stallGuard = @{}
    if ((Get-Command Invoke-WebRequest).Parameters.ContainsKey('OperationTimeoutSeconds')) {
        $stallGuard['OperationTimeoutSeconds'] = $DownloadStallTimeoutSec
    }
    Invoke-WebRequest -Uri $Uri -OutFile $Destination -UseBasicParsing @stallGuard
}

# Verifies the archive against checksums.txt from the same release. That
# guards against a corrupted transfer and against installing the wrong asset
# (another architecture or version). It does not guard against a compromised
# release origin or TLS path: the checksum and the archive share a trust root.
function Assert-Checksum {
    param(
        [string] $ArchivePath,
        [string] $ArchiveName,
        [string] $ChecksumsPath
    )

    $escapedArchiveName = [regex]::Escape($ArchiveName)
    $checksumLine = Get-Content -LiteralPath $ChecksumsPath |
        Where-Object { $_ -match "(?i)^[0-9a-f]{64}\s+\*?$escapedArchiveName\s*$" } |
        Select-Object -First 1

    if ([string]::IsNullOrWhiteSpace([string] $checksumLine)) {
        throw "Checksum for $ArchiveName was not found in checksums.txt."
    }

    $expectedChecksum = ([string] $checksumLine -split "\s+")[0]
    $actualChecksum = (Get-FileHash -LiteralPath $ArchivePath -Algorithm SHA256).Hash
    if (-not [string]::Equals($actualChecksum, $expectedChecksum, [StringComparison]::OrdinalIgnoreCase)) {
        throw "Checksum verification failed. Expected: $expectedChecksum, actual: $actualChecksum"
    }
}

# Classifies a failed file operation by its cause, walking the exception
# chain because PowerShell wraps provider and .NET errors differently:
# Move-Item surfaces UnauthorizedAccessException directly, a .NET method call
# puts it inside MethodInvocationException. Type is checked before HResult so
# a wrapper with its own HResult cannot hide the cause.
function Get-WriteFailureKind {
    param($Exception)

    $current = $Exception
    while ($null -ne $current) {
        if ($current -is [UnauthorizedAccessException] -or $current.HResult -eq 0x80070005) {
            return 'denied'
        }
        # ERROR_SHARING_VIOLATION and ERROR_LOCK_VIOLATION
        if ($current -is [IO.IOException] -and ($current.HResult -eq 0x80070020 -or $current.HResult -eq 0x80070021)) {
            return 'held'
        }
        $current = $current.InnerException
    }
    return 'other'
}

# The message for a failed write, naming the remedy only when the cause is
# known: access denied gets elevation or another directory, a held file gets
# "close the program", anything else is reported as it is.
function Get-WriteFailureMessage {
    param(
        [string] $Action,
        [string] $Path,
        [string] $Directory,
        $Exception
    )

    switch (Get-WriteFailureKind -Exception $Exception) {
        'denied' {
            return "Access denied writing to $Directory. Run PowerShell as Administrator, or pass -InstallDir with a directory you can write to. ($($Exception.Message))"
        }
        'held' {
            return "Cannot replace $Path because another program holds it open. Close entire (including any running 'entire mcp') and any tool that has the file open, then rerun the installer. ($($Exception.Message))"
        }
        default {
            return "Could not $Action $Path. ($($Exception.Message))"
        }
    }
}

# New-Item has no -LiteralPath; CreateDirectory takes the path as written,
# creates parents, and is a no-op when it exists.
function Initialize-InstallDirectory {
    param([string] $Path)

    try {
        [IO.Directory]::CreateDirectory($Path) | Out-Null
    }
    catch {
        throw (Get-WriteFailureMessage -Action 'create' -Path $Path -Directory $Path -Exception $_.Exception)
    }
}

# Windows lets a running executable be renamed but not overwritten or
# deleted, so an existing binary is moved aside, the verified file copied in,
# and the old image removed if nothing holds it. A .old that is itself still
# running gets out of the way under a unique name, and stale *.old files
# left by a previous run are removed first. The copy failing puts the old
# binary back before the error propagates.
function Install-BinaryFile {
    param(
        [string] $Source,
        [string] $Destination
    )

    $leaf = Split-Path -Leaf $Destination
    Get-ChildItem -LiteralPath (Split-Path -Parent $Destination) -Filter "$leaf*.old" -File -ErrorAction SilentlyContinue |
        Remove-Item -Force -ErrorAction SilentlyContinue

    $retired = $null
    if (Test-Path -LiteralPath $Destination -PathType Leaf) {
        $retired = "$Destination.old"
        if (Test-Path -LiteralPath $retired) {
            $retired = "$Destination.$([guid]::NewGuid().ToString('N')).old"
        }
        try {
            Move-Item -LiteralPath $Destination -Destination $retired -ErrorAction Stop
        }
        catch {
            throw (Get-WriteFailureMessage -Action 'move aside' -Path $Destination -Directory (Split-Path -Parent $Destination) -Exception $_.Exception)
        }
    }

    try {
        Copy-Item -LiteralPath $Source -Destination $Destination -ErrorAction Stop
    }
    catch {
        if ($null -ne $retired) {
            Move-Item -LiteralPath $retired -Destination $Destination -Force -ErrorAction SilentlyContinue
        }
        throw (Get-WriteFailureMessage -Action 'write' -Path $Destination -Directory (Split-Path -Parent $Destination) -Exception $_.Exception)
    }

    if ($null -ne $retired) {
        Remove-Item -LiteralPath $retired -Force -ErrorAction SilentlyContinue
    }
}

function Get-NormalizedPath {
    param([string] $Path)

    # Resolve against $PWD and expand ~. [IO.Path]::GetFullPath alone uses
    # [Environment]::CurrentDirectory, which Windows PowerShell 5.1 does
    # not keep in sync with Set-Location, and does not expand ~.
    $fullPath = [IO.Path]::GetFullPath(
        $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($Path)
    )
    $root = [IO.Path]::GetPathRoot($fullPath)
    if ([string]::Equals($fullPath, $root, [StringComparison]::OrdinalIgnoreCase)) {
        return $root
    }

    [char[]] $separators = @(
        [IO.Path]::DirectorySeparatorChar,
        [IO.Path]::AltDirectorySeparatorChar
    )
    return $fullPath.TrimEnd($separators)
}

function Test-SamePath {
    param(
        [string] $Left,
        [string] $Right
    )

    if ([string]::IsNullOrWhiteSpace($Left) -or [string]::IsNullOrWhiteSpace($Right)) {
        return $false
    }

    # The user PATH is read unexpanded, so an entry may be stored as
    # %USERPROFILE%\.local\bin. Expand before normalising: unexpanded, the
    # entry would be resolved relative to the current directory instead.
    $Left = [Environment]::ExpandEnvironmentVariables($Left)
    $Right = [Environment]::ExpandEnvironmentVariables($Right)

    try {
        return [string]::Equals(
            (Get-NormalizedPath -Path $Left),
            (Get-NormalizedPath -Path $Right),
            [StringComparison]::OrdinalIgnoreCase
        )
    }
    catch {
        return $false
    }
}

function Test-PathContains {
    # "Contains" is a verb form, not a plural noun.
    [Diagnostics.CodeAnalysis.SuppressMessageAttribute('PSUseSingularNouns', '')]
    param(
        [AllowNull()]
        [string] $PathValue,
        [string] $Directory
    )

    if ([string]::IsNullOrWhiteSpace($PathValue)) {
        return $false
    }

    foreach ($entry in ($PathValue -split ";")) {
        if (Test-SamePath -Left $entry.Trim() -Right $Directory) {
            return $true
        }
    }
    return $false
}

function Test-PathIsFirst {
    param(
        [AllowNull()]
        [string] $PathValue,
        [string] $Directory
    )

    if ([string]::IsNullOrWhiteSpace($PathValue)) {
        return $false
    }

    foreach ($entry in ($PathValue -split ";")) {
        $trimmed = $entry.Trim()
        if ([string]::IsNullOrWhiteSpace($trimmed)) {
            continue
        }
        return (Test-SamePath -Left $trimmed -Right $Directory)
    }
    return $false
}

function Get-PathWithDirectoryFirst {
    param(
        [AllowNull()]
        [string] $PathValue,
        [string] $Directory
    )

    # The value is someone's registry string and this function owns one
    # entry in it: other entries are kept verbatim, empty entries and a
    # trailing ";" included. When the directory is already present, its first
    # occurrence moves to the front as stored, so an entry written as
    # %USERPROFILE%\... stays unexpanded; only when it is absent is the
    # literal $Directory added.
    $kept = New-Object System.Collections.Generic.List[string]
    $existing = $null
    if (-not [string]::IsNullOrWhiteSpace($PathValue)) {
        foreach ($entry in ($PathValue -split ";")) {
            if (Test-SamePath -Left $entry.Trim() -Right $Directory) {
                if ($null -eq $existing) {
                    $existing = $entry
                }
                continue
            }
            $kept.Add($entry)
        }
    }
    $first = if ($null -ne $existing) { $existing } else { $Directory }
    if ($kept.Count -eq 0) {
        return $first
    }
    return "$first;" + ($kept -join ";")
}

# [Environment]::GetEnvironmentVariable expands REG_EXPAND_SZ on read and
# SetEnvironmentVariable writes REG_SZ, so a round trip through them bakes
# the current %USERPROFILE% into the stored value and downgrades its kind.
# These read and write the registry value as stored.
function Get-UserEnvironmentValue {
    param([string] $Name)

    # No HKCU: drive (not Windows) reads as no value, like a missing key.
    $hive = Get-Item -LiteralPath 'HKCU:' -ErrorAction SilentlyContinue
    if ($null -eq $hive) {
        return $null
    }
    $key = $hive.OpenSubKey('Environment')
    if ($null -eq $key) {
        return $null
    }
    return $key.GetValue($Name, $null, [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
}

function Write-UserEnvironmentValue {
    param(
        [string] $Name,
        [string] $Value
    )

    $key = (Get-Item -LiteralPath 'HKCU:').CreateSubKey('Environment')
    $existing = $key.GetValue($Name, $null, [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
    $kind = if ($Value.Contains('%')) {
        [Microsoft.Win32.RegistryValueKind]::ExpandString
    }
    elseif ($null -ne $existing) {
        $key.GetValueKind($Name)
    }
    else {
        [Microsoft.Win32.RegistryValueKind]::String
    }
    $key.SetValue($Name, $Value, $kind)
    Publish-EnvironmentChange
}

# Tell Explorer and processes started from it that the environment changed.
# Consoles that are already open keep their copy, which is why the caller
# still says to restart the terminal.
function Publish-EnvironmentChange {
    if (-not ('Win32.NativeMethods' -as [Type])) {
        Add-Type -Namespace Win32 -Name NativeMethods -MemberDefinition @'
[DllImport("user32.dll", SetLastError = true, CharSet = CharSet.Auto)]
public static extern IntPtr SendMessageTimeout(
    IntPtr hWnd, uint Msg, UIntPtr wParam, string lParam,
    uint fuFlags, uint uTimeout, out UIntPtr lpdwResult);
'@
    }

    $HWND_BROADCAST = [IntPtr] 0xffff
    $WM_SETTINGCHANGE = 0x1a
    $SMTO_ABORTIFHUNG = 2
    $result = [UIntPtr]::Zero
    [Win32.NativeMethods]::SendMessageTimeout(
        $HWND_BROADCAST, $WM_SETTINGCHANGE, [UIntPtr]::Zero, 'Environment',
        $SMTO_ABORTIFHUNG, 5000, [ref] $result
    ) | Out-Null
}

# Puts $Directory first in the named user environment variable, read and
# written as stored. Returns whether the value changed.
function Add-UserPathEntry {
    param(
        [string] $Name,
        [string] $Directory
    )

    $value = Get-UserEnvironmentValue -Name $Name
    if (Test-PathIsFirst -PathValue $value -Directory $Directory) {
        return $false
    }
    Write-UserEnvironmentValue -Name $Name -Value (Get-PathWithDirectoryFirst -PathValue $value -Directory $Directory)
    return $true
}

function Add-ToUserPath {
    param([string] $Directory)

    $userPathChanged = Add-UserPathEntry -Name 'Path' -Directory $Directory

    # Make the command available immediately when install.ps1 is evaluated
    # in the caller's current PowerShell process. Do this before Get-Command
    # "entire", which caches the first Application hit for the session.
    if (-not (Test-PathIsFirst -PathValue $env:Path -Directory $Directory)) {
        $env:Path = Get-PathWithDirectoryFirst -PathValue $env:Path -Directory $Directory
    }

    return $userPathChanged
}

function Get-EntireOnPath {
    # -All is required: without it Get-Command returns only the first
    # Application, so a later Scoop shim is never seen.
    # -NoEnumerate returns the array itself, so a single element keeps its
    # .Count under StrictMode 2.0. Callers must assign the result before
    # piping it: a -NoEnumerate array reaches a pipeline as one object on
    # pwsh and as its items on Windows PowerShell 5.1.
    $results = @(Get-Command "entire" -CommandType Application -All -ErrorAction SilentlyContinue)
    Write-Output -NoEnumerate -InputObject $results
}

# Every entire.exe that will compete for the name once the user opens a new
# terminal: the copies this process can see, in Get-Command order so the
# priority verdict is unchanged (Active = $true), followed by copies that
# are only on the stored user or machine PATH (Active = $false). A shell
# started before an earlier run wrote the registry cannot see that run's
# directory through $env:Path, and the installer tells the user to restart
# between installs.
function Get-EntireCopy {
    $found = @()
    foreach ($command in (Get-EntireOnPath)) {
        $found += [pscustomobject]@{ Source = $command.Source; Active = $true }
    }

    $stored = @(
        (Get-UserEnvironmentValue -Name 'Path'),
        [Environment]::GetEnvironmentVariable('Path', 'Machine')
    )

    foreach ($value in $stored) {
        if ([string]::IsNullOrWhiteSpace($value)) {
            continue
        }
        foreach ($entry in ($value -split ';')) {
            $directory = [Environment]::ExpandEnvironmentVariables($entry.Trim())
            if ([string]::IsNullOrWhiteSpace($directory)) {
                continue
            }
            $candidate = Join-Path $directory 'entire.exe'
            if (-not (Test-Path -LiteralPath $candidate -PathType Leaf)) {
                continue
            }
            $known = $false
            foreach ($existing in $found) {
                if (Test-SamePath -Left $existing.Source -Right $candidate) {
                    $known = $true
                    break
                }
            }
            if (-not $known) {
                $found += [pscustomobject]@{ Source = $candidate; Active = $false }
            }
        }
    }
    # Same contract as Get-EntireOnPath: assign before piping.
    Write-Output -NoEnumerate -InputObject $found
}

# The "Also found" lines read as a ranking, and only the copies this shell
# can run are ranked: the ones known from the stored PATH alone will compete
# after a restart, not now, so they go under their own heading.
function Write-OtherCopyReport {
    param(
        [object[]] $Copies,
        [string] $Indent
    )

    foreach ($copy in ($Copies | Where-Object { $_.Active })) {
        Write-Host "! Also found:$Indent$($copy.Source)"
    }
    $inactive = @($Copies | Where-Object { -not $_.Active })
    if ($inactive.Count -gt 0) {
        Write-Host "! Also on your saved PATH, not active in this shell:"
        foreach ($copy in $inactive) {
            Write-Host "!   $($copy.Source)"
        }
    }
}

function Install-Entire {
    # Set here rather than at script scope: preference variables assigned
    # in a function are local to it and its callees, and Set-StrictMode
    # applies to the current scope and its children, so under irm | iex
    # none of this reaches the caller's session.
    Set-StrictMode -Version 2.0
    $ErrorActionPreference = "Stop"
    $ProgressPreference = "SilentlyContinue"

    if ($Help) {
        Write-Usage
        return
    }

    Assert-Prerequisite

    Write-Info "Installing Entire CLI..."

    $scoopCommand = Get-Command "scoop" -ErrorAction SilentlyContinue
    if ($Channel -eq "stable" -and $null -ne $scoopCommand -and -not $InstallDirExplicit) {
        # Scoop owns the install location and the shims directory it puts on
        # PATH, so -NoPathUpdate does not apply on this branch, and a caller
        # who chose a directory is sent to the archive install instead (the
        # update hint names the running binary's directory so that binary is
        # replaced in place). Write-Usage says so; keep the two in step.
        Install-EntireWithScoop

        $prefixResult = Invoke-Scoop prefix entire
        if ($prefixResult.ExitCode -ne 0) {
            throw "Scoop installed Entire CLI, but its installation path could not be resolved."
        }
        $scoopPrefix = ($prefixResult.Output | Select-Object -First 1).ToString().Trim()
        $scoopRoot = Split-Path -Parent (Split-Path -Parent (Split-Path -Parent $scoopPrefix))
        $scoopShim = Join-Path $scoopRoot "shims\entire.exe"

        # The verdict is about what this shell runs; the list is about every
        # copy the user has, including ones this shell cannot see yet. Both
        # are assigned before being piped (see the archive branch).
        $pathCommands = Get-EntireOnPath
        $copies = Get-EntireCopy
        $first = $pathCommands | Select-Object -First 1
        if ($null -eq $first -or -not (Test-SamePath -Left $first.Source -Right $scoopShim)) {
            Write-Host ""
            Write-Host "! WARNING: PATH conflict detected" -ForegroundColor Yellow
            Write-Host "!"
            Write-Host "! Scoop shim: $scoopShim"
            if ($null -eq $first) {
                Write-Host "! 'entire' does not resolve to an executable on PATH."
            }
            else {
                Write-Host "! 'entire' currently resolves to: $($first.Source)"
            }
            Write-Host "! Remove the old installation or adjust PATH to prioritize:"
            Write-Host "!   $(Split-Path -Parent $scoopShim)"
            Write-Host ""
            throw "Scoop installed Entire CLI, but its shim does not take priority on PATH."
        }

        $conflicting = @($copies | Where-Object { -not (Test-SamePath -Left $_.Source -Right $scoopShim) })
        if ($conflicting.Count -gt 0) {
            Write-Host ""
            Write-Host "! WARNING: Other Entire CLI installations remain on PATH" -ForegroundColor Yellow
            Write-OtherCopyReport -Copies $conflicting -Indent ' '
            Write-Host "! The Scoop shim takes priority, but consider removing the other installation."
            Write-Host ""
        }
        return
    }
    if ($Channel -eq "nightly" -and $null -ne $scoopCommand) {
        Write-InstallerWarning "Scoop only publishes stable releases; installing nightly from the verified release archive."
    }
    elseif ($null -ne $scoopCommand) {
        Write-Info "Scoop detected, but -InstallDir was given; installing from the verified release archive."
    }

    $resolvedInstallDir = Get-NormalizedPath -Path $InstallDir
    $installPath = Join-Path $resolvedInstallDir "entire.exe"

    # GitHub requires TLS 1.2. Use the numeric value so this remains valid
    # on .NET versions whose SecurityProtocolType enum omits the name.
    [Net.ServicePointManager]::SecurityProtocol =
        [Net.ServicePointManager]::SecurityProtocol -bor 3072

    $architecture = Get-PlatformArchitecture
    Write-Info "Detected platform: windows/$architecture"

    Write-Info "Fetching latest $Channel version..."
    $version = Get-ReleaseVersion -ReleaseChannel $Channel
    Write-Info "Installing version: $version"

    $archiveName = "entire_windows_$architecture.zip"
    $releaseBaseUrl = "https://github.com/$GitHubRepo/releases/download/v$version"
    $downloadUrl = "$releaseBaseUrl/$archiveName"
    $checksumsUrl = "$releaseBaseUrl/checksums.txt"

    $tempDir = Join-Path ([IO.Path]::GetTempPath()) ("entire-install-" + [guid]::NewGuid().ToString("N"))
    [IO.Directory]::CreateDirectory($tempDir) | Out-Null

    try {
        $archivePath = Join-Path $tempDir $archiveName
        $checksumsPath = Join-Path $tempDir "checksums.txt"
        $extractDir = Join-Path $tempDir "extracted"

        Write-Info "Downloading $archiveName..."
        Save-RemoteFile -Uri $downloadUrl -Destination $archivePath

        Write-Info "Downloading checksums..."
        Save-RemoteFile -Uri $checksumsUrl -Destination $checksumsPath

        Write-Info "Verifying checksum..."
        Assert-Checksum -ArchivePath $archivePath -ArchiveName $archiveName -ChecksumsPath $checksumsPath
        Write-Success "Checksum verified"

        Write-Info "Extracting..."
        Expand-Archive -LiteralPath $archivePath -DestinationPath $extractDir -Force

        $sourceBinary = Join-Path $extractDir "entire.exe"
        $sourceHelper = Join-Path $extractDir "git-remote-entire.exe"
        if (-not (Test-Path -LiteralPath $sourceBinary -PathType Leaf)) {
            throw "entire.exe was not found in $archiveName."
        }

        Write-Info "Installing to $resolvedInstallDir..."
        Initialize-InstallDirectory -Path $resolvedInstallDir

        Install-BinaryFile -Source $sourceBinary -Destination $installPath

        if (Test-Path -LiteralPath $sourceHelper -PathType Leaf) {
            Install-BinaryFile -Source $sourceHelper -Destination (Join-Path $resolvedInstallDir "git-remote-entire.exe")
        }
        else {
            Write-InstallerWarning "git-remote-entire.exe was not found in the archive; entire:// clones will not work until the next release includes it."
        }

        & $installPath version *> $null
        if ($LASTEXITCODE -ne 0) {
            throw "Installation completed, but entire.exe failed to execute."
        }
        Write-Success "Entire CLI installed to $installPath"

        # Prepend PATH before Get-Command "entire". Checking first throws on
        # the documented nightly path (Scoop stable already installed) and
        # never updates PATH, so a rerun fails the same way.
        $userPathChanged = $false
        if (-not $NoPathUpdate) {
            $userPathChanged = Add-ToUserPath -Directory $resolvedInstallDir
        }

        # The verdict is about what this shell runs; the list is about every
        # copy the user has, including ones this shell cannot see yet. Both
        # are assigned before being piped: their -NoEnumerate output reaches a
        # pipeline as one array on pwsh, and as its items on 5.1.
        $pathCommands = Get-EntireOnPath
        $copies = Get-EntireCopy
        $conflicting = @($copies | Where-Object { -not (Test-SamePath -Left $_.Source -Right $installPath) })
        if ($conflicting.Count -gt 0) {
            # $first can be empty: with -NoPathUpdate nothing put the new
            # install on this shell's PATH, while stored copies still list.
            $first = $pathCommands | Select-Object -First 1
            $firstIsOurs = ($null -ne $first) -and (Test-SamePath -Left $first.Source -Right $installPath)
            Write-Host ""
            Write-Host "! WARNING: PATH conflict detected" -ForegroundColor Yellow
            Write-Host "!"
            Write-Host "! Installed to: $installPath"
            Write-OtherCopyReport -Copies $conflicting -Indent '   '

            # Our PATH write only ever lands in the user half, and Windows
            # composes a new session as machine-then-user. So a conflict on
            # the machine PATH outranks this install in every new terminal,
            # whatever the current session resolves to -- say so, because
            # neither the verdict below nor reordering the user PATH can fix
            # it.
            $machinePath = [Environment]::GetEnvironmentVariable("Path", "Machine")
            $machineConflict = $false
            foreach ($cmd in $conflicting) {
                if (Test-PathContains -PathValue $machinePath -Directory (Split-Path -Parent $cmd.Source)) {
                    $machineConflict = $true
                    Write-Host "!"
                    Write-Host "! $($cmd.Source) is on the machine-wide PATH, which Windows places"
                    Write-Host "! ahead of your user PATH, so it wins in a new terminal however your"
                    Write-Host "! user PATH is ordered. Remove that copy, or drop its directory from"
                    Write-Host "! the machine PATH (needs an administrator). To install over it"
                    Write-Host "! instead, rerun with:"
                    Write-Host "!   -InstallDir $(Split-Path -Parent $cmd.Source)"
                }
            }

            if (-not $firstIsOurs) {
                Write-Host "!"
                if ($null -eq $first) {
                    Write-Host "! 'entire' does not resolve to an executable on this shell's PATH."
                }
                else {
                    Write-Host "! 'entire' currently resolves to: $($first.Source)"
                }
                Write-Host "! Remove the old installation or adjust PATH to prioritize:"
                Write-Host "!   $resolvedInstallDir"
                Write-Host ""
                if ($NoPathUpdate) {
                    throw "Installation completed, but PATH was not updated (-NoPathUpdate)."
                }
                throw "Installation completed, but PATH needs adjustment."
            }
            # Suppressed on a machine-PATH conflict: the paragraph above has
            # already said that install wins in a new terminal, and claiming
            # priority as well leaves the reader nothing to act on.
            if (-not $machineConflict) {
                $others = if ($conflicting.Count -gt 1) { "the other installations" } else { "the other installation" }
                Write-Host "!"
                Write-Host "! The installed version takes priority, but consider removing"
                Write-Host "! $others to avoid confusion."
            }
            Write-Host ""
        }

        if ($NoPathUpdate) {
            if (-not (Test-PathContains -PathValue $env:Path -Directory $resolvedInstallDir)) {
                Write-InstallerWarning "$resolvedInstallDir is not on PATH. Add it before running entire."
            }
        }
        elseif ($userPathChanged) {
            Write-Success "Added $resolvedInstallDir to your user PATH"
            Write-Host "Restart your terminal, then run entire to get started."
        }
    }
    finally {
        Remove-Item -LiteralPath $tempDir -Recurse -Force -ErrorAction SilentlyContinue
    }
}

# Dot-sourcing loads the functions and nothing else, so tests can call
# them. Every other invocation installs: -File, `.\install.ps1`, `& path`,
# `irm | iex`, and the scriptblock form.
if ($MyInvocation.InvocationName -ne '.') {
    # MyCommand.Path is empty under irm | iex and the scriptblock form, and
    # set whenever the script runs from its file.
    $invokedFromFile = -not [string]::IsNullOrEmpty($MyInvocation.MyCommand.Path)
    try {
        Install-Entire
    }
    catch {
        $message = "Error: $($_.Exception.Message)"
        if ($invokedFromFile) {
            Write-Host $message -ForegroundColor Red
            exit 1
        }
        # irm | iex: throw without Write-Host so the message appears once, and
        # do not exit 1 -- that would close the user's interactive shell.
        throw $message
    }
}
