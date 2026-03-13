package strategy

import (
	"context"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/perf"
)

// PrePush is called by the git pre-push hook before pushing to a remote.
// It pushes the entire/checkpoints/v1 branch alongside the user's push.
//
// If a checkpoint_remote is configured in settings, the checkpoint branch is
// pushed to the derived URL instead of the user's push remote. Trails always
// go to the user's push remote.
//
// Configuration options (stored in .entire/settings.json under strategy_options):
//   - push_sessions: false to disable automatic pushing of checkpoints
//   - checkpoint_remote: {"provider": "github", "repo": "org/repo"} to push to a separate repo
func (s *ManualCommitStrategy) PrePush(ctx context.Context, remote string) error {
	// Load settings once for both remote resolution and push_sessions check
	ps := resolvePushSettings(ctx, remote)

	// Checkpoints: pushed to checkpoint URL if configured, otherwise to push remote
	if !ps.pushDisabled {
		_, pushCheckpointsSpan := perf.Start(ctx, "push_checkpoints_branch")
		if err := pushBranchIfNeeded(ctx, ps.pushTarget(), paths.MetadataBranchName); err != nil {
			pushCheckpointsSpan.RecordError(err)
			pushCheckpointsSpan.End()
			return err
		}
		pushCheckpointsSpan.End()
	}

	// Trails: always push to the user's push remote
	_, pushTrailsSpan := perf.Start(ctx, "push_trails_branch")
	err := pushBranchIfNeeded(ctx, ps.remote, paths.TrailsBranchName)
	pushTrailsSpan.RecordError(err)
	pushTrailsSpan.End()
	return err
}
