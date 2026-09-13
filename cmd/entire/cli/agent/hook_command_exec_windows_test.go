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

// windowsSystemRoot is %SystemRoot%, with the conventional fallback for the
// (unexpected) case of it being unset.
func windowsSystemRoot() string {
	if sysRoot := os.Getenv("SystemRoot"); sysRoot != "" {
		return sysRoot
	}
	return `C:\Windows`
}

// windowsPowerShellPath is the absolute path to Windows PowerShell 5.1.
//
// It is resolved absolutely rather than through exec.LookPath because
// powershell.exe is NOT in System32: it lives in
// System32\WindowsPowerShell\v1.0, a separate directory that Windows ships on
// the default PATH and that setWrapperPATH deliberately scrubs away. Cursor's
// own resolver spells this path out as its last tier for the same reason.
//
// Resolving it absolutely is what lets the scrub stay tight. A PowerShell child
// does not need itself on PATH, and the scrub's whole job is keeping a stray
// `entire` or `sh` out of the case under test, which adding directories back
// would work against.
func windowsPowerShellPath(t *testing.T) string {
	t.Helper()

	path := filepath.Join(windowsSystemRoot(), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Windows PowerShell not found at %s: %v", path, err)
	}
	return path
}

// setWrapperPATH scrubs PATH down to System32 — which supplies cmd.exe,
// where.exe, findstr.exe and powershell.exe — plus, when entirePresent, a
// directory holding an `entire.bat` with the given body. Nothing else is on
// PATH, so an `entire` installed on the host machine cannot leak into the
// "absent" case, and neither can an `sh` from Git for Windows.
//
// Returns the stub directory, or "" when entirePresent is false.
func setWrapperPATH(t *testing.T, entirePresent bool, stubBody string) string {
	t.Helper()

	pathEntries := []string{filepath.Join(windowsSystemRoot(), "System32")}
	stubDir := ""
	if entirePresent {
		stubDir = t.TempDir()
		if err := os.WriteFile(filepath.Join(stubDir, "entire.bat"), []byte(stubBody), 0o700); err != nil {
			t.Fatalf("write entire stub: %v", err)
		}
		pathEntries = append([]string{stubDir}, pathEntries...)
	}
	t.Setenv("PATH", strings.Join(pathEntries, ";"))
	return stubDir
}

// cursorHookScript is Cursor's Windows hook script, verbatim. Read out of the
// shipped builds — Cursor IDE 3.19.19 win32/x64 and the native cursor-agent CLI
// windows/x64 2026.09.08-6caf4ff, which compose it identically, as do IDE
// 3.15.19 and 3.11.19. The stored command is inserted with no quoting of
// Cursor's own.
func cursorHookScript(payloadPath, wrapper string) string {
	return `$OutputEncoding = [System.Text.Encoding]::UTF8; Get-Content -LiteralPath '` +
		payloadPath + `' -Raw | & { $input | ` + wrapper + ` }`
}

// runCursorWrapper runs a wrapper the way Cursor's Windows hook runner does:
// PowerShell, with the JSON payload written to a temp file and piped into the
// wrapped command's stdin.
//
// It cannot share runWrapperCmdLine. That builds a cmd.exe command line via
// SysProcAttr.CmdLine because cmd.exe does not follow the standard argv
// unquoting rules; Cursor spawns PowerShell with a real argv, so the ordinary
// exec.Command path is the faithful model. And Cursor is the only runner that
// delivers the payload on the wrapped command's stdin, which is what stdinSeen
// exists to check — a wrapper can run and still starve the hook of its input.
//
// The shell is Windows PowerShell 5.1, pinned by construction: the runner names
// its absolute path (see windowsPowerShellPath) rather than resolving whatever
// a runner happens to have. Cursor prefers pwsh when it is on PATH, so that
// branch of its resolution chain is NOT exercised here — see the PR's Not
// covered. 5.1 is the interesting one anyway: it is where the payload acquires
// a BOM.
func runCursorWrapper(t *testing.T, wrapper string, entirePresent bool) (stdout, stderr, argvSeen, stdinSeen string, code int) {
	t.Helper()

	recordDir := t.TempDir()
	ranPath := filepath.Join(recordDir, "ran.txt")
	stdinPath := filepath.Join(recordDir, "stdin.txt")
	// findstr /R "^" copies stdin through; `more` pages and can rewrite it.
	stub := "@echo off\r\n" +
		`echo ARGS:%*>> "` + ranPath + `"` + "\r\n" +
		`findstr /R "^" >> "` + stdinPath + `"` + "\r\n" +
		"exit /b 0\r\n"
	setWrapperPATH(t, entirePresent, stub)

	payloadPath := filepath.Join(recordDir, "payload.json")
	if err := os.WriteFile(payloadPath, []byte(cursorHookPayload), 0o600); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	shell := windowsPowerShellPath(t)

	runDir := t.TempDir() // clean CWD so `where` can't find a stray entire next to us
	cmd := exec.CommandContext(t.Context(), shell,
		"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-c", cursorHookScript(payloadPath, wrapper))
	cmd.Dir = runDir
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	code = 0
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("run cursor wrapper: %v", err)
		}
		code = exitErr.ExitCode()
	}
	return outBuf.String(), errBuf.String(), readIfExists(t, ranPath), readIfExists(t, stdinPath), code
}

// readIfExists returns a file's contents, or "" when it was never created.
func readIfExists(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // test-owned temp path
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// cursorHookPayload stands in for a Cursor hook's JSON payload. It must survive
// to the wrapped command's stdin intact enough to be found by substring: under
// Windows PowerShell 5.1 the pipe prepends a UTF-8 BOM and appends a CRLF, so
// an equality check would fail on the transport rather than on the wrapper.
const cursorHookPayload = `{"hook_event_name":"stop","conversation_id":"ENTIRE_PAYLOAD_MARKER"}`

// runWindowsWrapper mirrors Codex's Windows command runner: cmd.exe /C followed
// by the raw, quoted hook command. SysProcAttr.CmdLine is required because
// cmd.exe does not use the standard Windows argv unquoting rules.
func runWindowsWrapper(t *testing.T, wrapper string, entirePresent bool) (string, string, int) {
	t.Helper()

	setWrapperPATH(t, entirePresent, "@exit /b 0\r\n")

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

// TestWindowsWrappers_CursorComposition runs the wrappers through Cursor's own
// Windows hook runner — PowerShell, not cmd.exe — and is why cursor installs
// the native wrapper on every Windows host rather than on a probe's say-so.
//
// The sh subtest is the defect: with no `sh` reachable, the sh wrapper does not
// launch, nothing tells anyone, and the exit code is 0. Cursor reads that as a
// successful hook.
func TestWindowsWrappers_CursorComposition(t *testing.T) {
	// No t.Parallel(): t.Setenv("PATH") forbids it.

	t.Run("cmd wrapper/present runs the command and delivers the payload", func(t *testing.T) {
		out, stderr, argv, stdin, code := runCursorWrapper(
			t, WrapWindowsProductionSilentHookCommand("entire hooks cursor stop"), true)
		if argv == "" {
			t.Fatalf("wrapped command never ran; stdout=%q stderr=%q", out, stderr)
		}
		if !strings.Contains(argv, "hooks cursor stop") {
			t.Errorf("argv = %q, want it to carry `hooks cursor stop`", argv)
		}
		if !strings.Contains(stdin, "ENTIRE_PAYLOAD_MARKER") {
			t.Errorf("hook payload never reached stdin; got %q (stderr=%q)", stdin, stderr)
		}
		if code != 0 {
			t.Errorf("expected exit 0, got %d; stderr=%q", code, stderr)
		}
	})

	t.Run("cmd wrapper/absent skips the command and exits 0", func(t *testing.T) {
		out, stderr, argv, _, code := runCursorWrapper(
			t, WrapWindowsProductionSilentHookCommand("entire hooks cursor stop"), false)
		if argv != "" {
			t.Fatalf("wrapped command must NOT run when entire absent; argv=%q stdout=%q", argv, out)
		}
		if code != 0 {
			t.Errorf("expected exit 0 when entire absent, got %d; stderr=%q", code, stderr)
		}
	})

	// The reason for the gate change. `entire` IS present here; only `sh` is
	// missing, which is the state of a default Windows box — Git for Windows
	// keeps sh.exe in …\Git\usr\bin and …\Git\bin, neither on the machine
	// PATH. The hook simply never runs, and exit 0 means Cursor cannot tell.
	t.Run("sh wrapper/no sh reachable runs nothing and still exits 0", func(t *testing.T) {
		out, stderr, argv, _, code := runCursorWrapper(
			t, WrapProductionSilentHookCommand("entire hooks cursor stop"), true)
		if argv != "" {
			t.Fatalf("sh wrapper ran the command with no sh on PATH; argv=%q", argv)
		}
		if code != 0 {
			t.Errorf("expected the failure to be invisible (exit 0), got %d; stdout=%q stderr=%q", code, out, stderr)
		}
	})
}
