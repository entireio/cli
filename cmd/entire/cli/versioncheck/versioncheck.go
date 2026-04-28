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
	"runtime"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"golang.org/x/mod/semver"
)

const goosWindows = "windows"

// goos is a test seam for runtime.GOOS so the Windows-specific auto-install
// gating can be exercised from a non-Windows host.
var goos = runtime.GOOS

const (
	installManagerBrew    = "brew"
	installManagerMise    = "mise"
	installManagerScoop   = "scoop"
	installManagerUnknown = "unknown"
	installChannelStable  = "stable"
	installChannelNightly = "nightly"
)

// CheckAndNotify performs a version check and notifies the user if a newer version is available.
// This is the main entry point for the version check system.
// The function is silent on all errors to avoid interrupting CLI operations.
func CheckAndNotify(ctx context.Context, w io.Writer, currentVersion string) {
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

	// Fetch the latest version from the appropriate channel
	var latestVersion string
	if isNightly(currentVersion) {
		latestVersion, err = fetchLatestNightlyVersion(ctx)
	} else {
		latestVersion, err = fetchLatestVersion(ctx)
	}

	// Always update cache to avoid retrying on every CLI invocation
	cache.LastCheckTime = time.Now()
	if saveErr := saveCache(cache); saveErr != nil {
		logging.Debug(ctx, "version check: failed to save cache",
			"error", saveErr.Error())
	}

	if err != nil {
		logging.Debug(ctx, "version check: failed to fetch latest version",
			"error", err.Error())
		return
	}

	// Show notification and offer an interactive upgrade when outdated
	if isOutdated(currentVersion, latestVersion) {
		if cache.SkippedVersion == versionCacheKey(latestVersion) {
			return
		}

		action := MaybeAutoUpdate(ctx, w, currentVersion, latestVersion)
		if action == autoUpdateActionSkipUntilNextVersion {
			cache.SkippedVersion = versionCacheKey(latestVersion)
			if saveErr := saveCache(cache); saveErr != nil {
				logging.Debug(ctx, "version check: failed to save skipped version",
					"error", saveErr.Error())
			}
		}
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

// fetchLatestVersion fetches the latest stable version tag from the GitHub API.
func fetchLatestVersion(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubAPIURL, nil)
	if err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}

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

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("reading response: %w", err)
	}

	version, err := parseGitHubRelease(body)
	if err != nil {
		return "", fmt.Errorf("parsing release: %w", err)
	}
	return version, nil
}

// isNightly returns true if the version string is a nightly build.
func isNightly(version string) bool {
	if !strings.HasPrefix(version, "v") {
		version = "v" + version
	}
	return strings.Contains(semver.Prerelease(version), "nightly")
}

// fetchLatestNightlyVersion fetches the latest nightly version tag from the GitHub releases list.
func fetchLatestNightlyVersion(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubReleasesURL+"?per_page=20", nil)
	if err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}

	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "entire-cli/"+versioninfo.Version)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetching releases: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("reading response: %w", err)
	}

	var releases []GitHubRelease
	if err := json.Unmarshal(body, &releases); err != nil {
		return "", fmt.Errorf("parsing JSON: %w", err)
	}

	for _, r := range releases {
		if r.Prerelease && strings.Contains(r.TagName, "-nightly.") {
			return r.TagName, nil
		}
	}

	return "", errors.New("no nightly release found")
}

// parseGitHubRelease parses the GitHub API response and returns the latest stable version tag.
// Filters out prerelease versions.
func parseGitHubRelease(body []byte) (string, error) {
	var release GitHubRelease
	if err := json.Unmarshal(body, &release); err != nil {
		return "", fmt.Errorf("parsing JSON: %w", err)
	}

	if release.Prerelease {
		return "", errors.New("only prerelease versions available")
	}

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

	// Skip notification for dev builds (e.g., "1.0.0-dev-xxx").
	// These are local development builds and shouldn't trigger update notifications.
	// Normal prereleases (e.g., "1.0.0-rc1") should still be compared normally.
	if strings.Contains(semver.Prerelease(current), "dev") {
		return false
	}

	// semver.Compare returns -1 if current < latest
	return semver.Compare(current, latest) < 0
}

func versionCacheKey(version string) string {
	if version == "" || strings.HasPrefix(version, "v") {
		return version
	}
	return "v" + version
}

func displayVersion(version string) string {
	return strings.TrimPrefix(version, "v")
}

func releaseNotesURL(version string) string {
	return downloadsURL + "/tag/" + versionCacheKey(version)
}

// executablePath is the function used to get the current executable path.
// It's a variable so tests can override it.
var executablePath = os.Executable

func releaseChannel(version string) string {
	if isNightly(version) {
		return installChannelNightly
	}
	return installChannelStable
}

func installManagerForCurrentBinary() string {
	execPath, err := executablePath()
	if err != nil {
		return installManagerUnknown
	}

	realPath, err := filepath.EvalSymlinks(execPath)
	if err != nil {
		realPath = execPath
	}
	normalizedPath := strings.ReplaceAll(filepath.ToSlash(realPath), "\\", "/")

	switch {
	case strings.Contains(normalizedPath, "/Cellar/") ||
		strings.Contains(normalizedPath, "/opt/homebrew/") ||
		strings.Contains(normalizedPath, "/linuxbrew/") ||
		strings.Contains(normalizedPath, "/Caskroom/"):
		return installManagerBrew
	case strings.Contains(normalizedPath, "/mise/installs/"):
		return installManagerMise
	case strings.Contains(normalizedPath, "/scoop/apps/"):
		return installManagerScoop
	default:
		return installManagerUnknown
	}
}

// canAutoInstall reports whether updateCommand(currentVersion) is safe to
// execute on the current OS. Returns false on Windows when the install
// manager is unknown, because the POSIX curl-pipe-bash fallback can't run
// from cmd.exe and there's no Windows-native installer to substitute.
func canAutoInstall() bool {
	if goos != goosWindows {
		return true
	}
	switch installManagerForCurrentBinary() {
	case installManagerScoop, installManagerMise:
		return true
	default:
		return false
	}
}

// downloadsURL is the public page users visit when we can't offer an
// auto-installable command on their platform.
const downloadsURL = "https://github.com/entireio/cli/releases"

// updateCommand returns the appropriate update instruction based on how the binary was installed.
func updateCommand(currentVersion string) string {
	switch installManagerForCurrentBinary() {
	case installManagerBrew:
		if releaseChannel(currentVersion) == installChannelNightly {
			return "brew upgrade entire@nightly"
		}
		return "brew upgrade entire"
	case installManagerMise:
		return "mise upgrade entire"
	case installManagerScoop:
		return "scoop update entire/cli"
	}

	if releaseChannel(currentVersion) == installChannelNightly {
		return "curl -fsSL https://entire.io/install.sh | bash -s -- --channel nightly"
	}
	return "curl -fsSL https://entire.io/install.sh | bash"
}

// printNotification prints the version update notification to the user.
func printNotification(w io.Writer, current, latest string) {
	fmt.Fprintf(w, "\nUpdate available! %s -> %s\nRelease notes: %s\n",
		displayVersion(current), displayVersion(latest), releaseNotesURL(latest))
}
