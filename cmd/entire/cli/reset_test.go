package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
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

func TestResetCmd_IsDeprecated(t *testing.T) {
	cmd := newResetCmd()
	if cmd.Deprecated == "" {
		t.Error("reset command should have Deprecated field set")
	}
	if !strings.Contains(cmd.Deprecated, "entire clean") {
		t.Errorf("Deprecated message should mention 'entire clean', got: %s", cmd.Deprecated)
	}
}

func TestResetCmd_NothingToReset(t *testing.T) {
	setupResetTestRepo(t)

	cmd := newResetCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--force"})

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("reset command error = %v", err)
	}
}

func TestResetCmd_WithForce(t *testing.T) {
	repo, commitHash := setupResetTestRepo(t)

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

	if output := stdout.String(); !strings.Contains(output, "✓ Deleted shadow branch") {
		t.Fatalf("expected reset success output, got: %q", output)
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

func TestResetCmd_NotGitRepo(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()

	cmd := newResetCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := cmd.Execute()
	if err == nil {
		t.Fatal("reset command should return error for non-git directory")
	}
}
