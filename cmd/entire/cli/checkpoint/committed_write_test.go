package checkpoint

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/redact"
)

// unknownWriteRequest is a WriteRequest the dispatcher does not handle. It is
// sealed into the union via the unexported marker (only possible in-package),
// which lets the test exercise the default branch.
type unknownWriteRequest struct{}

func (unknownWriteRequest) isWriteRequest() {}

// TestWrite_DispatchesEachRequest verifies that Store.Write routes each request
// type to the corresponding git operation, observing the effect of each.
func TestWrite_DispatchesEachRequest(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	ctx := context.Background()
	cpID := id.MustCheckpointID("a1b2c3d4e5f6")

	// WriteSession materializes the checkpoint on first session.
	if err := store.Write(ctx, WriteSession{
		CheckpointID: cpID,
		SessionID:    "session-001",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte("provisional\n")),
		Prompts:      []string{"initial"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	}); err != nil {
		t.Fatalf("Write(WriteSession) error = %v", err)
	}
	summary, err := store.ReadCommitted(ctx, cpID)
	if err != nil || summary == nil {
		t.Fatalf("checkpoint not created by WriteSession: summary=%v err=%v", summary, err)
	}

	// BackfillTranscript replaces the session transcript.
	full := []byte("full line 1\nfull line 2\n")
	if err := store.Write(ctx, BackfillTranscript{
		CheckpointID: cpID,
		SessionID:    "session-001",
		Transcript:   redact.AlreadyRedacted(full),
	}); err != nil {
		t.Fatalf("Write(BackfillTranscript) error = %v", err)
	}
	content, err := store.ReadSessionContent(ctx, cpID, 0)
	if err != nil {
		t.Fatalf("ReadSessionContent() error = %v", err)
	}
	if string(content.Transcript) != string(full) {
		t.Errorf("BackfillTranscript not applied: got %q want %q", content.Transcript, full)
	}

	// BackfillSummary rewrites the latest session's summary.
	if err := store.Write(ctx, BackfillSummary{
		CheckpointID: cpID,
		Summary:      &Summary{Intent: "why", Outcome: "what"},
	}); err != nil {
		t.Fatalf("Write(BackfillSummary) error = %v", err)
	}
	if meta := readLatestSessionMetadata(t, repo, cpID); meta.Summary == nil || meta.Summary.Intent != "why" {
		t.Errorf("BackfillSummary not applied: %+v", meta.Summary)
	}

	// BackfillAttribution rewrites the checkpoint root combined attribution.
	if err := store.Write(ctx, BackfillAttribution{
		CheckpointID: cpID,
		Attribution:  &InitialAttribution{AgentLines: 42},
	}); err != nil {
		t.Fatalf("Write(BackfillAttribution) error = %v", err)
	}
	rootSummary, err := store.ReadCommitted(ctx, cpID)
	if err != nil {
		t.Fatalf("ReadCommitted() error = %v", err)
	}
	if rootSummary.CombinedAttribution == nil || rootSummary.CombinedAttribution.AgentLines != 42 {
		t.Errorf("BackfillAttribution not applied: %+v", rootSummary.CombinedAttribution)
	}
}

func TestWriteCommittedWritesBranchCheckpointVersion(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	ctx := context.Background()
	cpID := id.MustCheckpointID("b1b2c3d4e5f6")

	if err := store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-001",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte("transcript\n")),
		Prompts:      []string{"initial"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	}); err != nil {
		t.Fatalf("WriteCommitted() error = %v", err)
	}

	summary, err := store.ReadCommitted(ctx, cpID)
	if err != nil {
		t.Fatalf("ReadCommitted() error = %v", err)
	}
	if summary == nil {
		t.Fatal("ReadCommitted() returned nil summary")
	}
	if summary.CheckpointVersion != CheckpointVersionBranchV1 {
		t.Fatalf("CheckpointVersion = %q, want %q", summary.CheckpointVersion, CheckpointVersionBranchV1)
	}

	rawSummary := readSummaryFromBranch(t, repo, cpID)
	if rawSummary.CheckpointVersion != CheckpointVersionBranchV1 {
		t.Fatalf("raw checkpoint_version = %q, want %q", rawSummary.CheckpointVersion, CheckpointVersionBranchV1)
	}
}

// TestWrite_UnknownRequestErrors verifies the dispatcher surfaces an
// unhandled request type rather than silently ignoring it.
func TestWrite_UnknownRequestErrors(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())

	err := store.Write(context.Background(), unknownWriteRequest{})
	if err == nil {
		t.Fatal("Write(unknownWriteRequest) should error")
	}
	if !strings.Contains(err.Error(), "unsupported write request") {
		t.Errorf("error = %v, want mention of unsupported write request", err)
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

	err := store.Write(context.Background(), BackfillSummary{
		CheckpointID: id.MustCheckpointID("000000000000"),
		Summary:      &Summary{Intent: "x"},
	})
	if !errors.Is(err, ErrCheckpointNotFound) {
		t.Errorf("Write(BackfillSummary) error = %v, want ErrCheckpointNotFound", err)
	}
}
