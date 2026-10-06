package strategy

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func localBranches(t *testing.T, dir string) []string {
	t.Helper()
	out := strings.TrimSpace(testutil.RunGit(t, dir, "for-each-ref", "--format=%(refname:short)", "refs/heads/"))
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// CleanupLegacyShadowBranches runs once per repository: it deletes the strict
// worktree-suffixed shadow branches older CLIs wrote, never the bare
// "entire/<hex>" form (a human short-SHA branch looks identical) or Entire's
// own metadata branches, and records a marker so later session starts skip the
// ref scan. Uses t.Chdir — do NOT add t.Parallel().
func TestCleanupLegacyShadowBranches_DeletesSuffixedBranchesOnce(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	for _, branch := range []string{"entire/1234567-abcdef", "entire/89abcde0-123456", "entire/fedcba9", paths.MetadataBranchName, "feature"} {
		testutil.RunGit(t, dir, "branch", branch)
	}

	deleted, err := CleanupLegacyShadowBranches(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, deleted)

	branches := localBranches(t, dir)
	assert.NotContains(t, branches, "entire/1234567-abcdef")
	assert.NotContains(t, branches, "entire/89abcde0-123456")
	assert.Contains(t, branches, "entire/fedcba9", "the bare legacy form is left for `entire clean --all` to confirm")
	assert.Contains(t, branches, paths.MetadataBranchName)
	assert.Contains(t, branches, "feature")

	assert.FileExists(t, filepath.Join(dir, ".git", legacyShadowCleanupMarker))

	// The marker makes the pass one-time: a branch appearing later is left
	// alone (`entire clean` still removes it).
	testutil.RunGit(t, dir, "branch", "entire/7654321-fedcba")
	deleted, err = CleanupLegacyShadowBranches(context.Background())
	require.NoError(t, err)
	assert.Zero(t, deleted)
	assert.Contains(t, localBranches(t, dir), "entire/7654321-fedcba")
}

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
