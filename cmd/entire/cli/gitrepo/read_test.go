package gitrepo_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/testutil/gitenv"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/require"
)

func TestCommitAtReference_LocalObjects(t *testing.T) {
	gitenv.IsolateRepository(t)
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "file", "contents\n")
	testutil.RunGit(t, dir, "add", "file")
	testutil.RunGit(t, dir, "commit", "--no-gpg-sign", "-m", "message\n\nbody")
	head := strings.TrimSpace(testutil.RunGit(t, dir, "rev-parse", "HEAD"))
	blob := strings.TrimSpace(testutil.RunGit(t, dir, "rev-parse", "HEAD:file"))
	testutil.RunGit(t, dir, "tag", "--no-sign", "-a", "annotated", "-m", "tag")
	testutil.RunGit(t, dir, "tag", "--no-sign", "-a", "nested", "annotated", "-m", "nested")
	testutil.RunGit(t, dir, "update-ref", "refs/tags/blob", blob)
	testutil.RunGit(t, dir, "symbolic-ref", "refs/heads/alias", "HEAD")
	testutil.RunGit(t, dir, "pack-refs", "--all")
	linked := filepath.Join(t.TempDir(), "linked")
	shared := filepath.Join(t.TempDir(), "shared")
	testutil.RunGit(t, dir, "worktree", "add", "--detach", linked)
	testutil.RunGit(t, dir, "clone", "--shared", "--no-checkout", dir, shared)
	// All fixture writes are native Git. The reads must work without a Git
	// executable, including nested annotated tags and alternate object stores.
	t.Setenv("PATH", t.TempDir())
	for _, root := range []string{dir, linked, shared} {
		repo, err := gitrepo.OpenPath(root)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, repo.Close()) })
		for _, ref := range []plumbing.ReferenceName{plumbing.HEAD, "refs/tags/annotated", "refs/tags/nested"} {
			commit, err := gitrepo.CommitAtReference(t.Context(), repo, ref)
			require.NoError(t, err, "%s in %s", ref, root)
			require.Equal(t, head, commit.Hash.String())
			require.Equal(t, "message\n\nbody\n", commit.Message)
		}
		_, err = gitrepo.CommitAtReference(t.Context(), repo, "refs/tags/blob")
		require.ErrorIs(t, err, object.ErrUnsupportedObject)
		_, err = gitrepo.CommitAtReference(t.Context(), repo, "refs/heads/absent")
		require.ErrorIs(t, err, plumbing.ErrReferenceNotFound)
		_, err = gitrepo.CommitAtReference(t.Context(), repo, "HEAD~1")
		require.Error(t, err, "only exact refs are supported")
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err = gitrepo.CommitAtReference(ctx, repo, plumbing.HEAD)
		require.ErrorIs(t, err, context.Canceled)
	}
}

func TestReadsNeedNativeGit_Selectors(t *testing.T) {
	gitenv.IsolateRepository(t)
	require.False(t, gitrepo.ReadsNeedNativeGit())
	for _, key := range []string{
		"GIT_DIR", "GIT_COMMON_DIR", "GIT_WORK_TREE", "GIT_OBJECT_DIRECTORY",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE", "GIT_REPLACE_REF_BASE",
	} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, "explicit-selector")
			require.True(t, gitrepo.ReadsNeedNativeGit())
		})
	}
	t.Setenv("GIT_INDEX_FILE", "index-is-not-read")
	require.False(t, gitrepo.ReadsNeedNativeGit())
}
