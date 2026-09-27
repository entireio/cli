package cli

import (
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/testutil/gitenv"
	"github.com/stretchr/testify/require"
)

func TestLocalReads_ReftableProcessCount(t *testing.T) {
	for _, linked := range []bool{false, true} {
		name := "main"
		if linked {
			name = "linked"
		}
		t.Run(name, func(t *testing.T) {
			gitenv.IsolateRepository(t)
			root := t.TempDir()
			testutil.InitRepo(t, root)
			testutil.RunGit(t, root, "commit", "--allow-empty", "--no-gpg-sign", "-m", "no checkpoint trailer")
			testutil.RunGit(t, root, "tag", "--no-sign", "-a", "outer", "-m", "tag")
			testutil.RunGit(t, root, "tag", "--no-sign", "-a", "nested", "outer", "-m", "nested")
			testutil.RunGit(t, root, "update-ref", "refs/remotes/origin/"+paths.MetadataBranchName, "refs/tags/nested")
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
			t.Run("HEAD", func(t *testing.T) {
				commands := gitenv.TraceCommands(t)
				review, investigation, info := headCheckpointFlags(t.Context())
				require.False(t, review)
				require.False(t, investigation)
				require.Empty(t, info)
				calls := commands()
				require.Len(t, calls, 1, "%v", calls)
				require.Contains(t, calls[0], "log")
			})
			t.Run("metadata", func(t *testing.T) {
				commands := gitenv.TraceCommands(t)
				require.True(t, metadataTrackingRefExists(t.Context(), "origin"))
				calls := commands()
				require.Len(t, calls, 1, "%v", calls)
				require.Contains(t, calls[0], "rev-parse")
			})
		})
	}
}
