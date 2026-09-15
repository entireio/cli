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

	// A clean CWD. cmd.exe searches the current directory before PATH, so every
	// case about the wrapper's own logic must not be answered by a stray file
	// next to us — that property has its own test below.
	return runWindowsWrapperInDir(t, wrapper, entirePresent, t.TempDir())
}

// runWindowsWrapperInDir is runWindowsWrapper with the working directory named,
// so a test can stand the wrapper in a worktree with something planted in it.
func runWindowsWrapperInDir(t *testing.T, wrapper string, entirePresent bool, runDir string) (string, string, int) {
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

	cmdPath, err := exec.LookPath("cmd.exe")
	if err != nil {
		t.Fatalf("find cmd.exe: %v", err)
	}

	cmd := exec.CommandContext(t.Context(), cmdPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CmdLine: `"` + cmdPath + `" /C "` + wrapper + `"`,
	}
	cmd.Dir = runDir
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

// TestWindowsWrappers_DoNotResolveFromTheWorktree pins the half of the
// current-directory defence that no string assertion can reach.
//
// cmd.exe searches the current directory ahead of PATH, and the directory a
// hook runs in is the worktree. So an `entire.bat` committed to a repository was
// what the wrapper executed when the agent started a session — no user action,
// no prompt. windowsEntireGuard sets NoDefaultCurrentDirectoryInExePath to take
// the current directory out of that search.
//
// Asserted against a real cmd.exe on purpose. The documentation says the
// variable exists for shells that do their own resolution and names cmd.exe as
// the example, but it does not say whether cmd.exe re-reads it for commands
// later on a line that `set` it — which is exactly how the wrapper uses it.
// Believing that without checking is what this test refuses to do.
//
// The unhardened subtest is a POSITIVE CONTROL, not history. "The planted file
// did not run" is satisfied just as well by a wrapper that did not run at all,
// so without a form that DOES execute it this test would keep passing while
// checking nothing — it would survive the guard being deleted. It runs the
// wrapper Entire shipped before this change, in the same worktree, and requires
// the marker to appear.
//
// entire is absent from PATH in both, so the planted file is the only `entire`
// anywhere: where.exe searches the current directory too, so the guard passes
// either way and the difference is entirely in what the else branch resolves.
func TestWindowsWrappers_DoNotResolveFromTheWorktree(t *testing.T) {
	// No t.Parallel(): t.Setenv("PATH") forbids it.

	// The wrapper as it stood before windowsEntireGuard. Spelled out rather than
	// built, so that changing the production wrapper cannot quietly change what
	// the control proves.
	const unhardened = `cmd.exe /d /s /c "where.exe entire >nul 2>nul & ` +
		`if errorlevel 1 (ver>nul) else (entire hooks codex stop)"`

	for _, tc := range []struct {
		name    string
		wrapper string
		wantRan bool
	}{
		{
			name:    "unhardened wrapper runs the worktree's entire.bat",
			wrapper: unhardened,
			wantRan: true,
		},
		{
			name:    "hardened wrapper does not",
			wrapper: WrapWindowsProductionSilentHookCommand("entire hooks codex stop"),
			wantRan: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			worktree := t.TempDir()
			markerPath := filepath.Join(worktree, "planted-ran.txt")
			planted := "@echo off\r\n" + `echo PLANTED>> "` + markerPath + `"` + "\r\n" + "exit /b 0\r\n"
			if err := os.WriteFile(filepath.Join(worktree, "entire.bat"), []byte(planted), 0o700); err != nil {
				t.Fatalf("plant entire.bat: %v", err)
			}

			_, stderr, code := runWindowsWrapperInDir(t, tc.wrapper, false, worktree)

			ran := true
			if _, err := os.Stat(markerPath); err != nil {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("stat planted marker: %v", err)
				}
				ran = false
			}
			if ran != tc.wantRan {
				t.Fatalf("worktree entire.bat ran = %v, want %v; exit=%d stderr=%q", ran, tc.wantRan, code, stderr)
			}
		})
	}
}
