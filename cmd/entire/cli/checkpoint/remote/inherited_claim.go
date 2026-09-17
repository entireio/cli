package remote

import (
	"regexp"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// ClaimCheckpointRemoteCommand is the one command that resolves a
// checkpoint_remote the ownership check refused: it re-declares the same store
// in .entire/settings.local.json, which is gitignored and per-clone and so
// cannot have been inherited by cloning — the one signal
// checkpointRemoteIsInherited accepts as proof the store is this developer's
// own.
//
// It exists so every surface that reports the rejection (`entire status`, the
// pre-push warning) names the same fix. Before it, the rejection was reported
// with the settings FILE to edit, which is a different and much longer job than
// running a command, and is why an ignored checkpoint_remote kept being
// discovered rather than fixed.
//
// Returns "" rather than a command that would fail when pasted: the entry can
// be malformed (a repo with no owner, a provider the flag does not accept), and
// a remedy that errors is worse than none. parseCheckpointRemoteFlag is the
// authority on what parses, and TestClaimCommandParsesAsACheckpointRemoteFlag
// pins this against it.
// claimRepoPattern is what may be spliced into the suggested command: two
// path segments of the characters GitHub actually allows in an owner or a
// repository name, and nothing else.
//
// An ALLOWLIST, not a denylist, because the value is attacker-supplied and the
// output is a command a human is told to run. config.Repo is read from the
// COMMITTED .entire/settings.json — the inherited-from-upstream case this whole
// feature exists for — so a hostile repository controls it exactly. An earlier
// version rejected only whitespace, which let
// `acme/foo;id`, "acme/foo`id`", `acme/foo$(id)`, `acme/foo|id` and
// `acme/foo&&id` through verbatim into a copy-pasteable shell command printed
// by `entire status` and on every `git push`. Enumerating shell metacharacters
// would be the same mistake one character later; only the permitted set is
// safe to reason about.
var claimRepoPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)

// ClaimCheckpointRemoteCommand is the one command that resolves a
// checkpoint_remote the ownership check refused: it re-declares the same store
// in .entire/settings.local.json, which is gitignored and per-clone and so
// cannot have been inherited by cloning — the one signal
// checkpointRemoteIsInherited accepts as proof the store is this developer's
// own.
//
// It exists so every surface that reports the rejection (`entire status`, the
// pre-push warning) names the same fix. Before it, the rejection was reported
// with the settings FILE to edit, which is a different and much longer job than
// running a command, and is why an ignored checkpoint_remote kept being
// discovered rather than fixed.
//
// Returns "" rather than a command that would fail when pasted — or worse, one
// that does something other than what it appears to. Both the provider and the
// repo must be exactly what `entire enable --checkpoint-remote` accepts;
// anything else gets no command and the caller falls back to naming the
// settings file.
//
// The provider is pinned to the single value parseCheckpointRemoteFlag accepts.
// The resolver is wider than the flag — providerHost maps gitlab too, and
// GetCheckpointRemote validates no provider at all — so a hand-written gitlab
// checkpoint_remote resolves end to end while the command naming it is
// rejected. Do not widen this without widening parseCheckpointRemoteFlag first.
func ClaimCheckpointRemoteCommand(config *settings.CheckpointRemoteConfig) string {
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
	return "entire enable --local --checkpoint-remote github:" + repo
}
