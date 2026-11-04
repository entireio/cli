package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/redact"
)

// Under a git-refs primary, pre-push sends only queued checkpoint refs and never
// the v1 branch. A write to a legacy checkpoint that lives only on v1 therefore
// has to copy that checkpoint into its own ref first, or the write never leaves
// this clone.

const legacyCopyID = "a1b2c3d4e5f6"

// legacyCopyFixture is a repo with one hex checkpoint on the v1 branch and a
// router configured as it is after switching the primary to git-refs.
type legacyCopyFixture struct {
	repo   *git.Repository
	branch *GitStore
	refs   *gitRefsStore
	router PersistentStore
	cid    id.CheckpointID
	// v1Tree is the checkpoint's subtree on v1 before the write under test,
	// normalized for the refs layout as migration would write it.
	v1Tree plumbing.Hash
}

func newLegacyCopyFixture(t *testing.T) legacyCopyFixture {
	t.Helper()
	_, repo, _ := newTestRepo(t)
	cid := id.MustCheckpointID(legacyCopyID)
	branch := NewGitStore(repo, DefaultV1Refs())
	refs := newGitRefsStore(repo)
	writeRoutingCheckpoint(t, branch, cid, "before-switch")
	return legacyCopyFixture{
		repo:   repo,
		branch: branch,
		refs:   refs,
		router: newKindRoutingStore(refs, branch, refs, BackendTypeGitRefs, nil),
		cid:    cid,
		v1Tree: normalizedV1Subtree(t, repo, cid),
	}
}

// withLegacyRefFetcher returns f with a router that asks fetch for the
// checkpoint's ref before copying it from v1.
func (f legacyCopyFixture) withLegacyRefFetcher(fetch RefFetchFunc) legacyCopyFixture {
	f.router = newKindRoutingStore(f.refs, f.branch, f.refs, BackendTypeGitRefs, fetch)
	return f
}

// remoteCopy simulates another clone that already copied the checkpoint and
// pushed its ref: it returns the copy's commit and leaves no local ref.
func (f legacyCopyFixture) remoteCopy(t *testing.T) plumbing.Hash {
	t.Helper()
	ctx := context.Background()
	subtree, err := f.branch.checkpointSubtreeHash(ctx, f.cid)
	require.NoError(t, err)
	_, err = importBranchCheckpoint(ctx, f.repo, f.refs, f.cid, subtree, updatePersistentRef, importOnlyIntoAbsentRef)
	require.NoError(t, err)
	tip := f.refTip(t)
	refName, err := RefName(f.cid)
	require.NoError(t, err)
	require.NoError(t, f.repo.Storer.RemoveReference(refName))
	return tip
}

func normalizedV1Subtree(t *testing.T, repo *git.Repository, cid id.CheckpointID) plumbing.Hash {
	t.Helper()
	ref, err := repo.Reference(DefaultV1Refs().Primary, true)
	require.NoError(t, err)
	commit, err := repo.CommitObject(ref.Hash())
	require.NoError(t, err)
	root, err := commit.Tree()
	require.NoError(t, err)
	subtree, err := root.Tree(cid.Path())
	require.NoError(t, err)
	normalized, err := migratedCheckpointTree(context.Background(), repo, cid, subtree.Hash, false)
	require.NoError(t, err)
	return normalized
}

func (f legacyCopyFixture) refTip(t *testing.T) plumbing.Hash {
	t.Helper()
	refName, err := RefName(f.cid)
	require.NoError(t, err)
	ref, err := f.repo.Reference(refName, true)
	require.NoError(t, err, "the checkpoint must now have its own ref")
	return ref.Hash()
}

func (f legacyCopyFixture) requireQueued(t *testing.T) {
	t.Helper()
	refName, err := RefName(f.cid)
	require.NoError(t, err)
	queue, err := PushQueueForRepo(context.Background(), f.repo)
	require.NoError(t, err)
	queued, err := queue.Peek()
	require.NoError(t, err)
	require.Contains(t, queued, refName, "the copied ref must be queued, or pre-push never sends it")
}

func (f legacyCopyFixture) requireNoRef(t *testing.T) {
	t.Helper()
	refName, err := RefName(f.cid)
	require.NoError(t, err)
	_, err = f.repo.Reference(refName, true)
	require.ErrorIs(t, err, plumbing.ErrReferenceNotFound)
}

func (f legacyCopyFixture) markDeleted(t *testing.T) {
	t.Helper()
	list, err := DeletedCheckpointsForRepo(f.repo)
	require.NoError(t, err)
	require.NoError(t, list.Record(f.cid))
}

func afterSwitchSession(cid id.CheckpointID) ReservedSession {
	return ReservedSession(WriteOptions{
		CheckpointID: cid,
		SessionID:    "after-switch",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte("transcript for after-switch")),
		AuthorName:   "Test",
		AuthorEmail:  "test@example.com",
	})
}

func TestKindRoutingStore_ReservedSessionCopiesLegacyCheckpointIntoItsRef(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newLegacyCopyFixture(t)

	require.NoError(t, f.router.Write(ctx, afterSwitchSession(f.cid)))

	summary, err := f.refs.Read(ctx, f.cid)
	require.NoError(t, err)
	require.NotNil(t, summary, "the write must land in the checkpoint's ref")
	require.Len(t, summary.Sessions, 2, "the ref must hold the session from v1 and the new one")
	first, err := f.refs.ReadSessionMetadata(ctx, f.cid, 0)
	require.NoError(t, err)
	assert.Equal(t, "before-switch", first.SessionID)
	second, err := f.refs.ReadSessionMetadata(ctx, f.cid, 1)
	require.NoError(t, err)
	assert.Equal(t, "after-switch", second.SessionID)

	assert.True(t, treeInRefHistory(f.repo, f.refTip(t), f.v1Tree),
		"the ref's history must start from the checkpoint as it was on v1, as migration writes it")
	f.requireQueued(t)
}

// A second write after the copy builds on the ref and does not copy again.
func TestKindRoutingStore_ReservedSessionRetryAfterCopyAdvancesTheRef(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newLegacyCopyFixture(t)
	require.NoError(t, f.router.Write(ctx, afterSwitchSession(f.cid)))
	before := f.refTip(t)

	retry := WriteOptions(afterSwitchSession(f.cid))
	retry.Transcript = redact.AlreadyRedacted([]byte("retried transcript"))
	require.NoError(t, f.router.Write(ctx, ReservedSession(retry)))

	commit, err := f.repo.CommitObject(f.refTip(t))
	require.NoError(t, err)
	require.Equal(t, []plumbing.Hash{before}, commit.ParentHashes, "the retry must advance the copied ref")
	summary, err := f.refs.Read(ctx, f.cid)
	require.NoError(t, err)
	require.Len(t, summary.Sessions, 2, "a retry of the same session must not add a third")
}

// A session that reserved its hex ID before the switch and condenses after it
// writes a checkpoint that exists nowhere yet. It must still reach a ref.
func TestKindRoutingStore_ReservedHexCheckpointAbsentEverywhereLandsInItsRef(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, repo, _ := newTestRepo(t)
	cid := id.MustCheckpointID(legacyCopyID)
	branch := NewGitStore(repo, DefaultV1Refs())
	refs := newGitRefsStore(repo)
	router := newKindRoutingStore(refs, branch, refs, BackendTypeGitRefs, nil)

	require.NoError(t, router.Write(ctx, afterSwitchSession(cid)))

	summary, err := refs.Read(ctx, cid)
	require.NoError(t, err)
	require.NotNil(t, summary, "the reserved write must land in the checkpoint's ref")
	onBranch, err := branch.Read(ctx, cid)
	require.NoError(t, err)
	assert.Nil(t, onBranch, "nothing reads or pushes v1 for this checkpoint any more")
	legacyCopyFixture{repo: repo, cid: cid}.requireQueued(t)
}

func TestKindRoutingStore_ReservedSessionDoesNotResurrectDeletedCheckpoint(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newLegacyCopyFixture(t)
	f.markDeleted(t)

	require.NoError(t, f.router.Write(ctx, afterSwitchSession(f.cid)))

	f.requireNoRef(t)
	summary, err := f.branch.Read(ctx, f.cid)
	require.NoError(t, err)
	require.NotNil(t, summary)
	require.Len(t, summary.Sessions, 2, "the session must still be written, to v1")
	added, err := f.branch.ReadSessionMetadata(ctx, f.cid, 1)
	require.NoError(t, err)
	assert.Equal(t, "after-switch", added.SessionID)
}

// A backfill's own remote check can find nothing while a plain git fetch from
// another process installs the ref. The backfill must land on that ref: on v1
// it would be hidden behind the ref and never pushed.
func TestKindRoutingStore_BackfillLandsOnARefThatAppearsDuringItsRemoteCheck(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newLegacyCopyFixture(t)
	fetched := f.remoteCopy(t)
	f.refs.SetRefFetcher(func(_ context.Context, ref plumbing.ReferenceName) error {
		require.NoError(t, f.repo.Storer.SetReference(plumbing.NewHashReference(ref, fetched)))
		return fmt.Errorf("remote had no %s when asked: %w", ref, plumbing.ErrReferenceNotFound)
	})

	require.NoError(t, f.router.Write(ctx, SessionSummary{CheckpointID: f.cid, Summary: &Summary{Intent: "backfilled"}}))

	onRef, err := f.refs.ReadSessionMetadata(ctx, f.cid, 0)
	require.NoError(t, err)
	require.NotNil(t, onRef.Summary, "the backfill must land on the ref that appeared")
	assert.Equal(t, "backfilled", onRef.Summary.Intent)
	onBranch, err := f.branch.ReadSessionMetadata(ctx, f.cid, 0)
	require.NoError(t, err)
	assert.Nil(t, onBranch.Summary, "v1 must not take a backfill the ref now serves")
}

// A ref that appears while the copy asks the remote, such as a teammate's copy
// another process here fetched, holds sessions this clone's v1 lacks. The copy
// must build on it, not lay v1's tree over it: that would drop those sessions
// from the tip, and the push is a fast-forward the remote accepts.
func TestKindRoutingStore_CopyBuildsOnARefThatAppearsDuringTheRemoteCheck(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newLegacyCopyFixture(t)
	f = f.withLegacyRefFetcher(func(_ context.Context, ref plumbing.ReferenceName) error {
		writeRoutingCheckpoint(t, newGitRefsStore(f.repo), f.cid, "fetched-meanwhile")
		return fmt.Errorf("remote had no %s when asked: %w", ref, plumbing.ErrReferenceNotFound)
	})

	require.NoError(t, f.router.Write(ctx, afterSwitchSession(f.cid)))

	summary, err := f.refs.Read(ctx, f.cid)
	require.NoError(t, err)
	require.NotNil(t, summary)
	require.Len(t, summary.Sessions, 2)
	first, err := f.refs.ReadSessionMetadata(ctx, f.cid, 0)
	require.NoError(t, err)
	assert.Equal(t, "fetched-meanwhile", first.SessionID, "the ref that appeared must keep its session")
	second, err := f.refs.ReadSessionMetadata(ctx, f.cid, 1)
	require.NoError(t, err)
	assert.Equal(t, "after-switch", second.SessionID)
	assert.False(t, treeInRefHistory(f.repo, f.refTip(t), f.v1Tree),
		"v1 must not be imported over a ref that already exists")
}

func TestKindRoutingStore_BackfillCopiesLegacyCheckpointIntoItsRef(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		write func(cid id.CheckpointID) WriteRequest
	}{
		{
			name: "summary",
			write: func(cid id.CheckpointID) WriteRequest {
				return SessionSummary{CheckpointID: cid, Summary: &Summary{Intent: "copied intent"}}
			},
		},
		{
			name: "transcript",
			write: func(cid id.CheckpointID) WriteRequest {
				return SessionTranscript{
					CheckpointID: cid,
					SessionID:    "before-switch",
					Transcript:   redact.AlreadyRedacted([]byte("finalized transcript")),
				}
			},
		},
		{
			name: "tasks",
			write: func(cid id.CheckpointID) WriteRequest {
				return Tasks{CheckpointID: cid, Tasks: []TaskPayload{{
					ToolUseID:  "toolu_01Copied",
					AgentID:    "a0123456789abcdef",
					Transcript: redact.AlreadyRedacted([]byte("copied subagent\n")),
				}}}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newLegacyCopyFixture(t)

			require.NoError(t, f.router.Write(ctx, tt.write(f.cid)))

			summary, err := f.refs.Read(ctx, f.cid)
			require.NoError(t, err)
			require.NotNil(t, summary, "the backfill must land in the checkpoint's ref")
			assert.True(t, treeInRefHistory(f.repo, f.refTip(t), f.v1Tree),
				"the ref's history must start from the checkpoint as it was on v1")
			f.requireQueued(t)
		})
	}
}

// A clone that copies a checkpoint another clone already copied and pushed
// creates an unrelated history for the same ref, and push recovery replays it
// over the other clone's sessions. So the copy first asks the remote for the
// ref and builds on it when it is there.
func TestKindRoutingStore_ReservedSessionBuildsOnTheRemoteRefInsteadOfCopying(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newLegacyCopyFixture(t)
	remoteTip := f.remoteCopy(t)
	fetches := 0
	f = f.withLegacyRefFetcher(func(_ context.Context, ref plumbing.ReferenceName) error {
		fetches++
		return f.repo.Storer.SetReference(plumbing.NewHashReference(ref, remoteTip))
	})

	require.NoError(t, f.router.Write(ctx, afterSwitchSession(f.cid)))

	commit, err := f.repo.CommitObject(f.refTip(t))
	require.NoError(t, err)
	require.Equal(t, []plumbing.Hash{remoteTip}, commit.ParentHashes,
		"the write must advance the remote's ref, not start a second copy")
	assert.Equal(t, 1, fetches)
	f.requireQueued(t)
}

// A backfill whose primary store has no fetcher of its own reaches the copy
// through ErrCheckpointNotFound. When the copy's probe brings in the remote's
// ref, the backfill must land there, not fall through to v1.
func TestKindRoutingStore_BackfillBuildsOnTheRemoteRefInsteadOfCopying(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newLegacyCopyFixture(t)
	remoteTip := f.remoteCopy(t)
	f = f.withLegacyRefFetcher(func(_ context.Context, ref plumbing.ReferenceName) error {
		return f.repo.Storer.SetReference(plumbing.NewHashReference(ref, remoteTip))
	})
	v1Before, err := f.repo.Reference(DefaultV1Refs().Primary, true)
	require.NoError(t, err)

	require.NoError(t, f.router.Write(ctx, SessionSummary{CheckpointID: f.cid, Summary: &Summary{Intent: "on the remote ref"}}))

	commit, err := f.repo.CommitObject(f.refTip(t))
	require.NoError(t, err)
	require.Equal(t, []plumbing.Hash{remoteTip}, commit.ParentHashes, "the backfill must advance the remote's ref")
	v1After, err := f.repo.Reference(DefaultV1Refs().Primary, true)
	require.NoError(t, err)
	assert.Equal(t, v1Before.Hash(), v1After.Hash(), "the backfill must not land on v1")
}

func TestKindRoutingStore_ReservedSessionCopiesWhenTheRemoteHasNoRef(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newLegacyCopyFixture(t).withLegacyRefFetcher(func(context.Context, plumbing.ReferenceName) error {
		return fmt.Errorf("probe remote: %w", plumbing.ErrReferenceNotFound)
	})

	require.NoError(t, f.router.Write(ctx, afterSwitchSession(f.cid)))

	assert.True(t, treeInRefHistory(f.repo, f.refTip(t), f.v1Tree),
		"the ref's history must start from the checkpoint as it was on v1")
	f.requireQueued(t)
}

// Without an answer from the remote, copying could fork another clone's ref,
// so the write fails and nothing is written. The caller retries it later.
func TestKindRoutingStore_WriteFailsWithoutCopyingWhenTheRemoteCannotBeAsked(t *testing.T) {
	t.Parallel()
	errUnreachable := errors.New("ssh: connect to host example.com port 22: Operation timed out")
	tests := []struct {
		name  string
		write func(cid id.CheckpointID) WriteRequest
	}{
		{name: "reserved session", write: func(cid id.CheckpointID) WriteRequest { return afterSwitchSession(cid) }},
		{name: "summary backfill", write: func(cid id.CheckpointID) WriteRequest {
			return SessionSummary{CheckpointID: cid, Summary: &Summary{Intent: "offline"}}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newLegacyCopyFixture(t).withLegacyRefFetcher(func(context.Context, plumbing.ReferenceName) error {
				return errUnreachable
			})

			err := f.router.Write(context.Background(), tt.write(f.cid))

			require.ErrorIs(t, err, errUnreachable)
			f.requireNoRef(t)
		})
	}
}

// Asking the remote costs a network round trip inside git hooks, so it happens
// only for a checkpoint that v1 actually holds.
func TestKindRoutingStore_ReservedHexCheckpointAbsentFromV1DoesNotAskTheRemote(t *testing.T) {
	t.Parallel()
	_, repo, _ := newTestRepo(t)
	cid := id.MustCheckpointID(legacyCopyID)
	branch := NewGitStore(repo, DefaultV1Refs())
	refs := newGitRefsStore(repo)
	router := newKindRoutingStore(refs, branch, refs, BackendTypeGitRefs, func(context.Context, plumbing.ReferenceName) error {
		t.Error("the remote must not be asked about a checkpoint v1 does not hold")
		return nil
	})

	require.NoError(t, router.Write(context.Background(), afterSwitchSession(cid)))

	summary, err := refs.Read(context.Background(), cid)
	require.NoError(t, err)
	require.NotNil(t, summary, "the reserved write must still land in the checkpoint's ref")
}

func TestKindRoutingStore_BackfillDoesNotResurrectDeletedCheckpoint(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newLegacyCopyFixture(t)
	f.markDeleted(t)

	err := f.router.Write(ctx, SessionSummary{CheckpointID: f.cid, Summary: &Summary{Intent: "deleted"}})
	require.NoError(t, err)

	f.requireNoRef(t)
	session, err := f.branch.ReadSessionMetadata(ctx, f.cid, 0)
	require.NoError(t, err)
	require.NotNil(t, session.Summary)
	assert.Equal(t, "deleted", session.Summary.Intent, "the backfill must land on v1, as before the switch")
}

// The deleted list keeps a deleted checkpoint from coming back as a ref, so a
// list that cannot be read stops the write instead of being treated as empty.
func TestKindRoutingStore_WriteFailsClosedOnAnUnreadableDeletedList(t *testing.T) {
	t.Parallel()
	f := newLegacyCopyFixture(t)
	list, err := DeletedCheckpointsForRepo(f.repo)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(list.dir, deletedCheckpointsFileName), []byte("{not json"), 0o600))

	err = f.router.Write(context.Background(), afterSwitchSession(f.cid))

	require.ErrorContains(t, err, "read deleted checkpoints")
	f.requireNoRef(t)
}
