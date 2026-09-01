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

	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/entireio/cli/internal/entireclient/userdirs"
	"golang.org/x/mod/module"
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
	// Skip checks for local/unreleased builds.
	if isDevBuild(currentVersion) {
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

// globalConfigDirPath returns the CLI's global config directory. Resolution
// lives in userdirs.Config — the single implementation shared by all
// config-dir consumers (contexts.json, the file token store, this cache).
func globalConfigDirPath() string {
	return userdirs.Config()
}

// ensureGlobalConfigDir creates the global config directory if it doesn't exist.
//
// It goes through userdirs.ConfigRoot, whose create path is
// userdirs.EnsurePrivateDir, rather than a bare MkdirAll: this directory is
// shared with contexts.json and the file token store, both of which hold bearer
// tokens. The version check runs from the root command's PersistentPostRun, so
// on a fresh machine it is almost always what creates the directory, before the
// first login ever runs. Creating it world-readable here left it that way
// permanently, since MkdirAll cannot change the mode of a directory that
// already exists.
func ensureGlobalConfigDir() error {
	if _, err := userdirs.ConfigRoot(); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}
	return nil
}

// loadCache loads the version check cache from disk.
// Returns an error if the file doesn't exist or is corrupted.
func loadCache() (*VersionCache, error) {
	root, err := userdirs.ConfigRootForRead()
	if err != nil {
		return nil, fmt.Errorf("reading cache file: %w", err)
	}
	data, err := osroot.ReadFileNoFollow(root, cacheFileName)
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
	// Marshal to JSON
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling cache: %w", err)
	}

	root, err := userdirs.ConfigRoot()
	if err != nil {
		return fmt.Errorf("opening config directory: %w", err)
	}

	// Write to temp file first (atomic write)
	tmpFile, tmpName, err := jsonutil.CreateTempIn(root, cacheFileName)
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	defer func() { _ = root.Remove(tmpName) }() //nolint:errcheck // best-effort; a successful rename already consumed it

	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close() // cleanup on error path
		return fmt.Errorf("writing cache: %w", err)
	}

	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("closing temp file: %w", err)
	}

	// Rename temp file to final location
	if err := root.Rename(tmpName, cacheFileName); err != nil {
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
	req.Header.Set("User-Agent", versioninfo.UserAgent())

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
	req.Header.Set("User-Agent", versioninfo.UserAgent())

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

	// Local/unreleased builds shouldn't trigger update notifications.
	// Normal prereleases (e.g., "1.0.0-rc1") should still be compared normally.
	if isDevBuild(current) {
		return false
	}

	// semver.Compare returns -1 if current < latest
	return semver.Compare(current, latest) < 0
}

// isDevBuild reports whether v identifies a local, unreleased build that
// should not be nagged to update. This covers the "dev" sentinel and empty
// string (an unstamped binary), a Go pseudo-version (built from a commit that
// isn't a tagged release), and any build carrying +metadata such as "+dirty".
// Released builds — stable tags, nightly tags, and `go install ...@<tag>` —
// have clean semver and return false.
func isDevBuild(v string) bool {
	if v == "" || v == "dev" {
		return true
	}
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	return !semver.IsValid(v) || module.IsPseudoVersion(v) || semver.Build(v) != ""
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

// normalizedExecPath returns the running binary's real path with all separators
// normalized to forward slashes (symlinks resolved, best-effort). Both the
// install-manager detection and the Scoop app-dir lookup key off this form.
func normalizedExecPath() (string, error) {
	execPath, err := executablePath()
	if err != nil {
		return "", err
	}
	realPath, err := filepath.EvalSymlinks(execPath)
	if err != nil {
		realPath = execPath
	}
	return strings.ReplaceAll(filepath.ToSlash(realPath), "\\", "/"), nil
}

// scoopAppName returns the Scoop app directory the running binary lives under
// — the path segment after `/scoop/apps/` (e.g. "cli" or "entire") — or ""
// when the binary is not a Scoop install. This is the durable signal for the
// pre-rename package: a binary running from the `cli` app dir must migrate to
// `entire` regardless of its version (the fix ships in a final `cli` release,
// so the migrating binary's version is already past the rename).
func scoopAppName() string {
	normalized, err := normalizedExecPath()
	if err != nil {
		return ""
	}
	_, rest, ok := strings.Cut(normalized, "/scoop/apps/")
	if !ok {
		return ""
	}
	// Cut returns the whole string when the separator is absent, so this covers
	// both `.../scoop/apps/cli/current/entire.exe` and a bare app segment.
	app, _, _ := strings.Cut(rest, "/")
	return app
}

func installManagerForCurrentBinary() string {
	normalizedPath, err := normalizedExecPath()
	if err != nil {
		return installManagerUnknown
	}

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

// downloadsURL is the GitHub releases page, used for release-notes links.
const downloadsURL = "https://github.com/entireio/cli/releases"

// Windows install.ps1 one-liners from the README. Printed, never auto-run.
const (
	windowsInstallCmd        = "irm https://raw.githubusercontent.com/entireio/cli/main/scripts/install.ps1 -UseBasicParsing | iex"
	windowsInstallNightlyCmd = "& ([scriptblock]::Create((irm https://raw.githubusercontent.com/entireio/cli/main/scripts/install.ps1 -UseBasicParsing))) -Channel nightly"
)

// updateCommand returns the appropriate update instruction based on how the binary was installed.
func updateCommand(currentVersion string) string {
	switch installManagerForCurrentBinary() {
	case installManagerBrew:
		if releaseChannel(currentVersion) == installChannelNightly {
			return "brew upgrade --yes entire@nightly"
		}
		return "brew upgrade --yes entire"
	case installManagerMise:
		return "mise upgrade entire"
	case installManagerScoop:
		// The Scoop package was renamed from `cli` to `entire`. A binary still
		// running from the old `cli` app dir can never cross the rename with a
		// plain `scoop update entire/cli`, so migrate it: install the new
		// `entire` package, remove the old `cli` package, then reset the shared
		// shims. Both manifests declare `bin: [git-remote-entire.exe,
		// entire.exe]`, so the two packages contend for the same shims. Current
		// Scoop handles that itself: installing `entire` renames the displaced
		// shim to `entire.shim.cli` (warn_on_overwrite) and uninstalling `cli`
		// removes only that suffixed copy (rm_shim), leaving the active shims
		// pointing at `entire`. The reset step is belt-and-braces for older
		// Scoop versions that overwrote shims without that alt-file dance and
		// would otherwise take `entire.exe` and the git remote helper with them.
		//
		// The Windows updater is print-only, so return a self-contained command
		// users can paste into either cmd or PowerShell. Explicitly invoking
		// cmd.exe makes `&&` portable across those shells and ensures uninstall
		// only runs after install succeeds — a stale bucket or transient failure
		// therefore never removes the working CLI. (The uninstall→reset link is
		// weaker: `scoop uninstall` ends in an unconditional `exit 0`, so reset
		// runs even when the uninstall skipped its work. That is harmless, since
		// reset is a no-op on the Scoop versions that make it unnecessary.)
		//
		// `scoop install` also auto-refreshes the bucket when >3h stale
		// (is_scoop_outdated), so the new manifest usually lands
		// without an explicit refresh; a bucket clone older than that check can
		// still fail with "couldn't find manifest", and the README migration
		// section names `scoop update` as the retry. Binaries already on the
		// `entire` app just update in place.
		if scoopAppName() == "cli" {
			return `cmd.exe /D /C "scoop install entire/entire && scoop uninstall entire/cli && scoop reset entire"`
		}
		return "scoop update entire/entire"
	}

	if goos == goosWindows {
		if releaseChannel(currentVersion) == installChannelNightly {
			return windowsInstallNightlyCmd
		}
		return windowsInstallCmd
	}

	if releaseChannel(currentVersion) == installChannelNightly {
		return "curl -fsSL https://entire.io/install.sh | bash -s -- --channel nightly"
	}
	return "curl -fsSL https://entire.io/install.sh | bash"
}

func UpdateCommandForCurrentBinary(currentVersion string) string {
	return updateCommand(currentVersion)
}

// printNotification prints the version update notification to the user.
func printNotification(w io.Writer, current, latest string) {
	fmt.Fprintf(w, "\nUpdate available! %s -> %s\nRelease notes: %s\n",
		displayVersion(current), displayVersion(latest), releaseNotesURL(latest))
}
