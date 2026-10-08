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
	return ReservedSession(WriteOptions{
		CheckpointID: checkpointID,
		SessionID:    "reserved-routing",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte("reserved transcript")),
		AuthorName:   "Test",
		AuthorEmail:  "test@example.com",
	})
}

// TestKindRoutingStore_ReservedSessionSurvivesReadTargetFailure keeps a
// transient read failure in one backend from losing a retry.
//
// writeReservedSession reads the read-preferred backend first, to find out
// whether a migrated copy also needs updating. Treating that read's error as
// fatal fails the whole condensation, even though the write it was about to make
// would have been perfectly visible: under a git-refs primary, readOrder for a
// hex ID is [refs, branch], and firstResolved falls through a non-final store
// that errors. So a branch-only write still resolves.
func TestKindRoutingStore_ReservedSessionSurvivesReadTargetFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	checkpointID := id.MustCheckpointID("a1b2c3d4e5f6")

	_, repo, _ := newTestRepo(t)
	branch := NewGitStore(repo, DefaultV1Refs())
	refs := &flakyReadStore{PersistentStore: newGitRefsStore(repo), readErr: errors.New("refs fetch unavailable")}

	router := newKindRoutingStore(refs, branch, refs, BackendTypeGitRefs)
	require.NoError(t, router.Write(ctx, reservedRoutingRequest(checkpointID)),
		"a read failure in the other backend must not fail the reserved write")

	summary, err := branch.Read(ctx, checkpointID)
	require.NoError(t, err)
	require.NotNil(t, summary, "the reserved write must land on the backend its ID belongs to")
}

// TestKindRoutingStore_ReservedSessionFallsBackToWriterForUnknownPrimary keeps an
// unrecognised primary from being bypassed.
//
// writeReservedSession compares the ID-derived backend against primaryType, so
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
