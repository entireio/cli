package strategy

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/go-git/go-git/v6/plumbing"
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

// `git update-ref -d` does not refuse a checked-out branch, so the automatic
// pass must skip one itself: deleting it would leave that worktree's HEAD
// pointing at a missing ref. A checked-out branch is user-held, so skipping it
// still lets the one-time marker be written, while the other legacy branches
// are deleted. Uses t.Chdir — do NOT add t.Parallel().
func TestCleanupLegacyShadowBranches_SkipsCheckedOutBranches(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	const (
		inMain        = "entire/1234567-abcdef"
		inLinked      = "entire/89abcde0-123456"
		notCheckedOut = "entire/7654321-fedcba"
	)
	for _, branch := range []string{inMain, inLinked, notCheckedOut} {
		testutil.RunGit(t, dir, "branch", branch)
	}
	linked := filepath.Join(t.TempDir(), "linked")
	testutil.RunGit(t, dir, "worktree", "add", linked, inLinked)
	testutil.RunGit(t, dir, "checkout", inMain)

	deleted, err := CleanupLegacyShadowBranches(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, deleted)

	branches := localBranches(t, dir)
	assert.Contains(t, branches, inMain, "the branch checked out in the main worktree survives")
	assert.Contains(t, branches, inLinked, "the branch checked out in a linked worktree survives")
	assert.NotContains(t, branches, notCheckedOut)

	assert.Equal(t, inMain, strings.TrimSpace(testutil.RunGit(t, dir, "rev-parse", "--abbrev-ref", "HEAD")))
	assert.NotEmpty(t, strings.TrimSpace(testutil.RunGit(t, dir, "rev-parse", "--verify", "HEAD")), "main worktree HEAD still resolves")
	assert.NotEmpty(t, strings.TrimSpace(testutil.RunGit(t, linked, "rev-parse", "--verify", "HEAD")), "linked worktree HEAD still resolves")

	assert.FileExists(t, filepath.Join(dir, ".git", legacyShadowCleanupMarker),
		"a user-held branch is left alone, not a failure that blocks the marker")
}

// The compare-and-delete leaves a branch that moved after it was listed, and
// the pass then records no marker so the next session start retries it.
// Uses t.Chdir — do NOT add t.Parallel().
func TestDeleteLegacyShadowBranchesIfUnchanged_PreservesMovedBranch(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	const branch = "entire/1234567-abcdef"
	testutil.RunGit(t, dir, "branch", branch)
	listed := plumbing.NewHash(strings.TrimSpace(testutil.RunGit(t, dir, "rev-parse", branch)))

	testutil.WriteFile(t, dir, "moved.txt", "moved")
	testutil.GitAdd(t, dir, "moved.txt")
	testutil.GitCommit(t, dir, "move the branch")
	moved := strings.TrimSpace(testutil.RunGit(t, dir, "rev-parse", "HEAD"))
	testutil.RunGit(t, dir, "branch", "-f", branch, moved)

	deleted, failed := deleteLegacyShadowBranchesIfUnchanged(context.Background(), map[string]plumbing.Hash{branch: listed})
	assert.Empty(t, deleted)
	assert.Equal(t, []string{branch}, failed)
	assert.Equal(t, moved, strings.TrimSpace(testutil.RunGit(t, dir, "rev-parse", branch)), "the moved branch keeps its new tip")
}
