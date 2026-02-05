package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"entire.io/cli/cmd/entire/cli/paths"
	"entire.io/cli/cmd/entire/cli/strategy"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func setupCleanTestRepo(t *testing.T) (*git.Repository, plumbing.Hash) {
	t.Helper()

	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)
	paths.ClearRepoRootCache()

	// Create initial commit
	emptyTree := &object.Tree{Entries: []object.TreeEntry{}}
	obj := repo.Storer.NewEncodedObject()
	if err := emptyTree.Encode(obj); err != nil {
		t.Fatalf("failed to encode empty tree: %v", err)
	}
	emptyTreeHash, err := repo.Storer.SetEncodedObject(obj)
	if err != nil {
		t.Fatalf("failed to store empty tree: %v", err)
	}

	sig := object.Signature{Name: "test", Email: "test@test.com"}
	commit := &object.Commit{
		TreeHash:  emptyTreeHash,
		Author:    sig,
		Committer: sig,
		Message:   "initial commit",
	}
	commitObj := repo.Storer.NewEncodedObject()
	if err := commit.Encode(commitObj); err != nil {
		t.Fatalf("failed to encode commit: %v", err)
	}
	commitHash, err := repo.Storer.SetEncodedObject(commitObj)
	if err != nil {
		t.Fatalf("failed to store commit: %v", err)
	}

	// Create HEAD and master references
	headRef := plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("master"))
	if err := repo.Storer.SetReference(headRef); err != nil {
		t.Fatalf("failed to set HEAD: %v", err)
	}
	masterRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName("master"), commitHash)
	if err := repo.Storer.SetReference(masterRef); err != nil {
		t.Fatalf("failed to set master: %v", err)
	}

	return repo, commitHash
}

func TestRunClean_NoOrphanedItems(t *testing.T) {
	setupCleanTestRepo(t)

	var stdout bytes.Buffer
	err := runClean(&stdout, false)
	if err != nil {
		t.Fatalf("runClean() error = %v", err)
	}

	output := stdout.String()
	if !strings.Contains(output, "No orphaned items") {
		t.Errorf("Expected 'No orphaned items' message, got: %s", output)
	}
}

func TestRunClean_PreviewMode(t *testing.T) {
	repo, commitHash := setupCleanTestRepo(t)

	// Create shadow branches
	shadowBranches := []string{"entire/abc1234", "entire/def5678"}
	for _, b := range shadowBranches {
		ref := plumbing.NewHashReference(plumbing.NewBranchReferenceName(b), commitHash)
		if err := repo.Storer.SetReference(ref); err != nil {
			t.Fatalf("failed to create branch %s: %v", b, err)
		}
	}

	// Also create entire/sessions (should NOT be listed)
	sessionsRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), commitHash)
	if err := repo.Storer.SetReference(sessionsRef); err != nil {
		t.Fatalf("failed to create %s: %v", paths.MetadataBranchName, err)
	}

	var stdout bytes.Buffer
	err := runClean(&stdout, false) // force=false
	if err != nil {
		t.Fatalf("runClean() error = %v", err)
	}

	output := stdout.String()

	// Should show preview header
	if !strings.Contains(output, "orphaned items") {
		t.Errorf("Expected 'orphaned items' in output, got: %s", output)
	}

	// Should list the shadow branches
	if !strings.Contains(output, "entire/abc1234") {
		t.Errorf("Expected 'entire/abc1234' in output, got: %s", output)
	}
	if !strings.Contains(output, "entire/def5678") {
		t.Errorf("Expected 'entire/def5678' in output, got: %s", output)
	}

	// Should NOT list entire/sessions/v1
	if strings.Contains(output, paths.MetadataBranchName) {
		t.Errorf("Should not list '%s', got: %s", paths.MetadataBranchName, output)
	}

	// Should prompt to use --force
	if !strings.Contains(output, "--force") {
		t.Errorf("Expected '--force' prompt in output, got: %s", output)
	}

	// Branches should still exist (preview mode doesn't delete)
	for _, b := range shadowBranches {
		refName := plumbing.NewBranchReferenceName(b)
		if _, err := repo.Reference(refName, true); err != nil {
			t.Errorf("Branch %s should still exist after preview", b)
		}
	}
}

func TestRunClean_ForceMode(t *testing.T) {
	repo, commitHash := setupCleanTestRepo(t)

	// Create shadow branches
	shadowBranches := []string{"entire/abc1234", "entire/def5678"}
	for _, b := range shadowBranches {
		ref := plumbing.NewHashReference(plumbing.NewBranchReferenceName(b), commitHash)
		if err := repo.Storer.SetReference(ref); err != nil {
			t.Fatalf("failed to create branch %s: %v", b, err)
		}
	}

	var stdout bytes.Buffer
	err := runClean(&stdout, true) // force=true
	if err != nil {
		t.Fatalf("runClean() error = %v", err)
	}

	output := stdout.String()

	// Should show deletion confirmation
	if !strings.Contains(output, "Deleted") {
		t.Errorf("Expected 'Deleted' in output, got: %s", output)
	}

	// Branches should be deleted
	for _, b := range shadowBranches {
		refName := plumbing.NewBranchReferenceName(b)
		if _, err := repo.Reference(refName, true); err == nil {
			t.Errorf("Branch %s should be deleted but still exists", b)
		}
	}
}

func TestRunClean_SessionsBranchPreserved(t *testing.T) {
	repo, commitHash := setupCleanTestRepo(t)

	// Create shadow branch and sessions branch
	shadowRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName("entire/abc1234"), commitHash)
	if err := repo.Storer.SetReference(shadowRef); err != nil {
		t.Fatalf("failed to create shadow branch: %v", err)
	}

	sessionsRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), commitHash)
	if err := repo.Storer.SetReference(sessionsRef); err != nil {
		t.Fatalf("failed to create entire/sessions: %v", err)
	}

	var stdout bytes.Buffer
	err := runClean(&stdout, true) // force=true
	if err != nil {
		t.Fatalf("runClean() error = %v", err)
	}

	// Shadow branch should be deleted
	refName := plumbing.NewBranchReferenceName("entire/abc1234")
	if _, err := repo.Reference(refName, true); err == nil {
		t.Error("Shadow branch should be deleted")
	}

	// Sessions branch should still exist
	sessionsRefName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	if _, err := repo.Reference(sessionsRefName, true); err != nil {
		t.Error("entire/sessions branch should be preserved")
	}
}

func TestRunClean_NotGitRepository(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	paths.ClearRepoRootCache()

	var stdout bytes.Buffer
	err := runClean(&stdout, false)

	// Should return error for non-git directory
	if err == nil {
		t.Error("runClean() should return error for non-git directory")
	}
}

func TestRunClean_Subdirectory(t *testing.T) {
	repo, commitHash := setupCleanTestRepo(t)

	// Create shadow branch
	shadowRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName("entire/abc1234"), commitHash)
	if err := repo.Storer.SetReference(shadowRef); err != nil {
		t.Fatalf("failed to create shadow branch: %v", err)
	}

	// Create and cd into subdirectory within the repo
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}
	repoRoot := wt.Filesystem.Root()
	subDir := filepath.Join(repoRoot, "subdir")
	if err := wt.Filesystem.MkdirAll("subdir", 0o755); err != nil {
		t.Fatalf("failed to create subdir: %v", err)
	}

	t.Chdir(subDir)
	paths.ClearRepoRootCache()

	var stdout bytes.Buffer
	err = runClean(&stdout, false)
	if err != nil {
		t.Fatalf("runClean() from subdirectory error = %v", err)
	}

	output := stdout.String()
	if !strings.Contains(output, "entire/abc1234") {
		t.Errorf("Should find shadow branches from subdirectory, got: %s", output)
	}
}

func TestRunCleanWithItems_PartialFailure(t *testing.T) {
	// This test verifies that runCleanWithItems returns an error when some
	// deletions fail. We use runCleanWithItems to inject a list
	// containing both existing and non-existing items.

	repo, commitHash := setupCleanTestRepo(t)

	// Create one shadow branch
	shadowRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName("entire/abc1234"), commitHash)
	if err := repo.Storer.SetReference(shadowRef); err != nil {
		t.Fatalf("failed to create shadow branch: %v", err)
	}

	// Call runCleanWithItems with a mix of existing and non-existing branches
	items := []strategy.CleanupItem{
		{Type: strategy.CleanupTypeShadowBranch, ID: "entire/abc1234", Reason: "test"},
		{Type: strategy.CleanupTypeShadowBranch, ID: "entire/nonexistent1234567", Reason: "test"},
	}

	var stdout bytes.Buffer
	err := runCleanWithItems(&stdout, true, items) // force=true

	// Should return an error because one branch failed to delete
	if err == nil {
		t.Fatal("runCleanWithItems() should return error when items fail to delete")
	}

	// Error message should indicate the failure
	if !strings.Contains(err.Error(), "failed to delete") {
		t.Errorf("Error should mention 'failed to delete', got: %v", err)
	}

	// Output should show the successful deletion
	output := stdout.String()
	if !strings.Contains(output, "Deleted 1 items") {
		t.Errorf("Output should show successful deletion, got: %s", output)
	}

	// Output should also show the failures
	if !strings.Contains(output, "Failed to delete 1 items") {
		t.Errorf("Output should show failures, got: %s", output)
	}
}

func TestRunCleanWithItems_AllFailures(t *testing.T) {
	// Test that error is returned when ALL items fail to delete

	setupCleanTestRepo(t)

	// Call runCleanWithItems with only non-existing branches
	items := []strategy.CleanupItem{
		{Type: strategy.CleanupTypeShadowBranch, ID: "entire/nonexistent1234567", Reason: "test"},
		{Type: strategy.CleanupTypeShadowBranch, ID: "entire/alsononexistent", Reason: "test"},
	}

	var stdout bytes.Buffer
	err := runCleanWithItems(&stdout, true, items) // force=true

	// Should return an error because all items failed to delete
	if err == nil {
		t.Fatal("runCleanWithItems() should return error when items fail to delete")
	}

	// Error message should indicate 2 failures
	if !strings.Contains(err.Error(), "failed to delete 2 items") {
		t.Errorf("Error should mention 'failed to delete 2 items', got: %v", err)
	}

	// Output should NOT show any successful deletions
	output := stdout.String()
	if strings.Contains(output, "Deleted") {
		t.Errorf("Output should not show successful deletions, got: %s", output)
	}

	// Output should show the failures
	if !strings.Contains(output, "Failed to delete 2 items") {
		t.Errorf("Output should show failures, got: %s", output)
	}
}

func TestRunCleanWithItems_NoItems(t *testing.T) {
	setupCleanTestRepo(t)

	var stdout bytes.Buffer
	err := runCleanWithItems(&stdout, false, []strategy.CleanupItem{})
	if err != nil {
		t.Fatalf("runCleanWithItems() error = %v", err)
	}

	output := stdout.String()
	if !strings.Contains(output, "No orphaned items") {
		t.Errorf("Expected 'No orphaned items' message, got: %s", output)
	}
}

func TestRunCleanWithItems_MixedTypes_Preview(t *testing.T) {
	setupCleanTestRepo(t)

	// Test preview mode with different cleanup types
	items := []strategy.CleanupItem{
		{Type: strategy.CleanupTypeShadowBranch, ID: "entire/abc1234", Reason: "test"},
		{Type: strategy.CleanupTypeSessionState, ID: "session-123", Reason: "no checkpoints"},
		{Type: strategy.CleanupTypeCheckpoint, ID: "checkpoint-abc", Reason: "orphaned"},
	}

	var stdout bytes.Buffer
	err := runCleanWithItems(&stdout, false, items) // preview mode
	if err != nil {
		t.Fatalf("runCleanWithItems() error = %v", err)
	}

	output := stdout.String()

	// Should show all types
	if !strings.Contains(output, "Shadow branches") {
		t.Errorf("Expected 'Shadow branches' section, got: %s", output)
	}
	if !strings.Contains(output, "Session states") {
		t.Errorf("Expected 'Session states' section, got: %s", output)
	}
	if !strings.Contains(output, "Checkpoint metadata") {
		t.Errorf("Expected 'Checkpoint metadata' section, got: %s", output)
	}

	// Should show total count
	if !strings.Contains(output, "Found 3 orphaned items") {
		t.Errorf("Expected 'Found 3 orphaned items', got: %s", output)
	}
}
