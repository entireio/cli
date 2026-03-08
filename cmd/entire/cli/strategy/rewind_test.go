package strategy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/claudecode" // Register agent for ResolveAgentForRewind tests
	_ "github.com/entireio/cli/cmd/entire/cli/agent/geminicli"  // Register agent for ResolveAgentForRewind tests

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
)

func TestShadowStrategy_PreviewRewind(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)

	// Create initial commit
	readmeFile := filepath.Join(dir, "README.md")
	if err := os.WriteFile(readmeFile, []byte("# Test\n"), 0o644); err != nil {
		t.Fatalf("failed to write README: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	if _, err := worktree.Add("README.md"); err != nil {
		t.Fatalf("failed to add README: %v", err)
	}

	initialCommit, err := worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// Create checkpoint with app.js file
	appFile := filepath.Join(dir, "app.js")
	if err := os.WriteFile(appFile, []byte("console.log('hello');\n"), 0o644); err != nil {
		t.Fatalf("failed to write app.js: %v", err)
	}

	if _, err := worktree.Add("app.js"); err != nil {
		t.Fatalf("failed to add app.js: %v", err)
	}

	// Create metadata directory structure first
	sessionID := "test-session-123"
	metadataDir := filepath.Join(dir, entireDir, "metadata", sessionID)
	if err := os.MkdirAll(metadataDir, 0o755); err != nil {
		t.Fatalf("failed to create metadata dir: %v", err)
	}

	// Create session state to track untracked files at start
	s := &ManualCommitStrategy{}
	state := &SessionState{
		SessionID:             sessionID,
		BaseCommit:            initialCommit.String(),
		StartedAt:             time.Now(),
		UntrackedFilesAtStart: []string{"existing-untracked.txt"},
		StepCount:             1,
		WorktreePath:          dir,
	}
	if err := s.saveSessionState(context.Background(), state); err != nil {
		t.Fatalf("failed to save session state: %v", err)
	}

	// Create checkpoint commit with session trailer
	checkpointMsg := "Checkpoint\n\nEntire-Session: " + sessionID
	checkpointHash, err := worktree.Commit(checkpointMsg, &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("failed to create checkpoint: %v", err)
	}

	// Reset to initial commit to simulate moving forward in time
	if err := worktree.Reset(&git.ResetOptions{
		Commit: initialCommit,
		Mode:   git.HardReset,
	}); err != nil {
		t.Fatalf("failed to reset to initial: %v", err)
	}

	// Create files that would be deleted on rewind:
	// 1. A new untracked file (created after checkpoint)
	extraFile := filepath.Join(dir, "extra.js")
	if err := os.WriteFile(extraFile, []byte("console.log('extra');\n"), 0o644); err != nil {
		t.Fatalf("failed to write extra.js: %v", err)
	}

	// 2. An untracked file that existed at session start (should NOT be deleted)
	existingFile := filepath.Join(dir, "existing-untracked.txt")
	if err := os.WriteFile(existingFile, []byte("I existed before session\n"), 0o644); err != nil {
		t.Fatalf("failed to write existing-untracked.txt: %v", err)
	}

	// Create rewind point
	point := RewindPoint{
		ID:          checkpointHash.String(),
		Message:     "Checkpoint",
		MetadataDir: metadataDir,
		Date:        time.Now(),
	}

	// Test PreviewRewind
	preview, err := s.PreviewRewind(context.Background(), point)
	if err != nil {
		t.Fatalf("PreviewRewind() error = %v", err)
	}

	if preview == nil {
		t.Fatal("PreviewRewind() returned nil preview")
	}

	// Should restore app.js
	foundApp := false
	for _, f := range preview.FilesToRestore {
		if f == "app.js" {
			foundApp = true
			break
		}
	}
	if !foundApp {
		t.Errorf("FilesToRestore missing app.js, got: %v", preview.FilesToRestore)
	}

	// Should delete extra.js
	foundExtra := false
	for _, f := range preview.FilesToDelete {
		if f == "extra.js" {
			foundExtra = true
			break
		}
	}
	if !foundExtra {
		t.Errorf("FilesToDelete missing extra.js, got: %v", preview.FilesToDelete)
	}

	// Should NOT delete existing-untracked.txt (existed at session start)
	for _, f := range preview.FilesToDelete {
		if f == "existing-untracked.txt" {
			t.Errorf("FilesToDelete incorrectly includes existing-untracked.txt, got: %v", preview.FilesToDelete)
		}
	}
}

func TestShadowStrategy_PreviewRewind_LogsOnly(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)

	s := &ManualCommitStrategy{}

	// Logs-only point should return empty preview
	point := RewindPoint{
		ID:           "abc123",
		Message:      "Committed",
		IsLogsOnly:   true,
		CheckpointID: "a1b2c3d4e5f6",
		Date:         time.Now(),
	}

	preview, err := s.PreviewRewind(context.Background(), point)
	if err != nil {
		t.Fatalf("PreviewRewind() error = %v", err)
	}

	if preview == nil {
		t.Fatal("PreviewRewind() returned nil preview")
	}

	if len(preview.FilesToDelete) > 0 {
		t.Errorf("Logs-only preview should have no files to delete, got: %v", preview.FilesToDelete)
	}

	if len(preview.FilesToRestore) > 0 {
		t.Errorf("Logs-only preview should have no files to restore, got: %v", preview.FilesToRestore)
	}
}

func TestResolveAgentForRewind(t *testing.T) {
	t.Parallel()

	t.Run("empty type returns error", func(t *testing.T) {
		t.Parallel()
		_, err := ResolveAgentForRewind("")
		if err == nil {
			t.Error("expected error for empty agent type")
		}
	})

	t.Run("Claude Code type resolves correctly", func(t *testing.T) {
		t.Parallel()
		ag, err := ResolveAgentForRewind("Claude Code")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ag.Name() != agent.AgentNameClaudeCode {
			t.Errorf("Name() = %q, want %q", ag.Name(), agent.AgentNameClaudeCode)
		}
	})

	t.Run("Gemini CLI type resolves correctly", func(t *testing.T) {
		t.Parallel()
		ag, err := ResolveAgentForRewind("Gemini CLI")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ag.Name() != agent.AgentNameGemini {
			t.Errorf("Name() = %q, want %q", ag.Name(), agent.AgentNameGemini)
		}
	})

	t.Run("unknown type returns error", func(t *testing.T) {
		t.Parallel()
		_, err := ResolveAgentForRewind("Nonexistent Agent")
		if err == nil {
			t.Error("expected error for unknown agent type")
		}
	})
}
