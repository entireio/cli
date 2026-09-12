package globalhooks

import (
	"encoding/base64"
	"encoding/binary"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestWindowsCommandVersions(t *testing.T) {
	t.Parallel()
	const previousScript = "$ErrorActionPreference='Stop'; # entire-global-hook-v1\n" +
		`$exe='C:\Tools\Entire 雪 '' selected.exe'; if (!(Test-Path -LiteralPath $exe -PathType Leaf)) { [Console]::Error.WriteLine('Entire: selected hook installation is unavailable; use entire agent to select an installation.'); exit 0 }; & $exe 'hooks' 'global' 'gemini' 'after-agent'; exit $LASTEXITCODE`
	for name, script := range map[string]string{
		"previous": previousScript,
		"current":  "$ProgressPreference='SilentlyContinue'; " + previousScript,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if !IsCommand(windowsScriptCommand(script)) {
				t.Fatal("complete generated command not recognized")
			}
			for _, custom := range []string{
				strings.Replace(script, "'hooks' 'global'", "'status' '--json'", 1),
				script + "; Write-Output 'custom'",
			} {
				if IsCommand(windowsScriptCommand(custom)) {
					t.Fatal("customized command recognized as generated")
				}
			}
		})
	}
}

func windowsScriptCommand(script string) string {
	units := utf16.Encode([]rune(script))
	data := make([]byte, len(units)*2)
	for i, unit := range units {
		binary.LittleEndian.PutUint16(data[2*i:], unit)
	}
	return `"C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe" -NoProfile -NonInteractive -EncodedCommand ` + base64.StdEncoding.EncodeToString(data)
}
