package strategy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// TestHardResetWithProtection_PreservesProtectedDirs verifies that HardResetWithProtection
// does not delete untracked directories like .entire/ and .worktrees/.
//
// This is a safety net. Using go-git v5 incorrectly deletes untracked directories
// even when they're in .gitignore. These tests need to pass once we go back to go-git (v6?)
func TestHardResetWithProtection_PreservesProtectedDirs(t *testing.T) {
	// Create a temp directory for our test repo
	repoDir := t.TempDir()

	// Initialize a git repo
	repo, err := git.PlainInit(repoDir, false)
	if err != nil {
		t.Fatalf("failed to init repo: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	// Create initial commit
	initialFile := filepath.Join(repoDir, "initial.txt")
	if err := os.WriteFile(initialFile, []byte("initial content"), 0o644); err != nil {
		t.Fatalf("failed to create initial file: %v", err)
	}
	if _, err := worktree.Add("initial.txt"); err != nil {
		t.Fatalf("failed to add initial file: %v", err)
	}

	sig := &object.Signature{
		Name:  "Test",
		Email: "test@test.com",
		When:  time.Now(),
	}
	initialCommit, err := worktree.Commit("Initial commit", &git.CommitOptions{Author: sig})
	if err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// Create second commit
	secondFile := filepath.Join(repoDir, "second.txt")
	if err := os.WriteFile(secondFile, []byte("second content"), 0o644); err != nil {
		t.Fatalf("failed to create second file: %v", err)
	}
	if _, err := worktree.Add("second.txt"); err != nil {
		t.Fatalf("failed to add second file: %v", err)
	}
	if _, err := worktree.Commit("Second commit", &git.CommitOptions{Author: sig}); err != nil {
		t.Fatalf("failed to create second commit: %v", err)
	}

	// Create .gitignore to ignore our protected directories
	gitignore := filepath.Join(repoDir, ".gitignore")
	if err := os.WriteFile(gitignore, []byte(".entire/\n.worktrees/\n"), 0o644); err != nil {
		t.Fatalf("failed to create .gitignore: %v", err)
	}

	// Create protected directories with content (these are untracked/ignored)
	entireDir := filepath.Join(repoDir, ".entire")
	if err := os.MkdirAll(filepath.Join(entireDir, "metadata"), 0o755); err != nil {
		t.Fatalf("failed to create .entire dir: %v", err)
	}
	entireContent := "important session metadata"
	if err := os.WriteFile(filepath.Join(entireDir, "metadata", "session.json"), []byte(entireContent), 0o644); err != nil {
		t.Fatalf("failed to create .entire file: %v", err)
	}

	worktreesDir := filepath.Join(repoDir, ".worktrees")
	if err := os.MkdirAll(filepath.Join(worktreesDir, "feature-branch"), 0o755); err != nil {
		t.Fatalf("failed to create .worktrees dir: %v", err)
	}
	worktreesContent := "worktree config"
	if err := os.WriteFile(filepath.Join(worktreesDir, "feature-branch", "config"), []byte(worktreesContent), 0o644); err != nil {
		t.Fatalf("failed to create .worktrees file: %v", err)
	}

	// Change to repo directory so HardResetWithProtection can find the repo
	t.Chdir(repoDir)

	// Perform hard reset to initial commit
	shortID, err := HardResetWithProtection(context.Background(), initialCommit)
	if err != nil {
		t.Fatalf("HardResetWithProtection failed: %v", err)
	}

	// Verify reset worked (second.txt should be gone)
	if _, err := os.Stat(secondFile); !os.IsNotExist(err) {
		t.Error("second.txt should not exist after reset")
	}

	// Verify short ID is returned correctly
	if len(shortID) != 7 {
		t.Errorf("expected 7-char short ID, got %d chars: %s", len(shortID), shortID)
	}

	// CRITICAL: Verify protected directories still exist with their content
	// This is the main assertion - if this fails, the reset implementation is broken

	// Check .entire/
	if _, err := os.Stat(entireDir); os.IsNotExist(err) {
		t.Error(".entire/ directory was deleted by HardResetWithProtection - THIS IS A REGRESSION")
	}
	content, err := os.ReadFile(filepath.Join(entireDir, "metadata", "session.json"))
	if err != nil {
		t.Errorf("failed to read .entire file after reset: %v - directory may have been deleted", err)
	} else if string(content) != entireContent {
		t.Errorf(".entire content changed: got %q, want %q", content, entireContent)
	}

	// Check .worktrees/
	if _, err := os.Stat(worktreesDir); os.IsNotExist(err) {
		t.Error(".worktrees/ directory was deleted by HardResetWithProtection - THIS IS A REGRESSION")
	}
	content, err = os.ReadFile(filepath.Join(worktreesDir, "feature-branch", "config"))
	if err != nil {
		t.Errorf("failed to read .worktrees file after reset: %v - directory may have been deleted", err)
	} else if string(content) != worktreesContent {
		t.Errorf(".worktrees content changed: got %q, want %q", content, worktreesContent)
	}
}
