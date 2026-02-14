package checkpoint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/buildinfo"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/trailers"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
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
	t.Parallel()

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

func TestCopyMetadataDir_AllowsLeadingDotsInFileName(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()

	repo, err := git.PlainInit(tempDir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	metadataDir := filepath.Join(tempDir, "metadata")
	if err := os.MkdirAll(metadataDir, 0o755); err != nil {
		t.Fatalf("failed to create metadata dir: %v", err)
	}

	dotFile := filepath.Join(metadataDir, "..notes.txt")
	if err := os.WriteFile(dotFile, []byte("ok"), 0o644); err != nil {
		t.Fatalf("failed to write file with leading dots: %v", err)
	}

	store := NewGitStore(repo)
	entries := make(map[string]object.TreeEntry)
	if err := store.copyMetadataDir(metadataDir, "checkpoint/", entries); err != nil {
		t.Fatalf("copyMetadataDir() error = %v", err)
	}

	if _, ok := entries["checkpoint/..notes.txt"]; !ok {
		t.Error("expected file with leading dots to be included")
	}
}

func TestAddDirectoryToEntriesWithAbsPath_AllowsLeadingDotsInFileName(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()

	repo, err := git.PlainInit(tempDir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	entries := make(map[string]object.TreeEntry)

	metadataDir := filepath.Join(tempDir, ".entire", "metadata", "test-session")
	if err := os.MkdirAll(metadataDir, 0o755); err != nil {
		t.Fatalf("failed to create metadata dir: %v", err)
	}

	// Filename that begins with ".." but is still inside the directory.
	dotFile := filepath.Join(metadataDir, "..notes.txt")
	if err := os.WriteFile(dotFile, []byte("ok"), 0o644); err != nil {
		t.Fatalf("failed to write file with leading dots: %v", err)
	}

	relPath := filepath.ToSlash(filepath.Join(paths.EntireMetadataDir, "test-session"))
	err = addDirectoryToEntriesWithAbsPath(repo, metadataDir, relPath, entries)
	if err != nil {
		t.Fatalf("addDirectoryToEntriesWithAbsPath() error = %v", err)
	}

	expectedPath := filepath.ToSlash(filepath.Join(relPath, "..notes.txt"))
	if _, ok := entries[expectedPath]; !ok {
		t.Errorf("expected file with leading dots to be included: %s", expectedPath)
	}
}

func TestAddDirectoryToEntriesWithAbsPath_NormalizesTreePathSeparators(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()

	repo, err := git.PlainInit(tempDir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	entries := make(map[string]object.TreeEntry)

	metadataDir := filepath.Join(tempDir, ".entire", "metadata", "test-session")
	if err := os.MkdirAll(metadataDir, 0o755); err != nil {
		t.Fatalf("failed to create metadata dir: %v", err)
	}
	filePath := filepath.Join(metadataDir, "notes.txt")
	if err := os.WriteFile(filePath, []byte("ok"), 0o644); err != nil {
		t.Fatalf("failed to write notes.txt: %v", err)
	}

	// Simulate a windows-style relative path passed into tree construction.
	relPath := `checkpoint\metadata\test-session`
	if err := addDirectoryToEntriesWithAbsPath(repo, metadataDir, relPath, entries); err != nil {
		t.Fatalf("addDirectoryToEntriesWithAbsPath() error = %v", err)
	}

	expectedPath := "checkpoint/metadata/test-session/notes.txt"
	if _, ok := entries[expectedPath]; !ok {
		t.Fatalf("expected normalized path %q in entries", expectedPath)
	}
	for entryPath := range entries {
		if strings.Contains(entryPath, `\`) {
			t.Fatalf("entry path should use '/' separators, got %q", entryPath)
		}
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
	checkpointID := id.MustCheckpointID("a1b2c3d4e5f6")
	sessionID := "test-session-123"
	agentType := agent.AgentTypeClaudeCode

	err = store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID: checkpointID,
		SessionID:    sessionID,
		Strategy:     "manual-commit",
		Agent:        agentType,
		Transcript:   []byte("test transcript content"),
		AuthorName:   "Test Author",
		AuthorEmail:  "test@example.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() error = %v", err)
	}

	// Verify root metadata.json contains agents in the Agents array
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

	// Read root metadata.json from the sharded path
	shardedPath := checkpointID.Path()
	checkpointTree, err := tree.Tree(shardedPath)
	if err != nil {
		t.Fatalf("failed to find checkpoint tree at %s: %v", shardedPath, err)
	}

	metadataFile, err := checkpointTree.File(paths.MetadataFileName)
	if err != nil {
		t.Fatalf("failed to find metadata.json: %v", err)
	}

	content, err := metadataFile.Contents()
	if err != nil {
		t.Fatalf("failed to read metadata.json: %v", err)
	}

	// Root metadata is now CheckpointSummary (without Agents array)
	var summary CheckpointSummary
	if err := json.Unmarshal([]byte(content), &summary); err != nil {
		t.Fatalf("failed to parse metadata.json as CheckpointSummary: %v", err)
	}

	// Agent should be in the session-level metadata, not in the summary
	// Read first session's metadata to verify agent (0-based indexing)
	if len(summary.Sessions) > 0 {
		sessionTree, err := checkpointTree.Tree("0")
		if err != nil {
			t.Fatalf("failed to get session tree: %v", err)
		}
		sessionMetadataFile, err := sessionTree.File(paths.MetadataFileName)
		if err != nil {
			t.Fatalf("failed to find session metadata.json: %v", err)
		}
		sessionContent, err := sessionMetadataFile.Contents()
		if err != nil {
			t.Fatalf("failed to read session metadata.json: %v", err)
		}
		var sessionMetadata CommittedMetadata
		if err := json.Unmarshal([]byte(sessionContent), &sessionMetadata); err != nil {
			t.Fatalf("failed to parse session metadata.json: %v", err)
		}
		if sessionMetadata.Agent != agentType {
			t.Errorf("sessionMetadata.Agent = %q, want %q", sessionMetadata.Agent, agentType)
		}
	}

	// Verify commit message contains Entire-Agent trailer
	if !strings.Contains(commit.Message, trailers.AgentTrailerKey+": "+string(agentType)) {
		t.Errorf("commit message should contain %s trailer with value %q, got:\n%s",
			trailers.AgentTrailerKey, agentType, commit.Message)
	}
}

// readLatestSessionMetadata reads the session-specific metadata from the latest session subdirectory.
// This is where session-specific fields like Summary are stored.
func readLatestSessionMetadata(t *testing.T, repo *git.Repository, checkpointID id.CheckpointID) CommittedMetadata {
	t.Helper()

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

	checkpointTree, err := tree.Tree(checkpointID.Path())
	if err != nil {
		t.Fatalf("failed to get checkpoint tree: %v", err)
	}

	// Read root metadata.json to get session count
	rootFile, err := checkpointTree.File(paths.MetadataFileName)
	if err != nil {
		t.Fatalf("failed to find root metadata.json: %v", err)
	}

	rootContent, err := rootFile.Contents()
	if err != nil {
		t.Fatalf("failed to read root metadata.json: %v", err)
	}

	var summary CheckpointSummary
	if err := json.Unmarshal([]byte(rootContent), &summary); err != nil {
		t.Fatalf("failed to parse root metadata.json: %v", err)
	}

	// Read session-level metadata from latest session subdirectory (0-based indexing)
	latestIndex := len(summary.Sessions) - 1
	sessionDir := strconv.Itoa(latestIndex)
	sessionTree, err := checkpointTree.Tree(sessionDir)
	if err != nil {
		t.Fatalf("failed to get session tree at %s: %v", sessionDir, err)
	}

	sessionFile, err := sessionTree.File(paths.MetadataFileName)
	if err != nil {
		t.Fatalf("failed to find session metadata.json: %v", err)
	}

	content, err := sessionFile.Contents()
	if err != nil {
		t.Fatalf("failed to read session metadata.json: %v", err)
	}

	var metadata CommittedMetadata
	if err := json.Unmarshal([]byte(content), &metadata); err != nil {
		t.Fatalf("failed to parse session metadata.json: %v", err)
	}

	return metadata
}

func readFileFromCheckpointCommit(t *testing.T, repo *git.Repository, commitHash plumbing.Hash, filePath string) string {
	t.Helper()

	commit, err := repo.CommitObject(commitHash)
	if err != nil {
		t.Fatalf("failed to get commit object: %v", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		t.Fatalf("failed to get commit tree: %v", err)
	}

	file, err := tree.File(filePath)
	if err != nil {
		t.Fatalf("failed to find file %s: %v", filePath, err)
	}

	content, err := file.Contents()
	if err != nil {
		t.Fatalf("failed to read file %s: %v", filePath, err)
	}

	return content
}

// Note: Tests for Agents array and SessionCount fields have been removed
// as those fields were removed from CommittedMetadata in the simplification.

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

// setupBranchTestRepo creates a test repository with an initial commit.
func setupBranchTestRepo(t *testing.T) (*git.Repository, plumbing.Hash) {
	t.Helper()
	tempDir := t.TempDir()

	repo, err := git.PlainInit(tempDir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

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
	commitHash, err := worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com"},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	return repo, commitHash
}

// verifyBranchInMetadata reads and verifies the branch field in metadata.json.
func verifyBranchInMetadata(t *testing.T, repo *git.Repository, checkpointID id.CheckpointID, expectedBranch string, shouldOmit bool) {
	t.Helper()

	metadataRef, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	if err != nil {
		t.Fatalf("failed to get metadata branch reference: %v", err)
	}

	commit, err := repo.CommitObject(metadataRef.Hash())
	if err != nil {
		t.Fatalf("failed to get commit object: %v", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		t.Fatalf("failed to get tree: %v", err)
	}

	shardedPath := checkpointID.Path()
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

	if metadata.Branch != expectedBranch {
		t.Errorf("metadata.Branch = %q, want %q", metadata.Branch, expectedBranch)
	}

	if shouldOmit && strings.Contains(content, `"branch"`) {
		t.Errorf("metadata.json should not contain 'branch' field when empty (omitempty), got:\n%s", content)
	}
}

// TestWriteCommitted_BranchField verifies that the Branch field is correctly
// captured in metadata.json when on a branch, and is empty when in detached HEAD.
func TestWriteCommitted_BranchField(t *testing.T) {
	t.Run("on branch", func(t *testing.T) {
		repo, commitHash := setupBranchTestRepo(t)

		// Create a feature branch and switch to it
		branchName := "feature/test-branch"
		branchRef := plumbing.NewBranchReferenceName(branchName)
		ref := plumbing.NewHashReference(branchRef, commitHash)
		if err := repo.Storer.SetReference(ref); err != nil {
			t.Fatalf("failed to create branch: %v", err)
		}

		worktree, err := repo.Worktree()
		if err != nil {
			t.Fatalf("failed to get worktree: %v", err)
		}
		if err := worktree.Checkout(&git.CheckoutOptions{Branch: branchRef}); err != nil {
			t.Fatalf("failed to checkout branch: %v", err)
		}

		// Get current branch name
		var currentBranch string
		head, err := repo.Head()
		if err == nil && head.Name().IsBranch() {
			currentBranch = head.Name().Short()
		}

		// Write a committed checkpoint with branch information
		checkpointID := id.MustCheckpointID("a1b2c3d4e5f6")
		store := NewGitStore(repo)
		err = store.WriteCommitted(context.Background(), WriteCommittedOptions{
			CheckpointID: checkpointID,
			SessionID:    "test-session-123",
			Strategy:     "manual-commit",
			Branch:       currentBranch,
			Transcript:   []byte("test transcript content"),
			AuthorName:   "Test Author",
			AuthorEmail:  "test@example.com",
		})
		if err != nil {
			t.Fatalf("WriteCommitted() error = %v", err)
		}

		verifyBranchInMetadata(t, repo, checkpointID, branchName, false)
	})

	t.Run("detached HEAD", func(t *testing.T) {
		repo, commitHash := setupBranchTestRepo(t)

		// Checkout the commit directly (detached HEAD)
		worktree, err := repo.Worktree()
		if err != nil {
			t.Fatalf("failed to get worktree: %v", err)
		}
		if err := worktree.Checkout(&git.CheckoutOptions{Hash: commitHash}); err != nil {
			t.Fatalf("failed to checkout commit: %v", err)
		}

		// Verify we're in detached HEAD
		head, err := repo.Head()
		if err != nil {
			t.Fatalf("failed to get HEAD: %v", err)
		}
		if head.Name().IsBranch() {
			t.Fatalf("expected detached HEAD, but on branch %s", head.Name().Short())
		}

		// Write a committed checkpoint (branch should be empty in detached HEAD)
		checkpointID := id.MustCheckpointID("b2c3d4e5f6a7")
		store := NewGitStore(repo)
		err = store.WriteCommitted(context.Background(), WriteCommittedOptions{
			CheckpointID: checkpointID,
			SessionID:    "test-session-456",
			Strategy:     "manual-commit",
			Branch:       "", // Empty when in detached HEAD
			Transcript:   []byte("test transcript content"),
			AuthorName:   "Test Author",
			AuthorEmail:  "test@example.com",
		})
		if err != nil {
			t.Fatalf("WriteCommitted() error = %v", err)
		}

		verifyBranchInMetadata(t, repo, checkpointID, "", true)
	})
}

// TestUpdateSummary verifies that UpdateSummary correctly updates the summary
// field in an existing checkpoint's metadata.
func TestUpdateSummary(t *testing.T) {
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo)
	checkpointID := id.MustCheckpointID("f1e2d3c4b5a6")

	// First, create a checkpoint without a summary
	err := store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID: checkpointID,
		SessionID:    "test-session-summary",
		Strategy:     "manual-commit",
		Transcript:   []byte("test transcript content"),
		FilesTouched: []string{"file1.go", "file2.go"},
		AuthorName:   "Test Author",
		AuthorEmail:  "test@example.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() error = %v", err)
	}

	// Verify no summary initially (summary is stored in session-level metadata)
	metadata := readLatestSessionMetadata(t, repo, checkpointID)
	if metadata.Summary != nil {
		t.Error("initial checkpoint should not have a summary")
	}

	// Update with a summary
	summary := &Summary{
		Intent:  "Test intent",
		Outcome: "Test outcome",
		Learnings: LearningsSummary{
			Repo:     []string{"Repo learning 1"},
			Code:     []CodeLearning{{Path: "file1.go", Line: 10, Finding: "Code finding"}},
			Workflow: []string{"Workflow learning"},
		},
		Friction:  []string{"Some friction"},
		OpenItems: []string{"Open item 1"},
	}

	err = store.UpdateSummary(context.Background(), checkpointID, summary)
	if err != nil {
		t.Fatalf("UpdateSummary() error = %v", err)
	}

	// Verify summary was saved (in session-level metadata)
	updatedMetadata := readLatestSessionMetadata(t, repo, checkpointID)
	if updatedMetadata.Summary == nil {
		t.Fatal("updated checkpoint should have a summary")
	}
	if updatedMetadata.Summary.Intent != "Test intent" {
		t.Errorf("summary.Intent = %q, want %q", updatedMetadata.Summary.Intent, "Test intent")
	}
	if updatedMetadata.Summary.Outcome != "Test outcome" {
		t.Errorf("summary.Outcome = %q, want %q", updatedMetadata.Summary.Outcome, "Test outcome")
	}
	if len(updatedMetadata.Summary.Learnings.Repo) != 1 {
		t.Errorf("summary.Learnings.Repo length = %d, want 1", len(updatedMetadata.Summary.Learnings.Repo))
	}
	if len(updatedMetadata.Summary.Friction) != 1 {
		t.Errorf("summary.Friction length = %d, want 1", len(updatedMetadata.Summary.Friction))
	}

	// Verify other metadata fields are preserved
	if updatedMetadata.SessionID != "test-session-summary" {
		t.Errorf("metadata.SessionID = %q, want %q", updatedMetadata.SessionID, "test-session-summary")
	}
	if len(updatedMetadata.FilesTouched) != 2 {
		t.Errorf("metadata.FilesTouched length = %d, want 2", len(updatedMetadata.FilesTouched))
	}
}

// TestUpdateSummary_NotFound verifies that UpdateSummary returns an error
// when the checkpoint doesn't exist.
func TestUpdateSummary_NotFound(t *testing.T) {
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo)

	// Ensure sessions branch exists
	err := store.ensureSessionsBranch()
	if err != nil {
		t.Fatalf("ensureSessionsBranch() error = %v", err)
	}

	// Try to update a non-existent checkpoint (ID must be 12 hex chars)
	checkpointID := id.MustCheckpointID("000000000000")
	summary := &Summary{Intent: "Test", Outcome: "Test"}

	err = store.UpdateSummary(context.Background(), checkpointID, summary)
	if err == nil {
		t.Error("UpdateSummary() should return error for non-existent checkpoint")
	}
	if !errors.Is(err, ErrCheckpointNotFound) {
		t.Errorf("UpdateSummary() error = %v, want ErrCheckpointNotFound", err)
	}
}

// TestListCommitted_FallsBackToRemote verifies that ListCommitted can find
// checkpoints when only origin/entire/checkpoints/v1 exists (simulating post-clone state).
func TestListCommitted_FallsBackToRemote(t *testing.T) {
	// Create "remote" repo (non-bare, so we can make commits)
	remoteDir := t.TempDir()
	remoteRepo, err := git.PlainInit(remoteDir, false)
	if err != nil {
		t.Fatalf("failed to init remote repo: %v", err)
	}

	// Create an initial commit on main branch (required for cloning)
	remoteWorktree, err := remoteRepo.Worktree()
	if err != nil {
		t.Fatalf("failed to get remote worktree: %v", err)
	}
	readmeFile := filepath.Join(remoteDir, "README.md")
	if err := os.WriteFile(readmeFile, []byte("# Test"), 0o644); err != nil {
		t.Fatalf("failed to write README: %v", err)
	}
	if _, err := remoteWorktree.Add("README.md"); err != nil {
		t.Fatalf("failed to add README: %v", err)
	}
	if _, err := remoteWorktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com"},
	}); err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// Create entire/checkpoints/v1 branch on the remote with a checkpoint
	remoteStore := NewGitStore(remoteRepo)
	cpID := id.MustCheckpointID("abcdef123456")
	err = remoteStore.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "test-session-id",
		Strategy:     "manual-commit",
		Transcript:   []byte(`{"test": true}`),
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	if err != nil {
		t.Fatalf("failed to write checkpoint to remote: %v", err)
	}

	// Clone the repo (this clones main, but not entire/checkpoints/v1 by default)
	localDir := t.TempDir()
	localRepo, err := git.PlainClone(localDir, false, &git.CloneOptions{
		URL: remoteDir,
	})
	if err != nil {
		t.Fatalf("failed to clone repo: %v", err)
	}

	// Fetch the entire/checkpoints/v1 branch to origin/entire/checkpoints/v1
	// (but don't create local branch - simulating post-clone state)
	refSpec := fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", paths.MetadataBranchName, paths.MetadataBranchName)
	err = localRepo.Fetch(&git.FetchOptions{
		RemoteName: "origin",
		RefSpecs:   []config.RefSpec{config.RefSpec(refSpec)},
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		t.Fatalf("failed to fetch entire/checkpoints/v1: %v", err)
	}

	// Verify local branch doesn't exist
	_, err = localRepo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	if err == nil {
		t.Fatal("local entire/checkpoints/v1 branch should not exist")
	}

	// Verify remote-tracking branch exists
	_, err = localRepo.Reference(plumbing.NewRemoteReferenceName("origin", paths.MetadataBranchName), true)
	if err != nil {
		t.Fatalf("origin/entire/checkpoints/v1 should exist: %v", err)
	}

	// ListCommitted should find the checkpoint by falling back to remote
	localStore := NewGitStore(localRepo)
	checkpoints, err := localStore.ListCommitted(context.Background())
	if err != nil {
		t.Fatalf("ListCommitted() error = %v", err)
	}
	if len(checkpoints) != 1 {
		t.Errorf("ListCommitted() returned %d checkpoints, want 1", len(checkpoints))
	}
	if len(checkpoints) > 0 && checkpoints[0].CheckpointID.String() != cpID.String() {
		t.Errorf("ListCommitted() checkpoint ID = %q, want %q", checkpoints[0].CheckpointID, cpID)
	}
}

// TestGetCheckpointAuthor verifies that GetCheckpointAuthor retrieves the
// author of the commit that created the checkpoint on the entire/checkpoints/v1 branch.
func TestGetCheckpointAuthor(t *testing.T) {
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo)
	checkpointID := id.MustCheckpointID("a1b2c3d4e5f6")

	// Create a checkpoint with specific author info
	authorName := "Alice Developer"
	authorEmail := "alice@example.com"

	err := store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID: checkpointID,
		SessionID:    "test-session-author",
		Strategy:     "manual-commit",
		Transcript:   []byte("test transcript"),
		FilesTouched: []string{"main.go"},
		AuthorName:   authorName,
		AuthorEmail:  authorEmail,
	})
	if err != nil {
		t.Fatalf("WriteCommitted() error = %v", err)
	}

	// Retrieve the author
	author, err := store.GetCheckpointAuthor(context.Background(), checkpointID)
	if err != nil {
		t.Fatalf("GetCheckpointAuthor() error = %v", err)
	}

	if author.Name != authorName {
		t.Errorf("author.Name = %q, want %q", author.Name, authorName)
	}
	if author.Email != authorEmail {
		t.Errorf("author.Email = %q, want %q", author.Email, authorEmail)
	}
}

// TestGetCheckpointAuthor_NotFound verifies that GetCheckpointAuthor returns
// empty author when the checkpoint doesn't exist.
func TestGetCheckpointAuthor_NotFound(t *testing.T) {
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo)

	// Query for a non-existent checkpoint (must be valid hex)
	checkpointID := id.MustCheckpointID("ffffffffffff")

	author, err := store.GetCheckpointAuthor(context.Background(), checkpointID)
	if err != nil {
		t.Fatalf("GetCheckpointAuthor() error = %v", err)
	}

	// Should return empty author (no error)
	if author.Name != "" || author.Email != "" {
		t.Errorf("expected empty author for non-existent checkpoint, got Name=%q, Email=%q", author.Name, author.Email)
	}
}

// TestGetCheckpointAuthor_NoSessionsBranch verifies that GetCheckpointAuthor
// returns empty author when the entire/checkpoints/v1 branch doesn't exist.
func TestGetCheckpointAuthor_NoSessionsBranch(t *testing.T) {
	// Create a fresh repo without sessions branch
	tempDir := t.TempDir()
	repo, err := git.PlainInit(tempDir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	store := NewGitStore(repo)
	checkpointID := id.MustCheckpointID("aabbccddeeff")

	author, err := store.GetCheckpointAuthor(context.Background(), checkpointID)
	if err != nil {
		t.Fatalf("GetCheckpointAuthor() error = %v", err)
	}

	// Should return empty author (no error)
	if author.Name != "" || author.Email != "" {
		t.Errorf("expected empty author when sessions branch doesn't exist, got Name=%q, Email=%q", author.Name, author.Email)
	}
}

// =============================================================================
// Multi-Session Tests - Tests for checkpoint structure with CheckpointSummary
// at root level and sessions stored in numbered subfolders (0-based: 0/, 1/, 2/)
// =============================================================================

// TestWriteCommitted_MultipleSessionsSameCheckpoint verifies that writing multiple
// sessions to the same checkpoint ID creates separate numbered subdirectories.
func TestWriteCommitted_MultipleSessionsSameCheckpoint(t *testing.T) {
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo)
	checkpointID := id.MustCheckpointID("a1a2a3a4a5a6")

	// Write first session
	err := store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID:     checkpointID,
		SessionID:        "session-one",
		Strategy:         "manual-commit",
		Transcript:       []byte(`{"message": "first session"}`),
		Prompts:          []string{"First prompt"},
		FilesTouched:     []string{"file1.go"},
		CheckpointsCount: 3,
		AuthorName:       "Test Author",
		AuthorEmail:      "test@example.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() first session error = %v", err)
	}

	// Write second session to the same checkpoint ID
	err = store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID:     checkpointID,
		SessionID:        "session-two",
		Strategy:         "manual-commit",
		Transcript:       []byte(`{"message": "second session"}`),
		Prompts:          []string{"Second prompt"},
		FilesTouched:     []string{"file2.go"},
		CheckpointsCount: 2,
		AuthorName:       "Test Author",
		AuthorEmail:      "test@example.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() second session error = %v", err)
	}

	// Read the checkpoint summary
	summary, err := store.ReadCommitted(context.Background(), checkpointID)
	if err != nil {
		t.Fatalf("ReadCommitted() error = %v", err)
	}
	if summary == nil {
		t.Fatal("ReadCommitted() returned nil summary")
		return
	}

	// Verify Sessions array has 2 entries
	if len(summary.Sessions) != 2 {
		t.Errorf("len(summary.Sessions) = %d, want 2", len(summary.Sessions))
	}

	// Verify both sessions have correct file paths (0-based indexing)
	if !strings.Contains(summary.Sessions[0].Transcript, "/0/") {
		t.Errorf("session 0 transcript path should contain '/0/', got %s", summary.Sessions[0].Transcript)
	}
	if !strings.Contains(summary.Sessions[1].Transcript, "/1/") {
		t.Errorf("session 1 transcript path should contain '/1/', got %s", summary.Sessions[1].Transcript)
	}

	// Verify session content can be read from each subdirectory
	content0, err := store.ReadSessionContent(context.Background(), checkpointID, 0)
	if err != nil {
		t.Fatalf("ReadSessionContent(0) error = %v", err)
	}
	if content0.Metadata.SessionID != "session-one" {
		t.Errorf("session 0 SessionID = %q, want %q", content0.Metadata.SessionID, "session-one")
	}

	content1, err := store.ReadSessionContent(context.Background(), checkpointID, 1)
	if err != nil {
		t.Fatalf("ReadSessionContent(1) error = %v", err)
	}
	if content1.Metadata.SessionID != "session-two" {
		t.Errorf("session 1 SessionID = %q, want %q", content1.Metadata.SessionID, "session-two")
	}
}

// TestWriteCommitted_Aggregation verifies that CheckpointSummary correctly
// aggregates statistics (CheckpointsCount, FilesTouched, TokenUsage) from
// multiple sessions written to the same checkpoint.
func TestWriteCommitted_Aggregation(t *testing.T) {
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo)
	checkpointID := id.MustCheckpointID("b1b2b3b4b5b6")

	// Write first session with specific stats
	err := store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID:     checkpointID,
		SessionID:        "session-one",
		Strategy:         "manual-commit",
		Transcript:       []byte(`{"message": "first"}`),
		FilesTouched:     []string{"a.go", "b.go"},
		CheckpointsCount: 3,
		TokenUsage: &agent.TokenUsage{
			InputTokens:  100,
			OutputTokens: 50,
			APICallCount: 5,
		},
		AuthorName:  "Test Author",
		AuthorEmail: "test@example.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() first session error = %v", err)
	}

	// Write second session with overlapping and new files
	err = store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID:     checkpointID,
		SessionID:        "session-two",
		Strategy:         "manual-commit",
		Transcript:       []byte(`{"message": "second"}`),
		FilesTouched:     []string{"b.go", "c.go"}, // b.go overlaps
		CheckpointsCount: 2,
		TokenUsage: &agent.TokenUsage{
			InputTokens:  50,
			OutputTokens: 25,
			APICallCount: 3,
		},
		AuthorName:  "Test Author",
		AuthorEmail: "test@example.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() second session error = %v", err)
	}

	// Read the checkpoint summary
	summary, err := store.ReadCommitted(context.Background(), checkpointID)
	if err != nil {
		t.Fatalf("ReadCommitted() error = %v", err)
	}
	if summary == nil {
		t.Fatal("ReadCommitted() returned nil summary")
		return
	}

	// Verify aggregated CheckpointsCount = 3 + 2 = 5
	if summary.CheckpointsCount != 5 {
		t.Errorf("summary.CheckpointsCount = %d, want 5", summary.CheckpointsCount)
	}

	// Verify merged FilesTouched = ["a.go", "b.go", "c.go"] (sorted, deduplicated)
	expectedFiles := []string{"a.go", "b.go", "c.go"}
	if len(summary.FilesTouched) != len(expectedFiles) {
		t.Errorf("len(summary.FilesTouched) = %d, want %d", len(summary.FilesTouched), len(expectedFiles))
	}
	for i, want := range expectedFiles {
		if i >= len(summary.FilesTouched) {
			break
		}
		if summary.FilesTouched[i] != want {
			t.Errorf("summary.FilesTouched[%d] = %q, want %q", i, summary.FilesTouched[i], want)
		}
	}

	// Verify aggregated TokenUsage
	if summary.TokenUsage == nil {
		t.Fatal("summary.TokenUsage should not be nil")
	}
	if summary.TokenUsage.InputTokens != 150 {
		t.Errorf("summary.TokenUsage.InputTokens = %d, want 150", summary.TokenUsage.InputTokens)
	}
	if summary.TokenUsage.OutputTokens != 75 {
		t.Errorf("summary.TokenUsage.OutputTokens = %d, want 75", summary.TokenUsage.OutputTokens)
	}
	if summary.TokenUsage.APICallCount != 8 {
		t.Errorf("summary.TokenUsage.APICallCount = %d, want 8", summary.TokenUsage.APICallCount)
	}
}

// TestReadCommitted_ReturnsCheckpointSummary verifies that ReadCommitted returns
// a CheckpointSummary with the correct structure including Sessions array.
func TestReadCommitted_ReturnsCheckpointSummary(t *testing.T) {
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo)
	checkpointID := id.MustCheckpointID("c1c2c3c4c5c6")

	// Write two sessions
	for i, sessionID := range []string{"session-alpha", "session-beta"} {
		err := store.WriteCommitted(context.Background(), WriteCommittedOptions{
			CheckpointID:     checkpointID,
			SessionID:        sessionID,
			Strategy:         "manual-commit",
			Transcript:       []byte(fmt.Sprintf(`{"session": %d}`, i)),
			Prompts:          []string{fmt.Sprintf("Prompt %d", i)},
			Context:          []byte(fmt.Sprintf("Context %d", i)),
			FilesTouched:     []string{fmt.Sprintf("file%d.go", i)},
			CheckpointsCount: i + 1,
			AuthorName:       "Test Author",
			AuthorEmail:      "test@example.com",
		})
		if err != nil {
			t.Fatalf("WriteCommitted() session %d error = %v", i, err)
		}
	}

	// Read the checkpoint summary
	summary, err := store.ReadCommitted(context.Background(), checkpointID)
	if err != nil {
		t.Fatalf("ReadCommitted() error = %v", err)
	}
	if summary == nil {
		t.Fatal("ReadCommitted() returned nil summary")
		return
	}

	// Verify basic summary fields
	if summary.CheckpointID != checkpointID {
		t.Errorf("summary.CheckpointID = %v, want %v", summary.CheckpointID, checkpointID)
	}
	if summary.Strategy != "manual-commit" {
		t.Errorf("summary.Strategy = %q, want %q", summary.Strategy, "manual-commit")
	}

	// Verify Sessions array
	if len(summary.Sessions) != 2 {
		t.Fatalf("len(summary.Sessions) = %d, want 2", len(summary.Sessions))
	}

	// Verify file paths point to correct locations
	for i, session := range summary.Sessions {
		expectedSubdir := fmt.Sprintf("/%d/", i)
		if !strings.Contains(session.Metadata, expectedSubdir) {
			t.Errorf("session %d Metadata path should contain %q, got %q", i, expectedSubdir, session.Metadata)
		}
		if !strings.Contains(session.Transcript, expectedSubdir) {
			t.Errorf("session %d Transcript path should contain %q, got %q", i, expectedSubdir, session.Transcript)
		}
	}
}

// TestReadSessionContent_ByIndex verifies that ReadSessionContent can read
// specific sessions by their 0-based index within a checkpoint.
func TestReadSessionContent_ByIndex(t *testing.T) {
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo)
	checkpointID := id.MustCheckpointID("d1d2d3d4d5d6")

	// Write two sessions with distinct content
	sessions := []struct {
		id         string
		transcript string
		prompt     string
	}{
		{"session-first", `{"order": "first"}`, "First user prompt"},
		{"session-second", `{"order": "second"}`, "Second user prompt"},
	}

	for _, s := range sessions {
		err := store.WriteCommitted(context.Background(), WriteCommittedOptions{
			CheckpointID:     checkpointID,
			SessionID:        s.id,
			Strategy:         "manual-commit",
			Transcript:       []byte(s.transcript),
			Prompts:          []string{s.prompt},
			CheckpointsCount: 1,
			AuthorName:       "Test Author",
			AuthorEmail:      "test@example.com",
		})
		if err != nil {
			t.Fatalf("WriteCommitted() session %s error = %v", s.id, err)
		}
	}

	// Read session 0
	content0, err := store.ReadSessionContent(context.Background(), checkpointID, 0)
	if err != nil {
		t.Fatalf("ReadSessionContent(0) error = %v", err)
	}
	if content0.Metadata.SessionID != "session-first" {
		t.Errorf("session 0 SessionID = %q, want %q", content0.Metadata.SessionID, "session-first")
	}
	if !strings.Contains(string(content0.Transcript), "first") {
		t.Errorf("session 0 transcript should contain 'first', got %s", string(content0.Transcript))
	}
	if !strings.Contains(content0.Prompts, "First") {
		t.Errorf("session 0 prompts should contain 'First', got %s", content0.Prompts)
	}

	// Read session 1
	content1, err := store.ReadSessionContent(context.Background(), checkpointID, 1)
	if err != nil {
		t.Fatalf("ReadSessionContent(1) error = %v", err)
	}
	if content1.Metadata.SessionID != "session-second" {
		t.Errorf("session 1 SessionID = %q, want %q", content1.Metadata.SessionID, "session-second")
	}
	if !strings.Contains(string(content1.Transcript), "second") {
		t.Errorf("session 1 transcript should contain 'second', got %s", string(content1.Transcript))
	}
}

// writeSingleSession is a test helper that creates a store with a single session
// and returns the store and checkpoint ID for further testing.
func writeSingleSession(t *testing.T, cpIDStr, sessionID, transcript string) (*GitStore, id.CheckpointID) {
	t.Helper()
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo)
	checkpointID := id.MustCheckpointID(cpIDStr)

	err := store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID:     checkpointID,
		SessionID:        sessionID,
		Strategy:         "manual-commit",
		Transcript:       []byte(transcript),
		CheckpointsCount: 1,
		AuthorName:       "Test Author",
		AuthorEmail:      "test@example.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() error = %v", err)
	}
	return store, checkpointID
}

// TestReadSessionContent_InvalidIndex verifies that ReadSessionContent returns
// an error when requesting a session index that doesn't exist.
func TestReadSessionContent_InvalidIndex(t *testing.T) {
	store, checkpointID := writeSingleSession(t, "e1e2e3e4e5e6", "only-session", `{"single": true}`)

	// Try to read session index 1 (doesn't exist)
	_, err := store.ReadSessionContent(context.Background(), checkpointID, 1)
	if err == nil {
		t.Error("ReadSessionContent(1) should return error for non-existent session")
	}
	if !strings.Contains(err.Error(), "session 1 not found") {
		t.Errorf("error should mention session not found, got: %v", err)
	}
}

// TestReadLatestSessionContent verifies that ReadLatestSessionContent returns
// the content of the most recently added session (highest index).
func TestReadLatestSessionContent(t *testing.T) {
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo)
	checkpointID := id.MustCheckpointID("f1f2f3f4f5f6")

	// Write three sessions
	for i := range 3 {
		err := store.WriteCommitted(context.Background(), WriteCommittedOptions{
			CheckpointID:     checkpointID,
			SessionID:        fmt.Sprintf("session-%d", i),
			Strategy:         "manual-commit",
			Transcript:       []byte(fmt.Sprintf(`{"index": %d}`, i)),
			CheckpointsCount: 1,
			AuthorName:       "Test Author",
			AuthorEmail:      "test@example.com",
		})
		if err != nil {
			t.Fatalf("WriteCommitted() session %d error = %v", i, err)
		}
	}

	// Read latest session content
	content, err := store.ReadLatestSessionContent(context.Background(), checkpointID)
	if err != nil {
		t.Fatalf("ReadLatestSessionContent() error = %v", err)
	}

	// Should return session 2 (0-indexed, so latest is index 2)
	if content.Metadata.SessionID != "session-2" {
		t.Errorf("latest session SessionID = %q, want %q", content.Metadata.SessionID, "session-2")
	}
	if !strings.Contains(string(content.Transcript), `"index": 2`) {
		t.Errorf("latest session transcript should contain index 2, got %s", string(content.Transcript))
	}
}

// TestReadSessionContentByID verifies that ReadSessionContentByID can find
// a session by its session ID rather than by index.
func TestReadSessionContentByID(t *testing.T) {
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo)
	checkpointID := id.MustCheckpointID("010203040506")

	// Write two sessions with distinct IDs
	sessionIDs := []string{"unique-id-alpha", "unique-id-beta"}
	for i, sid := range sessionIDs {
		err := store.WriteCommitted(context.Background(), WriteCommittedOptions{
			CheckpointID:     checkpointID,
			SessionID:        sid,
			Strategy:         "manual-commit",
			Transcript:       []byte(fmt.Sprintf(`{"session_name": "%s"}`, sid)),
			CheckpointsCount: 1,
			AuthorName:       "Test Author",
			AuthorEmail:      "test@example.com",
		})
		if err != nil {
			t.Fatalf("WriteCommitted() session %d error = %v", i, err)
		}
	}

	// Read by session ID
	content, err := store.ReadSessionContentByID(context.Background(), checkpointID, "unique-id-beta")
	if err != nil {
		t.Fatalf("ReadSessionContentByID() error = %v", err)
	}

	if content.Metadata.SessionID != "unique-id-beta" {
		t.Errorf("SessionID = %q, want %q", content.Metadata.SessionID, "unique-id-beta")
	}
	if !strings.Contains(string(content.Transcript), "unique-id-beta") {
		t.Errorf("transcript should contain session name, got %s", string(content.Transcript))
	}
}

// TestReadSessionContentByID_NotFound verifies that ReadSessionContentByID
// returns an error when the session ID doesn't exist in the checkpoint.
func TestReadSessionContentByID_NotFound(t *testing.T) {
	store, checkpointID := writeSingleSession(t, "111213141516", "existing-session", `{"exists": true}`)

	// Try to read non-existent session ID
	_, err := store.ReadSessionContentByID(context.Background(), checkpointID, "nonexistent-session")
	if err == nil {
		t.Error("ReadSessionContentByID() should return error for non-existent session ID")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention 'not found', got: %v", err)
	}
}

// TestListCommitted_MultiSessionInfo verifies that ListCommitted returns correct
// information for checkpoints with multiple sessions.
func TestListCommitted_MultiSessionInfo(t *testing.T) {
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo)
	checkpointID := id.MustCheckpointID("212223242526")

	// Write two sessions to the same checkpoint
	for i, sid := range []string{"list-session-1", "list-session-2"} {
		err := store.WriteCommitted(context.Background(), WriteCommittedOptions{
			CheckpointID:     checkpointID,
			SessionID:        sid,
			Strategy:         "manual-commit",
			Agent:            agent.AgentTypeClaudeCode,
			Transcript:       []byte(fmt.Sprintf(`{"i": %d}`, i)),
			FilesTouched:     []string{fmt.Sprintf("file%d.go", i)},
			CheckpointsCount: i + 1,
			AuthorName:       "Test Author",
			AuthorEmail:      "test@example.com",
		})
		if err != nil {
			t.Fatalf("WriteCommitted() session %d error = %v", i, err)
		}
	}

	// List all checkpoints
	checkpoints, err := store.ListCommitted(context.Background())
	if err != nil {
		t.Fatalf("ListCommitted() error = %v", err)
	}

	// Find our checkpoint
	var found *CommittedInfo
	for i := range checkpoints {
		if checkpoints[i].CheckpointID == checkpointID {
			found = &checkpoints[i]
			break
		}
	}
	if found == nil {
		t.Fatal("checkpoint not found in ListCommitted() results")
		return
	}

	// Verify SessionCount = 2
	if found.SessionCount != 2 {
		t.Errorf("SessionCount = %d, want 2", found.SessionCount)
	}

	// Verify SessionID is from the latest session
	if found.SessionID != "list-session-2" {
		t.Errorf("SessionID = %q, want %q (latest session)", found.SessionID, "list-session-2")
	}

	// Verify Agent comes from latest session metadata
	if found.Agent != agent.AgentTypeClaudeCode {
		t.Errorf("Agent = %q, want %q", found.Agent, agent.AgentTypeClaudeCode)
	}
}

// TestWriteCommitted_SessionWithNoPrompts verifies that a session can be
// written without prompts and still be read correctly.
func TestWriteCommitted_SessionWithNoPrompts(t *testing.T) {
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo)
	checkpointID := id.MustCheckpointID("313233343536")

	// Write session without prompts
	err := store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID:     checkpointID,
		SessionID:        "no-prompts-session",
		Strategy:         "manual-commit",
		Transcript:       []byte(`{"no_prompts": true}`),
		Prompts:          nil, // No prompts
		Context:          []byte("Some context"),
		CheckpointsCount: 1,
		AuthorName:       "Test Author",
		AuthorEmail:      "test@example.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() error = %v", err)
	}

	// Read the session content
	content, err := store.ReadSessionContent(context.Background(), checkpointID, 0)
	if err != nil {
		t.Fatalf("ReadSessionContent() error = %v", err)
	}

	// Verify session metadata is correct
	if content.Metadata.SessionID != "no-prompts-session" {
		t.Errorf("SessionID = %q, want %q", content.Metadata.SessionID, "no-prompts-session")
	}

	// Verify transcript is present
	if len(content.Transcript) == 0 {
		t.Error("Transcript should not be empty")
	}

	// Verify prompts is empty
	if content.Prompts != "" {
		t.Errorf("Prompts should be empty, got %q", content.Prompts)
	}

	// Verify context is present
	if content.Context != "Some context" {
		t.Errorf("Context = %q, want %q", content.Context, "Some context")
	}
}

// TestWriteCommitted_SessionWithSummary verifies that a non-nil Summary
// in WriteCommittedOptions is persisted in the session-level metadata.json.
// Regression test for ENT-243 where Summary was omitted from the struct literal.
func TestWriteCommitted_SessionWithSummary(t *testing.T) {
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo)
	checkpointID := id.MustCheckpointID("aabbccddeeff")

	summary := &Summary{
		Intent:  "User wanted to fix a bug",
		Outcome: "Bug was fixed",
	}

	err := store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID:     checkpointID,
		SessionID:        "summary-session",
		Strategy:         "manual-commit",
		Transcript:       []byte(`{"test": true}`),
		CheckpointsCount: 1,
		AuthorName:       "Test Author",
		AuthorEmail:      "test@example.com",
		Summary:          summary,
	})
	if err != nil {
		t.Fatalf("WriteCommitted() error = %v", err)
	}

	content, err := store.ReadSessionContent(context.Background(), checkpointID, 0)
	if err != nil {
		t.Fatalf("ReadSessionContent() error = %v", err)
	}

	if content.Metadata.Summary == nil {
		t.Fatal("Summary should not be nil")
	}
	if content.Metadata.Summary.Intent != "User wanted to fix a bug" {
		t.Errorf("Summary.Intent = %q, want %q", content.Metadata.Summary.Intent, "User wanted to fix a bug")
	}
	if content.Metadata.Summary.Outcome != "Bug was fixed" {
		t.Errorf("Summary.Outcome = %q, want %q", content.Metadata.Summary.Outcome, "Bug was fixed")
	}
}

// TestWriteCommitted_SessionWithNoContext verifies that a session can be
// written without context and still be read correctly.
func TestWriteCommitted_SessionWithNoContext(t *testing.T) {
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo)
	checkpointID := id.MustCheckpointID("414243444546")

	// Write session without context
	err := store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID:     checkpointID,
		SessionID:        "no-context-session",
		Strategy:         "manual-commit",
		Transcript:       []byte(`{"no_context": true}`),
		Prompts:          []string{"A prompt"},
		Context:          nil, // No context
		CheckpointsCount: 1,
		AuthorName:       "Test Author",
		AuthorEmail:      "test@example.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() error = %v", err)
	}

	// Read the session content
	content, err := store.ReadSessionContent(context.Background(), checkpointID, 0)
	if err != nil {
		t.Fatalf("ReadSessionContent() error = %v", err)
	}

	// Verify session metadata is correct
	if content.Metadata.SessionID != "no-context-session" {
		t.Errorf("SessionID = %q, want %q", content.Metadata.SessionID, "no-context-session")
	}

	// Verify transcript is present
	if len(content.Transcript) == 0 {
		t.Error("Transcript should not be empty")
	}

	// Verify prompts is present
	if !strings.Contains(content.Prompts, "A prompt") {
		t.Errorf("Prompts should contain 'A prompt', got %q", content.Prompts)
	}

	// Verify context is empty
	if content.Context != "" {
		t.Errorf("Context should be empty, got %q", content.Context)
	}
}

// TestWriteCommitted_ThreeSessions verifies the structure with three sessions
// to ensure the 0-based indexing works correctly throughout.
func TestWriteCommitted_ThreeSessions(t *testing.T) {
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo)
	checkpointID := id.MustCheckpointID("515253545556")

	// Write three sessions
	for i := range 3 {
		err := store.WriteCommitted(context.Background(), WriteCommittedOptions{
			CheckpointID:     checkpointID,
			SessionID:        fmt.Sprintf("three-session-%d", i),
			Strategy:         "manual-commit",
			Transcript:       []byte(fmt.Sprintf(`{"session_number": %d}`, i)),
			FilesTouched:     []string{fmt.Sprintf("s%d.go", i)},
			CheckpointsCount: i + 1,
			TokenUsage: &agent.TokenUsage{
				InputTokens: 100 * (i + 1),
			},
			AuthorName:  "Test Author",
			AuthorEmail: "test@example.com",
		})
		if err != nil {
			t.Fatalf("WriteCommitted() session %d error = %v", i, err)
		}
	}

	// Read summary
	summary, err := store.ReadCommitted(context.Background(), checkpointID)
	if err != nil {
		t.Fatalf("ReadCommitted() error = %v", err)
	}

	// Verify 3 sessions
	if len(summary.Sessions) != 3 {
		t.Errorf("len(summary.Sessions) = %d, want 3", len(summary.Sessions))
	}

	// Verify aggregated stats
	// CheckpointsCount = 1 + 2 + 3 = 6
	if summary.CheckpointsCount != 6 {
		t.Errorf("summary.CheckpointsCount = %d, want 6", summary.CheckpointsCount)
	}

	// FilesTouched = [s0.go, s1.go, s2.go]
	if len(summary.FilesTouched) != 3 {
		t.Errorf("len(summary.FilesTouched) = %d, want 3", len(summary.FilesTouched))
	}

	// TokenUsage.InputTokens = 100 + 200 + 300 = 600
	if summary.TokenUsage == nil {
		t.Fatal("summary.TokenUsage should not be nil")
	}
	if summary.TokenUsage.InputTokens != 600 {
		t.Errorf("summary.TokenUsage.InputTokens = %d, want 600", summary.TokenUsage.InputTokens)
	}

	// Verify each session can be read by index
	for i := range 3 {
		content, err := store.ReadSessionContent(context.Background(), checkpointID, i)
		if err != nil {
			t.Errorf("ReadSessionContent(%d) error = %v", i, err)
			continue
		}
		expectedID := fmt.Sprintf("three-session-%d", i)
		if content.Metadata.SessionID != expectedID {
			t.Errorf("session %d SessionID = %q, want %q", i, content.Metadata.SessionID, expectedID)
		}
	}
}

// TestReadCommitted_NonexistentCheckpoint verifies that ReadCommitted returns
// nil (not an error) when the checkpoint doesn't exist.
func TestReadCommitted_NonexistentCheckpoint(t *testing.T) {
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo)

	// Ensure sessions branch exists
	err := store.ensureSessionsBranch()
	if err != nil {
		t.Fatalf("ensureSessionsBranch() error = %v", err)
	}

	// Try to read non-existent checkpoint
	checkpointID := id.MustCheckpointID("ffffffffffff")
	summary, err := store.ReadCommitted(context.Background(), checkpointID)
	if err != nil {
		t.Errorf("ReadCommitted() error = %v, want nil", err)
	}
	if summary != nil {
		t.Errorf("ReadCommitted() = %v, want nil for non-existent checkpoint", summary)
	}
}

// TestReadSessionContent_NonexistentCheckpoint verifies that ReadSessionContent
// returns ErrCheckpointNotFound when the checkpoint doesn't exist.
func TestReadSessionContent_NonexistentCheckpoint(t *testing.T) {
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo)

	// Ensure sessions branch exists
	err := store.ensureSessionsBranch()
	if err != nil {
		t.Fatalf("ensureSessionsBranch() error = %v", err)
	}

	// Try to read from non-existent checkpoint
	checkpointID := id.MustCheckpointID("eeeeeeeeeeee")
	_, err = store.ReadSessionContent(context.Background(), checkpointID, 0)
	if !errors.Is(err, ErrCheckpointNotFound) {
		t.Errorf("ReadSessionContent() error = %v, want ErrCheckpointNotFound", err)
	}
}

// TestWriteTemporary_FirstCheckpoint_CapturesModifiedTrackedFiles verifies that
// the first checkpoint captures modifications to tracked files that existed before
// the agent made any changes (user's uncommitted work).
func TestWriteTemporary_FirstCheckpoint_CapturesModifiedTrackedFiles(t *testing.T) {
	tempDir := t.TempDir()

	// Initialize a git repository with an initial commit containing README.md
	repo, err := git.PlainInit(tempDir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	// Create and commit README.md with original content
	readmeFile := filepath.Join(tempDir, "README.md")
	originalContent := "# Original Content\n"
	if err := os.WriteFile(readmeFile, []byte(originalContent), 0o644); err != nil {
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

	// Simulate user modifying README.md BEFORE agent starts (user's uncommitted work)
	modifiedContent := "# Modified by User\n\nThis change was made before the agent started.\n"
	if err := os.WriteFile(readmeFile, []byte(modifiedContent), 0o644); err != nil {
		t.Fatalf("failed to modify README: %v", err)
	}

	// Change to temp dir so paths.RepoRoot() works correctly
	t.Chdir(tempDir)

	// Create metadata directory
	metadataDir := filepath.Join(tempDir, ".entire", "metadata", "test-session")
	if err := os.MkdirAll(metadataDir, 0o755); err != nil {
		t.Fatalf("failed to create metadata dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(metadataDir, "full.jsonl"), []byte(`{"test": true}`), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	// Create checkpoint store and write first checkpoint
	// Note: ModifiedFiles is empty because agent hasn't touched anything yet
	// The first checkpoint should still capture README.md because it's modified in working dir
	store := NewGitStore(repo)
	baseCommit := initialCommit.String()

	result, err := store.WriteTemporary(context.Background(), WriteTemporaryOptions{
		SessionID:         "test-session",
		BaseCommit:        baseCommit,
		ModifiedFiles:     []string{}, // Agent hasn't modified anything
		MetadataDir:       ".entire/metadata/test-session",
		MetadataDirAbs:    metadataDir,
		CommitMessage:     "First checkpoint",
		AuthorName:        "Test",
		AuthorEmail:       "test@test.com",
		IsFirstCheckpoint: true,
	})
	if err != nil {
		t.Fatalf("WriteTemporary() error = %v", err)
	}
	if result.Skipped {
		t.Error("first checkpoint should not be skipped")
	}

	// Verify the shadow branch commit contains the MODIFIED README.md content
	commit, err := repo.CommitObject(result.CommitHash)
	if err != nil {
		t.Fatalf("failed to get commit object: %v", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		t.Fatalf("failed to get tree: %v", err)
	}

	// Find README.md in the tree
	file, err := tree.File("README.md")
	if err != nil {
		t.Fatalf("README.md not found in checkpoint tree: %v", err)
	}

	content, err := file.Contents()
	if err != nil {
		t.Fatalf("failed to read README.md content: %v", err)
	}

	if content != modifiedContent {
		t.Errorf("checkpoint should contain modified content\ngot:\n%s\nwant:\n%s", content, modifiedContent)
	}
}

// TestWriteTemporary_FirstCheckpoint_CapturesUntrackedFiles verifies that
// the first checkpoint captures untracked files that exist in the working directory.
func TestWriteTemporary_FirstCheckpoint_CapturesUntrackedFiles(t *testing.T) {
	tempDir := t.TempDir()

	// Initialize a git repository with an initial commit
	repo, err := git.PlainInit(tempDir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	// Create and commit README.md
	readmeFile := filepath.Join(tempDir, "README.md")
	if err := os.WriteFile(readmeFile, []byte("# Test\n"), 0o644); err != nil {
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

	// Create an untracked file (simulating user creating a file before agent starts)
	untrackedFile := filepath.Join(tempDir, "config.local.json")
	untrackedContent := `{"key": "secret_value"}`
	if err := os.WriteFile(untrackedFile, []byte(untrackedContent), 0o644); err != nil {
		t.Fatalf("failed to write untracked file: %v", err)
	}

	// Change to temp dir so paths.RepoRoot() works correctly
	t.Chdir(tempDir)

	// Create metadata directory
	metadataDir := filepath.Join(tempDir, ".entire", "metadata", "test-session")
	if err := os.MkdirAll(metadataDir, 0o755); err != nil {
		t.Fatalf("failed to create metadata dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(metadataDir, "full.jsonl"), []byte(`{"test": true}`), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	// Create checkpoint store and write first checkpoint
	store := NewGitStore(repo)
	baseCommit := initialCommit.String()

	result, err := store.WriteTemporary(context.Background(), WriteTemporaryOptions{
		SessionID:         "test-session",
		BaseCommit:        baseCommit,
		ModifiedFiles:     []string{},
		NewFiles:          []string{}, // NewFiles might be empty if this is truly "at session start"
		MetadataDir:       ".entire/metadata/test-session",
		MetadataDirAbs:    metadataDir,
		CommitMessage:     "First checkpoint",
		AuthorName:        "Test",
		AuthorEmail:       "test@test.com",
		IsFirstCheckpoint: true,
	})
	if err != nil {
		t.Fatalf("WriteTemporary() error = %v", err)
	}

	// Verify the shadow branch commit contains the untracked file
	commit, err := repo.CommitObject(result.CommitHash)
	if err != nil {
		t.Fatalf("failed to get commit object: %v", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		t.Fatalf("failed to get tree: %v", err)
	}

	// Find config.local.json in the tree
	file, err := tree.File("config.local.json")
	if err != nil {
		t.Fatalf("untracked file config.local.json not found in checkpoint tree: %v", err)
	}

	content, err := file.Contents()
	if err != nil {
		t.Fatalf("failed to read config.local.json content: %v", err)
	}

	if content != untrackedContent {
		t.Errorf("checkpoint should contain untracked file content\ngot:\n%s\nwant:\n%s", content, untrackedContent)
	}
}

// TestWriteTemporary_FirstCheckpoint_ExcludesGitIgnoredFiles verifies that
// the first checkpoint does NOT capture files that are in .gitignore.
func TestWriteTemporary_FirstCheckpoint_ExcludesGitIgnoredFiles(t *testing.T) {
	tempDir := t.TempDir()

	// Initialize a git repository with an initial commit
	repo, err := git.PlainInit(tempDir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	// Create .gitignore that ignores node_modules/
	gitignoreFile := filepath.Join(tempDir, ".gitignore")
	if err := os.WriteFile(gitignoreFile, []byte("node_modules/\n"), 0o644); err != nil {
		t.Fatalf("failed to write .gitignore: %v", err)
	}
	if _, err := worktree.Add(".gitignore"); err != nil {
		t.Fatalf("failed to add .gitignore: %v", err)
	}

	// Create and commit README.md
	readmeFile := filepath.Join(tempDir, "README.md")
	if err := os.WriteFile(readmeFile, []byte("# Test\n"), 0o644); err != nil {
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

	// Create node_modules/ directory with a file (should be ignored)
	nodeModulesDir := filepath.Join(tempDir, "node_modules")
	if err := os.MkdirAll(nodeModulesDir, 0o755); err != nil {
		t.Fatalf("failed to create node_modules: %v", err)
	}
	ignoredFile := filepath.Join(nodeModulesDir, "some-package.js")
	if err := os.WriteFile(ignoredFile, []byte("module.exports = {}"), 0o644); err != nil {
		t.Fatalf("failed to write ignored file: %v", err)
	}

	// Change to temp dir so paths.RepoRoot() works correctly
	t.Chdir(tempDir)

	// Create metadata directory
	metadataDir := filepath.Join(tempDir, ".entire", "metadata", "test-session")
	if err := os.MkdirAll(metadataDir, 0o755); err != nil {
		t.Fatalf("failed to create metadata dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(metadataDir, "full.jsonl"), []byte(`{"test": true}`), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	// Create checkpoint store and write first checkpoint
	store := NewGitStore(repo)
	baseCommit := initialCommit.String()

	result, err := store.WriteTemporary(context.Background(), WriteTemporaryOptions{
		SessionID:         "test-session",
		BaseCommit:        baseCommit,
		ModifiedFiles:     []string{},
		MetadataDir:       ".entire/metadata/test-session",
		MetadataDirAbs:    metadataDir,
		CommitMessage:     "First checkpoint",
		AuthorName:        "Test",
		AuthorEmail:       "test@test.com",
		IsFirstCheckpoint: true,
	})
	if err != nil {
		t.Fatalf("WriteTemporary() error = %v", err)
	}

	// Verify the shadow branch commit does NOT contain node_modules/
	commit, err := repo.CommitObject(result.CommitHash)
	if err != nil {
		t.Fatalf("failed to get commit object: %v", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		t.Fatalf("failed to get tree: %v", err)
	}

	// node_modules/some-package.js should NOT be in the tree
	_, err = tree.File("node_modules/some-package.js")
	if err == nil {
		t.Error("gitignored file node_modules/some-package.js should NOT be in checkpoint tree")
	} else if !errors.Is(err, object.ErrFileNotFound) && !errors.Is(err, object.ErrEntryNotFound) {
		t.Fatalf("expected node_modules/some-package.js to be absent (ErrFileNotFound/ErrEntryNotFound), got: %v", err)
	}
}

// TestWriteTemporary_FirstCheckpoint_UserAndAgentChanges verifies that
// the first checkpoint captures both user's pre-existing changes and agent changes.
func TestWriteTemporary_FirstCheckpoint_UserAndAgentChanges(t *testing.T) {
	tempDir := t.TempDir()

	// Initialize a git repository with an initial commit
	repo, err := git.PlainInit(tempDir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	// Create and commit README.md and main.go
	readmeFile := filepath.Join(tempDir, "README.md")
	if err := os.WriteFile(readmeFile, []byte("# Original\n"), 0o644); err != nil {
		t.Fatalf("failed to write README: %v", err)
	}
	mainFile := filepath.Join(tempDir, "main.go")
	if err := os.WriteFile(mainFile, []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("failed to write main.go: %v", err)
	}
	if _, err := worktree.Add("README.md"); err != nil {
		t.Fatalf("failed to add README: %v", err)
	}
	if _, err := worktree.Add("main.go"); err != nil {
		t.Fatalf("failed to add main.go: %v", err)
	}
	initialCommit, err := worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com"},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	// User modifies README.md BEFORE agent starts
	userModifiedContent := "# Modified by User\n"
	if err := os.WriteFile(readmeFile, []byte(userModifiedContent), 0o644); err != nil {
		t.Fatalf("failed to modify README: %v", err)
	}

	// Agent modifies main.go
	agentModifiedContent := "package main\n\nfunc main() {\n\tprintln(\"Hello\")\n}\n"
	if err := os.WriteFile(mainFile, []byte(agentModifiedContent), 0o644); err != nil {
		t.Fatalf("failed to modify main.go: %v", err)
	}

	// Change to temp dir so paths.RepoRoot() works correctly
	t.Chdir(tempDir)

	// Create metadata directory
	metadataDir := filepath.Join(tempDir, ".entire", "metadata", "test-session")
	if err := os.MkdirAll(metadataDir, 0o755); err != nil {
		t.Fatalf("failed to create metadata dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(metadataDir, "full.jsonl"), []byte(`{"test": true}`), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	// Create checkpoint - agent reports main.go as modified (from transcript)
	store := NewGitStore(repo)
	baseCommit := initialCommit.String()

	result, err := store.WriteTemporary(context.Background(), WriteTemporaryOptions{
		SessionID:         "test-session",
		BaseCommit:        baseCommit,
		ModifiedFiles:     []string{"main.go"}, // Only agent-modified file in list
		MetadataDir:       ".entire/metadata/test-session",
		MetadataDirAbs:    metadataDir,
		CommitMessage:     "First checkpoint",
		AuthorName:        "Test",
		AuthorEmail:       "test@test.com",
		IsFirstCheckpoint: true,
	})
	if err != nil {
		t.Fatalf("WriteTemporary() error = %v", err)
	}

	// Verify the checkpoint contains BOTH changes
	commit, err := repo.CommitObject(result.CommitHash)
	if err != nil {
		t.Fatalf("failed to get commit object: %v", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		t.Fatalf("failed to get tree: %v", err)
	}

	// Check README.md has user's modification
	readmeTreeFile, err := tree.File("README.md")
	if err != nil {
		t.Fatalf("README.md not found in tree: %v", err)
	}
	readmeContent, err := readmeTreeFile.Contents()
	if err != nil {
		t.Fatalf("failed to read README.md content: %v", err)
	}
	if readmeContent != userModifiedContent {
		t.Errorf("README.md should have user's modification\ngot:\n%s\nwant:\n%s", readmeContent, userModifiedContent)
	}

	// Check main.go has agent's modification
	mainTreeFile, err := tree.File("main.go")
	if err != nil {
		t.Fatalf("main.go not found in tree: %v", err)
	}
	mainContent, err := mainTreeFile.Contents()
	if err != nil {
		t.Fatalf("failed to read main.go content: %v", err)
	}
	if mainContent != agentModifiedContent {
		t.Errorf("main.go should have agent's modification\ngot:\n%s\nwant:\n%s", mainContent, agentModifiedContent)
	}
}

// TestWriteTemporary_FirstCheckpoint_CapturesUserDeletedFiles verifies that
// the first checkpoint excludes files that the user deleted before the session started.
func TestWriteTemporary_FirstCheckpoint_CapturesUserDeletedFiles(t *testing.T) {
	tempDir := t.TempDir()

	// Initialize a git repository with an initial commit
	repo, err := git.PlainInit(tempDir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	// Create and commit two files
	keepFile := filepath.Join(tempDir, "keep.txt")
	if err := os.WriteFile(keepFile, []byte("keep this"), 0o644); err != nil {
		t.Fatalf("failed to write keep.txt: %v", err)
	}
	deleteFile := filepath.Join(tempDir, "delete-me.txt")
	if err := os.WriteFile(deleteFile, []byte("delete this"), 0o644); err != nil {
		t.Fatalf("failed to write delete-me.txt: %v", err)
	}

	if _, err := worktree.Add("keep.txt"); err != nil {
		t.Fatalf("failed to add keep.txt: %v", err)
	}
	if _, err := worktree.Add("delete-me.txt"); err != nil {
		t.Fatalf("failed to add delete-me.txt: %v", err)
	}

	initialCommit, err := worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// User deletes delete-me.txt BEFORE the session starts
	if err := os.Remove(deleteFile); err != nil {
		t.Fatalf("failed to delete file: %v", err)
	}

	// Change to temp dir so paths.RepoRoot() works correctly
	t.Chdir(tempDir)

	// Create metadata directory
	metadataDir := filepath.Join(tempDir, ".entire", "metadata", "test-session")
	if err := os.MkdirAll(metadataDir, 0o755); err != nil {
		t.Fatalf("failed to create metadata dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(metadataDir, "full.jsonl"), []byte(`{"test": true}`), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	// Create checkpoint store and write first checkpoint
	store := NewGitStore(repo)
	baseCommit := initialCommit.String()

	result, err := store.WriteTemporary(context.Background(), WriteTemporaryOptions{
		SessionID:         "test-session",
		BaseCommit:        baseCommit,
		ModifiedFiles:     []string{},
		DeletedFiles:      []string{}, // No agent deletions
		MetadataDir:       ".entire/metadata/test-session",
		MetadataDirAbs:    metadataDir,
		CommitMessage:     "First checkpoint",
		AuthorName:        "Test",
		AuthorEmail:       "test@test.com",
		IsFirstCheckpoint: true,
	})
	if err != nil {
		t.Fatalf("WriteTemporary() error = %v", err)
	}

	// Verify the checkpoint tree
	commit, err := repo.CommitObject(result.CommitHash)
	if err != nil {
		t.Fatalf("failed to get commit object: %v", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		t.Fatalf("failed to get tree: %v", err)
	}

	// keep.txt should be in the tree (unchanged from HEAD)
	if _, err := tree.File("keep.txt"); err != nil {
		t.Errorf("keep.txt should be in checkpoint tree: %v", err)
	}

	// delete-me.txt should NOT be in the tree (user deleted it)
	_, err = tree.File("delete-me.txt")
	if err == nil {
		t.Error("delete-me.txt should NOT be in checkpoint tree (user deleted it before session)")
	} else if !errors.Is(err, object.ErrFileNotFound) && !errors.Is(err, object.ErrEntryNotFound) {
		t.Fatalf("expected delete-me.txt to be absent (ErrFileNotFound/ErrEntryNotFound), got: %v", err)
	}
}

// TestWriteTemporary_FirstCheckpoint_CapturesRenamedFiles verifies that
// the first checkpoint captures renamed files correctly.
func TestWriteTemporary_FirstCheckpoint_CapturesRenamedFiles(t *testing.T) {
	tempDir := t.TempDir()

	// Initialize a git repository with an initial commit
	repo, err := git.PlainInit(tempDir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	// Create and commit a file
	oldFile := filepath.Join(tempDir, "old-name.txt")
	if err := os.WriteFile(oldFile, []byte("content"), 0o644); err != nil {
		t.Fatalf("failed to write old-name.txt: %v", err)
	}

	if _, err := worktree.Add("old-name.txt"); err != nil {
		t.Fatalf("failed to add old-name.txt: %v", err)
	}

	initialCommit, err := worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// User renames the file using git mv BEFORE the session starts
	// Using git mv ensures git reports this as R (rename) status, not separate D+A
	cmd := exec.CommandContext(context.Background(), "git", "mv", "old-name.txt", "new-name.txt")
	cmd.Dir = tempDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to git mv: %v", err)
	}

	// Change to temp dir so paths.RepoRoot() works correctly
	t.Chdir(tempDir)

	// Create metadata directory
	metadataDir := filepath.Join(tempDir, ".entire", "metadata", "test-session")
	if err := os.MkdirAll(metadataDir, 0o755); err != nil {
		t.Fatalf("failed to create metadata dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(metadataDir, "full.jsonl"), []byte(`{"test": true}`), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	// Create checkpoint store and write first checkpoint
	store := NewGitStore(repo)
	baseCommit := initialCommit.String()

	result, err := store.WriteTemporary(context.Background(), WriteTemporaryOptions{
		SessionID:         "test-session",
		BaseCommit:        baseCommit,
		ModifiedFiles:     []string{},
		DeletedFiles:      []string{},
		MetadataDir:       ".entire/metadata/test-session",
		MetadataDirAbs:    metadataDir,
		CommitMessage:     "First checkpoint",
		AuthorName:        "Test",
		AuthorEmail:       "test@test.com",
		IsFirstCheckpoint: true,
	})
	if err != nil {
		t.Fatalf("WriteTemporary() error = %v", err)
	}

	// Verify the checkpoint tree
	commit, err := repo.CommitObject(result.CommitHash)
	if err != nil {
		t.Fatalf("failed to get commit object: %v", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		t.Fatalf("failed to get tree: %v", err)
	}

	// new-name.txt should be in the tree
	if _, err := tree.File("new-name.txt"); err != nil {
		t.Errorf("new-name.txt should be in checkpoint tree: %v", err)
	}

	// old-name.txt should NOT be in the tree (renamed away)
	_, err = tree.File("old-name.txt")
	if err == nil {
		t.Error("old-name.txt should NOT be in checkpoint tree (file was renamed)")
	} else if !errors.Is(err, object.ErrFileNotFound) && !errors.Is(err, object.ErrEntryNotFound) {
		t.Fatalf("expected old-name.txt to be absent (ErrFileNotFound/ErrEntryNotFound), got: %v", err)
	}
}

// TestWriteTemporary_FirstCheckpoint_FilenamesWithSpaces verifies that
// filenames with spaces are handled correctly.
func TestWriteTemporary_FirstCheckpoint_FilenamesWithSpaces(t *testing.T) {
	tempDir := t.TempDir()

	// Initialize a git repository with an initial commit
	repo, err := git.PlainInit(tempDir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	// Create and commit a simple file first
	simpleFile := filepath.Join(tempDir, "simple.txt")
	if err := os.WriteFile(simpleFile, []byte("simple"), 0o644); err != nil {
		t.Fatalf("failed to write simple.txt: %v", err)
	}

	if _, err := worktree.Add("simple.txt"); err != nil {
		t.Fatalf("failed to add simple.txt: %v", err)
	}

	initialCommit, err := worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// User creates a file with spaces in the name
	spacesFile := filepath.Join(tempDir, "file with spaces.txt")
	if err := os.WriteFile(spacesFile, []byte("content with spaces"), 0o644); err != nil {
		t.Fatalf("failed to write file with spaces: %v", err)
	}

	// Change to temp dir so paths.RepoRoot() works correctly
	t.Chdir(tempDir)

	// Create metadata directory
	metadataDir := filepath.Join(tempDir, ".entire", "metadata", "test-session")
	if err := os.MkdirAll(metadataDir, 0o755); err != nil {
		t.Fatalf("failed to create metadata dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(metadataDir, "full.jsonl"), []byte(`{"test": true}`), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	// Create checkpoint store and write first checkpoint
	store := NewGitStore(repo)
	baseCommit := initialCommit.String()

	result, err := store.WriteTemporary(context.Background(), WriteTemporaryOptions{
		SessionID:         "test-session",
		BaseCommit:        baseCommit,
		ModifiedFiles:     []string{},
		DeletedFiles:      []string{},
		MetadataDir:       ".entire/metadata/test-session",
		MetadataDirAbs:    metadataDir,
		CommitMessage:     "First checkpoint",
		AuthorName:        "Test",
		AuthorEmail:       "test@test.com",
		IsFirstCheckpoint: true,
	})
	if err != nil {
		t.Fatalf("WriteTemporary() error = %v", err)
	}

	// Verify the checkpoint tree
	commit, err := repo.CommitObject(result.CommitHash)
	if err != nil {
		t.Fatalf("failed to get commit object: %v", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		t.Fatalf("failed to get tree: %v", err)
	}

	// "file with spaces.txt" should be in the tree with correct name
	if _, err := tree.File("file with spaces.txt"); err != nil {
		t.Errorf("'file with spaces.txt' should be in checkpoint tree: %v", err)
	}
}

// =============================================================================
// Duplicate Session ID Tests - Tests for ENT-252 where the same session ID
// written twice to the same checkpoint should update in-place, not append.
// =============================================================================

// TestWriteCommitted_DuplicateSessionIDUpdatesInPlace verifies that writing
// the same session ID twice to the same checkpoint updates the existing slot
// rather than creating a duplicate subdirectory.
func TestWriteCommitted_DuplicateSessionIDUpdatesInPlace(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo)
	checkpointID := id.MustCheckpointID("deda01234567")

	// Write session "X" with initial data
	err := store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID:     checkpointID,
		SessionID:        "session-X",
		Strategy:         "manual-commit",
		Transcript:       []byte(`{"message": "session X v1"}`),
		FilesTouched:     []string{"a.go"},
		CheckpointsCount: 3,
		TokenUsage: &agent.TokenUsage{
			InputTokens:  100,
			OutputTokens: 50,
			APICallCount: 5,
		},
		AuthorName:  "Test Author",
		AuthorEmail: "test@example.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() session X v1 error = %v", err)
	}

	// Write session "Y"
	err = store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID:     checkpointID,
		SessionID:        "session-Y",
		Strategy:         "manual-commit",
		Transcript:       []byte(`{"message": "session Y"}`),
		FilesTouched:     []string{"b.go"},
		CheckpointsCount: 2,
		TokenUsage: &agent.TokenUsage{
			InputTokens:  50,
			OutputTokens: 25,
			APICallCount: 3,
		},
		AuthorName:  "Test Author",
		AuthorEmail: "test@example.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() session Y error = %v", err)
	}

	// Write session "X" again with updated data (should replace, not append)
	err = store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID:     checkpointID,
		SessionID:        "session-X",
		Strategy:         "manual-commit",
		Transcript:       []byte(`{"message": "session X v2"}`),
		FilesTouched:     []string{"a.go", "c.go"},
		CheckpointsCount: 5,
		TokenUsage: &agent.TokenUsage{
			InputTokens:  200,
			OutputTokens: 100,
			APICallCount: 10,
		},
		AuthorName:  "Test Author",
		AuthorEmail: "test@example.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() session X v2 error = %v", err)
	}

	// Read the checkpoint summary
	summary, err := store.ReadCommitted(context.Background(), checkpointID)
	if err != nil {
		t.Fatalf("ReadCommitted() error = %v", err)
	}
	if summary == nil {
		t.Fatal("ReadCommitted() returned nil summary")
	}

	// Should have 2 sessions, not 3
	if len(summary.Sessions) != 2 {
		t.Errorf("len(summary.Sessions) = %d, want 2 (not 3 - duplicate should be replaced)", len(summary.Sessions))
	}

	// Verify session 0 has updated data (session X v2)
	content0, err := store.ReadSessionContent(context.Background(), checkpointID, 0)
	if err != nil {
		t.Fatalf("ReadSessionContent(0) error = %v", err)
	}
	if content0.Metadata.SessionID != "session-X" {
		t.Errorf("session 0 SessionID = %q, want %q", content0.Metadata.SessionID, "session-X")
	}
	if content0.Metadata.CheckpointsCount != 5 {
		t.Errorf("session 0 CheckpointsCount = %d, want 5", content0.Metadata.CheckpointsCount)
	}
	if !strings.Contains(string(content0.Transcript), "session X v2") {
		t.Errorf("session 0 transcript should contain 'session X v2', got %s", string(content0.Transcript))
	}

	// Verify session 1 is still "Y" (unchanged)
	content1, err := store.ReadSessionContent(context.Background(), checkpointID, 1)
	if err != nil {
		t.Fatalf("ReadSessionContent(1) error = %v", err)
	}
	if content1.Metadata.SessionID != "session-Y" {
		t.Errorf("session 1 SessionID = %q, want %q", content1.Metadata.SessionID, "session-Y")
	}

	// Verify aggregated stats: count = 5 (X v2) + 2 (Y) = 7
	if summary.CheckpointsCount != 7 {
		t.Errorf("summary.CheckpointsCount = %d, want 7", summary.CheckpointsCount)
	}

	// Verify merged files: [a.go, b.go, c.go]
	expectedFiles := []string{"a.go", "b.go", "c.go"}
	if len(summary.FilesTouched) != len(expectedFiles) {
		t.Errorf("len(summary.FilesTouched) = %d, want %d", len(summary.FilesTouched), len(expectedFiles))
	}
	for i, want := range expectedFiles {
		if i < len(summary.FilesTouched) && summary.FilesTouched[i] != want {
			t.Errorf("summary.FilesTouched[%d] = %q, want %q", i, summary.FilesTouched[i], want)
		}
	}

	// Verify aggregated tokens: 200 (X v2) + 50 (Y) = 250
	if summary.TokenUsage == nil {
		t.Fatal("summary.TokenUsage should not be nil")
	}
	if summary.TokenUsage.InputTokens != 250 {
		t.Errorf("summary.TokenUsage.InputTokens = %d, want 250", summary.TokenUsage.InputTokens)
	}
	if summary.TokenUsage.OutputTokens != 125 {
		t.Errorf("summary.TokenUsage.OutputTokens = %d, want 125", summary.TokenUsage.OutputTokens)
	}
	if summary.TokenUsage.APICallCount != 13 {
		t.Errorf("summary.TokenUsage.APICallCount = %d, want 13", summary.TokenUsage.APICallCount)
	}
}

// TestWriteCommitted_DuplicateSessionIDSingleSession verifies that writing
// the same session ID twice when it's the only session updates in-place.
func TestWriteCommitted_DuplicateSessionIDSingleSession(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo)
	checkpointID := id.MustCheckpointID("dedb07654321")

	// Write session "X" with initial data
	err := store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID:     checkpointID,
		SessionID:        "session-X",
		Strategy:         "manual-commit",
		Transcript:       []byte(`{"message": "v1"}`),
		FilesTouched:     []string{"old.go"},
		CheckpointsCount: 1,
		AuthorName:       "Test Author",
		AuthorEmail:      "test@example.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() v1 error = %v", err)
	}

	// Write session "X" again with updated data
	err = store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID:     checkpointID,
		SessionID:        "session-X",
		Strategy:         "manual-commit",
		Transcript:       []byte(`{"message": "v2"}`),
		FilesTouched:     []string{"new.go"},
		CheckpointsCount: 5,
		AuthorName:       "Test Author",
		AuthorEmail:      "test@example.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() v2 error = %v", err)
	}

	// Read the checkpoint summary
	summary, err := store.ReadCommitted(context.Background(), checkpointID)
	if err != nil {
		t.Fatalf("ReadCommitted() error = %v", err)
	}
	if summary == nil {
		t.Fatal("ReadCommitted() returned nil summary")
	}

	// Should have 1 session, not 2
	if len(summary.Sessions) != 1 {
		t.Errorf("len(summary.Sessions) = %d, want 1 (duplicate should be replaced)", len(summary.Sessions))
	}

	// Verify session has updated data
	content, err := store.ReadSessionContent(context.Background(), checkpointID, 0)
	if err != nil {
		t.Fatalf("ReadSessionContent(0) error = %v", err)
	}
	if content.Metadata.SessionID != "session-X" {
		t.Errorf("session 0 SessionID = %q, want %q", content.Metadata.SessionID, "session-X")
	}
	if content.Metadata.CheckpointsCount != 5 {
		t.Errorf("session 0 CheckpointsCount = %d, want 5 (updated value)", content.Metadata.CheckpointsCount)
	}
	if !strings.Contains(string(content.Transcript), "v2") {
		t.Errorf("session 0 transcript should contain 'v2', got %s", string(content.Transcript))
	}

	// Verify aggregated stats match the single session
	if summary.CheckpointsCount != 5 {
		t.Errorf("summary.CheckpointsCount = %d, want 5", summary.CheckpointsCount)
	}
	expectedFiles := []string{"new.go"}
	if len(summary.FilesTouched) != 1 || summary.FilesTouched[0] != "new.go" {
		t.Errorf("summary.FilesTouched = %v, want %v", summary.FilesTouched, expectedFiles)
	}
}

// TestWriteCommitted_DuplicateSessionIDReusesIndex verifies that when a session ID
// already exists at index 0, writing it again reuses index 0 (not index 2).
// The session file paths in the summary must point to /0/, not /2/.
func TestWriteCommitted_DuplicateSessionIDReusesIndex(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo)
	checkpointID := id.MustCheckpointID("dedc0abcdef1")

	// Write session A at index 0
	err := store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID:     checkpointID,
		SessionID:        "session-A",
		Strategy:         "manual-commit",
		Transcript:       []byte(`{"v": 1}`),
		CheckpointsCount: 1,
		AuthorName:       "Test",
		AuthorEmail:      "test@example.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() session A error = %v", err)
	}

	// Write session B at index 1
	err = store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID:     checkpointID,
		SessionID:        "session-B",
		Strategy:         "manual-commit",
		Transcript:       []byte(`{"v": 2}`),
		CheckpointsCount: 1,
		AuthorName:       "Test",
		AuthorEmail:      "test@example.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() session B error = %v", err)
	}

	// Write session A again — should reuse index 0, not create index 2
	err = store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID:     checkpointID,
		SessionID:        "session-A",
		Strategy:         "manual-commit",
		Transcript:       []byte(`{"v": 3}`),
		CheckpointsCount: 2,
		AuthorName:       "Test",
		AuthorEmail:      "test@example.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() session A v2 error = %v", err)
	}

	summary, err := store.ReadCommitted(context.Background(), checkpointID)
	if err != nil {
		t.Fatalf("ReadCommitted() error = %v", err)
	}

	// Must still be 2 sessions
	if len(summary.Sessions) != 2 {
		t.Fatalf("len(summary.Sessions) = %d, want 2", len(summary.Sessions))
	}

	// Session A's file paths must point to subdirectory /0/, not /2/
	if !strings.Contains(summary.Sessions[0].Transcript, "/0/") {
		t.Errorf("session A should be at index 0, got transcript path %s", summary.Sessions[0].Transcript)
	}

	// Session B stays at /1/
	if !strings.Contains(summary.Sessions[1].Transcript, "/1/") {
		t.Errorf("session B should be at index 1, got transcript path %s", summary.Sessions[1].Transcript)
	}

	// Verify index 0 has the updated content
	content, err := store.ReadSessionContent(context.Background(), checkpointID, 0)
	if err != nil {
		t.Fatalf("ReadSessionContent(0) error = %v", err)
	}
	if content.Metadata.SessionID != "session-A" {
		t.Errorf("session 0 SessionID = %q, want %q", content.Metadata.SessionID, "session-A")
	}
	if !strings.Contains(string(content.Transcript), `"v": 3`) {
		t.Errorf("session 0 should have updated transcript, got %s", string(content.Transcript))
	}
}

// TestWriteCommitted_DuplicateSessionIDClearsStaleFiles verifies that when a session
// is overwritten in-place, optional files from the previous write (prompts, context)
// do not persist if the new write omits them, and sibling session data is untouched.
func TestWriteCommitted_DuplicateSessionIDClearsStaleFiles(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo)
	checkpointID := id.MustCheckpointID("dedd0abcdef2")

	// Write session A with prompts and context
	err := store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID:     checkpointID,
		SessionID:        "session-A",
		Strategy:         "manual-commit",
		Transcript:       []byte(`{"v": 1}`),
		Prompts:          []string{"original prompt"},
		Context:          []byte("original context"),
		CheckpointsCount: 1,
		AuthorName:       "Test",
		AuthorEmail:      "test@example.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() A v1 error = %v", err)
	}

	// Write session B with prompts and context
	err = store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID:     checkpointID,
		SessionID:        "session-B",
		Strategy:         "manual-commit",
		Transcript:       []byte(`{"session": "B"}`),
		Prompts:          []string{"B prompt"},
		Context:          []byte("B context"),
		CheckpointsCount: 1,
		AuthorName:       "Test",
		AuthorEmail:      "test@example.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() B error = %v", err)
	}

	// Overwrite session A WITHOUT prompts or context
	err = store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID:     checkpointID,
		SessionID:        "session-A",
		Strategy:         "manual-commit",
		Transcript:       []byte(`{"v": 2}`),
		Prompts:          nil,
		Context:          nil,
		CheckpointsCount: 2,
		AuthorName:       "Test",
		AuthorEmail:      "test@example.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() A v2 error = %v", err)
	}

	// Session A: stale prompts and context should be cleared
	contentA, err := store.ReadSessionContent(context.Background(), checkpointID, 0)
	if err != nil {
		t.Fatalf("ReadSessionContent(0) error = %v", err)
	}
	if contentA.Prompts != "" {
		t.Errorf("session A stale prompts should be cleared, got %q", contentA.Prompts)
	}
	if contentA.Context != "" {
		t.Errorf("session A stale context should be cleared, got %q", contentA.Context)
	}
	if !strings.Contains(string(contentA.Transcript), `"v": 2`) {
		t.Errorf("session A transcript should be updated, got %s", string(contentA.Transcript))
	}

	// Session B: data must be untouched
	contentB, err := store.ReadSessionContent(context.Background(), checkpointID, 1)
	if err != nil {
		t.Fatalf("ReadSessionContent(1) error = %v", err)
	}
	if contentB.Metadata.SessionID != "session-B" {
		t.Errorf("session B SessionID = %q, want %q", contentB.Metadata.SessionID, "session-B")
	}
	if !strings.Contains(contentB.Prompts, "B prompt") {
		t.Errorf("session B prompts should be preserved, got %q", contentB.Prompts)
	}
	if !strings.Contains(contentB.Context, "B context") {
		t.Errorf("session B context should be preserved, got %q", contentB.Context)
	}
}

// highEntropySecret is a string with Shannon entropy > 4.5 that will trigger redaction.
const highEntropySecret = "sk-ant-api03-xK9mZ2vL8nQ5rT1wY4bC7dF0gH3jE6pA"

func TestWriteCommitted_RedactsTranscriptSecrets(t *testing.T) {
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo)
	checkpointID := id.MustCheckpointID("aabbccddeef1")

	transcript := []byte(`{"role":"assistant","content":"Here is your key: ` + highEntropySecret + `"}` + "\n")

	err := store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID:     checkpointID,
		SessionID:        "redact-transcript-session",
		Strategy:         "manual-commit",
		Transcript:       transcript,
		CheckpointsCount: 1,
		AuthorName:       "Test Author",
		AuthorEmail:      "test@example.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() error = %v", err)
	}

	content, err := store.ReadSessionContent(context.Background(), checkpointID, 0)
	if err != nil {
		t.Fatalf("ReadSessionContent() error = %v", err)
	}

	if strings.Contains(string(content.Transcript), highEntropySecret) {
		t.Error("transcript should not contain the secret after redaction")
	}
	if !strings.Contains(string(content.Transcript), "REDACTED") {
		t.Error("transcript should contain REDACTED placeholder")
	}
}

func TestWriteCommitted_RedactsPromptSecrets(t *testing.T) {
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo)
	checkpointID := id.MustCheckpointID("aabbccddeef2")

	err := store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID:     checkpointID,
		SessionID:        "redact-prompt-session",
		Strategy:         "manual-commit",
		Transcript:       []byte(`{"msg":"safe"}`),
		Prompts:          []string{"Set API_KEY=" + highEntropySecret},
		CheckpointsCount: 1,
		AuthorName:       "Test Author",
		AuthorEmail:      "test@example.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() error = %v", err)
	}

	content, err := store.ReadSessionContent(context.Background(), checkpointID, 0)
	if err != nil {
		t.Fatalf("ReadSessionContent() error = %v", err)
	}

	if strings.Contains(content.Prompts, highEntropySecret) {
		t.Error("prompts should not contain the secret after redaction")
	}
	if !strings.Contains(content.Prompts, "REDACTED") {
		t.Error("prompts should contain REDACTED placeholder")
	}
}

func TestWriteCommitted_RedactsContextSecrets(t *testing.T) {
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo)
	checkpointID := id.MustCheckpointID("aabbccddeef3")

	err := store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID:     checkpointID,
		SessionID:        "redact-context-session",
		Strategy:         "manual-commit",
		Transcript:       []byte(`{"msg":"safe"}`),
		Context:          []byte("DB_PASSWORD=" + highEntropySecret),
		CheckpointsCount: 1,
		AuthorName:       "Test Author",
		AuthorEmail:      "test@example.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() error = %v", err)
	}

	content, err := store.ReadSessionContent(context.Background(), checkpointID, 0)
	if err != nil {
		t.Fatalf("ReadSessionContent() error = %v", err)
	}

	if strings.Contains(content.Context, highEntropySecret) {
		t.Error("context should not contain the secret after redaction")
	}
	if !strings.Contains(content.Context, "REDACTED") {
		t.Error("context should contain REDACTED placeholder")
	}
}

func TestCopyMetadataDir_RedactsSecrets(t *testing.T) {
	tempDir := t.TempDir()

	repo, err := git.PlainInit(tempDir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	metadataDir := filepath.Join(tempDir, "metadata")
	if err := os.MkdirAll(metadataDir, 0o755); err != nil {
		t.Fatalf("failed to create metadata dir: %v", err)
	}

	// Write a JSONL file with a secret
	jsonlFile := filepath.Join(metadataDir, "agent.jsonl")
	if err := os.WriteFile(jsonlFile, []byte(`{"content":"key=`+highEntropySecret+`"}`+"\n"), 0o644); err != nil {
		t.Fatalf("failed to write jsonl file: %v", err)
	}

	// Write a plain text file with a secret
	txtFile := filepath.Join(metadataDir, "notes.txt")
	if err := os.WriteFile(txtFile, []byte("secret: "+highEntropySecret), 0o644); err != nil {
		t.Fatalf("failed to write txt file: %v", err)
	}

	store := NewGitStore(repo)
	entries := make(map[string]object.TreeEntry)

	if err := store.copyMetadataDir(metadataDir, "cp/", entries); err != nil {
		t.Fatalf("copyMetadataDir() error = %v", err)
	}

	// Verify both files were added
	if _, ok := entries["cp/agent.jsonl"]; !ok {
		t.Fatal("agent.jsonl should be in entries")
	}
	if _, ok := entries["cp/notes.txt"]; !ok {
		t.Fatal("notes.txt should be in entries")
	}

	// Read back the blob content and verify redaction
	for path, entry := range entries {
		blob, bErr := repo.BlobObject(entry.Hash)
		if bErr != nil {
			t.Fatalf("failed to read blob for %s: %v", path, bErr)
		}
		reader, rErr := blob.Reader()
		if rErr != nil {
			t.Fatalf("failed to get reader for %s: %v", path, rErr)
		}
		buf := make([]byte, blob.Size)
		if _, rErr = reader.Read(buf); rErr != nil && rErr.Error() != "EOF" {
			t.Fatalf("failed to read blob content for %s: %v", path, rErr)
		}
		reader.Close()

		content := string(buf)
		if strings.Contains(content, highEntropySecret) {
			t.Errorf("%s should not contain the secret after redaction", path)
		}
		if !strings.Contains(content, "REDACTED") {
			t.Errorf("%s should contain REDACTED placeholder", path)
		}
	}
}

func TestWriteCommitted_MetadataJSON_RedactsSecrets(t *testing.T) {
	t.Parallel()

	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo)
	checkpointID := id.MustCheckpointID("aabbccddeef4")

	err := store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID: checkpointID,
		SessionID:    "redact-metadata-json-session",
		Strategy:     "manual-commit",
		Transcript:   []byte(`{"msg":"safe"}`),
		Summary: &Summary{
			Intent: highEntropySecret,
		},
		CheckpointsCount: 1,
		AuthorName:       "Test Author",
		AuthorEmail:      "test@example.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() error = %v", err)
	}

	metadata := readLatestSessionMetadata(t, repo, checkpointID)
	if metadata.Summary == nil {
		t.Fatal("expected summary to be present in session metadata")
	}

	if strings.Contains(metadata.Summary.Intent, highEntropySecret) {
		t.Error("session metadata summary intent should not contain the secret after redaction")
	}
	if !strings.Contains(metadata.Summary.Intent, "REDACTED") {
		t.Error("session metadata summary intent should contain REDACTED placeholder")
	}
}

func TestWriteCommitted_UpdateSummary_RedactsSecrets(t *testing.T) {
	t.Parallel()

	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo)
	checkpointID := id.MustCheckpointID("aabbccddeef5")

	err := store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID:     checkpointID,
		SessionID:        "redact-update-summary-session",
		Strategy:         "manual-commit",
		Transcript:       []byte(`{"msg":"safe"}`),
		CheckpointsCount: 1,
		AuthorName:       "Test Author",
		AuthorEmail:      "test@example.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() error = %v", err)
	}

	if err := store.UpdateSummary(context.Background(), checkpointID, &Summary{Intent: highEntropySecret}); err != nil {
		t.Fatalf("UpdateSummary() error = %v", err)
	}

	metadata := readLatestSessionMetadata(t, repo, checkpointID)
	if metadata.Summary == nil {
		t.Fatal("expected summary to be present in session metadata")
	}
	if strings.Contains(metadata.Summary.Intent, highEntropySecret) {
		t.Error("session metadata summary intent should not contain the secret after redaction")
	}
	if !strings.Contains(metadata.Summary.Intent, "REDACTED") {
		t.Error("session metadata summary intent should contain REDACTED placeholder")
	}
}

func TestWriteCommitted_MetadataJSON_PreservesMetadataIDFields(t *testing.T) {
	t.Parallel()

	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo)
	checkpointID := id.MustCheckpointID("aabbccddeef6")

	sessionID := "session_" + highEntropySecret
	toolUseID := "toolu_" + highEntropySecret
	transcriptIdentifier := "message_" + highEntropySecret
	transcriptPath := ".claude/projects/" + highEntropySecret + "/transcript.jsonl"

	err := store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID:                checkpointID,
		SessionID:                   sessionID,
		ToolUseID:                   toolUseID,
		Strategy:                    "manual-commit",
		Transcript:                  []byte(`{"msg":"safe"}`),
		CheckpointsCount:            1,
		TranscriptIdentifierAtStart: transcriptIdentifier,
		SessionTranscriptPath:       transcriptPath,
		Summary: &Summary{
			Intent: highEntropySecret,
		},
		AuthorName:  "Test Author",
		AuthorEmail: "test@example.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() error = %v", err)
	}

	metadata := readLatestSessionMetadata(t, repo, checkpointID)
	if metadata.SessionID != sessionID {
		t.Errorf("session metadata session_id = %q, want %q", metadata.SessionID, sessionID)
	}
	if metadata.ToolUseID != toolUseID {
		t.Errorf("session metadata tool_use_id = %q, want %q", metadata.ToolUseID, toolUseID)
	}
	if metadata.TranscriptIdentifierAtStart != transcriptIdentifier {
		t.Errorf("session metadata transcript_identifier_at_start = %q, want %q", metadata.TranscriptIdentifierAtStart, transcriptIdentifier)
	}
	if metadata.TranscriptPath != transcriptPath {
		t.Errorf("session metadata transcript_path = %q, want %q", metadata.TranscriptPath, transcriptPath)
	}
	if metadata.Summary == nil {
		t.Fatal("expected summary to be present in session metadata")
	}
	if strings.Contains(metadata.Summary.Intent, highEntropySecret) {
		t.Error("session metadata summary intent should not contain the secret after redaction")
	}
	if !strings.Contains(metadata.Summary.Intent, "REDACTED") {
		t.Error("session metadata summary intent should contain REDACTED placeholder")
	}
}

func TestWriteCommitted_UpdateSummary_PreservesMetadataIDFields(t *testing.T) {
	t.Parallel()

	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo)
	checkpointID := id.MustCheckpointID("aabbccddeef7")

	sessionID := "session_" + highEntropySecret
	toolUseID := "toolu_" + highEntropySecret
	transcriptIdentifier := "message_" + highEntropySecret
	transcriptPath := ".claude/projects/" + highEntropySecret + "/transcript.jsonl"

	err := store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID:                checkpointID,
		SessionID:                   sessionID,
		ToolUseID:                   toolUseID,
		Strategy:                    "manual-commit",
		Transcript:                  []byte(`{"msg":"safe"}`),
		CheckpointsCount:            1,
		TranscriptIdentifierAtStart: transcriptIdentifier,
		SessionTranscriptPath:       transcriptPath,
		AuthorName:                  "Test Author",
		AuthorEmail:                 "test@example.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() error = %v", err)
	}

	if err := store.UpdateSummary(context.Background(), checkpointID, &Summary{Intent: highEntropySecret}); err != nil {
		t.Fatalf("UpdateSummary() error = %v", err)
	}

	metadata := readLatestSessionMetadata(t, repo, checkpointID)
	if metadata.SessionID != sessionID {
		t.Errorf("session metadata session_id = %q, want %q", metadata.SessionID, sessionID)
	}
	if metadata.ToolUseID != toolUseID {
		t.Errorf("session metadata tool_use_id = %q, want %q", metadata.ToolUseID, toolUseID)
	}
	if metadata.TranscriptIdentifierAtStart != transcriptIdentifier {
		t.Errorf("session metadata transcript_identifier_at_start = %q, want %q", metadata.TranscriptIdentifierAtStart, transcriptIdentifier)
	}
	if metadata.TranscriptPath != transcriptPath {
		t.Errorf("session metadata transcript_path = %q, want %q", metadata.TranscriptPath, transcriptPath)
	}
	if metadata.Summary == nil {
		t.Fatal("expected summary to be present in session metadata")
	}
	if strings.Contains(metadata.Summary.Intent, highEntropySecret) {
		t.Error("session metadata summary intent should not contain the secret after redaction")
	}
	if !strings.Contains(metadata.Summary.Intent, "REDACTED") {
		t.Error("session metadata summary intent should contain REDACTED placeholder")
	}
}

func TestWriteCommitted_TaskSubagentTranscript_FallsBackToPlainTextRedaction(t *testing.T) {
	t.Parallel()

	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo)
	checkpointID := id.MustCheckpointID("aabbccddeef8")

	tempDir := t.TempDir()
	subagentTranscriptPath := filepath.Join(tempDir, "subagent-transcript.jsonl")
	invalidJSONL := `{"role":"assistant","content":"` + highEntropySecret + `"`
	if err := os.WriteFile(subagentTranscriptPath, []byte(invalidJSONL), 0o644); err != nil {
		t.Fatalf("failed to write subagent transcript: %v", err)
	}

	err := store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID:           checkpointID,
		SessionID:              "redact-task-subagent-fallback-session",
		Strategy:               "manual-commit",
		Transcript:             []byte(`{"msg":"safe"}`),
		CheckpointsCount:       1,
		IsTask:                 true,
		ToolUseID:              "tool-1",
		AgentID:                "agent-1",
		SubagentTranscriptPath: subagentTranscriptPath,
		AuthorName:             "Test Author",
		AuthorEmail:            "test@example.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() error = %v", err)
	}

	ref, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	if err != nil {
		t.Fatalf("failed to get metadata branch reference: %v", err)
	}

	agentPath := checkpointID.Path() + "/tasks/tool-1/agent-agent-1.jsonl"
	agentContent := readFileFromCheckpointCommit(t, repo, ref.Hash(), agentPath)
	if strings.Contains(agentContent, highEntropySecret) {
		t.Error("subagent transcript should not contain the secret after fallback redaction")
	}
	if !strings.Contains(agentContent, "REDACTED") {
		t.Error("subagent transcript should contain REDACTED placeholder after fallback redaction")
	}
}

func TestWriteTemporaryTask_MetadataFiles_RedactSecrets(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()

	repo, err := git.PlainInit(tempDir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	readmeFile := filepath.Join(tempDir, "README.md")
	if err := os.WriteFile(readmeFile, []byte("# Test"), 0644); err != nil {
		t.Fatalf("failed to write README: %v", err)
	}
	if _, err := worktree.Add("README.md"); err != nil {
		t.Fatalf("failed to add README: %v", err)
	}
	initialCommit, err := worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com"},
	})
	if err != nil {
		t.Fatalf("failed to commit initial tree: %v", err)
	}

	store := NewGitStore(repo)

	transcriptPath := filepath.Join(tempDir, "task-full.jsonl")
	if err := os.WriteFile(transcriptPath, []byte(`{"role":"assistant","content":"`+highEntropySecret+`"}`+"\n"), 0644); err != nil {
		t.Fatalf("failed to write task transcript: %v", err)
	}

	agentTranscriptPath := filepath.Join(tempDir, "agent-transcript.jsonl")
	if err := os.WriteFile(agentTranscriptPath, []byte(`{"role":"assistant","content":"agent `+highEntropySecret+`"}`+"\n"), 0644); err != nil {
		t.Fatalf("failed to write agent transcript: %v", err)
	}

	commitHash, err := store.WriteTemporaryTask(context.Background(), WriteTemporaryTaskOptions{
		SessionID:              "redact-temp-task-session",
		BaseCommit:             initialCommit.String(),
		ToolUseID:              "tool-1",
		AgentID:                "agent-1",
		TranscriptPath:         transcriptPath,
		SubagentTranscriptPath: agentTranscriptPath,
		CommitMessage:          "task checkpoint",
		AuthorName:             "Test",
		AuthorEmail:            "test@example.com",
	})
	if err != nil {
		t.Fatalf("WriteTemporaryTask() error = %v", err)
	}

	metadataDir := strings.TrimSuffix(paths.EntireMetadataDir, "/") + "/redact-temp-task-session"
	transcriptFile := metadataDir + "/" + paths.TranscriptFileName
	subagentFile := strings.TrimSuffix(paths.EntireMetadataDir, "/") + "/redact-temp-task-session/tasks/tool-1/agent-agent-1.jsonl"

	transcriptContent := readFileFromCheckpointCommit(t, repo, commitHash, transcriptFile)
	if strings.Contains(transcriptContent, highEntropySecret) {
		t.Error("task transcript should not contain the secret after redaction")
	}
	if !strings.Contains(transcriptContent, "REDACTED") {
		t.Error("task transcript should contain REDACTED placeholder")
	}

	subagentContent := readFileFromCheckpointCommit(t, repo, commitHash, subagentFile)
	if strings.Contains(subagentContent, highEntropySecret) {
		t.Error("subagent transcript should not contain the secret after redaction")
	}
	if !strings.Contains(subagentContent, "REDACTED") {
		t.Error("subagent transcript should contain REDACTED placeholder")
	}
}

func TestWriteTemporary_MetadataDir_RedactsSecrets(t *testing.T) {
	tempDir := t.TempDir()

	repo, err := git.PlainInit(tempDir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	readmeFile := filepath.Join(tempDir, "README.md")
	if err := os.WriteFile(readmeFile, []byte("# Test"), 0644); err != nil {
		t.Fatalf("failed to write README: %v", err)
	}
	if _, err := worktree.Add("README.md"); err != nil {
		t.Fatalf("failed to add README: %v", err)
	}
	if _, err := worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com"},
	}); err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	origDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get current working directory: %v", err)
	}
	t.Cleanup(func() {
		t.Chdir(origDir)
	})

	// Ensure paths.RepoRoot() resolves to this repo in this test.
	t.Chdir(tempDir)

	store := NewGitStore(repo)
	metadataDir := filepath.Join(tempDir, ".entire", "metadata", "test-session")
	if err := os.MkdirAll(metadataDir, 0o755); err != nil {
		t.Fatalf("failed to create metadata dir: %v", err)
	}

	fullPath := filepath.Join(metadataDir, "full.jsonl")
	if err := os.WriteFile(fullPath, []byte(`{"content":"secret=`+highEntropySecret+`"}`+"\n"), 0644); err != nil {
		t.Fatalf("failed to write full.jsonl: %v", err)
	}

	notePath := filepath.Join(metadataDir, "notes.txt")
	if err := os.WriteFile(notePath, []byte("secret="+highEntropySecret), 0644); err != nil {
		t.Fatalf("failed to write notes.txt: %v", err)
	}

	head, err := repo.Head()
	if err != nil {
		t.Fatalf("failed to get HEAD: %v", err)
	}

	result, err := store.WriteTemporary(context.Background(), WriteTemporaryOptions{
		SessionID:         "redact-temp-commit-session",
		BaseCommit:        head.Hash().String(),
		ModifiedFiles:     []string{"README.md"},
		MetadataDir:       ".entire/metadata/test-session",
		MetadataDirAbs:    metadataDir,
		CommitMessage:     "checkpoint",
		AuthorName:        "Test",
		AuthorEmail:       "test@example.com",
		IsFirstCheckpoint: false,
	})
	if err != nil {
		t.Fatalf("WriteTemporary() error = %v", err)
	}

	full := readFileFromCheckpointCommit(t, repo, result.CommitHash, ".entire/metadata/test-session/full.jsonl")
	if strings.Contains(full, highEntropySecret) {
		t.Error("full.jsonl should not contain the secret after redaction")
	}
	if !strings.Contains(full, "REDACTED") {
		t.Error("full.jsonl should contain REDACTED placeholder")
	}

	notes := readFileFromCheckpointCommit(t, repo, result.CommitHash, ".entire/metadata/test-session/notes.txt")
	if strings.Contains(notes, highEntropySecret) {
		t.Error("notes.txt should not contain the secret after redaction")
	}
	if !strings.Contains(notes, "REDACTED") {
		t.Error("notes.txt should contain REDACTED placeholder")
	}
}

func TestWriteTemporary_MetadataDir_SkipsSymlinks(t *testing.T) {
	tempDir := t.TempDir()

	repo, err := git.PlainInit(tempDir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	readmeFile := filepath.Join(tempDir, "README.md")
	if err := os.WriteFile(readmeFile, []byte("# Test"), 0644); err != nil {
		t.Fatalf("failed to write README: %v", err)
	}
	if _, err := worktree.Add("README.md"); err != nil {
		t.Fatalf("failed to add README: %v", err)
	}
	if _, err := worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com"},
	}); err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	origDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get current working directory: %v", err)
	}
	t.Cleanup(func() {
		t.Chdir(origDir)
	})
	t.Chdir(tempDir)

	store := NewGitStore(repo)
	sessionID := "redact-temp-symlink-session"
	metadataDir := filepath.Join(tempDir, ".entire", "metadata", sessionID)
	if err := os.MkdirAll(metadataDir, 0o755); err != nil {
		t.Fatalf("failed to create metadata dir: %v", err)
	}

	regularFile := filepath.Join(metadataDir, "notes.txt")
	if err := os.WriteFile(regularFile, []byte("secret="+highEntropySecret), 0644); err != nil {
		t.Fatalf("failed to write notes.txt: %v", err)
	}

	sensitiveFile := filepath.Join(tempDir, "sensitive.txt")
	if err := os.WriteFile(sensitiveFile, []byte("very-secret-data"), 0644); err != nil {
		t.Fatalf("failed to write sensitive file: %v", err)
	}
	symlinkPath := filepath.Join(metadataDir, "sneaky-link")
	if err := os.Symlink(sensitiveFile, symlinkPath); err != nil {
		t.Fatalf("failed to create symlink: %v", err)
	}

	head, err := repo.Head()
	if err != nil {
		t.Fatalf("failed to get HEAD: %v", err)
	}

	result, err := store.WriteTemporary(context.Background(), WriteTemporaryOptions{
		SessionID:         sessionID,
		BaseCommit:        head.Hash().String(),
		ModifiedFiles:     []string{"README.md"},
		MetadataDir:       filepath.ToSlash(filepath.Join(paths.EntireMetadataDir, sessionID)),
		MetadataDirAbs:    metadataDir,
		CommitMessage:     "checkpoint",
		AuthorName:        "Test",
		AuthorEmail:       "test@example.com",
		IsFirstCheckpoint: false,
	})
	if err != nil {
		t.Fatalf("WriteTemporary() error = %v", err)
	}

	regular := readFileFromCheckpointCommit(t, repo, result.CommitHash, filepath.ToSlash(filepath.Join(paths.EntireMetadataDir, sessionID, "notes.txt")))
	if strings.Contains(regular, highEntropySecret) {
		t.Error("notes.txt should not contain the secret after redaction")
	}
	if !strings.Contains(regular, "REDACTED") {
		t.Error("notes.txt should contain REDACTED placeholder")
	}

	commit, err := repo.CommitObject(result.CommitHash)
	if err != nil {
		t.Fatalf("failed to get checkpoint commit object: %v", err)
	}
	tree, err := commit.Tree()
	if err != nil {
		t.Fatalf("failed to get checkpoint tree: %v", err)
	}
	if _, err := tree.File(filepath.ToSlash(filepath.Join(paths.EntireMetadataDir, sessionID, "sneaky-link"))); err == nil {
		t.Error("sneaky-link should not be included in temporary checkpoint metadata")
	}
}

// TestRedactSummary_CoversAllFields guards against Summary fields being added
// without updating redactSummary(). It uses struct-shape tripwires and
// reflection to verify that every string field is redacted, no field is
// silently zeroed, and non-string data is preserved unchanged.
func TestRedactSummary_CoversAllFields(t *testing.T) {
	t.Parallel()

	// Tripwire: fail when struct shape changes.
	assertFieldCount(t, Summary{}, 5, "Summary")
	assertFieldCount(t, LearningsSummary{}, 3, "LearningsSummary")
	assertFieldCount(t, CodeLearning{}, 4, "CodeLearning")

	// Auto-populate every field: strings get a secret marker,
	// ints get a non-zero sentinel, slices get one element.
	input := &Summary{}
	fillStructFields(reflect.ValueOf(input).Elem(), highEntropySecret)

	// Sanity: every scalar in the auto-filled input must be non-zero.
	// Catches fillStructFields silently skipping a new field kind.
	assertAllScalarsNonZero(t, reflect.ValueOf(input), "input")

	result := redactSummary(input)
	if result == nil {
		t.Fatal("redactSummary returned nil for non-nil input")
	}

	inputStrings := collectStringFields(reflect.ValueOf(input), "")
	resultStrings := collectStringFields(reflect.ValueOf(result), "")

	// Key sets must match exactly.
	for path := range inputStrings {
		if _, ok := resultStrings[path]; !ok {
			t.Errorf("field %s present in input but missing in result — redactSummary does not handle it", path)
		}
	}
	for path := range resultStrings {
		if _, ok := inputStrings[path]; !ok {
			t.Errorf("unexpected field %s in result but not in input", path)
		}
	}

	// Every input string must be non-empty (auto-fill sanity check).
	for path, val := range inputStrings {
		if val == "" {
			t.Fatalf("auto-fill bug: field %s is empty in input", path)
		}
	}

	// No result string may be empty (silently zeroed) or contain the raw secret.
	for path, val := range resultStrings {
		if val == "" {
			t.Errorf("field %s was silently zeroed — redactSummary does not copy this field", path)
		}
		if strings.Contains(val, highEntropySecret) {
			t.Errorf("field %s still contains raw secret after redaction", path)
		}
	}

	// Non-string invariant: only string content may differ.
	// Deep-copy both, normalize all strings to a fixed token, then DeepEqual.
	inputNorm := jsonRoundTripSummary(t, input)
	resultNorm := jsonRoundTripSummary(t, result)
	setAllStrings(reflect.ValueOf(inputNorm).Elem(), "X")
	setAllStrings(reflect.ValueOf(resultNorm).Elem(), "X")
	if !reflect.DeepEqual(inputNorm, resultNorm) {
		t.Errorf("redactSummary altered non-string data:\n  input  (normalized): %+v\n  result (normalized): %+v", inputNorm, resultNorm)
	}
}

func assertFieldCount(t *testing.T, v any, expected int, name string) {
	t.Helper()
	actual := reflect.TypeOf(v).NumField()
	if actual != expected {
		t.Fatalf("%s has %d fields (expected %d) — update redactSummary() and this test",
			name, actual, expected)
	}
}

// fillStructFields recursively populates every field in a struct with
// non-zero values: strings → secret, bools → true, ints/uints → 7,
// floats → 7.5, pointers → allocate and recurse, slices → one element.
func fillStructFields(v reflect.Value, secret string) {
	switch v.Kind() {
	case reflect.Ptr:
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		fillStructFields(v.Elem(), secret)
	case reflect.Struct:
		for i := range v.NumField() {
			fillStructFields(v.Field(i), secret)
		}
	case reflect.String:
		v.SetString(secret)
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(7)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(7)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(7.5)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fillStructFields(s.Index(0), secret)
		v.Set(s)
	}
}

// assertAllScalarsNonZero walks a struct and fatals if any scalar field
// (string, bool, int, uint, float) is at its zero value. This guards
// against fillStructFields silently skipping a new field kind.
func assertAllScalarsNonZero(t *testing.T, v reflect.Value, prefix string) {
	t.Helper()
	if v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return
		}
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.Struct:
		tp := v.Type()
		for i := range tp.NumField() {
			name := tp.Field(i).Name
			p := name
			if prefix != "" {
				p = prefix + "." + name
			}
			assertAllScalarsNonZero(t, v.Field(i), p)
		}
	case reflect.Slice:
		for i := range v.Len() {
			assertAllScalarsNonZero(t, v.Index(i), fmt.Sprintf("%s[%d]", prefix, i))
		}
	case reflect.String:
		if v.String() == "" {
			t.Fatalf("auto-fill bug: %s is empty string", prefix)
		}
	case reflect.Bool:
		if !v.Bool() {
			t.Fatalf("auto-fill bug: %s is false", prefix)
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if v.Int() == 0 {
			t.Fatalf("auto-fill bug: %s is zero", prefix)
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if v.Uint() == 0 {
			t.Fatalf("auto-fill bug: %s is zero", prefix)
		}
	case reflect.Float32, reflect.Float64:
		if v.Float() == 0 {
			t.Fatalf("auto-fill bug: %s is zero", prefix)
		}
	}
}

// collectStringFields recursively extracts all string values keyed by
// dotted path (e.g. "Learnings.Code[0].Path").
func collectStringFields(v reflect.Value, prefix string) map[string]string {
	out := make(map[string]string)
	if v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return out
		}
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.Struct:
		t := v.Type()
		for i := range t.NumField() {
			name := t.Field(i).Name
			p := name
			if prefix != "" {
				p = prefix + "." + name
			}
			for k, val := range collectStringFields(v.Field(i), p) {
				out[k] = val
			}
		}
	case reflect.Slice:
		for i := range v.Len() {
			p := fmt.Sprintf("%s[%d]", prefix, i)
			for k, val := range collectStringFields(v.Index(i), p) {
				out[k] = val
			}
		}
	case reflect.String:
		out[prefix] = v.String()
	}
	return out
}

// setAllStrings recursively sets every string field/element to token.
func setAllStrings(v reflect.Value, token string) {
	switch v.Kind() {
	case reflect.Ptr:
		if !v.IsNil() {
			setAllStrings(v.Elem(), token)
		}
	case reflect.Struct:
		for i := range v.NumField() {
			setAllStrings(v.Field(i), token)
		}
	case reflect.Slice:
		for i := range v.Len() {
			setAllStrings(v.Index(i), token)
		}
	case reflect.String:
		v.SetString(token)
	}
}

// jsonRoundTripSummary returns a deep copy of s via JSON round-trip.
func jsonRoundTripSummary(t *testing.T, s *Summary) *Summary {
	t.Helper()
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("failed to marshal Summary for deep copy: %v", err)
	}
	var cp Summary
	if err := json.Unmarshal(data, &cp); err != nil {
		t.Fatalf("failed to unmarshal Summary for deep copy: %v", err)
	}
	return &cp
}

// TestWriteCommitted_CLIVersionField verifies that buildinfo.Version is written
// to both the root CheckpointSummary and session-level CommittedMetadata.
func TestWriteCommitted_CLIVersionField(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()

	repo, err := git.PlainInit(tempDir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

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

	store := NewGitStore(repo)

	checkpointID := id.MustCheckpointID("b1c2d3e4f5a6")
	sessionID := "test-session-version"

	err = store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID: checkpointID,
		SessionID:    sessionID,
		Strategy:     "manual-commit",
		Agent:        agent.AgentTypeClaudeCode,
		Transcript:   []byte("test transcript"),
		AuthorName:   "Test Author",
		AuthorEmail:  "test@example.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() error = %v", err)
	}

	// Read the metadata branch
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

	checkpointTree, err := tree.Tree(checkpointID.Path())
	if err != nil {
		t.Fatalf("failed to find checkpoint tree at %s: %v", checkpointID.Path(), err)
	}

	// Verify root metadata.json (CheckpointSummary) has CLIVersion
	metadataFile, err := checkpointTree.File(paths.MetadataFileName)
	if err != nil {
		t.Fatalf("failed to find root metadata.json: %v", err)
	}

	content, err := metadataFile.Contents()
	if err != nil {
		t.Fatalf("failed to read root metadata.json: %v", err)
	}

	var summary CheckpointSummary
	if err := json.Unmarshal([]byte(content), &summary); err != nil {
		t.Fatalf("failed to parse root metadata.json: %v", err)
	}

	if summary.CLIVersion != buildinfo.Version {
		t.Errorf("CheckpointSummary.CLIVersion = %q, want %q", summary.CLIVersion, buildinfo.Version)
	}

	// Verify session-level metadata.json (CommittedMetadata) has CLIVersion
	sessionTree, err := checkpointTree.Tree("0")
	if err != nil {
		t.Fatalf("failed to get session tree: %v", err)
	}

	sessionMetadataFile, err := sessionTree.File(paths.MetadataFileName)
	if err != nil {
		t.Fatalf("failed to find session metadata.json: %v", err)
	}

	sessionContent, err := sessionMetadataFile.Contents()
	if err != nil {
		t.Fatalf("failed to read session metadata.json: %v", err)
	}

	var sessionMetadata CommittedMetadata
	if err := json.Unmarshal([]byte(sessionContent), &sessionMetadata); err != nil {
		t.Fatalf("failed to parse session metadata.json: %v", err)
	}

	if sessionMetadata.CLIVersion != buildinfo.Version {
		t.Errorf("CommittedMetadata.CLIVersion = %q, want %q", sessionMetadata.CLIVersion, buildinfo.Version)
	}
}
