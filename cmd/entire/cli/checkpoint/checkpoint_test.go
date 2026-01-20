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

// TestWriteTemporary_Deduplication verifies that WriteTemporary skips creating
// a new commit when the tree hash matches the previous checkpoint.
func TestWriteTemporary_Deduplication(t *testing.T) {
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
	initialCommit, err := worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com"},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	// Change to temp dir so paths.RepoRoot() works correctly
	t.Chdir(tempDir)

	// Create a test file that will be included in checkpoints
	testFile := filepath.Join(tempDir, "test.go")
	if err := os.WriteFile(testFile, []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	// Create metadata directory
	metadataDir := filepath.Join(tempDir, ".entire", "metadata", "test-session")
	if err := os.MkdirAll(metadataDir, 0o755); err != nil {
		t.Fatalf("failed to create metadata dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(metadataDir, "full.jsonl"), []byte(`{"test": true}`), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	// Create checkpoint store
	store := NewGitStore(repo)

	// First checkpoint should be created
	baseCommit := initialCommit.String()
	result1, err := store.WriteTemporary(context.Background(), WriteTemporaryOptions{
		SessionID:         "test-session",
		BaseCommit:        baseCommit,
		ModifiedFiles:     []string{"test.go"},
		MetadataDir:       ".entire/metadata/test-session",
		MetadataDirAbs:    metadataDir,
		CommitMessage:     "Checkpoint 1",
		AuthorName:        "Test",
		AuthorEmail:       "test@test.com",
		IsFirstCheckpoint: true,
	})
	if err != nil {
		t.Fatalf("WriteTemporary() first call error = %v", err)
	}
	if result1.Skipped {
		t.Error("first checkpoint should not be skipped")
	}
	if result1.CommitHash == plumbing.ZeroHash {
		t.Error("first checkpoint should have a commit hash")
	}

	// Second checkpoint with identical content should be skipped
	result2, err := store.WriteTemporary(context.Background(), WriteTemporaryOptions{
		SessionID:         "test-session",
		BaseCommit:        baseCommit,
		ModifiedFiles:     []string{"test.go"},
		MetadataDir:       ".entire/metadata/test-session",
		MetadataDirAbs:    metadataDir,
		CommitMessage:     "Checkpoint 2",
		AuthorName:        "Test",
		AuthorEmail:       "test@test.com",
		IsFirstCheckpoint: false,
	})
	if err != nil {
		t.Fatalf("WriteTemporary() second call error = %v", err)
	}
	if !result2.Skipped {
		t.Error("second checkpoint with identical content should be skipped")
	}
	if result2.CommitHash != result1.CommitHash {
		t.Errorf("skipped checkpoint should return previous commit hash, got %s, want %s",
			result2.CommitHash, result1.CommitHash)
	}

	// Modify the file and create another checkpoint - should NOT be skipped
	if err := os.WriteFile(testFile, []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatalf("failed to modify test file: %v", err)
	}

	result3, err := store.WriteTemporary(context.Background(), WriteTemporaryOptions{
		SessionID:         "test-session",
		BaseCommit:        baseCommit,
		ModifiedFiles:     []string{"test.go"},
		MetadataDir:       ".entire/metadata/test-session",
		MetadataDirAbs:    metadataDir,
		CommitMessage:     "Checkpoint 3",
		AuthorName:        "Test",
		AuthorEmail:       "test@test.com",
		IsFirstCheckpoint: false,
	})
	if err != nil {
		t.Fatalf("WriteTemporary() third call error = %v", err)
	}
	if result3.Skipped {
		t.Error("third checkpoint with modified content should NOT be skipped")
	}
	if result3.CommitHash == result1.CommitHash {
		t.Error("third checkpoint should have a different commit hash than first")
	}
}
