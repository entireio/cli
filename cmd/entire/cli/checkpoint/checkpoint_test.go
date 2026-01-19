package checkpoint

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"entire.io/cli/cmd/entire/cli/paths"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func TestCheckpointType_Values(t *testing.T) {
	// Verify the enum values are distinct
	if Temporary == Committed {
		t.Error("Temporary and Committed should have different values")
	}

	// Verify Temporary is the zero value (default for Type)
	var defaultType Type
	if defaultType != Temporary {
		t.Errorf("expected zero value of Type to be Temporary, got %d", defaultType)
	}
}

func TestCopyMetadataDir_SkipsSymlinks(t *testing.T) {
	// Create a temp directory for the test
	tempDir := t.TempDir()

	// Initialize a git repository
	repo, err := git.PlainInit(tempDir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	// Create metadata directory structure
	metadataDir := filepath.Join(tempDir, "metadata")
	if err := os.MkdirAll(metadataDir, 0755); err != nil {
		t.Fatalf("failed to create metadata dir: %v", err)
	}

	// Create a regular file that should be included
	regularFile := filepath.Join(metadataDir, "regular.txt")
	if err := os.WriteFile(regularFile, []byte("regular content"), 0644); err != nil {
		t.Fatalf("failed to create regular file: %v", err)
	}

	// Create a sensitive file outside the metadata directory
	sensitiveFile := filepath.Join(tempDir, "sensitive.txt")
	if err := os.WriteFile(sensitiveFile, []byte("SECRET DATA"), 0644); err != nil {
		t.Fatalf("failed to create sensitive file: %v", err)
	}

	// Create a symlink inside metadata directory pointing to the sensitive file
	symlinkPath := filepath.Join(metadataDir, "sneaky-link")
	if err := os.Symlink(sensitiveFile, symlinkPath); err != nil {
		t.Fatalf("failed to create symlink: %v", err)
	}

	// Create GitStore and call copyMetadataDir
	store := NewGitStore(repo)
	entries := make(map[string]object.TreeEntry)

	err = store.copyMetadataDir(metadataDir, "checkpoint/", entries)
	if err != nil {
		t.Fatalf("copyMetadataDir failed: %v", err)
	}

	// Verify regular file was included
	if _, ok := entries["checkpoint/regular.txt"]; !ok {
		t.Error("regular.txt should be included in entries")
	}

	// Verify symlink was NOT included (security fix)
	if _, ok := entries["checkpoint/sneaky-link"]; ok {
		t.Error("symlink should NOT be included in entries - this would allow reading files outside the metadata directory")
	}

	// Verify the correct number of entries
	if len(entries) != 1 {
		t.Errorf("expected 1 entry, got %d", len(entries))
	}
}

// TestEnsureSessionsBranch_CreatesReadme verifies that the README.md is created
// in the root of the entire/sessions branch when it's first initialized.
func TestEnsureSessionsBranch_CreatesReadme(t *testing.T) {
	tempDir := t.TempDir()

	// Initialize a git repository with an initial commit
	repo, err := git.PlainInit(tempDir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	// Create worktree and make initial commit
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	readmeFile := filepath.Join(tempDir, "README.md")
	if err := os.WriteFile(readmeFile, []byte("# Test"), 0o644); err != nil {
		t.Fatalf("failed to write README: %v", err)
	}
	if _, err := worktree.Add("README.md"); err != nil {
		t.Fatalf("failed to add README: %v", err)
	}
	if _, err := worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com"},
	}); err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	// Create checkpoint store and write a committed checkpoint
	// (this triggers ensureSessionsBranch)
	store := NewGitStore(repo)
	err = store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID: "a1b2c3d4e5f6",
		SessionID:    "test-session-123",
		Strategy:     "manual-commit",
		Transcript:   []byte("test transcript"),
		AuthorName:   "Test Author",
		AuthorEmail:  "test@example.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() error = %v", err)
	}

	// Get the sessions branch
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	if err != nil {
		t.Fatalf("failed to get sessions branch: %v", err)
	}

	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		t.Fatalf("failed to get commit object: %v", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		t.Fatalf("failed to get tree: %v", err)
	}

	// Verify README.md exists in the branch root
	readmeInBranch, err := tree.File(paths.ReadmeFileName)
	if err != nil {
		t.Fatalf("README.md should exist in sessions branch root: %v", err)
	}

	content, err := readmeInBranch.Contents()
	if err != nil {
		t.Fatalf("failed to read README.md content: %v", err)
	}

	// Verify content matches expected
	if content != paths.SessionsBranchReadme {
		t.Errorf("README.md content mismatch\ngot:\n%s\nwant:\n%s", content, paths.SessionsBranchReadme)
	}
}

// TestWriteCommitted_AgentField verifies that the Agent field is written
// to both metadata.json and the commit message trailer.
func TestWriteCommitted_AgentField(t *testing.T) {
	tempDir := t.TempDir()

	// Initialize a git repository with an initial commit
	repo, err := git.PlainInit(tempDir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	// Create worktree and make initial commit
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	readmeFile := filepath.Join(tempDir, "README.md")
	if err := os.WriteFile(readmeFile, []byte("# Test"), 0o644); err != nil {
		t.Fatalf("failed to write README: %v", err)
	}
	if _, err := worktree.Add("README.md"); err != nil {
		t.Fatalf("failed to add README: %v", err)
	}
	if _, err := worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com"},
	}); err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	// Create checkpoint store
	store := NewGitStore(repo)

	// Write a committed checkpoint with Agent field
	checkpointID := "a1b2c3d4e5f6"
	sessionID := "test-session-123"
	agentName := "Claude Code"

	err = store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID: checkpointID,
		SessionID:    sessionID,
		Strategy:     "manual-commit",
		Agent:        agentName,
		Transcript:   []byte("test transcript content"),
		AuthorName:   "Test Author",
		AuthorEmail:  "test@example.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() error = %v", err)
	}

	// Verify metadata.json contains agent field
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	if err != nil {
		t.Fatalf("failed to get metadata branch reference: %v", err)
	}

	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		t.Fatalf("failed to get commit object: %v", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		t.Fatalf("failed to get tree: %v", err)
	}

	// Read metadata.json from the sharded path
	shardedPath := paths.CheckpointPath(checkpointID)
	metadataPath := shardedPath + "/" + paths.MetadataFileName
	metadataFile, err := tree.File(metadataPath)
	if err != nil {
		t.Fatalf("failed to find metadata.json at %s: %v", metadataPath, err)
	}

	content, err := metadataFile.Contents()
	if err != nil {
		t.Fatalf("failed to read metadata.json: %v", err)
	}

	var metadata CommittedMetadata
	if err := json.Unmarshal([]byte(content), &metadata); err != nil {
		t.Fatalf("failed to parse metadata.json: %v", err)
	}

	if metadata.Agent != agentName {
		t.Errorf("metadata.Agent = %q, want %q", metadata.Agent, agentName)
	}

	// Verify commit message contains Entire-Agent trailer
	if !strings.Contains(commit.Message, paths.AgentTrailerKey+": "+agentName) {
		t.Errorf("commit message should contain %s trailer with value %q, got:\n%s",
			paths.AgentTrailerKey, agentName, commit.Message)
	}
}
