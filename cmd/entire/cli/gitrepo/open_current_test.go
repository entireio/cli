package gitrepo_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"
)

// hookLikeRepos builds the shape git produces when it runs a hook: two
// repositories, the process sitting in one while GIT_DIR and GIT_WORK_TREE name
// the other. It returns the repository git names, which is the one every
// assertion here is about; the directory the process sits in is only ever the
// wrong answer.
//
// No t.Parallel in this file: every test chdirs and sets environment variables.
func hookLikeRepos(t *testing.T) string {
	t.Helper()
	// Resolve the temp dir: on macOS t.TempDir() hands back /var/... while git
	// reports the /private/var/... it resolves to, so the two disagree on a
	// path that is the same directory.
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	hookRepo := filepath.Join(base, "hook-repo")
	cwdRepo := filepath.Join(base, "cwd-repo")
	for _, dir := range []string{hookRepo, cwdRepo} {
		require.NoError(t, os.MkdirAll(dir, 0o750))
		testutil.InitRepo(t, dir)
	}
	t.Chdir(cwdRepo)
	t.Setenv("GIT_DIR", filepath.Join(hookRepo, ".git"))
	t.Setenv("GIT_WORK_TREE", hookRepo)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)
	return hookRepo
}

// The bug this pins: OpenCurrent used to fall back to ".", which is a different
// repository whenever git and the current directory disagree. git exports
// GIT_DIR to the hooks it runs and paths.WorktreeRoot honours it; OpenPath(".")
// cannot see it. With git unavailable the old code opened the unrelated
// repository the process happened to be sitting in, and reported success.
func TestOpenCurrent_RefusesToGuessWhenTheWorktreeRootIsUnresolvable(t *testing.T) {
	hookRepo := hookLikeRepos(t)

	root, err := paths.WorktreeRoot(context.Background())
	require.NoError(t, err)
	require.Equal(t, hookRepo, root, "git must honour GIT_WORK_TREE here, or the test proves nothing")

	// The only way to make the worktree root unresolvable without touching the
	// repositories themselves.
	t.Setenv("PATH", t.TempDir())
	paths.ClearWorktreeRootCache()

	repo, err := gitrepo.OpenCurrent(context.Background())
	require.Error(t, err, "OpenCurrent must not substitute the current directory for the repository")
	require.Nil(t, repo)
}

// The one caller that wants the old behaviour still gets it, by name.
func TestOpenCurrentOrCwd_FallsBackForTheAdvisoryCaller(t *testing.T) {
	_ = hookLikeRepos(t)
	t.Setenv("PATH", t.TempDir())
	paths.ClearWorktreeRootCache()

	repo, err := gitrepo.OpenCurrentOrCwd(context.Background())
	require.NoError(t, err)
	require.NotNil(t, repo)
	require.NoError(t, repo.Close())
}

func TestOpenCurrent_ResolvesTheRepositoryGitNames(t *testing.T) {
	hookRepo := hookLikeRepos(t)

	repo, err := gitrepo.OpenCurrent(context.Background())
	require.NoError(t, err)
	defer repo.Close()

	// Prove it is the hook's repository, not the one we are sitting in, by
	// writing a ref through the handle and finding it on disk under hookRepo.
	require.NoError(t, repo.Storer.SetReference(testRef()))
	_, err = os.Stat(filepath.Join(hookRepo, ".git", "refs", "heads", "probe"))
	require.NoError(t, err, "OpenCurrent opened the wrong repository")
}

func testRef() *plumbing.Reference {
	return plumbing.NewHashReference("refs/heads/probe", plumbing.ZeroHash)
}
