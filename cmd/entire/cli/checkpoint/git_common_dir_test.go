package checkpoint

import (
	"context"
	"testing"

	git "github.com/go-git/go-git/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// openTestRepo initializes a repo with one commit and opens it.
func openTestRepo(t *testing.T) *git.Repository {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "README.md", "# test")
	testutil.GitAdd(t, dir, "README.md")
	testutil.GitCommit(t, dir, "init")
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	return repo
}

// TestResolveGitCommonDir_CachesPerWorktree proves the memo is keyed by
// worktree root rather than shared globally: two repos must never resolve to
// each other's common dir. A single cached value (or a cwd-keyed one) would
// hand the second repo the first's .git, silently writing one repo's
// push-discovery queue into another's.
func TestResolveGitCommonDir_CachesPerWorktree(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	repoA, repoB := openTestRepo(t), openTestRepo(t)

	firstA, err := resolveGitCommonDir(ctx, repoA)
	require.NoError(t, err)
	firstB, err := resolveGitCommonDir(ctx, repoB)
	require.NoError(t, err)
	assert.NotEqual(t, firstA, firstB, "distinct repos must resolve to distinct common dirs")

	// Second resolution is served from the memo and must agree with the first.
	secondA, err := resolveGitCommonDir(ctx, repoA)
	require.NoError(t, err)
	assert.Equal(t, firstA, secondA)
	secondB, err := resolveGitCommonDir(ctx, repoB)
	require.NoError(t, err)
	assert.Equal(t, firstB, secondB)
}

// TestResolveGitCommonDir_CachedValueServesCanceledContext proves a cache hit
// needs no subprocess: once resolved, a canceled context still yields the
// common dir. This is what lets enqueueForPush record a ref during shutdown
// without paying (or failing) a `git rev-parse`.
func TestResolveGitCommonDir_CachedValueServesCanceledContext(t *testing.T) {
	t.Parallel()
	repo := openTestRepo(t)

	want, err := resolveGitCommonDir(context.Background(), repo)
	require.NoError(t, err)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := resolveGitCommonDir(canceled, repo)
	require.NoError(t, err, "a cached common dir must not need the subprocess")
	assert.Equal(t, want, got)
}

// TestResolveGitCommonDir_FailureIsNotCached proves a failed resolution leaves
// the memo empty, so a transient failure (e.g. a canceled context) cannot
// poison every later caller in the process.
func TestResolveGitCommonDir_FailureIsNotCached(t *testing.T) {
	t.Parallel()
	repo := openTestRepo(t)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := resolveGitCommonDir(canceled, repo)
	require.Error(t, err, "a canceled context should fail the resolving subprocess")

	got, err := resolveGitCommonDir(context.Background(), repo)
	require.NoError(t, err, "the earlier failure must not be cached")
	assert.NotEmpty(t, got)
}
