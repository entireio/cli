//go:build windows

package versioncheck

import (
	"errors"
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
const windowsProgramFilesPath = `C:\Program Files\Entire\entire.exe`

// TestWindowsScoopAppName pins the signal the rename migration keys off. The ""
// results matter most: they are what keeps the `== "cli"` comparison in
// UpdateCommandForCurrentBinary from misfiring on a non-Scoop binary that happens to live in a
// directory named `cli`.
func TestWindowsScoopAppName(t *testing.T) {
	tests := []struct {
		name     string
		execPath func() (string, error)
		want     string
	}{
		{
			name:     "pre-rename cli app dir",
			execPath: func() (string, error) { return scoopExecutablePath, nil },
			want:     "cli",
		},
		{
			name:     "renamed entire app dir",
			execPath: func() (string, error) { return scoopEntireExecutablePath, nil },
			want:     "entire",
		},
		{
			name:     "junction-resolved versioned entire app dir",
			execPath: func() (string, error) { return scoopEntireVersionedExecutablePath, nil },
			want:     "entire",
		},
		{
			name:     "non-scoop path in a cli directory",
			execPath: func() (string, error) { return `C:\tools\cli\entire.exe`, nil },
			want:     "",
		},
		{
			name:     "posix install",
			execPath: func() (string, error) { return plainBinPath, nil },
			want:     "",
		},
		{
			name:     "executable path unavailable",
			execPath: func() (string, error) { return "", errors.New("not found") },
			want:     "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := executablePath
			executablePath = tt.execPath
			t.Cleanup(func() { executablePath = original })

			if got := scoopAppName(); got != tt.want {
				t.Errorf("scoopAppName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWindowsUpdateCommandForCurrentBinary(t *testing.T) {
	tests := []struct {
		name           string
		currentVersion string
		execPath       func() (string, error)
		want           string
	}{
		{
			name:           "scoop entire app updates in place",
			currentVersion: "1.0.0",
			execPath:       func() (string, error) { return scoopEntireExecutablePath, nil },
			want:           "scoop update entire/entire",
		},
		{
			name:           "scoop entire versioned path updates in place",
			currentVersion: "1.0.0",
			execPath:       func() (string, error) { return scoopEntireVersionedExecutablePath, nil },
			want:           "scoop update entire/entire",
		},
		{
			name:           "scoop cli app routes through package rename regardless of version",
			currentVersion: "1.0.0",
			execPath:       func() (string, error) { return scoopExecutablePath, nil },
			want:           `cmd.exe /D /C "scoop install entire/entire && scoop uninstall entire/cli && scoop reset entire"`,
		},
		{
			name:           "windows unknown path stable uses install.ps1",
			currentVersion: "1.0.0",
			execPath:       func() (string, error) { return windowsLocalBinPath, nil },
			want:           windowsInstallCmd,
		},
		{
			name:           "windows unknown path nightly uses install.ps1 -Channel nightly",
			currentVersion: "1.0.1-nightly.202604101200.abc1234",
			execPath:       func() (string, error) { return windowsLocalBinPath, nil },
			want:           windowsInstallNightlyCmd,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := executablePath
			executablePath = tt.execPath
			t.Cleanup(func() { executablePath = original })

			if got := UpdateCommandForCurrentBinary(tt.currentVersion); got != tt.want {
				t.Errorf("UpdateCommandForCurrentBinary() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWindowsCommandsNeverNamePOSIXInstallers(t *testing.T) {
	versions := []string{"1.0.0", "1.0.1-nightly.202604101200.abc1234"}
	for _, version := range versions {
		t.Run(version, func(t *testing.T) {
			for _, p := range installProbes {
				assertNoPOSIXInstallerNames(t, p.command(`C:\x`, version))
			}
			assertNoPOSIXInstallerNames(t, fallbackInstallCommand(version))
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
