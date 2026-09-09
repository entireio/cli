package remote

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

const (
	deleteRefsTestA = "refs/entire/checkpoints/f6/a1b2c3d4e5f6"
	deleteRefsTestB = "refs/entire/checkpoints/a1/b2c3d4e5f6a1"
)

// setupDeleteRefsRepos returns a work repo and a bare remote holding two
// checkpoint refs plus a "feature" branch.
func setupDeleteRefsRepos(t *testing.T) (workDir, bareDir string) {
	t.Helper()
	workDir = t.TempDir()
	testutil.InitRepo(t, workDir)
	testutil.WriteFile(t, workDir, "f.txt", "init")
	testutil.GitAdd(t, workDir, "f.txt")
	testutil.GitCommit(t, workDir, "init")

	bareDir = t.TempDir()
	testutil.RunGit(t, bareDir, "init", "--bare")
	testutil.RunGit(t, workDir, "push", "-q", bareDir,
		"HEAD:"+deleteRefsTestA, "HEAD:"+deleteRefsTestB, "HEAD:refs/heads/feature")
	return workDir, bareDir
}

func bareRefs(t *testing.T, bareDir string) string {
	t.Helper()
	return testutil.RunGit(t, bareDir, "for-each-ref", "--format=%(refname)")
}

func TestDeleteRefs_DeletesFullAndShortNames(t *testing.T) {
	t.Parallel()
	workDir, bareDir := setupDeleteRefsRepos(t)

	absent, err := DeleteRefs(context.Background(), bareDir, workDir, []string{deleteRefsTestA, "feature"})
	require.NoError(t, err)
	assert.Empty(t, absent)

	refs := bareRefs(t, bareDir)
	assert.NotContains(t, refs, deleteRefsTestA)
	assert.NotContains(t, refs, "refs/heads/feature")
	assert.Contains(t, refs, deleteRefsTestB, "refs not named are untouched")
}

func TestDeleteRefs_AbsentRefReportedNotFailed(t *testing.T) {
	t.Parallel()
	workDir, bareDir := setupDeleteRefsRepos(t)
	testutil.RunGit(t, bareDir, "update-ref", "-d", deleteRefsTestA)

	absent, err := DeleteRefs(context.Background(), bareDir, workDir, []string{deleteRefsTestA, deleteRefsTestB})
	require.NoError(t, err, "one absent ref must not fail the batch")
	assert.Equal(t, []string{deleteRefsTestA}, absent)
	assert.NotContains(t, bareRefs(t, bareDir), deleteRefsTestB, "the present ref is still deleted on the retry")

	// Everything absent: no retry push, all reported.
	absent, err = DeleteRefs(context.Background(), bareDir, workDir, []string{deleteRefsTestA, deleteRefsTestB, "feature-gone"})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{deleteRefsTestA, deleteRefsTestB, "feature-gone"}, absent)
}

func TestDeleteRefs_EmptyIsNoop(t *testing.T) {
	t.Parallel()
	absent, err := DeleteRefs(context.Background(), "unused", "", nil)
	require.NoError(t, err)
	assert.Nil(t, absent)
}

func TestDeleteRefs_UnreachableTargetErrors(t *testing.T) {
	t.Parallel()
	workDir, _ := setupDeleteRefsRepos(t)
	_, err := DeleteRefs(context.Background(), t.TempDir()+"/does-not-exist.git", workDir, []string{deleteRefsTestA})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "before deleting refs", "the pre-delete listing is what fails")
}
