//go:build unix

package versioncheck

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// plainBinPath is a POSIX install under no recognized install manager.
const plainBinPath = "/usr/local/bin/entire"

func TestUpdateCommandForCurrentBinary_Unix(t *testing.T) {
	tests := []struct {
		name           string
		currentVersion string
		execPath       func() (string, error)
		want           string
	}{
		{
			name:           "homebrew stable caskroom path uses brew command",
			currentVersion: "1.0.0",
			execPath:       func() (string, error) { return "/opt/homebrew/Caskroom/entire/1.0.0/entire", nil },
			want:           brewUpgradeCmd,
		},
		{
			name:           "homebrew stable prefix bin path uses brew command",
			currentVersion: "1.0.0",
			execPath:       func() (string, error) { return "/opt/homebrew/bin/entire", nil },
			want:           brewUpgradeCmd,
		},
		{
			name:           "homebrew nightly path uses brew command",
			currentVersion: "1.0.1-nightly.202604101200.abc1234",
			execPath:       func() (string, error) { return "/opt/homebrew/bin/entire", nil },
			want:           "brew upgrade --yes entire@nightly",
		},
		{
			name:           "linuxbrew path",
			currentVersion: "1.0.0",
			execPath:       func() (string, error) { return "/home/linuxbrew/.linuxbrew/bin/entire", nil },
			want:           brewUpgradeCmd,
		},
		{
			name:           "unknown path stable falls back to stable curl command",
			currentVersion: "1.0.0",
			execPath:       func() (string, error) { return plainBinPath, nil },
			want:           "curl -fsSL https://entire.io/install.sh | bash",
		},
		{
			name:           "unknown path nightly falls back to nightly curl command",
			currentVersion: "1.0.1-nightly.202604101200.abc1234",
			execPath:       func() (string, error) { return plainBinPath, nil },
			want:           "curl -fsSL https://entire.io/install.sh | bash -s -- --channel nightly",
		},
		{
			name:           "executable error falls back to stable curl command",
			currentVersion: "1.0.0",
			execPath:       func() (string, error) { return "", errors.New("not found") },
			want:           "curl -fsSL https://entire.io/install.sh | bash",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setExecutable(t, tt.execPath)

			if got := UpdateCommandForCurrentBinary(tt.currentVersion); got != tt.want {
				t.Errorf("UpdateCommandForCurrentBinary() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestUnixCommandsNeverNameWindowsInstallers(t *testing.T) {
	t.Parallel()
	versions := []string{"1.0.0", "1.0.1-nightly.202604101200.abc1234"}
	for _, version := range versions {
		t.Run(version, func(t *testing.T) {
			t.Parallel()
			for _, p := range installProbes {
				assertNoWindowsInstallerNames(t, p.command("/x", version))
			}
			assertNoWindowsInstallerNames(t, fallbackInstallCommand(version))
		})
	}
}

func TestUnixMiseRootSymlink(t *testing.T) {
	tmp := t.TempDir()
	realDir := filepath.Join(tmp, "real")
	link := filepath.Join(tmp, "link")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MISE_INSTALLS_DIR", link)
	t.Setenv("MISE_DATA_DIR", "")
	execPath := filepath.Join(realDir, "entire", "1.0.0", "bin", "entire")
	if err := os.MkdirAll(filepath.Dir(execPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(execPath, []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}
	setExecutablePath(t, execPath)

	if got := UpdateCommandForCurrentBinary("1.0.0"); got != miseUpgradeCmd {
		t.Errorf("UpdateCommandForCurrentBinary() = %q, want %q", got, miseUpgradeCmd)
	}
}

func assertNoWindowsInstallerNames(t *testing.T, cmd string) {
	t.Helper()
	for _, needle := range []string{"scoop", "irm", "install.ps1"} {
		if strings.Contains(cmd, needle) {
			t.Errorf("unix command %q contains %q", cmd, needle)
		}
	}
}
