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

func TestCommittedMetadata_SummaryFields(t *testing.T) {
	meta := CommittedMetadata{
		CheckpointID:   "abc123def456",
		SessionID:      "2026-01-19-test",
		Intent:         "Add user authentication",
		Outcome:        "Implemented JWT-based auth with refresh tokens",
		Learnings:      []string{"go-jwt library requires explicit algorithm", "refresh tokens need separate storage"},
		FrictionPoints: []string{"Initial approach with sessions didn't scale"},
	}

	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	var decoded CommittedMetadata
	err = json.Unmarshal(data, &decoded)
	if err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	if decoded.Intent != "Add user authentication" {
		t.Errorf("Intent = %q, want %q", decoded.Intent, "Add user authentication")
	}
	if decoded.Outcome != "Implemented JWT-based auth with refresh tokens" {
		t.Errorf("Outcome = %q, want %q", decoded.Outcome, "Implemented JWT-based auth with refresh tokens")
	}
	if len(decoded.Learnings) != 2 {
		t.Errorf("len(Learnings) = %d, want 2", len(decoded.Learnings))
	}
	if len(decoded.FrictionPoints) != 1 {
		t.Errorf("len(FrictionPoints) = %d, want 1", len(decoded.FrictionPoints))
	}
}

func TestUpdateSummary(t *testing.T) {
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
	checkpointID := "a1b2c3d4e5f6"
	sessionID := "test-session-123"

	// First, create a checkpoint without summary fields
	err = store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID: checkpointID,
		SessionID:    sessionID,
		Strategy:     "manual-commit",
		Transcript:   []byte("test transcript"),
		AuthorName:   "Test Author",
		AuthorEmail:  "test@example.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() error = %v", err)
	}

	// Verify checkpoint has no summary
	result, err := store.ReadCommitted(context.Background(), checkpointID)
	if err != nil {
		t.Fatalf("ReadCommitted() error = %v", err)
	}
	if result.Metadata.Intent != "" {
		t.Errorf("Initial Intent should be empty, got %q", result.Metadata.Intent)
	}

	// Update the summary
	err = store.UpdateSummary(context.Background(), UpdateSummaryOptions{
		CheckpointID:   checkpointID,
		Intent:         "Add a feature",
		Outcome:        "Feature was added successfully",
		Learnings:      []string{"Learned something new"},
		FrictionPoints: []string{"Had to debug issue"},
		AuthorName:     "Test Author",
		AuthorEmail:    "test@example.com",
	})
	if err != nil {
		t.Fatalf("UpdateSummary() error = %v", err)
	}

	// Verify summary was updated
	result, err = store.ReadCommitted(context.Background(), checkpointID)
	if err != nil {
		t.Fatalf("ReadCommitted() after update error = %v", err)
	}
	if result.Metadata.Intent != "Add a feature" {
		t.Errorf("Intent = %q, want %q", result.Metadata.Intent, "Add a feature")
	}
	if result.Metadata.Outcome != "Feature was added successfully" {
		t.Errorf("Outcome = %q, want %q", result.Metadata.Outcome, "Feature was added successfully")
	}
	if len(result.Metadata.Learnings) != 1 || result.Metadata.Learnings[0] != "Learned something new" {
		t.Errorf("Learnings = %v, want [Learned something new]", result.Metadata.Learnings)
	}
	if len(result.Metadata.FrictionPoints) != 1 || result.Metadata.FrictionPoints[0] != "Had to debug issue" {
		t.Errorf("FrictionPoints = %v, want [Had to debug issue]", result.Metadata.FrictionPoints)
	}

	// Verify other metadata fields are preserved
	if result.Metadata.SessionID != sessionID {
		t.Errorf("SessionID = %q, want %q (should be preserved)", result.Metadata.SessionID, sessionID)
	}
	if result.Metadata.Strategy != "manual-commit" {
		t.Errorf("Strategy = %q, want %q (should be preserved)", result.Metadata.Strategy, "manual-commit")
	}
}

func TestUpdateSummary_CheckpointNotFound(t *testing.T) {
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

	store := NewGitStore(repo)

	// Try to update a non-existent checkpoint
	err = store.UpdateSummary(context.Background(), UpdateSummaryOptions{
		CheckpointID: "nonexistent1",
		Intent:       "Test",
		AuthorName:   "Test Author",
		AuthorEmail:  "test@example.com",
	})

	// Should return an error (either sessions branch doesn't exist or checkpoint not found)
	if err == nil {
		t.Error("UpdateSummary() for non-existent checkpoint should return error")
	}
}
