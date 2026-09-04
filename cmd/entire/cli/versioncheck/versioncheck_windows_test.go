//go:build windows

package versioncheck

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// scoopExecutablePath is the pre-rename `cli` app dir; scoopEntireExecutablePath
// is the renamed `entire` app dir. The Scoop update command is chosen by which
// app dir the binary runs from, not by version.
const scoopExecutablePath = `C:\Users\test\scoop\apps\cli\current\entire.exe`
const scoopEntireExecutablePath = `C:\Users\test\scoop\apps\entire\current\entire.exe`
const scoopEntireVersionedExecutablePath = `C:\Users\test\scoop\apps\entire\0.10.5\entire.exe`

// windowsLocalBinPath is a direct install.ps1 destination. windowsProgramFilesPath
// is an unknown Windows path that matches none of the install-manager prefixes.
const windowsLocalBinPath = `C:\Users\test\.local\bin\entire.exe`

// windowsMiseExecutablePath is mise's default Windows layout; the mixed case
// and backslashes exercise the normalization the marker match relies on.
const windowsMiseExecutablePath = `C:\Users\test\AppData\Local\mise\installs\entire\1.0.0\bin\entire.exe`
const windowsProgramFilesPath = `C:\Program Files\Entire\entire.exe`

const scoopUpdateCmd = "scoop update entire/entire"
const scoopMigrateCmd = `cmd.exe /D /C "scoop install entire/entire && scoop uninstall entire/cli && scoop reset entire"`

// TestWindowsUpdateCommandForCurrentBinary pins the command per install
// location. The non-Scoop `cli` directory row keeps the rename migration from
// misfiring on a binary that merely lives in a directory named `cli`.
func TestWindowsUpdateCommandForCurrentBinary(t *testing.T) {
	tests := []struct {
		name           string
		currentVersion string
		execPath       string
		want           string
	}{
		{
			name:           "scoop entire app updates in place",
			currentVersion: "1.0.0",
			execPath:       scoopEntireExecutablePath,
			want:           scoopUpdateCmd,
		},
		{
			name:           "scoop entire versioned path updates in place",
			currentVersion: "1.0.0",
			execPath:       scoopEntireVersionedExecutablePath,
			want:           scoopUpdateCmd,
		},
		{
			name:           "scoop cli app routes through package rename regardless of version",
			currentVersion: "1.0.0",
			execPath:       scoopExecutablePath,
			want:           scoopMigrateCmd,
		},
		{
			name:           "scoop marker matches regardless of case",
			currentVersion: "1.0.0",
			execPath:       `C:\Users\test\Scoop\Apps\entire\current\entire.exe`,
			want:           scoopUpdateCmd,
		},
		{
			name:           "non-scoop path in a cli directory uses install.ps1",
			currentVersion: "1.0.0",
			execPath:       `C:\tools\cli\entire.exe`,
			want:           windowsInstallCmd + ` -InstallDir "C:\tools\cli"`,
		},
		{
			name:           "mise default layout uses mise upgrade",
			currentVersion: "1.0.0",
			execPath:       windowsMiseExecutablePath,
			want:           miseUpgradeCmd,
		},
		{
			name:           "windows unknown path stable uses install.ps1",
			currentVersion: "1.0.0",
			execPath:       windowsLocalBinPath,
			want:           windowsInstallCmd + ` -InstallDir "C:\Users\test\.local\bin"`,
		},
		{
			name:           "windows unknown path nightly uses install.ps1 -Channel nightly",
			currentVersion: "1.0.1-nightly.202604101200.abc1234",
			execPath:       windowsLocalBinPath,
			want:           windowsInstallCmd + ` -Channel nightly -InstallDir "C:\Users\test\.local\bin"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setExecutablePath(t, tt.execPath)

			if got := UpdateCommandForCurrentBinary(tt.currentVersion); got != tt.want {
				t.Errorf("UpdateCommandForCurrentBinary() = %q, want %q", got, tt.want)
			}
		})
	}
}

// Without an exec path there is no directory to name, so the hint falls back
// to the bare one-liner and install.ps1 keeps its own default.
func TestWindowsUpdateCommandWithoutExecPathOmitsInstallDir(t *testing.T) {
	setExecutable(t, func() (string, error) { return "", errors.New("not found") })

	if got := UpdateCommandForCurrentBinary("1.0.0"); got != windowsInstallCmd {
		t.Errorf("UpdateCommandForCurrentBinary() = %q, want %q", got, windowsInstallCmd)
	}
}

func isolateWindowsScoopConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("SCOOP", "")
	t.Setenv("SCOOP_GLOBAL", "")
	return dir
}

func TestWindowsScoopRelocatedSCOOPEnv(t *testing.T) {
	isolateWindowsScoopConfig(t)
	t.Setenv("SCOOP", `D:\tools`)
	setExecutablePath(t, `D:\tools\apps\entire\current\entire.exe`)

	if got := UpdateCommandForCurrentBinary("1.0.0"); got != scoopUpdateCmd {
		t.Errorf("UpdateCommandForCurrentBinary() = %q, want %q", got, scoopUpdateCmd)
	}
}

func TestWindowsScoopRelocatedSCOOPEnvCLIAppMigrates(t *testing.T) {
	isolateWindowsScoopConfig(t)
	t.Setenv("SCOOP", `D:\tools`)
	setExecutablePath(t, `D:\tools\apps\cli\current\entire.exe`)

	if got := UpdateCommandForCurrentBinary("1.0.0"); got != scoopMigrateCmd {
		t.Errorf("UpdateCommandForCurrentBinary() = %q, want %q", got, scoopMigrateCmd)
	}
}

func TestWindowsScoopRelocatedSCOOPGlobal(t *testing.T) {
	isolateWindowsScoopConfig(t)
	t.Setenv("SCOOP_GLOBAL", `D:\g`)
	setExecutablePath(t, `D:\g\apps\entire\current\entire.exe`)

	if got := UpdateCommandForCurrentBinary("1.0.0"); got != scoopUpdateCmd {
		t.Errorf("UpdateCommandForCurrentBinary() = %q, want %q", got, scoopUpdateCmd)
	}
}

func TestWindowsScoopRelocatedConfigRootPath(t *testing.T) {
	dir := isolateWindowsScoopConfig(t)
	cfgDir := filepath.Join(dir, "scoop")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(`{"root_path":"D:\\from-config"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	setExecutablePath(t, `D:\from-config\apps\entire\current\entire.exe`)

	if got := UpdateCommandForCurrentBinary("1.0.0"); got != scoopUpdateCmd {
		t.Errorf("UpdateCommandForCurrentBinary() = %q, want %q", got, scoopUpdateCmd)
	}
}

func TestWindowsScoopRelocatedCaseInsensitive(t *testing.T) {
	isolateWindowsScoopConfig(t)
	t.Setenv("SCOOP", `d:\Tools`)
	setExecutablePath(t, `D:\tools\apps\entire\current\entire.exe`)

	if got := UpdateCommandForCurrentBinary("1.0.0"); got != scoopUpdateCmd {
		t.Errorf("UpdateCommandForCurrentBinary() = %q, want %q", got, scoopUpdateCmd)
	}
}

func TestWindowsScoopDefaultMarkerStillMatches(t *testing.T) {
	isolateWindowsScoopConfig(t)
	setExecutablePath(t, `C:\Users\x\scoop\apps\entire\current\entire.exe`)

	if got := UpdateCommandForCurrentBinary("1.0.0"); got != scoopUpdateCmd {
		t.Errorf("UpdateCommandForCurrentBinary() = %q, want %q", got, scoopUpdateCmd)
	}
}

func TestWindowsCommandsNeverNamePOSIXInstallers(t *testing.T) {
	versions := []string{"1.0.0", "1.0.1-nightly.202604101200.abc1234"}
	for _, version := range versions {
		t.Run(version, func(t *testing.T) {
			for _, p := range installProbes {
				assertNoPOSIXInstallerNames(t, p.command(`C:\x`, version))
			}
			assertNoPOSIXInstallerNames(t, fallbackInstallCommand(`C:\x\entire.exe`, version))
		})
	}
}

func assertNoPOSIXInstallerNames(t *testing.T, cmd string) {
	t.Helper()
	for _, needle := range []string{"curl", "brew", "sh "} {
		if strings.Contains(cmd, needle) {
			t.Errorf("windows command %q contains %q", cmd, needle)
		}
	}
}
