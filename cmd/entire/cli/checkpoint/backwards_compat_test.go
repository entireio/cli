package checkpoint

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// TestReadCommitted_MissingTokenUsage verifies that reading metadata without
// token_usage field doesn't cause errors (for very old checkpoints that
// predate token tracking).
//
// Background: TokenUsage was moved from the checkpoint package to the agent
// package. The struct definition is identical, so JSON unmarshaling is
// inherently compatible. This test verifies graceful handling of old
// checkpoints that don't have token tracking at all.
func TestReadCommitted_MissingTokenUsage(t *testing.T) {
	tempDir := t.TempDir()

	repo, err := git.PlainInit(tempDir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	readmeFile := filepath.Join(tempDir, "README.md")
	if err := os.WriteFile(readmeFile, []byte("# Test"), 0o644); err != nil {
		t.Fatalf("failed to write README: %v", err)
	}
	if _, err := worktree.Add("README.md"); err != nil {
		t.Fatalf("failed to add README: %v", err)
	}
	if _, err := worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com"},
	}); err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	store := NewGitStore(repo)
	checkpointID := id.MustCheckpointID("def456abc123")

	// Write checkpoint WITHOUT token usage (simulates old checkpoints)
	err = store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID: checkpointID,
		SessionID:    "test-session-old",
		Strategy:     "manual-commit",
		Agent:        agent.AgentTypeClaudeCode,
		Transcript:   []byte("ancient transcript"),
		AuthorName:   "Test Author",
		AuthorEmail:  "test@example.com",
		// TokenUsage is nil - simulates old checkpoint
		TokenUsage: nil,
	})
	if err != nil {
		t.Fatalf("WriteCommitted() error = %v", err)
	}

	// Reading should succeed with nil TokenUsage
	summary, err := store.ReadCommitted(context.Background(), checkpointID)
	if err != nil {
		t.Fatalf("ReadCommitted() error = %v", err)
	}

	if summary.CheckpointID != checkpointID {
		t.Errorf("CheckpointID = %v, want %v", summary.CheckpointID, checkpointID)
	}

	// TokenUsage should be nil for old checkpoints without token tracking
	if summary.TokenUsage != nil {
		t.Errorf("TokenUsage should be nil for metadata without token_usage field, got %+v", summary.TokenUsage)
	}
}
