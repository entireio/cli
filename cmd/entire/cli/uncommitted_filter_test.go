package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// TestFilterToUncommittedDeletions verifies the deletion-side committed filter:
// a deletion already recorded in HEAD (file absent from HEAD tree) is dropped,
// while a working-tree-only deletion (file still in HEAD) is kept. SaveStep at
// TurnEnd must only checkpoint uncommitted state — a mid-turn commit already
// condensed committed deletions via PostCommit.
func TestFilterToUncommittedDeletions(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	repo, err := git.PlainInit(tmpDir, false)
	if err != nil {
		t.Fatalf("failed to init repo: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}
	sig := &object.Signature{Name: "Test User", Email: "test@example.com"}

	// Commit both files.
	for _, name := range []string{"committed-gone.txt", "still-tracked.txt"} {
		if err := os.WriteFile(filepath.Join(tmpDir, name), []byte(name+"\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		if _, err := wt.Add(name); err != nil {
			t.Fatalf("add %s: %v", name, err)
		}
	}
	if _, err := wt.Commit("add both", &git.CommitOptions{Author: sig}); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Delete committed-gone.txt and COMMIT the deletion (it leaves HEAD).
	if err := os.Remove(filepath.Join(tmpDir, "committed-gone.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Remove("committed-gone.txt"); err != nil {
		t.Fatalf("git rm: %v", err)
	}
	if _, err := wt.Commit("remove committed-gone", &git.CommitOptions{Author: sig}); err != nil {
		t.Fatalf("commit deletion: %v", err)
	}

	// Delete still-tracked.txt from the working tree only (uncommitted deletion).
	if err := os.Remove(filepath.Join(tmpDir, "still-tracked.txt")); err != nil {
		t.Fatal(err)
	}

	result := filterToUncommittedDeletions(context.Background(), []string{"committed-gone.txt", "still-tracked.txt"})
	if len(result) != 1 || result[0] != "still-tracked.txt" {
		t.Errorf("filterToUncommittedDeletions() = %v, want [still-tracked.txt]", result)
	}
}
