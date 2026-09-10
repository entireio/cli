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
	script := strings.Replace(string(utf16.Decode(units)), "'hooks' 'global'", "'status' '--json'", 1)
	units = utf16.Encode([]rune(script))
	data = make([]byte, len(units)*2)
	for i, unit := range units {
		binary.LittleEndian.PutUint16(data[2*i:], unit)
	}
	commands := map[string]string{
		"POSIX non-hook":          strings.Replace(posix, " hooks global ", " status --json ", 1),
		"Windows non-hook":        prefix + " -EncodedCommand " + base64.StdEncoding.EncodeToString(data),
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
