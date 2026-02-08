package versioncheck

import "time"

// VersionCache represents the cached version check data.
type VersionCache struct {
	LastCheckTime time.Time `json:"last_check_time"`
}

// GitHubRelease represents the GitHub API response for a release.
type GitHubRelease struct {
	TagName    string `json:"tag_name"`
	Prerelease bool   `json:"prerelease"`
}

// githubAPIURL is the GitHub API endpoint for fetching the latest release.
// This is a var (not const) to allow overriding in tests.
var githubAPIURL = "https://api.github.com/repos/entireio/cli/releases/latest"

const (
	// checkInterval is the duration between version checks.
	checkInterval = 24 * time.Hour

	// httpTimeout is the timeout for HTTP requests to the GitHub API.
	httpTimeout = 2 * time.Second

	// cacheFileName is the name of the cache file stored in the global config directory.
	cacheFileName = "version_check.json"

	// globalConfigDirName is the name of the global config directory in the user's home.
	globalConfigDirName = ".entire"
)
