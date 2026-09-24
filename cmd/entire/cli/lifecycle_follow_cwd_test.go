package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// followAgentWorkingDirectory moves the hook process only into another
// worktree of the same repository. Not parallel: it changes the process CWD.
func TestFollowAgentWorkingDirectory(t *testing.T) {
	resolved := func(dir string) string {
		r, err := filepath.EvalSymlinks(dir)
		require.NoError(t, err)
		return r
	}
	parent := resolved(t.TempDir())
	testutil.InitRepo(t, parent)
	testutil.WriteFile(t, parent, "README.md", "base\n")
	testutil.GitAdd(t, parent, "README.md")
	testutil.GitCommit(t, parent, "init")
	worktree := filepath.Join(resolved(t.TempDir()), "feature")
	testutil.RunGit(t, parent, "worktree", "add", "-q", "-b", "feature", worktree)
	require.NoError(t, os.MkdirAll(filepath.Join(worktree, "src", "pkg"), 0o755))
	testutil.WriteFile(t, worktree, ".entire/settings.json", `{"enabled": true}`)
	plain := filepath.Join(resolved(t.TempDir()), "plain")
	testutil.RunGit(t, parent, "worktree", "add", "-q", "-b", "plain", plain)
	other := resolved(t.TempDir())
	testutil.InitRepo(t, other)
	testutil.WriteFile(t, other, "x", "x\n")
	testutil.GitAdd(t, other, "x")
	testutil.GitCommit(t, other, "x")

	ag, err := agent.Get(agent.AgentNameClaudeCode)
	require.NoError(t, err)

	cases := map[string]struct {
		eventType agent.EventType
		cwd       string
		want      string // process directory afterwards
		confirmed bool   // the payload named the tree the hook now runs in
	}{
		"another worktree of the repo":           {eventType: agent.TurnStart, cwd: worktree, want: worktree, confirmed: true},
		"a subdirectory of that worktree":        {eventType: agent.TurnStart, cwd: filepath.Join(worktree, "src", "pkg"), want: worktree, confirmed: true},
		"a subdirectory of the current tree":     {eventType: agent.TurnStart, cwd: filepath.Join(parent, ".git"), want: parent, confirmed: true},
		"subagent task worktree":                 {eventType: agent.SubagentStart, cwd: worktree, want: worktree},
		"subagent completion worktree":           {eventType: agent.SubagentEnd, cwd: worktree, want: worktree},
		"tool use worktree":                      {eventType: agent.ToolUse, cwd: worktree, want: worktree},
		"a worktree where entire is not enabled": {eventType: agent.TurnStart, cwd: plain, want: parent},
		"a different repository":                 {eventType: agent.TurnStart, cwd: other, want: parent},
		"a directory that does not exist":        {eventType: agent.TurnStart, cwd: filepath.Join(parent, "nope"), want: parent},
		"no cwd in the payload":                  {eventType: agent.TurnStart, cwd: "", want: parent},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Chdir(parent)
			paths.ClearWorktreeRootCache()
			launchLogger, err := newLogger(context.Background())
			require.NoError(t, err)
			defer launchLogger.Close()
			ctx := followAgentWorkingDirectory(logging.WithLogger(context.Background(), launchLogger), ag, &agent.Event{Type: tc.eventType, CWD: tc.cwd})
			require.Equal(t, tc.confirmed, strategy.AgentWorkingTreeConfirmed(ctx),
				"only a turn boundary naming the current tree lets a hook re-home a session")
			moved := tc.want != parent
			require.Equal(t, moved, logging.LoggerFromContext(ctx) != launchLogger, "the log sink follows only when the hook moved")
			if moved {
				logging.Info(ctx, "probe")
				require.FileExists(t, filepath.Join(tc.want, ".entire", logging.LogsName, logging.LogFileName),
					"after the move the hook logs in the worktree it works in")
			}
			got, err := os.Getwd()
			require.NoError(t, err)
			require.Equal(t, tc.want, resolved(got))
			root, err := paths.WorktreeRoot(context.Background())
			require.NoError(t, err)
			require.Equal(t, tc.want, resolved(root), "everything resolved from the process directory must follow")
		})
	}
}
