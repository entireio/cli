package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/spf13/cobra"
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

	// Ensure entire/checkpoints/v1 branch exists
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
		exists, err := BranchExistsLocally(context.Background(), "feature")
		if err != nil {
			t.Fatalf("BranchExistsLocally() error = %v", err)
		}
		if !exists {
			t.Error("BranchExistsLocally() = false, want true for existing branch")
		}
	})

	t.Run("returns false for nonexistent branch", func(t *testing.T) {
		exists, err := BranchExistsLocally(context.Background(), "nonexistent")
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
		err := CheckoutBranch(context.Background(), "feature")
		if err != nil {
			t.Fatalf("CheckoutBranch() error = %v", err)
		}

		// Verify we're on the feature branch
		branch, err := GetCurrentBranch(context.Background())
		if err != nil {
			t.Fatalf("GetCurrentBranch() error = %v", err)
		}
		if branch != "feature" {
			t.Errorf("After CheckoutBranch(), current branch = %q, want %q", branch, "feature")
		}
	})

	t.Run("returns error for nonexistent branch", func(t *testing.T) {
		err := CheckoutBranch(context.Background(), "nonexistent")
		if err == nil {
			t.Error("CheckoutBranch() expected error for nonexistent branch, got nil")
		}
	})

	t.Run("rejects ref starting with dash to prevent argument injection", func(t *testing.T) {
		// "git checkout -b evil" would create a new branch named "evil" instead
		// of failing, because git interprets "-b" as a flag.
		err := CheckoutBranch(context.Background(), "-b evil")
		if err == nil {
			t.Fatal("CheckoutBranch() should reject refs starting with '-', got nil")
		}
		if !strings.Contains(err.Error(), "invalid ref") {
			t.Errorf("CheckoutBranch() error = %q, want error containing 'invalid ref'", err.Error())
		}
	})
}

func TestPerformGitResetHard_RejectsArgumentInjection(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	setupResumeTestRepo(t, tmpDir, false)

	// "git reset --hard -q" would silently reset to HEAD in quiet mode instead
	// of failing, because git interprets "-q" as the --quiet flag.
	err := performGitResetHard(context.Background(), "-q")
	if err == nil {
		t.Fatal("performGitResetHard() should reject hashes starting with '-', got nil")
	}
	if !strings.Contains(err.Error(), "invalid commit hash") {
		t.Errorf("performGitResetHard() error = %q, want error containing 'invalid commit hash'", err.Error())
	}
}

func TestResumeFromCurrentBranch_NoCheckpoint(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize repo with initial commit (no checkpoint trailer)
	setupResumeTestRepo(t, tmpDir, false)

	// Run resumeFromCurrentBranch - should not error, just report no checkpoint found
	err := resumeFromCurrentBranch(context.Background(), io.Discard, io.Discard, "master", false)
	if err != nil {
		t.Errorf("resumeFromCurrentBranch() returned error for commit without checkpoint: %v", err)
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
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err := runResume(context.Background(), cmd, "feature", false)
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
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err := runResume(context.Background(), cmd, "nonexistent", false)
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
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err := runResume(context.Background(), cmd, "feature", false)
	if err == nil {
		t.Error("runResume() expected error for uncommitted changes, got nil")
	}
}

// createCheckpointOnMetadataBranch creates a checkpoint on the entire/checkpoints/v1 branch
// with a default checkpoint ID ("abc123def456") and default timestamp.
func createCheckpointOnMetadataBranch(t *testing.T, repo *git.Repository, sessionID string) id.CheckpointID {
	t.Helper()
	return createCheckpointOnMetadataBranchFull(t, repo, sessionID, id.MustCheckpointID("abc123def456"), time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
}

// createCheckpointOnMetadataBranchFull creates a checkpoint on the entire/checkpoints/v1 branch
// with a caller-specified checkpoint ID and timestamp.
func createCheckpointOnMetadataBranchFull(t *testing.T, repo *git.Repository, sessionID string, checkpointID id.CheckpointID, createdAt time.Time) id.CheckpointID {
	t.Helper()

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
  "created_at": %q
}`, checkpointID.String(), sessionID, createdAt.Format(time.RFC3339))

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

// TestResolveLatestCheckpoint verifies that resolveLatestCheckpoint returns the
// checkpoint with the newest CreatedAt, regardless of trailer order.
func TestResolveLatestCheckpoint(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	repo, _, _ := setupResumeTestRepo(t, tmpDir, false)

	// Create checkpoints with different timestamps.
	// Simulate git CLI squash merge order: newest first in the commit message.
	t1 := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC) // oldest
	t2 := time.Date(2025, 1, 1, 11, 0, 0, 0, time.UTC)
	t3 := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC) // newest

	cpID1 := createCheckpointOnMetadataBranchFull(t, repo, "session-oldest", id.MustCheckpointID("aaa111bbb222"), t1)
	cpID2 := createCheckpointOnMetadataBranchFull(t, repo, "session-middle", id.MustCheckpointID("ccc333ddd444"), t2)
	cpID3 := createCheckpointOnMetadataBranchFull(t, repo, "session-newest", id.MustCheckpointID("eee555fff666"), t3)

	// Pass checkpoint IDs in reverse chronological order (newest first),
	// simulating git CLI squash merge trailer order.
	reverseOrderIDs := []id.CheckpointID{cpID3, cpID2, cpID1}
	latest, tree, _, err := resolveLatestCheckpoint(context.Background(), reverseOrderIDs)
	if err != nil {
		t.Fatalf("resolveLatestCheckpoint() error = %v", err)
	}

	// Should return the newest checkpoint regardless of input order
	if latest.String() != cpID3.String() {
		t.Errorf("resolveLatestCheckpoint() = %s, want newest %s", latest, cpID3)
	}

	// Should return a non-nil tree for reuse
	if tree == nil {
		t.Error("resolveLatestCheckpoint() returned nil tree")
	}

	// Also verify with chronological order
	chronologicalIDs := []id.CheckpointID{cpID1, cpID2, cpID3}
	latest2, _, _, err := resolveLatestCheckpoint(context.Background(), chronologicalIDs)
	if err != nil {
		t.Fatalf("resolveLatestCheckpoint() error = %v", err)
	}
	if latest2.String() != cpID3.String() {
		t.Errorf("resolveLatestCheckpoint() = %s, want newest %s", latest2, cpID3)
	}
}

func TestFindCheckpointInHistory_MultipleCheckpoints(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	repo, w, _ := setupResumeTestRepo(t, tmpDir, false)

	// Create a commit that simulates a squash merge with multiple checkpoint trailers
	testFile := filepath.Join(tmpDir, "squash.txt")
	if err := os.WriteFile(testFile, []byte("squash content"), 0o644); err != nil {
		t.Fatalf("Failed to write file: %v", err)
	}
	if _, err := w.Add("squash.txt"); err != nil {
		t.Fatalf("Failed to add file: %v", err)
	}

	squashMsg := "Soph/test branch (#2)\n* random_letter script\n\nEntire-Checkpoint: 0aa0814d9839\n\n* random color\n\nEntire-Checkpoint: 33fb587b6fbb\n"
	_, err := w.Commit(squashMsg, &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test User",
			Email: "test@example.com",
		},
	})
	if err != nil {
		t.Fatalf("Failed to create squash commit: %v", err)
	}

	head, err := repo.Head()
	if err != nil {
		t.Fatalf("Failed to get HEAD: %v", err)
	}
	headCommit, err := repo.CommitObject(head.Hash())
	if err != nil {
		t.Fatalf("Failed to get HEAD commit: %v", err)
	}

	result := findCheckpointInHistory(headCommit, nil)

	if len(result.checkpointIDs) != 2 {
		t.Fatalf("findCheckpointInHistory() returned %d checkpoint IDs, want 2", len(result.checkpointIDs))
	}
	if result.checkpointIDs[0].String() != "0aa0814d9839" {
		t.Errorf("checkpointIDs[0] = %q, want %q", result.checkpointIDs[0].String(), "0aa0814d9839")
	}
	if result.checkpointIDs[1].String() != "33fb587b6fbb" {
		t.Errorf("checkpointIDs[1] = %q, want %q", result.checkpointIDs[1].String(), "33fb587b6fbb")
	}
	if result.newerCommitsExist {
		t.Error("newerCommitsExist should be false when HEAD has the checkpoints")
	}
}

func TestFindBranchCheckpoint_SquashMergeMultipleCheckpoints(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	repo, w, _ := setupResumeTestRepo(t, tmpDir, false)

	// Create two checkpoints on metadata branch with different session IDs
	sessionID1 := "2025-01-01-session-one"
	cpID1 := createCheckpointOnMetadataBranch(t, repo, sessionID1)

	sessionID2 := "2025-01-01-session-two"
	cpID2 := createCheckpointOnMetadataBranchFull(t, repo, sessionID2, id.MustCheckpointID("def456abc123"), time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

	// Create a squash merge commit with both checkpoint trailers
	testFile := filepath.Join(tmpDir, "squash.txt")
	if err := os.WriteFile(testFile, []byte("squash content"), 0o644); err != nil {
		t.Fatalf("Failed to write file: %v", err)
	}
	if _, err := w.Add("squash.txt"); err != nil {
		t.Fatalf("Failed to add file: %v", err)
	}

	squashMsg := fmt.Sprintf("Squash merge (#1)\n* first feature\n\nEntire-Checkpoint: %s\n\n* second feature\n\nEntire-Checkpoint: %s\n",
		cpID1.String(), cpID2.String())
	_, err := w.Commit(squashMsg, &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test User",
			Email: "test@example.com",
		},
	})
	if err != nil {
		t.Fatalf("Failed to create squash commit: %v", err)
	}

	// Verify findBranchCheckpoints returns both checkpoint IDs
	result, err := findBranchCheckpoints(repo, "master")
	if err != nil {
		t.Fatalf("findBranchCheckpoints() error = %v", err)
	}
	if len(result.checkpointIDs) != 2 {
		t.Fatalf("findBranchCheckpoints() returned %d checkpoint IDs, want 2", len(result.checkpointIDs))
	}
	if result.checkpointIDs[0].String() != cpID1.String() {
		t.Errorf("checkpointIDs[0] = %q, want %q", result.checkpointIDs[0].String(), cpID1.String())
	}
	if result.checkpointIDs[1].String() != cpID2.String() {
		t.Errorf("checkpointIDs[1] = %q, want %q", result.checkpointIDs[1].String(), cpID2.String())
	}
}

func TestCheckRemoteMetadata_MetadataExistsOnRemote(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	repo, _, _ := setupResumeTestRepo(t, tmpDir, false)

	// Create checkpoint metadata on local entire/checkpoints/v1 branch
	sessionID := "2025-01-01-test-session"
	checkpointID := createCheckpointOnMetadataBranch(t, repo, sessionID)

	// Copy the local entire/checkpoints/v1 to origin/entire/checkpoints/v1 (simulate remote)
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

	// Delete local entire/checkpoints/v1 branch to simulate "not fetched yet"
	if err := repo.Storer.RemoveReference(plumbing.NewBranchReferenceName(paths.MetadataBranchName)); err != nil {
		t.Fatalf("Failed to remove local metadata branch: %v", err)
	}

	// Call checkRemoteMetadata - should find metadata on the remote tree and
	// attempt to resume, but fail because the test checkpoint has no agent field.
	err = checkRemoteMetadata(context.Background(), os.Stdout, os.Stderr, checkpointID)
	if err == nil {
		t.Error("checkRemoteMetadata() should return error when agent is missing from metadata")
	} else if !strings.Contains(err.Error(), "failed to resolve agent") {
		t.Errorf("checkRemoteMetadata() expected agent resolution error, got: %v", err)
	}
}

func TestCheckRemoteMetadata_NoRemoteMetadataBranch(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	repo, _, _ := setupResumeTestRepo(t, tmpDir, false)

	// Delete local entire/checkpoints/v1 branch
	if err := repo.Storer.RemoveReference(plumbing.NewBranchReferenceName(paths.MetadataBranchName)); err != nil {
		t.Fatalf("Failed to remove local metadata branch: %v", err)
	}

	// Don't create any remote ref - simulating no remote entire/checkpoints/v1

	// Call checkRemoteMetadata - should handle gracefully (no remote branch)
	err := checkRemoteMetadata(context.Background(), os.Stdout, os.Stderr, id.MustCheckpointID("aaa111bbb222"))
	if err != nil {
		t.Errorf("checkRemoteMetadata() returned error when no remote branch: %v", err)
	}
}

func TestCheckRemoteMetadata_CheckpointNotOnRemote(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	repo, _, _ := setupResumeTestRepo(t, tmpDir, false)

	// Create checkpoint metadata on local entire/checkpoints/v1 branch
	sessionID := "2025-01-01-test-session"
	_ = createCheckpointOnMetadataBranch(t, repo, sessionID)

	// Copy the local entire/checkpoints/v1 to origin/entire/checkpoints/v1 (simulate remote)
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

	// Delete local entire/checkpoints/v1 branch
	if err := repo.Storer.RemoveReference(plumbing.NewBranchReferenceName(paths.MetadataBranchName)); err != nil {
		t.Fatalf("Failed to remove local metadata branch: %v", err)
	}

	// Call checkRemoteMetadata with a DIFFERENT checkpoint ID (not on remote)
	err = checkRemoteMetadata(context.Background(), os.Stdout, os.Stderr, id.MustCheckpointID("abcd12345678"))
	if err != nil {
		t.Errorf("checkRemoteMetadata() returned error for missing checkpoint: %v", err)
	}
}

func TestResumeFromCurrentBranch_NoMetadataAvailable(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Set up a fake Claude project directory for testing
	claudeDir := filepath.Join(tmpDir, "claude-projects")
	t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", claudeDir)

	repo, w, _ := setupResumeTestRepo(t, tmpDir, false)

	// Create checkpoint metadata on local entire/checkpoints/v1 branch
	sessionID := "2025-01-01-test-session-uuid"
	checkpointID := createCheckpointOnMetadataBranch(t, repo, sessionID)

	// Delete local entire/checkpoints/v1 branch to simulate "not fetched yet".
	// Don't create a remote ref — getMetadataTree falls back to
	// GetRemoteMetadataBranchTree which reads refs/remotes/origin/... directly,
	// so a remote ref would let it succeed without a real fetch.
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
	var err error
	_, err = w.Commit(commitMsg, &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test User",
			Email: "test@example.com",
		},
	})
	if err != nil {
		t.Fatalf("Failed to create commit with checkpoint: %v", err)
	}

	// Run resumeFromCurrentBranch - no local or remote metadata branch exists,
	// so checkRemoteMetadata prints an informational message and returns nil.
	err = resumeFromCurrentBranch(context.Background(), io.Discard, io.Discard, "master", false)
	if err != nil {
		t.Errorf("resumeFromCurrentBranch() returned unexpected error: %v", err)
	}
}

func TestDisplayRestoredSessions_SingleSessionOutput(t *testing.T) {
	t.Parallel()

	session := strategy.RestoredSession{
		SessionID: "2026-02-02-resume-output",
		Agent:     "Claude Code",
		Prompt:    "Implement auth",
		CreatedAt: time.Date(2026, time.February, 2, 12, 0, 0, 0, time.UTC),
	}

	ag, err := strategy.ResolveAgentForRewind(session.Agent)
	if err != nil {
		t.Fatalf("ResolveAgentForRewind() error = %v", err)
	}

	var output bytes.Buffer
	if err := displayRestoredSessions(&output, []strategy.RestoredSession{session}); err != nil {
		t.Fatalf("displayRestoredSessions() error = %v", err)
	}

	got := output.String()
	if !strings.Contains(got, "✓ Restored session 2026-02-02-resume-output.\n") {
		t.Fatalf("displayRestoredSessions() missing session header, got: %q", got)
	}
	if !strings.Contains(got, "\nTo continue this session, run:\n") {
		t.Fatalf("displayRestoredSessions() missing continuation header, got: %q", got)
	}
	wantCommand := "  " + ag.FormatResumeCommand(session.SessionID) + "  # Implement auth\n"
	if !strings.Contains(got, wantCommand) {
		t.Fatalf("displayRestoredSessions() missing command %q in %q", wantCommand, got)
	}
}

func TestPrintMultiSessionResumeCommands_SingleSessionHasCheckmark(t *testing.T) {
	t.Parallel()

	sessions := []strategy.RestoredSession{
		{
			SessionID: "2026-02-02-rewind-single",
			Agent:     "Claude Code",
			Prompt:    "Fix the bug",
		},
	}

	ag, err := strategy.ResolveAgentForRewind("Claude Code")
	if err != nil {
		t.Fatalf("ResolveAgentForRewind() error = %v", err)
	}

	var output bytes.Buffer
	var errOutput bytes.Buffer
	printMultiSessionResumeCommands(&output, &errOutput, sessions)

	got := output.String()
	if !strings.Contains(got, "✓ Restored session 2026-02-02-rewind-single.\n") {
		t.Fatalf("printMultiSessionResumeCommands() single session missing ✓ header, got: %q", got)
	}
	if !strings.Contains(got, "\nTo continue this session, run:\n") {
		t.Fatalf("printMultiSessionResumeCommands() missing continuation line, got: %q", got)
	}
	wantCommand := "  " + ag.FormatResumeCommand("2026-02-02-rewind-single") + "  # Fix the bug\n"
	if !strings.Contains(got, wantCommand) {
		t.Fatalf("printMultiSessionResumeCommands() missing command %q in %q", wantCommand, got)
	}
	if errOutput.Len() != 0 {
		t.Fatalf("printMultiSessionResumeCommands() unexpected stderr: %q", errOutput.String())
	}
}

func TestPrintMultiSessionResumeCommands_OutputMatchesResumeStyle(t *testing.T) {
	t.Parallel()

	sessions := []strategy.RestoredSession{
		{
			SessionID: "2026-02-02-rewind-old",
			Agent:     "Claude Code",
			Prompt:    "Old prompt",
		},
		{
			SessionID: "2026-02-02-rewind-new",
			Agent:     "Claude Code",
			Prompt:    "Most recent prompt",
		},
	}

	ag, err := strategy.ResolveAgentForRewind("Claude Code")
	if err != nil {
		t.Fatalf("ResolveAgentForRewind() error = %v", err)
	}

	var output bytes.Buffer
	var errOutput bytes.Buffer
	printMultiSessionResumeCommands(&output, &errOutput, sessions)

	got := output.String()
	if !strings.Contains(got, "\n✓ Restored 2 sessions. To continue, run:\n") {
		t.Fatalf("printMultiSessionResumeCommands() missing multi-session header, got: %q", got)
	}
	oldCommand := "  " + ag.FormatResumeCommand("2026-02-02-rewind-old") + "  # Old prompt\n"
	if !strings.Contains(got, oldCommand) {
		t.Fatalf("printMultiSessionResumeCommands() missing older command %q in %q", oldCommand, got)
	}
	newCommand := "  " + ag.FormatResumeCommand("2026-02-02-rewind-new") + "  # Most recent prompt (most recent)\n"
	if !strings.Contains(got, newCommand) {
		t.Fatalf("printMultiSessionResumeCommands() missing latest command %q in %q", newCommand, got)
	}
	if errOutput.Len() != 0 {
		t.Fatalf("printMultiSessionResumeCommands() unexpected stderr: %q", errOutput.String())
	}
}
