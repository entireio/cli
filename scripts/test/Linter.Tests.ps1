Describe 'PSScriptAnalyzer' -Tag 'Linter' {
    It 'has a settings file' {
        Join-Path (Split-Path -Parent $PSScriptRoot) 'PSScriptAnalyzerSettings.psd1' | Should -Exist
    }

    # A green suite with zero findings looks the same whether the analyzer
    # evaluated every rule or none, so prove it reports a rule the settings file
    # does not exclude. The bad script lives in $TestDrive, outside scripts/, so
    # the recursive scan below never sees it.
    It 'reports a known-bad script under the same settings' {
        $settings = Join-Path (Split-Path -Parent $PSScriptRoot) 'PSScriptAnalyzerSettings.psd1'
        $bad = Join-Path $TestDrive 'empty-catch.ps1'
        Set-Content -Path $bad -Value "try { Get-Item . } catch { }"
        $analysis = @(Invoke-ScriptAnalyzer -Path $bad -Settings $settings)
        $analysis.RuleName | Should -Contain 'PSAvoidUsingEmptyCatchBlock'
    }

    It 'reports no findings under scripts/' {
        $scriptsDir = Split-Path -Parent $PSScriptRoot
        $analysis = @(Invoke-ScriptAnalyzer -Path $scriptsDir -Recurse -Settings (Join-Path $scriptsDir 'PSScriptAnalyzerSettings.psd1'))
        foreach ($result in $analysis) {
            Write-Warning ("{0} {1} {2}:{3} {4}" -f $result.Severity, $result.RuleName, $result.ScriptName, $result.Line, $result.Message)
        }
        $analysis | Should -HaveCount 0
    }
}
