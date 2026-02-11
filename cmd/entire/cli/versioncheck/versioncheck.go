package versioncheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"golang.org/x/mod/semver"
)

// CheckAndNotify performs a version check and notifies the user if a newer version is available.
// This is the main entry point for the version check system.
// The function is silent on all errors to avoid interrupting CLI operations.
func CheckAndNotify(w io.Writer, currentVersion string) {
	// Skip checks for dev builds
	if currentVersion == "dev" || currentVersion == "" {
		return
	}

	// Ensure the global config directory exists
	if err := ensureGlobalConfigDir(); err != nil {
		// Silent failure - don't block CLI operations
		return
	}

	// Load the cache to check when we last fetched
	cache, err := loadCache()
	if err != nil {
		cache = &VersionCache{}
	}

	// Skip if we checked recently (within 24 hours)
	if time.Since(cache.LastCheckTime) < checkInterval {
		return
	}

	// Fetch the latest version from GitHub API
	latestVersion, err := fetchLatestVersion()

	// Always update cache to avoid retrying on every CLI invocation
	cache.LastCheckTime = time.Now()
	if saveErr := saveCache(cache); saveErr != nil {
		logging.Debug(context.Background(), "version check: failed to save cache",
			"error", saveErr.Error())
	}

	if err != nil {
		logging.Debug(context.Background(), "version check: failed to fetch latest version",
			"error", err.Error())
		return
	}

	// Show notification if outdated
	if isOutdated(currentVersion, latestVersion) {
		printNotification(w, currentVersion, latestVersion)
	}
}

// globalConfigDirPath returns the expanded path to the global config directory (~/.config/entire).
func globalConfigDirPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("getting home directory: %w", err)
	}
	return filepath.Join(home, globalConfigDirName), nil
}

// ensureGlobalConfigDir creates the global config directory if it doesn't exist.
func ensureGlobalConfigDir() error {
	configDir, err := globalConfigDirPath()
	if err != nil {
		return err
	}

	//nolint:gosec // ~/.config/entire is user home directory, 0o755 is appropriate
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}

	return nil
}

// cacheFilePath returns the full path to the version check cache file.
func cacheFilePath() (string, error) {
	configDir, err := globalConfigDirPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, cacheFileName), nil
}

// loadCache loads the version check cache from disk.
// Returns an error if the file doesn't exist or is corrupted.
func loadCache() (*VersionCache, error) {
	filePath, err := cacheFilePath()
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(filePath) //nolint:gosec // cacheFilePath is safe
	if err != nil {
		return nil, fmt.Errorf("reading cache file: %w", err)
	}

	var cache VersionCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return nil, fmt.Errorf("parsing cache: %w", err)
	}

	return &cache, nil
}

// saveCache saves the version check cache to disk.
// Uses atomic write semantics (write to temp file, then rename).
func saveCache(cache *VersionCache) error {
	filePath, err := cacheFilePath()
	if err != nil {
		return err
	}

	// Marshal to JSON
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling cache: %w", err)
	}

	// Write to temp file first (atomic write)
	dir := filepath.Dir(filePath)
	tmpFile, err := os.CreateTemp(dir, ".version_check_tmp_")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close() // cleanup on error path
		return fmt.Errorf("writing cache: %w", err)
	}

	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("closing temp file: %w", err)
	}

	// Rename temp file to final location
	if err := os.Rename(tmpFile.Name(), filePath); err != nil {
		return fmt.Errorf("renaming cache file: %w", err)
	}

	return nil
}

// fetchLatestVersion fetches the latest version from the GitHub API.
// Returns a timeout-safe version check using the configured HTTP timeout.
func fetchLatestVersion() (string, error) {
	// Create a context with timeout for the HTTP request
	ctx, cancel := context.WithTimeout(context.Background(), httpTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubAPIURL, nil)
	if err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}

	// Set headers to identify as Entire CLI
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "entire-cli")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetching release info: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	// Read response body (limit to 1MB to prevent memory exhaustion)
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("reading response: %w", err)
	}

	// Parse GitHub release response
	version, err := parseGitHubRelease(body)
	if err != nil {
		return "", fmt.Errorf("parsing release: %w", err)
	}

	return version, nil
}

// parseGitHubRelease parses the GitHub API response and extracts the latest stable version.
// Filters out prerelease versions.
func parseGitHubRelease(body []byte) (string, error) {
	var release GitHubRelease
	if err := json.Unmarshal(body, &release); err != nil {
		return "", fmt.Errorf("parsing JSON: %w", err)
	}

	// Skip prerelease versions
	if release.Prerelease {
		return "", errors.New("only prerelease versions available")
	}

	// Ensure we have a tag name
	if release.TagName == "" {
		return "", errors.New("empty tag name")
	}

	return release.TagName, nil
}

// isOutdated compares current and latest versions using semantic versioning.
// Returns true if current < latest.
func isOutdated(current, latest string) bool {
	// Ensure versions have "v" prefix for semver package
	if !strings.HasPrefix(current, "v") {
		current = "v" + current
	}
	if !strings.HasPrefix(latest, "v") {
		latest = "v" + latest
	}

	// semver.Compare returns -1 if current < latest
	return semver.Compare(current, latest) < 0
}

// updateCommand returns the appropriate update instruction based on how the binary was installed.
func updateCommand() string {
	execPath, err := os.Executable()
	if err != nil {
		return "curl -fsSL https://entire.io/install.sh | bash"
	}

	// Resolve symlinks to find the real path (Homebrew symlinks from bin/ to Cellar/)
	realPath, err := filepath.EvalSymlinks(execPath)
	if err != nil {
		realPath = execPath
	}

	if strings.Contains(realPath, "/Cellar/") || strings.Contains(realPath, "/homebrew/") {
		return "brew upgrade entire"
	}

	return "curl -fsSL https://entire.io/install.sh | bash"
}

// printNotification prints the version update notification to the user.
func printNotification(w io.Writer, current, latest string) {
	msg := fmt.Sprintf("\nA newer version of Entire CLI is available: %s (current: %s)\nRun '%s' to update.\n",
		latest, current, updateCommand())
	fmt.Fprint(w, msg)
}
