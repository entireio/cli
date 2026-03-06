package strategy

import (
	"context"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/perf"
)

// PrePush is called by the git pre-push hook before pushing to a remote.
// It pushes the entire/checkpoints/v1 branch alongside the user's push.
// Configuration options (stored in .entire/settings.json under strategy_options.push_sessions):
//   - "auto": always push automatically
//   - "prompt" (default): ask user with option to enable auto
//   - "false"/"off"/"no": never push
func (s *ManualCommitStrategy) PrePush(ctx context.Context, remote string) error {
	_, pushCheckpointsSpan := perf.Start(ctx, "push_checkpoints_branch")
	if err := pushSessionsBranchCommon(ctx, remote, paths.MetadataBranchName); err != nil {
		pushCheckpointsSpan.RecordError(err)
		pushCheckpointsSpan.End()
		return err
	}
	pushCheckpointsSpan.End()

	_, pushTrailsSpan := perf.Start(ctx, "push_trails_branch")
	err := PushTrailsBranch(ctx, remote)
	pushTrailsSpan.RecordError(err)
	pushTrailsSpan.End()
	return err
}
