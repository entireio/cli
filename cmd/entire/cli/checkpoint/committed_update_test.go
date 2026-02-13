package checkpoint

import (
	"context"
	"errors"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

func TestGitStore_UpdateCommitted_AppendsTranscript(t *testing.T) {
	t.Parallel()

	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo)
	checkpointID := id.MustCheckpointID("a1b2c3d4e5f6")
	sessionID := "test-session-trailing"

	// Phase 1: Write initial checkpoint with transcript, prompts, context
	initialTranscript := []byte(`{"type":"human","message":{"content":"initial prompt"}}
{"type":"assistant","message":{"content":"initial response"}}`)
	initialPrompts := []string{"initial prompt"}
	initialContext := []byte("# Initial Context\n\nSome context here.\n")

	err := store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID: checkpointID,
		SessionID:    sessionID,
		Strategy:     "manual-commit",
		Transcript:   initialTranscript,
		Prompts:      initialPrompts,
		Context:      initialContext,
		FilesTouched: []string{"file1.go"},
		AuthorName:   "Test Author",
		AuthorEmail:  "test@example.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() error = %v", err)
	}

	// Verify initial content
	content, err := store.ReadSessionContent(context.Background(), checkpointID, 0)
	if err != nil {
		t.Fatalf("ReadSessionContent() error = %v", err)
	}
	if content.Prompts != "initial prompt" {
		t.Errorf("initial prompts = %q, want %q", content.Prompts, "initial prompt")
	}

	// Phase 2: Append trailing transcript
	trailingTranscript := []byte(`{"type":"human","message":{"content":"trailing question"}}
{"type":"assistant","message":{"content":"trailing answer"}}`)
	trailingPrompts := []string{"trailing question"}
	updatedContext := []byte("# Updated Context\n\nIncludes trailing conversation.\n")

	err = store.UpdateCommitted(context.Background(), UpdateCommittedOptions{
		CheckpointID: checkpointID,
		SessionID:    sessionID,
		Transcript:   trailingTranscript,
		Prompts:      trailingPrompts,
		Context:      updatedContext,
	})
	if err != nil {
		t.Fatalf("UpdateCommitted() error = %v", err)
	}

	// Phase 3: Verify merged content
	updated, err := store.ReadSessionContent(context.Background(), checkpointID, 0)
	if err != nil {
		t.Fatalf("ReadSessionContent() after update error = %v", err)
	}

	// Transcript should contain both initial and trailing lines
	if len(updated.Transcript) <= len(initialTranscript) {
		t.Errorf("updated transcript length (%d) should be greater than initial (%d)",
			len(updated.Transcript), len(initialTranscript))
	}

	// Prompts should contain both initial and trailing
	if updated.Prompts != "initial prompt\n\n---\n\ntrailing question" {
		t.Errorf("updated prompts = %q, want combined prompts", updated.Prompts)
	}

	// Context should be replaced
	if updated.Context != "# Updated Context\n\nIncludes trailing conversation.\n" {
		t.Errorf("updated context = %q, want replacement context", updated.Context)
	}

	// Verify metadata is NOT modified (FilesTouched, CheckpointsCount unchanged)
	metadata := readLatestSessionMetadata(t, repo, checkpointID)
	if len(metadata.FilesTouched) != 1 || metadata.FilesTouched[0] != "file1.go" {
		t.Errorf("metadata.FilesTouched = %v, want [file1.go] (should be unchanged)", metadata.FilesTouched)
	}
}

func TestGitStore_UpdateCommitted_NotFound(t *testing.T) {
	t.Parallel()

	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo)

	// Ensure sessions branch exists
	err := store.ensureSessionsBranch()
	if err != nil {
		t.Fatalf("ensureSessionsBranch() error = %v", err)
	}

	// Try to update a non-existent checkpoint
	checkpointID := id.MustCheckpointID("000000000000")
	err = store.UpdateCommitted(context.Background(), UpdateCommittedOptions{
		CheckpointID: checkpointID,
		SessionID:    "nonexistent-session",
		Transcript:   []byte("trailing data"),
	})
	if err == nil {
		t.Error("UpdateCommitted() should return error for non-existent checkpoint")
	}
	if !errors.Is(err, ErrCheckpointNotFound) {
		t.Errorf("UpdateCommitted() error = %v, want ErrCheckpointNotFound", err)
	}
}

func TestGitStore_UpdateCommitted_MatchesSessionID(t *testing.T) {
	t.Parallel()

	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo)
	checkpointID := id.MustCheckpointID("b1c2d3e4f5a6")

	// Write initial checkpoint with two sessions
	err := store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID: checkpointID,
		SessionID:    "session-1",
		Strategy:     "manual-commit",
		Transcript:   []byte(`{"type":"human","message":{"content":"s1 prompt"}}`),
		Prompts:      []string{"s1 prompt"},
		FilesTouched: []string{"file1.go"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() session-1 error = %v", err)
	}

	// Write second session to same checkpoint
	err = store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID: checkpointID,
		SessionID:    "session-2",
		Strategy:     "manual-commit",
		Transcript:   []byte(`{"type":"human","message":{"content":"s2 prompt"}}`),
		Prompts:      []string{"s2 prompt"},
		FilesTouched: []string{"file2.go"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() session-2 error = %v", err)
	}

	// Update session-1 with trailing transcript
	err = store.UpdateCommitted(context.Background(), UpdateCommittedOptions{
		CheckpointID: checkpointID,
		SessionID:    "session-1",
		Transcript:   []byte(`{"type":"assistant","message":{"content":"trailing for s1"}}`),
		Prompts:      []string{"trailing prompt for s1"},
	})
	if err != nil {
		t.Fatalf("UpdateCommitted() error = %v", err)
	}

	// Verify session-1 was updated
	s1, err := store.ReadSessionContentByID(context.Background(), checkpointID, "session-1")
	if err != nil {
		t.Fatalf("ReadSessionContentByID() session-1 error = %v", err)
	}
	if s1.Prompts != "s1 prompt\n\n---\n\ntrailing prompt for s1" {
		t.Errorf("session-1 prompts = %q, want combined", s1.Prompts)
	}

	// Verify session-2 was NOT modified
	s2, err := store.ReadSessionContentByID(context.Background(), checkpointID, "session-2")
	if err != nil {
		t.Fatalf("ReadSessionContentByID() session-2 error = %v", err)
	}
	if s2.Prompts != "s2 prompt" {
		t.Errorf("session-2 prompts = %q, want unchanged", s2.Prompts)
	}
}
