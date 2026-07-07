package checkpoint

import (
	"context"
	"errors"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/redact"
)

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

	// SessionSummary rewrites the latest session's summary.
	if err := store.Write(ctx, SessionSummary{
		CheckpointID: cpID,
		Summary:      &Summary{Intent: "why", Outcome: "what"},
	}); err != nil {
		t.Fatalf("Write(SessionSummary) error = %v", err)
	}
	if meta := readLatestSessionMetadata(t, repo, cpID); meta.Summary == nil || meta.Summary.Intent != "why" {
		t.Errorf("SessionSummary not applied: %+v", meta.Summary)
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
