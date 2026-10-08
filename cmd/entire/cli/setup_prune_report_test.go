package cli

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// The add/reinstall path (setupAgentHookSet) reports pruning the retired Claude
// Code post-todo hook instead of rewriting the config silently. Uses t.Chdir —
// do NOT add t.Parallel().
func TestSetupAgentHookSet_ReportsPrunedStaleHooks(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()

	todo := agent.WrapProductionSilentHookCommand("entire hooks claude-code post-todo")
	testutil.WriteFile(t, dir, ".claude/settings.json", fmt.Sprintf(`{
  "hooks": {
    "PostToolUse": [
      {"matcher": "TaskCreate|TaskUpdate", "hooks": [{"type": "command", "command": %q}]}
    ]
  }
}`, todo))

	claude, err := agent.Get(agent.AgentNameClaudeCode)
	require.NoError(t, err)

	var out bytes.Buffer
	added, errs := setupAgentHookSet(context.Background(), &out, []agent.Agent{claude}, false)
	require.Empty(t, errs)
	require.Len(t, added, 1)
	require.Contains(t, out.String(), "Removed outdated Entire hooks for")

	// A second install has nothing left to prune and says nothing about it.
	out.Reset()
	_, errs = setupAgentHookSet(context.Background(), &out, []agent.Agent{claude}, false)
	require.Empty(t, errs)
	require.NotContains(t, out.String(), "Removed outdated")
}
