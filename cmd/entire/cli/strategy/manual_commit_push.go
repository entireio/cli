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
// pushed to the derived URL instead of the user's push remote.
//
// Configuration options (stored in .entire/settings.json under strategy_options):
//   - push_sessions: false to disable automatic pushing of checkpoints
//   - checkpoint_remote: {"provider": "github", "repo": "org/repo"} to push to a separate repo
func (s *ManualCommitStrategy) PrePush(ctx context.Context, remote string) error {
	// Load settings once for remote resolution and push_sessions check
	ps := resolvePushSettings(ctx, remote)

	if ps.pushDisabled {
		return nil
	}

	_, pushCheckpointsSpan := perf.Start(ctx, "push_checkpoints_branch")
	err := pushBranchIfNeeded(ctx, ps.pushTarget(), paths.MetadataBranchName)
	if err != nil {
		pushCheckpointsSpan.RecordError(err)
	}
	pushCheckpointsSpan.End()
	return err
}
