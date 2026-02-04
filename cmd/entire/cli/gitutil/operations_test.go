package gitutil

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func TestHasUncommittedChanges(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	repo, err := git.PlainInit(tmpDir, false)
	if err != nil {
		t.Fatalf("Failed to init repo: %v", err)
	}

	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Failed to get worktree: %v", err)
	}

	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("test"), 0o644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("Failed to add test file: %v", err)
	}
	if _, err := w.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
		},
	}); err != nil {
		t.Fatalf("Failed to commit: %v", err)
	}

	// Test clean working tree
	hasChanges, err := HasUncommittedChanges()
	if err != nil {
		t.Fatalf("HasUncommittedChanges() error = %v", err)
	}
	if hasChanges {
		t.Error("HasUncommittedChanges() = true, want false for clean tree")
	}

	// Make unstaged change
	if err := os.WriteFile(testFile, []byte("modified"), 0o644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	// Test with unstaged changes
	hasChanges, err = HasUncommittedChanges()
	if err != nil {
		t.Fatalf("HasUncommittedChanges() error = %v", err)
	}
	if !hasChanges {
		t.Error("HasUncommittedChanges() = false, want true for modified file")
	}

	// Stage the change
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("Failed to add test file: %v", err)
	}

	// Test with staged changes
	hasChanges, err = HasUncommittedChanges()
	if err != nil {
		t.Fatalf("HasUncommittedChanges() error = %v", err)
	}
	if !hasChanges {
		t.Error("HasUncommittedChanges() = false, want true for staged file")
	}

	// Commit and add untracked file
	if _, err := w.Commit("second commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
		},
	}); err != nil {
		t.Fatalf("Failed to commit: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "untracked.txt"), []byte("new"), 0o644); err != nil {
		t.Fatalf("Failed to write untracked file: %v", err)
	}

	// Test with untracked file (should be true)
	hasChanges, err = HasUncommittedChanges()
	if err != nil {
		t.Fatalf("HasUncommittedChanges() error = %v", err)
	}
	if !hasChanges {
		t.Error("HasUncommittedChanges() = false, want true for untracked file")
	}

	// Clean up untracked file for next test
	if err := os.Remove(filepath.Join(tmpDir, "untracked.txt")); err != nil {
		t.Fatalf("Failed to remove untracked file: %v", err)
	}

	// Test global gitignore (core.excludesfile) handling
	// go-git doesn't read global gitignore, so we use git CLI instead.
	globalIgnoreDir := t.TempDir()
	globalIgnoreFile := filepath.Join(globalIgnoreDir, "global-gitignore")
	if err := os.WriteFile(globalIgnoreFile, []byte("*.globally-ignored\n"), 0o644); err != nil {
		t.Fatalf("Failed to write global gitignore: %v", err)
	}

	// Set core.excludesfile in repo config
	cmd := exec.CommandContext(context.Background(), "git", "config", "core.excludesfile", globalIgnoreFile)
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("Failed to set core.excludesfile: %v", err)
	}

	// Create a file that matches the global ignore pattern
	if err := os.WriteFile(filepath.Join(tmpDir, "secret.globally-ignored"), []byte("ignored"), 0o644); err != nil {
		t.Fatalf("Failed to write globally ignored file: %v", err)
	}

	// Test with globally gitignored file - should return false (clean)
	hasChanges, err = HasUncommittedChanges()
	if err != nil {
		t.Fatalf("HasUncommittedChanges() error = %v", err)
	}
	if hasChanges {
		t.Error("HasUncommittedChanges() = true, want false for globally gitignored file (core.excludesfile)")
	}
}

func TestGetConfigValue(t *testing.T) {
	// Test that invalid keys return empty string
	invalid := GetConfigValue("nonexistent.key.that.does.not.exist")
	if invalid != "" {
		t.Errorf("expected empty string for invalid key, got %q", invalid)
	}

	// Test that it returns a value for user.name (assuming git is configured on test machine)
	name := GetConfigValue("user.name")
	t.Logf("git config user.name returned: %q", name)
}

func TestGetConfigValueTrimsWhitespace(t *testing.T) {
	// The git config command returns values with trailing newline
	// Verify that GetConfigValue trims whitespace properly
	email := GetConfigValue("user.email")
	t.Logf("git config user.email returned: %q", email)

	// If email is set, verify no leading/trailing whitespace
	if email != "" {
		if email[0] == ' ' || email[0] == '\n' || email[0] == '\t' {
			t.Errorf("expected no leading whitespace, got %q", email)
		}
		if email[len(email)-1] == ' ' || email[len(email)-1] == '\n' || email[len(email)-1] == '\t' {
			t.Errorf("expected no trailing whitespace, got %q", email)
		}
	}
}
