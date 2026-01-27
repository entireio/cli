package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"entire.io/cli/cmd/entire/cli/checkpoint/id"
	"entire.io/cli/cmd/entire/cli/paths"
	"entire.io/cli/cmd/entire/cli/sessionid"
	"entire.io/cli/cmd/entire/cli/strategy"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func TestFirstLine(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "single line",
			input:    "hello world",
			expected: "hello world",
		},
		{
			name:     "multiple lines",
			input:    "first line\nsecond line\nthird line",
			expected: "first line",
		},
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "only newline",
			input:    "\n",
			expected: "",
		},
		{
			name:     "newline at start",
			input:    "\nfirst line",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := firstLine(tt.input)
			if result != tt.expected {
				t.Errorf("firstLine(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

// setupResumeTestRepo creates a test repository with an initial commit and optional feature branch.
// Returns the repository, worktree, and commit hash. The caller should use t.Chdir(tmpDir).
func setupResumeTestRepo(t *testing.T, tmpDir string, createFeatureBranch bool) (*git.Repository, *git.Worktree, plumbing.Hash) {
	t.Helper()

	repo, err := git.PlainInit(tmpDir, false)
	if err != nil {
		t.Fatalf("Failed to init repo: %v", err)
	}

	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Failed to get worktree: %v", err)
	}

	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("test content"), 0o644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("Failed to add test file: %v", err)
	}

	commit, err := w.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test User",
			Email: "test@example.com",
		},
	})
	if err != nil {
		t.Fatalf("Failed to create initial commit: %v", err)
	}

	if createFeatureBranch {
		featureRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName("feature"), commit)
		if err := repo.Storer.SetReference(featureRef); err != nil {
			t.Fatalf("Failed to create feature branch: %v", err)
		}
	}

	// Ensure entire/sessions branch exists
	if err := strategy.EnsureMetadataBranch(repo); err != nil {
		t.Fatalf("Failed to create metadata branch: %v", err)
	}

	return repo, w, commit
}

func TestBranchExistsLocally(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	setupResumeTestRepo(t, tmpDir, true)

	t.Run("returns true for existing branch", func(t *testing.T) {
		exists, err := BranchExistsLocally("feature")
		if err != nil {
			t.Fatalf("BranchExistsLocally() error = %v", err)
		}
		if !exists {
			t.Error("BranchExistsLocally() = false, want true for existing branch")
		}
	})

	t.Run("returns false for nonexistent branch", func(t *testing.T) {
		exists, err := BranchExistsLocally("nonexistent")
		if err != nil {
			t.Fatalf("BranchExistsLocally() error = %v", err)
		}
		if exists {
			t.Error("BranchExistsLocally() = true, want false for nonexistent branch")
		}
	})
}

func TestCheckoutBranch(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	setupResumeTestRepo(t, tmpDir, true)

	t.Run("successfully checks out existing branch", func(t *testing.T) {
		err := CheckoutBranch("feature")
		if err != nil {
			t.Fatalf("CheckoutBranch() error = %v", err)
		}

		// Verify we're on the feature branch
		branch, err := GetCurrentBranch()
		if err != nil {
			t.Fatalf("GetCurrentBranch() error = %v", err)
		}
		if branch != "feature" {
			t.Errorf("After CheckoutBranch(), current branch = %q, want %q", branch, "feature")
		}
	})

	t.Run("returns error for nonexistent branch", func(t *testing.T) {
		err := CheckoutBranch("nonexistent")
		if err == nil {
			t.Error("CheckoutBranch() expected error for nonexistent branch, got nil")
		}
	})
}

func TestResumeFromCurrentBranch_NoCheckpoint(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize repo with initial commit (no checkpoint trailer)
	setupResumeTestRepo(t, tmpDir, false)

	// Run resumeFromCurrentBranch - should not error, just report no checkpoint found
	err := resumeFromCurrentBranch("master", false)
	if err != nil {
		t.Errorf("resumeFromCurrentBranch() returned error for commit without checkpoint: %v", err)
	}
}

func TestResumeFromCurrentBranch_WithEntireCheckpointTrailer(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Set up a fake Claude project directory for testing
	claudeDir := filepath.Join(tmpDir, "claude-projects")
	t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", claudeDir)

	_, _, _ = setupResumeTestRepo(t, tmpDir, false)

	// Set up the auto-commit strategy and create checkpoint metadata on entire/sessions branch
	strat := strategy.NewDualStrategy()
	if err := strat.EnsureSetup(); err != nil {
		t.Fatalf("Failed to ensure setup: %v", err)
	}

	// Create metadata directory with session log (required for SaveChanges)
	sessionID := "2025-12-10-xyz789"
	sessionLogContent := `{"type":"test"}`
	metadataDir := filepath.Join(tmpDir, paths.EntireMetadataDir, sessionID)
	if err := os.MkdirAll(metadataDir, 0o755); err != nil {
		t.Fatalf("Failed to create metadata dir: %v", err)
	}
	logFile := filepath.Join(metadataDir, paths.TranscriptFileName)
	if err := os.WriteFile(logFile, []byte(sessionLogContent), 0o644); err != nil {
		t.Fatalf("Failed to write log file: %v", err)
	}

	// Create a file change to commit
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("metadata content"), 0o644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	// Use SaveChanges to create a commit with checkpoint metadata on entire/sessions branch
	ctx := strategy.SaveContext{
		CommitMessage:  "test commit with checkpoint",
		MetadataDir:    filepath.Join(paths.EntireMetadataDir, sessionID),
		MetadataDirAbs: metadataDir,
		NewFiles:       []string{},
		ModifiedFiles:  []string{"test.txt"},
		DeletedFiles:   []string{},
		AuthorName:     "Test User",
		AuthorEmail:    "test@example.com",
	}
	if err := strat.SaveChanges(ctx); err != nil {
		t.Fatalf("Failed to save changes: %v", err)
	}

	// Run resumeFromCurrentBranch
	err := resumeFromCurrentBranch("master", false)
	if err != nil {
		t.Errorf("resumeFromCurrentBranch() returned error: %v", err)
	}

	// Verify that the session log was written to the Claude project directory
	claudeSessionID := sessionid.ModelSessionID(sessionID)
	expectedLogPath := filepath.Join(claudeDir, claudeSessionID+".jsonl")

	content, err := os.ReadFile(expectedLogPath)
	if err != nil {
		t.Fatalf("Failed to read session log from Claude project dir: %v (expected the log to be restored)", err)
	}

	if string(content) != sessionLogContent {
		t.Errorf("Session log content mismatch.\nGot: %s\nWant: %s", string(content), sessionLogContent)
	}
}

func TestRunResume_AlreadyOnBranch(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Set up a fake Claude project directory for testing
	claudeDir := filepath.Join(tmpDir, "claude-projects")
	t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", claudeDir)

	_, w, _ := setupResumeTestRepo(t, tmpDir, true)

	// Checkout feature branch
	if err := w.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName("feature"),
	}); err != nil {
		t.Fatalf("Failed to checkout feature branch: %v", err)
	}

	// Run resume on the branch we're already on - should skip checkout
	err := runResume("feature", false)
	// Should not error (no session, but shouldn't error)
	if err != nil {
		t.Errorf("runResume() returned error when already on branch: %v", err)
	}
}

func TestRunResume_BranchDoesNotExist(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	setupResumeTestRepo(t, tmpDir, false)

	// Run resume on a branch that doesn't exist
	err := runResume("nonexistent", false)
	if err == nil {
		t.Error("runResume() expected error for nonexistent branch, got nil")
	}
}

func TestRunResume_UncommittedChanges(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	setupResumeTestRepo(t, tmpDir, true)

	// Make uncommitted changes
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("uncommitted modification"), 0o644); err != nil {
		t.Fatalf("Failed to modify test file: %v", err)
	}

	// Run resume - should fail due to uncommitted changes
	err := runResume("feature", false)
	if err == nil {
		t.Error("runResume() expected error for uncommitted changes, got nil")
	}
}

// createCheckpointOnMetadataBranch creates a checkpoint on the entire/sessions branch.
// Returns the checkpoint ID.
func createCheckpointOnMetadataBranch(t *testing.T, repo *git.Repository, sessionID string) id.CheckpointID {
	t.Helper()

	checkpointID := id.MustCheckpointID("abc123def456") // Fixed ID for testing

	// Get existing metadata branch or create it
	if err := strategy.EnsureMetadataBranch(repo); err != nil {
		t.Fatalf("Failed to ensure metadata branch: %v", err)
	}

	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	ref, err := repo.Reference(refName, true)
	if err != nil {
		t.Fatalf("Failed to get metadata branch ref: %v", err)
	}

	parentCommit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		t.Fatalf("Failed to get parent commit: %v", err)
	}

	// Create metadata content
	metadataJSON := fmt.Sprintf(`{
  "checkpoint_id": %q,
  "session_id": %q,
  "created_at": "2025-01-01T00:00:00Z",
  "strategy": "auto-commit"
}`, checkpointID.String(), sessionID)

	// Create blob for metadata
	blob := repo.Storer.NewEncodedObject()
	blob.SetType(plumbing.BlobObject)
	writer, err := blob.Writer()
	if err != nil {
		t.Fatalf("Failed to create blob writer: %v", err)
	}
	if _, err := writer.Write([]byte(metadataJSON)); err != nil {
		t.Fatalf("Failed to write blob: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Failed to close writer: %v", err)
	}
	metadataBlobHash, err := repo.Storer.SetEncodedObject(blob)
	if err != nil {
		t.Fatalf("Failed to store blob: %v", err)
	}

	// Create session log blob
	logBlob := repo.Storer.NewEncodedObject()
	logBlob.SetType(plumbing.BlobObject)
	logWriter, err := logBlob.Writer()
	if err != nil {
		t.Fatalf("Failed to create log blob writer: %v", err)
	}
	if _, err := logWriter.Write([]byte(`{"type":"test"}`)); err != nil {
		t.Fatalf("Failed to write log blob: %v", err)
	}
	if err := logWriter.Close(); err != nil {
		t.Fatalf("Failed to close log writer: %v", err)
	}
	logBlobHash, err := repo.Storer.SetEncodedObject(logBlob)
	if err != nil {
		t.Fatalf("Failed to store log blob: %v", err)
	}

	// Build tree structure: <id[:2]>/<id[2:]>/metadata.json
	shardedPath := checkpointID.Path()
	checkpointIDStr := checkpointID.String()

	// Create checkpoint tree with metadata and transcript files
	// Entries must be sorted alphabetically
	checkpointTree := object.Tree{
		Entries: []object.TreeEntry{
			{Name: paths.TranscriptFileName, Mode: filemode.Regular, Hash: logBlobHash},
			{Name: paths.MetadataFileName, Mode: filemode.Regular, Hash: metadataBlobHash},
		},
	}
	checkpointTreeObj := repo.Storer.NewEncodedObject()
	if err := checkpointTree.Encode(checkpointTreeObj); err != nil {
		t.Fatalf("Failed to encode checkpoint tree: %v", err)
	}
	checkpointTreeHash, err := repo.Storer.SetEncodedObject(checkpointTreeObj)
	if err != nil {
		t.Fatalf("Failed to store checkpoint tree: %v", err)
	}

	// Create inner shard tree (id[2:])
	innerTree := object.Tree{
		Entries: []object.TreeEntry{
			{Name: checkpointIDStr[2:], Mode: filemode.Dir, Hash: checkpointTreeHash},
		},
	}
	innerTreeObj := repo.Storer.NewEncodedObject()
	if err := innerTree.Encode(innerTreeObj); err != nil {
		t.Fatalf("Failed to encode inner tree: %v", err)
	}
	innerTreeHash, err := repo.Storer.SetEncodedObject(innerTreeObj)
	if err != nil {
		t.Fatalf("Failed to store inner tree: %v", err)
	}

	// Get existing tree entries from parent
	parentTree, err := parentCommit.Tree()
	if err != nil {
		t.Fatalf("Failed to get parent tree: %v", err)
	}

	// Build new root tree with shard bucket
	var rootEntries []object.TreeEntry
	for _, entry := range parentTree.Entries {
		if entry.Name != shardedPath[:2] {
			rootEntries = append(rootEntries, entry)
		}
	}
	rootEntries = append(rootEntries, object.TreeEntry{
		Name: checkpointIDStr[:2],
		Mode: filemode.Dir,
		Hash: innerTreeHash,
	})

	rootTree := object.Tree{Entries: rootEntries}
	rootTreeObj := repo.Storer.NewEncodedObject()
	if err := rootTree.Encode(rootTreeObj); err != nil {
		t.Fatalf("Failed to encode root tree: %v", err)
	}
	rootTreeHash, err := repo.Storer.SetEncodedObject(rootTreeObj)
	if err != nil {
		t.Fatalf("Failed to store root tree: %v", err)
	}

	// Create commit on metadata branch
	commit := &object.Commit{
		Author: object.Signature{
			Name:  "Test",
			Email: "test@example.com",
			When:  parentCommit.Author.When,
		},
		Committer: object.Signature{
			Name:  "Test",
			Email: "test@example.com",
			When:  parentCommit.Author.When,
		},
		Message:      "Add checkpoint metadata",
		TreeHash:     rootTreeHash,
		ParentHashes: []plumbing.Hash{parentCommit.Hash},
	}
	commitObj := repo.Storer.NewEncodedObject()
	if err := commit.Encode(commitObj); err != nil {
		t.Fatalf("Failed to encode commit: %v", err)
	}
	commitHash, err := repo.Storer.SetEncodedObject(commitObj)
	if err != nil {
		t.Fatalf("Failed to store commit: %v", err)
	}

	// Update metadata branch ref
	newRef := plumbing.NewHashReference(refName, commitHash)
	if err := repo.Storer.SetReference(newRef); err != nil {
		t.Fatalf("Failed to update metadata branch: %v", err)
	}

	return checkpointID
}

func TestCheckRemoteMetadata_MetadataExistsOnRemote(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	repo, _, _ := setupResumeTestRepo(t, tmpDir, false)

	// Create checkpoint metadata on local entire/sessions branch
	sessionID := "2025-01-01-test-session"
	checkpointID := createCheckpointOnMetadataBranch(t, repo, sessionID)

	// Copy the local entire/sessions to origin/entire/sessions (simulate remote)
	localRef, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	if err != nil {
		t.Fatalf("Failed to get local metadata branch: %v", err)
	}
	remoteRef := plumbing.NewHashReference(
		plumbing.NewRemoteReferenceName("origin", paths.MetadataBranchName),
		localRef.Hash(),
	)
	if err := repo.Storer.SetReference(remoteRef); err != nil {
		t.Fatalf("Failed to create remote ref: %v", err)
	}

	// Delete local entire/sessions branch to simulate "not fetched yet"
	if err := repo.Storer.RemoveReference(plumbing.NewBranchReferenceName(paths.MetadataBranchName)); err != nil {
		t.Fatalf("Failed to remove local metadata branch: %v", err)
	}

	// Call checkRemoteMetadata - should find it on remote and attempt to fetch
	// In this test environment without a real origin remote, the fetch will fail
	// but it should return a SilentError (user-friendly error message already printed)
	err = checkRemoteMetadata(repo, checkpointID)
	if err == nil {
		t.Error("checkRemoteMetadata() should return SilentError when fetch fails")
	} else {
		var silentErr *SilentError
		if !errors.As(err, &silentErr) {
			t.Errorf("checkRemoteMetadata() should return SilentError, got: %v", err)
		}
	}
}

func TestCheckRemoteMetadata_NoRemoteMetadataBranch(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	repo, _, _ := setupResumeTestRepo(t, tmpDir, false)

	// Delete local entire/sessions branch
	if err := repo.Storer.RemoveReference(plumbing.NewBranchReferenceName(paths.MetadataBranchName)); err != nil {
		t.Fatalf("Failed to remove local metadata branch: %v", err)
	}

	// Don't create any remote ref - simulating no remote entire/sessions

	// Call checkRemoteMetadata - should handle gracefully (no remote branch)
	err := checkRemoteMetadata(repo, "nonexistent123")
	if err != nil {
		t.Errorf("checkRemoteMetadata() returned error when no remote branch: %v", err)
	}
}

func TestCheckRemoteMetadata_CheckpointNotOnRemote(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	repo, _, _ := setupResumeTestRepo(t, tmpDir, false)

	// Create checkpoint metadata on local entire/sessions branch
	sessionID := "2025-01-01-test-session"
	_ = createCheckpointOnMetadataBranch(t, repo, sessionID)

	// Copy the local entire/sessions to origin/entire/sessions (simulate remote)
	localRef, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	if err != nil {
		t.Fatalf("Failed to get local metadata branch: %v", err)
	}
	remoteRef := plumbing.NewHashReference(
		plumbing.NewRemoteReferenceName("origin", paths.MetadataBranchName),
		localRef.Hash(),
	)
	if err := repo.Storer.SetReference(remoteRef); err != nil {
		t.Fatalf("Failed to create remote ref: %v", err)
	}

	// Delete local entire/sessions branch
	if err := repo.Storer.RemoveReference(plumbing.NewBranchReferenceName(paths.MetadataBranchName)); err != nil {
		t.Fatalf("Failed to remove local metadata branch: %v", err)
	}

	// Call checkRemoteMetadata with a DIFFERENT checkpoint ID (not on remote)
	err = checkRemoteMetadata(repo, "abcd12345678")
	if err != nil {
		t.Errorf("checkRemoteMetadata() returned error for missing checkpoint: %v", err)
	}
}

func TestResumeFromCurrentBranch_FallsBackToRemote(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Set up a fake Claude project directory for testing
	claudeDir := filepath.Join(tmpDir, "claude-projects")
	t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", claudeDir)

	repo, w, _ := setupResumeTestRepo(t, tmpDir, false)

	// Create checkpoint metadata on local entire/sessions branch
	sessionID := "2025-01-01-test-session-uuid"
	checkpointID := createCheckpointOnMetadataBranch(t, repo, sessionID)

	// Copy the local entire/sessions to origin/entire/sessions (simulate remote)
	localRef, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	if err != nil {
		t.Fatalf("Failed to get local metadata branch: %v", err)
	}
	remoteRef := plumbing.NewHashReference(
		plumbing.NewRemoteReferenceName("origin", paths.MetadataBranchName),
		localRef.Hash(),
	)
	if err := repo.Storer.SetReference(remoteRef); err != nil {
		t.Fatalf("Failed to create remote ref: %v", err)
	}

	// Delete local entire/sessions branch to simulate "not fetched yet"
	if err := repo.Storer.RemoveReference(plumbing.NewBranchReferenceName(paths.MetadataBranchName)); err != nil {
		t.Fatalf("Failed to remove local metadata branch: %v", err)
	}

	// Create a commit with the checkpoint trailer
	testFile := filepath.Join(tmpDir, "feature.txt")
	if err := os.WriteFile(testFile, []byte("feature content"), 0o644); err != nil {
		t.Fatalf("Failed to write feature file: %v", err)
	}
	if _, err := w.Add("feature.txt"); err != nil {
		t.Fatalf("Failed to add feature file: %v", err)
	}

	commitMsg := "Add feature\n\nEntire-Checkpoint: " + checkpointID.String()
	_, err = w.Commit(commitMsg, &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test User",
			Email: "test@example.com",
		},
	})
	if err != nil {
		t.Fatalf("Failed to create commit with checkpoint: %v", err)
	}

	// Run resumeFromCurrentBranch - should fall back to remote and attempt fetch
	// In this test environment without a real origin remote, the fetch will fail
	// but it should return a SilentError (user-friendly error message already printed)
	err = resumeFromCurrentBranch("master", false)
	if err == nil {
		t.Error("resumeFromCurrentBranch() should return SilentError when fetch fails")
	} else {
		var silentErr *SilentError
		if !errors.As(err, &silentErr) {
			t.Errorf("resumeFromCurrentBranch() should return SilentError, got: %v", err)
		}
	}
}
