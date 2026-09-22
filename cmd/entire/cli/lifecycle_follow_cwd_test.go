package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
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
	other := resolved(t.TempDir())
	testutil.InitRepo(t, other)
	testutil.WriteFile(t, other, "x", "x\n")
	testutil.GitAdd(t, other, "x")
	testutil.GitCommit(t, other, "x")

	ag, err := agent.Get(agent.AgentNameClaudeCode)
	require.NoError(t, err)

	cases := map[string]struct {
		cwd  string
		want string // process directory afterwards
	}{
		"another worktree of the repo":       {cwd: worktree, want: worktree},
		"a subdirectory of that worktree":    {cwd: filepath.Join(worktree, "src", "pkg"), want: worktree},
		"a subdirectory of the current tree": {cwd: filepath.Join(parent, ".git"), want: parent},
		"a different repository":             {cwd: other, want: parent},
		"a directory that does not exist":    {cwd: filepath.Join(parent, "nope"), want: parent},
		"no cwd in the payload":              {cwd: "", want: parent},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Chdir(parent)
			paths.ClearWorktreeRootCache()
			followAgentWorkingDirectory(context.Background(), ag, &agent.Event{Type: agent.TurnStart, CWD: tc.cwd})
			got, err := os.Getwd()
			require.NoError(t, err)
			require.Equal(t, tc.want, resolved(got))
			root, err := paths.WorktreeRoot(context.Background())
			require.NoError(t, err)
			require.Equal(t, tc.want, resolved(root), "everything resolved from the process directory must follow")
		})
	}
}
