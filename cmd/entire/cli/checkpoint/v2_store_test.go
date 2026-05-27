package checkpoint

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/redact"
	"github.com/stretchr/testify/require"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/storage"
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

func TestV2GitStore_GetRefState_FallsBackToGitCLIWhenCommitObjectMissing(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "README.md", "init")
	testutil.GitAdd(t, dir, "README.md")
	testutil.GitCommit(t, dir, "initial")
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	writeV2TestCheckpoint(t, repo, v2TestCheckpointOptions{
		CheckpointID: id.MustCheckpointID("b1b2b3b4b5b6"),
		SessionID:    "session-fallback",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte("transcript\n")),
	})

	refName := plumbing.ReferenceName(paths.V2FullCurrentRefName)
	ref, err := repo.Reference(refName, true)
	require.NoError(t, err)
	commit, err := repo.CommitObject(ref.Hash())
	require.NoError(t, err)

	store := NewV2GitStore(&git.Repository{
		Storer: commitObjectMissingStorer{
			Storer:  repo.Storer,
			missing: ref.Hash(),
		},
	})
	parentHash, treeHash, err := store.GetRefState(refName)
	require.NoError(t, err)
	require.Equal(t, ref.Hash(), parentHash)
	require.Equal(t, commit.TreeHash, treeHash)
}

type commitObjectMissingStorer struct {
	storage.Storer

	missing plumbing.Hash
}

func (s commitObjectMissingStorer) EncodedObject(objectType plumbing.ObjectType, hash plumbing.Hash) (plumbing.EncodedObject, error) {
	if hash == s.missing && objectType == plumbing.CommitObject {
		return nil, plumbing.ErrObjectNotFound
	}
	return s.Storer.EncodedObject(objectType, hash)
}
