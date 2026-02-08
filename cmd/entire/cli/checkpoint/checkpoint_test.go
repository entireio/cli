package checkpoint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
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
// checkpoints when only origin/entire/sessions exists (simulating post-clone state).
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

	// Create entire/sessions branch on the remote with a checkpoint
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

	// Clone the repo (this clones main, but not entire/sessions by default)
	localDir := t.TempDir()
	localRepo, err := git.PlainClone(localDir, false, &git.CloneOptions{
		URL: remoteDir,
	})
	if err != nil {
		t.Fatalf("failed to clone repo: %v", err)
	}

	// Fetch the entire/sessions branch to origin/entire/sessions
	// (but don't create local branch - simulating post-clone state)
	refSpec := fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", paths.MetadataBranchName, paths.MetadataBranchName)
	err = localRepo.Fetch(&git.FetchOptions{
		RemoteName: "origin",
		RefSpecs:   []config.RefSpec{config.RefSpec(refSpec)},
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		t.Fatalf("failed to fetch entire/sessions: %v", err)
	}

	// Verify local branch doesn't exist
	_, err = localRepo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	if err == nil {
		t.Fatal("local entire/sessions branch should not exist")
	}

	// Verify remote-tracking branch exists
	_, err = localRepo.Reference(plumbing.NewRemoteReferenceName("origin", paths.MetadataBranchName), true)
	if err != nil {
		t.Fatalf("origin/entire/sessions should exist: %v", err)
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
// author of the commit that created the checkpoint on the entire/sessions branch.
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
// returns empty author when the entire/sessions branch doesn't exist.
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
