package strategy

import (
	"context"

	"github.com/entireio/cli/cmd/entire/cli/plugins"
)

// firePluginPostCommit dispatches the post_commit observer hook after a commit
// is processed by the strategy. Best-effort: a no-op when no plugin is enabled
// and never propagates plugin failures into the git hook.
func firePluginPostCommit(ctx context.Context, commitSHA, checkpointID string, hasCheckpoint bool) {
	payload := map[string]any{
		"commit":         commitSHA,
		"has_checkpoint": hasCheckpoint,
	}
	if checkpointID != "" {
		payload["checkpoint_id"] = checkpointID
	}
	plugins.FireHook(ctx, plugins.HookPostCommit, payload)
}

// firePluginPrePush dispatches the pre_push observer hook before checkpoint
// refs are pushed. The mutating (veto) variant is added in a later phase.
func firePluginPrePush(ctx context.Context, remote, pushTarget string) {
	payload := map[string]any{"remote": remote}
	if pushTarget != "" {
		payload["push_target"] = pushTarget
	}
	plugins.FireHook(ctx, plugins.HookPrePush, payload)
}
