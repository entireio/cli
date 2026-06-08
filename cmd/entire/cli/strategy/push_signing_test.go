package strategy

import (
	"context"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildCherryPickCommit_ReturnsUnsignedCommit(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "init")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	head, err := repo.Head()
	require.NoError(t, err)
	headCommit, err := repo.CommitObject(head.Hash())
	require.NoError(t, err)

	built, err := buildCherryPickCommit(context.Background(), repo, headCommit.TreeHash, head.Hash(), headCommit)
	require.NoError(t, err)
	assert.Equal(t, headCommit.Message, built.Message)
	assert.Equal(t, headCommit.TreeHash, built.TreeHash)
	assert.Empty(t, built.Signature, "build must not sign")
	assert.Equal(t, []plumbing.Hash{head.Hash()}, built.ParentHashes)
}

func TestPersistCherryPickCommit_StoresAndReturnsHash(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "init")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	head, err := repo.Head()
	require.NoError(t, err)
	headCommit, err := repo.CommitObject(head.Hash())
	require.NoError(t, err)

	built, err := buildCherryPickCommit(context.Background(), repo, headCommit.TreeHash, head.Hash(), headCommit)
	require.NoError(t, err)

	hash, err := persistCherryPickCommit(repo, built)
	require.NoError(t, err)
	assert.NotEqual(t, plumbing.ZeroHash, hash)

	stored, err := repo.CommitObject(hash)
	require.NoError(t, err)
	assert.Equal(t, headCommit.Message, stored.Message)
	assert.Equal(t, headCommit.TreeHash, stored.TreeHash)
}
