package strategy

import (
	"context"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/perf"
)

// PrePush is called by the git pre-push hook before pushing to a remote.
// It pushes the checkpoint metadata ref alongside the user's push.
// Legacy checkpoints v2 settings are ignored and warn before falling back to v1.
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

	settings.WarnIfCheckpointsV2Disallowed(ctx)

	// Thread the span's context into the push so the network push and any
	// fetch+rebase recovery nest beneath it as child steps in the perf trace.
	// Use ps.remote (the user's actual push remote, e.g. "upstream") for the
	// tracking ref, not pushTarget() which can be the checkpoint_remote URL —
	// the tracking ref must be the local mirror of the remote being pushed
	// to, not the URL push target.
	pushCtx, pushCheckpointsSpan := perf.Start(ctx, "push_checkpoints_branch")
	err := pushRefIfNeeded(pushCtx, ps.pushTarget(),
		checkpoint.MetadataRef(ctx),
		checkpoint.MetadataTrackingRefForRemote(ctx, ps.remote))
	pushCheckpointsSpan.RecordError(err)
	pushCheckpointsSpan.End()

	return err
}
