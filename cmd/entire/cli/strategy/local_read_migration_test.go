package strategy

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/testutil/gitenv"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"
)

func TestLocalRefReads_NoGitAfterResolution(t *testing.T) {
	gitenv.IsolateRepository(t)
	root, _, head := initCountTestRepo(t)
	t.Chdir(root)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)
	testutil.RunGit(t, root, "update-ref", "refs/heads/shadow", head)
	testutil.RunGit(t, root, "pack-refs", "--all")
	repo, err := OpenRepository(t.Context())
	require.NoError(t, err)
	defer repo.Close()
	t.Setenv("PATH", t.TempDir())
	require.NoError(t, branchExists(t.Context(), repo, "shadow"))
	require.Error(t, branchExists(t.Context(), repo, "absent"))
}

func TestBranchExists_SameHandleObservesPackedDeletion(t *testing.T) {
	gitenv.IsolateRepository(t)
	root, _, head := initCountTestRepo(t)
	t.Chdir(root)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)
	testutil.RunGit(t, root, "update-ref", "refs/heads/shadow", head)
	testutil.RunGit(t, root, "pack-refs", "--all")
	repo, err := OpenRepository(t.Context())
	require.NoError(t, err)
	defer repo.Close()
	require.NoError(t, branchExists(t.Context(), repo, "shadow"))
	testutil.RunGit(t, root, "branch", "-D", "shadow")
	require.ErrorIs(t, branchExists(t.Context(), repo, "shadow"), plumbing.ErrReferenceNotFound)
}

func TestLocalRefReads_StoreOverrideAndBare(t *testing.T) {
	gitenv.IsolateRepository(t)
	root, _, head := initCountTestRepo(t)
	other := t.TempDir()
	testutil.InitRepo(t, other)
	repo, err := gitrepo.OpenPath(other)
	require.NoError(t, err)
	defer repo.Close()
	testutil.RunGit(t, root, "update-ref", "refs/heads/shadow", head)
	testutil.RunGit(t, root, "update-ref", "refs/remotes/origin/topic", head)
	bare := filepath.Join(t.TempDir(), "bare.git")
	testutil.RunGit(t, root, "clone", "--mirror", root, bare)
	t.Chdir(bare)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)
	require.NoError(t, branchExists(t.Context(), repo, "shadow"))
	require.True(t, remoteHasTrackingRefs(t.Context(), "origin"))
	t.Chdir(other)
	paths.ClearWorktreeRootCache()
	require.Error(t, branchExists(t.Context(), repo, "shadow"))
	require.False(t, remoteHasTrackingRefs(t.Context(), "origin"))
	// Keep the warmed root cache while changing the native repository selector.
	t.Setenv("GIT_DIR", filepath.Join(root, ".git"))
	t.Setenv("GIT_WORK_TREE", root)
	require.NoError(t, branchExists(t.Context(), repo, "shadow"))
	require.True(t, remoteHasTrackingRefs(t.Context(), "origin"))
}

// Git exports GIT_DIR to hooks in linked worktrees. When it names the
// discovered Git directory, the read stays on go-git and spawns no Git.
func TestLocalRefReads_LinkedWorktreeHookEnvironment(t *testing.T) {
	gitenv.IsolateRepository(t)
	root, _, head := initCountTestRepo(t)
	testutil.RunGit(t, root, "update-ref", "refs/heads/shadow", head)
	linked := filepath.Join(t.TempDir(), "linked")
	testutil.RunGit(t, root, "worktree", "add", "--detach", linked)
	gitDir := strings.TrimSpace(testutil.RunGit(t, linked, "rev-parse", "--absolute-git-dir"))
	t.Chdir(linked)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)
	t.Setenv("GIT_DIR", gitDir)
	repo, err := OpenRepository(t.Context())
	require.NoError(t, err)
	defer repo.Close()
	t.Setenv("PATH", t.TempDir())
	require.NoError(t, branchExists(t.Context(), repo, "shadow"))
	require.ErrorIs(t, branchExists(t.Context(), repo, "absent"), plumbing.ErrReferenceNotFound)
}
