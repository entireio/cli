package checkpoint

import (
	"context"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/require"
)

// TestGitStoreListTasks_SurfacesStorageErrors: a metadata ref whose commit
// cannot be read is a storage failure, not an absent checkpoint. Reporting it
// as ErrCheckpointNotFound would tell the user the checkpoint does not exist.
func TestGitStoreListTasks_SurfacesStorageErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, _ := setupBranchTestRepo(t)
	refs := DefaultV1Refs()
	missingCommit := plumbing.NewHash("1111111111111111111111111111111111111111")
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(refs.Read, missingCommit)))
	store := NewGitStore(repo, refs)
	cid := id.MustCheckpointID("aabbccdd0404")

	_, err := store.ListTasks(ctx, cid)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrCheckpointNotFound)

	_, err = store.ReadTaskTranscript(ctx, cid, "toolu_any")
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrCheckpointNotFound)
}

// TestReadTaskTranscript_UnreadableTaskTreeIsNotAbsence: when tasks/<id> is
// listed but its tree object cannot be read, the record exists and is broken.
// Reporting ErrTaskNotFound would make the routing store treat the answer as
// final and skip a fallback backend that can read it.
func TestReadTaskTranscript_UnreadableTaskTreeIsNotAbsence(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t)
	tasksHash, err := storeTree(repo, []object.TreeEntry{{
		Name: "toolu_broken",
		Mode: filemode.Dir,
		Hash: plumbing.NewHash("2222222222222222222222222222222222222222"),
	}})
	require.NoError(t, err)
	rootHash, err := storeTree(repo, []object.TreeEntry{{Name: taskRecordsDirName, Mode: filemode.Dir, Hash: tasksHash}})
	require.NoError(t, err)
	root, err := object.GetTree(repo.Storer, rootHash)
	require.NoError(t, err)

	_, err = readTaskTranscriptFromCheckpointTree(NewFetchingTree(context.Background(), root, repo.Storer, nil), "toolu_broken")
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrTaskNotFound)

	// go-git reports a missing tree object as ErrDirectoryNotFound, so the
	// same applies one level up: an unreadable tasks/ tree is a failure, not
	// "this checkpoint has no subagents".
	brokenRootHash, err := storeTree(repo, []object.TreeEntry{{
		Name: taskRecordsDirName,
		Mode: filemode.Dir,
		Hash: plumbing.NewHash("3333333333333333333333333333333333333333"),
	}})
	require.NoError(t, err)
	brokenRoot, err := object.GetTree(repo.Storer, brokenRootHash)
	require.NoError(t, err)
	entries, err := listTasksFromCheckpointTree(NewFetchingTree(context.Background(), brokenRoot, repo.Storer, nil))
	require.Error(t, err, "got entries %v", entries)
}
