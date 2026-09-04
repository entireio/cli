//go:build unix

package versioncheck

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestMaybeAutoUpdate_KillSwitch(t *testing.T) {
	f := newAutoUpdateFixture(t)
	setExecutablePath(t, brewCaskPath)
	t.Setenv(envKillSwitch, "1")

	var buf bytes.Buffer
	action := maybeAutoUpdate(context.Background(), &buf, "1.0.0", "v2.0.0")

	assertPrintOnly(t, f, action, buf.String(), brewUpgradeCmd)
}

func TestMaybeAutoUpdate_NoTTY(t *testing.T) {
	f := newAutoUpdateFixture(t)
	setExecutablePath(t, brewCaskPath)
	// No TTY → maybeAutoUpdate must print the manual hint instead of prompting.
	t.Setenv("ENTIRE_TEST_TTY", "0")

	var buf bytes.Buffer
	action := maybeAutoUpdate(context.Background(), &buf, "1.0.0", "v2.0.0")

	assertPrintOnly(t, f, action, buf.String(), brewUpgradeCmd)
}

func TestMaybeAutoUpdate_CIEnv(t *testing.T) {
	f := newAutoUpdateFixture(t)
	setExecutablePath(t, brewCaskPath)
	// Clear the test override so the real CanPromptInteractively path runs.
	t.Setenv("ENTIRE_TEST_TTY", "")
	t.Setenv("CI", "true")

	var buf bytes.Buffer
	action := maybeAutoUpdate(context.Background(), &buf, "1.0.0", "v2.0.0")

	assertPrintOnly(t, f, action, buf.String(), brewUpgradeCmd)
}

func TestMaybeAutoUpdate_NonTerminalWriter(t *testing.T) {
	f := newAutoUpdateFixture(t)
	setExecutablePath(t, brewCaskPath)
	isTerminalOut = func(_ io.Writer) bool { return false }

	var buf bytes.Buffer
	action := maybeAutoUpdate(context.Background(), &buf, "1.0.0", "v2.0.0")

	assertPrintOnly(t, f, action, buf.String(), brewUpgradeCmd)
}

func TestMaybeAutoUpdate_UserDeclines(t *testing.T) {
	f := newAutoUpdateFixture(t)
	setExecutablePath(t, brewCaskPath)
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
	setExecutablePath(t, brewCaskPath)

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
	setExecutablePath(t, brewCaskPath)
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
// that supports auto-installation. Scoop is absent: Windows is print-only, see
// autoupdate_windows_test.go.
type installerCase struct {
	name     string
	execPath string
	wantCmd  string
}

func autoInstallers() []installerCase {
	return []installerCase{
		{name: "brew", execPath: brewCaskPath, wantCmd: brewUpgradeCmd},
		{name: "mise", execPath: miseExecutablePath, wantCmd: miseUpgradeCmd},
		{name: "unknown_curl_bash", execPath: plainBinPath, wantCmd: "curl -fsSL https://entire.io/install.sh | bash"},
	}
}

// TestMaybeAutoUpdate_AllInstallers_PromptReceivesCorrectCommand verifies
// that the prompt seam is invoked with the right shell command for every
// install manager. The huh.Select itself is exercised by the manual
// smoke script (test-auto.sh); here we only check that the cmd we build
// from UpdateCommandForCurrentBinary() is what reaches the prompt.
func TestMaybeAutoUpdate_AllInstallers_PromptReceivesCorrectCommand(t *testing.T) {
	for _, tt := range autoInstallers() {
		t.Run(tt.name, func(t *testing.T) {
			f := newAutoUpdateFixture(t)
			setExecutablePath(t, tt.execPath)
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
	for _, tt := range autoInstallers() {
		t.Run(tt.name, func(t *testing.T) {
			f := newAutoUpdateFixture(t)
			setExecutablePath(t, tt.execPath)

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
	for _, tt := range autoInstallers() {
		t.Run(tt.name, func(t *testing.T) {
			f := newAutoUpdateFixture(t)
			setExecutablePath(t, tt.execPath)
			t.Setenv(envKillSwitch, "1")

			var buf bytes.Buffer
			action := maybeAutoUpdate(context.Background(), &buf, "1.0.0", "v2.0.0")

			assertPrintOnly(t, f, action, buf.String(), tt.wantCmd)
		})
	}
}

func TestMaybeAutoUpdate_AllInstallers_UserSkips(t *testing.T) {
	for _, tt := range autoInstallers() {
		t.Run(tt.name, func(t *testing.T) {
			f := newAutoUpdateFixture(t)
			setExecutablePath(t, tt.execPath)
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
