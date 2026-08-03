package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// runWindowsWrapper mirrors Codex's Windows command runner: cmd.exe /C followed
// by the raw, quoted hook command. SysProcAttr.CmdLine is required because
// cmd.exe does not use the standard Windows argv unquoting rules.
func runWindowsWrapper(t *testing.T, wrapper string, entirePresent bool) (string, string, int) {
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
	cmdPath, err := exec.LookPath("cmd.exe")
	if err != nil {
		t.Fatalf("find cmd.exe: %v", err)
	}

	cmd := exec.CommandContext(t.Context(), cmdPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CmdLine: `"` + cmdPath + `" /C "` + wrapper + `"`,
	}
	cmd.Dir = runDir // clean CWD so `where` can't find a stray entire next to us
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return stdout.String(), stderr.String(), exitErr.ExitCode()
		}
		t.Fatalf("run wrapper: %v", err)
	}
	return stdout.String(), stderr.String(), 0
}

// TestWindowsWrappers_Execution verifies the cmd.exe wrappers behave correctly
// when actually executed — the gap the trail's medium finding flagged (prior
// tests asserted only string contents). It confirms the wrapped command runs
// (and propagates its exit code) when entire is present, and is skipped with a
// 0 exit when entire is absent, for both the silent and JSON-warning forms.
func TestWindowsWrappers_Execution(t *testing.T) {
	// No t.Parallel(): t.Setenv("PATH") forbids it.

	const marker = "ENTIRE_HOOK_RAN"

	t.Run("silent/present runs the command", func(t *testing.T) {
		out, stderr, code := runWindowsWrapper(t, WrapWindowsProductionSilentHookCommand("echo "+marker), true)
		if !strings.Contains(out, marker) {
			t.Fatalf("expected wrapped command to run; stdout=%q stderr=%q", out, stderr)
		}
		if code != 0 {
			t.Fatalf("expected exit 0, got %d; stderr=%q", code, stderr)
		}
	})

	t.Run("silent/present propagates the command exit code", func(t *testing.T) {
		_, stderr, code := runWindowsWrapper(t, WrapWindowsProductionSilentHookCommand("cmd /c exit 7"), true)
		if code != 7 {
			t.Fatalf("expected wrapped command exit code 7 to propagate, got %d; stderr=%q", code, stderr)
		}
	})

	t.Run("silent/absent skips the command and exits 0", func(t *testing.T) {
		out, stderr, code := runWindowsWrapper(t, WrapWindowsProductionSilentHookCommand("echo "+marker), false)
		if strings.Contains(out, marker) {
			t.Fatalf("wrapped command must NOT run when entire absent; stdout=%q stderr=%q", out, stderr)
		}
		if code != 0 {
			t.Fatalf("expected exit 0 when entire absent, got %d; stderr=%q", code, stderr)
		}
	})

	t.Run("json/absent emits valid JSON and skips the command", func(t *testing.T) {
		out, stderr, code := runWindowsWrapper(t, WrapWindowsProductionJSONWarningHookCommand("echo "+marker, WarningFormatSingleLine), false)
		if strings.Contains(out, marker) {
			t.Fatalf("wrapped command must NOT run when entire absent; stdout=%q stderr=%q", out, stderr)
		}
		if code != 0 {
			t.Fatalf("expected exit 0, got %d; stderr=%q", code, stderr)
		}
		var payload struct {
			SystemMessage string `json:"systemMessage"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &payload); err != nil {
			t.Fatalf("expected valid JSON on stdout, got %q stderr=%q (err %v)", out, stderr, err)
		}
		if !strings.Contains(payload.SystemMessage, "Entire CLI") {
			t.Fatalf("unexpected systemMessage: %q", payload.SystemMessage)
		}
	})

	t.Run("json/present runs the command without a warning", func(t *testing.T) {
		out, stderr, code := runWindowsWrapper(t, WrapWindowsProductionJSONWarningHookCommand("echo "+marker, WarningFormatSingleLine), true)
		if !strings.Contains(out, marker) {
			t.Fatalf("expected wrapped command to run; stdout=%q stderr=%q", out, stderr)
		}
		if strings.Contains(out, "systemMessage") {
			t.Fatalf("warning must NOT be emitted when entire present; stdout=%q stderr=%q", out, stderr)
		}
		if code != 0 {
			t.Fatalf("expected exit 0, got %d; stderr=%q", code, stderr)
		}
	})

	t.Run("json/present propagates the command exit code", func(t *testing.T) {
		out, stderr, code := runWindowsWrapper(
			t,
			WrapWindowsProductionJSONWarningHookCommand("cmd /c exit 7", WarningFormatSingleLine),
			true,
		)
		if strings.Contains(out, "systemMessage") {
			t.Fatalf("warning must NOT be emitted when entire present; stdout=%q stderr=%q", out, stderr)
		}
		if code != 7 {
			t.Fatalf("expected wrapped command exit code 7 to propagate, got %d; stderr=%q", code, stderr)
		}
	})
}
