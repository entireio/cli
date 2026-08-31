package checkpoint

import (
	"context"
	"errors"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/redact"
)

const routingSampleULID = "01KVBJCWYA4YW6J5M9GP655HZN"

// writeRoutingCheckpoint writes a minimal one-session checkpoint to store.
func writeRoutingCheckpoint(t *testing.T, store PersistentStore, cid id.CheckpointID, sessionID string) {
	t.Helper()
	require.NoError(t, store.Write(context.Background(), Session{
		CheckpointID: cid,
		SessionID:    sessionID,
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte("transcript for " + sessionID)),
		AuthorName:   "Test",
		AuthorEmail:  "test@example.com",
	}))
}

func TestKindRoutingStore_Read(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	hexID := id.MustCheckpointID("a1b2c3d4e5f6")
	ulidID := id.MustCheckpointID(routingSampleULID)

	t.Run("git-branch primary: hex on branch, ULID still from refs", func(t *testing.T) {
		t.Parallel()
		_, repo, _ := newTestRepo(t)
		branch := NewGitStore(repo, DefaultV1Refs())
		refs := newGitRefsStore(repo)
		writeRoutingCheckpoint(t, branch, hexID, "hex-on-branch")
		writeRoutingCheckpoint(t, refs, ulidID, "ulid-in-refs")

		router := newKindRoutingStore(branch, branch, refs, BackendTypeGitBranch)

		got, err := router.Read(ctx, hexID)
		require.NoError(t, err)
		require.NotNil(t, got, "hex checkpoint should resolve from the branch")

		got, err = router.Read(ctx, ulidID)
		require.NoError(t, err)
		require.NotNil(t, got, "ULID checkpoint should resolve from refs even under a git-branch primary")
	})

	t.Run("git-refs primary: ULID from refs, pre-migration hex from branch fallback", func(t *testing.T) {
		t.Parallel()
		_, repo, _ := newTestRepo(t)
		branch := NewGitStore(repo, DefaultV1Refs())
		refs := newGitRefsStore(repo)
		writeRoutingCheckpoint(t, branch, hexID, "hex-on-branch")
		writeRoutingCheckpoint(t, refs, ulidID, "ulid-in-refs")

		router := newKindRoutingStore(refs, branch, refs, BackendTypeGitRefs)

		got, err := router.Read(ctx, ulidID)
		require.NoError(t, err)
		require.NotNil(t, got)

		got, err = router.Read(ctx, hexID)
		require.NoError(t, err)
		require.NotNil(t, got, "hex checkpoint on the branch should resolve via fallback under a git-refs primary")
	})

	t.Run("git-refs primary: migrated hex in refs resolves from refs first", func(t *testing.T) {
		t.Parallel()
		_, repo, _ := newTestRepo(t)
		branch := NewGitStore(repo, DefaultV1Refs())
		refs := newGitRefsStore(repo)
		migratedHex := id.MustCheckpointID("ffffffffeeee")
		writeRoutingCheckpoint(t, refs, migratedHex, "hex-migrated-to-refs")

		router := newKindRoutingStore(refs, branch, refs, BackendTypeGitRefs)

		got, err := router.Read(ctx, migratedHex)
		require.NoError(t, err)
		require.NotNil(t, got, "a hex checkpoint migrated into refs should resolve under a git-refs primary")
	})

	t.Run("git-refs primary: a refs fetch error falls back to the branch", func(t *testing.T) {
		t.Parallel()
		_, repo, _ := newTestRepo(t)
		branch := NewGitStore(repo, DefaultV1Refs())
		refs := newGitRefsStore(repo)
		// A missing local ref triggers an on-demand fetch; simulate that fetch
		// failing (network down) so the refs read returns a hard error rather than
		// ErrCheckpointNotFound.
		refs.SetRefFetcher(func(context.Context, plumbing.ReferenceName) error {
			return errors.New("network down")
		})
		writeRoutingCheckpoint(t, branch, hexID, "hex-on-branch")

		router := newKindRoutingStore(refs, branch, refs, BackendTypeGitRefs)

		got, err := router.Read(ctx, hexID)
		require.NoError(t, err, "a refs fetch error must not block the branch fallback")
		require.NotNil(t, got, "hex checkpoint on the branch should resolve even when the refs read errors")
	})

	t.Run("a ULID is never read from the branch", func(t *testing.T) {
		t.Parallel()
		_, repo, _ := newTestRepo(t)
		branch := NewGitStore(repo, DefaultV1Refs())
		refs := newGitRefsStore(repo)
		// Deliberately put a ULID-named checkpoint on the branch (the wrong place)
		// and nothing in refs; routing must not find it, proving branch is never
		// consulted for a ULID.
		writeRoutingCheckpoint(t, branch, ulidID, "stray-ulid-on-branch")

		router := newKindRoutingStore(branch, branch, refs, BackendTypeGitBranch)

		got, err := router.Read(ctx, ulidID)
		require.NoError(t, err)
		assert.Nil(t, got, "a ULID must be read only from refs; a stray ULID on the branch must not resolve")
	})
}

func TestKindRoutingStore_SessionReadRoutes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, repo, _ := newTestRepo(t)
	branch := NewGitStore(repo, DefaultV1Refs())
	refs := newGitRefsStore(repo)

	ulidID := id.MustCheckpointID(routingSampleULID)
	writeRoutingCheckpoint(t, refs, ulidID, "ulid-in-refs")

	router := newKindRoutingStore(branch, branch, refs, BackendTypeGitBranch)

	meta, err := router.ReadSessionMetadata(ctx, ulidID, 0)
	require.NoError(t, err)
	require.NotNil(t, meta, "session metadata for a ULID checkpoint should route to refs")
	assert.Equal(t, "ulid-in-refs", meta.SessionID)
}

func TestKindRoutingStore_ReservedSessionUsesOriginalBackendAfterConfigChange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	tests := []struct {
		name        string
		primaryType string
		checkpoint  id.CheckpointID
		wantBackend string
	}{
		{
			name:        "ULID reserved under refs after switching to branch",
			primaryType: BackendTypeGitBranch,
			checkpoint:  id.MustCheckpointID(routingSampleULID),
			wantBackend: BackendTypeGitRefs,
		},
		{
			name:        "hex reserved under branch after switching to refs",
			primaryType: BackendTypeGitRefs,
			checkpoint:  id.MustCheckpointID("a1b2c3d4e5f6"),
			wantBackend: BackendTypeGitBranch,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, repo, _ := newTestRepo(t)
			branch := NewGitStore(repo, DefaultV1Refs())
			refs := newGitRefsStore(repo)
			var writer PersistentStore = branch
			if tt.primaryType == BackendTypeGitRefs {
				writer = refs
			}
			router := newKindRoutingStore(writer, branch, refs, tt.primaryType)

			req := ReservedSession(WriteOptions{
				CheckpointID: tt.checkpoint,
				SessionID:    "reserved-session",
				Strategy:     "manual-commit",
				Transcript:   redact.AlreadyRedacted([]byte("reserved transcript")),
				AuthorName:   "Test",
				AuthorEmail:  "test@example.com",
			})
			require.NoError(t, router.Write(ctx, req))

			branchSummary, err := branch.Read(ctx, tt.checkpoint)
			require.NoError(t, err)
			refsSummary, err := refs.Read(ctx, tt.checkpoint)
			require.NoError(t, err)
			if tt.wantBackend == BackendTypeGitRefs {
				require.NotNil(t, refsSummary)
				assert.Nil(t, branchSummary)
			} else {
				require.NotNil(t, branchSummary)
				assert.Nil(t, refsSummary)
			}
		})
	}
}

func TestKindRoutingStore_ReservedSessionRetryUpdatesBothBackendsOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	checkpointID := id.MustCheckpointID("a1b2c3d4e5f6")

	_, repo, _ := newTestRepo(t)
	branch := NewGitStore(repo, DefaultV1Refs())
	refs := newGitRefsStore(repo)
	writeRoutingCheckpoint(t, branch, checkpointID, "reserved-session")
	writeRoutingCheckpoint(t, refs, checkpointID, "reserved-session")

	branchBefore, err := ReadRefHash(branch.repo, branch.refs.Primary)
	require.NoError(t, err)
	writer := newFanoutStore(refs, []Writer{branch})
	router := newKindRoutingStore(writer, branch, refs, BackendTypeGitRefs)
	require.NoError(t, router.Write(ctx, ReservedSession(WriteOptions{
		CheckpointID: checkpointID,
		SessionID:    "reserved-session",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte("new transcript from retry")),
		AuthorName:   "Test",
		AuthorEmail:  "test@example.com",
	})))

	branchContent, err := branch.ReadSessionContent(ctx, checkpointID, 0)
	require.NoError(t, err)
	require.Contains(t, string(branchContent.Transcript), "new transcript from retry")

	visibleContent, err := router.ReadSessionContent(ctx, checkpointID, 0)
	require.NoError(t, err)
	require.Contains(t, string(visibleContent.Transcript), "new transcript from retry",
		"a successful retry must be visible through normal refs-first reads")

	branchAfter, err := ReadRefHash(branch.repo, branch.refs.Primary)
	require.NoError(t, err)
	commit, err := repo.CommitObject(branchAfter)
	require.NoError(t, err)
	require.Equal(t, []plumbing.Hash{branchBefore}, commit.ParentHashes,
		"the required original-backend write must not be repeated by mirror fan-out")
}

func TestKindRoutingStore_BatchSessionsUsesOriginalBackendAfterConfigChange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tests := []struct {
		name        string
		primaryType string
		checkpoint  id.CheckpointID
		wantBackend string
	}{
		{
			name:        "ULID batch under refs after switching to branch",
			primaryType: BackendTypeGitBranch,
			checkpoint:  id.MustCheckpointID(routingSampleULID),
			wantBackend: BackendTypeGitRefs,
		},
		{
			name:        "hex batch under branch after switching to refs",
			primaryType: BackendTypeGitRefs,
			checkpoint:  id.MustCheckpointID("a1b2c3d4e5f6"),
			wantBackend: BackendTypeGitBranch,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, repo, _ := newTestRepo(t)
			branch := NewGitStore(repo, DefaultV1Refs())
			refs := newGitRefsStore(repo)
			var writer PersistentStore = branch
			if tt.primaryType == BackendTypeGitRefs {
				writer = refs
			}
			router := newKindRoutingStore(writer, branch, refs, tt.primaryType)
			require.NoError(t, router.Write(ctx, batchRoutingRequest(tt.checkpoint)))

			branchSummary, err := branch.Read(ctx, tt.checkpoint)
			require.NoError(t, err)
			refsSummary, err := refs.Read(ctx, tt.checkpoint)
			require.NoError(t, err)
			if tt.wantBackend == BackendTypeGitRefs {
				require.NotNil(t, refsSummary)
				assert.Nil(t, branchSummary)
			} else {
				require.NotNil(t, branchSummary)
				assert.Nil(t, refsSummary)
			}
		})
	}
}

func TestKindRoutingStore_ListUnionsBothBackends(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, repo, _ := newTestRepo(t)
	branch := NewGitStore(repo, DefaultV1Refs())
	refs := newGitRefsStore(repo)

	hexID := id.MustCheckpointID("a1b2c3d4e5f6")
	ulidID := id.MustCheckpointID(routingSampleULID)
	writeRoutingCheckpoint(t, branch, hexID, "hex")
	writeRoutingCheckpoint(t, refs, ulidID, "ulid")

	router := newKindRoutingStore(branch, branch, refs, BackendTypeGitBranch)

	infos, err := router.List(ctx)
	require.NoError(t, err)
	seen := make(map[string]bool, len(infos))
	for _, info := range infos {
		seen[info.CheckpointID.String()] = true
	}
	assert.True(t, seen[hexID.String()], "list should include the hex checkpoint from the branch")
	assert.True(t, seen[ulidID.String()], "list should include the ULID checkpoint from refs")
}

func TestKindRoutingStore_ListDedupesAcrossBackends(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, repo, _ := newTestRepo(t)
	branch := NewGitStore(repo, DefaultV1Refs())
	refs := newGitRefsStore(repo)

	// The same checkpoint present in BOTH backends (as happens for a mirrored
	// checkpoint or a migrated one) must appear only once in the merged list.
	dupID := id.MustCheckpointID("a1b2c3d4e5f6")
	writeRoutingCheckpoint(t, branch, dupID, "on-branch")
	writeRoutingCheckpoint(t, refs, dupID, "in-refs")

	router := newKindRoutingStore(branch, branch, refs, BackendTypeGitRefs)

	infos, err := router.List(ctx)
	require.NoError(t, err)
	count := 0
	for _, info := range infos {
		if info.CheckpointID == dupID {
			count++
		}
	}
	assert.Equal(t, 1, count, "a checkpoint present in both backends should appear once")
}

func TestKindRoutingStore_SummaryBackfillFallsBackToBranch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Regression: `entire checkpoint explain --generate <hex-id>` under a
	// git-refs primary. The checkpoint resolves via the branch read fallback,
	// but the summary write went only to the refs primary, which has no ref
	// for it, so the generated summary was discarded with ErrCheckpointNotFound.
	hexID := id.MustCheckpointID("a1b2c3d4e5f6")
	_, repo, _ := newTestRepo(t)
	branch := NewGitStore(repo, DefaultV1Refs())
	refs := newGitRefsStore(repo)
	writeRoutingCheckpoint(t, branch, hexID, "hex-on-branch")

	router := newKindRoutingStore(refs, branch, refs, BackendTypeGitRefs)

	err := router.Write(ctx, SessionSummary{
		CheckpointID: hexID,
		Summary:      &Summary{Intent: "test intent", Outcome: "test outcome"},
	})
	require.NoError(t, err, "summary backfill for a pre-migration hex checkpoint must fall back to the branch store")

	meta, err := router.ReadSessionMetadata(ctx, hexID, 0)
	require.NoError(t, err)
	require.NotNil(t, meta.Summary, "backfilled summary should be readable back through the router")
	assert.Equal(t, "test intent", meta.Summary.Intent)
}

func TestKindRoutingStore_TranscriptAndAttributionBackfillsFallBackToBranch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	hexID := id.MustCheckpointID("a1b2c3d4e5f6")
	_, repo, _ := newTestRepo(t)
	branch := NewGitStore(repo, DefaultV1Refs())
	refs := newGitRefsStore(repo)
	writeRoutingCheckpoint(t, branch, hexID, "hex-on-branch")

	router := newKindRoutingStore(refs, branch, refs, BackendTypeGitRefs)

	err := router.Write(ctx, SessionTranscript{
		CheckpointID: hexID,
		SessionID:    "hex-on-branch",
		Transcript:   redact.AlreadyRedacted([]byte("finalized transcript")),
	})
	require.NoError(t, err, "transcript backfill for a hex checkpoint on the branch must fall back to the branch store")

	err = router.Write(ctx, CheckpointAttribution{
		CheckpointID: hexID,
		Attribution:  &Attribution{AgentLines: 7},
	})
	require.NoError(t, err, "attribution backfill for a hex checkpoint on the branch must fall back to the branch store")
}

func TestKindRoutingStore_BackfillULIDRoutesToRefs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// A ULID checkpoint only ever lives in refs, so its backfill must reach the
	// refs store even when the configured primary is git-branch.
	ulidID := id.MustCheckpointID(routingSampleULID)
	_, repo, _ := newTestRepo(t)
	branch := NewGitStore(repo, DefaultV1Refs())
	refs := newGitRefsStore(repo)
	writeRoutingCheckpoint(t, refs, ulidID, "ulid-in-refs")

	router := newKindRoutingStore(branch, branch, refs, BackendTypeGitBranch)

	err := router.Write(ctx, SessionSummary{
		CheckpointID: ulidID,
		Summary:      &Summary{Intent: "ulid intent"},
	})
	require.NoError(t, err, "summary backfill for a ULID checkpoint must route to refs under a git-branch primary")

	meta, err := router.ReadSessionMetadata(ctx, ulidID, 0)
	require.NoError(t, err)
	require.NotNil(t, meta.Summary)
	assert.Equal(t, "ulid intent", meta.Summary.Intent)
}

func TestKindRoutingStore_BackfillMissingEverywhereReturnsNotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	_, repo, _ := newTestRepo(t)
	branch := NewGitStore(repo, DefaultV1Refs())
	refs := newGitRefsStore(repo)

	router := newKindRoutingStore(refs, branch, refs, BackendTypeGitRefs)

	err := router.Write(ctx, SessionSummary{
		CheckpointID: id.MustCheckpointID("a1b2c3d4e5f6"),
		Summary:      &Summary{Intent: "orphan"},
	})
	require.ErrorIs(t, err, ErrCheckpointNotFound, "a backfill for a checkpoint absent from every backend still reports not-found")
}

func TestKindRoutingStore_BackfillMirrorFanout(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	hexID := id.MustCheckpointID("a1b2c3d4e5f6")

	t.Run("backfill landing on the primary still fans out to mirrors", func(t *testing.T) {
		t.Parallel()
		_, repo, _ := newTestRepo(t)
		branch := NewGitStore(repo, DefaultV1Refs())
		refs := newGitRefsStore(repo)
		writeRoutingCheckpoint(t, refs, hexID, "hex-migrated-to-refs")

		mirror := &fakeMirror{}
		writer := newFanoutStore(refs, []Writer{mirror})
		router := newKindRoutingStore(writer, branch, refs, BackendTypeGitRefs)

		req := SessionSummary{CheckpointID: hexID, Summary: &Summary{Intent: "mirrored"}}
		require.NoError(t, router.Write(ctx, req))
		assert.Len(t, mirror.writes, 1, "a backfill served by the primary should reach mirrors")
	})

	t.Run("backfill falling back to the branch skips mirrors", func(t *testing.T) {
		t.Parallel()
		_, repo, _ := newTestRepo(t)
		branch := NewGitStore(repo, DefaultV1Refs())
		refs := newGitRefsStore(repo)
		writeRoutingCheckpoint(t, branch, hexID, "hex-on-branch")

		mirror := &fakeMirror{}
		writer := newFanoutStore(refs, []Writer{mirror})
		router := newKindRoutingStore(writer, branch, refs, BackendTypeGitRefs)

		req := SessionSummary{CheckpointID: hexID, Summary: &Summary{Intent: "not mirrored"}}
		require.NoError(t, router.Write(ctx, req))
		assert.Empty(t, mirror.writes, "a backfill served by a fallback store must not fan out to mirrors (mirrors follow the primary)")
	})
}

func TestKindRoutingStore_BackfillDoesNotCreateSessionsBranchOnMiss(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// A refs-only repo has no v1 branch. Probing the branch store for a hex
	// checkpoint that exists nowhere (e.g. a typo'd `explain --generate <hex>`)
	// must report not-found WITHOUT creating the sessions branch as a side
	// effect — the branch would otherwise become live (List union, pre-push).
	_, repo, _ := newTestRepo(t)
	branch := NewGitStore(repo, DefaultV1Refs())
	refs := newGitRefsStore(repo)

	router := newKindRoutingStore(refs, branch, refs, BackendTypeGitRefs)

	err := router.Write(ctx, SessionSummary{
		CheckpointID: id.MustCheckpointID("a1b2c3d4e5f6"),
		Summary:      &Summary{Intent: "orphan"},
	})
	require.ErrorIs(t, err, ErrCheckpointNotFound)

	_, refErr := repo.Reference(DefaultV1Refs().Primary, true)
	require.ErrorIs(t, refErr, plumbing.ErrReferenceNotFound,
		"a backfill miss must not create the sessions branch")
}

func TestKindRoutingStore_BackfillHardErrorAbortsFallthrough(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Only ErrCheckpointNotFound may fall through — deliberately stricter than
	// read routing, which falls through on any error. Redirecting a write to
	// another backend after a transient primary failure could fork the data:
	// the checkpoint also exists on the branch here, and the backfill must NOT
	// reach it when the primary failed hard.
	hexID := id.MustCheckpointID("a1b2c3d4e5f6")
	_, repo, _ := newTestRepo(t)
	branch := NewGitStore(repo, DefaultV1Refs())
	writeRoutingCheckpoint(t, branch, hexID, "hex-on-branch")

	failing := &fakePrimary{writeErr: errors.New("refs backend io error")}
	router := newKindRoutingStore(failing, branch, failing, BackendTypeGitRefs)

	err := router.Write(ctx, SessionSummary{
		CheckpointID: hexID,
		Summary:      &Summary{Intent: "must not land"},
	})
	require.ErrorContains(t, err, "refs backend io error", "a hard primary error must surface verbatim")

	meta, readErr := branch.ReadSessionMetadata(ctx, hexID, 0)
	require.NoError(t, readErr)
	require.Nil(t, meta.Summary, "the fallback store must not receive a write after a hard primary error")
}

func TestKindRoutingStore_BackfillPrefersRefsWhenInBothBackends(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// A migrated hex checkpoint can coexist in both backends. The backfill must
	// land on refs (read order under a refs primary), because refs-first reads
	// would never surface a summary written to the branch copy.
	hexID := id.MustCheckpointID("a1b2c3d4e5f6")
	_, repo, _ := newTestRepo(t)
	branch := NewGitStore(repo, DefaultV1Refs())
	refs := newGitRefsStore(repo)
	writeRoutingCheckpoint(t, branch, hexID, "hex-on-branch")
	writeRoutingCheckpoint(t, refs, hexID, "hex-migrated-to-refs")

	router := newKindRoutingStore(refs, branch, refs, BackendTypeGitRefs)

	require.NoError(t, router.Write(ctx, SessionSummary{
		CheckpointID: hexID,
		Summary:      &Summary{Intent: "lands on refs"},
	}))

	meta, err := router.ReadSessionMetadata(ctx, hexID, 0)
	require.NoError(t, err)
	require.NotNil(t, meta.Summary, "refs-first reads must see the backfilled summary")
	assert.Equal(t, "lands on refs", meta.Summary.Intent)

	branchMeta, err := branch.ReadSessionMetadata(ctx, hexID, 0)
	require.NoError(t, err)
	assert.Nil(t, branchMeta.Summary, "the branch copy must not have received the backfill")
}

// TestKindRoutingStore_ListSurfacesRemoteDiscovery proves the git-refs remote
// discovery flows through the routing store: the routing List delegates to the
// refs store with the caller's context, so a WithRemoteListDiscovery context
// makes a checkpoint present only on the remote appear in the unioned list.
func TestKindRoutingStore_ListSurfacesRemoteDiscovery(t *testing.T) {
	t.Parallel()
	_, repo, _ := newTestRepo(t)
	branch := NewGitStore(repo, DefaultV1Refs())
	refs := newGitRefsStore(repo)

	localULID := id.MustCheckpointID(routingSampleULID)
	writeRoutingCheckpoint(t, refs, localULID, "ulid-in-refs")

	// A different ULID that lives only on the remote (no local ref).
	remoteOnly := id.MustCheckpointID("01KVBJCWYA4YW6J5M9GP655HYY")
	refs.SetRemoteRefLister(func(context.Context) ([]plumbing.ReferenceName, error) {
		return []plumbing.ReferenceName{mustRefName(t, remoteOnly)}, nil
	})

	router := newKindRoutingStore(refs, branch, refs, BackendTypeGitRefs)

	infos, err := router.List(WithRemoteListDiscovery(context.Background()))
	require.NoError(t, err)
	seen := make(map[id.CheckpointID]struct{}, len(infos))
	for _, info := range infos {
		seen[info.CheckpointID] = struct{}{}
	}
	assert.Contains(t, seen, localULID, "local refs checkpoint is listed")
	assert.Contains(t, seen, remoteOnly, "remote-only checkpoint is discovered through the routing store")
}

func TestKindRoutingStore_GetCheckpointAuthorRoutes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, repo, _ := newTestRepo(t)
	branch := NewGitStore(repo, DefaultV1Refs())
	refs := newGitRefsStore(repo)

	ulidID := id.MustCheckpointID(routingSampleULID)
	writeRoutingCheckpoint(t, refs, ulidID, "ulid-in-refs")

	router := newKindRoutingStore(branch, branch, refs, BackendTypeGitBranch)
	author, ok := router.(AuthorReader)
	require.True(t, ok, "routing store over git backends should expose AuthorReader")

	got, err := author.GetCheckpointAuthor(ctx, ulidID)
	require.NoError(t, err)
	assert.Equal(t, "Test", got.Name, "author of a ULID checkpoint should route to refs")
}
