package checkpoint

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/redact"
)

// flakyReadStore wraps a real store and fails Read, leaving every other
// operation intact. It stands in for a backend whose on-demand fetch is
// temporarily unavailable.
type flakyReadStore struct {
	PersistentStore

	readErr error
	writes  int
}

func (s *flakyReadStore) Read(ctx context.Context, checkpointID id.CheckpointID) (*CheckpointSummary, error) {
	if s.readErr != nil {
		return nil, s.readErr
	}
	return s.PersistentStore.Read(ctx, checkpointID)
}

func (s *flakyReadStore) Write(ctx context.Context, req WriteRequest) error {
	s.writes++
	return s.PersistentStore.Write(ctx, req)
}

// countingStore records the write requests it receives so a test can assert
// which backend a write reached.
type countingStore struct {
	PersistentStore

	requests []WriteRequest
}

func (s *countingStore) Write(ctx context.Context, req WriteRequest) error {
	s.requests = append(s.requests, req)
	return s.PersistentStore.Write(ctx, req)
}

func reservedRoutingRequest(checkpointID id.CheckpointID) ReservedSession {
	return reservedRoutingRequestWith(checkpointID, "reserved-routing", "reserved transcript")
}

func reservedRoutingRequestWith(checkpointID id.CheckpointID, sessionID, transcript string) ReservedSession {
	return ReservedSession(WriteOptions{
		CheckpointID: checkpointID,
		SessionID:    sessionID,
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(transcript)),
		AuthorName:   "Test",
		AuthorEmail:  "test@example.com",
	})
}

func batchRoutingRequest(checkpointID id.CheckpointID) BatchSessions {
	return BatchSessions{
		CheckpointID: checkpointID,
		Sessions: []ReservedSession{
			reservedRoutingRequest(checkpointID),
		},
		CommitTime:  batchTestTime,
		AuthorName:  "Test",
		AuthorEmail: "test@example.com",
	}
}

// TestKindRoutingStore_ReservedSessionProbeFailurePublishesNowhere keeps a
// transient read failure from publishing a one-sided migrated write.
//
// writeReservedRequest reads the read-preferred backend first, to find out
// whether a migrated copy also needs updating. Treating that read's error as
// non-fatal could leave the copy stale and read-preferred after recovery.
func TestKindRoutingStore_ReservedSessionProbeFailurePublishesNowhere(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	checkpointID := id.MustCheckpointID("a1b2c3d4e5f6")

	_, repo, _ := newTestRepo(t)
	branch := NewGitStore(repo, DefaultV1Refs())
	refs := &flakyReadStore{PersistentStore: newGitRefsStore(repo), readErr: errors.New("refs fetch unavailable")}

	router := newKindRoutingStore(refs, branch, refs, BackendTypeGitRefs)
	err := router.Write(ctx, reservedRoutingRequest(checkpointID))
	require.ErrorContains(t, err, "probe read-preferred backend")

	summary, err := branch.Read(ctx, checkpointID)
	require.NoError(t, err)
	assert.Nil(t, summary, "a failed read-preferred probe must not publish the original-backend write")
}

func TestKindRoutingStore_ReservedSessionProbeFailurePreservesExistingCopies(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	checkpointID := id.MustCheckpointID("b1b2c3d4e5f6")

	_, repo, _ := newTestRepo(t)
	branch := NewGitStore(repo, DefaultV1Refs())
	refsBase := newGitRefsStore(repo)
	writeRoutingCheckpoint(t, branch, checkpointID, "old-session")
	writeRoutingCheckpoint(t, refsBase, checkpointID, "old-session")
	refs := &flakyReadStore{PersistentStore: refsBase, readErr: errors.New("refs fetch unavailable")}
	router := newKindRoutingStore(refs, branch, refs, BackendTypeGitRefs)

	err := router.Write(ctx, reservedRoutingRequest(checkpointID))
	require.ErrorContains(t, err, "probe read-preferred backend")

	refs.readErr = nil
	branchContent, err := branch.ReadSessionContent(ctx, checkpointID, 0)
	require.NoError(t, err)
	assert.Contains(t, string(branchContent.Transcript), "old-session")
	refsContent, err := refsBase.ReadSessionContent(ctx, checkpointID, 0)
	require.NoError(t, err)
	assert.Contains(t, string(refsContent.Transcript), "old-session")

	require.NoError(t, router.Write(ctx, reservedRoutingRequestWith(checkpointID, "old-session", "new transcript")))
	summary, err := router.Read(ctx, checkpointID)
	require.NoError(t, err)
	require.NotNil(t, summary)
	assert.Len(t, summary.Sessions, 1)
	latestIndex := len(summary.Sessions) - 1
	visibleContent, err := router.ReadSessionContent(ctx, checkpointID, latestIndex)
	require.NoError(t, err)
	assert.Contains(t, string(visibleContent.Transcript), "new transcript")
}

// TestKindRoutingStore_ReservedSessionFallsBackToWriterForUnknownPrimary keeps an
// unrecognised primary from being bypassed.
//
// writeReservedRequest compares the ID-derived backend against primaryType, so
// when primaryType is neither git-branch nor git-refs the comparison never
// matches and every reserved write skips s.writer — the configured primary and
// all its mirrors. readOrder already has an explicit default arm for this case;
// the write path should be at least as careful.
func TestKindRoutingStore_ReservedSessionFallsBackToWriterForUnknownPrimary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	checkpointID := id.MustCheckpointID("a1b2c3d4e5f6")

	_, repo, _ := newTestRepo(t)
	branch := NewGitStore(repo, DefaultV1Refs())
	refs := newGitRefsStore(repo)
	writer := &countingStore{PersistentStore: branch}

	router := newKindRoutingStore(writer, branch, refs, "some-future-git-backend")
	require.NoError(t, router.Write(ctx, reservedRoutingRequest(checkpointID)))

	assert.Len(t, writer.requests, 1,
		"a reserved write under an unrecognised primary must still go through the configured writer")
}

// TestKindRoutingStore_FreshSessionWriteIsNotKindRouted pins the documented
// contract that creates go to the configured primary. It is the counterpart to
// TestKindRoutingStore_ReservedSessionUsesOriginalBackendAfterConfigChange: only
// a reserved ID may be relocated by its format.
func TestKindRoutingStore_FreshSessionWriteIsNotKindRouted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	checkpointID := id.MustCheckpointID("a1b2c3d4e5f6")

	_, repo, _ := newTestRepo(t)
	branch := NewGitStore(repo, DefaultV1Refs())
	refs := newGitRefsStore(repo)
	writer := &countingStore{PersistentStore: refs}

	router := newKindRoutingStore(writer, branch, refs, BackendTypeGitRefs)
	writeRoutingCheckpoint(t, router, checkpointID, "fresh-hex-under-refs-primary")

	require.Len(t, writer.requests, 1, "a fresh create must go through the configured primary")

	onBranch, err := branch.Read(ctx, checkpointID)
	require.NoError(t, err)
	assert.Nil(t, onBranch, "a fresh create must not be relocated to the other backend by ID format")
}

func TestKindRoutingStore_BatchProbeFailurePublishesNowhere(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	checkpointID := id.MustCheckpointID("a1b2c3d4e5f6")

	_, repo, _ := newTestRepo(t)
	branch := NewGitStore(repo, DefaultV1Refs())
	refs := &flakyReadStore{PersistentStore: newGitRefsStore(repo), readErr: errors.New("refs fetch unavailable")}
	router := newKindRoutingStore(refs, branch, refs, BackendTypeGitRefs)

	err := router.Write(ctx, batchRoutingRequest(checkpointID))
	require.ErrorContains(t, err, "probe read-preferred backend")
	assert.Zero(t, refs.writes, "the read-preferred backend was written after its probe failed")
	summary, readErr := branch.Read(ctx, checkpointID)
	require.NoError(t, readErr)
	assert.Nil(t, summary, "the original backend was written before the probe succeeded")
}

func TestClassifyWriteRequest_CoversSealedUnion(t *testing.T) {
	t.Parallel()
	checkpointID := id.MustCheckpointID("a1b2c3d4e5f6")
	tests := []struct {
		name string
		req  WriteRequest
		want writeRequestClass
	}{
		{"Session", Session{CheckpointID: checkpointID, SessionID: "session"}, writeRequestCreate},
		{"ReservedSession", reservedRoutingRequest(checkpointID), writeRequestReserved},
		{"BatchSessions", batchRoutingRequest(checkpointID), writeRequestReserved},
		{"SessionTranscript", SessionTranscript{CheckpointID: checkpointID}, writeRequestBackfill},
		{"SessionSummary", SessionSummary{CheckpointID: checkpointID}, writeRequestBackfill},
		{"CheckpointAttribution", CheckpointAttribution{CheckpointID: checkpointID}, writeRequestBackfill},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, gotID, normalized, err := classifyWriteRequest(tt.req)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, checkpointID, gotID)
			assert.IsType(t, tt.req, normalized)
		})
	}
}
