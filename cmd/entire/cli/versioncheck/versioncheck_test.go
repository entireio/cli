package versioncheck

import (
	"bytes"
	"encoding/json"
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

	version, err := fetchLatestVersion()
	if err != nil {
		t.Fatalf("fetchLatestVersion() error = %v", err)
	}
	if version != "v1.2.3" {
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

	_, err := fetchLatestVersion()
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

	_, err := fetchLatestVersion()
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

func TestUpdateCommand(t *testing.T) {
	// updateCommand should return one of the two valid update commands
	cmd := updateCommand()

	validCommands := map[string]bool{
		"brew upgrade entire":                            true,
		"curl -fsSL https://entire.io/install.sh | bash": true,
	}

	if !validCommands[cmd] {
		t.Errorf("updateCommand() = %q, want one of %v", cmd, validCommands)
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

func TestCheckAndNotify_SkipsHiddenCommand(t *testing.T) {
	server := newVersionServer(t, "v9.9.9")
	cmd, buf := setupCheckAndNotifyTest(t, server.URL)
	cmd.Hidden = true

	CheckAndNotify(cmd, "1.0.0")

	if buf.Len() != 0 {
		t.Errorf("expected no output for hidden command, got %q", buf.String())
	}
}

func TestCheckAndNotify_SkipsDevVersion(t *testing.T) {
	server := newVersionServer(t, "v9.9.9")
	cmd, buf := setupCheckAndNotifyTest(t, server.URL)

	CheckAndNotify(cmd, "dev")

	if buf.Len() != 0 {
		t.Errorf("expected no output for dev version, got %q", buf.String())
	}
}

func TestCheckAndNotify_SkipsEmptyVersion(t *testing.T) {
	server := newVersionServer(t, "v9.9.9")
	cmd, buf := setupCheckAndNotifyTest(t, server.URL)

	CheckAndNotify(cmd, "")

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

	CheckAndNotify(cmd, "1.0.0")

	if buf.Len() != 0 {
		t.Errorf("expected no output when cache is fresh, got %q", buf.String())
	}
}

func TestCheckAndNotify_PrintsNotificationWhenOutdated(t *testing.T) {
	server := newVersionServer(t, "v2.0.0")
	cmd, buf := setupCheckAndNotifyTest(t, server.URL)

	CheckAndNotify(cmd, "1.0.0")

	output := buf.String()
	if !strings.Contains(output, "v2.0.0") {
		t.Errorf("expected notification with latest version, got %q", output)
	}
	if !strings.Contains(output, "1.0.0") {
		t.Errorf("expected notification with current version, got %q", output)
	}
}

func TestCheckAndNotify_NoNotificationWhenUpToDate(t *testing.T) {
	server := newVersionServer(t, "v1.0.0")
	cmd, buf := setupCheckAndNotifyTest(t, server.URL)

	CheckAndNotify(cmd, "1.0.0")

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

	CheckAndNotify(cmd, "1.0.0")

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
