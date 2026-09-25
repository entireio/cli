package strategy

import (
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/testutil/gitenv"
	"github.com/stretchr/testify/require"
)

func TestLocalRefReads_ReftableProcessCount(t *testing.T) {
	for _, linked := range []bool{false, true} {
		name := "main"
		if linked {
			name = "linked"
		}
		t.Run(name, func(t *testing.T) {
			gitenv.IsolateRepository(t)
			root, _, head := initCountTestRepo(t)
			testutil.RunGit(t, root, "update-ref", "refs/heads/shadow", head)
			testutil.RunGit(t, root, "update-ref", "refs/remotes/origin/topic", head)
			testutil.MigrateToReftable(t, root)
			if linked {
				dir := filepath.Join(t.TempDir(), "linked")
				testutil.RunGit(t, root, "worktree", "add", "--detach", dir)
				root = dir
			}
			t.Chdir(root)
			paths.ClearWorktreeRootCache()
			t.Cleanup(paths.ClearWorktreeRootCache)
			_, err := paths.WorktreeRoot(t.Context())
			require.NoError(t, err)
			t.Run("branch", func(t *testing.T) {
				commands := gitenv.TraceCommands(t)
				require.NoError(t, branchExistsFresh(t.Context(), "shadow"))
				calls := commands()
				require.Len(t, calls, 1, "%v", calls)
				require.Contains(t, calls[0], "show-ref")
			})
			t.Run("tracking", func(t *testing.T) {
				commands := gitenv.TraceCommands(t)
				require.True(t, remoteHasTrackingRefs(t.Context(), "origin"))
				calls := commands()
				require.Len(t, calls, 1, "%v", calls)
				require.Contains(t, calls[0], "for-each-ref")
			})
		})
	}
}
