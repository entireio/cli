package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"entire.io/cli/cmd/entire/cli/checkpoint"
	"entire.io/cli/cmd/entire/cli/paths"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// setupResetTestRepo is a test helper that creates a git repository with an initial commit.
// This is intentionally duplicated from clean_test.go to keep test files independent.
//
//nolint:dupl // Test setup code duplication is acceptable for test isolation
func setupResetTestRepo(t *testing.T) (*git.Repository, plumbing.Hash) {
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

func TestResetCmd_NothingToReset(t *testing.T) {
	setupResetTestRepo(t)

	// No shadow branch and no sessions - should report nothing to reset
	cmd := newResetCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--force"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("reset command error = %v", err)
	}

	// Command should succeed without deleting anything
}

func TestResetCmd_WithForce(t *testing.T) {
	repo, commitHash := setupResetTestRepo(t)

	// Get worktree path and ID for shadow branch naming
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}
	worktreePath := wt.Filesystem.Root()
	worktreeID, err := paths.GetWorktreeID(worktreePath)
	if err != nil {
		t.Fatalf("failed to get worktree ID: %v", err)
	}

	// Create shadow branch with correct naming format
	shadowBranch := checkpoint.ShadowBranchNameForCommit(commitHash.String(), worktreeID)
	shadowRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName(shadowBranch), commitHash)
	if err := repo.Storer.SetReference(shadowRef); err != nil {
		t.Fatalf("failed to create shadow branch: %v", err)
	}

	// Create session state file
	repoRoot := worktreePath
	sessionStateDir := filepath.Join(repoRoot, ".git", "entire-sessions")
	if err := os.MkdirAll(sessionStateDir, 0o755); err != nil {
		t.Fatalf("failed to create session state dir: %v", err)
	}

	sessionFile := filepath.Join(sessionStateDir, "2026-02-02-test123.json")
	sessionState := map[string]any{
		"session_id":       "2026-02-02-test123",
		"base_commit":      commitHash.String(),
		"checkpoint_count": 1,
	}
	sessionData, err := json.Marshal(sessionState)
	if err != nil {
		t.Fatalf("failed to marshal session state: %v", err)
	}
	if err := os.WriteFile(sessionFile, sessionData, 0o600); err != nil {
		t.Fatalf("failed to write session state file: %v", err)
	}

	// Run reset command with force
	cmd := newResetCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--force"})

	err = cmd.Execute()
	if err != nil {
		t.Fatalf("reset command error = %v", err)
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

func TestResetCmd_SessionsWithoutShadowBranch(t *testing.T) {
	repo, commitHash := setupResetTestRepo(t)

	// Create session state files WITHOUT a shadow branch
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}
	repoRoot := wt.Filesystem.Root()
	sessionStateDir := filepath.Join(repoRoot, ".git", "entire-sessions")
	if err := os.MkdirAll(sessionStateDir, 0o755); err != nil {
		t.Fatalf("failed to create session state dir: %v", err)
	}

	sessionFile := filepath.Join(sessionStateDir, "2026-02-02-orphaned.json")
	sessionState := map[string]any{
		"session_id":       "2026-02-02-orphaned",
		"base_commit":      commitHash.String(),
		"checkpoint_count": 1,
	}
	sessionData, err := json.Marshal(sessionState)
	if err != nil {
		t.Fatalf("failed to marshal session state: %v", err)
	}
	if err := os.WriteFile(sessionFile, sessionData, 0o600); err != nil {
		t.Fatalf("failed to write session state file: %v", err)
	}

	// Run reset command with force
	cmd := newResetCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--force"})

	err = cmd.Execute()
	if err != nil {
		t.Fatalf("reset command error = %v", err)
	}

	// Verify session state file deleted (even without shadow branch)
	if _, err := os.Stat(sessionFile); !os.IsNotExist(err) {
		t.Error("session state file should be deleted even without shadow branch")
	}

	// Verify no shadow branch was created or exists
	shadowBranch := "entire/" + commitHash.String()[:7]
	refName := plumbing.NewBranchReferenceName(shadowBranch)
	if _, err := repo.Reference(refName, true); err == nil {
		t.Error("shadow branch should not exist")
	}
}

func TestResetCmd_NotGitRepo(t *testing.T) {
	// Create temp dir (not git repo)
	dir := t.TempDir()
	t.Chdir(dir)
	paths.ClearRepoRootCache()

	// Run reset
	cmd := newResetCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := cmd.Execute()
	if err == nil {
		t.Fatal("reset command should return error for non-git directory")
	}

	// Verify error message
	output := stderr.String()
	if !strings.Contains(output, "not a git repository") {
		t.Errorf("Expected 'not a git repository' message, got: %s", output)
	}
}

func TestResetCmd_AutoCommitStrategy(t *testing.T) {
	setupResetTestRepo(t)

	// Write auto-commit strategy settings
	writeSettings(t, `{"strategy": "auto-commit", "enabled": true}`)

	// Run reset
	cmd := newResetCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := cmd.Execute()
	if err == nil {
		t.Fatal("reset command should return error for auto-commit strategy")
	}

	// Verify helpful error message
	output := stderr.String()
	if !strings.Contains(output, "strategy auto-commit does not support reset") {
		t.Errorf("Expected message about auto-commit strategy, got: %s", output)
	}
}

func TestResetCmd_MultipleSessions(t *testing.T) {
	repo, commitHash := setupResetTestRepo(t)

	// Get worktree path and ID for shadow branch naming
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}
	worktreePath := wt.Filesystem.Root()
	worktreeID, err := paths.GetWorktreeID(worktreePath)
	if err != nil {
		t.Fatalf("failed to get worktree ID: %v", err)
	}

	// Create shadow branch with correct naming format
	shadowBranch := checkpoint.ShadowBranchNameForCommit(commitHash.String(), worktreeID)
	shadowRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName(shadowBranch), commitHash)
	if err := repo.Storer.SetReference(shadowRef); err != nil {
		t.Fatalf("failed to create shadow branch: %v", err)
	}

	// Create multiple session state files
	repoRoot := worktreePath
	sessionStateDir := filepath.Join(repoRoot, ".git", "entire-sessions")
	if err := os.MkdirAll(sessionStateDir, 0o755); err != nil {
		t.Fatalf("failed to create session state dir: %v", err)
	}

	session1File := filepath.Join(sessionStateDir, "2026-02-02-session1.json")
	session1State := map[string]any{
		"session_id":       "2026-02-02-session1",
		"base_commit":      commitHash.String(),
		"checkpoint_count": 1,
	}
	session1Data, err := json.Marshal(session1State)
	if err != nil {
		t.Fatalf("failed to marshal session1 state: %v", err)
	}
	if err := os.WriteFile(session1File, session1Data, 0o600); err != nil {
		t.Fatalf("failed to write session1 state file: %v", err)
	}

	session2File := filepath.Join(sessionStateDir, "2026-02-02-session2.json")
	session2State := map[string]any{
		"session_id":       "2026-02-02-session2",
		"base_commit":      commitHash.String(),
		"checkpoint_count": 2,
	}
	session2Data, err := json.Marshal(session2State)
	if err != nil {
		t.Fatalf("failed to marshal session2 state: %v", err)
	}
	if err := os.WriteFile(session2File, session2Data, 0o600); err != nil {
		t.Fatalf("failed to write session2 state file: %v", err)
	}

	// Run reset with force
	cmd := newResetCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--force"})

	err = cmd.Execute()
	if err != nil {
		t.Fatalf("reset command error = %v", err)
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
