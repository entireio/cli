package strategy

import (
	"context"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/perf"
)

// PrePush is called by the git pre-push hook before pushing to a remote.
// It pushes the entire/checkpoints/v1 branch alongside the user's push (unless
// v1 writes are disabled by checkpoints_version: 2), and pushes v2 refs whenever
// IsPushV2RefsEnabled is true — i.e. either checkpoints_v2 + push_v2_refs, or
// checkpoints_version: 2.
//
// If a checkpoint_remote is configured in settings, checkpoint branches/refs
// are pushed to the derived URL instead of the user's push remote.
//
// Configuration options (stored in .entire/settings.json under strategy_options):
//   - push_sessions: false to disable automatic pushing of checkpoints
//   - checkpoint_remote: {"provider": "github", "repo": "org/repo"} to push to a separate repo
//   - push_v2_refs: true to enable pushing v2 refs (requires checkpoints_v2)
//   - checkpoints_version: 2 to skip the v1 metadata branch entirely and force v2 ref pushes on
func (s *ManualCommitStrategy) PrePush(ctx context.Context, remote string) error {
	// Load settings once for remote resolution and push_sessions check
	ps := resolvePushSettings(ctx, remote)

	if ps.pushDisabled {
		return nil
	}

	var err error
	if settings.CheckpointsVersion(ctx) != 2 {
		_, pushCheckpointsSpan := perf.Start(ctx, "push_checkpoints_branch")
		err = pushBranchIfNeeded(ctx, ps.pushTarget(), paths.MetadataBranchName)
		if err != nil {
			pushCheckpointsSpan.RecordError(err)
		}
		pushCheckpointsSpan.End()
	}

	// Push v2 refs when enabled.
	if settings.IsPushV2RefsEnabled(ctx) {
		_, pushV2Span := perf.Start(ctx, "push_v2_refs")
		pushV2Refs(ctx, ps.pushTarget())
		pushV2Span.End()
	}

	// Push gmeta ref when gmeta is enabled.
	if settings.IsGmetaEnabled(ctx) {
		_, pushGmetaSpan := perf.Start(ctx, "push_gmeta_ref")
		pushGmetaRef(ctx, ps.pushTarget())
		pushGmetaSpan.End()
	}

	return err
}
