package checkpoint

import (
	"context"
	"errors"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/redact"
)

const firstSummaryIntent = "first summary"

// Note: the Write dispatcher's default ("unsupported request") branch is no
// longer reachable from this package — the WriteRequest union is sealed to the
// api/checkpoint contract, so an unhandled request type can only be introduced
// there. The per-request dispatch below is the meaningful coverage.

// TestWrite_DispatchesEachRequest verifies that Store.Write routes each request
// type to the corresponding git operation, observing the effect of each.
func TestWrite_DispatchesEachRequest(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	ctx := context.Background()
	cpID := id.MustCheckpointID("a1b2c3d4e5f6")

	// Session materializes the checkpoint on first session.
	if err := store.Write(ctx, Session{
		CheckpointID: cpID,
		SessionID:    "session-001",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte("provisional\n")),
		Prompts:      []string{"initial"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	}); err != nil {
		t.Fatalf("Write(Session) error = %v", err)
	}
	summary, err := store.Read(ctx, cpID)
	if err != nil || summary == nil {
		t.Fatalf("checkpoint not created by Session: summary=%v err=%v", summary, err)
	}

	// ReservedSession uses the same builder through its distinct dispatch arm.
	reservedID := id.MustCheckpointID("b1b2c3d4e5f6")
	if err := store.Write(ctx, ReservedSession{
		CheckpointID: reservedID,
		SessionID:    "reserved-session",
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	}); err != nil {
		t.Fatalf("Write(ReservedSession) error = %v", err)
	}
	reservedSummary, err := store.Read(ctx, reservedID)
	if err != nil || reservedSummary == nil {
		t.Fatalf("checkpoint not created by ReservedSession: summary=%v err=%v", reservedSummary, err)
	}

	// BatchSessions is a checkpoint-level write with canonical multi-session
	// semantics, not an alias of ReservedSession dispatch.
	batchID := id.MustCheckpointID("c1b2c3d4e5f6")
	if err := store.Write(ctx, BatchSessions{
		CheckpointID: batchID,
		Sessions: []ReservedSession{
			{CheckpointID: batchID, SessionID: "session-b"},
			{CheckpointID: batchID, SessionID: "session-a"},
		},
		AuthorName:  "Test",
		AuthorEmail: "test@test.com",
	}); err != nil {
		t.Fatalf("Write(BatchSessions) error = %v", err)
	}
	batchSummary, err := store.Read(ctx, batchID)
	if err != nil || batchSummary == nil || len(batchSummary.Sessions) != 2 {
		t.Fatalf("checkpoint not created by BatchSessions: summary=%v err=%v", batchSummary, err)
	}

	// SessionTranscript replaces the session transcript.
	full := []byte("full line 1\nfull line 2\n")
	if err := store.Write(ctx, SessionTranscript{
		CheckpointID: cpID,
		SessionID:    "session-001",
		Transcript:   redact.AlreadyRedacted(full),
	}); err != nil {
		t.Fatalf("Write(SessionTranscript) error = %v", err)
	}
	content, err := store.ReadSessionContent(ctx, cpID, 0)
	if err != nil {
		t.Fatalf("ReadSessionContent() error = %v", err)
	}
	if string(content.Transcript) != string(full) {
		t.Errorf("SessionTranscript not applied: got %q want %q", content.Transcript, full)
	}
	if err := store.Write(ctx, Session{
		CheckpointID: cpID,
		SessionID:    "session-002",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte("second")),
	}); err != nil {
		t.Fatalf("Write(second Session) error = %v", err)
	}

	// SessionSummary rewrites the explicitly named session, not necessarily the latest.
	if err := store.Write(ctx, SessionSummary{
		CheckpointID: cpID,
		SessionID:    "session-001",
		Summary:      &Summary{Intent: "why", Outcome: "what"},
	}); err != nil {
		t.Fatalf("Write(SessionSummary) error = %v", err)
	}
	if meta, err := store.ReadSessionMetadata(ctx, cpID, 0); err != nil || meta.Summary == nil || meta.Summary.Intent != "why" {
		t.Errorf("SessionSummary not applied to session-001: meta=%+v err=%v", meta, err)
	}
	if meta, err := store.ReadSessionMetadata(ctx, cpID, 1); err != nil || meta.Summary != nil {
		t.Errorf("SessionSummary unexpectedly applied to session-002: meta=%+v err=%v", meta, err)
	}

	// CheckpointAttribution rewrites the checkpoint root combined attribution.
	if err := store.Write(ctx, CheckpointAttribution{
		CheckpointID: cpID,
		Attribution:  &Attribution{AgentLines: 42},
	}); err != nil {
		t.Fatalf("Write(CheckpointAttribution) error = %v", err)
	}
	rootSummary, err := store.Read(ctx, cpID)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if rootSummary.CombinedAttribution == nil || rootSummary.CombinedAttribution.AgentLines != 42 {
		t.Errorf("CheckpointAttribution not applied: %+v", rootSummary.CombinedAttribution)
	}
}

// TestWrite_BackfillSummaryNotFound verifies error propagation through dispatch.
func TestWrite_BackfillSummaryNotFound(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	if err := store.ensureSessionsBranch(context.Background()); err != nil {
		t.Fatalf("ensureSessionsBranch() error = %v", err)
	}

	err := store.Write(context.Background(), SessionSummary{
		CheckpointID: id.MustCheckpointID("000000000000"),
		Summary:      &Summary{Intent: "x"},
	})
	if !errors.Is(err, ErrCheckpointNotFound) {
		t.Errorf("Write(SessionSummary) error = %v, want ErrCheckpointNotFound", err)
	}
}

func TestGitStore_SummaryRetryUpdatesOriginalSession(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	ctx := context.Background()
	cpID := id.MustCheckpointID("d1e2f3a4b5c6")

	if err := store.Write(ctx, Session{
		CheckpointID: cpID,
		SessionID:    "session-001",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte("first")),
	}); err != nil {
		t.Fatalf("Write(first Session) error = %v", err)
	}

	inserted := false
	ctx = withBeforeRefCAS(ctx, func() {
		if inserted {
			return
		}
		inserted = true
		if err := store.Write(ctx, Session{
			CheckpointID: cpID,
			SessionID:    "session-002",
			Strategy:     "manual-commit",
			Transcript:   redact.AlreadyRedacted([]byte("second")),
		}); err != nil {
			t.Fatalf("Write(second Session) error = %v", err)
		}
	})
	if err := store.Write(ctx, SessionSummary{
		CheckpointID: cpID,
		SessionID:    "session-001",
		Summary:      &Summary{Intent: firstSummaryIntent},
	}); err != nil {
		t.Fatalf("Write(SessionSummary) error = %v", err)
	}

	first, err := store.ReadSessionMetadata(ctx, cpID, 0)
	if err != nil {
		t.Fatalf("read first metadata: %v", err)
	}
	if first.Summary == nil || first.Summary.Intent != firstSummaryIntent {
		t.Fatalf("first summary = %+v", first.Summary)
	}
	second, err := store.ReadSessionMetadata(ctx, cpID, 1)
	if err != nil {
		t.Fatalf("read second metadata: %v", err)
	}
	if second.Summary != nil {
		t.Fatalf("second summary = %+v, want nil", second.Summary)
	}
}

func TestGitStore_LegacySummaryTargetPinnedAcrossRetry(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	ctx := context.Background()
	cpID := id.MustCheckpointID("e1f2a3b4c5d6")

	if err := store.Write(ctx, Session{
		CheckpointID: cpID,
		SessionID:    "session-001",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte("first")),
	}); err != nil {
		t.Fatalf("Write(first Session) error = %v", err)
	}

	inserted := false
	ctx = withBeforeRefCAS(ctx, func() {
		if inserted {
			return
		}
		inserted = true
		if err := store.Write(ctx, Session{
			CheckpointID: cpID,
			SessionID:    "session-002",
			Strategy:     "manual-commit",
			Transcript:   redact.AlreadyRedacted([]byte("second")),
		}); err != nil {
			t.Fatalf("Write(second Session) error = %v", err)
		}
	})
	if err := store.Write(ctx, SessionSummary{
		CheckpointID: cpID,
		Summary:      &Summary{Intent: firstSummaryIntent},
	}); err != nil {
		t.Fatalf("legacy Write(SessionSummary) error = %v", err)
	}

	first, err := store.ReadSessionMetadata(ctx, cpID, 0)
	if err != nil {
		t.Fatalf("read first metadata: %v", err)
	}
	if first.Summary == nil || first.Summary.Intent != firstSummaryIntent {
		t.Fatalf("first summary = %+v", first.Summary)
	}
	second, err := store.ReadSessionMetadata(ctx, cpID, 1)
	if err != nil {
		t.Fatalf("read second metadata: %v", err)
	}
	if second.Summary != nil {
		t.Fatalf("second summary = %+v, want nil", second.Summary)
	}
}
