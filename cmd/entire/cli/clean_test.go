package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/spf13/cobra"
)

// newTestCleanCmd creates a cobra.Command with captured stdout/stderr for testing.
func newTestCleanCmd(t *testing.T) (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	return cmd, &stdout, &stderr
}

func setupCleanTestRepo(t *testing.T) (*git.Repository, plumbing.Hash) {
	t.Helper()

	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)
	paths.ClearWorktreeRootCache()

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

// createSessionStateFile creates a session state JSON file in .git/entire-sessions/.
func createSessionStateFile(t *testing.T, repoRoot string, sessionID string, commitHash plumbing.Hash) string {
	t.Helper()

	sessionStateDir := filepath.Join(repoRoot, ".git", "entire-sessions")
	if err := os.MkdirAll(sessionStateDir, 0o755); err != nil {
		t.Fatalf("failed to create session state dir: %v", err)
	}

	sessionFile := filepath.Join(sessionStateDir, sessionID+".json")
	sessionState := map[string]any{
		"session_id":       sessionID,
		"base_commit":      commitHash.String(),
		"checkpoint_count": 1,
		"started_at":       time.Now().Format(time.RFC3339),
	}
	sessionData, err := json.Marshal(sessionState)
	if err != nil {
		t.Fatalf("failed to marshal session state: %v", err)
	}
	if err := os.WriteFile(sessionFile, sessionData, 0o600); err != nil {
		t.Fatalf("failed to write session state file: %v", err)
	}
	return sessionFile
}

// --- Default mode tests (current HEAD cleanup) ---

func TestCleanCmd_DefaultMode_NothingToClean(t *testing.T) {
	setupCleanTestRepo(t)

	cmd := newCleanCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--force"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("clean command error = %v", err)
	}
}

func TestCleanCmd_DefaultMode_WithForce(t *testing.T) {
	repo, commitHash := setupCleanTestRepo(t)

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}
	worktreePath := wt.Filesystem.Root()
	worktreeID, err := paths.GetWorktreeID(worktreePath)
	if err != nil {
		t.Fatalf("failed to get worktree ID: %v", err)
	}

	// Create shadow branch
	shadowBranch := checkpoint.ShadowBranchNameForCommit(commitHash.String(), worktreeID)
	shadowRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName(shadowBranch), commitHash)
	if err := repo.Storer.SetReference(shadowRef); err != nil {
		t.Fatalf("failed to create shadow branch: %v", err)
	}

	// Create session state file
	sessionFile := createSessionStateFile(t, worktreePath, "2026-02-02-test123", commitHash)

	cmd := newCleanCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--force"})

	err = cmd.Execute()
	if err != nil {
		t.Fatalf("clean command error = %v", err)
	}

	// Verify shadow branch deleted
	refName := plumbing.NewBranchReferenceName(shadowBranch)
	if _, err := repo.Reference(refName, true); err == nil {
		t.Error("shadow branch should be deleted")
	}

	// Verify session state file deleted
	if _, err := os.Stat(sessionFile); !os.IsNotExist(err) {
		t.Error("session state file should be deleted")
	}
}

func TestCleanCmd_DefaultMode_DryRun(t *testing.T) {
	repo, commitHash := setupCleanTestRepo(t)

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}
	worktreePath := wt.Filesystem.Root()
	worktreeID, err := paths.GetWorktreeID(worktreePath)
	if err != nil {
		t.Fatalf("failed to get worktree ID: %v", err)
	}

	// Create shadow branch
	shadowBranch := checkpoint.ShadowBranchNameForCommit(commitHash.String(), worktreeID)
	shadowRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName(shadowBranch), commitHash)
	if err := repo.Storer.SetReference(shadowRef); err != nil {
		t.Fatalf("failed to create shadow branch: %v", err)
	}

	// Create session state file
	sessionFile := createSessionStateFile(t, worktreePath, "2026-02-02-test123", commitHash)

	cmd := newCleanCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--dry-run"})

	err = cmd.Execute()
	if err != nil {
		t.Fatalf("clean command error = %v", err)
	}

	output := stdout.String()
	if !strings.Contains(output, "Would clean") {
		t.Errorf("Expected 'Would clean' in output, got: %s", output)
	}
	if !strings.Contains(output, shadowBranch) {
		t.Errorf("Expected shadow branch name in output, got: %s", output)
	}
	if !strings.Contains(output, "2026-02-02-test123") {
		t.Errorf("Expected session ID in output, got: %s", output)
	}

	// Verify nothing was deleted
	refName := plumbing.NewBranchReferenceName(shadowBranch)
	if _, err := repo.Reference(refName, true); err != nil {
		t.Error("shadow branch should still exist after dry-run")
	}
	if _, err := os.Stat(sessionFile); os.IsNotExist(err) {
		t.Error("session state file should still exist after dry-run")
	}
}

func TestCleanCmd_DefaultMode_DryRun_NothingToClean(t *testing.T) {
	setupCleanTestRepo(t)

	cmd := newCleanCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--dry-run"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("clean command error = %v", err)
	}

	output := stdout.String()
	if !strings.Contains(output, "Nothing to clean") {
		t.Errorf("Expected 'Nothing to clean' message, got: %s", output)
	}
}

func TestCleanCmd_DefaultMode_SessionsWithoutShadowBranch(t *testing.T) {
	repo, commitHash := setupCleanTestRepo(t)

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}
	worktreePath := wt.Filesystem.Root()

	// Create session state files WITHOUT a shadow branch
	sessionFile := createSessionStateFile(t, worktreePath, "2026-02-02-orphaned", commitHash)

	cmd := newCleanCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--force"})

	err = cmd.Execute()
	if err != nil {
		t.Fatalf("clean command error = %v", err)
	}

	// Verify session state file deleted
	if _, err := os.Stat(sessionFile); !os.IsNotExist(err) {
		t.Error("session state file should be deleted even without shadow branch")
	}
}

func TestCleanCmd_DefaultMode_MultipleSessions(t *testing.T) {
	repo, commitHash := setupCleanTestRepo(t)

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}
	worktreePath := wt.Filesystem.Root()
	worktreeID, err := paths.GetWorktreeID(worktreePath)
	if err != nil {
		t.Fatalf("failed to get worktree ID: %v", err)
	}

	// Create shadow branch
	shadowBranch := checkpoint.ShadowBranchNameForCommit(commitHash.String(), worktreeID)
	shadowRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName(shadowBranch), commitHash)
	if err := repo.Storer.SetReference(shadowRef); err != nil {
		t.Fatalf("failed to create shadow branch: %v", err)
	}

	// Create multiple session state files
	session1File := createSessionStateFile(t, worktreePath, "2026-02-02-session1", commitHash)
	session2File := createSessionStateFile(t, worktreePath, "2026-02-02-session2", commitHash)

	cmd := newCleanCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--force"})

	err = cmd.Execute()
	if err != nil {
		t.Fatalf("clean command error = %v", err)
	}

	// Verify both session files deleted
	if _, err := os.Stat(session1File); !os.IsNotExist(err) {
		t.Error("session1 file should be deleted")
	}
	if _, err := os.Stat(session2File); !os.IsNotExist(err) {
		t.Error("session2 file should be deleted")
	}

	// Verify shadow branch deleted
	refName := plumbing.NewBranchReferenceName(shadowBranch)
	if _, err := repo.Reference(refName, true); err == nil {
		t.Error("shadow branch should be deleted")
	}
}

func TestCleanCmd_DefaultMode_NotGitRepo(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()

	cmd := newCleanCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := cmd.Execute()
	if err == nil {
		t.Fatal("clean command should return error for non-git directory")
	}
	if !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("Expected 'not a git repository' error, got: %v", err)
	}
}

// --- --all mode tests (repo-wide orphan cleanup) ---

func TestCleanCmd_All_NoOrphanedItems(t *testing.T) {
	setupCleanTestRepo(t)

	cmd := newCleanCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"--all"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("clean --all error = %v", err)
	}

	output := stdout.String()
	if !strings.Contains(output, "No orphaned items") {
		t.Errorf("Expected 'No orphaned items' message, got: %s", output)
	}
}

func TestCleanCmd_All_PreviewMode(t *testing.T) {
	repo, commitHash := setupCleanTestRepo(t)

	// Create shadow branches
	shadowBranches := []string{"entire/abc1234", "entire/def5678"}
	for _, b := range shadowBranches {
		ref := plumbing.NewHashReference(plumbing.NewBranchReferenceName(b), commitHash)
		if err := repo.Storer.SetReference(ref); err != nil {
			t.Fatalf("failed to create branch %s: %v", b, err)
		}
	}

	// Also create entire/checkpoints/v1 (should NOT be listed)
	sessionsRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), commitHash)
	if err := repo.Storer.SetReference(sessionsRef); err != nil {
		t.Fatalf("failed to create %s: %v", paths.MetadataBranchName, err)
	}

	cmd := newCleanCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"--all", "--dry-run"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("clean --all --dry-run error = %v", err)
	}

	output := stdout.String()

	if !strings.Contains(output, "to clean") {
		t.Errorf("Expected 'to clean' in output, got: %s", output)
	}
	if !strings.Contains(output, "entire/abc1234") {
		t.Errorf("Expected 'entire/abc1234' in output, got: %s", output)
	}
	if !strings.Contains(output, "entire/def5678") {
		t.Errorf("Expected 'entire/def5678' in output, got: %s", output)
	}
	if strings.Contains(output, paths.MetadataBranchName) {
		t.Errorf("Should not list '%s', got: %s", paths.MetadataBranchName, output)
	}
	if !strings.Contains(output, "without --dry-run") {
		t.Errorf("Expected '--dry-run' hint in output, got: %s", output)
	}

	// Branches should still exist (dry-run doesn't delete)
	for _, b := range shadowBranches {
		refName := plumbing.NewBranchReferenceName(b)
		if _, err := repo.Reference(refName, true); err != nil {
			t.Errorf("Branch %s should still exist after dry-run", b)
		}
	}
}

func TestCleanCmd_All_DryRun(t *testing.T) {
	repo, commitHash := setupCleanTestRepo(t)

	shadowBranches := []string{"entire/abc1234"}
	for _, b := range shadowBranches {
		ref := plumbing.NewHashReference(plumbing.NewBranchReferenceName(b), commitHash)
		if err := repo.Storer.SetReference(ref); err != nil {
			t.Fatalf("failed to create branch %s: %v", b, err)
		}
	}

	cmd := newCleanCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"--all", "--dry-run"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("clean --all --dry-run error = %v", err)
	}

	output := stdout.String()
	if !strings.Contains(output, "to clean") {
		t.Errorf("Expected 'to clean' in output, got: %s", output)
	}
	if !strings.Contains(output, "without --dry-run") {
		t.Errorf("Expected '--dry-run' hint in output, got: %s", output)
	}

	// Branches should still exist
	for _, b := range shadowBranches {
		refName := plumbing.NewBranchReferenceName(b)
		if _, err := repo.Reference(refName, true); err != nil {
			t.Errorf("Branch %s should still exist after dry-run", b)
		}
	}
}

func TestCleanCmd_All_ForceMode(t *testing.T) {
	repo, commitHash := setupCleanTestRepo(t)

	shadowBranches := []string{"entire/abc1234", "entire/def5678"}
	for _, b := range shadowBranches {
		ref := plumbing.NewHashReference(plumbing.NewBranchReferenceName(b), commitHash)
		if err := repo.Storer.SetReference(ref); err != nil {
			t.Fatalf("failed to create branch %s: %v", b, err)
		}
	}

	cmd := newCleanCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"--all", "--force"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("clean --all --force error = %v", err)
	}

	output := stdout.String()
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

func TestCleanCmd_All_SessionsBranchPreserved(t *testing.T) {
	repo, commitHash := setupCleanTestRepo(t)

	shadowRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName("entire/abc1234"), commitHash)
	if err := repo.Storer.SetReference(shadowRef); err != nil {
		t.Fatalf("failed to create shadow branch: %v", err)
	}

	sessionsRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), commitHash)
	if err := repo.Storer.SetReference(sessionsRef); err != nil {
		t.Fatalf("failed to create entire/checkpoints/v1: %v", err)
	}

	cmd := newCleanCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"--all", "--force"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("clean --all --force error = %v", err)
	}

	// Shadow branch should be deleted
	refName := plumbing.NewBranchReferenceName("entire/abc1234")
	if _, err := repo.Reference(refName, true); err == nil {
		t.Error("Shadow branch should be deleted")
	}

	// Sessions branch should still exist
	sessionsRefName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	if _, err := repo.Reference(sessionsRefName, true); err != nil {
		t.Error("entire/checkpoints/v1 branch should be preserved")
	}
}

func TestCleanCmd_All_NotGitRepository(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()

	cmd := newCleanCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"--all"})

	err := cmd.Execute()
	// Should return error for non-git directory
	if err == nil {
		t.Error("clean --all should return error for non-git directory")
	}
}

func TestCleanCmd_All_Subdirectory(t *testing.T) {
	repo, commitHash := setupCleanTestRepo(t)

	shadowRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName("entire/abc1234"), commitHash)
	if err := repo.Storer.SetReference(shadowRef); err != nil {
		t.Fatalf("failed to create shadow branch: %v", err)
	}

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
	paths.ClearWorktreeRootCache()

	cmd := newCleanCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"--all", "--dry-run"})

	err = cmd.Execute()
	if err != nil {
		t.Fatalf("clean --all --dry-run from subdirectory error = %v", err)
	}

	output := stdout.String()
	if !strings.Contains(output, "entire/abc1234") {
		t.Errorf("Should find shadow branches from subdirectory, got: %s", output)
	}
}

// --- runCleanAllWithItems unit tests ---

func TestRunCleanAllWithItems_PartialFailure(t *testing.T) {
	repo, commitHash := setupCleanTestRepo(t)

	shadowRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName("entire/abc1234"), commitHash)
	if err := repo.Storer.SetReference(shadowRef); err != nil {
		t.Fatalf("failed to create shadow branch: %v", err)
	}

	items := []strategy.CleanupItem{
		{Type: strategy.CleanupTypeShadowBranch, ID: "entire/abc1234", Reason: "test"},
		{Type: strategy.CleanupTypeShadowBranch, ID: "entire/nonexistent1234567", Reason: "test"},
	}

	cmd, stdout, stderr := newTestCleanCmd(t)
	err := runCleanAllWithItems(cmd.Context(), cmd, true, false, items, nil)

	if err == nil {
		t.Fatal("runCleanAllWithItems() should return error when items fail to delete")
	}
	if !strings.Contains(err.Error(), "failed to delete 1 item") {
		t.Errorf("Error should mention 'failed to delete 1 item', got: %v", err)
	}
	// Verify singular (not "1 items")
	if strings.Contains(err.Error(), "1 items") {
		t.Errorf("Error should use singular 'item' for count 1, got: %v", err)
	}

	// Output should show the successful deletion with singular grammar
	output := stdout.String()
	if !strings.Contains(output, "✓ Deleted 1 item:") {
		t.Errorf("Output should show '✓ Deleted 1 item:', got: %s", output)
	}
	// Stderr should show the failure with singular grammar
	errOutput := stderr.String()
	if !strings.Contains(errOutput, "Failed to delete 1 item:") {
		t.Errorf("Stderr should show 'Failed to delete 1 item:', got: %s", errOutput)
	}
}

func TestRunCleanAllWithItems_AllFailures(t *testing.T) {
	setupCleanTestRepo(t)

	items := []strategy.CleanupItem{
		{Type: strategy.CleanupTypeShadowBranch, ID: "entire/nonexistent1234567", Reason: "test"},
		{Type: strategy.CleanupTypeShadowBranch, ID: "entire/alsononexistent", Reason: "test"},
	}

	cmd, stdout, stderr := newTestCleanCmd(t)
	err := runCleanAllWithItems(cmd.Context(), cmd, true, false, items, nil)

	if err == nil {
		t.Fatal("runCleanAllWithItems() should return error when items fail to delete")
	}
	if !strings.Contains(err.Error(), "failed to delete 2 items") {
		t.Errorf("Error should mention 'failed to delete 2 items', got: %v", err)
	}

	output := stdout.String()
	if strings.Contains(output, "✓ Deleted") {
		t.Errorf("Output should not show successful deletions, got: %s", output)
	}
	// Failures are written to stderr
	errOutput := stderr.String()
	if !strings.Contains(errOutput, "Failed to delete 2 items:") {
		t.Errorf("Stderr should show 'Failed to delete 2 items:', got: %s", errOutput)
	}
}

func TestRunCleanAllWithItems_NoItems(t *testing.T) {
	setupCleanTestRepo(t)

	cmd, stdout, _ := newTestCleanCmd(t)
	err := runCleanAllWithItems(cmd.Context(), cmd, false, false, []strategy.CleanupItem{}, nil)
	if err != nil {
		t.Fatalf("runCleanAllWithItems() error = %v", err)
	}

	output := stdout.String()
	if !strings.Contains(output, "No orphaned items") {
		t.Errorf("Expected 'No orphaned items' message, got: %s", output)
	}
}

func TestRunCleanAllWithItems_MixedTypes_Preview(t *testing.T) {
	setupCleanTestRepo(t)

	items := []strategy.CleanupItem{
		{Type: strategy.CleanupTypeShadowBranch, ID: "entire/abc1234", Reason: "test"},
		{Type: strategy.CleanupTypeSessionState, ID: "session-123", Reason: "no checkpoints"},
		{Type: strategy.CleanupTypeCheckpoint, ID: "checkpoint-abc", Reason: "orphaned"},
	}

	cmd, stdout, _ := newTestCleanCmd(t)
	err := runCleanAllWithItems(cmd.Context(), cmd, false, true, items, nil)
	if err != nil {
		t.Fatalf("runCleanAllWithItems() error = %v", err)
	}

	output := stdout.String()
	if !strings.Contains(output, "Shadow branches") {
		t.Errorf("Expected 'Shadow branches' section, got: %s", output)
	}
	if !strings.Contains(output, "Session states") {
		t.Errorf("Expected 'Session states' section, got: %s", output)
	}
	if !strings.Contains(output, "Checkpoint metadata") {
		t.Errorf("Expected 'Checkpoint metadata' section, got: %s", output)
	}
	if !strings.Contains(output, "Found 3 items to clean") {
		t.Errorf("Expected 'Found 3 items to clean', got: %s", output)
	}
}

// --- Flag validation tests ---

func TestCleanCmd_MutuallyExclusiveFlags(t *testing.T) {
	setupCleanTestRepo(t)

	cmd := newCleanCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--all", "--session", "test-session"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("--all and --session should be mutually exclusive")
	}
	if !strings.Contains(err.Error(), "cannot be used together") {
		t.Errorf("Expected mutual exclusion error, got: %v", err)
	}
}
