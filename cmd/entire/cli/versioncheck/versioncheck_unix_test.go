//go:build unix

package versioncheck

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// plainBinPath is a POSIX install under no recognized install manager.
const plainBinPath = "/usr/local/bin/entire"

// brewUpgradeCmd is the install command produced for any brew-installed
// binary on a stable channel. Hoisted to a const so tests can reference
// it without tripping goconst on repeated string literals.
const brewUpgradeCmd = "brew upgrade --yes entire"

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
			assertNoWindowsInstallerNames(t, fallbackInstallCommand("/x/entire", version))
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

// brew, mise, and the curl|bash one-liner all run in any POSIX shell, so
// messages that print an update command name no shell here.
func TestUpdateCommandShell_Unix(t *testing.T) {
	t.Parallel()

	if got := UpdateCommandShell(); got != "" {
		t.Errorf("UpdateCommandShell() = %q, want %q", got, "")
	}
}

func TestCheckAndNotify_BrewSkipUntilNextVersionCachesLatest(t *testing.T) {
	server := newVersionServer(t, "v2.0.0")
	cmd, _ := setupCheckAndNotifyTest(t, server.URL)
	f := newAutoUpdateFixture(t)
	setExecutablePath(t, brewCaskPath)
	f.chooseValue = autoUpdateActionSkipUntilNextVersion

	CheckAndNotify(context.Background(), cmd.OutOrStdout(), "1.0.0")

	if f.installCalls != 0 {
		t.Fatalf("installer called %d times, want 0", f.installCalls)
	}
	cache, err := loadCache()
	if err != nil {
		t.Fatalf("loadCache() error = %v", err)
	}
	if cache.SkippedVersion != "v2.0.0" {
		t.Errorf("SkippedVersion = %q, want v2.0.0", cache.SkippedVersion)
	}
	if f.lastCmdStr != brewUpgradeCmd {
		t.Errorf("prompt got cmd %q, want %q", f.lastCmdStr, brewUpgradeCmd)
	}
}

// TestCheckAndNotify_MiseSkipUntilNextVersionCachesLatest verifies the
// skip-until-next-version persistence works for non-brew installers too.
// The cache flow is installer-agnostic; this locks that contract in.
func TestCheckAndNotify_MiseSkipUntilNextVersionCachesLatest(t *testing.T) {
	server := newVersionServer(t, "v2.0.0")
	cmd, _ := setupCheckAndNotifyTest(t, server.URL)
	f := newAutoUpdateFixture(t)
	setExecutablePath(t, miseExecutablePath)
	f.chooseValue = autoUpdateActionSkipUntilNextVersion

	CheckAndNotify(context.Background(), cmd.OutOrStdout(), "1.0.0")

	if f.installCalls != 0 {
		t.Fatalf("installer called %d times, want 0", f.installCalls)
	}
	cache, err := loadCache()
	if err != nil {
		t.Fatalf("loadCache() error = %v", err)
	}
	if cache.SkippedVersion != "v2.0.0" {
		t.Errorf("SkippedVersion = %q, want v2.0.0", cache.SkippedVersion)
	}
	if f.lastCmdStr != miseUpgradeCmd {
		t.Errorf("prompt got cmd %q, want %q", f.lastCmdStr, miseUpgradeCmd)
	}
}

func TestCheckAndNotify_InstallerFailureKeepsCacheFresh(t *testing.T) {
	server := newVersionServer(t, "v2.0.0")
	cmd, buf := setupCheckAndNotifyTest(t, server.URL)

	// Simulate an interactive user who accepts the upgrade prompt, and an
	// installer that fails (e.g. brew upgrade blew up mid-run).
	t.Setenv("ENTIRE_TEST_TTY", "1")
	setExecutablePath(t, brewCaskPath)

	origChoose := chooseUpdate
	chooseUpdate = func(_ context.Context, _, _, _ string) (AutoUpdateAction, error) {
		return autoUpdateActionUpdate, nil
	}
	t.Cleanup(func() { chooseUpdate = origChoose })

	origRun := runInstaller
	runInstaller = func(_ context.Context, _ string) error { return errors.New("boom") }
	t.Cleanup(func() { runInstaller = origRun })

	origIsTerminalOut := isTerminalOut
	isTerminalOut = func(_ io.Writer) bool { return true }
	t.Cleanup(func() { isTerminalOut = origIsTerminalOut })

	CheckAndNotify(context.Background(), cmd.OutOrStdout(), "1.0.0")

	// User sees the failure message with a manual-retry hint.
	if !strings.Contains(buf.String(), "Try again later running:") {
		t.Errorf("missing retry hint in output: %q", buf.String())
	}

	// Cache must remain bumped: we don't want to re-prompt every invocation
	// while the upstream issue is still in place. The user already has the
	// hint with the exact command to run manually.
	cache, err := loadCache()
	if err != nil {
		t.Fatalf("loadCache() error = %v", err)
	}
	if cache.LastCheckTime.IsZero() {
		t.Errorf("cache LastCheckTime was reset after installer failure; want fresh bump")
	}
	if time.Since(cache.LastCheckTime) > time.Minute {
		t.Errorf("cache LastCheckTime not fresh after installer failure: %v", cache.LastCheckTime)
	}
}

// TestUnixBrewBeatsAMiseRootCoveringTheSamePath pins probe precedence, which
// is a product invariant and not only a test-support one: a developer can have
// entire installed by brew *and* mise relocated somewhere broad enough to
// cover the cask path (MISE_INSTALLS_DIR is the one probe variable used
// verbatim, so /opt is enough), and that user must be told to run brew.
// installProbes orders brewProbe first and brewProbe matches a cask path by
// marker, so mise cannot claim it however wide its root is.
func TestUnixBrewBeatsAMiseRootCoveringTheSamePath(t *testing.T) {
	t.Setenv("MISE_INSTALLS_DIR", "/opt")

	norm := normalizePath(brewCaskPath)
	if !miseProbe.matches(norm) {
		t.Fatalf("precondition: miseProbe should also match %q under MISE_INSTALLS_DIR=/opt; "+
			"without a contending probe this test no longer pins precedence", norm)
	}

	setExecutablePath(t, brewCaskPath)
	if got := UpdateCommandForCurrentBinary("1.0.0"); got != brewUpgradeCmd {
		t.Errorf("UpdateCommandForCurrentBinary() = %q, want %q; brew must be probed before mise",
			got, brewUpgradeCmd)
	}
}
