package versioncheck

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/entireio/cli/internal/entireclient/userdirs"
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
		{"0.6.3-nightly.202605250745.b5855692.0.20260527023747-77e1fedc5741", "0.6.4", false, "pseudo-version (local dev build) skips check"},

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
		// Near misses the old substring test accepted. install.sh and the
		// GitHub release scan reject all three, so accepting them here made
		// the CLI announce updates the installer would not install.
		{"0.5.3-nightly", false},
		{"0.5.3-prenightly", false},
		{"0.5.3-rc.nightly.1", false},
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

func TestIsDevBuild(t *testing.T) {
	t.Parallel()
	tests := []struct {
		version string
		want    bool
	}{
		{"dev", true},
		{"", true},
		{"garbage", true}, // unparseable -> treat as local build, don't nag
		// Released builds: clean semver, should be checked for updates.
		{"0.6.3", false},
		{"v0.6.3", false},
		{"1.0.0-rc1", false},
		{"0.6.3-nightly.202605250745.b5855692", false},
		{"v0.6.3-nightly.202605250745.b5855692", false},
		// Local builds: Go pseudo-versions and dirty trees.
		{"0.6.3-nightly.202605250745.b5855692.0.20260527023747-77e1fedc5741", true},
		{"0.6.3-nightly.202605250745.b5855692.0.20260527023747-77e1fedc5741+dirty", true},
		{"0.6.4-0.20260527023747-77e1fedc5741", true}, // pseudo-version off a stable tag
	}
	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			t.Parallel()
			if got := isDevBuild(tt.version); got != tt.want {
				t.Errorf("isDevBuild(%q) = %v, want %v", tt.version, got, tt.want)
			}
		})
	}
}

// testStableVersion is the stable tag reused across these tests.
const testStableVersion = "v1.2.3"

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

	version, err := fetchLatestNightlyVersion(context.Background(), http.DefaultClient)
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

	_, err := fetchLatestNightlyVersion(context.Background(), http.DefaultClient)
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
		if want := versioninfo.UserAgent(); r.Header.Get("User-Agent") != want {
			t.Errorf("User-Agent header = %q, want %q", r.Header.Get("User-Agent"), want)
		}

		release := GitHubRelease{
			TagName:    testStableVersion,
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

	version, err := fetchLatestVersion(context.Background(), http.DefaultClient)
	if err != nil {
		t.Fatalf("fetchLatestVersion() error = %v", err)
	}
	if version != testStableVersion {
		t.Errorf("fetchLatestVersion() = %q, want v1.2.3", version)
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

	_, err := fetchLatestVersion(context.Background(), http.DefaultClient)
	if err == nil {
		t.Fatal("fetchLatestVersion() expected error for prerelease, got nil")
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

	_, err := fetchLatestVersion(context.Background(), http.DefaultClient)
	if err == nil {
		t.Fatal("fetchLatestVersion() expected error for 500 response, got nil")
	}
}

func TestParseGitHubRelease(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    string
		wantErr bool
	}{
		{"valid release", `{"tag_name": "v1.2.3", "prerelease": false}`, testStableVersion, false},
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

// brewCaskPath is a brew-installed binary; tests that do not care about the
// install manager use it.
const brewCaskPath = "/opt/homebrew/Caskroom/entire/1.0.0/entire"

const miseExecutablePath = "/home/user/.local/share/mise/installs/entire/1.0.0/bin/entire"

func relocatedTestRoot(elem string) string {
	if runtime.GOOS == "windows" {
		return `D:\` + elem
	}
	return "/" + elem
}

// TestUpdateCommandForCurrentBinary_MiseRoots covers mise detection through
// the env-relocated roots and the default marker.
func TestUpdateCommandForCurrentBinary_MiseRoots(t *testing.T) {
	installsDir := relocatedTestRoot("srv/tools")
	dataDir := relocatedTestRoot("srv/mise")
	tests := []struct {
		name        string
		installsDir string
		dataDir     string
		execPath    string
	}{
		{
			name:        "MISE_INSTALLS_DIR",
			installsDir: installsDir,
			execPath:    filepath.Join(installsDir, "entire", "1.0.0", "bin", "entire"),
		},
		{
			name:     "MISE_DATA_DIR",
			dataDir:  dataDir,
			execPath: filepath.Join(dataDir, "installs", "entire", "1.0.0", "bin", "entire"),
		},
		{
			name:     "default marker",
			execPath: miseExecutablePath,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("MISE_INSTALLS_DIR", tt.installsDir)
			t.Setenv("MISE_DATA_DIR", tt.dataDir)
			setExecutablePath(t, tt.execPath)

			if got := UpdateCommandForCurrentBinary("1.0.0"); got != miseUpgradeCmd {
				t.Errorf("UpdateCommandForCurrentBinary() = %q, want %q", got, miseUpgradeCmd)
			}
		})
	}
}

// setupCheckAndNotifyTest points the global config dir at a per-test temp
// dir and overrides githubAPIURL. Returns a cobra.Command with captured
// stdout and a cleanup function.
func setupCheckAndNotifyTest(t *testing.T, serverURL string) (*cobra.Command, *bytes.Buffer) {
	t.Helper()

	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())

	// These tests exercise the GitHub origin specifically. Without this the
	// chain would try the real install.entire.com first, putting a live network call
	// in a unit test.
	useGitHubOriginOnly(t)

	origURL := githubAPIURL
	githubAPIURL = serverURL
	t.Cleanup(func() { githubAPIURL = origURL })

	var buf bytes.Buffer
	cmd := &cobra.Command{Use: "test"}
	cmd.SetOut(&buf)

	return cmd, &buf
}

// useGitHubOriginOnly narrows the origin chain to the GitHub candidate for
// tests that stub githubAPIURL / githubReleasesURL, so nothing reaches the
// real release origin.
func useGitHubOriginOnly(t *testing.T) {
	t.Helper()
	orig := versionOrigins
	versionOrigins = func() []versionOrigin {
		return []versionOrigin{{name: "api.github.com", fetch: fetchGitHubVersion}}
	}
	t.Cleanup(func() { versionOrigins = orig })
}

// useChannelOrigin points the release-origin candidate at a local server and
// keeps GitHub in the chain behind it, which is the production ordering.
func useChannelOrigin(t *testing.T, baseURL string) {
	t.Helper()
	orig := channelBaseURL
	channelBaseURL = baseURL
	t.Cleanup(func() { channelBaseURL = orig })
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

func TestCheckAndNotify_SkipsPseudoVersion(t *testing.T) {
	server := newVersionServer(t, "v9.9.9")
	cmd, buf := setupCheckAndNotifyTest(t, server.URL)

	// A local working-tree build reports a Go pseudo-version; it must not be
	// nagged to update even though it isn't the bare "dev" sentinel.
	CheckAndNotify(context.Background(), cmd.OutOrStdout(),
		"0.6.3-nightly.202605250745.b5855692.0.20260527023747-77e1fedc5741")

	if buf.Len() != 0 {
		t.Errorf("expected no output for pseudo-version build, got %q", buf.String())
	}
}

func TestCheckAndNotify_SkipsWhenCacheIsFresh(t *testing.T) {
	server := newVersionServer(t, "v9.9.9")
	cmd, buf := setupCheckAndNotifyTest(t, server.URL)

	// Pre-seed the cache with a recent check time
	configDir := globalConfigDirPath()
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

func TestCheckAndNotify_SkipsVersionMarkedSkipped(t *testing.T) {
	server := newVersionServer(t, "v2.0.0")
	cmd, buf := setupCheckAndNotifyTest(t, server.URL)
	f := newAutoUpdateFixture(t)
	setExecutablePath(t, brewCaskPath)
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

// globalConfigDirPath returns the CLI's global config directory. Resolution
// lives in userdirs.Config — the single implementation shared by all
// config-dir consumers (contexts.json, the file token store, this cache).
func globalConfigDirPath() string {
	return userdirs.Config()
}

// globalConfigDirName is the name of the global config directory in the user's home.
const globalConfigDirName = ".config/entire"

// newChannelServer serves a channel manifest at /channels/<channel>.json and
// records which paths were requested.
func newChannelServer(t *testing.T, manifest ChannelManifest) (*httptest.Server, *[]string) {
	t.Helper()
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		//nolint:errcheck // test helper
		json.NewEncoder(w).Encode(manifest)
	}))
	t.Cleanup(server.Close)
	return server, &paths
}

func TestFetchLatestForChannel_PrefersReleaseOrigin(t *testing.T) {
	channelServer, paths := newChannelServer(t, ChannelManifest{
		Channel: "stable",
		Tag:     testStableVersion,
		Version: "1.2.3",
	})
	useChannelOrigin(t, channelServer.URL)

	githubHits := 0
	githubServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		githubHits++
		w.Header().Set("Content-Type", "application/json")
		//nolint:errcheck // test helper
		json.NewEncoder(w).Encode(GitHubRelease{TagName: "v9.9.9"})
	}))
	defer githubServer.Close()
	origURL := githubAPIURL
	githubAPIURL = githubServer.URL
	t.Cleanup(func() { githubAPIURL = origURL })

	version, err := fetchLatestForChannel(context.Background(), false)
	if err != nil {
		t.Fatalf("fetchLatestForChannel() error = %v", err)
	}
	if version != testStableVersion {
		t.Errorf("fetchLatestForChannel() = %q, want v1.2.3", version)
	}
	if githubHits != 0 {
		t.Errorf("GitHub was queried %d times, want 0 when the release origin answers", githubHits)
	}
	if len(*paths) != 1 || (*paths)[0] != "/channels/stable.json" {
		t.Errorf("requested paths = %v, want [/channels/stable.json]", *paths)
	}
}

func TestFetchLatestForChannel_FallsBackToGitHub(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer down.Close()
	useChannelOrigin(t, down.URL)

	githubServer := newVersionServer(t, testStableVersion)
	origURL := githubAPIURL
	githubAPIURL = githubServer.URL
	t.Cleanup(func() { githubAPIURL = origURL })

	version, err := fetchLatestForChannel(context.Background(), false)
	if err != nil {
		t.Fatalf("fetchLatestForChannel() error = %v", err)
	}
	if version != testStableVersion {
		t.Errorf("fetchLatestForChannel() = %q, want v1.2.3 from the fallback origin", version)
	}
}

func TestFetchLatestForChannel_NightlyFromReleaseOrigin(t *testing.T) {
	want := "v0.5.4-nightly.202604061200.abc1234"
	channelServer, paths := newChannelServer(t, ChannelManifest{
		Channel: "nightly",
		Tag:     want,
	})
	useChannelOrigin(t, channelServer.URL)

	version, err := fetchLatestForChannel(context.Background(), true)
	if err != nil {
		t.Fatalf("fetchLatestForChannel() error = %v", err)
	}
	if version != want {
		t.Errorf("fetchLatestForChannel() = %q, want %q", version, want)
	}
	if len(*paths) != 1 || (*paths)[0] != "/channels/nightly.json" {
		t.Errorf("requested paths = %v, want [/channels/nightly.json]", *paths)
	}
}

func TestFetchLatestForChannel_FallsBackWhenManifestIsUnusable(t *testing.T) {
	// The whole point of validating the manifest is that a corrupt pointer on
	// our own origin must not take update checks down with it.
	channelServer, _ := newChannelServer(t, ChannelManifest{
		Channel: "stable",
		Version: "not-a-version",
	})
	useChannelOrigin(t, channelServer.URL)

	githubServer := newVersionServer(t, testStableVersion)
	origURL := githubAPIURL
	githubAPIURL = githubServer.URL
	t.Cleanup(func() { githubAPIURL = origURL })

	version, err := fetchLatestForChannel(context.Background(), false)
	if err != nil {
		t.Fatalf("fetchLatestForChannel() error = %v", err)
	}
	if version != testStableVersion {
		t.Errorf("fetchLatestForChannel() = %q, want %q from the fallback origin", version, testStableVersion)
	}
}

func TestFetchLatestForChannel_HangingPrimaryStillReachesFallback(t *testing.T) {
	// The failure the second origin exists for: a primary that accepts the
	// connection and never answers. If it could consume the whole chain budget
	// the fallback would never be tried, and the update check would be worse
	// off than before the primary existed.
	blocked := make(chan struct{})
	t.Cleanup(func() { close(blocked) })
	hung := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-blocked:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(hung.Close)
	useChannelOrigin(t, hung.URL)

	githubServer := newVersionServer(t, testStableVersion)
	origURL := githubAPIURL
	githubAPIURL = githubServer.URL
	t.Cleanup(func() { githubAPIURL = origURL })

	started := time.Now()
	version, err := fetchLatestForChannel(context.Background(), false)
	elapsed := time.Since(started)

	if err != nil {
		t.Fatalf("fetchLatestForChannel() error = %v (after %s)", err, elapsed)
	}
	if version != testStableVersion {
		t.Errorf("fetchLatestForChannel() = %q, want %q from the fallback", version, testStableVersion)
	}
	if elapsed > httpTimeout {
		t.Errorf("took %s, which exceeds the whole-chain budget of %s", elapsed, httpTimeout)
	}
}

func TestFetchLatestForChannel_CancelledContextReportsCancellation(t *testing.T) {
	useGitHubOriginOnly(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := fetchLatestForChannel(ctx, false)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want it to report the cancellation rather than an empty chain", err)
	}
}

func TestFetchLatestForChannel_AllOriginsFail(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer down.Close()
	useChannelOrigin(t, down.URL)

	origURL := githubAPIURL
	githubAPIURL = down.URL
	t.Cleanup(func() { githubAPIURL = origURL })

	_, err := fetchLatestForChannel(context.Background(), false)
	if err == nil {
		t.Fatal("fetchLatestForChannel() expected an error when every origin fails, got nil")
	}
	// The first origin's failure is the one reported, so an operator reading a
	// debug log learns which origin they should be looking at first.
	if !strings.Contains(err.Error(), "install.entire.com") {
		t.Errorf("error = %q, want it to name the first origin", err.Error())
	}
}

func TestFetchChannelManifestVersion_RejectsCrossChannel(t *testing.T) {
	tests := []struct {
		name     string
		nightly  bool
		manifest ChannelManifest
	}{
		{
			name:     "nightly tag on the stable channel",
			nightly:  false,
			manifest: ChannelManifest{Channel: "stable", Tag: "v0.5.4-nightly.202604061200.abc1234"},
		},
		{
			name:     "stable tag on the nightly channel",
			nightly:  true,
			manifest: ChannelManifest{Channel: "nightly", Tag: testStableVersion},
		},
		{
			name:     "manifest declares a different channel",
			nightly:  false,
			manifest: ChannelManifest{Channel: "nightly", Tag: testStableVersion},
		},
		{
			name:     "manifest names no version",
			nightly:  false,
			manifest: ChannelManifest{Channel: "stable"},
		},
		{
			// isNightly is false for an unparseable string, so this passes the
			// channel check. Accepting it ends the chain, and isOutdated then
			// reports no update because an invalid version sorts below every
			// valid one — update checks silently switch off.
			name:     "version is not a version",
			nightly:  false,
			manifest: ChannelManifest{Channel: "stable", Version: "not-a-version"},
		},
		{
			name:     "tag is not a version",
			nightly:  false,
			manifest: ChannelManifest{Channel: "stable", Tag: "vnope"},
		},
		{
			name:     "version is a bare major.minor",
			nightly:  false,
			manifest: ChannelManifest{Channel: "stable", Version: "1.2"},
		},
		{
			// publish-channel-manifest.sh refuses to route this at all, and
			// install.sh rejects it on stable. Accepting it here would push an
			// rc to every stable user from the one reader that allowed it.
			name:     "non-nightly prerelease on the stable channel",
			nightly:  false,
			manifest: ChannelManifest{Channel: "stable", Tag: "v1.2.3-rc.1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, _ := newChannelServer(t, tt.manifest)
			useChannelOrigin(t, server.URL)

			if _, err := fetchChannelManifestVersion(context.Background(), http.DefaultClient, tt.nightly); err == nil {
				t.Fatalf("fetchChannelManifestVersion(nightly=%v) expected an error, got nil", tt.nightly)
			}
		})
	}
}

func TestFetchChannelManifestVersion_FallsBackToVersionField(t *testing.T) {
	// A manifest without a tag is still usable: the version field carries the
	// same value without the "v" prefix.
	server, _ := newChannelServer(t, ChannelManifest{Channel: "stable", Version: "1.2.3"})
	useChannelOrigin(t, server.URL)

	version, err := fetchChannelManifestVersion(context.Background(), http.DefaultClient, false)
	if err != nil {
		t.Fatalf("fetchChannelManifestVersion() error = %v", err)
	}
	if version != "1.2.3" {
		t.Errorf("fetchChannelManifestVersion() = %q, want 1.2.3", version)
	}
}

func TestFetchLatestForChannel_StopsWhenBudgetSpent(t *testing.T) {
	// A cancelled context must not produce requests that can only fail.
	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	useChannelOrigin(t, server.URL)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := fetchLatestForChannel(ctx, false); err == nil {
		t.Fatal("fetchLatestForChannel() expected an error on a cancelled context, got nil")
	}
	if hits != 0 {
		t.Errorf("server was hit %d times on a cancelled context, want 0", hits)
	}
}
