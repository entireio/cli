package agent

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// runWindowsWrapper executes a Windows hook wrapper string through cmd.exe and
// returns its stdout and exit code. The wrapper is written verbatim to a .bat
// file (rather than re-quoted into argv) so the test exercises exactly the
// string Entire writes into hooks.json. PATH is scoped so `where.exe entire`
// resolves only when entirePresent is true.
func runWindowsWrapper(t *testing.T, wrapper string, entirePresent bool) (string, int) {
	t.Helper()

	sysRoot := os.Getenv("SystemRoot")
	if sysRoot == "" {
		sysRoot = `C:\Windows`
	}
	// System32 supplies cmd.exe and where.exe; nothing else is on PATH so an
	// `entire` installed on the host machine can't leak into the "absent" case.
	pathEntries := []string{filepath.Join(sysRoot, "System32")}
	if entirePresent {
		stubDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(stubDir, "entire.bat"), []byte("@exit /b 0\r\n"), 0o700); err != nil {
			t.Fatalf("write entire stub: %v", err)
		}
		pathEntries = append([]string{stubDir}, pathEntries...)
	}
	t.Setenv("PATH", strings.Join(pathEntries, ";"))

	runDir := t.TempDir()
	batPath := filepath.Join(runDir, "run.bat")
	if err := os.WriteFile(batPath, []byte(wrapper+"\r\n"), 0o700); err != nil {
		t.Fatalf("write run.bat: %v", err)
	}

	cmd := exec.CommandContext(t.Context(), "cmd.exe", "/d", "/s", "/c", batPath)
	cmd.Dir = runDir // clean CWD so `where` can't find a stray entire next to us
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return string(out), exitErr.ExitCode()
		}
		t.Fatalf("run wrapper: %v", err)
	}
	return string(out), 0
}

// TestWindowsWrappers_Execution verifies the cmd.exe wrappers behave correctly
// when actually executed — the gap the trail's medium finding flagged (prior
// tests asserted only string contents). It confirms the wrapped command runs
// (and propagates its exit code) when entire is present, and is skipped with a
// 0 exit when entire is absent, for both the silent and JSON-warning forms.
func TestWindowsWrappers_Execution(t *testing.T) {
	if runtime.GOOS != windowsOS {
		t.Skip("cmd.exe wrapper execution test runs only on Windows")
	}
	// No t.Parallel(): t.Setenv("PATH") forbids it.

	const marker = "ENTIRE_HOOK_RAN"

	t.Run("silent/present runs the command", func(t *testing.T) {
		out, code := runWindowsWrapper(t, WrapWindowsProductionSilentHookCommand("echo "+marker), true)
		if !strings.Contains(out, marker) {
			t.Fatalf("expected wrapped command to run; stdout=%q", out)
		}
		if code != 0 {
			t.Fatalf("expected exit 0, got %d", code)
		}
	})

	t.Run("silent/present propagates the command exit code", func(t *testing.T) {
		_, code := runWindowsWrapper(t, WrapWindowsProductionSilentHookCommand("cmd /c exit 7"), true)
		if code != 7 {
			t.Fatalf("expected wrapped command exit code 7 to propagate, got %d", code)
		}
	})

	t.Run("silent/absent skips the command and exits 0", func(t *testing.T) {
		out, code := runWindowsWrapper(t, WrapWindowsProductionSilentHookCommand("echo "+marker), false)
		if strings.Contains(out, marker) {
			t.Fatalf("wrapped command must NOT run when entire absent; stdout=%q", out)
		}
		if code != 0 {
			t.Fatalf("expected exit 0 when entire absent, got %d", code)
		}
	})

	t.Run("json/absent emits valid JSON and skips the command", func(t *testing.T) {
		out, code := runWindowsWrapper(t, WrapWindowsProductionJSONWarningHookCommand("echo "+marker, WarningFormatSingleLine), false)
		if strings.Contains(out, marker) {
			t.Fatalf("wrapped command must NOT run when entire absent; stdout=%q", out)
		}
		if code != 0 {
			t.Fatalf("expected exit 0, got %d", code)
		}
		var payload struct {
			SystemMessage string `json:"systemMessage"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &payload); err != nil {
			t.Fatalf("expected valid JSON on stdout, got %q (err %v)", out, err)
		}
		if !strings.Contains(payload.SystemMessage, "Entire CLI") {
			t.Fatalf("unexpected systemMessage: %q", payload.SystemMessage)
		}
	})

	t.Run("json/present runs the command without a warning", func(t *testing.T) {
		out, code := runWindowsWrapper(t, WrapWindowsProductionJSONWarningHookCommand("echo "+marker, WarningFormatSingleLine), true)
		if !strings.Contains(out, marker) {
			t.Fatalf("expected wrapped command to run; stdout=%q", out)
		}
		if strings.Contains(out, "systemMessage") {
			t.Fatalf("warning must NOT be emitted when entire present; stdout=%q", out)
		}
		if code != 0 {
			t.Fatalf("expected exit 0, got %d", code)
		}
	})
}
