package strategy

import (
	"context"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// `entire clean` and `entire doctor` act on the strict worktree-suffixed shape
// only; `entire clean --all` also lists the bare entire/<hex> form, which a
// human short-SHA branch can look like. Uses t.Chdir — do NOT add
// t.Parallel().
func TestListRemovableLegacyShadowBranches(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	for _, branch := range []string{"entire/1234567-abcdef", "entire/fedcba9", paths.MetadataBranchName} {
		testutil.RunGit(t, dir, "branch", branch)
	}

	removable, err := ListRemovableLegacyShadowBranches(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"entire/1234567-abcdef"}, removable)

	all, err := ListLegacyShadowBranches(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"entire/1234567-abcdef", "entire/fedcba9"}, all)
}
