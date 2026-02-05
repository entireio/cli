package checkpoint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"entire.io/cli/cmd/entire/cli/agent"
	"entire.io/cli/cmd/entire/cli/checkpoint/id"
	"entire.io/cli/cmd/entire/cli/paths"
	"entire.io/cli/cmd/entire/cli/trailers"

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

	if metadata.Agent != agentType {
		t.Errorf("metadata.Agent = %q, want %q", metadata.Agent, agentType)
	}

	// Verify commit message contains Entire-Agent trailer
	if !strings.Contains(commit.Message, trailers.AgentTrailerKey+": "+string(agentType)) {
		t.Errorf("commit message should contain %s trailer with value %q, got:\n%s",
			trailers.AgentTrailerKey, agentType, commit.Message)
	}
}

// readCheckpointMetadata reads metadata.json from the metadata branch for a checkpoint.
func readCheckpointMetadata(t *testing.T, repo *git.Repository, checkpointID id.CheckpointID) CommittedMetadata {
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

	metadataPath := checkpointID.Path() + "/" + paths.MetadataFileName
	metadataFile, err := tree.File(metadataPath)
	if err != nil {
		t.Fatalf("failed to find metadata.json: %v", err)
	}

	content, err := metadataFile.Contents()
	if err != nil {
		t.Fatalf("failed to read metadata.json: %v", err)
	}

	var metadata CommittedMetadata
	if err := json.Unmarshal([]byte(content), &metadata); err != nil {
		t.Fatalf("failed to parse metadata.json: %v", err)
	}

	return metadata
}

func TestWriteCommitted_AgentsArray_SingleSession(t *testing.T) {
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo)
	checkpointID := id.MustCheckpointID("c1d2e3f4a5b6")

	err := store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID: checkpointID,
		SessionID:    "test-session-single",
		Strategy:     "auto-commit",
		Agent:        agent.AgentTypeGemini,
		Transcript:   []byte("test transcript"),
		AuthorName:   "Test Author",
		AuthorEmail:  "test@example.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() error = %v", err)
	}

	metadata := readCheckpointMetadata(t, repo, checkpointID)

	if metadata.Agent != agent.AgentTypeGemini {
		t.Errorf("metadata.Agent = %q, want %q", metadata.Agent, agent.AgentTypeGemini)
	}
	if len(metadata.Agents) != 0 {
		t.Errorf("metadata.Agents length = %d, want 0 (single-session should not have agents array)", len(metadata.Agents))
	}
}

func TestWriteCommitted_AgentsArray_MultiSession(t *testing.T) {
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo)
	checkpointID := id.MustCheckpointID("d2e3f4a5b6c7")

	// First session with Gemini CLI
	err := store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID: checkpointID,
		SessionID:    "session-1",
		Strategy:     "auto-commit",
		Agent:        agent.AgentTypeGemini,
		Transcript:   []byte("gemini transcript"),
		AuthorName:   "Test Author",
		AuthorEmail:  "test@example.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() first session error = %v", err)
	}

	// Second session with Claude Code (same checkpoint ID triggers merge)
	err = store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID: checkpointID,
		SessionID:    "session-2",
		Strategy:     "auto-commit",
		Agent:        agent.AgentTypeClaudeCode,
		Transcript:   []byte("claude transcript"),
		AuthorName:   "Test Author",
		AuthorEmail:  "test@example.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() second session error = %v", err)
	}

	metadata := readCheckpointMetadata(t, repo, checkpointID)

	// Verify Agent is the first agent (backwards compat)
	if metadata.Agent != agent.AgentTypeGemini {
		t.Errorf("metadata.Agent = %q, want %q (first agent for backwards compat)", metadata.Agent, agent.AgentTypeGemini)
	}

	// Verify Agents array contains both agents in order
	if len(metadata.Agents) != 2 {
		t.Errorf("metadata.Agents length = %d, want 2", len(metadata.Agents))
	}
	if len(metadata.Agents) >= 2 {
		if metadata.Agents[0] != agent.AgentTypeGemini {
			t.Errorf("metadata.Agents[0] = %q, want %q", metadata.Agents[0], agent.AgentTypeGemini)
		}
		if metadata.Agents[1] != agent.AgentTypeClaudeCode {
			t.Errorf("metadata.Agents[1] = %q, want %q", metadata.Agents[1], agent.AgentTypeClaudeCode)
		}
	}

	if metadata.SessionCount != 2 {
		t.Errorf("metadata.SessionCount = %d, want 2", metadata.SessionCount)
	}
}

func TestWriteCommitted_AgentsArray_Deduplication(t *testing.T) {
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo)
	checkpointID := id.MustCheckpointID("e3f4a5b6c7d8")

	// Two sessions with the same agent
	for i := 1; i <= 2; i++ {
		err := store.WriteCommitted(context.Background(), WriteCommittedOptions{
			CheckpointID: checkpointID,
			SessionID:    "session-" + string(rune('0'+i)),
			Strategy:     "auto-commit",
			Agent:        agent.AgentTypeClaudeCode,
			Transcript:   []byte("transcript"),
			AuthorName:   "Test Author",
			AuthorEmail:  "test@example.com",
		})
		if err != nil {
			t.Fatalf("WriteCommitted() session %d error = %v", i, err)
		}
	}

	metadata := readCheckpointMetadata(t, repo, checkpointID)

	// Should only have one agent (deduplicated)
	if len(metadata.Agents) != 1 {
		t.Errorf("metadata.Agents length = %d, want 1 (deduplicated)", len(metadata.Agents))
	}
	if len(metadata.Agents) > 0 && metadata.Agents[0] != agent.AgentTypeClaudeCode {
		t.Errorf("metadata.Agents[0] = %q, want %q", metadata.Agents[0], agent.AgentTypeClaudeCode)
	}

	// But session count should be 2
	if metadata.SessionCount != 2 {
		t.Errorf("metadata.SessionCount = %d, want 2", metadata.SessionCount)
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

// TestArchiveExistingSession_ChunkedTranscript verifies that when archiving
// a session with chunked transcripts, all chunk files are moved to the archive folder.
func TestArchiveExistingSession_ChunkedTranscript(t *testing.T) {
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo)

	basePath := "a1/b2c3d4e5f6/"

	// Simulate existing checkpoint with chunked transcript
	// Chunk 0 is the base file (full.jsonl), chunks 1+ have suffixes (.001, .002)
	entries := map[string]object.TreeEntry{
		basePath + paths.MetadataFileName:            {Name: basePath + paths.MetadataFileName, Hash: plumbing.NewHash("aaa")},
		basePath + paths.TranscriptFileName:          {Name: basePath + paths.TranscriptFileName, Hash: plumbing.NewHash("bbb")},          // chunk 0
		basePath + paths.TranscriptFileName + ".001": {Name: basePath + paths.TranscriptFileName + ".001", Hash: plumbing.NewHash("ccc")}, // chunk 1
		basePath + paths.TranscriptFileName + ".002": {Name: basePath + paths.TranscriptFileName + ".002", Hash: plumbing.NewHash("ddd")}, // chunk 2
		basePath + paths.PromptFileName:              {Name: basePath + paths.PromptFileName, Hash: plumbing.NewHash("eee")},
		basePath + paths.ContextFileName:             {Name: basePath + paths.ContextFileName, Hash: plumbing.NewHash("fff")},
		basePath + paths.ContentHashFileName:         {Name: basePath + paths.ContentHashFileName, Hash: plumbing.NewHash("ggg")},
	}

	existingMetadata := &CommittedMetadata{
		SessionCount: 1,
	}

	// Archive the existing session
	store.archiveExistingSession(basePath, existingMetadata, entries)

	archivePath := basePath + "1/"

	// Verify standard files were archived
	if _, ok := entries[archivePath+paths.MetadataFileName]; !ok {
		t.Error("metadata.json should be archived to 1/")
	}
	if _, ok := entries[archivePath+paths.TranscriptFileName]; !ok {
		t.Error("full.jsonl (chunk 0) should be archived to 1/")
	}
	if _, ok := entries[archivePath+paths.PromptFileName]; !ok {
		t.Error("prompt.txt should be archived to 1/")
	}

	// Verify chunk files were archived
	if _, ok := entries[archivePath+paths.TranscriptFileName+".001"]; !ok {
		t.Error("full.jsonl.001 (chunk 1) should be archived to 1/")
	}
	if _, ok := entries[archivePath+paths.TranscriptFileName+".002"]; !ok {
		t.Error("full.jsonl.002 (chunk 2) should be archived to 1/")
	}

	// Verify original locations are cleared
	if _, ok := entries[basePath+paths.TranscriptFileName]; ok {
		t.Error("original full.jsonl should be removed from base path")
	}
	if _, ok := entries[basePath+paths.TranscriptFileName+".001"]; ok {
		t.Error("original full.jsonl.001 should be removed from base path")
	}
	if _, ok := entries[basePath+paths.TranscriptFileName+".002"]; ok {
		t.Error("original full.jsonl.002 should be removed from base path")
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

	// Verify no summary initially
	metadata := readCheckpointMetadata(t, repo, checkpointID)
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

	// Verify summary was saved
	updatedMetadata := readCheckpointMetadata(t, repo, checkpointID)
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
