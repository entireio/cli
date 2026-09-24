package remote

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// IgnoredCheckpointRemoteGuidance supplies the same ownership explanation and
// actionable command to status, enable, and pre-push. A mismatched owner must
// never produce a command that sends a fork contributor's transcripts upstream.
func IgnoredCheckpointRemoteGuidance(ctx context.Context, config *settings.CheckpointRemoteConfig, verdict OwnershipVerdict, reason string) (explanation, command string) {
	if !verdict.Refused() || config == nil {
		return "", ""
	}
	if verdict == OwnershipDisproved {
		return reason + fmt.Sprintf(". If this is a fork, do not claim this store: your transcripts would go to %q's store", config.Owner()), ""
	}
	if rejection := settings.CheckpointRemoteLocalClaimRejection(ctx); rejection != "" {
		return reason + ". " + rejection, ""
	}
	explanation = reason + ". The checkpoint_remote comes from .entire/settings.json and may be inherited from another project; declaring it in your own untracked .entire/settings.local.json confirms it for this clone and skips the owner check"
	command = ClaimCheckpointRemoteCommand(config)
	if command == "" {
		explanation += ". If this store is yours, set checkpoint_remote in that local file"
	}
	return explanation, command
}

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
