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

// claimNestedRepoPattern is claimRepoPattern for GitLab, whose projects can sit
// in nested groups (group/subgroup/project). The alphabet is the same, so the
// shell-safety argument above holds unchanged; only the segment count differs.
// The owner compared by the ownership vote is the first segment on both sides
// (CheckpointRemoteConfig.Owner, gitremote's splitOwnerRepo).
var claimNestedRepoPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)+$`)

// ClaimCheckpointRemoteFlagValue returns the validated `provider:repo` value for
// `entire enable --checkpoint-remote`, or empty when the configured entry is not
// something that flag would accept. The single source of truth for both the
// command printed to the user and the value written on their behalf — building
// them separately let a field with surrounding whitespace pass the check and
// fail the write.
//
// The providers are exactly the ones parseCheckpointRemoteFlag accepts (github
// and gitlab); GetCheckpointRemote validates no provider at all, so anything
// else would print a command that fails when pasted. Keep the two in step:
// TestClaimCommandParsesAsACheckpointRemoteFlag runs every offered value
// through the real parser.
func ClaimCheckpointRemoteFlagValue(config *settings.CheckpointRemoteConfig) string {
	if config == nil {
		return ""
	}
	repo := strings.TrimSpace(config.Repo)
	switch provider := strings.ToLower(strings.TrimSpace(config.Provider)); provider {
	case ProviderGitHub:
		if !claimRepoPattern.MatchString(repo) {
			return ""
		}
		return provider + ":" + repo
	case ProviderGitLab:
		if !claimNestedRepoPattern.MatchString(repo) {
			return ""
		}
		return provider + ":" + repo
	default:
		return ""
	}
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
