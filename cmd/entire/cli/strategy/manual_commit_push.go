package strategy

import (
	"context"
	"log/slog"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/perf"
)

// PrePush is called by the git pre-push hook before pushing to a remote.
// It pushes each ref in refs.Push alongside the user's push.
//
// If a checkpoint_remote is configured in settings, checkpoint branches/refs
// are pushed to the derived URL instead of the user's push remote.
//
// Configuration options (stored in .entire/settings.json under strategy_options):
//   - push_sessions: false to disable automatic pushing of checkpoints
//   - checkpoint_remote: {"provider": "github", "repo": "org/repo"} to push to a separate repo
func (s *ManualCommitStrategy) PrePush(ctx context.Context, remote string) error {
	// Load settings once for remote resolution and push_sessions check.
	// Spanned because checkpoint-remote resolution can perform a one-time
	// network fetch of the metadata branch (fetchMetadataBranchIfMissing),
	// which is otherwise invisible in the pre-push trace.
	resolveCtx, resolveSpan := perf.Start(ctx, "resolve_push_settings")
	ps := resolvePushSettings(resolveCtx, remote)
	resolveSpan.End()

	if ps.pushDisabled {
		return nil
	}

	refs := checkpoint.ResolveCommittedRefs(ctx)

	refreshMirrorBeforePush(ctx, refs)

	// Thread the span's context into the push so the network push and any
	// fetch+rebase recovery nest beneath it as child steps in the perf trace.
	pushCtx, pushCheckpointsSpan := perf.Start(ctx, "push_checkpoint_refs")
	defer pushCheckpointsSpan.End()
	for _, ref := range refs.Push {
		if err := pushRefIfNeeded(pushCtx, ps.pushTarget(), ref); err != nil {
			pushCheckpointsSpan.RecordError(err)
			return err
		}
	}
	return nil
}

// refreshMirrorBeforePush advances the mirror to the primary tip before
// pushing. Best-effort: failures are logged, never blocking the push.
func refreshMirrorBeforePush(ctx context.Context, refs checkpoint.CommittedRefs) {
	if !refs.HasMirror() {
		return
	}
	repo, err := OpenRepository(ctx)
	if err != nil {
		logging.Debug(ctx, "pre-push mirror refresh skipped: open repository failed",
			slog.String("error", err.Error()))
		return
	}
	defer repo.Close()
	mirrorCommittedMetadataRefBestEffort(ctx, repo, refs)
}
