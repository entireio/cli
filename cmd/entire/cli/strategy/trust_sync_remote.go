package strategy

import (
	"context"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/settings/repopolicy"
)

// Egress consent is keyed on the remote checkpoints actually go to. That
// remote is elected here in strategy (ResolveCheckpointSyncRemote:
// checkpoint_push_remote → captured election → origin → sole → first), which
// the repopolicy leaf cannot import, so the resolver is installed at package
// initialization — the same seam settings uses for LocalSettingsTrusted.
// Every binary that links strategy (the CLI and its hooks) gets the election;
// anything that does not keeps repopolicy's origin default.
var _ = installTrustSyncRemoteResolver()

func installTrustSyncRemoteResolver() struct{} {
	repopolicy.ResolveSyncRemote = resolveTrustSyncRemote
	return struct{}{}
}

type pendingSyncRemoteKey struct{}

// withPendingSyncRemote marks the remote a pre-push is about to elect by
// evidence (see maybeCaptureCheckpointSyncRemote) so consent is asked about
// that destination on the very push that starts carrying checkpoints there.
func withPendingSyncRemote(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, pendingSyncRemoteKey{}, name)
}

// WithSyncRemoteOverride keys trust identity on the named remote instead of
// the current election, for foreground `entire trust --remote <name>`. The
// election is persisted only once checkpoints actually land on a captured
// remote (1046's delivery rule), so a held non-interactive push to a remote
// that is about to be captured cannot be unblocked by plain `entire trust` —
// that would record consent for the still-elected remote. This is how the
// user names the destination the pre-push hold told them about.
func WithSyncRemoteOverride(ctx context.Context, name string) context.Context {
	return withPendingSyncRemote(ctx, name)
}

func pendingSyncRemoteFromContext(ctx context.Context) string {
	name, ok := ctx.Value(pendingSyncRemoteKey{}).(string)
	if !ok {
		return ""
	}
	return name
}

// resolveTrustSyncRemote is the installed repopolicy.ResolveSyncRemote. The
// election helpers are scoped to the current worktree, which is also the
// worktree policy is classified for in production (ClassifyRepoPolicy
// resolves "."), so repository only supplies the root to read remotes from.
//
// Dedicated checkpoint_remote mode: checkpoints go to a URL derived from the
// elected remote, not to the remote itself, so consent names that URL.
// An election error (unreadable settings, checkpoint_push_remote naming a
// missing remote) is returned as-is: sync is disabled in that state and the
// gate fails closed with it.
func resolveTrustSyncRemote(ctx context.Context, repository repopolicy.Repository) (repopolicy.SyncRemote, error) {
	name := pendingSyncRemoteFromContext(ctx)
	if name == "" {
		elected, err := ResolveCheckpointSyncRemote(ctx)
		if err != nil {
			return repopolicy.SyncRemote{}, fmt.Errorf("electing checkpoint sync remote: %w", err)
		}
		name = elected.Name
	}
	if name == "" {
		return repopolicy.SyncRemote{}, nil // no remotes configured: path identity
	}
	if s, err := settings.Load(ctx); err == nil && s.GetCheckpointRemote() != nil {
		if url, enabled, err := remote.PushURL(ctx, name); err == nil && enabled && url != "" {
			return repopolicy.SyncRemote{Name: name, URLs: []string{url}, Dedicated: true}, nil
		}
	}
	return repopolicy.RemoteURLsInDir(ctx, repository.WorktreeRoot, name) //nolint:wrapcheck // the leaf's error already names the remote
}
