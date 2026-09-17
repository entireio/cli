package remote

import (
	"fmt"
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
func ClaimCheckpointRemoteCommand(config *settings.CheckpointRemoteConfig) string {
	if config == nil {
		return ""
	}
	provider := strings.TrimSpace(config.Provider)
	repo := strings.TrimSpace(config.Repo)
	// Only providers `entire enable --checkpoint-remote` actually accepts. The
	// resolver is wider than the flag — providerHost maps gitlab too, and
	// GetCheckpointRemote validates no provider at all — so a hand-written
	// gitlab checkpoint_remote resolves end to end while the command naming it
	// is rejected. Printing that command would be the exact failure this
	// function exists to prevent, so an unsupported provider gets no command
	// and the caller falls back to naming the settings file. Widening the flag
	// is tracked separately; do not widen this list to match the resolver
	// without widening parseCheckpointRemoteFlag first.
	if provider != "github" {
		return ""
	}
	if strings.ContainsAny(provider, ": \t") {
		return ""
	}
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.ContainsAny(repo, " \t") {
		return ""
	}
	return fmt.Sprintf("entire enable --local --checkpoint-remote %s:%s", provider, repo)
}
