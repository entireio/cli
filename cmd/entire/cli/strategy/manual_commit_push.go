package strategy

import (
	"context"

	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// PrePush is called by the git pre-push hook before pushing to a remote.
// It pushes the entire/checkpoints/v1 branch alongside the user's push.
// Configuration options (stored in .entire/settings.json under strategy_options.push_sessions):
//   - "auto": always push automatically
//   - "prompt" (default): ask user with option to enable auto
//   - "false"/"off"/"no": never push
func (s *ManualCommitStrategy) PrePush(ctx context.Context, remote string) error {
	if err := pushSessionsBranchCommon(ctx, remote, paths.MetadataBranchName); err != nil {
		return err
	}
	return PushTrailsBranch(ctx, remote)
}
