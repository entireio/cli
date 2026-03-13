package strategy

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHasUnpushedSessionsCommon(t *testing.T) {
	t.Parallel()

	branchName := "entire/checkpoints/v1"

	setupRepo := func(t *testing.T) (*git.Repository, plumbing.Hash) {
		t.Helper()
		tmpDir := t.TempDir()
		testutil.InitRepo(t, tmpDir)
		testutil.WriteFile(t, tmpDir, "f.txt", "init")
		testutil.GitAdd(t, tmpDir, "f.txt")
		testutil.GitCommit(t, tmpDir, "init")

		repo, err := git.PlainOpen(tmpDir)
		require.NoError(t, err)

		head, err := repo.Head()
		require.NoError(t, err)

		localRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName(branchName), head.Hash())
		require.NoError(t, repo.Storer.SetReference(localRef))

		return repo, head.Hash()
	}

	t.Run("no remote tracking ref exists", func(t *testing.T) {
		t.Parallel()
		repo, headHash := setupRepo(t)
		assert.True(t, hasUnpushedSessionsCommon(repo, "origin", headHash, branchName))
	})

	t.Run("local and remote same hash", func(t *testing.T) {
		t.Parallel()
		repo, headHash := setupRepo(t)

		remoteRef := plumbing.NewHashReference(
			plumbing.NewRemoteReferenceName("origin", branchName),
			headHash,
		)
		require.NoError(t, repo.Storer.SetReference(remoteRef))

		assert.False(t, hasUnpushedSessionsCommon(repo, "origin", headHash, branchName))
	})

	t.Run("local differs from remote", func(t *testing.T) {
		t.Parallel()
		repo, _ := setupRepo(t)

		differentHash := plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
		assert.True(t, hasUnpushedSessionsCommon(repo, "origin", differentHash, branchName))
	})
}
