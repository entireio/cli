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
	chooseValue  AutoUpdateAction
	chooseErr    error
	lastCmdStr   string
}

func newAutoUpdateFixture(t *testing.T) *autoUpdateFixture {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv(envKillSwitch, "")
	// Force interactive mode on by default; individual tests can opt out.
	t.Setenv("ENTIRE_TEST_TTY", "1")

	f := &autoUpdateFixture{chooseValue: autoUpdateActionUpdate}

	origRun := runInstaller
	runInstaller = func(_ context.Context, cmd string) error {
		f.installCalls++
		f.lastCommand = cmd
		return f.installErr
	}
	origChoose := chooseUpdate
	chooseUpdate = func(_ context.Context, _, _, cmdStr string) (AutoUpdateAction, error) {
		f.lastCmdStr = cmdStr
		return f.chooseValue, f.chooseErr
	}
	origIsTerminalOut := isTerminalOut
	isTerminalOut = func(_ io.Writer) bool { return true }

	t.Cleanup(func() {
		runInstaller = origRun
		chooseUpdate = origChoose
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

// useMiseExecutable points the install-manager detector at a mise install path.
func useMiseExecutable(t *testing.T) {
	t.Helper()
	orig := executablePath
	executablePath = func() (string, error) {
		return "/home/user/.local/share/mise/installs/entire/1.0.0/bin/entire", nil
	}
	t.Cleanup(func() { executablePath = orig })
}

// useScoopExecutable points the install-manager detector at a scoop install path.
func useScoopExecutable(t *testing.T) {
	t.Helper()
	orig := executablePath
	executablePath = func() (string, error) {
		return `C:\Users\test\scoop\apps\cli\current\entire.exe`, nil
	}
	t.Cleanup(func() { executablePath = orig })
}

// useUnknownExecutable points the install-manager detector at a plain path
// with no recognised manager prefix (curl-bash fallback).
func useUnknownExecutable(t *testing.T) {
	t.Helper()
	orig := executablePath
	executablePath = func() (string, error) {
		return "/usr/local/bin/entire", nil
	}
	t.Cleanup(func() { executablePath = orig })
}

// pinNonWindowsGOOS pins the goos seam to a non-Windows value so the
// table-driven tests below pass on Windows hosts. canAutoInstall() blocks
// brew and the curl-bash fallback on Windows; without this pin those
// installer cases would short-circuit to the downloads-page path.
func pinNonWindowsGOOS(t *testing.T) {
	t.Helper()
	orig := goos
	goos = "darwin"
	t.Cleanup(func() { goos = orig })
}

// assertManualHint checks that the "To update, run:\n  <cmd>" hint
// was printed when the prompt couldn't be shown, and that the wantCmd
// installer command is included.
func assertManualHint(t *testing.T, out, wantCmd string) {
	t.Helper()
	if !strings.Contains(out, "To update, run:") {
		t.Errorf("missing manual-update hint: %q", out)
	}
	if !strings.Contains(out, wantCmd) {
		t.Errorf("manual hint missing installer command %q: %q", wantCmd, out)
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
	assertManualHint(t, buf.String(), "brew upgrade entire")
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
	assertManualHint(t, buf.String(), "brew upgrade entire")
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
	assertManualHint(t, buf.String(), "brew upgrade entire")
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
	assertManualHint(t, buf.String(), "brew upgrade entire")
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
	useScoopExecutable(t)

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

// installerCase covers the same prompt contract for every install manager
// that supports auto-installation.
type installerCase struct {
	name    string
	setup   func(*testing.T)
	wantCmd string
}

func nonWindowsAutoInstallers() []installerCase {
	return []installerCase{
		{name: "brew", setup: useBrewExecutable, wantCmd: "brew upgrade entire"},
		{name: "mise", setup: useMiseExecutable, wantCmd: "mise upgrade entire"},
		{name: "scoop", setup: useScoopExecutable, wantCmd: "scoop update entire/cli"},
		{name: "unknown_curl_bash", setup: useUnknownExecutable, wantCmd: "curl -fsSL https://entire.io/install.sh | bash"},
	}
}

// TestMaybeAutoUpdate_AllInstallers_PromptReceivesCorrectCommand verifies
// that the prompt seam is invoked with the right shell command for every
// install manager. The huh.Select itself is exercised by the manual
// smoke script (test-auto.sh); here we only check that the cmd we build
// from updateCommand() is what reaches the prompt.
func TestMaybeAutoUpdate_AllInstallers_PromptReceivesCorrectCommand(t *testing.T) {
	pinNonWindowsGOOS(t)
	for _, tt := range nonWindowsAutoInstallers() {
		t.Run(tt.name, func(t *testing.T) {
			f := newAutoUpdateFixture(t)
			tt.setup(t)
			f.chooseValue = autoUpdateActionSkipUntilNextVersion

			var buf bytes.Buffer
			action := MaybeAutoUpdate(context.Background(), &buf, "1.0.0", "v2.0.0")

			if f.installCalls != 0 {
				t.Errorf("installer called after skip-until-next-version")
			}
			if action != autoUpdateActionSkipUntilNextVersion {
				t.Errorf("action = %q, want %q", action, autoUpdateActionSkipUntilNextVersion)
			}
			if f.lastCmdStr != tt.wantCmd {
				t.Errorf("prompt got cmd %q, want %q", f.lastCmdStr, tt.wantCmd)
			}
		})
	}
}

func TestMaybeAutoUpdate_AllInstallers_HappyPathRunsInstaller(t *testing.T) {
	pinNonWindowsGOOS(t)
	for _, tt := range nonWindowsAutoInstallers() {
		t.Run(tt.name, func(t *testing.T) {
			f := newAutoUpdateFixture(t)
			tt.setup(t)

			var buf bytes.Buffer
			action := MaybeAutoUpdate(context.Background(), &buf, "1.0.0", "v2.0.0")

			if f.installCalls != 1 {
				t.Fatalf("installer called %d times, want 1", f.installCalls)
			}
			if f.lastCommand != tt.wantCmd {
				t.Errorf("installer got %q, want %q", f.lastCommand, tt.wantCmd)
			}
			if action != autoUpdateActionUpdate {
				t.Errorf("action = %q, want %q", action, autoUpdateActionUpdate)
			}
			if !strings.Contains(buf.String(), "Update complete") {
				t.Errorf("missing success message: %q", buf.String())
			}
		})
	}
}

func TestMaybeAutoUpdate_AllInstallers_KillSwitchPrintsManualHint(t *testing.T) {
	pinNonWindowsGOOS(t)
	for _, tt := range nonWindowsAutoInstallers() {
		t.Run(tt.name, func(t *testing.T) {
			f := newAutoUpdateFixture(t)
			tt.setup(t)
			t.Setenv(envKillSwitch, "1")

			var buf bytes.Buffer
			MaybeAutoUpdate(context.Background(), &buf, "1.0.0", "v2.0.0")

			if f.installCalls != 0 {
				t.Errorf("installer called with kill-switch set")
			}
			assertManualHint(t, buf.String(), tt.wantCmd)
		})
	}
}

func TestMaybeAutoUpdate_AllInstallers_UserSkips(t *testing.T) {
	pinNonWindowsGOOS(t)
	for _, tt := range nonWindowsAutoInstallers() {
		t.Run(tt.name, func(t *testing.T) {
			f := newAutoUpdateFixture(t)
			tt.setup(t)
			f.chooseValue = autoUpdateActionSkip

			var buf bytes.Buffer
			action := MaybeAutoUpdate(context.Background(), &buf, "1.0.0", "v2.0.0")

			if f.installCalls != 0 {
				t.Errorf("installer called after user chose skip")
			}
			if action != autoUpdateActionSkip {
				t.Errorf("action = %q, want %q", action, autoUpdateActionSkip)
			}
		})
	}
}
