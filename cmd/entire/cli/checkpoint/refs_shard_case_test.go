package checkpoint

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// A ULID whose shard ("6B") differs from a legacy hex shard only by case, and a
// legacy hex ID whose shard ("f6") does the same in the other direction. These
// are the two IDs the case-fold collision can produce; see FoldedRefName.
const (
	foldableULID   = id.CheckpointID("01M2DCHJCHTR9T9MZTSB7WV76B")
	foldableLegacy = id.CheckpointID("a1b2c3d4e5f6")
	// A legacy ID whose shard is all digits, so both formats spell its bucket
	// identically and nothing can diverge.
	unfoldableLegacy = id.CheckpointID("a1b2c3d4e512")
)

func TestFoldedRefName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cid     id.CheckpointID
		want    plumbing.ReferenceName
		wantOK  bool
		comment string
	}{
		{
			name:   "ulid shard folds down to the legacy hex spelling",
			cid:    foldableULID,
			want:   "refs/entire/checkpoints/6b/01M2DCHJCHTR9T9MZTSB7WV76B",
			wantOK: true,
		},
		{
			name:   "legacy hex shard folds up to the ulid spelling",
			cid:    foldableLegacy,
			want:   "refs/entire/checkpoints/F6/a1b2c3d4e5f6",
			wantOK: true,
		},
		{
			name:   "all-digit shard has no alternate spelling",
			cid:    unfoldableLegacy,
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := FoldedRefName(tt.cid)
			require.Equal(t, tt.wantOK, ok)
			if !tt.wantOK {
				return
			}
			assert.Equal(t, tt.want, got)
			canonical, err := RefName(tt.cid)
			require.NoError(t, err)
			assert.NotEqual(t, canonical, got, "the folded spelling must differ from the canonical one")
			assert.True(t, strings.EqualFold(canonical.String(), got.String()),
				"the folded spelling must differ from the canonical one only by case")
		})
	}
}

// repoDirOf recovers the worktree path of a store's repository, so a test can
// drive the git CLI against the same repo the store holds open.
func repoDirOf(t *testing.T, store *gitRefsStore) string {
	t.Helper()
	wt, err := store.repo.Worktree()
	require.NoError(t, err)
	return wt.Filesystem().Root()
}

// foldShardRefAndPack reproduces what a case-insensitive filesystem does to a
// checkpoint ref: the ref ends up stored under the OTHER format's spelling of
// its shard bucket. It then packs the refs, which is the step that actually
// breaks lookup — while the ref is loose, a case-insensitive filesystem
// resolves the canonical name onto the folded directory for us, so the bug is
// invisible on the very platforms that cause it. packed-refs is an exact string
// match on every platform, which is also what makes this test meaningful on
// macOS and Linux alike.
//
// It returns a freshly opened store, the way the next CLI invocation would see
// the repository.
func foldShardRefAndPack(t *testing.T, store *gitRefsStore, cid id.CheckpointID) (*gitRefsStore, plumbing.ReferenceName) {
	t.Helper()
	dir := repoDirOf(t, store)
	canonical := mustRefName(t, cid)
	folded, ok := FoldedRefName(cid)
	require.True(t, ok, "test fixture must use an ID whose shard has two spellings")

	ref, err := store.repo.Reference(canonical, true)
	require.NoError(t, err)
	require.NoError(t, store.repo.Storer.RemoveReference(canonical))
	// The bucket the canonical write created has to go before the folded ref is
	// planted: on a case-insensitive filesystem it would swallow the folded
	// write straight back into itself. The end state is planted rather than
	// provoked because provoking it needs such a filesystem, and this must
	// assert the same thing on Linux CI.
	require.NoError(t, os.RemoveAll(filepath.Join(dir, ".git", "refs", "entire", "checkpoints", cid.ShardFor())))
	require.NoError(t, store.repo.Storer.SetReference(plumbing.NewHashReference(folded, ref.Hash())))
	testutil.RunGit(t, dir, "pack-refs", "--all")

	// Reopened rather than reused: this is how the next CLI invocation sees the
	// repository, and it keeps the assertion off go-git's in-process ref cache.
	reopened, err := git.PlainOpen(dir)
	require.NoError(t, err)
	return newGitRefsStore(reopened), folded
}

func checkpointRefNames(t *testing.T, store *gitRefsStore) []string {
	t.Helper()
	out := testutil.RunGit(t, repoDirOf(t, store), "for-each-ref", CheckpointRefPrefix, "--format=%(refname)")
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line != "" {
			names = append(names, line)
		}
	}
	return names
}

// TestGitRefsStore_ReadsPackedFoldedShardRef is the regression for #2402's read
// half: an intact checkpoint whose ref is stored under the case-folded shard
// must still be readable. Before resolveLocalRef, Read reported it as absent.
func TestGitRefsStore_ReadsPackedFoldedShardRef(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newRefsStore(t)
	refsWrite(t, store, foldableULID, "sess-1", "transcript")

	folded, foldedName := foldShardRefAndPack(t, store, foldableULID)
	require.Equal(t, plumbing.ReferenceName("refs/entire/checkpoints/6b/01M2DCHJCHTR9T9MZTSB7WV76B"), foldedName)
	_, err := folded.repo.Reference(mustRefName(t, foldableULID), true)
	require.ErrorIs(t, err, plumbing.ErrReferenceNotFound,
		"precondition: the canonical spelling must no longer resolve, or the test proves nothing")

	summary, err := folded.Read(ctx, foldableULID)
	require.NoError(t, err)
	require.NotNil(t, summary, "a checkpoint stored under the folded shard must still read")
	assert.Equal(t, foldableULID, summary.CheckpointID)

	author, err := folded.GetCheckpointAuthor(ctx, foldableULID)
	require.NoError(t, err)
	assert.Equal(t, "Test Author", author.Name, "author lookup must follow the same fallback")

	infos, err := folded.List(ctx)
	require.NoError(t, err)
	require.Len(t, infos, 1)
	assert.Equal(t, foldableULID, infos[0].CheckpointID)
}

// TestGitRefsStore_WriteExtendsFoldedRefInsteadOfForking is the regression for
// the write half: with only a tolerant read, refBase finds the folded ref and
// hands back its tip while the update creates a SECOND ref at the canonical
// spelling, splitting one checkpoint across two refs.
func TestGitRefsStore_WriteExtendsFoldedRefInsteadOfForking(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newRefsStore(t)
	refsWrite(t, store, foldableULID, "sess-1", "transcript")

	folded, foldedName := foldShardRefAndPack(t, store, foldableULID)
	before, err := folded.repo.Reference(foldedName, true)
	require.NoError(t, err)

	refsWrite(t, folded, foldableULID, "sess-2", "more transcript")

	assert.Equal(t, []string{foldedName.String()}, checkpointRefNames(t, folded),
		"the second write must advance the existing ref, not create a canonical twin")

	after, err := folded.repo.Reference(foldedName, true)
	require.NoError(t, err)
	commit, err := folded.repo.CommitObject(after.Hash())
	require.NoError(t, err)
	require.Len(t, commit.ParentHashes, 1, "the write must extend the existing history, not start an orphan")
	assert.Equal(t, before.Hash(), commit.ParentHashes[0])

	summary, err := folded.Read(ctx, foldableULID)
	require.NoError(t, err)
	require.NotNil(t, summary)
	assert.Len(t, summary.Sessions, 2, "both sessions land on the one checkpoint")
}

// TestGitRefsStore_ListDedupesForkedShardSpellings covers a repo a pre-fix CLI
// already forked: two refs, one per spelling, naming the same checkpoint. With
// ParseRef case-folding they both parse, so the listing has to collapse them —
// onto the canonical spelling, which is the one resolveLocalRef serves to Read.
//
// The fork is planted the way it really forms, and the way it must to be
// testable on a case-insensitive filesystem at all: the folded spelling in
// packed-refs, the canonical one loose. Two shard DIRECTORIES differing only by
// case cannot coexist there, which is the whole bug — but packed-refs is a text
// file, so one spelling in each store can.
func TestGitRefsStore_ListDedupesForkedShardSpellings(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newRefsStore(t)
	dir := repoDirOf(t, store)
	canonical := mustRefName(t, foldableULID)
	foldedName, ok := FoldedRefName(foldableULID)
	require.True(t, ok)

	refsWrite(t, store, foldableULID, "sess-1", "transcript")
	oneSession, err := store.repo.Reference(canonical, true)
	require.NoError(t, err)
	firstHash := oneSession.Hash()

	refsWrite(t, store, foldableULID, "sess-2", "more transcript")
	twoSessions, err := store.repo.Reference(canonical, true)
	require.NoError(t, err)
	secondHash := twoSessions.Hash()
	require.NotEqual(t, firstHash, secondHash)

	// Strand the one-session commit under the folded spelling, in packed-refs…
	require.NoError(t, store.repo.Storer.RemoveReference(canonical))
	require.NoError(t, os.RemoveAll(filepath.Join(dir, ".git", "refs", "entire", "checkpoints", foldableULID.ShardFor())))
	require.NoError(t, store.repo.Storer.SetReference(plumbing.NewHashReference(foldedName, firstHash)))
	testutil.RunGit(t, dir, "pack-refs", "--all")
	// …and the two-session commit under the canonical one, loose. This is the
	// ref a pre-fix CLI created: with the packed folded ref unreadable by name,
	// refBase reported the checkpoint absent and the write went to a twin.
	require.NoError(t, store.repo.Storer.SetReference(plumbing.NewHashReference(canonical, secondHash)))

	reopened, err := git.PlainOpen(dir)
	require.NoError(t, err)
	forked := newGitRefsStore(reopened)
	require.ElementsMatch(t, []string{canonical.String(), foldedName.String()}, checkpointRefNames(t, forked),
		"precondition: both spellings must really exist, or the test proves nothing")

	infos, err := forked.List(ctx)
	require.NoError(t, err)
	require.Len(t, infos, 1, "one checkpoint, however many spellings name it")
	assert.Equal(t, foldableULID, infos[0].CheckpointID)

	summary, err := forked.Read(ctx, foldableULID)
	require.NoError(t, err)
	require.NotNil(t, summary)
	assert.Len(t, summary.Sessions, len(infos[0].SessionIDs),
		"the listing must show the ref Read serves, not the stranded twin")
	assert.Len(t, summary.Sessions, 2)
}

// TestGitRefsStore_FetchesFoldedRefFromRemote covers the cross-machine half:
// the checkpoint was written on a machine whose shard bucket was folded, so it
// reached the remote under the folded spelling (batchPushRefs pushes
// <ref>:<ref>). A clone with NEITHER spelling locally must ask the remote for
// both before concluding the checkpoint does not exist.
func TestGitRefsStore_FetchesFoldedRefFromRemote(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newRefsStore(t)
	canonical := mustRefName(t, foldableULID)
	foldedName, ok := FoldedRefName(foldableULID)
	require.True(t, ok)

	refsWrite(t, store, foldableULID, "sess-1", "transcript")
	ref, err := store.repo.Reference(canonical, true)
	require.NoError(t, err)
	commitHash := ref.Hash()

	// Drop every local spelling: this clone has only what it can fetch.
	require.NoError(t, store.repo.Storer.RemoveReference(canonical))
	_, err = store.repo.Reference(canonical, true)
	require.ErrorIs(t, err, plumbing.ErrReferenceNotFound, "precondition: nothing resolves locally")

	// A remote that holds the folded spelling and nothing else.
	var asked []string
	store.SetRefFetcher(func(_ context.Context, rn plumbing.ReferenceName) error {
		asked = append(asked, rn.String())
		if rn != foldedName {
			return plumbing.ErrReferenceNotFound
		}
		return store.repo.Storer.SetReference(plumbing.NewHashReference(rn, commitHash))
	})

	summary, err := store.Read(ctx, foldableULID)
	require.NoError(t, err)
	require.NotNil(t, summary, "a checkpoint published under the folded spelling must still read")
	assert.Equal(t, foldableULID, summary.CheckpointID)
	assert.Equal(t, []string{canonical.String(), foldedName.String()}, asked,
		"canonical first, folded only after the remote reports the canonical one absent")
}

// A transport failure must still return immediately rather than spending a
// second round-trip on the other spelling: the memo exists so one outage is
// paid once, and "the remote is unreachable" is not per-spelling news.
func TestGitRefsStore_FetchFailureDoesNotRetryFoldedSpelling(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newRefsStore(t)
	refsWrite(t, store, foldableULID, "sess-1", "transcript")
	require.NoError(t, store.repo.Storer.RemoveReference(mustRefName(t, foldableULID)))

	asked := 0
	store.SetRefFetcher(func(_ context.Context, _ plumbing.ReferenceName) error {
		asked++
		return assert.AnError
	})

	_, err := store.Read(ctx, foldableULID)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrCheckpointNotFound, "an outage must not be reported as absence")
	assert.Equal(t, 1, asked, "a transport failure ends the lookup; it does not try the other spelling")
}

// TestGitRefsStore_RemoteDiscoverySkipsForkedLocalCheckpoint pins the
// interaction between the folded-spelling collapse and remote discovery: a
// checkpoint the listing already collapsed from two local refs is present, so
// the remote's copy — under either spelling — must not be appended as a second,
// unhydratable stub. One map answers both questions, which is what keeps the
// two halves from disagreeing.
func TestGitRefsStore_RemoteDiscoverySkipsForkedLocalCheckpoint(t *testing.T) {
	t.Parallel()
	store := newRefsStore(t)
	canonical := mustRefName(t, foldableULID)
	foldedName, ok := FoldedRefName(foldableULID)
	require.True(t, ok)

	refsWrite(t, store, foldableULID, "sess-1", "transcript")
	ref, err := store.repo.Reference(canonical, true)
	require.NoError(t, err)
	// A local fork: both spellings name this checkpoint.
	require.NoError(t, store.repo.Storer.SetReference(plumbing.NewHashReference(foldedName, ref.Hash())))

	// The remote lists the folded spelling, plus a checkpoint held only there.
	remoteOnly := id.CheckpointID("01M2DCHJCHTR9T9MZTSB7WV77C")
	remoteOnlyRef, err := RefName(remoteOnly)
	require.NoError(t, err)
	store.SetRemoteRefLister(func(context.Context) ([]plumbing.ReferenceName, error) {
		return []plumbing.ReferenceName{foldedName, canonical, remoteOnlyRef}, nil
	})

	infos, err := store.List(WithRemoteListDiscovery(context.Background()))
	require.NoError(t, err)

	counts := map[id.CheckpointID]int{}
	for _, info := range infos {
		counts[info.CheckpointID]++
	}
	assert.Equal(t, 1, counts[foldableULID], "a forked local checkpoint stays one entry, whatever the remote lists")
	assert.Equal(t, 1, counts[remoteOnly], "a genuinely remote-only checkpoint is still discovered")
	assert.Len(t, infos, 2)
}
