package versioncheck

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

// autoUpdateFixture wires the test seams for MaybeAutoUpdate.
type autoUpdateFixture struct {
	installCalls int
	installErr   error
	lastCommand  string
	confirmValue bool
	confirmErr   error
	chooseValue  AutoUpdateAction
	chooseErr    error
}

func newAutoUpdateFixture(t *testing.T) *autoUpdateFixture {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv(envKillSwitch, "")
	// Force interactive mode on by default; individual tests can opt out.
	t.Setenv("ENTIRE_TEST_TTY", "1")

	f := &autoUpdateFixture{confirmValue: true, chooseValue: autoUpdateActionUpdate}

	origRun := runInstaller
	runInstaller = func(_ context.Context, cmd string) error {
		f.installCalls++
		f.lastCommand = cmd
		return f.installErr
	}
	origConfirm := confirmUpdate
	confirmUpdate = func() (bool, error) { return f.confirmValue, f.confirmErr }
	origChoose := chooseBrewUpdate
	chooseBrewUpdate = func(io.Writer) (AutoUpdateAction, error) { return f.chooseValue, f.chooseErr }
	origIsTerminalOut := isTerminalOut
	isTerminalOut = func(_ io.Writer) bool { return true }

	t.Cleanup(func() {
		runInstaller = origRun
		confirmUpdate = origConfirm
		chooseBrewUpdate = origChoose
		isTerminalOut = origIsTerminalOut
	})
	return f
}

// useBrewExecutable points the install-manager detector at a brew cellar path.
func useBrewExecutable(t *testing.T) {
	t.Helper()
	orig := executablePath
	executablePath = func() (string, error) {
		return "/opt/homebrew/Cellar/entire/1.0.0/bin/entire", nil
	}
	t.Cleanup(func() { executablePath = orig })
}

// assertManualHint checks that the "To update entire run:\n  <cmd>" hint
// was printed when the prompt couldn't be shown.
func assertManualHint(t *testing.T, out string) {
	t.Helper()
	if !strings.Contains(out, "To update, run:") {
		t.Errorf("missing manual-update hint: %q", out)
	}
	if !strings.Contains(out, "brew upgrade entire") {
		t.Errorf("manual hint missing installer command: %q", out)
	}
	if strings.Contains(out, "1. Update now") ||
		strings.Contains(out, "2. Skip") ||
		strings.Contains(out, "3. Skip until next version") ||
		strings.Contains(out, "Press enter to continue") {
		t.Errorf("non-interactive output included interactive menu: %q", out)
	}
}

func TestMaybeAutoUpdate_KillSwitch(t *testing.T) {
	f := newAutoUpdateFixture(t)
	useBrewExecutable(t)
	t.Setenv(envKillSwitch, "1")

	var buf bytes.Buffer
	MaybeAutoUpdate(context.Background(), &buf, "1.0.0", "v2.0.0")

	if f.installCalls != 0 {
		t.Errorf("installer called with kill-switch set")
	}
	assertManualHint(t, buf.String())
}

func TestMaybeAutoUpdate_NoTTY(t *testing.T) {
	f := newAutoUpdateFixture(t)
	useBrewExecutable(t)
	// No TTY → MaybeAutoUpdate must print the manual hint instead of prompting.
	t.Setenv("ENTIRE_TEST_TTY", "0")

	var buf bytes.Buffer
	MaybeAutoUpdate(context.Background(), &buf, "1.0.0", "v2.0.0")

	if f.installCalls != 0 {
		t.Errorf("installer called without TTY")
	}
	assertManualHint(t, buf.String())
}

func TestMaybeAutoUpdate_CIEnv(t *testing.T) {
	f := newAutoUpdateFixture(t)
	useBrewExecutable(t)
	// Clear the test override so the real CanPromptInteractively path runs.
	t.Setenv("ENTIRE_TEST_TTY", "")
	t.Setenv("CI", "true")

	var buf bytes.Buffer
	MaybeAutoUpdate(context.Background(), &buf, "1.0.0", "v2.0.0")

	if f.installCalls != 0 {
		t.Errorf("installer called on CI (CI=true)")
	}
	assertManualHint(t, buf.String())
}

func TestMaybeAutoUpdate_NonTerminalWriter(t *testing.T) {
	f := newAutoUpdateFixture(t)
	useBrewExecutable(t)
	isTerminalOut = func(_ io.Writer) bool { return false }

	var buf bytes.Buffer
	MaybeAutoUpdate(context.Background(), &buf, "1.0.0", "v2.0.0")

	if f.installCalls != 0 {
		t.Errorf("installer called with non-terminal output writer")
	}
	assertManualHint(t, buf.String())
}

// TestMaybeAutoUpdate_WindowsUnknownInstallerNoAutoRun verifies that on
// Windows without a detected install manager we never execute the POSIX
// curl-pipe-bash fallback (which would error from cmd.exe). Instead the
// user is pointed at the releases download page.
func TestMaybeAutoUpdate_WindowsUnknownInstallerNoAutoRun(t *testing.T) {
	f := newAutoUpdateFixture(t)
	// Force unknown install manager: point executablePath at a plain
	// Program Files path that matches none of the known prefixes.
	orig := executablePath
	executablePath = func() (string, error) {
		return `C:\Program Files\Entire\entire.exe`, nil
	}
	t.Cleanup(func() { executablePath = orig })

	origGOOS := goos
	goos = goosWindows
	t.Cleanup(func() { goos = origGOOS })

	var buf bytes.Buffer
	MaybeAutoUpdate(context.Background(), &buf, "1.0.0", "v2.0.0")

	if f.installCalls != 0 {
		t.Errorf("installer was auto-run on Windows + unknown install manager")
	}
	out := buf.String()
	if !strings.Contains(out, "download the latest release") ||
		!strings.Contains(out, "github.com/entireio/cli/releases") {
		t.Errorf("expected download-page hint, got: %q", out)
	}
	if strings.Contains(out, "curl -fsSL") {
		t.Errorf("Windows fallback must not show POSIX curl command: %q", out)
	}
}

// TestMaybeAutoUpdate_WindowsScoopStillAutoRuns verifies that a Windows
// scoop install still takes the interactive path — only unknown install
// managers are blocked on Windows.
func TestMaybeAutoUpdate_WindowsScoopStillAutoRuns(t *testing.T) {
	f := newAutoUpdateFixture(t)
	orig := executablePath
	executablePath = func() (string, error) {
		return `C:\Users\test\scoop\apps\cli\current\entire.exe`, nil
	}
	t.Cleanup(func() { executablePath = orig })

	origGOOS := goos
	goos = goosWindows
	t.Cleanup(func() { goos = origGOOS })

	var buf bytes.Buffer
	MaybeAutoUpdate(context.Background(), &buf, "1.0.0", "v2.0.0")

	if f.installCalls != 1 {
		t.Fatalf("scoop install should auto-run on Windows; calls=%d", f.installCalls)
	}
	if f.lastCommand != "scoop update entire/cli" {
		t.Errorf("got %q, want scoop update entire/cli", f.lastCommand)
	}
}

func TestMaybeAutoUpdate_UserDeclines(t *testing.T) {
	f := newAutoUpdateFixture(t)
	useBrewExecutable(t)
	f.chooseValue = autoUpdateActionSkip

	var buf bytes.Buffer
	action := MaybeAutoUpdate(context.Background(), &buf, "1.0.0", "v2.0.0")

	if f.installCalls != 0 {
		t.Errorf("installer called after user declined")
	}
	if action != autoUpdateActionSkip {
		t.Errorf("action = %q, want %q", action, autoUpdateActionSkip)
	}
}

func TestMaybeAutoUpdate_BrewSkipUntilNextVersion(t *testing.T) {
	f := newAutoUpdateFixture(t)
	useBrewExecutable(t)
	f.chooseValue = autoUpdateActionSkipUntilNextVersion

	var buf bytes.Buffer
	action := MaybeAutoUpdate(context.Background(), &buf, "1.0.0", "v2.0.0")

	if f.installCalls != 0 {
		t.Errorf("installer called after skip-until-next-version")
	}
	if action != autoUpdateActionSkipUntilNextVersion {
		t.Errorf("action = %q, want %q", action, autoUpdateActionSkipUntilNextVersion)
	}
	out := buf.String()
	for _, want := range []string{
		"Update available! 1.0.0 -> 2.0.0",
		"Release notes: https://github.com/entireio/cli/releases/tag/v2.0.0",
		"1. Update now (runs `brew upgrade entire`)",
		"2. Skip",
		"3. Skip until next version",
		"Press enter to continue",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output: %q", want, out)
		}
	}
}

func TestMaybeAutoUpdate_HappyPath(t *testing.T) {
	f := newAutoUpdateFixture(t)
	useBrewExecutable(t)

	var buf bytes.Buffer
	action := MaybeAutoUpdate(context.Background(), &buf, "1.0.0", "v2.0.0")

	if f.installCalls != 1 {
		t.Fatalf("installer called %d times, want 1", f.installCalls)
	}
	if f.lastCommand != "brew upgrade entire" {
		t.Errorf("installer got %q, want brew upgrade entire", f.lastCommand)
	}
	if action != autoUpdateActionUpdate {
		t.Errorf("action = %q, want %q", action, autoUpdateActionUpdate)
	}
	if !strings.Contains(buf.String(), "Update complete") {
		t.Errorf("missing success message: %q", buf.String())
	}
}

func TestMaybeAutoUpdate_InstallerFailurePrintedToUser(t *testing.T) {
	f := newAutoUpdateFixture(t)
	useBrewExecutable(t)
	f.installErr = errors.New("boom")

	var buf bytes.Buffer
	MaybeAutoUpdate(context.Background(), &buf, "1.0.0", "v2.0.0")

	if f.installCalls != 1 {
		t.Fatalf("installer called %d times, want 1", f.installCalls)
	}
	out := buf.String()
	if !strings.Contains(out, "Update failed") {
		t.Errorf("missing failure message: %q", out)
	}
	// Failure message should include a manual-retry hint with the exact command.
	if !strings.Contains(out, "Try again later running:") {
		t.Errorf("missing retry hint: %q", out)
	}
	if !strings.Contains(out, "brew upgrade entire") {
		t.Errorf("retry hint missing installer command: %q", out)
	}
}

func TestParseBrewUpdateChoice(t *testing.T) {
	tests := []struct {
		input string
		want  AutoUpdateAction
		ok    bool
	}{
		{input: "", want: autoUpdateActionUpdate, ok: true},
		{input: "\n", want: autoUpdateActionUpdate, ok: true},
		{input: "1", want: autoUpdateActionUpdate, ok: true},
		{input: "2", want: autoUpdateActionSkip, ok: true},
		{input: "3", want: autoUpdateActionSkipUntilNextVersion, ok: true},
		{input: "nope", want: autoUpdateActionSkip, ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, ok := parseBrewUpdateChoice(tt.input)
			if got != tt.want || ok != tt.ok {
				t.Errorf("parseBrewUpdateChoice(%q) = (%q, %v), want (%q, %v)",
					tt.input, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestChooseBrewUpdateFromReader_EmptyEOFSkips(t *testing.T) {
	var buf bytes.Buffer
	action, err := chooseBrewUpdateFromReader(&buf, strings.NewReader(""))
	if err != nil {
		t.Fatalf("chooseBrewUpdateFromReader() error = %v", err)
	}
	if action != autoUpdateActionSkip {
		t.Errorf("action = %q, want %q", action, autoUpdateActionSkip)
	}
}

func TestChooseBrewUpdateFromReader_EnterUpdates(t *testing.T) {
	var buf bytes.Buffer
	action, err := chooseBrewUpdateFromReader(&buf, strings.NewReader("\n"))
	if err != nil {
		t.Fatalf("chooseBrewUpdateFromReader() error = %v", err)
	}
	if action != autoUpdateActionUpdate {
		t.Errorf("action = %q, want %q", action, autoUpdateActionUpdate)
	}
}
