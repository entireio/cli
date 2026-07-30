package checkpoint

import (
	"context"
	"testing"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/redact"
)

func newRefsStore(t *testing.T) *gitRefsStore {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "README.md", "# test")
	testutil.GitAdd(t, dir, "README.md")
	testutil.GitCommit(t, dir, "init")
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	return newGitRefsStore(repo)
}

func refsWrite(t *testing.T, store *gitRefsStore, cid id.CheckpointID, sessionID, transcript string) {
	t.Helper()
	require.NoError(t, store.Write(context.Background(), Session{
		CheckpointID: cid,
		SessionID:    sessionID,
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(transcript)),
		Prompts:      []string{"do the thing"},
		FilesTouched: []string{"a.go"},
		AuthorName:   "Test Author",
		AuthorEmail:  "test@example.com",
	}))
}

func TestGitRefsStore_WriteEnqueuesForPush(t *testing.T) {
	t.Parallel()
	store := newRefsStore(t)
	cid := id.MustCheckpointID("a1b2c3d4e5f6")

	refsWrite(t, store, cid, "sess-1", "transcript")

	q, err := PushQueueForRepo(context.Background(), store.repo)
	require.NoError(t, err)
	refs, err := q.Drain()
	require.NoError(t, err)
	assert.Contains(t, refs, mustRefName(t, cid), "a session write should enqueue its checkpoint ref for push")
}

func TestGitRefsStore_OnDemandRefFetch(t *testing.T) {
	t.Parallel()
	store := newRefsStore(t)
	ctx := context.Background()
	cid := id.MustCheckpointID("a1b2c3d4e5f6")

	refsWrite(t, store, cid, "sess-1", "transcript")
	ref, err := store.repo.Reference(mustRefName(t, cid), true)
	require.NoError(t, err)
	commitHash := ref.Hash()

	// Simulate "not present locally" by dropping the ref (the commit object
	// survives, so a fetch can restore the ref).
	require.NoError(t, store.repo.Storer.RemoveReference(mustRefName(t, cid)))

	// No fetcher configured: read resolves to not-found (nil summary).
	summary, err := store.Read(ctx, cid)
	require.NoError(t, err)
	assert.Nil(t, summary, "missing ref with no fetcher reads as not-found")

	// A fetcher that restores the ref makes the read succeed, and is invoked once.
	fetched := 0
	store.SetRefFetcher(func(_ context.Context, rn plumbing.ReferenceName) error {
		fetched++
		return store.repo.Storer.SetReference(plumbing.NewHashReference(rn, commitHash))
	})
	summary, err = store.Read(ctx, cid)
	require.NoError(t, err)
	require.NotNil(t, summary, "ref should resolve after on-demand fetch")
	assert.Equal(t, cid, summary.CheckpointID)
	assert.Equal(t, 1, fetched, "fetcher invoked once for the missing ref")
}

// TestGitRefsStore_OnDemandRefFetch_FailurePropagates: a fetch that fails
// (offline, network error, context cancellation) must surface as a real error
// rather than be masked as "checkpoint not found" — otherwise a transient
// failure looks like missing data. A fetch that succeeds but still finds no such
// ref on the remote is a genuine not-found and reads as (nil, nil).
func TestGitRefsStore_OnDemandRefFetch_FailurePropagates(t *testing.T) {
	t.Parallel()

	t.Run("fetch error propagates", func(t *testing.T) {
		t.Parallel()
		store := newRefsStore(t)
		store.SetRefFetcher(func(_ context.Context, _ plumbing.ReferenceName) error {
			return assert.AnError // fetch fails (e.g. offline / network)
		})
		summary, err := store.Read(context.Background(), id.MustCheckpointID("ffffffffffff"))
		require.ErrorIs(t, err, assert.AnError, "a failed fetch must not be masked as not-found")
		assert.Nil(t, summary)
	})

	t.Run("successful fetch with still-absent ref reads as not-found", func(t *testing.T) {
		t.Parallel()
		store := newRefsStore(t)
		store.SetRefFetcher(func(_ context.Context, _ plumbing.ReferenceName) error {
			return nil // fetch "succeeds" but the ref still doesn't exist
		})
		summary, err := store.Read(context.Background(), id.MustCheckpointID("ffffffffffff"))
		require.NoError(t, err, "a genuinely absent checkpoint reads as not-found")
		assert.Nil(t, summary)
	})
}

// TestGitRefsStore_ListRemoteDiscovery exercises the git-refs List remote-ref
// discovery that fixes #1770: on a second device, a checkpoint written
// elsewhere has no local ref, so a purely local List shows zero. With discovery
// opted in (WithRemoteListDiscovery) and a remote lister configured, List
// enumerates the checkpoint remote (names only) and surfaces the not-yet-local
// checkpoint; a later read hydrates it.
func TestGitRefsStore_ListRemoteDiscovery(t *testing.T) {
	t.Parallel()

	// A ULID that exists only "on the remote" (never written locally).
	remoteOnly := id.CheckpointID("01KVBJCWYA4YW6J5M9GP655HZN")
	remoteOnlyRef := mustRefName(t, remoteOnly)
	//nolint:unparam // test fake mirrors RemoteRefListFunc's (…, error) signature; it always succeeds here.
	lister := func(context.Context) ([]plumbing.ReferenceName, error) {
		return []plumbing.ReferenceName{remoteOnlyRef}, nil
	}

	ids := func(infos []CheckpointInfo) map[id.CheckpointID]struct{} {
		out := make(map[id.CheckpointID]struct{}, len(infos))
		for _, info := range infos {
			out[info.CheckpointID] = struct{}{}
		}
		return out
	}

	t.Run("discovers remote-only checkpoint when opted in", func(t *testing.T) {
		t.Parallel()
		store := newRefsStore(t)
		local := id.MustCheckpointID("a1b2c3d4e5f6")
		refsWrite(t, store, local, "s-local", "t")
		store.SetRemoteRefLister(lister)

		infos, err := store.List(WithRemoteListDiscovery(context.Background()))
		require.NoError(t, err)
		got := ids(infos)
		assert.Contains(t, got, local, "local checkpoint still listed")
		assert.Contains(t, got, remoteOnly, "remote-only checkpoint discovered via ls-remote")

		// The discovered entry carries the ULID's embedded creation time, so it
		// sorts by real recency without an object fetch.
		for _, info := range infos {
			if info.CheckpointID == remoteOnly {
				assert.False(t, info.CreatedAt.IsZero(), "discovered ULID checkpoint should carry its embedded creation time")
			}
		}
	})

	t.Run("stays local-only without the discovery marker", func(t *testing.T) {
		t.Parallel()
		store := newRefsStore(t)
		local := id.MustCheckpointID("a1b2c3d4e5f6")
		refsWrite(t, store, local, "s-local", "t")
		store.SetRemoteRefLister(lister)

		infos, err := store.List(context.Background())
		require.NoError(t, err)
		got := ids(infos)
		assert.Contains(t, got, local)
		assert.NotContains(t, got, remoteOnly, "no enumeration without WithRemoteListDiscovery (keeps the hot path network-free)")
	})

	t.Run("stays local-only when no lister is configured", func(t *testing.T) {
		t.Parallel()
		store := newRefsStore(t)
		local := id.MustCheckpointID("a1b2c3d4e5f6")
		refsWrite(t, store, local, "s-local", "t")

		infos, err := store.List(WithRemoteListDiscovery(context.Background()))
		require.NoError(t, err)
		got := ids(infos)
		assert.Contains(t, got, local)
		assert.NotContains(t, got, remoteOnly)
	})

	t.Run("does not duplicate a checkpoint already present locally", func(t *testing.T) {
		t.Parallel()
		store := newRefsStore(t)
		local := id.MustCheckpointID("a1b2c3d4e5f6")
		refsWrite(t, store, local, "s-local", "t")
		// The lister also advertises the checkpoint that already exists locally.
		store.SetRemoteRefLister(func(context.Context) ([]plumbing.ReferenceName, error) {
			return []plumbing.ReferenceName{mustRefName(t, local), remoteOnlyRef}, nil
		})

		infos, err := store.List(WithRemoteListDiscovery(context.Background()))
		require.NoError(t, err)
		count := 0
		for _, info := range infos {
			if info.CheckpointID == local {
				count++
			}
		}
		assert.Equal(t, 1, count, "a locally-present checkpoint advertised by the remote is not duplicated")
	})

	t.Run("enumeration failure degrades to local-only", func(t *testing.T) {
		t.Parallel()
		store := newRefsStore(t)
		local := id.MustCheckpointID("a1b2c3d4e5f6")
		refsWrite(t, store, local, "s-local", "t")
		store.SetRemoteRefLister(func(context.Context) ([]plumbing.ReferenceName, error) {
			return nil, assert.AnError // e.g. offline / ls-remote failed
		})

		infos, err := store.List(WithRemoteListDiscovery(context.Background()))
		require.NoError(t, err, "a remote enumeration failure must not fail the whole listing")
		got := ids(infos)
		assert.Contains(t, got, local, "local checkpoints remain listed when discovery fails")
		assert.NotContains(t, got, remoteOnly)
	})
}

// TestHydrateListedCheckpointInfo covers the trail-871 gap: a names-only List
// stub has empty SessionID, so --session filters would silently drop it until
// the checkpoint is read. HydrateListedCheckpointInfo fills session identity
// from the store (triggering on-demand fetch when configured) so filters match.
func TestHydrateListedCheckpointInfo(t *testing.T) {
	t.Parallel()

	store := newRefsStore(t)
	cid := id.MustCheckpointID("a1b2c3d4e5f6")
	refsWrite(t, store, cid, "session-from-device-a", "transcript")

	stub := remoteDiscoveredInfo(cid)
	require.True(t, listedCheckpointNeedsHydration(stub))
	require.True(t, stub.ListedStub)
	require.Empty(t, stub.SessionID)
	require.Zero(t, stub.SessionCount)

	hydrated := HydrateListedCheckpointInfo(context.Background(), store, stub)
	assert.Equal(t, "session-from-device-a", hydrated.SessionID)
	assert.Equal(t, 1, hydrated.SessionCount)
	assert.Equal(t, []string{"session-from-device-a"}, hydrated.SessionIDs)
	assert.False(t, listedCheckpointNeedsHydration(hydrated))
	assert.False(t, hydrated.ListedStub)

	// Already-hydrated infos are returned unchanged (no redundant reads needed
	// for the session-filter path once collectCheckpoint has cached them).
	again := HydrateListedCheckpointInfo(context.Background(), store, hydrated)
	assert.Equal(t, hydrated, again)

	// Missing checkpoint: fail-once clears ListedStub so callers do not re-fetch,
	// but leaves SessionID empty so listing can still surface the ID.
	missing := remoteDiscoveredInfo(id.CheckpointID("01KVBJCWYA4YW6J5M9GP655HZN"))
	failed := HydrateListedCheckpointInfo(context.Background(), store, missing)
	assert.Equal(t, missing.CheckpointID, failed.CheckpointID)
	assert.Empty(t, failed.SessionID)
	assert.False(t, failed.ListedStub, "failed hydration must clear ListedStub (fail-once)")
	assert.False(t, listedCheckpointNeedsHydration(failed))
}

// TestHydrateListedCheckpointInfo_MatchesLocalList pins the field mapping shared
// with readCommittedInfoFromCheckpointTree: hydrating a stub for a locally
// present checkpoint must yield the same CheckpointInfo that List returns for
// it. Deliberate CreatedAt divergence (documented on HydrateListedCheckpointInfo):
// local List assigns meta.CreatedAt unconditionally; hydration only overwrites
// when non-zero (keeping ULID-derived time). A normal refsWrite has non-zero
// CreatedAt, so both paths agree here.
func TestHydrateListedCheckpointInfo_MatchesLocalList(t *testing.T) {
	t.Parallel()

	store := newRefsStore(t)
	cid := id.MustCheckpointID("a1b2c3d4e5f6")
	refsWrite(t, store, cid, "session-from-device-a", "transcript")

	infos, err := store.List(context.Background())
	require.NoError(t, err)
	var local CheckpointInfo
	for _, info := range infos {
		if info.CheckpointID == cid {
			local = info
			break
		}
	}
	require.Equal(t, cid, local.CheckpointID)
	require.False(t, local.ListedStub)

	hydrated := HydrateListedCheckpointInfo(context.Background(), store, remoteDiscoveredInfo(cid))
	assert.Equal(t, local, hydrated)
}

func TestGitRefsStore_WriteAllVariantsAndRead(t *testing.T) {
	t.Parallel()
	store := newRefsStore(t)
	ctx := context.Background()
	cid := id.MustCheckpointID("a1b2c3d4e5f6")
	const sessionID = "sess-1"

	refsWrite(t, store, cid, sessionID, "initial transcript")
	require.NoError(t, store.Write(ctx, SessionTranscript{
		CheckpointID: cid, SessionID: sessionID,
		Transcript: redact.AlreadyRedacted([]byte("final transcript")),
		Prompts:    []string{"do the thing"},
	}))
	require.NoError(t, store.Write(ctx, SessionSummary{
		CheckpointID: cid, Summary: &Summary{Intent: "intent-x", Outcome: "outcome-y"},
	}))
	require.NoError(t, store.Write(ctx, CheckpointAttribution{
		CheckpointID: cid, Attribution: &Attribution{AgentLines: 7, AgentPercentage: 70},
	}))

	// The per-checkpoint ref exists at the sharded name.
	_, err := store.repo.Reference(mustRefName(t, cid), true)
	require.NoError(t, err, "checkpoint ref should exist")

	summary, err := store.Read(ctx, cid)
	require.NoError(t, err)
	require.NotNil(t, summary)
	require.Len(t, summary.Sessions, 1)
	require.NotNil(t, summary.CombinedAttribution)
	assert.Equal(t, 7, summary.CombinedAttribution.AgentLines)

	content, err := store.ReadSessionContent(ctx, cid, 0)
	require.NoError(t, err)
	assert.Equal(t, []byte("final transcript"), content.Transcript)

	meta, err := store.ReadSessionMetadata(ctx, cid, 0)
	require.NoError(t, err)
	require.NotNil(t, meta.Summary)
	assert.Equal(t, "intent-x", meta.Summary.Intent)
}

func TestGitRefsStore_RefSharding(t *testing.T) {
	t.Parallel()
	store := newRefsStore(t)
	ctx := context.Background()

	// A legacy hex checkpoint stores under a last-two-char shard and round-trips.
	legacy := id.MustCheckpointID("a1b2c3d4e5f6")
	refsWrite(t, store, legacy, "s-legacy", "t")
	_, err := store.repo.Reference("refs/entire/checkpoints/f6/a1b2c3d4e5f6", true)
	require.NoError(t, err)
	summary, err := store.Read(ctx, legacy)
	require.NoError(t, err)
	require.NotNil(t, summary)
	assert.Equal(t, legacy, summary.CheckpointID)

	// ULIDs shard on the last two chars too (the ref namespace is ULID-ready). Only
	// the ref-naming layer is asserted here: storing a ULID checkpoint also needs
	// id.CheckpointID JSON (un)marshaling to accept ULIDs, which lands with the
	// deferred ULID-generation switch.
	ulid := id.CheckpointID("01KVBJCWYA4YW6J5M9GP655HZN")
	assert.Equal(t, "refs/entire/checkpoints/ZN/01KVBJCWYA4YW6J5M9GP655HZN", mustRefName(t, ulid).String())
}

func TestGitRefsStore_MultipleSessions(t *testing.T) {
	t.Parallel()
	store := newRefsStore(t)
	ctx := context.Background()
	cid := id.MustCheckpointID("abcdef012345")

	refsWrite(t, store, cid, "sess-1", "first")
	refsWrite(t, store, cid, "sess-2", "second")

	summary, err := store.Read(ctx, cid)
	require.NoError(t, err)
	require.Len(t, summary.Sessions, 2, "two sessions should occupy two numbered dirs")

	infos, err := store.List(ctx)
	require.NoError(t, err)
	require.Len(t, infos, 1)
	assert.Equal(t, 2, infos[0].SessionCount)
}

func TestGitRefsStore_SeparateCheckpointsSeparateRefs(t *testing.T) {
	t.Parallel()
	store := newRefsStore(t)
	ctx := context.Background()
	cid1 := id.MustCheckpointID("a1b2c3d4e5f6")
	cid2 := id.MustCheckpointID("f6e5d4c3b2a1")

	refsWrite(t, store, cid1, "s1", "t1")
	refsWrite(t, store, cid2, "s2", "t2")

	_, err := store.repo.Reference("refs/entire/checkpoints/f6/a1b2c3d4e5f6", true)
	require.NoError(t, err)
	_, err = store.repo.Reference("refs/entire/checkpoints/a1/f6e5d4c3b2a1", true)
	require.NoError(t, err)

	infos, err := store.List(ctx)
	require.NoError(t, err)
	assert.Len(t, infos, 2)
}

func TestGitRefsStore_PerCheckpointHistory(t *testing.T) {
	t.Parallel()
	store := newRefsStore(t)
	ctx := context.Background()
	cid := id.MustCheckpointID("a1b2c3d4e5f6")

	refsWrite(t, store, cid, "sess-1", "t")

	// First write is an orphan (no parent).
	ref, err := store.repo.Reference(mustRefName(t, cid), true)
	require.NoError(t, err)
	first, err := store.repo.CommitObject(ref.Hash())
	require.NoError(t, err)
	require.Empty(t, first.ParentHashes, "first checkpoint commit should be an orphan")

	// A backfill advances the same ref, preserving history.
	require.NoError(t, store.Write(ctx, SessionSummary{
		CheckpointID: cid, Summary: &Summary{Intent: "later"},
	}))
	ref, err = store.repo.Reference(mustRefName(t, cid), true)
	require.NoError(t, err)
	second, err := store.repo.CommitObject(ref.Hash())
	require.NoError(t, err)
	require.Len(t, second.ParentHashes, 1, "backfill should parent on the prior tip")
	assert.Equal(t, first.Hash, second.ParentHashes[0])
}

func TestGitRefsStore_GetCheckpointAuthor(t *testing.T) {
	t.Parallel()
	store := newRefsStore(t)
	ctx := context.Background()
	cid := id.MustCheckpointID("a1b2c3d4e5f6")

	refsWrite(t, store, cid, "sess-1", "t")

	author, err := store.GetCheckpointAuthor(ctx, cid)
	require.NoError(t, err)
	assert.Equal(t, "Test Author", author.Name)
	assert.Equal(t, "test@example.com", author.Email)
}

func TestGitRefsStore_BackfillUnknownCheckpointNotFound(t *testing.T) {
	t.Parallel()
	store := newRefsStore(t)
	ctx := context.Background()
	cid := id.MustCheckpointID("a1b2c3d4e5f6")

	err := store.Write(ctx, SessionTranscript{
		CheckpointID: cid, SessionID: "s",
		Transcript: redact.AlreadyRedacted([]byte("x")),
	})
	require.ErrorIs(t, err, ErrCheckpointNotFound)

	err = store.Write(ctx, SessionSummary{CheckpointID: cid, Summary: &Summary{Intent: "x"}})
	require.ErrorIs(t, err, ErrCheckpointNotFound)

	err = store.Write(ctx, CheckpointAttribution{CheckpointID: cid, Attribution: &Attribution{AgentLines: 1}})
	require.ErrorIs(t, err, ErrCheckpointNotFound)

	// Read of an absent checkpoint is (nil, nil) per the contract.
	summary, err := store.Read(ctx, cid)
	require.NoError(t, err)
	assert.Nil(t, summary)
}
