//go:build windows

package versioncheck

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestRealRunInstaller_WindowsNotImplemented(t *testing.T) {
	t.Parallel()
	err := realRunInstaller(context.Background(), "echo hi")
	want := "auto-update is not implemented on Windows"
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
}

// TestMaybeAutoUpdate_WindowsNeverAutoRuns verifies that on Windows the update
// is never auto-run — a running entire.exe cannot replace its own shim — so
// the command is printed for the user to run once entire has exited. The POSIX
// curl fallback and the releases page must not appear.
func TestMaybeAutoUpdate_WindowsNeverAutoRuns(t *testing.T) {
	tests := []struct {
		name           string
		execPath       string
		currentVersion string
		wantCmd        string
	}{
		{
			name:           "scoop cli app prints migration command",
			execPath:       scoopExecutablePath,
			currentVersion: "1.0.0",
			wantCmd:        scoopMigrateCmd,
		},
		{
			name:           "mise prints mise upgrade",
			execPath:       windowsMiseExecutablePath,
			currentVersion: "1.0.0",
			wantCmd:        miseUpgradeCmd,
		},
		{
			name:           "unknown stable prints install.ps1",
			execPath:       windowsProgramFilesPath,
			currentVersion: "1.0.0",
			wantCmd:        windowsInstallCmd,
		},
		{
			name:           "unknown nightly prints install.ps1 -Channel nightly",
			execPath:       windowsLocalBinPath,
			currentVersion: "1.0.1-nightly.202604101200.abc1234",
			wantCmd:        windowsInstallNightlyCmd,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newAutoUpdateFixture(t)
			setExecutablePath(t, tt.execPath)

			var buf bytes.Buffer
			action := maybeAutoUpdate(context.Background(), &buf, tt.currentVersion, "v2.0.0")

			out := buf.String()
			assertPrintOnly(t, f, action, out, tt.wantCmd)
			if !strings.Contains(out, "in PowerShell when entire is not running") {
				t.Errorf("missing Windows manual-run hint: %q", out)
			}
			if strings.Contains(out, "curl -fsSL") {
				t.Errorf("Windows must not show the POSIX curl command: %q", out)
			}
			if strings.Contains(out, "download the latest release") {
				t.Errorf("Windows must not point at the releases page: %q", out)
			}
		})
	}
}
