#Requires -Version 5.1

# Runs scripts/install.ps1 for real against GitHub Releases and checks what it
# leaves behind. It rewrites the user PATH and installs binaries under the
# profile, so it refuses to run anywhere but GitHub Actions; the raw user PATH
# and its value kind are restored at the end regardless.
#
# Phases, each asserting before the next:
#   1. stable install through the documented `irm | iex` shape
#   2. reinstall while entire.exe is running, then again after it exits
#   3. nightly install into a second directory, alongside the first

# Phase 1 runs the installer through the documented `irm | iex` shape, which
# is the point of that phase.
[Diagnostics.CodeAnalysis.SuppressMessageAttribute('PSAvoidUsingInvokeExpression', '', Justification = 'Phase 1 exercises the documented irm | iex shape')]
param()

$ErrorActionPreference = 'Stop'

if ($env:CI -ne 'true' -or $env:GITHUB_ACTIONS -ne 'true') {
    throw 'scripts/test/e2e.ps1 installs into the user profile and rewrites the user PATH; it runs only in GitHub Actions CI.'
}

$installerPath = Join-Path (Split-Path -Parent $PSScriptRoot) 'install.ps1'
# Dot-sourcing loads the functions without installing.
. $installerPath

function Assert-Equal {
    param([string] $Phase, [string] $What, $Expected, $Actual)
    if (-not [object]::Equals($Expected, $Actual)) {
        throw "[$Phase] $What`n  expected: $Expected`n  actual:   $Actual"
    }
}

function Assert-True {
    param([string] $Phase, [string] $What, [bool] $Condition)
    if (-not $Condition) {
        throw "[$Phase] $What"
    }
}

function Get-PathEntry {
    param([AllowNull()] [string] $Value)
    if ([string]::IsNullOrEmpty($Value)) {
        return @()
    }
    return @($Value -split ';')
}

function Get-UserPathKind {
    $key = (Get-Item -LiteralPath 'HKCU:').OpenSubKey('Environment')
    if ($null -eq $key -or $null -eq $key.GetValue('Path', $null, [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)) {
        return $null
    }
    return $key.GetValueKind('Path')
}

# The kind Write-UserEnvironmentValue promises: ExpandString when the value
# contains %, otherwise whatever the value had before, String if it did not
# exist.
function Get-ExpectedKind {
    param([string] $NewValue, $PreviousKind)
    if ($NewValue.Contains('%')) {
        return [Microsoft.Win32.RegistryValueKind]::ExpandString
    }
    if ($null -ne $PreviousKind) {
        return $PreviousKind
    }
    return [Microsoft.Win32.RegistryValueKind]::String
}

# The machine PATH as stored, for the phase that plants a copy on it.
function Get-MachinePathKey {
    (Get-Item -LiteralPath 'HKLM:\SYSTEM\CurrentControlSet\Control\Session Manager').OpenSubKey('Environment', $true)
}

function Get-MachinePath {
    (Get-MachinePathKey).GetValue('Path', $null, [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
}

# Runs $Body in a child of the same shell, so each CI leg tests its own host.
function Invoke-ChildShell {
    param([string] $Body)
    $shell = (Get-Process -Id $PID).Path
    @(& $shell -NoProfile -NonInteractive -Command $Body 2>&1 | ForEach-Object { "$_" })
}

function Invoke-Installer {
    # A hashtable splat binds by name; an array splat would bind positionally.
    param([hashtable] $Arguments = @{})
    $installer = [scriptblock]::Create((Get-Content -Raw -LiteralPath $installerPath))
    # Write-Host is the information stream; merge it so the report can be asserted.
    & $installer @Arguments 6>&1 | ForEach-Object { "$_" }
}

function Get-EntireVersion {
    param([string] $Phase, [string] $Directory)
    $output = & (Join-Path $Directory 'entire.exe') version 2>&1 | Out-String
    Assert-Equal -Phase $Phase -What 'entire.exe version exit code' -Expected 0 -Actual $LASTEXITCODE
    return $output.Trim()
}

$snapshot = Get-UserEnvironmentValue -Name 'Path'
$snapshotKind = Get-UserPathKind
$snapshotEntries = Get-PathEntry $snapshot
# This process's PATH before any install: what a terminal opened before the
# first install would inherit.
$processPathBefore = $env:Path
$machineSnapshot = Get-MachinePath
$machineSnapshotKind = (Get-MachinePathKey).GetValueKind('Path')
$stubDir = Join-Path $env:TEMP 'entire-machine-stub'
$stableDir = Join-Path $env:USERPROFILE '.local\bin'
$nightlyDir = Join-Path $env:USERPROFILE 'entire-nightly'
$running = $null

try {
    # ---- Phase 1: stable through irm | iex --------------------------------
    $phase = 'phase 1: stable via iex'
    Write-Host "==> $phase"
    Get-Content -Raw -LiteralPath $installerPath | Invoke-Expression

    Assert-True -Phase $phase -What 'entire.exe installed' -Condition (Test-Path -LiteralPath (Join-Path $stableDir 'entire.exe') -PathType Leaf)
    Assert-True -Phase $phase -What 'git-remote-entire.exe installed' -Condition (Test-Path -LiteralPath (Join-Path $stableDir 'git-remote-entire.exe') -PathType Leaf)

    $afterStable = Get-UserEnvironmentValue -Name 'Path'
    $entries = Get-PathEntry $afterStable
    Assert-Equal -Phase $phase -What 'first raw PATH entry' -Expected $stableDir -Actual $entries[0]
    Assert-Equal -Phase $phase -What 'remaining raw PATH entries' -Expected ($snapshotEntries -join ';') -Actual (($entries | Select-Object -Skip 1) -join ';')
    Assert-Equal -Phase $phase -What 'raw PATH value kind' -Expected (Get-ExpectedKind -NewValue $afterStable -PreviousKind $snapshotKind) -Actual (Get-UserPathKind)
    Assert-Equal -Phase $phase -What 'first in-process $env:Path entry' -Expected $stableDir -Actual ((Get-PathEntry $env:Path)[0])
    $stableVersion = Get-EntireVersion -Phase $phase -Directory $stableDir
    Write-Host "    stable: $stableVersion"

    # ---- Phase 2: reinstall while entire.exe is running -------------------
    $phase = 'phase 2: reinstall while running'
    Write-Host "==> $phase"
    $startInfo = New-Object System.Diagnostics.ProcessStartInfo
    $startInfo.FileName = Join-Path $stableDir 'entire.exe'
    $startInfo.Arguments = 'mcp'
    $startInfo.UseShellExecute = $false
    $startInfo.RedirectStandardInput = $true
    $startInfo.RedirectStandardOutput = $true
    # stdin is never written to or closed, so the MCP server waits on it.
    $running = [System.Diagnostics.Process]::Start($startInfo)
    Start-Sleep -Seconds 2
    Assert-True -Phase $phase -What 'entire mcp is still running' -Condition (-not $running.HasExited)

    # A mapped executable image refuses to be opened for writing. Prove that
    # without writing anything: if the open succeeds, the file is untouched
    # and the precondition, not a later phase, is what fails.
    $opened = $false
    try {
        $handle = [IO.File]::Open((Join-Path $stableDir 'entire.exe'), [IO.FileMode]::Open, [IO.FileAccess]::Write)
        $handle.Dispose()
        $opened = $true
    }
    catch {
        $opened = $false
    }
    Assert-True -Phase $phase -What 'running entire.exe refuses a write handle (precondition)' -Condition (-not $opened)

    Invoke-Installer | Out-Null
    Assert-True -Phase $phase -What 'entire.exe.old left behind while the old image runs' -Condition (Test-Path -LiteralPath (Join-Path $stableDir 'entire.exe.old') -PathType Leaf)
    Get-EntireVersion -Phase $phase -Directory $stableDir | Out-Null
    Assert-Equal -Phase $phase -What 'raw PATH after reinstall' -Expected $afterStable -Actual (Get-UserEnvironmentValue -Name 'Path')

    $running.Kill()
    $running.WaitForExit()
    $running = $null
    Invoke-Installer | Out-Null
    $stale = @(Get-ChildItem -LiteralPath $stableDir -Filter 'entire.exe*.old' -File)
    Assert-Equal -Phase $phase -What 'stale .old files after the old image exited' -Expected 0 -Actual $stale.Count
    Assert-Equal -Phase $phase -What 'raw PATH after the third install' -Expected $afterStable -Actual (Get-UserEnvironmentValue -Name 'Path')

    # ---- Phase 3: nightly into a second directory, from a stale shell ------
    # The installer tells the user to restart the terminal between installs.
    # A terminal opened before phase 1 wrote the registry has a PATH without
    # the stable directory, so this install runs in a child given exactly that
    # PATH; the stable copy must then be reported from the stored PATH alone.
    $phase = 'phase 3: nightly to a custom dir from a stale shell'
    Write-Host "==> $phase"
    $childBody = @(
        "`$env:Path = '$processPathBefore'"
        "if (@(`$env:Path -split ';' | Where-Object { `$_ -eq '$stableDir' }).Count -eq 0) { 'PRECONDITION: stable dir absent from this shell' }"
        "& ([scriptblock]::Create((Get-Content -Raw -LiteralPath '$installerPath'))) -Channel nightly -InstallDir '$nightlyDir' 6>&1 | ForEach-Object { `"`$_`" }"
    ) -join '; '
    $reportLines = Invoke-ChildShell -Body $childBody
    $report = $reportLines -join "`n"
    # The assertions below judge this text; show it so a failure is diagnosable from the log.
    Write-Host $report

    Assert-True -Phase $phase -What 'child shell PATH lacked the stable dir (precondition)' -Condition ($reportLines -contains 'PRECONDITION: stable dir absent from this shell')
    Assert-True -Phase $phase -What 'conflict warning printed' -Condition ($report -match '! WARNING: PATH conflict detected')
    Assert-True -Phase $phase -What 'stored-PATH group printed' -Condition ($reportLines -contains '! Also on your saved PATH, not active in this shell:')
    Assert-True -Phase $phase -What 'stable install named under the stored-PATH group' -Condition ($reportLines -contains "!   $(Join-Path $stableDir 'entire.exe')")
    Assert-True -Phase $phase -What 'nightly install reported as taking priority' -Condition ($report -match 'The installed version takes priority')

    $afterNightly = Get-UserEnvironmentValue -Name 'Path'
    $entries = Get-PathEntry $afterNightly
    Assert-Equal -Phase $phase -What 'first raw PATH entry' -Expected $nightlyDir -Actual $entries[0]
    Assert-Equal -Phase $phase -What 'second raw PATH entry (stable, moved down, not removed)' -Expected $stableDir -Actual $entries[1]
    Assert-Equal -Phase $phase -What 'remaining raw PATH entries' -Expected ($snapshotEntries -join ';') -Actual (($entries | Select-Object -Skip 2) -join ';')
    Assert-Equal -Phase $phase -What 'raw PATH value kind' -Expected (Get-ExpectedKind -NewValue $afterNightly -PreviousKind $snapshotKind) -Actual (Get-UserPathKind)
    $nightlyVersion = Get-EntireVersion -Phase $phase -Directory $nightlyDir
    Write-Host "    nightly: $nightlyVersion"
    Assert-True -Phase $phase -What 'nightly binary reports a nightly version' -Condition ($nightlyVersion -match 'nightly')
    Assert-True -Phase $phase -What 'nightly and stable versions differ' -Condition ($nightlyVersion -ne $stableVersion)

    # ---- Phase 4: a copy on the machine-wide PATH ---------------------------
    # Windows composes a new session as machine PATH then user PATH, so a copy
    # in a machine-PATH directory outranks anything the installer writes; the
    # report has a paragraph for that. The runner is elevated, so plant one.
    $phase = 'phase 4: copy on the machine PATH'
    Write-Host "==> $phase"
    [IO.Directory]::CreateDirectory($stubDir) | Out-Null
    Copy-Item -LiteralPath $env:ComSpec -Destination (Join-Path $stubDir 'entire.exe')
    (Get-MachinePathKey).SetValue('Path', "$machineSnapshot;$stubDir", $machineSnapshotKind)
    $report = Invoke-Installer | Out-String
    Write-Host $report

    Assert-True -Phase $phase -What 'machine-PATH paragraph printed' -Condition ($report -match 'is on the machine-wide PATH')
    Assert-True -Phase $phase -What 'machine-PATH copy named' -Condition ($report.Contains("$(Join-Path $stubDir 'entire.exe') is on the machine-wide PATH"))
    Assert-True -Phase $phase -What 'install-over remedy names the machine-PATH directory' -Condition ($report.Contains("!   -InstallDir $stubDir"))
    # Reinstalling stable puts its directory first again; the nightly entry
    # moves down by one and nothing else changes.
    $entries = Get-PathEntry (Get-UserEnvironmentValue -Name 'Path')
    Assert-Equal -Phase $phase -What 'first raw PATH entry' -Expected $stableDir -Actual $entries[0]
    Assert-Equal -Phase $phase -What 'second raw PATH entry' -Expected $nightlyDir -Actual $entries[1]
    Assert-Equal -Phase $phase -What 'remaining raw PATH entries' -Expected ($snapshotEntries -join ';') -Actual (($entries | Select-Object -Skip 2) -join ';')

    Write-Host '==> all phases passed'
}
finally {
    if ($null -ne $running -and -not $running.HasExited) {
        $running.Kill()
        $running.WaitForExit()
    }
    # The runner is ephemeral; restoring anyway records that this script
    # knows it mutated shared state.
    if ($null -ne $machineSnapshot) {
        (Get-MachinePathKey).SetValue('Path', $machineSnapshot, $machineSnapshotKind)
    }
    Remove-Item -LiteralPath $stubDir -Recurse -Force -ErrorAction SilentlyContinue
    $key = (Get-Item -LiteralPath 'HKCU:').CreateSubKey('Environment')
    if ($null -eq $snapshot) {
        $key.DeleteValue('Path', $false)
    }
    else {
        $key.SetValue('Path', $snapshot, $snapshotKind)
    }
}
