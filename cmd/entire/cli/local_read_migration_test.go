package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/testutil/gitenv"
	"github.com/stretchr/testify/require"
)

func TestLocalReads_NoGitAfterResolution(t *testing.T) {
	gitenv.IsolateRepository(t)
	root := t.TempDir()
	testutil.InitRepo(t, root)
	t.Chdir(root)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)
	testutil.WriteFile(t, root, "tracked", "committed\n")
	testutil.RunGit(t, root, "add", "tracked")
	testutil.RunGit(t, root, "commit", "--no-gpg-sign", "-m", "initial")
	testutil.WriteFile(t, root, "tracked", "uncommitted\n")
	testutil.RunGit(t, root, "update-ref", "refs/remotes/origin/"+paths.MetadataBranchName, "HEAD")
	_, err := paths.WorktreeRoot(t.Context())
	require.NoError(t, err)
	repo, err := gitrepo.OpenPath(root)
	require.NoError(t, err)
	defer repo.Close()
	indexPath := filepath.Join(root, ".git", "index")
	indexBefore, err := os.ReadFile(indexPath)
	require.NoError(t, err)
	t.Setenv("PATH", t.TempDir())
	message, err := headCommitMessage(t.Context(), repo, root)
	require.NoError(t, err)
	require.Equal(t, "initial\n", message)
	require.True(t, metadataTrackingRefExists(t.Context(), "origin"))
	require.False(t, metadataTrackingRefExists(t.Context(), "missing"))
	indexAfter, err := os.ReadFile(indexPath)
	require.NoError(t, err)
	require.Equal(t, indexBefore, indexAfter)
	content, err := os.ReadFile(filepath.Join(root, "tracked"))
	require.NoError(t, err)
	require.Equal(t, "uncommitted\n", string(content))
}

func TestHeadCommitMessage_NativeCompatibility(t *testing.T) {
	for _, mode := range []string{"replace ref", "store override"} {
		t.Run(mode, func(t *testing.T) {
			gitenv.IsolateRepository(t)
			root := t.TempDir()
			testutil.InitRepo(t, root)
			t.Chdir(root)
			testutil.RunGit(t, root, "commit", "--allow-empty", "--no-gpg-sign", "-m", "original")
			head := strings.TrimSpace(testutil.RunGit(t, root, "rev-parse", "HEAD"))
			repo, err := gitrepo.OpenPath(root)
			require.NoError(t, err)
			defer repo.Close()
			if mode == "replace ref" {
				tree := strings.TrimSpace(testutil.RunGit(t, root, "rev-parse", "HEAD^{tree}"))
				replacement := strings.TrimSpace(testutil.RunGit(t, root, "-c", "commit.gpgsign=false", "commit-tree", tree, "-m", "replacement"))
				testutil.RunGit(t, root, "replace", head, replacement)
			} else {
				other := t.TempDir()
				testutil.InitRepo(t, other)
				testutil.RunGit(t, other, "commit", "--allow-empty", "--no-gpg-sign", "-m", "selected store")
				t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
				t.Setenv("GIT_WORK_TREE", other)
			}
			want := testutil.RunGit(t, root, "log", "-1", "--format=%B")
			message, err := headCommitMessage(t.Context(), repo, root)
			require.NoError(t, err)
			require.Equal(t, want, message, "retain native replacement/selector semantics")
		})
	}
}

func TestMetadataTrackingRefExists_StoreOverride(t *testing.T) {
	gitenv.IsolateRepository(t)
	root, other := t.TempDir(), t.TempDir()
	testutil.InitRepo(t, root)
	testutil.InitRepo(t, other)
	testutil.RunGit(t, other, "commit", "--allow-empty", "--no-gpg-sign", "-m", "selected")
	testutil.RunGit(t, other, "update-ref", "refs/remotes/origin/"+paths.MetadataBranchName, "HEAD")
	t.Chdir(root)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)
	require.False(t, metadataTrackingRefExists(t.Context(), "origin"))
	// Even a warmed CWD root cache must not hide an explicitly selected store.
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)
	require.True(t, metadataTrackingRefExists(t.Context(), "origin"))
}

func TestHeadCommitMessage_MissingObjectIsNotSuccess(t *testing.T) {
	gitenv.IsolateRepository(t)
	root := t.TempDir()
	testutil.InitRepo(t, root)
	t.Chdir(root)
	testutil.RunGit(t, root, "commit", "--allow-empty", "--no-gpg-sign", "-m", "initial")
	head := strings.TrimSpace(testutil.RunGit(t, root, "rev-parse", "HEAD"))
	require.NoError(t, os.Remove(filepath.Join(root, ".git", "objects", head[:2], head[2:])))
	repo, err := gitrepo.OpenPath(root)
	require.NoError(t, err)
	defer repo.Close()
	message, err := headCommitMessage(t.Context(), repo, root)
	require.Error(t, err)
	require.Empty(t, message)
}
