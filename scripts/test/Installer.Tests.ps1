# One case runs the installer through the documented `iex "& {...}"` shape,
# which is the point of that case.
[Diagnostics.CodeAnalysis.SuppressMessageAttribute('PSAvoidUsingInvokeExpression', '', Justification = 'Pins the documented iex one-liner shape')]
param()

Describe 'install.ps1' {
    BeforeAll {
        function Get-InstallerPath {
            Join-Path (Split-Path -Parent $PSScriptRoot) 'install.ps1'
        }

        # Runs $Body in a child process of the same shell, so the Windows
        # PowerShell 5.1 and pwsh CI steps each test their own host.
        #
        # The child's PATH is cut down to the shell's own directory. The
        # installer branches on `Get-Command scoop` before it does anything
        # else, and with Scoop resolvable a stable-channel run performs a real
        # `scoop bucket add` and `scoop install`. Scrubbing PATH pins every case
        # here to the direct-install branch whatever the host has installed; the
        # child reports the resolution so the scrub is asserted, not assumed.
        #
        # The child also shadows Invoke-RestMethod with a function that throws:
        # command resolution prefers functions over cmdlets, so the direct-install
        # branch's first network call fails with the sentinel and nothing is
        # downloaded.
        function Invoke-InstallerChild {
            param([string] $Body)
            $shell = (Get-Process -Id $PID).Path
            $preamble = @(
                "`$env:PATH = '$(Split-Path -Parent $shell)'"
                "'SCOOP:' + [bool](Get-Command scoop -ErrorAction SilentlyContinue)"
                "function Invoke-RestMethod { throw 'entire-test: network blocked' }"
            ) -join '; '
            $output = @(& $shell -NoProfile -NonInteractive -Command "$preamble; $Body" 2>&1 | ForEach-Object { "$_" })
            [pscustomobject]@{ Output = $output; ExitCode = $LASTEXITCODE }
        }

        # On Windows the run reaches the direct-install branch and fails at the
        # stubbed network call; elsewhere the OS guard refuses first, so the
        # message that proves the installer ran is the guard's.
        function Assert-DirectInstallBranch {
            param([pscustomobject] $Run)
            $Run.Output | Should -Contain 'SCOOP:False'
            if ($env:OS -eq 'Windows_NT') {
                $Run.Output | Should -Contain '==> Installing Entire CLI...'
            }
        }

        function Assert-ExpectedFailure {
            param([string[]] $Output, [string] $Prefix)
            if ($env:OS -eq 'Windows_NT') {
                # Invoke-GitHubApi wraps the stub's message, so match inside the line.
                @($Output | Where-Object { $_ -like "${Prefix}Error: *entire-test: network blocked*" }) | Should -HaveCount 1
            }
            else {
                @($Output | Where-Object { $_ -like "${Prefix}Error: install.ps1 supports Windows only.*" }) | Should -HaveCount 1
            }
        }

        # The installer's own `exit 1` must not be reached when it runs inside
        # a live session, so the child continues past it and ends with its own
        # exit code: seeing 7 is the proof that the session survived.
        function Assert-ThrowsWithoutExiting {
            param([pscustomobject] $Run)
            Assert-DirectInstallBranch -Run $Run
            @($Run.Output | Where-Object { $_ -like 'CAUGHT:Error: *' }) | Should -HaveCount 1
            @($Run.Output | Where-Object { $_ -like 'Error: *' }) | Should -HaveCount 0
            Assert-ExpectedFailure -Output $Run.Output -Prefix 'CAUGHT:'
            $Run.Output | Should -Contain 'ALIVE'
            $Run.ExitCode | Should -Be 7
        }

        function Assert-NoExceptionFormatting {
            param([string[]] $Output)
            @($Output | Where-Object { $_ -like 'Error: *' }) | Should -HaveCount 1
            @($Output | Where-Object { $_ -match 'At line:|CategoryInfo|FullyQualifiedErrorId' }) | Should -HaveCount 0
        }
    }

    Context 'documented invocation shapes' {
        It 'runs as a scriptblock with -Help' {
            $installer = [scriptblock]::Create((Get-Content -Raw (Get-InstallerPath)))
            $output = @(& $installer -Help 6>&1 | ForEach-Object { "$_" })
            $output[0] | Should -BeLike 'Usage: install.ps1*'
            ($output -join "`n") | Should -BeLike '*manages its own PATH entry.*An explicit -InstallDir selects a release-archive install*'
        }

        It 'binds -Channel nightly through the scriptblock form' {
            $installer = [scriptblock]::Create((Get-Content -Raw (Get-InstallerPath)))
            $output = @(& $installer -Channel nightly -Help 6>&1 | ForEach-Object { "$_" })
            $output[0] | Should -BeLike 'Usage: install.ps1*'
        }

        # The README's nightly one-liner, with the download replaced by a file
        # read: the fetched text is wrapped in braces and called with arguments.
        It 'binds arguments through the documented iex "& {...}" form' {
            $path = Get-InstallerPath
            $output = @(Invoke-Expression "& {$(Get-Content -Raw $path)} -Channel nightly -Help" 6>&1 | ForEach-Object { "$_" })
            $output[0] | Should -BeLike 'Usage: install.ps1*'
        }

        It 'loads functions without installing when dot-sourced' {
            $run = Invoke-InstallerChild -Body ". '$(Get-InstallerPath)'; 'INSTALL-ENTIRE:' + [bool](Get-Command Install-Entire -CommandType Function -ErrorAction SilentlyContinue); 'ALIVE'; exit 7"
            $run.Output | Should -Contain 'INSTALL-ENTIRE:True'
            $run.Output | Should -Not -Contain '==> Installing Entire CLI...'
            $run.Output | Should -Contain 'ALIVE'
            $run.ExitCode | Should -Be 7
        }
    }

    Context 'error path' {
        It 'throws and leaves the session running when piped to Invoke-Expression' {
            $run = Invoke-InstallerChild -Body "try { Get-Content -Raw '$(Get-InstallerPath)' | Invoke-Expression } catch { 'CAUGHT:' + `$_.Exception.Message }; 'ALIVE'; exit 7"
            Assert-ThrowsWithoutExiting -Run $run
        }

        It 'throws and leaves the session running when run as a scriptblock with parameters' {
            $installDir = Join-Path $TestDrive 'bin'
            $run = Invoke-InstallerChild -Body "try { & ([scriptblock]::Create((Get-Content -Raw '$(Get-InstallerPath)'))) -InstallDir '$installDir' } catch { 'CAUGHT:' + `$_.Exception.Message }; 'ALIVE'; exit 7"
            Assert-ThrowsWithoutExiting -Run $run
        }

        It 'prints one Error: line and exits 1 when run from a file' {
            $installDir = Join-Path $TestDrive 'bin'
            $run = Invoke-InstallerChild -Body "& '$(Get-InstallerPath)' -InstallDir '$installDir'; exit `$LASTEXITCODE"
            Assert-DirectInstallBranch -Run $run
            Assert-NoExceptionFormatting -Output $run.Output
            Assert-ExpectedFailure -Output $run.Output -Prefix ''
            $run.ExitCode | Should -Be 1
        }

        # The -File process is spawned from inside the scrubbed child so it
        # inherits the cut-down PATH, but it is a fresh process, so the
        # Invoke-RestMethod stub cannot reach it. A drive that does not exist
        # fails in Get-NormalizedPath, which runs before the registry read and
        # the first network call, so on Windows this case asserts the provider
        # message instead of the sentinel. Elsewhere the OS guard refuses first.
        It 'exits the process with 1 when run with -File' {
            $shell = (Get-Process -Id $PID).Path
            $run = Invoke-InstallerChild -Body "& '$shell' -NoProfile -NonInteractive -File '$(Get-InstallerPath)' -InstallDir 'entiretestnodrive:\bin'; exit `$LASTEXITCODE"
            Assert-DirectInstallBranch -Run $run
            Assert-NoExceptionFormatting -Output $run.Output
            if ($env:OS -eq 'Windows_NT') {
                @($run.Output | Where-Object { $_ -like 'Error: *Cannot find drive*entiretestnodrive*' }) | Should -HaveCount 1
            }
            else {
                @($run.Output | Where-Object { $_ -like 'Error: install.ps1 supports Windows only.*' }) | Should -HaveCount 1
            }
            $run.ExitCode | Should -Be 1
        }
    }
}

Describe 'Assert-Prerequisite' {
    BeforeAll {
        . (Join-Path (Split-Path -Parent $PSScriptRoot) 'install.ps1')
    }

    It 'refuses a non-Windows host and points at install.sh' {
        Mock Test-IsWindows { $false }
        { Assert-Prerequisite } | Should -Throw -ExpectedMessage '*supports Windows only*install.sh*'
    }

    It 'refuses PowerShell older than 5.1' {
        Mock Test-IsWindows { $true }
        Mock Test-MinimumPowerShell { $false }
        { Assert-Prerequisite } | Should -Throw -ExpectedMessage '*5.1 or later is required*'
    }

    It 'refuses a session that is not in FullLanguage mode' {
        Mock Test-IsWindows { $true }
        Mock Test-MinimumPowerShell { $true }
        Mock Test-FullLanguageMode { $false }
        { Assert-Prerequisite } | Should -Throw -ExpectedMessage '*FullLanguage mode is required*'
    }

    It 'reports the host before the PowerShell version' {
        Mock Test-IsWindows { $false }
        Mock Test-MinimumPowerShell { $false }
        Mock Test-FullLanguageMode { $false }
        { Assert-Prerequisite } | Should -Throw -ExpectedMessage '*supports Windows only*'
    }

    It 'passes when every prerequisite holds' {
        Mock Test-IsWindows { $true }
        Mock Test-MinimumPowerShell { $true }
        Mock Test-FullLanguageMode { $true }
        { Assert-Prerequisite } | Should -Not -Throw
    }
}

Describe 'PATH entry handling' {
    BeforeAll {
        . (Join-Path (Split-Path -Parent $PSScriptRoot) 'install.ps1')
        # An environment variable this process owns, so %ENTIRE_TEST_ROOT% is
        # an unexpanded entry on every OS.
        $env:ENTIRE_TEST_ROOT = $TestDrive
    }

    AfterAll {
        Remove-Item -Path Env:ENTIRE_TEST_ROOT -ErrorAction SilentlyContinue
    }

    It 'treats an unexpanded entry as the same path as its expansion' {
        Test-SamePath -Left '%ENTIRE_TEST_ROOT%/sub' -Right (Join-Path $TestDrive 'sub') | Should -BeTrue
    }

    It 'adds the directory when it is absent and keeps the rest verbatim' {
        $other = Join-Path $TestDrive 'other'
        $target = Join-Path $TestDrive 'sub'
        Get-PathWithDirectoryFirst -PathValue "$other;" -Directory $target | Should -Be "$target;$other;"
        Get-PathWithDirectoryFirst -PathValue "$other;;$other" -Directory $target | Should -Be "$target;$other;;$other"
        Get-PathWithDirectoryFirst -PathValue '' -Directory $target | Should -Be $target
    }

    It 'moves an unexpanded entry to the front as stored' {
        $other = Join-Path $TestDrive 'other'
        $target = Join-Path $TestDrive 'sub'
        Get-PathWithDirectoryFirst -PathValue "$other;%ENTIRE_TEST_ROOT%/sub" -Directory $target | Should -Be "%ENTIRE_TEST_ROOT%/sub;$other"
    }

    It 'moves a literal entry to the front instead of adding a second one' {
        $other = Join-Path $TestDrive 'other'
        $target = Join-Path $TestDrive 'sub'
        Get-PathWithDirectoryFirst -PathValue "$other;$target" -Directory $target | Should -Be "$target;$other"
    }

    It 'recognises an unexpanded first entry' {
        Test-PathIsFirst -PathValue ";%ENTIRE_TEST_ROOT%/sub;other" -Directory (Join-Path $TestDrive 'sub') | Should -BeTrue
    }
}

Describe 'User environment registry' -Skip:($env:OS -ne 'Windows_NT') {
    BeforeAll {
        . (Join-Path (Split-Path -Parent $PSScriptRoot) 'install.ps1')

        # A throwaway value in the real HKCU:\Environment, named per process
        # and removed in AfterAll. The real Path value is never touched.
        function Get-TestValueName { "ENTIRE_INSTALL_TEST_$PID" }

        function Write-RawTestValue {
            param([string] $Value, [Microsoft.Win32.RegistryValueKind] $Kind)
            $key = (Get-Item -LiteralPath 'HKCU:').OpenSubKey('Environment', $true)
            $key.SetValue((Get-TestValueName), $Value, $Kind)
        }

        function Get-TestValueKind {
            (Get-Item -LiteralPath 'HKCU:').OpenSubKey('Environment').GetValueKind((Get-TestValueName))
        }
    }

    AfterAll {
        $key = (Get-Item -LiteralPath 'HKCU:').OpenSubKey('Environment', $true)
        if ($null -ne $key) {
            $key.DeleteValue("ENTIRE_INSTALL_TEST_$PID", $false)
        }
    }

    It 'stores a value containing % unexpanded, as REG_EXPAND_SZ' {
        Write-UserEnvironmentValue -Name (Get-TestValueName) -Value '%USERPROFILE%\entire-test-a'
        Get-UserEnvironmentValue -Name (Get-TestValueName) | Should -Be '%USERPROFILE%\entire-test-a'
        Get-TestValueKind | Should -Be ([Microsoft.Win32.RegistryValueKind]::ExpandString)
    }

    It 'keeps REG_SZ for a value without %' {
        Write-RawTestValue -Value 'C:\plain' -Kind ([Microsoft.Win32.RegistryValueKind]::String)
        Write-UserEnvironmentValue -Name (Get-TestValueName) -Value 'C:\plain;C:\more'
        Get-TestValueKind | Should -Be ([Microsoft.Win32.RegistryValueKind]::String)
    }

    It 'keeps REG_EXPAND_SZ when the new value has no %' {
        Write-RawTestValue -Value '%USERPROFILE%\x' -Kind ([Microsoft.Win32.RegistryValueKind]::ExpandString)
        Write-UserEnvironmentValue -Name (Get-TestValueName) -Value 'C:\plain'
        Get-TestValueKind | Should -Be ([Microsoft.Win32.RegistryValueKind]::ExpandString)
    }

    It 'leaves an unexpanded first entry alone' {
        Write-RawTestValue -Value '%USERPROFILE%\entire-test-leaf;C:\other' -Kind ([Microsoft.Win32.RegistryValueKind]::ExpandString)
        Add-UserPathEntry -Name (Get-TestValueName) -Directory (Join-Path $env:USERPROFILE 'entire-test-leaf') | Should -BeFalse
        Get-UserEnvironmentValue -Name (Get-TestValueName) | Should -Be '%USERPROFILE%\entire-test-leaf;C:\other'
    }

    It 'moves an unexpanded entry to the front without expanding it' {
        Write-RawTestValue -Value 'C:\other;%USERPROFILE%\entire-test-leaf' -Kind ([Microsoft.Win32.RegistryValueKind]::ExpandString)
        Add-UserPathEntry -Name (Get-TestValueName) -Directory (Join-Path $env:USERPROFILE 'entire-test-leaf') | Should -BeTrue
        Get-UserEnvironmentValue -Name (Get-TestValueName) | Should -Be '%USERPROFILE%\entire-test-leaf;C:\other'
        Get-TestValueKind | Should -Be ([Microsoft.Win32.RegistryValueKind]::ExpandString)
    }

    It 'moves a literal entry to the front' {
        $leaf = Join-Path $env:USERPROFILE 'entire-test-leaf'
        Write-RawTestValue -Value "C:\other;$leaf" -Kind ([Microsoft.Win32.RegistryValueKind]::String)
        Add-UserPathEntry -Name (Get-TestValueName) -Directory $leaf | Should -BeTrue
        Get-UserEnvironmentValue -Name (Get-TestValueName) | Should -Be "$leaf;C:\other"
    }

    It 'broadcasts the change without throwing' {
        { Publish-EnvironmentChange } | Should -Not -Throw
    }
}

Describe 'Install-BinaryFile' {
    BeforeAll {
        . (Join-Path (Split-Path -Parent $PSScriptRoot) 'install.ps1')

        function Write-TestBinary {
            param([string] $Path, [string] $Content)
            [IO.Directory]::CreateDirectory((Split-Path -Parent $Path)) | Out-Null
            Set-Content -LiteralPath $Path -Value $Content -NoNewline
        }

        function Get-OldFile {
            param([string] $Directory)
            @(Get-ChildItem -LiteralPath $Directory -Filter '*.old' -File -ErrorAction SilentlyContinue)
        }
    }

    It 'copies into place when no binary exists yet' {
        $dir = Join-Path $TestDrive 'fresh'
        Write-TestBinary -Path (Join-Path $dir 'src.bin') -Content 'new'
        Install-BinaryFile -Source (Join-Path $dir 'src.bin') -Destination (Join-Path $dir 'entire.exe')
        Get-Content -LiteralPath (Join-Path $dir 'entire.exe') -Raw | Should -Be 'new'
        Get-OldFile -Directory $dir | Should -HaveCount 0
    }

    It 'puts the old binary back when the copy fails' {
        $dir = Join-Path $TestDrive 'rollback'
        $target = Join-Path $dir 'entire.exe'
        Write-TestBinary -Path $target -Content 'old'
        { Install-BinaryFile -Source (Join-Path $dir 'missing.bin') -Destination $target } | Should -Throw
        Get-Content -LiteralPath $target -Raw | Should -Be 'old'
        Get-OldFile -Directory $dir | Should -HaveCount 0
    }

    It 'removes stale .old files before writing' {
        $dir = Join-Path $TestDrive 'sweep'
        Write-TestBinary -Path (Join-Path $dir 'entire.exe.old') -Content 'stale'
        Write-TestBinary -Path (Join-Path $dir 'git-remote-entire.exe.old') -Content 'stale'
        Write-TestBinary -Path (Join-Path $dir 'src.bin') -Content 'new'
        Install-BinaryFile -Source (Join-Path $dir 'src.bin') -Destination (Join-Path $dir 'entire.exe')
        Install-BinaryFile -Source (Join-Path $dir 'src.bin') -Destination (Join-Path $dir 'git-remote-entire.exe')
        Get-OldFile -Directory $dir | Should -HaveCount 0
    }

    It 'treats brackets in the install directory literally' {
        $dir = Join-Path (Join-Path $TestDrive '[x]') 'bin'
        [IO.Directory]::CreateDirectory($dir) | Out-Null
        Write-TestBinary -Path (Join-Path $dir 'src.bin') -Content 'new'
        Install-BinaryFile -Source (Join-Path $dir 'src.bin') -Destination (Join-Path $dir 'entire.exe')
        Test-Path -LiteralPath (Join-Path $dir 'entire.exe') -PathType Leaf | Should -BeTrue
        Test-Path -LiteralPath (Join-Path $TestDrive 'x') | Should -BeFalse
    }

    # Windows will not overwrite or delete a mapped executable image but will
    # rename it; Unix allows all three, so these cases have no meaning there.
    Context 'on Windows' -Skip:($env:OS -ne 'Windows_NT') {
        BeforeAll {
            function Invoke-RunningCopy {
                param([string] $Path)
                Copy-Item -LiteralPath $env:ComSpec -Destination $Path
                Start-Process -FilePath $Path -ArgumentList '/c', 'timeout', '/t', '120', '/nobreak' -WindowStyle Hidden -PassThru
            }

            function Close-RunningCopy {
                param($Process)
                if ($null -ne $Process -and -not $Process.HasExited) {
                    Stop-Process -Id $Process.Id -Force
                    $Process.WaitForExit()
                }
            }
        }

        It 'replaces a running binary and clears the old image once it exits' {
            $dir = Join-Path $TestDrive 'running'
            $target = Join-Path $dir 'entire.exe'
            [IO.Directory]::CreateDirectory($dir) | Out-Null
            Write-TestBinary -Path (Join-Path $dir 'src.bin') -Content 'new'
            $process = Invoke-RunningCopy -Path $target
            try {
                { Copy-Item -LiteralPath (Join-Path $dir 'src.bin') -Destination $target -Force -ErrorAction Stop } | Should -Throw
                Install-BinaryFile -Source (Join-Path $dir 'src.bin') -Destination $target
                Get-Content -LiteralPath $target -Raw | Should -Be 'new'
                Test-Path -LiteralPath "$target.old" -PathType Leaf | Should -BeTrue
            }
            finally {
                Close-RunningCopy -Process $process
            }
            Install-BinaryFile -Source (Join-Path $dir 'src.bin') -Destination $target
            Get-OldFile -Directory $dir | Should -HaveCount 0
        }

        It 'names the remedy when another program holds the binary open' {
            $dir = Join-Path $TestDrive 'held'
            $target = Join-Path $dir 'entire.exe'
            Write-TestBinary -Path $target -Content 'old'
            Write-TestBinary -Path (Join-Path $dir 'src.bin') -Content 'new'
            $handle = [IO.File]::Open($target, [IO.FileMode]::Open, [IO.FileAccess]::Read, [IO.FileShare]::None)
            try {
                { Install-BinaryFile -Source (Join-Path $dir 'src.bin') -Destination $target } | Should -Throw -ExpectedMessage '*another program holds it open*'
            }
            finally {
                $handle.Dispose()
            }
            Get-Content -LiteralPath $target -Raw | Should -Be 'old'
            Get-OldFile -Directory $dir | Should -HaveCount 0
        }

        It 'moves aside under a unique name when the previous .old is still running' {
            $dir = Join-Path $TestDrive 'twice'
            $target = Join-Path $dir 'entire.exe'
            [IO.Directory]::CreateDirectory($dir) | Out-Null
            Write-TestBinary -Path (Join-Path $dir 'src.bin') -Content 'new'
            # A running .old arises the way the installer makes it: the image
            # is started as entire.exe and then renamed while running. Windows
            # will not start a process from a file named entire.exe.old.
            $old = Invoke-RunningCopy -Path $target
            Move-Item -LiteralPath $target -Destination "$target.old"
            $current = Invoke-RunningCopy -Path $target
            try {
                Install-BinaryFile -Source (Join-Path $dir 'src.bin') -Destination $target
                Get-Content -LiteralPath $target -Raw | Should -Be 'new'
                Get-OldFile -Directory $dir | Should -HaveCount 2
            }
            finally {
                Close-RunningCopy -Process $current
                Close-RunningCopy -Process $old
            }
            Install-BinaryFile -Source (Join-Path $dir 'src.bin') -Destination $target
            Get-OldFile -Directory $dir | Should -HaveCount 0
        }
    }
}

Describe 'Install-EntireWithScoop' {
    BeforeAll {
        . (Join-Path (Split-Path -Parent $PSScriptRoot) 'install.ps1')
    }

    BeforeEach {
        Mock Write-Info {}
        Mock Write-Success {}
        Mock Test-ScoopAppInstalled { $false }
        Mock Invoke-Scoop { @{ ExitCode = 0; Output = @() } }
    }

    It 'installs after the bucket is added (exit <ExitCode>)' -TestCases @(
        @{ ExitCode = 0 }
        @{ ExitCode = 2 }
    ) {
        Mock Invoke-Scoop { @{ ExitCode = $ExitCode; Output = @() } } -ParameterFilter { $ScoopArgs[0] -eq 'bucket' -and $ScoopArgs[1] -eq 'add' }
        Install-EntireWithScoop
        Should -Invoke Invoke-Scoop -Times 1 -Exactly -ParameterFilter { $ScoopArgs[0] -eq 'install' }
        Should -Invoke Invoke-Scoop -Times 0 -Exactly -ParameterFilter { $ScoopArgs[0] -eq 'bucket' -and $ScoopArgs[1] -eq 'list' }
    }

    It 'reports scoop output when adding the bucket fails' {
        Mock Invoke-Scoop { @{ ExitCode = 1; Output = @('boom: no git') } } -ParameterFilter { $ScoopArgs[0] -eq 'bucket' -and $ScoopArgs[1] -eq 'add' }
        { Install-EntireWithScoop } | Should -Throw -ExpectedMessage '*Failed to add the Entire Scoop bucket (scoop exited 1)*boom: no git*'
        Should -Invoke Invoke-Scoop -Times 0 -Exactly -ParameterFilter { $ScoopArgs[0] -eq 'install' }
        Should -Invoke Invoke-Scoop -Times 0 -Exactly -ParameterFilter { $ScoopArgs[0] -eq 'bucket' -and $ScoopArgs[1] -eq 'list' }
    }

    It 'updates instead of installing when entire is already present' {
        Mock Test-ScoopAppInstalled { $true }
        Install-EntireWithScoop
        Should -Invoke Invoke-Scoop -Times 1 -Exactly -ParameterFilter { $ScoopArgs[0] -eq 'update' }
        Should -Invoke Invoke-Scoop -Times 0 -Exactly -ParameterFilter { $ScoopArgs[0] -eq 'install' }
    }

    It 'reports scoop output when the install fails' {
        Mock Invoke-Scoop { @{ ExitCode = 1; Output = @('manifest not found') } } -ParameterFilter { $ScoopArgs[0] -eq 'install' }
        { Install-EntireWithScoop } | Should -Throw -ExpectedMessage '*Scoop failed to install Entire CLI (scoop exited 1)*manifest not found*'
    }
}

Describe 'Get-ReleaseVersion' {
    BeforeAll {
        . (Join-Path (Split-Path -Parent $PSScriptRoot) 'install.ps1')
    }

    It 'names the missing nightly when GitHub returns no releases' {
        Mock Invoke-GitHubApi { $null }
        { Get-ReleaseVersion -ReleaseChannel nightly } | Should -Throw -ExpectedMessage '*No nightly release found among the 0 most recent releases*'
    }

    It 'names the missing nightly when every release is stable' {
        Mock Invoke-GitHubApi { @(@{ tag_name = 'v1.2.0' }, @{ tag_name = 'v1.1.0' }, @{ tag_name = 'v1.0.0' }) }
        { Get-ReleaseVersion -ReleaseChannel nightly } | Should -Throw -ExpectedMessage '*No nightly release found among the 3 most recent releases*'
    }

    It 'returns the first nightly tag' {
        Mock Invoke-GitHubApi { @(@{ tag_name = 'v1.2.0' }, @{ tag_name = 'v1.2.1-nightly.202609040000.abc1234' }, @{ tag_name = 'v1.1.0-nightly.1' }) }
        Get-ReleaseVersion -ReleaseChannel nightly | Should -Be '1.2.1-nightly.202609040000.abc1234'
    }

    It 'names a stable release without a tag' {
        Mock Invoke-GitHubApi { @{ tag_name = '' } }
        { Get-ReleaseVersion -ReleaseChannel stable } | Should -Throw -ExpectedMessage '*without a tag name*'
    }

    It 'returns the stable version without its v prefix' {
        Mock Invoke-GitHubApi { @{ tag_name = 'v1.2.3' } }
        Get-ReleaseVersion -ReleaseChannel stable | Should -Be '1.2.3'
    }
}

Describe 'Invoke-GitHubApi' {
    BeforeAll {
        . (Join-Path (Split-Path -Parent $PSScriptRoot) 'install.ps1')
    }

    It 'blames connectivity when no response came back' {
        Mock Invoke-RestMethod { throw 'No such host is known.' }
        { Invoke-GitHubApi -Uri 'https://api.github.com/x' } | Should -Throw -ExpectedMessage '*Could not reach GitHub at https://api.github.com/x. No such host is known. Check your internet connection.*'
    }

    It 'survives an exception type without a Response property under StrictMode' {
        Mock Invoke-RestMethod { throw [System.Threading.Tasks.TaskCanceledException]::new('The operation was canceled.') }
        Set-StrictMode -Version 2.0
        { Invoke-GitHubApi -Uri 'https://api.github.com/x' } | Should -Throw -ExpectedMessage '*Could not reach GitHub*The operation was canceled.*'
    }
}

Describe 'Web timeouts' {
    BeforeAll {
        . (Join-Path (Split-Path -Parent $PSScriptRoot) 'install.ps1')
    }

    # Pester exposes a mocked call's arguments to the filter as variables, with
    # alias names filled in too, so $TimeoutSec is set whether the host binds
    # it as TimeoutSec (5.1) or ConnectionTimeoutSeconds (7.4+).
    It 'caps the GitHub API calls at 60 seconds' {
        Mock Invoke-RestMethod { @{} }
        Invoke-GitHubApi -Uri 'https://api.github.com/x'
        Should -Invoke Invoke-RestMethod -Times 1 -Exactly -ParameterFilter { $TimeoutSec -eq 60 }
    }

    It 'downloads with a stall guard where the host has one and no whole-transfer cap' {
        $hostHasStallGuard = (Get-Command Invoke-WebRequest).Parameters.ContainsKey('OperationTimeoutSeconds')
        Mock Invoke-WebRequest {}
        Save-RemoteFile -Uri 'https://example.invalid/a.zip' -Destination (Join-Path $TestDrive 'a.zip')
        Should -Invoke Invoke-WebRequest -Times 1 -Exactly -ParameterFilter {
            $null -eq $TimeoutSec -and $null -eq $ConnectionTimeoutSeconds -and
            $(if ($hostHasStallGuard) { $OperationTimeoutSeconds -eq 60 } else { $null -eq $OperationTimeoutSeconds })
        }
    }
}

Describe 'Scoop or archive selection' {
    BeforeAll {
        . (Join-Path (Split-Path -Parent $PSScriptRoot) 'install.ps1')
    }

    BeforeEach {
        Mock Assert-Prerequisite {}
        Mock Write-Info {}
        Mock Install-EntireWithScoop {}
        Mock Get-PlatformArchitecture { 'amd64' }
        # The archive branch's first step after the directory is resolved; stop there.
        Mock Get-ReleaseVersion { throw 'stop-here' }
    }

    It 'takes the archive path when -InstallDir is explicit even with Scoop on PATH' {
        function scoop {}
        # Set-Variable, not assignment: these are read by Install-Entire through
        # the scope chain, which the linter's unused-variable rule cannot see.
        Set-Variable -Name Channel -Value 'stable'
        Set-Variable -Name InstallDir -Value (Join-Path $TestDrive 'chosen')
        Set-Variable -Name InstallDirExplicit -Value $true
        { Install-Entire } | Should -Throw -ExpectedMessage '*stop-here*'
        Should -Invoke Install-EntireWithScoop -Times 0 -Exactly
        Should -Invoke Get-ReleaseVersion -Times 1 -Exactly
    }

    It 'reports a copy that is only on the stored PATH after a Scoop install' {
        function scoop {}
        Set-Variable -Name Channel -Value 'stable'
        Set-Variable -Name InstallDirExplicit -Value $false
        $shim = Join-Path (Join-Path $TestDrive 'scoop') 'shims\entire.exe'
        $otherDir = Join-Path $TestDrive 'elsewhere'
        [IO.Directory]::CreateDirectory($otherDir) | Out-Null
        Set-Content -LiteralPath (Join-Path $otherDir 'entire.exe') -Value 'x' -NoNewline
        Mock Invoke-Scoop { @{ ExitCode = 0; Output = @((Join-Path (Join-Path (Join-Path $TestDrive 'scoop') 'apps\entire') 'current')) } } -ParameterFilter { $ScoopArgs[0] -eq 'prefix' }
        Mock Get-EntireOnPath { @([pscustomobject]@{ Source = $shim }) }
        Mock Get-UserEnvironmentValue { $otherDir }
        $report = @(Install-Entire 6>&1 | ForEach-Object { "$_" })
        $report | Should -Contain '! Also on your saved PATH, not active in this shell:'
        $report | Should -Contain "!   $(Join-Path $otherDir 'entire.exe')"
        @($report | Where-Object { $_ -like '! Also found:*' }) | Should -HaveCount 0
    }

    It 'reports nothing extra when the stored PATH holds no other copy' {
        function scoop {}
        Set-Variable -Name Channel -Value 'stable'
        Set-Variable -Name InstallDirExplicit -Value $false
        $shim = Join-Path (Join-Path $TestDrive 'scoop') 'shims\entire.exe'
        Mock Invoke-Scoop { @{ ExitCode = 0; Output = @((Join-Path (Join-Path (Join-Path $TestDrive 'scoop') 'apps\entire') 'current')) } } -ParameterFilter { $ScoopArgs[0] -eq 'prefix' }
        Mock Get-EntireOnPath { @([pscustomobject]@{ Source = $shim }) }
        Mock Get-UserEnvironmentValue { Join-Path $TestDrive 'nothing-here' }
        $report = @(Install-Entire 6>&1 | ForEach-Object { "$_" })
        @($report | Where-Object { $_ -like '! Also found:*' }) | Should -HaveCount 0
    }

    It 'uses Scoop on stable when -InstallDir was not given' {
        function scoop {}
        # Set-Variable, not assignment: these are read by Install-Entire through
        # the scope chain, which the linter's unused-variable rule cannot see.
        Set-Variable -Name Channel -Value 'stable'
        Set-Variable -Name InstallDirExplicit -Value $false
        # Stop inside the Scoop branch; what follows it shells out to scoop.
        Mock Install-EntireWithScoop { throw 'scoop-branch' }
        { Install-Entire } | Should -Throw -ExpectedMessage '*scoop-branch*'
        Should -Invoke Install-EntireWithScoop -Times 1 -Exactly
        Should -Invoke Get-ReleaseVersion -Times 0 -Exactly
    }
}

Describe 'Get-WriteFailureKind' {
    BeforeAll {
        . (Join-Path (Split-Path -Parent $PSScriptRoot) 'install.ps1')
    }

    It 'classifies <Name> as <Expected>' -TestCases @(
        @{ Name = 'UnauthorizedAccessException'; Expected = 'denied'; Build = { [UnauthorizedAccessException]::new('denied') } }
        @{ Name = 'IOException with the access-denied HResult'; Expected = 'denied'; Build = { [IO.IOException]::new('denied', 0x80070005) } }
        @{ Name = 'IOException with ERROR_SHARING_VIOLATION'; Expected = 'held'; Build = { [IO.IOException]::new('held', 0x80070020) } }
        @{ Name = 'IOException with ERROR_LOCK_VIOLATION'; Expected = 'held'; Build = { [IO.IOException]::new('held', 0x80070021) } }
        @{ Name = 'IOException with another HResult'; Expected = 'other'; Build = { [IO.IOException]::new('disk full', 0x80070070) } }
        @{ Name = 'an unrelated exception'; Expected = 'other'; Build = { [InvalidOperationException]::new('nope') } }
        @{ Name = 'a PowerShell wrapper around access denied'; Expected = 'denied'; Build = { [System.Management.Automation.RuntimeException]::new('wrapped', [UnauthorizedAccessException]::new('denied')) } }
        @{ Name = 'a PowerShell wrapper around a sharing violation'; Expected = 'held'; Build = { [System.Management.Automation.RuntimeException]::new('wrapped', [IO.IOException]::new('held', 0x80070020)) } }
    ) {
        Get-WriteFailureKind -Exception (& $Build) | Should -Be $Expected
    }
}

Describe 'Denied writes' {
    BeforeAll {
        . (Join-Path (Split-Path -Parent $PSScriptRoot) 'install.ps1')

        # Removes or restores write access for the current user on a directory.
        function Edit-DirectoryWriteAccess {
            param([string] $Path, [bool] $Writable)
            if ($env:OS -eq 'Windows_NT') {
                $ace = "$($env:USERNAME):(W)"
                if ($Writable) { icacls $Path /remove:d $env:USERNAME | Out-Null }
                else { icacls $Path /deny $ace | Out-Null }
            }
            else {
                chmod $(if ($Writable) { '755' } else { '555' }) $Path
            }
        }
    }

    It 'names access denied, not a held file, when the directory is not writable' {
        $dir = Join-Path $TestDrive 'locked'
        $target = Join-Path $dir 'entire.exe'
        [IO.Directory]::CreateDirectory($dir) | Out-Null
        Set-Content -LiteralPath $target -Value 'old' -NoNewline
        Set-Content -LiteralPath (Join-Path $TestDrive 'src.bin') -Value 'new' -NoNewline
        Edit-DirectoryWriteAccess -Path $dir -Writable $false
        try {
            $thrown = $null
            try { Install-BinaryFile -Source (Join-Path $TestDrive 'src.bin') -Destination $target } catch { $thrown = $_.Exception.Message }
            $thrown | Should -BeLike "Access denied writing to $dir.*"
            $thrown | Should -Not -BeLike '*holds it open*'
        }
        finally {
            Edit-DirectoryWriteAccess -Path $dir -Writable $true
        }
        Get-Content -LiteralPath $target -Raw | Should -Be 'old'
    }

    It 'names access denied when the install directory cannot be created' {
        $parent = Join-Path $TestDrive 'ro-parent'
        [IO.Directory]::CreateDirectory($parent) | Out-Null
        Edit-DirectoryWriteAccess -Path $parent -Writable $false
        try {
            $wanted = Join-Path $parent 'bin'
            { Initialize-InstallDirectory -Path $wanted } | Should -Throw -ExpectedMessage "Access denied writing to $wanted.*"
        }
        finally {
            Edit-DirectoryWriteAccess -Path $parent -Writable $true
        }
    }
}

Describe 'Get-EntireCopy' {
    BeforeAll {
        . (Join-Path (Split-Path -Parent $PSScriptRoot) 'install.ps1')
    }

    It 'adds copies that are only on the stored user PATH and lists shared ones once' {
        $processDir = Join-Path $TestDrive 'proc'
        $storedDir = Join-Path $TestDrive 'stored'
        foreach ($dir in $processDir, $storedDir) {
            [IO.Directory]::CreateDirectory($dir) | Out-Null
            Set-Content -LiteralPath (Join-Path $dir 'entire.exe') -Value 'x' -NoNewline
        }
        Mock Get-EntireOnPath { @([pscustomobject]@{ Source = (Join-Path $processDir 'entire.exe') }) }
        Mock Get-UserEnvironmentValue { "$storedDir;$processDir" }

        # Assigned, then indexed: piping the -NoEnumerate result straight into
        # ForEach-Object would member-enumerate .Source over the whole array on
        # pwsh and hide a wrong element count.
        $copies = Get-EntireCopy
        @($copies) | Should -HaveCount 2
        $copies[0].Source | Should -Be (Join-Path $processDir 'entire.exe')
        $copies[0].Active | Should -BeTrue
        $copies[1].Source | Should -Be (Join-Path $storedDir 'entire.exe')
        $copies[1].Active | Should -BeFalse
    }

    It 'ignores stored PATH directories without an entire.exe' {
        $processDir = Join-Path $TestDrive 'proc2'
        [IO.Directory]::CreateDirectory($processDir) | Out-Null
        Set-Content -LiteralPath (Join-Path $processDir 'entire.exe') -Value 'x' -NoNewline
        Mock Get-EntireOnPath { @([pscustomobject]@{ Source = (Join-Path $processDir 'entire.exe') }) }
        Mock Get-UserEnvironmentValue { (Join-Path $TestDrive 'empty') + ';;' }

        $copies = Get-EntireCopy
        @($copies | Where-Object { $_.Source -like "*$TestDrive*" }) | Should -HaveCount 1
    }
}

Describe 'Get-PlatformArchitecture' {
    BeforeAll {
        . (Join-Path (Split-Path -Parent $PSScriptRoot) 'install.ps1')
    }

    It 'maps <Native> to <Expected>' -TestCases @(
        @{ Native = 'AMD64'; Expected = 'amd64' }
        @{ Native = 'ARM64'; Expected = 'arm64' }
        @{ Native = 'arm64'; Expected = 'arm64' }
    ) {
        Mock Get-NativeArchitectureName { $Native }
        Get-PlatformArchitecture | Should -Be $Expected
    }

    It 'refuses an architecture it has no build for' {
        Mock Get-NativeArchitectureName { 'x86' }
        { Get-PlatformArchitecture } | Should -Throw -ExpectedMessage 'Unsupported architecture: x86'
    }

    It 'refuses an empty architecture value' {
        Mock Get-NativeArchitectureName { '' }
        { Get-PlatformArchitecture } | Should -Throw -ExpectedMessage 'Cannot determine the Windows architecture.'
    }
}
