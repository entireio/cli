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
	"os"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/devin"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
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

// shouldSkipDevinCrossFiredHook reports whether a claude-code hook invocation
// was actually spawned by Devin CLI. Devin loads .claude/settings.json hooks
// by default (its Claude Code compatibility layer), so in a repo where Entire
// is enabled for claude-code, Devin sessions invoke `entire hooks claude-code
// <verb>` with Devin payloads — which carry no transcript_path, so the
// transcript-ownership guard above cannot catch them. Devin sets
// DEVIN_PROJECT_DIR in every hook process it spawns (verified live, 3000.2.17,
// which also confirmed it does NOT set CLAUDE_PROJECT_DIR), while real Claude
// Code always sets CLAUDE_PROJECT_DIR for its hooks — so requiring
// CLAUDE_PROJECT_DIR to be absent keeps a genuine Claude Code session working
// even when it runs nested inside a Devin environment that leaked
// DEVIN_PROJECT_DIR. Devin's own hooks (installed in .devin/hooks.v1.json)
// handle the session instead.
func shouldSkipDevinCrossFiredHook(agentName types.AgentName) bool {
	return agentName == agent.AgentNameClaudeCode &&
		os.Getenv(devin.ProjectDirEnv) != "" &&
		os.Getenv("CLAUDE_PROJECT_DIR") == ""
}
