Describe 'run.ps1' {
    BeforeAll {
        function Invoke-Runner {
            param([string] $Directory)
            $shell = (Get-Process -Id $PID).Path
            $output = @(& $shell -NoProfile -NonInteractive -File (Join-Path $PSScriptRoot 'run.ps1') -Path $Directory 2>&1 | ForEach-Object { "$_" })
            [pscustomobject]@{ Output = $output; ExitCode = $LASTEXITCODE }
        }
    }

    It 'exits 0 for a passing container' {
        $dir = Join-Path $TestDrive 'passing'
        [IO.Directory]::CreateDirectory($dir) | Out-Null
        Set-Content -LiteralPath (Join-Path $dir 'Ok.Tests.ps1') -Value 'Describe "ok" { It "passes" { 1 | Should -Be 1 } }'
        (Invoke-Runner -Directory $dir).ExitCode | Should -Be 0
    }

    # A file that fails to parse produces no failed tests, only a failed
    # container, so an exit code taken from FailedCount would be 0.
    It 'exits non-zero when a test file fails to load' {
        $dir = Join-Path $TestDrive 'broken'
        [IO.Directory]::CreateDirectory($dir) | Out-Null
        Set-Content -LiteralPath (Join-Path $dir 'Ok.Tests.ps1') -Value 'Describe "ok" { It "passes" { 1 | Should -Be 1 } }'
        Set-Content -LiteralPath (Join-Path $dir 'Broken.Tests.ps1') -Value "Describe 'broken' {`n    It 'never runs' { 1 | Should -Be 1 }"
        $run = Invoke-Runner -Directory $dir
        $run.ExitCode | Should -Not -Be 0
        ($run.Output -join "`n") | Should -BeLike '*Failed*'
    }

    It 'exits non-zero when a BeforeAll throws' {
        $dir = Join-Path $TestDrive 'beforeall'
        [IO.Directory]::CreateDirectory($dir) | Out-Null
        Set-Content -LiteralPath (Join-Path $dir 'Setup.Tests.ps1') -Value 'Describe "setup" { BeforeAll { throw "no" }; It "never runs" { 1 | Should -Be 1 } }'
        (Invoke-Runner -Directory $dir).ExitCode | Should -Not -Be 0
    }
}
