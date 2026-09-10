package globalhooks

import (
	"encoding/base64"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestSelectionPersistsPathAcrossReplacement(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	path := filepath.Join(t.TempDir(), "entire")
	if err := os.WriteFile(path, []byte("first"), 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("second"), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := Load()
	if err != nil || got != s {
		t.Fatalf("selection after replacement = %+v, %v", got, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil {
		t.Fatal("missing executable accepted")
	}
}

func TestSelectionRejectsRelativePath(t *testing.T) {
	t.Parallel()
	if _, err := New("entire"); err == nil {
		t.Fatal("relative selection accepted")
	}
}

func TestCommandPreservesArguments(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == windowsPlatform {
		t.Skip("POSIX launcher execution")
	}
	path := filepath.Join(t.TempDir(), "chosen ' $ executable")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n/bin/cat\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	command := s.Command("claude-code", "session-start")
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", command)
	cmd.Env = []string{"PATH=/nonexistent"}
	cmd.Stdin = strings.NewReader(`{"session_id":"sample"}`)
	out, err := cmd.CombinedOutput()
	if err != nil || string(out) != "hooks\nglobal\nclaude-code\nsession-start\n"+`{"session_id":"sample"}` {
		t.Fatalf("command output = %q, %v", out, err)
	}
	if !IsCommand(command) || IsCommand("echo "+command) {
		t.Fatal("command ownership mismatch")
	}
	if strings.Contains(command, "command -v") {
		t.Fatal("unexpected executable lookup")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	cmd = exec.CommandContext(t.Context(), "/bin/sh", "-c", command)
	cmd.Env = []string{"PATH=/nonexistent"}
	out, err = cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "selected hook installation is unavailable") {
		t.Fatalf("missing selected command = %q, %v", out, err)
	}
}

func TestWindowsCommandCarriesLiteralSelectedPath(t *testing.T) {
	t.Parallel()
	s := Selection{Executable: `C:\Programs\100% ' chosen & tool\entire.exe`, Launcher: `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`, Platform: windowsPlatform}
	command := s.Command("claude-code", "session-start")
	_, encoded, ok := strings.Cut(command, " -EncodedCommand ")
	if !ok {
		t.Fatal("missing encoded launcher script")
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	units := make([]uint16, len(data)/2)
	for i := range units {
		units[i] = binary.LittleEndian.Uint16(data[2*i:])
	}
	script := string(utf16.Decode(units))
	if !strings.HasPrefix(script, "$ProgressPreference='SilentlyContinue'; ") {
		t.Fatal("launcher must suppress its own progress output before invoking commands")
	}
	if !strings.Contains(script, psQuote(s.Executable)) || strings.Contains(command, "100%") || !IsCommand(command) {
		t.Fatalf("selected path not represented literally: %s", script)
	}
}
