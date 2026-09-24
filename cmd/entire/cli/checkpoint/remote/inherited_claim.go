package remote

import (
	"regexp"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// claimRepoPattern is an ALLOWLIST, not a denylist, because config.Repo is read
// from the COMMITTED .entire/settings.json — the inherited-from-upstream case
// this feature exists for — and the output is a command a human is told to run.
// An earlier version rejected only whitespace, which let `acme/foo;id`,
// "acme/foo`id`", `acme/foo$(id)`, `acme/foo|id` and `acme/foo&&id` through
// verbatim. Enumerating shell metacharacters is the same mistake one character
// later.
var claimRepoPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)

// ClaimCheckpointRemoteFlagValue returns the validated `provider:repo` value for
// `entire enable --checkpoint-remote`, or empty when the configured entry is not
// something that flag would accept. The single source of truth for both the
// command printed to the user and the value written on their behalf — building
// them separately let a field with surrounding whitespace pass the check and
// fail the write.
//
// The provider is pinned to the one value parseCheckpointRemoteFlag takes. The
// resolver is wider — providerHost maps gitlab, GetCheckpointRemote validates
// nothing — so a hand-written gitlab store resolves while the command naming it
// is rejected. Do not widen this without widening the flag first.
func ClaimCheckpointRemoteFlagValue(config *settings.CheckpointRemoteConfig) string {
	if config == nil {
		return ""
	}
	if strings.TrimSpace(config.Provider) != "github" {
		return ""
	}
	repo := strings.TrimSpace(config.Repo)
	if !claimRepoPattern.MatchString(repo) {
		return ""
	}
	return "github:" + repo
}

// ClaimCheckpointRemoteCommand returns the command that claims a refused
// checkpoint_remote for this clone, so every surface reporting the rejection
// names the same fix. Empty when the entry is not one the flag would accept.
func ClaimCheckpointRemoteCommand(config *settings.CheckpointRemoteConfig) string {
	value := ClaimCheckpointRemoteFlagValue(config)
	if value == "" {
		return ""
	}
	return "entire enable --local --checkpoint-remote " + value
}
