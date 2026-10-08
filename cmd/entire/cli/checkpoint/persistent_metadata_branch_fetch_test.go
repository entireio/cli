package checkpoint

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/vercelconfig"
)

// dropV1Branch removes the local v1 ref after capturing its hash, modelling a
// fresh clone: the checkpoints exist on the checkpoint remote, but no local ref
// points at them and origin never carried the branch either.
func dropV1Branch(t *testing.T, repo *git.Repository) plumbing.Hash {
	t.Helper()
	primary := DefaultV1Refs().Primary
	ref, err := repo.Reference(primary, true)
	require.NoError(t, err)
	hash := ref.Hash()
	require.NoError(t, repo.Storer.RemoveReference(primary))
	return hash
}

// TestGetSessionsBranchTree_FetchesMetadataBranchWhenMissing pins the recovery
// tier that makes a dedicated checkpoint_remote readable from a fresh clone.
//
// The git-branch store previously resolved reads from the local ref, then from
// origin's remote-tracking ref, and gave up. A repo whose checkpoints live on a
// separate checkpoint_remote has neither — origin never received the branch — so
// every committed checkpoint read as "not found" with no path to recovery.
func TestGetSessionsBranchTree_FetchesMetadataBranchWhenMissing(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	cid := id.MustCheckpointID("a1b2c3d4e5f6")
	seedBranchCheckpoint(t, store, cid, "s1")

	hash := dropV1Branch(t, repo)

	// Without a fetcher the read still fails, exactly as before.
	_, err := store.getSessionsBranchTree(t.Context())
	require.Error(t, err, "a missing branch with no fetcher must still fail")

	calls := 0
	store.SetMetadataBranchFetcher(func(_ context.Context) error {
		calls++
		return repo.Storer.SetReference(plumbing.NewHashReference(DefaultV1Refs().Primary, hash))
	})

	tree, err := store.getSessionsBranchTree(t.Context())
	require.NoError(t, err, "the fetcher should have made the branch readable")
	assert.Equal(t, 1, calls, "fetcher should be called exactly once")

	// The recovered tree is the real one: it carries the seeded checkpoint.
	_, err = tree.Tree(cid.Path())
	require.NoError(t, err, "recovered tree should contain the seeded checkpoint")
}

// TestGetSessionsBranchTree_SkipsFetchWhenBranchPresent pins that the fetcher is
// a recovery tier, not a refresh: a branch that resolves locally must never
// trigger a network call. Reads run on hot paths where an unconditional fetch
// would be a per-read stall.
func TestGetSessionsBranchTree_SkipsFetchWhenBranchPresent(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	seedBranchCheckpoint(t, store, id.MustCheckpointID("a1b2c3d4e5f6"), "s1")

	store.SetMetadataBranchFetcher(func(_ context.Context) error {
		t.Error("fetcher must not run when the branch resolves locally")
		return nil
	})

	_, err := store.getSessionsBranchTree(t.Context())
	require.NoError(t, err)
}

// TestGetSessionsBranchTree_FetchesAtMostOncePerStore pins the recovery latch.
// A single command re-enters getSessionsBranchTree several times — List, then
// getFetchingTree for each read — so a repo the fetch cannot recover (remote has
// no v1, unreachable, or refusing auth) would otherwise re-pay the whole fetch
// budget on every entry, multiplying one command's worst case by the number of
// reads it performs.
func TestGetSessionsBranchTree_FetchesAtMostOncePerStore(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	seedBranchCheckpoint(t, store, id.MustCheckpointID("a1b2c3d4e5f6"), "s1")
	dropV1Branch(t, repo)

	calls := 0
	store.SetMetadataBranchFetcher(func(_ context.Context) error {
		calls++
		return errors.New("remote unreachable")
	})

	for range 3 {
		_, err := store.getSessionsBranchTree(t.Context())
		require.Error(t, err)
	}
	assert.Equal(t, 1, calls, "an unrecoverable branch must not re-fetch on every read")
}

// TestGetSessionsBranchTree_FetchFailureKeepsOriginalError pins that a failing
// fetch degrades to the pre-existing "not found" error rather than replacing it
// with a transport error. Callers such as List treat not-found as an empty
// result, so surfacing the fetch failure here would turn an offline read into a
// hard error.
func TestGetSessionsBranchTree_FetchFailureKeepsOriginalError(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	seedBranchCheckpoint(t, store, id.MustCheckpointID("a1b2c3d4e5f6"), "s1")
	dropV1Branch(t, repo)

	_, wantErr := store.getSessionsBranchTree(t.Context())
	require.Error(t, wantErr)

	store.SetMetadataBranchFetcher(func(_ context.Context) error {
		return errors.New("no checkpoint_remote configured")
	})

	_, err := store.getSessionsBranchTree(t.Context())
	require.Error(t, err)
	assert.Equal(t, wantErr.Error(), err.Error(),
		"a failed fetch should leave the original not-found error intact")
}

// TestGetSessionsBranchTree_RecoversFromDataFreeOrphan pins that a branch which
// resolves but carries no checkpoint data triggers recovery just like a missing
// one.
//
// This is the case a ref-missing trigger cannot see. A repo left holding an
// un-initialized v1 orphan — created before the primary was switched to
// git-refs, where enable no longer heals it — resolves the ref cleanly, so
// without this the miss is masked and the real checkpoints on the checkpoint
// remote stay permanently unreachable.
func TestGetSessionsBranchTree_RecoversFromDataFreeOrphan(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	cid := id.MustCheckpointID("a1b2c3d4e5f6")
	seedBranchCheckpoint(t, store, cid, "s1")
	realHash := dropV1Branch(t, repo)

	// Stand up an orphan carrying nothing but init artifacts, the shape enable
	// used to leave behind, and point v1 at it.
	orphan := emptyOrphanCommit(t, repo)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(DefaultV1Refs().Primary, orphan)))

	// It resolves, so the ref-missing trigger stays silent...
	tree, err := store.getSessionsBranchTree(t.Context())
	require.NoError(t, err, "a data-free orphan still resolves")
	assert.False(t, treeHasCheckpointData(tree), "precondition: the orphan carries no checkpoint data")

	// ...but with a fetcher wired, the data-free branch is treated as a miss.
	store.metadataBranchFetchTried = false
	calls := 0
	store.SetMetadataBranchFetcher(func(_ context.Context) error {
		calls++
		return repo.Storer.SetReference(plumbing.NewHashReference(DefaultV1Refs().Primary, realHash))
	})

	recovered, err := store.getSessionsBranchTree(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, calls, "a data-free branch should trigger recovery")
	_, err = recovered.Tree(cid.Path())
	require.NoError(t, err, "recovered tree should carry the real checkpoint")
}

// emptyOrphanCommit creates a commit whose tree holds only the vercel.json init
// artifact — the "un-initialized orphan" shape strategy.metadataBranchHasData
// treats as data-free.
func emptyOrphanCommit(t *testing.T, repo *git.Repository) plumbing.Hash {
	t.Helper()
	blob := repo.Storer.NewEncodedObject()
	blob.SetType(plumbing.BlobObject)
	w, err := blob.Writer()
	require.NoError(t, err)
	_, err = w.Write([]byte("{}\n"))
	require.NoError(t, err)
	require.NoError(t, w.Close())
	blobHash, err := repo.Storer.SetEncodedObject(blob)
	require.NoError(t, err)

	tree := &object.Tree{Entries: []object.TreeEntry{
		{Name: vercelconfig.FileName, Mode: filemode.Regular, Hash: blobHash},
	}}
	treeObj := repo.Storer.NewEncodedObject()
	require.NoError(t, tree.Encode(treeObj))
	treeHash, err := repo.Storer.SetEncodedObject(treeObj)
	require.NoError(t, err)

	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	commit := &object.Commit{
		Message:   "init orphan",
		TreeHash:  treeHash,
		Author:    object.Signature{Name: "t", Email: "t@example.com", When: when},
		Committer: object.Signature{Name: "t", Email: "t@example.com", When: when},
	}
	commitObj := repo.Storer.NewEncodedObject()
	require.NoError(t, commit.Encode(commitObj))
	commitHash, err := repo.Storer.SetEncodedObject(commitObj)
	require.NoError(t, err)
	return commitHash
}
