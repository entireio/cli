package strategy

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"entire.io/cli/cmd/entire/cli/checkpoint"
	"entire.io/cli/cmd/entire/cli/paths"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// TestDetermineSuffix_NoExistingSuffix verifies that when no suffix is assigned yet,
// determineSuffix returns suffix 1.
func TestDetermineSuffix_NoExistingSuffix(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	// Create initial commit
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}
	testFile := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(testFile, []byte("test"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := worktree.Add("test.txt"); err != nil {
		t.Fatalf("failed to add file: %v", err)
	}
	initialCommit, err := worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	t.Chdir(dir)

	s := &ManualCommitStrategy{}

	// Session with no suffix assigned yet
	state := &SessionState{
		SessionID:          "test-session",
		BaseCommit:         initialCommit.String(),
		ShadowBranchSuffix: 0, // No suffix yet
	}

	suffix, isNew, err := s.determineSuffix(repo, state)
	if err != nil {
		t.Fatalf("determineSuffix() error = %v", err)
	}

	if suffix != 1 {
		t.Errorf("suffix = %d, want 1 (first suffix)", suffix)
	}
	if !isNew {
		t.Error("isNew should be true for first suffix")
	}
}

// TestDetermineSuffix_CleanWorktree verifies that a clean worktree results in
// a new suffix (previous work was dismissed).
func TestDetermineSuffix_CleanWorktree(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	// Create initial commit
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}
	testFile := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(testFile, []byte("line1\n"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := worktree.Add("test.txt"); err != nil {
		t.Fatalf("failed to add file: %v", err)
	}
	initialCommit, err := worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	t.Chdir(dir)

	// Create a shadow branch at suffix 1 (simulating previous checkpoint)
	shadowBranchName := checkpoint.ShadowBranchNameForCommitWithSuffix(initialCommit.String()[:7], 1)
	createShadowBranch(t, repo, shadowBranchName, initialCommit)

	s := &ManualCommitStrategy{}

	// Session with suffix 1, but worktree is clean (user dismissed changes)
	state := &SessionState{
		SessionID:          "test-session",
		BaseCommit:         initialCommit.String(),
		ShadowBranchSuffix: 1,
	}

	suffix, isNew, err := s.determineSuffix(repo, state)
	if err != nil {
		t.Fatalf("determineSuffix() error = %v", err)
	}

	if suffix != 2 {
		t.Errorf("suffix = %d, want 2 (new suffix for clean worktree)", suffix)
	}
	if !isNew {
		t.Error("isNew should be true (dismissed work)")
	}
}

// TestDetermineSuffix_ModifiedWithOverlapPreserved verifies that when the agent's
// work is still present in the worktree, we continue on the same suffix.
func TestDetermineSuffix_ModifiedWithOverlapPreserved(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	// Create initial commit with a file
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}
	testFile := filepath.Join(dir, "test.txt")
	baseContent := "line1\n" //nolint:goconst // Test data
	if err := os.WriteFile(testFile, []byte(baseContent), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := worktree.Add("test.txt"); err != nil {
		t.Fatalf("failed to add file: %v", err)
	}
	initialCommit, err := worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	t.Chdir(dir)

	// Create shadow branch with agent's additions (adds "line2")
	shadowBranchName := checkpoint.ShadowBranchNameForCommitWithSuffix(initialCommit.String()[:7], 1)
	agentContent := "line1\nline2\n" //nolint:goconst // Test data
	createShadowBranchWithFile(t, repo, shadowBranchName, initialCommit, "test.txt", agentContent)

	// Modify worktree to have agent's content still present
	if err := os.WriteFile(testFile, []byte(agentContent), 0o644); err != nil {
		t.Fatalf("failed to write modified file: %v", err)
	}

	s := &ManualCommitStrategy{}

	// Session with suffix 1, worktree has agent's changes
	state := &SessionState{
		SessionID:          "test-session",
		BaseCommit:         initialCommit.String(),
		ShadowBranchSuffix: 1,
	}

	suffix, isNew, err := s.determineSuffix(repo, state)
	if err != nil {
		t.Fatalf("determineSuffix() error = %v", err)
	}

	if suffix != 1 {
		t.Errorf("suffix = %d, want 1 (continue on same suffix)", suffix)
	}
	if isNew {
		t.Error("isNew should be false (continuing previous work)")
	}
}

// TestDetermineSuffix_ModifiedWithOverlapDismissed verifies that when the agent's
// work has been removed from the worktree, we get a new suffix.
func TestDetermineSuffix_ModifiedWithOverlapDismissed(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	// Create initial commit with a file
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}
	testFile := filepath.Join(dir, "test.txt")
	baseContent := "line1\n"
	if err := os.WriteFile(testFile, []byte(baseContent), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := worktree.Add("test.txt"); err != nil {
		t.Fatalf("failed to add file: %v", err)
	}
	initialCommit, err := worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	t.Chdir(dir)

	// Create shadow branch with agent's additions (adds "line2")
	shadowBranchName := checkpoint.ShadowBranchNameForCommitWithSuffix(initialCommit.String()[:7], 1)
	agentContent := "line1\nline2\n"
	createShadowBranchWithFile(t, repo, shadowBranchName, initialCommit, "test.txt", agentContent)

	// Modify worktree but remove agent's line (user dismissed the addition)
	differentContent := "line1\nline3\n" // Different change, agent's line2 is gone
	if err := os.WriteFile(testFile, []byte(differentContent), 0o644); err != nil {
		t.Fatalf("failed to write modified file: %v", err)
	}

	s := &ManualCommitStrategy{}

	// Session with suffix 1, but agent's work was dismissed
	state := &SessionState{
		SessionID:          "test-session",
		BaseCommit:         initialCommit.String(),
		ShadowBranchSuffix: 1,
	}

	suffix, isNew, err := s.determineSuffix(repo, state)
	if err != nil {
		t.Fatalf("determineSuffix() error = %v", err)
	}

	if suffix != 2 {
		t.Errorf("suffix = %d, want 2 (new suffix for dismissed work)", suffix)
	}
	if !isNew {
		t.Error("isNew should be true (work was dismissed)")
	}
}

// TestDetermineSuffix_ModifiedNoOverlap verifies that when the modified files
// don't overlap with shadow branch files, we get a new suffix.
func TestDetermineSuffix_ModifiedNoOverlap(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	// Create initial commit with two files
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}
	testFile := filepath.Join(dir, "test.txt")
	otherFile := filepath.Join(dir, "other.txt")
	if err := os.WriteFile(testFile, []byte("line1\n"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if err := os.WriteFile(otherFile, []byte("original\n"), 0o644); err != nil {
		t.Fatalf("failed to write other file: %v", err)
	}
	if _, err := worktree.Add("test.txt"); err != nil {
		t.Fatalf("failed to add file: %v", err)
	}
	if _, err := worktree.Add("other.txt"); err != nil {
		t.Fatalf("failed to add file: %v", err)
	}
	initialCommit, err := worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	t.Chdir(dir)

	// Create shadow branch with agent's modifications to test.txt
	shadowBranchName := checkpoint.ShadowBranchNameForCommitWithSuffix(initialCommit.String()[:7], 1)
	createShadowBranchWithFile(t, repo, shadowBranchName, initialCommit, "test.txt", "line1\nline2\n")

	// Modify other.txt (no overlap with shadow branch changes)
	if err := os.WriteFile(otherFile, []byte("modified\n"), 0o644); err != nil {
		t.Fatalf("failed to write modified file: %v", err)
	}

	s := &ManualCommitStrategy{}

	// Session with suffix 1, but modified files don't overlap
	state := &SessionState{
		SessionID:          "test-session",
		BaseCommit:         initialCommit.String(),
		ShadowBranchSuffix: 1,
	}

	suffix, isNew, err := s.determineSuffix(repo, state)
	if err != nil {
		t.Fatalf("determineSuffix() error = %v", err)
	}

	if suffix != 2 {
		t.Errorf("suffix = %d, want 2 (new suffix for no overlap)", suffix)
	}
	if !isNew {
		t.Error("isNew should be true (no relationship to previous work)")
	}
}

// TestDetermineSuffix_LegacyMigration verifies that sessions with suffix=0
// and an existing legacy branch get migrated to suffix 1.
func TestDetermineSuffix_LegacyMigration(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	// Create initial commit
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}
	testFile := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(testFile, []byte("line1\n"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := worktree.Add("test.txt"); err != nil {
		t.Fatalf("failed to add file: %v", err)
	}
	initialCommit, err := worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	t.Chdir(dir)

	// Create a legacy shadow branch (no suffix)
	legacyBranchName := checkpoint.ShadowBranchNameForCommit(initialCommit.String())
	createShadowBranch(t, repo, legacyBranchName, initialCommit)

	// Also make the worktree dirty so we don't trigger the clean worktree case
	if err := os.WriteFile(testFile, []byte("line1\nmodified\n"), 0o644); err != nil {
		t.Fatalf("failed to modify file: %v", err)
	}

	s := &ManualCommitStrategy{}

	// Session with no suffix yet (legacy session)
	state := &SessionState{
		SessionID:          "test-session",
		BaseCommit:         initialCommit.String(),
		ShadowBranchSuffix: 0, // Legacy, no suffix
	}

	suffix, isNew, err := s.determineSuffix(repo, state)
	if err != nil {
		t.Fatalf("determineSuffix() error = %v", err)
	}

	if suffix != 1 {
		t.Errorf("suffix = %d, want 1 (migrated from legacy)", suffix)
	}
	if isNew {
		t.Error("isNew = true, want false (continuing on migrated branch)")
	}

	// Check that legacy branch was renamed to suffixed branch
	newBranchName := checkpoint.ShadowBranchNameForCommitWithSuffix(initialCommit.String()[:7], 1)
	refName := plumbing.NewBranchReferenceName(newBranchName)
	if _, err := repo.Reference(refName, true); err != nil {
		t.Errorf("suffixed branch should exist after migration: %v", err)
	}

	// Legacy branch should be gone
	legacyRefName := plumbing.NewBranchReferenceName(legacyBranchName)
	if _, err := repo.Reference(legacyRefName, true); err == nil {
		t.Error("legacy branch should be deleted after migration")
	}
}

// TestAgentLinesPreserved_ExactMatch verifies that exact line matching works correctly.
func TestAgentLinesPreserved_ExactMatch(t *testing.T) {
	baseContent := "line1\n"
	shadowContent := "line1\nline2\n" // Agent added line2
	workContent := "line1\nline2\n"   // line2 still present

	preserved := agentLinesPreserved(baseContent, shadowContent, workContent)
	if !preserved {
		t.Error("agentLinesPreserved should return true when agent's line is still present")
	}
}

// TestAgentLinesPreserved_LineDismissed verifies that dismissed lines are detected.
func TestAgentLinesPreserved_LineDismissed(t *testing.T) {
	baseContent := "line1\n"
	shadowContent := "line1\nline2\n" // Agent added line2
	workContent := "line1\n"          // line2 was removed by user

	preserved := agentLinesPreserved(baseContent, shadowContent, workContent)
	if preserved {
		t.Error("agentLinesPreserved should return false when agent's line was removed")
	}
}

// TestAgentLinesPreserved_PartialPreserved verifies that partial preservation counts as preserved.
func TestAgentLinesPreserved_PartialPreserved(t *testing.T) {
	baseContent := "line1\n"
	shadowContent := "line1\nline2\nline3\n" // Agent added line2 and line3
	workContent := "line1\nline2\nline4\n"   // line2 still present, line3 changed to line4

	preserved := agentLinesPreserved(baseContent, shadowContent, workContent)
	if !preserved {
		t.Error("agentLinesPreserved should return true when at least one agent line is preserved")
	}
}

// Helper function to create a shadow branch pointing to a commit.
func createShadowBranch(t *testing.T, repo *git.Repository, branchName string, commitHash plumbing.Hash) {
	t.Helper()
	refName := plumbing.NewBranchReferenceName(branchName)
	ref := plumbing.NewHashReference(refName, commitHash)
	if err := repo.Storer.SetReference(ref); err != nil {
		t.Fatalf("failed to create shadow branch %s: %v", branchName, err)
	}
}

// Helper function to create a shadow branch with a modified file in its tree.
func createShadowBranchWithFile(t *testing.T, repo *git.Repository, branchName string, baseCommit plumbing.Hash, filename, content string) {
	t.Helper()

	// Get the base commit
	commit, err := repo.CommitObject(baseCommit)
	if err != nil {
		t.Fatalf("failed to get commit: %v", err)
	}

	baseTree, err := commit.Tree()
	if err != nil {
		t.Fatalf("failed to get tree: %v", err)
	}

	// Build new tree with modified file
	entries := make(map[string]object.TreeEntry)
	if err := checkpoint.FlattenTree(repo, baseTree, "", entries); err != nil {
		t.Fatalf("failed to flatten tree: %v", err)
	}

	// Create blob for the modified content
	blobHash, err := checkpoint.CreateBlobFromContent(repo, []byte(content))
	if err != nil {
		t.Fatalf("failed to create blob: %v", err)
	}

	// Update the file entry
	entries[filename] = object.TreeEntry{
		Name: filename,
		Mode: 0o100644,
		Hash: blobHash,
	}

	// Build the new tree
	newTreeHash, err := checkpoint.BuildTreeFromEntries(repo, entries)
	if err != nil {
		t.Fatalf("failed to build tree: %v", err)
	}

	// Create a commit with the new tree
	sig := object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()}
	newCommit := &object.Commit{
		TreeHash:     newTreeHash,
		ParentHashes: []plumbing.Hash{baseCommit},
		Author:       sig,
		Committer:    sig,
		Message:      "Shadow checkpoint\n\nEntire-Session: test-session\nEntire-Metadata: " + paths.EntireMetadataDir + "/test-session",
	}

	obj := repo.Storer.NewEncodedObject()
	if err := newCommit.Encode(obj); err != nil {
		t.Fatalf("failed to encode commit: %v", err)
	}

	newCommitHash, err := repo.Storer.SetEncodedObject(obj)
	if err != nil {
		t.Fatalf("failed to store commit: %v", err)
	}

	// Create the branch reference
	refName := plumbing.NewBranchReferenceName(branchName)
	ref := plumbing.NewHashReference(refName, newCommitHash)
	if err := repo.Storer.SetReference(ref); err != nil {
		t.Fatalf("failed to create shadow branch %s: %v", branchName, err)
	}
}
