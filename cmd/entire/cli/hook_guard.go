// hook_guard.go protects against cross-agent hook forwarding. Cursor IDE
// invokes any hook configured under .claude/settings.json or .cursor/hooks.json
// for the active session — when only one of those files is installed, the
// other agent's hook command receives the event. shouldSkipForwardedHook
// detects this by inspecting the transcript path: if it lives inside another
// registered agent's session directory, the firing agent is forwarded and
// must no-op so the session isn't claimed for the wrong agent (#1262).
package cli

import (
	"context"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// shouldSkipForwardedHook reports whether the firing agent should ignore this
// event because the transcript path proves it belongs to a different
// registered agent. Returns false when:
//   - event has no SessionRef (no signal — fail open)
//   - SessionRef is not inside any registered agent's session directory
//   - SessionRef belongs to the firing agent itself
//   - the worktree root cannot be resolved (fail open; downstream
//     handlers will surface the error)
func shouldSkipForwardedHook(ctx context.Context, ag agent.Agent, event *agent.Event) bool {
	if ag == nil || event == nil || event.SessionRef == "" {
		return false
	}
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return false
	}
	owner, ok := agent.AgentForTranscriptPath(event.SessionRef, repoRoot)
	if !ok {
		return false
	}
	return owner.Name() != ag.Name()
}
