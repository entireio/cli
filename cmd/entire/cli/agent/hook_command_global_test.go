package agent

import (
	"encoding/base64"
	"encoding/binary"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/entireio/cli/cmd/entire/cli/agent/globalhooks"
)

func TestCustomizedGlobalLaunchersArePreserved(t *testing.T) {
	t.Parallel()
	posix := globalhooks.Selection{Executable: "/tools/entire", Launcher: "/bin/sh", Platform: "linux"}.Command("claude-code", "stop")
	windows := globalhooks.Selection{Executable: `C:\Tools\entire.exe`, Launcher: `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`, Platform: "windows"}.Command("gemini", "after-agent")
	customWindows := transformWindowsTestCommand(t, windows, func(script string) string {
		return strings.Replace(script, "'hooks' 'global'", "'status' '--json'", 1)
	})
	commands := map[string]string{
		"POSIX non-hook":   strings.Replace(posix, " hooks global ", " status --json ", 1),
		"Windows non-hook": customWindows,
		"Windows previous non-hook": transformWindowsTestCommand(t, customWindows, func(script string) string {
			return strings.TrimPrefix(script, "$ProgressPreference='SilentlyContinue'; ")
		}),
		"POSIX customized suffix": posix + " ; printf custom",
	}
	for name, command := range commands {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			kept, dropped := DropStaleManagedHooks([]string{command}, func(s string) string { return s }, nil)
			if dropped || len(kept) != 1 || kept[0] != command {
				t.Fatalf("customized launcher was removed: %s", command)
			}
		})
	}
}

func TestWindowsPreviousGlobalCommandMigration(t *testing.T) {
	t.Parallel()
	current := globalhooks.Selection{Executable: `C:\Tools\entire.exe`, Launcher: `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`, Platform: "windows"}.Command("gemini", "after-agent")
	previous := transformWindowsTestCommand(t, current, func(script string) string {
		return strings.TrimPrefix(script, "$ProgressPreference='SilentlyContinue'; ")
	})
	if previous == current {
		t.Fatal("current command must differ from previous version")
	}
	for name, expected := range map[string][]string{"repair": {current}, "uninstall": nil} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			kept, dropped := DropStaleManagedHooks([]string{previous}, func(s string) string { return s }, expected)
			if !dropped || len(kept) != 0 {
				t.Fatal("previous command must be removed before installing current inventory")
			}
		})
	}
}

func transformWindowsTestCommand(t *testing.T, windows string, transform func(string) string) string {
	t.Helper()
	prefix, encoded, ok := strings.Cut(windows, " -EncodedCommand ")
	if !ok {
		t.Fatal("missing encoded command")
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	units := make([]uint16, len(data)/2)
	for i := range units {
		units[i] = binary.LittleEndian.Uint16(data[2*i:])
	}
	script := transform(string(utf16.Decode(units)))
	units = utf16.Encode([]rune(script))
	data = make([]byte, len(units)*2)
	for i, unit := range units {
		binary.LittleEndian.PutUint16(data[2*i:], unit)
	}
	return prefix + " -EncodedCommand " + base64.StdEncoding.EncodeToString(data)
}
