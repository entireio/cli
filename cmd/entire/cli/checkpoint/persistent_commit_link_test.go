package checkpoint

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/redact"
)

// Two distinct, well-formed commit SHAs for link tests. Neither needs to exist
// as an object: the store treats commit_sha as opaque metadata.
const (
	linkSHAFallback = "b01b59663fd4860fd15a9939499be44a14dbf168"
	linkSHARecorded = "5f2e1d0c9b8a7766554433221100ffeeddccbbaa"
)

// linkTestSessionID is the single session every commit-link fixture writes.
const linkTestSessionID = "s1"

// writeImportedSession writes a one-session imported checkpoint carrying the
// given link, the starting point for every backfill test below.
func writeImportedSession(t *testing.T, store PersistentStore, red redact.RedactedBytes, cid id.CheckpointID, commitSHA, method string) {
	t.Helper()
	require.NoError(t, store.Write(context.Background(), Session{
		CheckpointID:     cid,
		SessionID:        linkTestSessionID,
		Strategy:         "import",
		Kind:             importedSessionKind,
		Agent:            agent.AgentTypeClaudeCode,
		Transcript:       red,
		Prompts:          []string{"hi"},
		CheckpointsCount: 1,
		CommitSHA:        commitSHA,
		CommitSHAMethod:  method,
		AuthorName:       "Test",
		AuthorEmail:      "test@example.com",
	}))
}

// assertStoredLink checks both tiers a link lives on: the session metadata and
// the root summary. A backfill that updates only one of them would leave the
// two disagreeing, and different readers use different tiers.
func assertStoredLink(t *testing.T, store PersistentStore, cid id.CheckpointID, wantSHA, wantMethod string) {
	t.Helper()
	ctx := context.Background()
	md, err := store.ReadSessionMetadata(ctx, cid, 0)
	require.NoError(t, err)
	assert.Equal(t, wantSHA, md.CommitSHA, "session metadata commit_sha")
	assert.Equal(t, wantMethod, md.CommitSHAMethod, "session metadata commit_sha_method")

	summary, err := store.Read(ctx, cid)
	require.NoError(t, err)
	require.NotNil(t, summary)
	assert.Equal(t, wantSHA, summary.CommitSHA, "root summary commit_sha")
	assert.Equal(t, wantMethod, summary.CommitSHAMethod, "root summary commit_sha_method")
}

func TestGitStore_CommitSHABackfill(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cid := id.MustCheckpointID("aabbccddeeff")

	t.Run("upgrades a fallback anchor to a recorded link", func(t *testing.T) {
		t.Parallel()
		store, red := newImportedTestStore(t)
		writeImportedSession(t, store, red, cid, linkSHAFallback, CommitSHAMethodFallback)
		// The create path must land the method on both tiers too, or the
		// backfill below would be indistinguishable from a first write.
		assertStoredLink(t, store, cid, linkSHAFallback, CommitSHAMethodFallback)

		require.NoError(t, store.Write(ctx, CheckpointCommitSHA{
			CheckpointID: cid, SessionID: linkTestSessionID,
			CommitSHA: linkSHARecorded, Method: CommitSHAMethodRecorded,
		}))
		assertStoredLink(t, store, cid, linkSHARecorded, CommitSHAMethodRecorded)
	})

	t.Run("an empty session ID targets every imported session", func(t *testing.T) {
		t.Parallel()
		store, red := newImportedTestStore(t)
		writeImportedSession(t, store, red, cid, "", "")

		require.NoError(t, store.Write(ctx, CheckpointCommitSHA{
			CheckpointID: cid,
			CommitSHA:    linkSHARecorded, Method: CommitSHAMethodRecorded,
		}))
		assertStoredLink(t, store, cid, linkSHARecorded, CommitSHAMethodRecorded)
	})

	t.Run("refuses to downgrade a recorded link and writes nothing", func(t *testing.T) {
		t.Parallel()
		store, red := newImportedTestStore(t)
		writeImportedSession(t, store, red, cid, linkSHARecorded, CommitSHAMethodRecorded)
		before := v1Ref(t, store)

		require.NoError(t, store.Write(ctx, CheckpointCommitSHA{
			CheckpointID: cid, SessionID: linkTestSessionID,
			CommitSHA: linkSHAFallback, Method: CommitSHAMethodHeuristic,
		}), "a refused downgrade is a no-op, not an error")
		assertStoredLink(t, store, cid, linkSHARecorded, CommitSHAMethodRecorded)
		assert.Equal(t, before, v1Ref(t, store),
			"a refused downgrade must not create a commit on the v1 branch")
	})

	t.Run("re-applying the same link writes nothing", func(t *testing.T) {
		t.Parallel()
		store, red := newImportedTestStore(t)
		writeImportedSession(t, store, red, cid, linkSHARecorded, CommitSHAMethodRecorded)
		before := v1Ref(t, store)

		require.NoError(t, store.Write(ctx, CheckpointCommitSHA{
			CheckpointID: cid, SessionID: linkTestSessionID,
			CommitSHA: linkSHARecorded, Method: CommitSHAMethodRecorded,
		}))
		assert.Equal(t, before, v1Ref(t, store),
			"an idempotent backfill must leave the v1 branch untouched")
	})

	t.Run("reports not-found for an unknown checkpoint", func(t *testing.T) {
		t.Parallel()
		store, red := newImportedTestStore(t)
		writeImportedSession(t, store, red, cid, linkSHAFallback, CommitSHAMethodFallback)

		err := store.Write(ctx, CheckpointCommitSHA{
			CheckpointID: id.MustCheckpointID("112233445566"),
			CommitSHA:    linkSHARecorded, Method: CommitSHAMethodRecorded,
		})
		require.ErrorIs(t, err, ErrCheckpointNotFound)
	})
}

// v1Ref returns the store's primary ref hash as a string, or "" when the branch
// does not exist yet.
func v1Ref(t *testing.T, store *GitStore) string {
	t.Helper()
	ref, err := store.repo.Reference(store.refs.Primary, true)
	if err != nil {
		return ""
	}
	return ref.Hash().String()
}

// TestGitRefsStore_CommitSHABackfill mirrors the git-branch coverage on the
// per-checkpoint-ref backend: the link lands on the ref, the push queue picks
// the ref up, and a refused/no-op backfill neither moves the ref nor enqueues
// it (an empty push would otherwise be scheduled for every converged re-run).
func TestGitRefsStore_CommitSHABackfill(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cid := id.MustCheckpointID("a1b2c3d4e5f6")

	refTip := func(t *testing.T, store *gitRefsStore) string {
		t.Helper()
		ref, err := store.repo.Reference(mustRefName(t, cid), true)
		if err != nil {
			return ""
		}
		return ref.Hash().String()
	}

	t.Run("upgrades a fallback anchor and enqueues the ref", func(t *testing.T) {
		t.Parallel()
		store := newRefsStore(t)
		writeImportedSession(t, store, redact.AlreadyRedacted([]byte("t\n")), cid,
			linkSHAFallback, CommitSHAMethodFallback)

		q := drainedPushQueue(t, store)

		require.NoError(t, store.Write(ctx, CheckpointCommitSHA{
			CheckpointID: cid, SessionID: linkTestSessionID,
			CommitSHA: linkSHARecorded, Method: CommitSHAMethodRecorded,
		}))
		assertStoredLink(t, store, cid, linkSHARecorded, CommitSHAMethodRecorded)

		refs, err := q.Drain()
		require.NoError(t, err)
		assert.Contains(t, refs, mustRefName(t, cid), "a commit-link backfill should enqueue its ref for push")
	})

	t.Run("a refused downgrade moves nothing", func(t *testing.T) {
		t.Parallel()
		store := newRefsStore(t)
		writeImportedSession(t, store, redact.AlreadyRedacted([]byte("t\n")), cid,
			linkSHARecorded, CommitSHAMethodRecorded)
		before := refTip(t, store)
		q := drainedPushQueue(t, store)

		require.NoError(t, store.Write(ctx, CheckpointCommitSHA{
			CheckpointID: cid, SessionID: linkTestSessionID,
			CommitSHA: linkSHAFallback, Method: CommitSHAMethodHeuristic,
		}))
		assertStoredLink(t, store, cid, linkSHARecorded, CommitSHAMethodRecorded)
		assert.Equal(t, before, refTip(t, store), "a refused downgrade must not move the checkpoint ref")

		refs, err := q.Drain()
		require.NoError(t, err)
		assert.Empty(t, refs, "a no-op backfill must not enqueue an empty push")
	})

	t.Run("reports not-found for an unknown checkpoint", func(t *testing.T) {
		t.Parallel()
		store := newRefsStore(t)
		err := store.Write(ctx, CheckpointCommitSHA{
			CheckpointID: cid, CommitSHA: linkSHARecorded, Method: CommitSHAMethodRecorded,
		})
		require.ErrorIs(t, err, ErrCheckpointNotFound)
	})
}

// drainedPushQueue returns the repo's push queue with all pending entries
// removed, so a later Drain reports only what the code under test enqueued.
// (Drain reads without clearing; Remove is what empties the queue.)
func drainedPushQueue(t *testing.T, store *gitRefsStore) *PushQueue {
	t.Helper()
	q, err := PushQueueForRepo(context.Background(), store.repo)
	require.NoError(t, err)
	pending, err := q.Drain()
	require.NoError(t, err)
	require.NoError(t, q.Remove(pending))
	return q
}

// TestKindRoutingStore_CommitSHABackfillFallsBackToBranch is the routing half
// of the contract: an imported checkpoint minted as 12-hex lives on the v1
// branch, so under a git-refs primary its link backfill must fall through to
// the branch store instead of failing with not-found. A CheckpointCommitSHA
// missing from backfillTarget would take create routing (primary only) and
// silently discard the write.
func TestKindRoutingStore_CommitSHABackfillFallsBackToBranch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	hexID := id.MustCheckpointID("a1b2c3d4e5f6")
	_, repo, _ := newTestRepo(t)
	branch := NewGitStore(repo, DefaultV1Refs())
	refs := newGitRefsStore(repo)
	writeImportedSession(t, branch, redact.AlreadyRedacted([]byte("t\n")), hexID,
		linkSHAFallback, CommitSHAMethodFallback)

	router := newKindRoutingStore(refs, branch, refs, BackendTypeGitRefs)

	require.NoError(t, router.Write(ctx, CheckpointCommitSHA{
		CheckpointID: hexID, SessionID: linkTestSessionID,
		CommitSHA: linkSHARecorded, Method: CommitSHAMethodRecorded,
	}), "a commit-link backfill for a hex checkpoint on the branch must fall back to the branch store")

	assertStoredLink(t, router, hexID, linkSHARecorded, CommitSHAMethodRecorded)
}
