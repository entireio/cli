package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	git "github.com/go-git/go-git/v6"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// Push seams, as package vars so the sync path's branching is testable without
// a remote or a real push. Production wiring is the real functions (same pattern
// as the login seams in import_sync_notice.go).
var (
	importResolvePushRemote = resolveCheckpointPushRemote
	importPushQueuedRefs    = strategy.PushQueuedCheckpointRefs
)

// syncImportedCheckpoints pushes checkpoints an import wrote to the checkpoint
// sync remote, so imported history reaches the dashboard without waiting for an
// unrelated `git push`.
//
// Import writes read-only checkpoints and creates no commit, so the pre-push git
// hook — the only sync trigger — never fires for it. Before this, history
// imported while logged out stayed local-only forever: import's idempotency is
// keyed on local existence, so a post-login re-run reported "0 imported (N
// already imported)" and made no attempt to sync (issue #1773).
//
// It therefore runs on every logged-in import, not only one that wrote new
// turns. The git-refs store enqueues each checkpoint ref as it is written
// (checkpoint.gitRefsStore.setRef) and nothing drains that queue but a push, so
// turns imported while logged out are still queued afterwards and a re-run once
// logged in is what recovers them.
//
// Best-effort by design: the import itself already succeeded locally, so a push
// failure is reported and swallowed rather than failing the command.
func syncImportedCheckpoints(ctx context.Context, w io.Writer, repo *git.Repository, explicitRemote string) {
	if !importLoggedIn() {
		// warnIfImportNotSynced has already explained that this stays local.
		return
	}

	// git-branch primary has no push queue — its checkpoints live on the
	// entire/checkpoints/v1 branch, which the pre-push hook ships alongside the
	// user's own push. Draining a queue it never fills would be a no-op that
	// still paid for a checkpoint-policy sync, so skip the backend entirely.
	cpCfg, cfgErr := settings.LoadCheckpointsConfig(ctx)
	if cfgErr != nil || !checkpoint.PrimaryIsRefs(cpCfg) {
		return
	}

	remote, err := importResolvePushRemote(ctx, explicitRemote)
	if err != nil {
		logging.Debug(ctx, "import: skipping checkpoint sync", "error", err.Error())
		fmt.Fprintf(w, "Note: imported history is not synced yet: %v\n", err)
		return
	}

	pushed, pushDisabled, err := importPushQueuedRefs(ctx, repo, remote)
	switch {
	case err != nil:
		if errors.Is(err, context.Canceled) {
			return
		}
		logging.Warn(ctx, "import: checkpoint sync failed", "remote", remote, "error", err.Error())
		fmt.Fprintf(w, "Note: could not sync imported history to %s: %v\n", remote, err)
	case pushDisabled:
		fmt.Fprintln(w, "Checkpoint pushing is disabled in settings; imported history stays local.")
	case pushed == 0:
		// Nothing queued — everything this import produced was already synced.
	default:
		fmt.Fprintf(w, "Synced %d checkpoint(s) to %s.\n", pushed, remote)
	}
}
