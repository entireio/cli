package versioncheck

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestIsOutdated(t *testing.T) {
	tests := []struct {
		current string
		latest  string
		want    bool
		desc    string
	}{
		// Standard semver cases
		{"1.0.0", "1.0.1", true, "patch version bump"},
		{"1.0.0", "1.1.0", true, "minor version bump"},
		{"1.0.0", "2.0.0", true, "major version bump"},
		{"1.0.1", "1.0.0", false, "current is newer"},
		{"2.0.0", "1.9.9", false, "current major is higher"},
		{"1.0.0", "1.0.0", false, "same version"},

		// v-prefix handling
		{"v1.0.0", "v1.0.1", true, "with v prefix"},
		{"v1.0.0", "1.0.1", true, "mixed v prefix"},
		{"1.0.0", "v1.0.1", true, "mixed v prefix reversed"},

		// Pre-release versions (semver uses hyphen)
		{"1.0.0-rc1", "1.0.0", true, "prerelease in current"},
		{"1.0.0", "1.0.1-rc1", true, "prerelease in latest is still newer"},
		{"1.0.0-dev-xxx", "1.0.1", false, "dev build skips version check"},

		// Nightly-vs-nightly comparisons (same channel)
		{"0.5.3-nightly.202604051159.abc1234", "0.5.3-nightly.202604061200.def5678", true, "older nightly is outdated by newer nightly"},
		{"0.5.3-nightly.202604061200.def5678", "0.5.3-nightly.202604051159.abc1234", false, "newer nightly is not outdated by older nightly"},
		{"0.5.3-nightly.202604061200.abc1234", "0.5.3-nightly.202604061200.abc1234", false, "same nightly is not outdated"},
	}

	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			got := isOutdated(tt.current, tt.latest)
			if got != tt.want {
				t.Errorf("isOutdated(%q, %q) = %v, want %v", tt.current, tt.latest, got, tt.want)
			}
		})
	}
}

func TestIsNightly(t *testing.T) {
	t.Parallel()
	tests := []struct {
		version string
		want    bool
	}{
		{"0.5.3-nightly.202604061200.abc1234", true},
		{"v0.5.3-nightly.202604061200.abc1234", true},
		{"1.0.0", false},
		{"v1.0.0", false},
		{"1.0.0-rc1", false},
		{"1.0.0-dev-xxx", false},
		{"dev", false},
	}
	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			t.Parallel()
			if got := isNightly(tt.version); got != tt.want {
				t.Errorf("isNightly(%q) = %v, want %v", tt.version, got, tt.want)
			}
		})
	}
}

func TestFetchLatestNightlyVersion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		releases := []GitHubRelease{
			{TagName: "v0.5.4", Prerelease: false},
			{TagName: "v0.5.4-nightly.202604061200.abc1234", Prerelease: true},
			{TagName: "v0.5.4-nightly.202604051159.def5678", Prerelease: true},
		}
		w.Header().Set("Content-Type", "application/json")
		//nolint:errcheck // test helper
		json.NewEncoder(w).Encode(releases)
	}))
	defer server.Close()

	original := githubReleasesURL
	githubReleasesURL = server.URL
	t.Cleanup(func() { githubReleasesURL = original })

	version, err := fetchLatestNightlyVersion(context.Background())
	if err != nil {
		t.Fatalf("fetchLatestNightlyVersion() error = %v", err)
	}
	if version != "v0.5.4-nightly.202604061200.abc1234" {
		t.Errorf("fetchLatestNightlyVersion() = %q, want v0.5.4-nightly.202604061200.abc1234", version)
	}
}

func TestFetchLatestNightlyVersion_NoNightlies(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		releases := []GitHubRelease{
			{TagName: "v0.5.4", Prerelease: false},
		}
		w.Header().Set("Content-Type", "application/json")
		//nolint:errcheck // test helper
		json.NewEncoder(w).Encode(releases)
	}))
	defer server.Close()

	original := githubReleasesURL
	githubReleasesURL = server.URL
	t.Cleanup(func() { githubReleasesURL = original })

	_, err := fetchLatestNightlyVersion(context.Background())
	if err == nil {
		t.Fatal("fetchLatestNightlyVersion() expected error when no nightlies, got nil")
	}
}

func TestCacheReadWrite(t *testing.T) {
	// Create a temporary directory for the test
	tmpDir := t.TempDir()

	// Create config directory structure
	configDir := filepath.Join(tmpDir, globalConfigDirName)
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("Failed to create config dir: %v", err)
	}

	// Test saving and loading cache directly to temp directory
	originalCache := &VersionCache{
		LastCheckTime: time.Now().Round(time.Second), // Round to second for JSON consistency
	}

	// Write cache manually to temp directory
	filePath := filepath.Join(configDir, cacheFileName)
	data, err := json.MarshalIndent(originalCache, "", "  ")
	if err != nil {
		t.Fatalf("json.MarshalIndent() error = %v", err)
	}

	if err := os.WriteFile(filePath, data, 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	// Load and verify

	loadedData, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	var loaded VersionCache
	if err := json.Unmarshal(loadedData, &loaded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	// Verify the loaded cache LastCheckTime matches (within 1 second tolerance for JSON rounding)
	if loaded.LastCheckTime.Sub(originalCache.LastCheckTime).Abs() > time.Second {
		t.Errorf("LastCheckTime = %v, want %v", loaded.LastCheckTime, originalCache.LastCheckTime)
	}

	// Verify file exists
	if _, err := os.Stat(filePath); err != nil {
		t.Errorf("cache file not found at %s: %v", filePath, err)
	}
}

func TestEnsureGlobalConfigDir(t *testing.T) {
	// This test verifies that the directory creation logic works
	// We test the actual os.MkdirAll behavior by creating a temp directory structure

	tmpDir := t.TempDir()
	configDir := filepath.Join(tmpDir, globalConfigDirName)

	// Verify the directory doesn't exist yet
	if _, err := os.Stat(configDir); err == nil {
		t.Fatalf("directory already exists before test")
	}

	// Simulate the ensureGlobalConfigDir logic
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	// Verify directory was created
	info, err := os.Stat(configDir)
	if err != nil {
		t.Fatalf("directory not created: %v", err)
	}

	// Verify it's a directory
	if !info.IsDir() {
		t.Errorf("path is not a directory")
	}

	// Verify permissions (on Unix systems)
	// The directory should be readable/writable/executable by owner
	if mode := info.Mode(); (mode & 0o700) != 0o700 {
		t.Errorf("directory permissions = %o, expected at least 0o700", mode)
	}
}

func TestFetchLatestVersion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/vnd.github+json" {
			t.Errorf("Accept header = %q, want application/vnd.github+json", r.Header.Get("Accept"))
		}
		if r.Header.Get("User-Agent") != "entire-cli" {
			t.Errorf("User-Agent header = %q, want entire-cli", r.Header.Get("User-Agent"))
		}

		release := GitHubRelease{
			TagName:    "v1.2.3",
			Prerelease: false,
		}
		w.Header().Set("Content-Type", "application/json")
		//nolint:errcheck // test helper, encoding error is acceptable
		json.NewEncoder(w).Encode(release)
	}))
	defer server.Close()

	original := githubAPIURL
	githubAPIURL = server.URL
	t.Cleanup(func() { githubAPIURL = original })

	version, err := fetchLatestVersion(context.Background())
	if err != nil {
		t.Fatalf("fetchLatestVersion(context.Background()) error = %v", err)
	}
	if version != "v1.2.3" {
		t.Errorf("fetchLatestVersion(context.Background()) = %q, want v1.2.3", version)
	}
}

func TestFetchLatestVersionPrerelease(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		release := GitHubRelease{
			TagName:    "v2.0.0-rc1",
			Prerelease: true,
		}
		w.Header().Set("Content-Type", "application/json")
		//nolint:errcheck // test helper, encoding error is acceptable
		json.NewEncoder(w).Encode(release)
	}))
	defer server.Close()

	original := githubAPIURL
	githubAPIURL = server.URL
	t.Cleanup(func() { githubAPIURL = original })

	_, err := fetchLatestVersion(context.Background())
	if err == nil {
		t.Fatal("fetchLatestVersion(context.Background()) expected error for prerelease, got nil")
	}
}

func TestFetchLatestVersionServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	original := githubAPIURL
	githubAPIURL = server.URL
	t.Cleanup(func() { githubAPIURL = original })

	_, err := fetchLatestVersion(context.Background())
	if err == nil {
		t.Fatal("fetchLatestVersion(context.Background()) expected error for 500 response, got nil")
	}
}

func TestParseGitHubRelease(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    string
		wantErr bool
	}{
		{"valid release", `{"tag_name": "v1.2.3", "prerelease": false}`, "v1.2.3", false},
		{"prerelease", `{"tag_name": "v2.0.0-rc1", "prerelease": true}`, "", true},
		{"empty tag", `{"tag_name": "", "prerelease": false}`, "", true},
		{"invalid json", `not json`, "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseGitHubRelease([]byte(tt.body))
			if (err != nil) != tt.wantErr {
				t.Errorf("parseGitHubRelease() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("parseGitHubRelease() = %q, want %q", got, tt.want)
			}
		})
	}
}

// brewUpgradeCmd is the install command produced for any brew-installed
// binary on a stable channel. Hoisted to a const so tests can reference
// it without tripping goconst on repeated string literals.
const brewUpgradeCmd = "brew upgrade entire"

func TestUpdateCommand(t *testing.T) {
	const plainBinPath = "/usr/local/bin/entire"
	tests := []struct {
		name           string
		currentVersion string
		execPath       func() (string, error)
		want           string
	}{
		{
			name:           "homebrew stable cellar path uses brew command",
			currentVersion: "1.0.0",
			execPath:       func() (string, error) { return "/opt/homebrew/Cellar/entire/1.0.0/bin/entire", nil },
			want:           brewUpgradeCmd,
		},
		{
			name:           "homebrew stable cask path uses brew command",
			currentVersion: "1.0.0",
			execPath:       func() (string, error) { return "/opt/homebrew/bin/entire", nil },
			want:           brewUpgradeCmd,
		},
		{
			name:           "homebrew nightly path uses brew command",
			currentVersion: "1.0.1-nightly.202604101200.abc1234",
			execPath:       func() (string, error) { return "/opt/homebrew/bin/entire", nil },
			want:           "brew upgrade entire@nightly",
		},
		{
			name:           "linuxbrew path",
			currentVersion: "1.0.0",
			execPath:       func() (string, error) { return "/home/linuxbrew/.linuxbrew/bin/entire", nil },
			want:           brewUpgradeCmd,
		},
		{
			name:           "mise path",
			currentVersion: "1.0.0",
			execPath:       func() (string, error) { return "/home/user/.local/share/mise/installs/entire/1.0.0/bin/entire", nil },
			want:           "mise upgrade entire",
		},
		{
			name:           "scoop path",
			currentVersion: "1.0.0",
			execPath:       func() (string, error) { return `C:\Users\test\scoop\apps\cli\current\entire.exe`, nil },
			want:           "scoop update entire/cli",
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
			original := executablePath
			executablePath = tt.execPath
			t.Cleanup(func() { executablePath = original })

			if got := updateCommand(tt.currentVersion); got != tt.want {
				t.Errorf("updateCommand() = %q, want %q", got, tt.want)
			}
		})
	}
}

// setupCheckAndNotifyTest sets HOME to a temp dir and overrides githubAPIURL.
// Returns a cobra.Command with captured stdout and a cleanup function.
func setupCheckAndNotifyTest(t *testing.T, serverURL string) (*cobra.Command, *bytes.Buffer) {
	t.Helper()

	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	origURL := githubAPIURL
	githubAPIURL = serverURL
	t.Cleanup(func() { githubAPIURL = origURL })

	var buf bytes.Buffer
	cmd := &cobra.Command{Use: "test"}
	cmd.SetOut(&buf)

	return cmd, &buf
}

// newVersionServer returns an httptest.Server that responds with the given version.
func newVersionServer(t *testing.T, version string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		release := GitHubRelease{TagName: version, Prerelease: false}
		w.Header().Set("Content-Type", "application/json")
		//nolint:errcheck // test helper
		json.NewEncoder(w).Encode(release)
	}))
	t.Cleanup(server.Close)
	return server
}

func seedVersionCache(t *testing.T, cache *VersionCache) {
	t.Helper()
	if err := ensureGlobalConfigDir(); err != nil {
		t.Fatalf("ensureGlobalConfigDir() error = %v", err)
	}
	if err := saveCache(cache); err != nil {
		t.Fatalf("saveCache() error = %v", err)
	}
}

func TestCheckAndNotify_SkipsDevVersion(t *testing.T) {
	server := newVersionServer(t, "v9.9.9")
	cmd, buf := setupCheckAndNotifyTest(t, server.URL)

	CheckAndNotify(context.Background(), cmd.OutOrStdout(), "dev")

	if buf.Len() != 0 {
		t.Errorf("expected no output for dev version, got %q", buf.String())
	}
}

func TestCheckAndNotify_SkipsEmptyVersion(t *testing.T) {
	server := newVersionServer(t, "v9.9.9")
	cmd, buf := setupCheckAndNotifyTest(t, server.URL)

	CheckAndNotify(context.Background(), cmd.OutOrStdout(), "")

	if buf.Len() != 0 {
		t.Errorf("expected no output for empty version, got %q", buf.String())
	}
}

func TestCheckAndNotify_SkipsWhenCacheIsFresh(t *testing.T) {
	server := newVersionServer(t, "v9.9.9")
	cmd, buf := setupCheckAndNotifyTest(t, server.URL)

	// Pre-seed the cache with a recent check time
	configDir, err := globalConfigDirPath()
	if err != nil {
		t.Fatalf("globalConfigDirPath() error = %v", err)
	}
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	cache := &VersionCache{LastCheckTime: time.Now()}
	if err := saveCache(cache); err != nil {
		t.Fatalf("saveCache() error = %v", err)
	}

	CheckAndNotify(context.Background(), cmd.OutOrStdout(), "1.0.0")

	if buf.Len() != 0 {
		t.Errorf("expected no output when cache is fresh, got %q", buf.String())
	}
}

func TestCheckAndNotify_PrintsNotificationWhenOutdated(t *testing.T) {
	server := newVersionServer(t, "v2.0.0")
	cmd, buf := setupCheckAndNotifyTest(t, server.URL)

	CheckAndNotify(context.Background(), cmd.OutOrStdout(), "1.0.0")

	output := buf.String()
	if !strings.Contains(output, "v2.0.0") {
		t.Errorf("expected notification with latest version, got %q", output)
	}
	if !strings.Contains(output, "1.0.0") {
		t.Errorf("expected notification with current version, got %q", output)
	}
}

func TestCheckAndNotify_BrewSkipUntilNextVersionCachesLatest(t *testing.T) {
	server := newVersionServer(t, "v2.0.0")
	cmd, _ := setupCheckAndNotifyTest(t, server.URL)
	f := newAutoUpdateFixture(t)
	useBrewExecutable(t)
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
		t.Errorf("prompt got cmd %q, want brew upgrade entire", f.lastCmdStr)
	}
}

// TestCheckAndNotify_MiseSkipUntilNextVersionCachesLatest verifies the
// skip-until-next-version persistence works for non-brew installers too.
// The cache flow is installer-agnostic; this locks that contract in.
func TestCheckAndNotify_MiseSkipUntilNextVersionCachesLatest(t *testing.T) {
	server := newVersionServer(t, "v2.0.0")
	cmd, _ := setupCheckAndNotifyTest(t, server.URL)
	f := newAutoUpdateFixture(t)
	useMiseExecutable(t)
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
	if f.lastCmdStr != "mise upgrade entire" {
		t.Errorf("prompt got cmd %q, want mise upgrade entire", f.lastCmdStr)
	}
}

func TestCheckAndNotify_SkipsVersionMarkedSkipped(t *testing.T) {
	server := newVersionServer(t, "v2.0.0")
	cmd, buf := setupCheckAndNotifyTest(t, server.URL)
	f := newAutoUpdateFixture(t)
	useBrewExecutable(t)
	seedVersionCache(t, &VersionCache{
		LastCheckTime:  time.Now().Add(-checkInterval - time.Minute),
		SkippedVersion: "v2.0.0",
	})

	CheckAndNotify(context.Background(), cmd.OutOrStdout(), "1.0.0")

	if f.installCalls != 0 {
		t.Fatalf("installer called %d times for skipped version, want 0", f.installCalls)
	}
	if buf.Len() != 0 {
		t.Errorf("expected no output for skipped version, got %q", buf.String())
	}
}

func TestCheckAndNotify_NoNotificationWhenUpToDate(t *testing.T) {
	server := newVersionServer(t, "v1.0.0")
	cmd, buf := setupCheckAndNotifyTest(t, server.URL)

	CheckAndNotify(context.Background(), cmd.OutOrStdout(), "1.0.0")

	if buf.Len() != 0 {
		t.Errorf("expected no output when up to date, got %q", buf.String())
	}
}

func TestCheckAndNotify_InstallerFailureKeepsCacheFresh(t *testing.T) {
	server := newVersionServer(t, "v2.0.0")
	cmd, buf := setupCheckAndNotifyTest(t, server.URL)

	// Simulate an interactive user who accepts the upgrade prompt, and an
	// installer that fails (e.g. brew upgrade blew up mid-run).
	t.Setenv("ENTIRE_TEST_TTY", "1")
	useBrewExecutable(t)

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

func TestCheckAndNotify_FetchFailureUpdatesCacheToPreventRetry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	cmd, buf := setupCheckAndNotifyTest(t, server.URL)

	CheckAndNotify(context.Background(), cmd.OutOrStdout(), "1.0.0")

	// No notification on fetch failure
	if buf.Len() != 0 {
		t.Errorf("expected no output on fetch failure, got %q", buf.String())
	}

	// Cache should have been updated so a second call skips the fetch
	cache, err := loadCache()
	if err != nil {
		t.Fatalf("loadCache() error = %v", err)
	}
	if time.Since(cache.LastCheckTime) > time.Minute {
		t.Errorf("cache LastCheckTime not updated after fetch failure: %v", cache.LastCheckTime)
	}
}
