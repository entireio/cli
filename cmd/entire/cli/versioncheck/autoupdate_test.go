package versioncheck

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

// autoUpdateFixture wires the test seams for maybeAutoUpdate.
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
// table-driven tests below exercise the interactive/auto-run path rather
// than the Windows print-only branch.
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
	if !strings.Contains(out, "  "+wantCmd) {
		t.Errorf("manual hint missing installer command %q: %q", wantCmd, out)
	}
}

func TestMaybeAutoUpdate_KillSwitch(t *testing.T) {
	f := newAutoUpdateFixture(t)
	useBrewExecutable(t)
	t.Setenv(envKillSwitch, "1")

	var buf bytes.Buffer
	maybeAutoUpdate(context.Background(), &buf, "1.0.0", "v2.0.0")

	if f.installCalls != 0 {
		t.Errorf("installer called with kill-switch set")
	}
	assertManualHint(t, buf.String(), brewUpgradeCmd)
}

func TestMaybeAutoUpdate_NoTTY(t *testing.T) {
	f := newAutoUpdateFixture(t)
	useBrewExecutable(t)
	// No TTY → maybeAutoUpdate must print the manual hint instead of prompting.
	t.Setenv("ENTIRE_TEST_TTY", "0")

	var buf bytes.Buffer
	maybeAutoUpdate(context.Background(), &buf, "1.0.0", "v2.0.0")

	if f.installCalls != 0 {
		t.Errorf("installer called without TTY")
	}
	assertManualHint(t, buf.String(), brewUpgradeCmd)
}

func TestMaybeAutoUpdate_CIEnv(t *testing.T) {
	f := newAutoUpdateFixture(t)
	useBrewExecutable(t)
	// Clear the test override so the real CanPromptInteractively path runs.
	t.Setenv("ENTIRE_TEST_TTY", "")
	t.Setenv("CI", "true")

	var buf bytes.Buffer
	maybeAutoUpdate(context.Background(), &buf, "1.0.0", "v2.0.0")

	if f.installCalls != 0 {
		t.Errorf("installer called on CI (CI=true)")
	}
	assertManualHint(t, buf.String(), brewUpgradeCmd)
}

func TestMaybeAutoUpdate_NonTerminalWriter(t *testing.T) {
	f := newAutoUpdateFixture(t)
	useBrewExecutable(t)
	isTerminalOut = func(_ io.Writer) bool { return false }

	var buf bytes.Buffer
	maybeAutoUpdate(context.Background(), &buf, "1.0.0", "v2.0.0")

	if f.installCalls != 0 {
		t.Errorf("installer called with non-terminal output writer")
	}
	assertManualHint(t, buf.String(), brewUpgradeCmd)
}

// TestMaybeAutoUpdate_WindowsUnknownInstallerNoAutoRun verifies that on
// Windows without Scoop or mise we print the install.ps1 one-liner and
// never auto-run it (a running entire.exe cannot replace itself). The
// POSIX curl fallback must not appear.
func TestMaybeAutoUpdate_WindowsUnknownInstallerNoAutoRun(t *testing.T) {
	tests := []struct {
		name           string
		currentVersion string
		wantCmd        string
	}{
		{
			name:           "stable",
			currentVersion: "1.0.0",
			wantCmd:        windowsInstallCmd,
		},
		{
			name:           "nightly",
			currentVersion: "1.0.1-nightly.202604101200.abc1234",
			wantCmd:        windowsInstallNightlyCmd,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newAutoUpdateFixture(t)
			orig := executablePath
			executablePath = func() (string, error) {
				return windowsProgramFilesPath, nil
			}
			t.Cleanup(func() { executablePath = orig })

			origGOOS := goos
			goos = goosWindows
			t.Cleanup(func() { goos = origGOOS })

			var buf bytes.Buffer
			action := maybeAutoUpdate(context.Background(), &buf, tt.currentVersion, "v2.0.0")

			if f.installCalls != 0 {
				t.Errorf("installer was auto-run on Windows + unknown install manager")
			}
			if action != autoUpdateActionSkip {
				t.Errorf("action = %q, want %q", action, autoUpdateActionSkip)
			}
			out := buf.String()
			if !strings.Contains(out, "when entire is not running") {
				t.Errorf("missing Windows manual-run hint: %q", out)
			}
			if !strings.Contains(out, "  "+tt.wantCmd) {
				t.Errorf("manual hint missing command %q: %q", tt.wantCmd, out)
			}
			if strings.Contains(out, "curl -fsSL") {
				t.Errorf("Windows fallback must not show POSIX curl command: %q", out)
			}
			if strings.Contains(out, "download the latest release") {
				t.Errorf("Windows unknown installer must not point at the releases page: %q", out)
			}
		})
	}
}

// TestMaybeAutoUpdate_WindowsNeverAutoRuns verifies that on Windows the update
// is never auto-run — a running entire.exe can't replace its own shim — so the
// command is printed for the user to run once entire has exited. This holds for
// every auto-installable manager on Windows, not just Scoop: mise is covered so
// narrowing the branch back to Scoop can't silently regress it.
func TestMaybeAutoUpdate_WindowsNeverAutoRuns(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(*testing.T)
		wantCmd string
	}{
		{
			name:    "scoop cli app prints migration command",
			setup:   useScoopExecutable,
			wantCmd: `cmd.exe /D /C "scoop install entire/entire && scoop uninstall entire/cli && scoop reset entire"`,
		},
		{
			name:    "mise prints upgrade command",
			setup:   useMiseExecutable,
			wantCmd: "mise upgrade entire",
		},
		{
			name: "unknown prints install.ps1 command",
			setup: func(t *testing.T) {
				t.Helper()
				orig := executablePath
				executablePath = func() (string, error) {
					return windowsLocalBinPath, nil
				}
				t.Cleanup(func() { executablePath = orig })
			},
			wantCmd: windowsInstallCmd,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newAutoUpdateFixture(t)
			tt.setup(t)

			origGOOS := goos
			goos = goosWindows
			t.Cleanup(func() { goos = origGOOS })

			var buf bytes.Buffer
			action := maybeAutoUpdate(context.Background(), &buf, "1.0.0", "v2.0.0")

			if f.installCalls != 0 {
				t.Fatalf("installer must not auto-run on Windows; calls=%d", f.installCalls)
			}
			// Plain skip, not skip-until-next-version: Windows gets no prompt,
			// so there is no per-version choice to persist.
			if action != autoUpdateActionSkip {
				t.Errorf("action = %q, want %q", action, autoUpdateActionSkip)
			}
			out := buf.String()
			if !strings.Contains(out, "when entire is not running") {
				t.Errorf("missing Windows manual-run hint: %q", out)
			}
			if !strings.Contains(out, "  "+tt.wantCmd) {
				t.Errorf("manual hint missing command %q: %q", tt.wantCmd, out)
			}
		})
	}
}

func TestMaybeAutoUpdate_UserDeclines(t *testing.T) {
	f := newAutoUpdateFixture(t)
	useBrewExecutable(t)
	f.chooseValue = autoUpdateActionSkip

	var buf bytes.Buffer
	action := maybeAutoUpdate(context.Background(), &buf, "1.0.0", "v2.0.0")

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
	action := maybeAutoUpdate(context.Background(), &buf, "1.0.0", "v2.0.0")

	if f.installCalls != 1 {
		t.Fatalf("installer called %d times, want 1", f.installCalls)
	}
	if f.lastCommand != brewUpgradeCmd {
		t.Errorf("installer got %q, want %q", f.lastCommand, brewUpgradeCmd)
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
	maybeAutoUpdate(context.Background(), &buf, "1.0.0", "v2.0.0")

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
	if !strings.Contains(out, brewUpgradeCmd) {
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
	// Scoop is intentionally absent: it is a Windows-only installer, and on
	// Windows the update is print-only (never auto-run). Its command building is
	// covered by TestUpdateCommand and its print-only path by
	// TestMaybeAutoUpdate_WindowsNeverAutoRuns.
	return []installerCase{
		{name: "brew", setup: useBrewExecutable, wantCmd: brewUpgradeCmd},
		{name: "mise", setup: useMiseExecutable, wantCmd: "mise upgrade entire"},
		{name: "unknown_curl_bash", setup: useUnknownExecutable, wantCmd: "curl -fsSL https://entire.io/install.sh | bash"},
	}
}

// TestMaybeAutoUpdate_AllInstallers_PromptReceivesCorrectCommand verifies
// that the prompt seam is invoked with the right shell command for every
// install manager. The huh.Select itself is exercised by the manual
// smoke script (test-auto.sh); here we only check that the cmd we build
// from UpdateCommandForCurrentBinary() is what reaches the prompt.
func TestMaybeAutoUpdate_AllInstallers_PromptReceivesCorrectCommand(t *testing.T) {
	pinNonWindowsGOOS(t)
	for _, tt := range nonWindowsAutoInstallers() {
		t.Run(tt.name, func(t *testing.T) {
			f := newAutoUpdateFixture(t)
			tt.setup(t)
			f.chooseValue = autoUpdateActionSkipUntilNextVersion

			var buf bytes.Buffer
			action := maybeAutoUpdate(context.Background(), &buf, "1.0.0", "v2.0.0")

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
			action := maybeAutoUpdate(context.Background(), &buf, "1.0.0", "v2.0.0")

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
			maybeAutoUpdate(context.Background(), &buf, "1.0.0", "v2.0.0")

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
			action := maybeAutoUpdate(context.Background(), &buf, "1.0.0", "v2.0.0")

			if f.installCalls != 0 {
				t.Errorf("installer called after user chose skip")
			}
			if action != autoUpdateActionSkip {
				t.Errorf("action = %q, want %q", action, autoUpdateActionSkip)
			}
		})
	}
}
