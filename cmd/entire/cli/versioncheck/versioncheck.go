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

	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/entireio/cli/internal/entireclient/httpclient"
	"github.com/entireio/cli/internal/entireclient/userdirs"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
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
	cache, cacheErr := loadCache()
	if cacheErr != nil {
		cache = &VersionCache{}
	}

	// Skip if we checked recently (within 24 hours)
	if time.Since(cache.LastCheckTime) < checkInterval {
		return
	}

	// Fetch the latest version for this binary's channel. The origin chain
	// (our release origin, then GitHub) is resolved inside this call, which
	// matters: the cache is stamped below regardless of the outcome, so an
	// origin that only gets tried after that point would never get a second
	// chance within the 24h window.
	latestVersion, err := fetchLatestForChannel(ctx, isNightly(currentVersion))

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

		action := maybeAutoUpdate(ctx, w, currentVersion, latestVersion)
		if action == autoUpdateActionSkipUntilNextVersion {
			cache.SkippedVersion = versionCacheKey(latestVersion)
			if saveErr := saveCache(cache); saveErr != nil {
				logging.Debug(ctx, "version check: failed to save skipped version",
					"error", saveErr.Error())
			}
		}
	}
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

// versionOrigin is one candidate in the version-lookup chain.
type versionOrigin struct {
	name  string
	fetch func(ctx context.Context, client *http.Client, nightly bool) (string, error)
}

// versionOrigins is the ordered candidate chain: the release origin we own
// first, GitHub second. A var so tests can install their own chain.
//
// The ordering rules mirror strategy.CheckpointReadRemotesWithElection, the
// closest precedent in this repo — candidates in a fixed order, first success
// wins, and the chain fails open: every origin failing is reported as a single
// error, never as "you are up to date".
var versionOrigins = func() []versionOrigin {
	return []versionOrigin{
		{name: "install.entire.com", fetch: fetchChannelManifestVersion},
		{name: "api.github.com", fetch: fetchGitHubVersion},
	}
}

// fetchLatestForChannel resolves the newest version on channel from the first
// origin that answers. httpTimeout bounds the whole chain, not each origin —
// see its doc comment for why that ceiling is absolute.
func fetchLatestForChannel(ctx context.Context, nightly bool) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()

	// New() supplies the shared redirect policy (no HTTPS downgrades, capped
	// chains), which this path needs because the GitHub candidate redirects.
	// NewTransport supplies the shared dial budget and
	// ENTIRE_CONNECT_TIMEOUT_SECONDS, which http.DefaultTransport would not.
	client := httpclient.New(httpclient.Options{Transport: httpclient.NewTransport(false)})
	defer client.CloseIdleConnections()

	// Each candidate gets an equal share of whatever is left, so a primary that
	// hangs cannot consume the whole budget and starve the fallback. That is
	// not a per-origin timeout multiplied into a ceiling that never binds: the
	// shares divide one absolute deadline, so the chain still cannot exceed
	// httpTimeout. Without it a silently dropped primary — a firewall that
	// blackholes rather than refuses — takes the update check down for good,
	// which is strictly worse than before this origin existed.
	deadline, hasDeadline := ctx.Deadline()

	var firstErr error
	origins := versionOrigins()
	for i, origin := range origins {
		if ctx.Err() != nil {
			// The budget is spent. Stop rather than issuing a request that
			// can only fail, so the error we report is the one that matters.
			break
		}

		attemptCtx := ctx
		cancelAttempt := context.CancelFunc(func() {})
		if hasDeadline {
			share := time.Until(deadline) / time.Duration(len(origins)-i)
			if share <= 0 {
				break
			}
			attemptCtx, cancelAttempt = context.WithTimeout(ctx, share)
		}

		version, err := origin.fetch(attemptCtx, client, nightly)
		cancelAttempt()
		if err == nil {
			if i > 0 {
				// Which candidate actually answered. Without this a silently
				// dead primary is invisible until the day GitHub is also down.
				logging.Debug(ctx, "version check: answered by fallback origin",
					"origin", origin.name, "index", i, "channel", channelName(nightly))
			}
			return version, nil
		}

		logging.Debug(ctx, "version check: origin failed",
			"origin", origin.name, "channel", channelName(nightly), "error", err.Error())
		if firstErr == nil {
			firstErr = fmt.Errorf("%s: %w", origin.name, err)
		}
	}

	if firstErr == nil {
		// Nothing was tried. Distinguish "the caller's context was already
		// done" from "the chain is empty" — reporting the latter for the
		// former sends anyone reading a debug log after the wrong thing.
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("version check budget: %w", err)
		}
		return "", errors.New("no version origins configured")
	}
	return "", firstErr
}

// fetchChannelManifestVersion reads the channel pointer we publish at release
// time. Unlike the GitHub paths it needs no list-and-filter: the manifest
// names one version, which is the whole point of publishing it.
func fetchChannelManifestVersion(ctx context.Context, client *http.Client, nightly bool) (string, error) {
	channel := channelName(nightly)
	url := strings.TrimSuffix(channelBaseURL, "/") + "/channels/" + channel + ".json"

	body, err := getVersionJSON(ctx, client, url, "application/json")
	if err != nil {
		return "", err
	}

	var manifest ChannelManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return "", fmt.Errorf("parsing manifest: %w", err)
	}

	version := manifest.Tag
	if version == "" {
		version = manifest.Version
	}
	if version == "" {
		return "", errors.New("manifest names no version")
	}
	// Reject anything that is not a version before the channel checks, because
	// those checks pass for garbage: isNightly is false for an unparseable
	// string, which matches the stable channel. Accepting it would end the
	// chain here, and isOutdated then reports no update because an invalid
	// version sorts below every valid one — so a corrupt pointer on our own
	// origin would silently switch update checks off instead of falling
	// through to GitHub.
	// Canonical form, not merely valid: semver.IsValid accepts "v1.2" and "v1"
	// with an implied patch, but the artifact URLs are built as
	// v<version>/<file>, so a short form resolves to a prefix that does not
	// exist. Comparing against Canonical also rejects build metadata, which no
	// release tag carries.
	if tag := semverTag(version); !semver.IsValid(tag) || semver.Canonical(tag) != tag {
		return "", fmt.Errorf("manifest names %q, which is not a release version", version)
	}

	// A manifest published to the wrong key, or left stale across a channel
	// rename, would otherwise cross the streams — nudging stable users onto a
	// nightly, or nightly users backwards onto stable. Both checks are cheap
	// and turn a silent mis-upgrade into a skipped check.
	if manifest.Channel != "" && manifest.Channel != channel {
		return "", fmt.Errorf("manifest declares channel %q, wanted %q", manifest.Channel, channel)
	}
	// Stable means no prerelease at all, not merely "not a nightly". An rc tag
	// would otherwise be accepted here and pushed to every stable user, and it
	// is also the form install.sh rejects — the two readers have to agree about
	// what a channel contains.
	if nightly {
		if !isNightly(version) {
			return "", fmt.Errorf("version %q does not belong to channel %q", version, channel)
		}
	} else if pre := semver.Prerelease(semverTag(version)); pre != "" {
		return "", fmt.Errorf("version %q is a prerelease (%s) and does not belong to channel %q", version, pre, channel)
	}

	return version, nil
}

// fetchGitHubVersion is the fallback origin: the GitHub releases API.
func fetchGitHubVersion(ctx context.Context, client *http.Client, nightly bool) (string, error) {
	if nightly {
		return fetchLatestNightlyVersion(ctx, client)
	}
	return fetchLatestVersion(ctx, client)
}

// getVersionJSON performs one GET within the caller's context and returns the
// bounded body. The client belongs to the chain in fetchLatestForChannel, so
// every origin is fetched with the same configured client rather than an
// ad-hoc one per call site.
func getVersionJSON(ctx context.Context, client *http.Client, url, accept string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", versioninfo.UserAgent())

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	return body, nil
}

// fetchLatestVersion fetches the latest stable version tag from the GitHub API.
// The chain in fetchLatestForChannel owns the timeout and the client.
func fetchLatestVersion(ctx context.Context, client *http.Client) (string, error) {
	body, err := getVersionJSON(ctx, client, githubAPIURL, "application/vnd.github+json")
	if err != nil {
		return "", err
	}

	version, err := parseGitHubRelease(body)
	if err != nil {
		return "", fmt.Errorf("parsing release: %w", err)
	}
	return version, nil
}

// channelName is a channel's published name: the path segment under channels/
// on the release origin and the value its manifest declares. It is the only
// place the nightly predicate becomes a string, because those two wire values
// are the only things that need one.
func channelName(nightly bool) string {
	if nightly {
		return "nightly"
	}
	return "stable"
}

// semverTag normalizes a version to the "v"-prefixed form the semver package
// expects. Both the nightly predicate and the manifest validation go through
// it so they cannot disagree about what they are parsing.
func semverTag(version string) string {
	if !strings.HasPrefix(version, "v") {
		return "v" + version
	}
	return version
}

// isNightly reports whether a version is a nightly build, identified by the
// literal "-nightly." prerelease segment that create-nightly-tag.sh produces.
//
// A substring test for "nightly" is too loose, and the looseness is not
// harmless: it accepts v0.5.4-nightly and v0.5.4-prenightly, which install.sh's
// version_valid_for_channel and the GitHub release scan below both reject. The
// CLI would then announce an update the installer refuses to install. All four
// places that answer this question test the same segment.
func isNightly(version string) bool {
	return strings.HasPrefix(semver.Prerelease(semverTag(version)), "-nightly.")
}

// fetchLatestNightlyVersion fetches the latest nightly version tag from the GitHub releases list.
// The chain in fetchLatestForChannel owns the timeout and the client.
func fetchLatestNightlyVersion(ctx context.Context, client *http.Client) (string, error) {
	body, err := getVersionJSON(ctx, client, githubReleasesURL+"?per_page=20", "application/vnd.github+json")
	if err != nil {
		return "", err
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

// normalizePath returns p with symlinks resolved (best-effort), separators as
// forward slashes, and case folded where the OS compares paths
// case-insensitively (foldPathCase). Every install-path comparison in this
// package is a plain prefix or substring test on this form.
func normalizePath(p string) string {
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		resolved = p
	}
	return foldPathCase(strings.ReplaceAll(filepath.ToSlash(resolved), "\\", "/"))
}

// downloadsURL is the GitHub releases page, used for release-notes links.
const downloadsURL = "https://github.com/entireio/cli/releases"

// installProbe identifies one install manager from the running binary's path
// and returns the command that upgrades it. roots are env/config install
// prefixes (relocated dirs, optional); markers cover default layouts.
type installProbe struct {
	roots   func() []string
	markers []string
	command func(execPath, currentVersion string) string
}

func miseRoots() []string {
	if d := os.Getenv("MISE_INSTALLS_DIR"); d != "" {
		return []string{d}
	}
	if d := os.Getenv("MISE_DATA_DIR"); d != "" {
		return []string{filepath.Join(d, "installs")}
	}
	return nil
}

// normalizeInstallRoot returns an absolute root in normalizePath form with a
// trailing slash, or "" for empty/relative values. A root that does not exist
// on disk still matches by cleaned prefix.
func normalizeInstallRoot(root string) string {
	if root == "" || !filepath.IsAbs(root) {
		return ""
	}
	n := normalizePath(filepath.Clean(root))
	if !strings.HasSuffix(n, "/") {
		n += "/"
	}
	return n
}

func (p installProbe) matches(execPath string) bool {
	if p.roots != nil {
		for _, raw := range p.roots() {
			if root := normalizeInstallRoot(raw); root != "" && strings.HasPrefix(execPath, root) {
				return true
			}
		}
	}
	for _, marker := range p.markers {
		if strings.Contains(execPath, marker) {
			return true
		}
	}
	return false
}

const miseUpgradeCmd = "mise upgrade entire"

func miseUpgradeCommand(_, _ string) string {
	return miseUpgradeCmd
}

var miseProbe = installProbe{
	roots:   miseRoots,
	markers: []string{"/mise/installs/"},
	command: miseUpgradeCommand,
}

// UpdateCommandForCurrentBinary returns the shell command that updates this
// binary, based on how it was installed.
//
// The fallback receives the exec path as the OS reports it (not normalized),
// so the Windows installer hint can name the directory the binary lives in.
func UpdateCommandForCurrentBinary(currentVersion string) string {
	execPath, err := executablePath()
	if err != nil {
		return fallbackInstallCommand("", currentVersion)
	}
	normalized := normalizePath(execPath)
	for _, p := range installProbes {
		if p.matches(normalized) {
			return p.command(normalized, currentVersion)
		}
	}
	return fallbackInstallCommand(execPath, currentVersion)
}

// printNotification prints the version update notification to the user.
func printNotification(w io.Writer, current, latest string) {
	fmt.Fprintf(w, "\nUpdate available! %s -> %s\nRelease notes: %s\n",
		displayVersion(current), displayVersion(latest), releaseNotesURL(latest))
}
