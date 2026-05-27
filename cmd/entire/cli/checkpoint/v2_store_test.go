package checkpoint

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/stretchr/testify/require"

	"github.com/go-git/go-git/v6/plumbing"
)

func TestNewV2GitStore(t *testing.T) {
	t.Parallel()

	repo := initTestRepo(t)
	store := NewV2GitStore(repo)

	require.NotNil(t, store)
	require.Equal(t, repo, store.repo)
	require.NotNil(t, store.gs)
}

func TestV2GitStore_GetRefState_ReturnsParentAndTree(t *testing.T) {
	t.Parallel()

	repo := initTestRepo(t)
	store := NewV2GitStore(repo)

	writeV2TestCheckpoint(t, repo, v2TestCheckpointOptions{
		CheckpointID: id.MustCheckpointID("a1a2a3a4a5a6"),
		SessionID:    "session-1",
		Strategy:     "manual-commit",
	})

	parentHash, treeHash, err := store.GetRefState(plumbing.ReferenceName(paths.V2MainRefName))
	require.NoError(t, err)
	require.False(t, parentHash.IsZero())
	require.False(t, treeHash.IsZero())

	commit, err := repo.CommitObject(parentHash)
	require.NoError(t, err)
	require.Equal(t, commit.TreeHash, treeHash)
}

func TestV2GitStore_GetRefState_ErrorsOnMissingRef(t *testing.T) {
	t.Parallel()

	repo := initTestRepo(t)
	store := NewV2GitStore(repo)

	_, _, err := store.GetRefState(plumbing.ReferenceName(paths.V2MainRefName))
	require.Error(t, err)
	require.Contains(t, err.Error(), "ref refs/entire/checkpoints/v2/main not found")
}
