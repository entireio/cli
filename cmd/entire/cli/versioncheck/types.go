package versioncheck

import "time"

// VersionCache represents the cached version check data.
type VersionCache struct {
	LastCheckTime  time.Time `json:"last_check_time"`
	SkippedVersion string    `json:"skipped_version,omitempty"`
}

// GitHubRelease represents the GitHub API response for a release.
type GitHubRelease struct {
	TagName    string `json:"tag_name"`
	Prerelease bool   `json:"prerelease"`
}

// ChannelManifest is the per-channel pointer file published to the release
// origin at release time (channels/stable.json, channels/nightly.json). It
// answers exactly one question — "what is the current version on this
// channel" — and deliberately carries no checksums: checksums.txt, which
// GoReleaser generates and mirrors alongside the archives, stays the single
// integrity source. Two places listing the same hashes is two places that
// can disagree.
type ChannelManifest struct {
	Channel     string `json:"channel"`
	Tag         string `json:"tag"`
	Version     string `json:"version"`
	PublishedAt string `json:"published_at,omitempty"`
}

// channelBaseURL is the release origin we own, and the first candidate in the
// version-lookup chain. Reading it before api.github.com means the update
// check keeps working through a GitHub outage, and — because it is read on
// every check rather than only during an incident — the path cannot rot into
// a failover that has never actually run.
//
// This is a var (not const) to allow overriding in tests.
var channelBaseURL = "https://install.entire.com"

// githubAPIURL is the GitHub API endpoint for fetching the latest stable release.
// This is a var (not const) to allow overriding in tests.
var githubAPIURL = "https://api.github.com/repos/entireio/cli/releases/latest"

// githubReleasesURL is the GitHub API endpoint for listing releases (used for nightly checks).
var githubReleasesURL = "https://api.github.com/repos/entireio/cli/releases"

const (
	// checkInterval is the duration between version checks.
	checkInterval = 24 * time.Hour

	// httpTimeout is the budget for the WHOLE origin chain, not per origin.
	//
	// It is an absolute number for the reason spelled out in
	// checkpoint/remote/checkpoint_ref.go: a ceiling computed as
	// origins × per-origin-timeout equals the loop's own worst case, so it
	// can never bind. Keeping it at the historical 2s means adding a second
	// origin costs no extra worst-case latency on this path, which runs
	// synchronously after every command (see root.go's PersistentPostRun).
	//
	// fetchLatestForChannel divides this budget between the candidates rather
	// than handing the whole of it to the first one, so a primary that hangs
	// cannot starve the fallback. An earlier version of this comment claimed a
	// hang "degrades to exactly today's behaviour"; it does not. Before this
	// origin existed the check reached GitHub and worked, so a blackholed
	// primary consuming the budget would have been a regression, not a no-op.
	httpTimeout = 2 * time.Second

	// cacheFileName is the name of the cache file stored in the global config directory.
	cacheFileName = "version_check.json"
)
