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

func useScoopExecutable(t *testing.T) {
	t.Helper()
	orig := executablePath
	executablePath = func() (string, error) {
		return scoopExecutablePath, nil
	}
	t.Cleanup(func() { executablePath = orig })
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

func TestMaybeAutoUpdate_WindowsScoopAndUnknownNeverAutoRun(t *testing.T) {
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
