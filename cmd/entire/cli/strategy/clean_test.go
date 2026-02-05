package strategy

import (
	"testing"
	"time"

	"entire.io/cli/cmd/entire/cli/checkpoint"
	"entire.io/cli/cmd/entire/cli/paths"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

func TestIsShadowBranch(t *testing.T) {
	tests := []struct {
		name       string
		branchName string
		want       bool
	}{
		// Valid shadow branches - old format (7+ hex chars)
		{"old format: 7 hex chars", "entire/abc1234", true},
		{"old format: 7 hex chars numeric", "entire/1234567", true},
		{"old format: full commit hash", "entire/abcdef0123456789abcdef0123456789abcdef01", true},
		{"old format: mixed case hex", "entire/AbCdEf1", true},

		// Valid shadow branches - new format with worktree hash (7 hex + dash + 6 hex)
		{"new format: standard", "entire/abc1234-e3b0c4", true},
		{"new format: numeric worktree hash", "entire/1234567-123456", true},
		{"new format: full commit with worktree", "entire/abcdef0123456789-fedcba", true},
		{"new format: mixed case", "entire/AbCdEf1-AbCdEf", true},

		// Invalid patterns
		{"empty after prefix", "entire/", false},
		{"too short commit (6 chars)", "entire/abc123", false},
		{"too short commit (1 char)", "entire/a", false},
		{"non-hex chars in commit", "entire/ghijklm", false},
		{"sessions branch", paths.MetadataBranchName, false},
		{"no prefix", "abc1234", false},
		{"wrong prefix", "feature/abc1234", false},
		{"main branch", "main", false},
		{"master branch", "master", false},
		{"empty string", "", false},
		{"just entire", "entire", false},
		{"entire with slash only", "entire/", false},
		{"worktree hash too short (5 chars)", "entire/abc1234-e3b0c", false},
		{"worktree hash too long (7 chars)", "entire/abc1234-e3b0c44", false},
		{"non-hex in worktree hash", "entire/abc1234-ghijkl", false},
		{"missing commit hash", "entire/-e3b0c4", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsShadowBranch(tt.branchName)
			if got != tt.want {
				t.Errorf("IsShadowBranch(%q) = %v, want %v", tt.branchName, got, tt.want)
			}
		})
	}
}

func TestListShadowBranches(t *testing.T) {
	// Setup: create a temp git repo with various branches
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)

	// Create initial commit so we have something to branch from
	emptyTreeHash := plumbing.NewHash("4b825dc642cb6eb9a060e54bf8d69288fbee4904")
	commitHash, err := createCommit(repo, emptyTreeHash, plumbing.ZeroHash, "initial commit", "test", "test@test.com")
	if err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// Create HEAD reference pointing to master
	headRef := plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("master"))
	if err := repo.Storer.SetReference(headRef); err != nil {
		t.Fatalf("failed to set HEAD: %v", err)
	}
	masterRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName("master"), commitHash)
	if err := repo.Storer.SetReference(masterRef); err != nil {
		t.Fatalf("failed to set master: %v", err)
	}

	// Create various branches
	branches := []struct {
		name     string
		isShadow bool
	}{
		{"entire/abc1234", true},
		{"entire/def5678", true},
		{paths.MetadataBranchName, false}, // Should NOT be listed
		{"feature/foo", false},
		{"main", false},
	}

	for _, b := range branches {
		ref := plumbing.NewHashReference(plumbing.NewBranchReferenceName(b.name), commitHash)
		if err := repo.Storer.SetReference(ref); err != nil {
			t.Fatalf("failed to create branch %s: %v", b.name, err)
		}
	}

	// Test ListShadowBranches
	shadowBranches, err := ListShadowBranches()
	if err != nil {
		t.Fatalf("ListShadowBranches() error = %v", err)
	}

	// Should have exactly 2 shadow branches
	if len(shadowBranches) != 2 {
		t.Errorf("ListShadowBranches() returned %d branches, want 2: %v", len(shadowBranches), shadowBranches)
	}

	// Check that the expected branches are present
	shadowSet := make(map[string]bool)
	for _, b := range shadowBranches {
		shadowSet[b] = true
	}

	if !shadowSet["entire/abc1234"] {
		t.Error("ListShadowBranches() missing 'entire/abc1234'")
	}
	if !shadowSet["entire/def5678"] {
		t.Error("ListShadowBranches() missing 'entire/def5678'")
	}
	if shadowSet[paths.MetadataBranchName] {
		t.Errorf("ListShadowBranches() should not include '%s'", paths.MetadataBranchName)
	}
}

func TestListShadowBranches_Empty(t *testing.T) {
	// Setup: create a temp git repo with no shadow branches
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)

	// Create initial commit
	emptyTreeHash := plumbing.NewHash("4b825dc642cb6eb9a060e54bf8d69288fbee4904")
	commitHash, err := createCommit(repo, emptyTreeHash, plumbing.ZeroHash, "initial commit", "test", "test@test.com")
	if err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// Create HEAD reference
	headRef := plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("master"))
	if err := repo.Storer.SetReference(headRef); err != nil {
		t.Fatalf("failed to set HEAD: %v", err)
	}
	masterRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName("master"), commitHash)
	if err := repo.Storer.SetReference(masterRef); err != nil {
		t.Fatalf("failed to set master: %v", err)
	}

	// Test ListShadowBranches returns empty slice (not nil)
	shadowBranches, err := ListShadowBranches()
	if err != nil {
		t.Fatalf("ListShadowBranches() error = %v", err)
	}

	if shadowBranches == nil {
		t.Error("ListShadowBranches() returned nil, want empty slice")
	}

	if len(shadowBranches) != 0 {
		t.Errorf("ListShadowBranches() returned %d branches, want 0", len(shadowBranches))
	}
}

func TestDeleteShadowBranches(t *testing.T) {
	// Setup: create a temp git repo with shadow branches
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)

	// Create initial commit
	emptyTreeHash := plumbing.NewHash("4b825dc642cb6eb9a060e54bf8d69288fbee4904")
	commitHash, err := createCommit(repo, emptyTreeHash, plumbing.ZeroHash, "initial commit", "test", "test@test.com")
	if err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// Create HEAD reference
	headRef := plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("master"))
	if err := repo.Storer.SetReference(headRef); err != nil {
		t.Fatalf("failed to set HEAD: %v", err)
	}
	masterRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName("master"), commitHash)
	if err := repo.Storer.SetReference(masterRef); err != nil {
		t.Fatalf("failed to set master: %v", err)
	}

	// Create shadow branches
	shadowBranches := []string{"entire/abc1234", "entire/def5678"}
	for _, b := range shadowBranches {
		ref := plumbing.NewHashReference(plumbing.NewBranchReferenceName(b), commitHash)
		if err := repo.Storer.SetReference(ref); err != nil {
			t.Fatalf("failed to create branch %s: %v", b, err)
		}
	}

	// Delete shadow branches
	deleted, failed, err := DeleteShadowBranches(shadowBranches)
	if err != nil {
		t.Fatalf("DeleteShadowBranches() error = %v", err)
	}

	// All should be deleted successfully
	if len(deleted) != 2 {
		t.Errorf("DeleteShadowBranches() deleted %d branches, want 2", len(deleted))
	}
	if len(failed) != 0 {
		t.Errorf("DeleteShadowBranches() failed %d branches, want 0: %v", len(failed), failed)
	}

	// Verify branches are actually deleted
	for _, b := range shadowBranches {
		refName := plumbing.NewBranchReferenceName(b)
		_, err := repo.Reference(refName, true)
		if err == nil {
			t.Errorf("Branch %s still exists after deletion", b)
		}
	}
}

func TestDeleteShadowBranches_NonExistent(t *testing.T) {
	// Setup: create a temp git repo
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)

	// Create initial commit
	emptyTreeHash := plumbing.NewHash("4b825dc642cb6eb9a060e54bf8d69288fbee4904")
	commitHash, err := createCommit(repo, emptyTreeHash, plumbing.ZeroHash, "initial commit", "test", "test@test.com")
	if err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// Create HEAD reference
	headRef := plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("master"))
	if err := repo.Storer.SetReference(headRef); err != nil {
		t.Fatalf("failed to set HEAD: %v", err)
	}
	masterRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName("master"), commitHash)
	if err := repo.Storer.SetReference(masterRef); err != nil {
		t.Fatalf("failed to set master: %v", err)
	}

	// Try to delete non-existent branches
	nonExistent := []string{"entire/doesnotexist"}
	deleted, failed, err := DeleteShadowBranches(nonExistent)
	if err != nil {
		t.Fatalf("DeleteShadowBranches() error = %v", err)
	}

	// Should have one failed branch
	if len(deleted) != 0 {
		t.Errorf("DeleteShadowBranches() deleted %d branches, want 0", len(deleted))
	}
	if len(failed) != 1 {
		t.Errorf("DeleteShadowBranches() failed %d branches, want 1", len(failed))
	}
}

func TestDeleteShadowBranches_Empty(t *testing.T) {
	// Setup: create a temp git repo
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)

	// Delete empty list should return empty results
	deleted, failed, err := DeleteShadowBranches([]string{})
	if err != nil {
		t.Fatalf("DeleteShadowBranches() error = %v", err)
	}

	if len(deleted) != 0 || len(failed) != 0 {
		t.Errorf("DeleteShadowBranches([]) = (%v, %v), want ([], [])", deleted, failed)
	}
}

// TestListOrphanedSessionStates_RecentSessionNotOrphaned tests that recently started
// sessions are NOT marked as orphaned, even if they have no checkpoints yet.
//
// P1 Bug: A session that just started (via InitializeSession) but hasn't created
// its first checkpoint yet would be incorrectly marked as orphaned because it has:
// - A session state file
// - No checkpoints on entire/sessions
// - No shadow branch (if using auto-commit strategy, or before first checkpoint)
//
// This test should FAIL with the current implementation, demonstrating the bug.
func TestListOrphanedSessionStates_RecentSessionNotOrphaned(t *testing.T) {
	// Setup: create a temp git repo
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)

	// Create initial commit
	emptyTreeHash := plumbing.NewHash("4b825dc642cb6eb9a060e54bf8d69288fbee4904")
	commitHash, err := createCommit(repo, emptyTreeHash, plumbing.ZeroHash, "initial commit", "test", "test@test.com")
	if err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// Create HEAD reference
	headRef := plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("master"))
	if err := repo.Storer.SetReference(headRef); err != nil {
		t.Fatalf("failed to set HEAD: %v", err)
	}
	masterRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName("master"), commitHash)
	if err := repo.Storer.SetReference(masterRef); err != nil {
		t.Fatalf("failed to set master: %v", err)
	}

	// Create a session state file that was JUST started (simulating InitializeSession)
	// This session has no checkpoints and no shadow branch yet
	state := &SessionState{
		SessionID:       "recent-session-123",
		BaseCommit:      commitHash.String(), // Full 40-char hash
		StartedAt:       time.Now(),          // Just started!
		CheckpointCount: 0,                   // No checkpoints yet
	}
	if err := SaveSessionState(state); err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}

	// List orphaned session states
	orphaned, err := ListOrphanedSessionStates()
	if err != nil {
		t.Fatalf("ListOrphanedSessionStates() error = %v", err)
	}

	// The recently started session should NOT be marked as orphaned
	// because it's actively being used (StartedAt is recent)
	for _, item := range orphaned {
		if item.ID == "recent-session-123" {
			t.Errorf("ListOrphanedSessionStates() incorrectly marked recent session as orphaned.\n"+
				"Session was started %v ago, which is too recent to be considered orphaned.\n"+
				"Expected: session to be protected from cleanup during active use.\n"+
				"Got: session marked as orphaned with reason: %q",
				time.Since(state.StartedAt), item.Reason)
		}
	}
}

// TestListOrphanedSessionStates_ShadowBranchMatching tests that session states are correctly
// matched against shadow branches using worktree-specific naming.
//
// Shadow branches use the format "entire/<commit[:7]>-<worktreeHash[:6]>" and session states
// store both the full BaseCommit and WorktreeID. The comparison constructs the expected
// branch name from these fields and checks if it exists.
func TestListOrphanedSessionStates_ShadowBranchMatching(t *testing.T) {
	// Setup: create a temp git repo
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)

	// Create initial commit
	emptyTreeHash := plumbing.NewHash("4b825dc642cb6eb9a060e54bf8d69288fbee4904")
	commitHash, err := createCommit(repo, emptyTreeHash, plumbing.ZeroHash, "initial commit", "test", "test@test.com")
	if err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// Create HEAD reference
	headRef := plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("master"))
	if err := repo.Storer.SetReference(headRef); err != nil {
		t.Fatalf("failed to set HEAD: %v", err)
	}
	masterRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName("master"), commitHash)
	if err := repo.Storer.SetReference(masterRef); err != nil {
		t.Fatalf("failed to set master: %v", err)
	}

	// Create a shadow branch using worktree-specific naming (matching real behavior)
	// Real code: shadowBranch := checkpoint.ShadowBranchNameForCommit(baseCommit, worktreeID)
	fullHash := commitHash.String()
	worktreeID := "" // Main worktree
	shadowBranchName := checkpoint.ShadowBranchNameForCommit(fullHash, worktreeID)
	shadowRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName(shadowBranchName), commitHash)
	if err := repo.Storer.SetReference(shadowRef); err != nil {
		t.Fatalf("failed to create shadow branch: %v", err)
	}

	// Create a session state with the FULL 40-char hash and WorktreeID (matching real behavior)
	// Real code: state.BaseCommit = head.Hash().String(), state.WorktreeID = worktreeID
	state := &SessionState{
		SessionID:       "session-with-shadow-branch",
		BaseCommit:      fullHash, // Full 40-char hash
		WorktreeID:      worktreeID,
		StartedAt:       time.Now().Add(-1 * time.Hour),
		CheckpointCount: 1,
	}
	if err := SaveSessionState(state); err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}

	// Verify the shadow branch exists with worktree-specific name
	shadowBranches, err := ListShadowBranches()
	if err != nil {
		t.Fatalf("ListShadowBranches() error = %v", err)
	}
	if len(shadowBranches) != 1 || shadowBranches[0] != shadowBranchName {
		t.Fatalf("Expected shadow branch %q, got %v", shadowBranchName, shadowBranches)
	}

	// Log info about the branch naming
	t.Logf("Shadow branch name: %q", shadowBranchName)
	t.Logf("Session BaseCommit (40 chars): %q", fullHash)
	t.Logf("Session WorktreeID: %q", worktreeID)

	// List orphaned session states
	orphaned, err := ListOrphanedSessionStates()
	if err != nil {
		t.Fatalf("ListOrphanedSessionStates() error = %v", err)
	}

	// The session should NOT be marked as orphaned because it HAS a shadow branch!
	// With worktree-specific naming, the expected branch name is constructed from
	// BaseCommit and WorktreeID, which should match the actual shadow branch.
	for _, item := range orphaned {
		if item.ID == "session-with-shadow-branch" {
			t.Errorf("ListOrphanedSessionStates() incorrectly marked session as orphaned.\n"+
				"Shadow branch exists: %q\n"+
				"Session BaseCommit: %q\n"+
				"Session WorktreeID: %q\n"+
				"Expected branch: %q\n"+
				"Got: session marked as orphaned with reason: %q",
				shadowBranchName, fullHash, worktreeID,
				checkpoint.ShadowBranchNameForCommit(fullHash, worktreeID), item.Reason)
		}
	}
}
