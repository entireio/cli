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

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/redact"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/spf13/cobra"
)

const resumeTestStrategy = "manual-commit"

type recordingResumeAgent struct {
	sessionDir     string
	writtenSession *agent.AgentSession
}

var _ agent.Agent = (*recordingResumeAgent)(nil)

func (a *recordingResumeAgent) Name() types.AgentName                          { return "recording-resume" }
func (a *recordingResumeAgent) Type() types.AgentType                          { return "recording-resume" }
func (a *recordingResumeAgent) Description() string                            { return "recording resume agent" }
func (a *recordingResumeAgent) IsPreview() bool                                { return false }
func (a *recordingResumeAgent) DetectPresence(_ context.Context) (bool, error) { return true, nil }
func (a *recordingResumeAgent) ProtectedDirs() []string                        { return nil }
func (a *recordingResumeAgent) ReadTranscript(string) ([]byte, error)          { return nil, nil }
func (a *recordingResumeAgent) ChunkTranscript(_ context.Context, content []byte, _ int) ([][]byte, error) {
	return [][]byte{content}, nil
}
func (a *recordingResumeAgent) ReassembleTranscript(chunks [][]byte) ([]byte, error) {
	var out []byte
	for _, chunk := range chunks {
		out = append(out, chunk...)
	}
	return out, nil
}
func (a *recordingResumeAgent) GetSessionID(*agent.HookInput) string { return "" }
func (a *recordingResumeAgent) GetSessionDir(string) (string, error) { return a.sessionDir, nil }
func (a *recordingResumeAgent) ResolveSessionFile(sessionDir, sessionID string) string {
	return filepath.Join(sessionDir, sessionID+".jsonl")
}
func (a *recordingResumeAgent) ReadSession(*agent.HookInput) (*agent.AgentSession, error) {
	return nil, nil //nolint:nilnil // Not used by this test agent.
}
func (a *recordingResumeAgent) WriteSession(_ context.Context, session *agent.AgentSession) error {
	a.writtenSession = session
	return nil
}
func (a *recordingResumeAgent) FormatResumeCommand(sessionID string) string {
	return "recording resume " + sessionID
}

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
	if err := strategy.EnsureMetadataBranch(t.Context(), repo); err != nil {
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
	if err := strategy.EnsureMetadataBranch(t.Context(), repo); err != nil {
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

func writeCommittedResumeCheckpoint(t *testing.T, repo *git.Repository, checkpointID id.CheckpointID, sessionID string, createdAt time.Time) {
	t.Helper()

	writeCommittedResumeCheckpointWithAgent(t, repo, checkpointID, sessionID, createdAt, agent.AgentTypeClaudeCode)
}

func writeCommittedResumeCheckpointWithAgent(
	t *testing.T,
	repo *git.Repository,
	checkpointID id.CheckpointID,
	sessionID string,
	createdAt time.Time,
	agentType types.AgentType,
) {
	t.Helper()

	rawTranscript := []byte(`{"type":"user","message":{"content":[{"type":"text","text":"resume"}]}}` + "\n")
	writeCommittedResumeCheckpointWithTranscript(t, repo, checkpointID, sessionID, createdAt, agentType, rawTranscript)
}

func writeCommittedResumeCheckpointWithTranscript(
	t *testing.T,
	repo *git.Repository,
	checkpointID id.CheckpointID,
	sessionID string,
	createdAt time.Time,
	agentType types.AgentType,
	rawTranscript []byte,
) {
	t.Helper()

	if err := checkpoint.NewGitStore(repo).WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
		CheckpointID: checkpointID,
		SessionID:    sessionID,
		CreatedAt:    createdAt,
		Strategy:     resumeTestStrategy,
		Transcript:   redact.AlreadyRedacted(rawTranscript),
		Prompts:      []string{"resume prompt"},
		Agent:        agentType,
		AuthorName:   "Test",
		AuthorEmail:  "test@example.com",
	}); err != nil {
		t.Fatalf("WriteCommitted(%s): %v", sessionID, err)
	}
}

func mirrorMetadataBranchToV11Ref(t *testing.T, repo *git.Repository) {
	t.Helper()

	v1Ref, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	if err != nil {
		t.Fatalf("read v1 metadata branch: %v", err)
	}
	if err := repo.Storer.SetReference(plumbing.NewHashReference(plumbing.ReferenceName(paths.MetadataRefName), v1Ref.Hash())); err != nil {
		t.Fatalf("set v1.1 metadata ref: %v", err)
	}
}

func enableResumeV11(t *testing.T, repoRoot string) {
	t.Helper()

	t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", filepath.Join(repoRoot, "claude-projects"))

	if err := os.MkdirAll(filepath.Join(repoRoot, ".entire"), 0o755); err != nil {
		t.Fatalf("create settings dir: %v", err)
	}
	settingsPath := filepath.Join(repoRoot, ".entire", paths.SettingsFileName)
	settingsJSON := []byte(`{"enabled": true, "strategy_options": {"checkpoints_version": "1.1"}}`)
	if err := os.WriteFile(settingsPath, settingsJSON, 0o644); err != nil {
		t.Fatalf("write settings: %v", err)
	}
}

func commitResumeTrailer(t *testing.T, worktree *git.Worktree, repoRoot, fileName, commitMessage string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(repoRoot, fileName), []byte("feature content"), 0o644); err != nil {
		t.Fatalf("write feature file: %v", err)
	}
	if _, err := worktree.Add(fileName); err != nil {
		t.Fatalf("add feature file: %v", err)
	}
	if _, err := worktree.Commit(commitMessage, &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com"},
	}); err != nil {
		t.Fatalf("commit trailer: %v", err)
	}
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

	cpID1 := id.MustCheckpointID("aaa111bbb222")
	cpID2 := id.MustCheckpointID("ccc333ddd444")
	cpID3 := id.MustCheckpointID("eee555fff666")
	writeCommittedResumeCheckpoint(t, repo, cpID1, "session-oldest", t1)
	writeCommittedResumeCheckpoint(t, repo, cpID2, "session-middle", t2)
	writeCommittedResumeCheckpoint(t, repo, cpID3, "session-newest", t3)

	// Pass checkpoint IDs in reverse chronological order (newest first),
	// simulating git CLI squash merge trailer order.
	reverseOrderIDs := []id.CheckpointID{cpID3, cpID2, cpID1}
	reader := checkpoint.NewGitStore(repo)
	latest, err := resolveLatestCheckpoint(context.Background(), reader, reverseOrderIDs)
	if err != nil {
		t.Fatalf("resolveLatestCheckpoint() error = %v", err)
	}

	// Should return the newest checkpoint regardless of input order
	if latest.CheckpointID.String() != cpID3.String() {
		t.Errorf("resolveLatestCheckpoint() = %s, want newest %s", latest.CheckpointID, cpID3)
	}

	// Also verify with chronological order
	chronologicalIDs := []id.CheckpointID{cpID1, cpID2, cpID3}
	latest2, err := resolveLatestCheckpoint(context.Background(), reader, chronologicalIDs)
	if err != nil {
		t.Fatalf("resolveLatestCheckpoint() error = %v", err)
	}
	if latest2.CheckpointID.String() != cpID3.String() {
		t.Errorf("resolveLatestCheckpoint() = %s, want newest %s", latest2.CheckpointID, cpID3)
	}
}

func TestReadCheckpointInfoFromStoreUsesLatestSessionMetadata(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	repo, _, _ := setupResumeTestRepo(t, tmpDir, false)
	store := checkpoint.NewGitStore(repo)
	cpID := id.MustCheckpointID("112233445566")
	ctx := context.Background()
	oldCreatedAt := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
	latestCreatedAt := time.Date(2025, 1, 1, 11, 0, 0, 0, time.UTC)

	sessions := []struct {
		sessionID string
		createdAt time.Time
		agent     types.AgentType
	}{
		{
			sessionID: "session-old",
			createdAt: oldCreatedAt,
			agent:     agent.AgentTypeClaudeCode,
		},
		{
			sessionID: "session-latest",
			createdAt: latestCreatedAt,
			agent:     agent.AgentTypeCursor,
		},
	}
	for _, session := range sessions {
		if err := store.WriteCommitted(ctx, checkpoint.WriteCommittedOptions{
			CheckpointID: cpID,
			SessionID:    session.sessionID,
			CreatedAt:    session.createdAt,
			Strategy:     "manual-commit",
			Transcript:   redact.AlreadyRedacted([]byte(`{"type":"test"}` + "\n")),
			Prompts:      []string{"prompt for " + session.sessionID},
			AuthorName:   "Test",
			AuthorEmail:  "test@example.com",
			Agent:        session.agent,
		}); err != nil {
			t.Fatalf("WriteCommitted(%s) error = %v", session.sessionID, err)
		}
	}

	info, err := readCheckpointInfoFromStore(ctx, store, cpID)
	if err != nil {
		t.Fatalf("readCheckpointInfoFromStore() error = %v", err)
	}
	if info.SessionID != "session-latest" {
		t.Errorf("SessionID = %q, want latest session", info.SessionID)
	}
	if !info.CreatedAt.Equal(latestCreatedAt) {
		t.Errorf("CreatedAt = %s, want %s", info.CreatedAt, latestCreatedAt)
	}
	if info.Agent != agent.AgentTypeCursor {
		t.Errorf("Agent = %q, want %q", info.Agent, agent.AgentTypeCursor)
	}
	if len(info.SessionIDs) != 2 || info.SessionIDs[0] != "session-old" || info.SessionIDs[1] != "session-latest" {
		t.Errorf("SessionIDs = %#v, want [session-old session-latest]", info.SessionIDs)
	}
}

func TestResolveLatestCheckpointUsesV1Checkpoint(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	repo, _, _ := setupResumeTestRepo(t, tmpDir, false)

	targetID := id.MustCheckpointID("aa11bb22cc33")
	writeCommittedResumeCheckpoint(t, repo, targetID, "session-v1-target", time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC))

	store := checkpoint.NewGitStore(repo)

	latest, err := resolveLatestCheckpoint(context.Background(), store, []id.CheckpointID{targetID})
	if err != nil {
		t.Fatalf("resolveLatestCheckpoint() error = %v", err)
	}
	if latest.CheckpointID != targetID {
		t.Errorf("resolveLatestCheckpoint() = %s, want %s", latest.CheckpointID, targetID)
	}
}

func TestResumeFromCurrentBranch_UsesV11MetadataOnly(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	repo, worktree, _ := setupResumeTestRepo(t, tmpDir, false)
	enableResumeV11(t, tmpDir)

	cpID := id.MustCheckpointID("b1b2b3b4b5b6")
	sessionID := "session-v11-only"
	writeCommittedResumeCheckpoint(t, repo, cpID, sessionID, time.Date(2025, 1, 2, 10, 0, 0, 0, time.UTC))
	mirrorMetadataBranchToV11Ref(t, repo)

	if err := repo.Storer.RemoveReference(plumbing.NewBranchReferenceName(paths.MetadataBranchName)); err != nil {
		t.Fatalf("remove v1 metadata branch: %v", err)
	}

	commitResumeTrailer(t, worktree, tmpDir, "feature-v11-only.txt", "Add feature\n\nEntire-Checkpoint: "+cpID.String())

	var stdout, stderr bytes.Buffer
	if err := resumeFromCurrentBranch(context.Background(), &stdout, &stderr, "master", true); err != nil {
		t.Fatalf("resumeFromCurrentBranch error: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}

	combined := stdout.String() + stderr.String()
	if !strings.Contains(combined, sessionID) {
		t.Fatalf("resume did not read v1.1 metadata for session %q:\n%s", sessionID, combined)
	}
	if strings.Contains(combined, "found in commit but the entire/checkpoints/v1 branch is not available") {
		t.Fatalf("resume fell through to missing-v1 metadata message despite v1.1 metadata:\n%s", combined)
	}
}

func TestResolveLatestCheckpoint_V11DoesNotFallbackToStaleV1(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	repo, _, _ := setupResumeTestRepo(t, tmpDir, false)
	enableResumeV11(t, tmpDir)

	baseHash := readMetadataBranchHash(t, repo)
	customOnlyID := id.MustCheckpointID("c1c2c3c4c5c6")
	writeCommittedResumeCheckpoint(t, repo, customOnlyID, "session-custom-only", time.Date(2025, 1, 2, 10, 0, 0, 0, time.UTC))
	mirrorMetadataBranchToV11Ref(t, repo)

	v1RefName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	if err := repo.Storer.SetReference(plumbing.NewHashReference(v1RefName, baseHash)); err != nil {
		t.Fatalf("reset v1 metadata branch: %v", err)
	}

	staleV1ID := id.MustCheckpointID("d1d2d3d4d5d6")
	writeCommittedResumeCheckpoint(t, repo, staleV1ID, "session-stale-v1", time.Date(2025, 1, 3, 10, 0, 0, 0, time.UTC))

	store := checkpoint.NewCommittedReadStore(context.Background(), repo)
	store.SetBlobFetcher(FetchBlobsByHash)

	_, err := resolveLatestCheckpoint(context.Background(), store, []id.CheckpointID{staleV1ID})
	if err == nil {
		t.Fatal("resolveLatestCheckpoint found checkpoint from stale v1 even though v1.1 custom ref differs")
	}
}

func TestResumeFromCurrentBranch_V11DoesNotSeedFromV1(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	repo, worktree, _ := setupResumeTestRepo(t, tmpDir, false)
	enableResumeV11(t, tmpDir)

	cpID := id.MustCheckpointID("1122aabbccdd")
	sessionID := "session-v1-only"
	writeCommittedResumeCheckpoint(t, repo, cpID, sessionID, time.Date(2025, 1, 2, 10, 0, 0, 0, time.UTC))

	commitResumeTrailer(t, worktree, tmpDir, "feature-v1-only.txt", "Add feature\n\nEntire-Checkpoint: "+cpID.String())

	var stdout, stderr bytes.Buffer
	if err := resumeFromCurrentBranch(context.Background(), &stdout, &stderr, "master", true); err != nil {
		t.Fatalf("resumeFromCurrentBranch error: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}

	combined := stdout.String() + stderr.String()
	if strings.Contains(combined, sessionID) {
		t.Fatalf("resume used v1 metadata for session %q in v1.1 mode:\n%s", sessionID, combined)
	}
	if !strings.Contains(stderr.String(), paths.MetadataRefName) {
		t.Fatalf("resume did not report missing v1.1 metadata ref:\nstdout: %s\nstderr: %s", stdout.String(), stderr.String())
	}
	if strings.Contains(combined, "git fetch origin entire/checkpoints/v1") {
		t.Fatalf("resume suggested v1 fetch in v1.1 mode:\n%s", combined)
	}
	if !strings.Contains(stderr.String(), "entire explain "+cpID.String()) {
		t.Fatalf("resume did not suggest 'entire explain' for missing v1.1 metadata:\nstderr: %s", stderr.String())
	}
}

func TestResumeFromCurrentBranch_V11SquashUsesLatestMetadataTimestamp(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	repo, worktree, _ := setupResumeTestRepo(t, tmpDir, false)
	enableResumeV11(t, tmpDir)

	olderID := id.MustCheckpointID("e1e2e3e4e5e6")
	newerID := id.MustCheckpointID("f1f2f3f4f5f6")
	olderSession := "session-v11-older"
	newerSession := "session-v11-newer"
	writeCommittedResumeCheckpoint(t, repo, olderID, olderSession, time.Date(2025, 1, 2, 10, 0, 0, 0, time.UTC))
	writeCommittedResumeCheckpoint(t, repo, newerID, newerSession, time.Date(2025, 1, 2, 11, 0, 0, 0, time.UTC))
	mirrorMetadataBranchToV11Ref(t, repo)

	if err := repo.Storer.RemoveReference(plumbing.NewBranchReferenceName(paths.MetadataBranchName)); err != nil {
		t.Fatalf("remove v1 metadata branch: %v", err)
	}

	squashMsg := fmt.Sprintf("Squash feature\n\nEntire-Checkpoint: %s\n\nEntire-Checkpoint: %s\n", olderID, newerID)
	commitResumeTrailer(t, worktree, tmpDir, "feature-v11-squash.txt", squashMsg)

	var stdout, stderr bytes.Buffer
	if err := resumeFromCurrentBranch(context.Background(), &stdout, &stderr, "master", true); err != nil {
		t.Fatalf("resumeFromCurrentBranch error: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}

	combined := stdout.String() + stderr.String()
	if !strings.Contains(combined, "resuming from the latest") {
		t.Fatalf("resume did not report latest-checkpoint selection:\n%s", combined)
	}
	if !strings.Contains(combined, newerSession) {
		t.Fatalf("resume did not use newest v1.1 metadata session %q:\n%s", newerSession, combined)
	}
	if strings.Contains(combined, olderSession) {
		t.Fatalf("resume used older v1.1 metadata session %q:\n%s", olderSession, combined)
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

func TestResumeSingleSession_UsesV1Transcript(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	repo, _, _ := setupResumeTestRepo(t, tmpDir, false)

	if err := os.MkdirAll(filepath.Join(tmpDir, ".entire"), 0o755); err != nil {
		t.Fatalf("failed to create settings dir: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(tmpDir, ".entire", "settings.json"),
		[]byte(`{"enabled": true}`),
		0o644,
	); err != nil {
		t.Fatalf("failed to write settings: %v", err)
	}

	ctx := context.Background()
	cpID := id.MustCheckpointID("abc123abc123")
	sessionID := "resume-v1-fallback-session"
	raw := []byte(`{"type":"user","message":{"content":[{"type":"text","text":"resume v1 fallback"}]}}` + "\n")

	v1Store := checkpoint.NewGitStore(repo)
	if err := v1Store.WriteCommitted(ctx, checkpoint.WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    sessionID,
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted(raw),
		AuthorName:   "Test",
		AuthorEmail:  "test@example.com",
	}); err != nil {
		t.Fatalf("failed to write v1 checkpoint: %v", err)
	}

	ag := &recordingResumeAgent{sessionDir: filepath.Join(tmpDir, "sessions")}
	var stdout, stderr bytes.Buffer
	if err := resumeSingleSession(ctx, &stdout, &stderr, ag, sessionID, cpID, tmpDir, true); err != nil {
		t.Fatalf("resumeSingleSession() error = %v", err)
	}

	if ag.writtenSession == nil {
		t.Fatal("resumeSingleSession() did not restore a session")
	}
	if string(ag.writtenSession.NativeData) != string(raw) {
		t.Fatalf("restored transcript = %q, want %q", string(ag.writtenSession.NativeData), string(raw))
	}
	if strings.Contains(stdout.String(), "session log not available") {
		t.Fatalf("resumeSingleSession() reported missing log: %q", stdout.String())
	}
}

func TestResumeSingleSession_UsesV11Transcript(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	repo, _, _ := setupResumeTestRepo(t, tmpDir, false)
	enableResumeV11(t, tmpDir)

	ctx := context.Background()
	cpID := id.MustCheckpointID("bca123bca123")
	sessionID := "resume-v11-session"
	v11Raw := []byte(`{"type":"user","message":{"content":[{"type":"text","text":"resume from v11"}]}}` + "\n")
	staleV1Raw := []byte(`{"type":"user","message":{"content":[{"type":"text","text":"stale v1"}]}}` + "\n")

	writeCommittedResumeCheckpointWithTranscript(
		t,
		repo,
		cpID,
		sessionID,
		time.Date(2025, 1, 2, 10, 0, 0, 0, time.UTC),
		agent.AgentTypeClaudeCode,
		v11Raw,
	)
	mirrorMetadataBranchToV11Ref(t, repo)
	writeCommittedResumeCheckpointWithTranscript(
		t,
		repo,
		cpID,
		sessionID,
		time.Date(2025, 1, 3, 10, 0, 0, 0, time.UTC),
		agent.AgentTypeClaudeCode,
		staleV1Raw,
	)

	ag := &recordingResumeAgent{sessionDir: filepath.Join(tmpDir, "sessions")}
	var stdout, stderr bytes.Buffer
	if err := resumeSingleSession(ctx, &stdout, &stderr, ag, sessionID, cpID, tmpDir, true); err != nil {
		t.Fatalf("resumeSingleSession() error = %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}

	if ag.writtenSession == nil {
		t.Fatal("resumeSingleSession() did not restore a session")
	}
	if string(ag.writtenSession.NativeData) != string(v11Raw) {
		t.Fatalf("restored transcript = %q, want v1.1 transcript %q", string(ag.writtenSession.NativeData), string(v11Raw))
	}
}

func TestCheckRemoteMetadata_MetadataExistsOnRemote(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	repo, _, _ := setupResumeTestRepo(t, tmpDir, false)

	// Create checkpoint metadata on local entire/checkpoints/v1 branch
	sessionID := "2025-01-01-test-session"
	checkpointID := id.MustCheckpointID("abc123def456")
	writeCommittedResumeCheckpointWithAgent(
		t,
		repo,
		checkpointID,
		sessionID,
		time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		"",
	)

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
	err = checkRemoteMetadata(context.Background(), os.Stdout, os.Stderr, checkpointID, plumbing.NewBranchReferenceName(paths.MetadataBranchName))
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
	err := checkRemoteMetadata(
		context.Background(),
		os.Stdout,
		os.Stderr,
		id.MustCheckpointID("aaa111bbb222"),
		plumbing.NewBranchReferenceName(paths.MetadataBranchName),
	)
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
	writeCommittedResumeCheckpoint(
		t,
		repo,
		id.MustCheckpointID("abc123def456"),
		sessionID,
		time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
	)

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
	err = checkRemoteMetadata(
		context.Background(),
		os.Stdout,
		os.Stderr,
		id.MustCheckpointID("abcd12345678"),
		plumbing.NewBranchReferenceName(paths.MetadataBranchName),
	)
	if err != nil {
		t.Errorf("checkRemoteMetadata() returned error for missing checkpoint: %v", err)
	}
}

// makeLocalMetadataBranchStale advances origin/entire/checkpoints/v1 to the
// current local hash and rewinds the local ref back to baseHash, leaving the
// local metadata branch behind its remote-tracking counterpart. Returns the
// hash the remote-tracking ref now points at.
func makeLocalMetadataBranchStale(t *testing.T, repo *git.Repository, baseHash plumbing.Hash) plumbing.Hash {
	t.Helper()
	localRefName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	current, err := repo.Reference(localRefName, true)
	if err != nil {
		t.Fatalf("read advanced metadata branch ref: %v", err)
	}
	if current.Hash() == baseHash {
		t.Fatalf("makeLocalMetadataBranchStale: local ref must have advanced past baseHash before calling")
	}
	remoteRefName := plumbing.NewRemoteReferenceName("origin", paths.MetadataBranchName)
	if err := repo.Storer.SetReference(plumbing.NewHashReference(remoteRefName, current.Hash())); err != nil {
		t.Fatalf("set remote-tracking ref: %v", err)
	}
	if err := repo.Storer.SetReference(plumbing.NewHashReference(localRefName, baseHash)); err != nil {
		t.Fatalf("rewind local metadata branch: %v", err)
	}
	return current.Hash()
}

// readMetadataBranchHash returns the current hash of refs/heads/entire/checkpoints/v1.
func readMetadataBranchHash(t *testing.T, repo *git.Repository) plumbing.Hash {
	t.Helper()
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	if err != nil {
		t.Fatalf("read metadata branch ref: %v", err)
	}
	return ref.Hash()
}

// Before the fix, promoteRemoteTrackingMetadataBranch returned early whenever
// the local ref existed, even when it was behind the remote-tracking ref —
// so downstream metadata readers using the local ref missed checkpoints
// already fetched into refs/remotes/origin/...
func TestPromoteRemoteTrackingMetadataBranch_FastForwardsStaleLocal(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	repo, _, _ := setupResumeTestRepo(t, tmpDir, false)

	initialHash := readMetadataBranchHash(t, repo)
	_ = createCheckpointOnMetadataBranch(t, repo, "2025-01-01-test-session-uuid")
	descendantHash := makeLocalMetadataBranchStale(t, repo, initialHash)

	promoteRemoteTrackingMetadataBranch(context.Background(), repo)

	if got := readMetadataBranchHash(t, repo); got != descendantHash {
		t.Errorf("local should be fast-forwarded to remote-tracking ref: got %s, want %s", got, descendantHash)
	}
}

// End-to-end coverage for the same bug: when a fresh checkpoint has been
// pushed to origin but the user's local entire/checkpoints/v1 ref is behind,
// `entire resume` previously printed "session log not available" because the
// committed-checkpoint reader only falls back to origin/... when the local
// ref is missing entirely.
func TestResumeFromCurrentBranch_FastForwardsStaleLocalMetadata(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", filepath.Join(tmpDir, "claude-projects"))

	repo, w, _ := setupResumeTestRepo(t, tmpDir, false)
	initialHash := readMetadataBranchHash(t, repo)

	ctx := context.Background()
	cpID := id.MustCheckpointID("abc123def456")
	rawTranscript := []byte(`{"type":"user","message":{"content":[{"type":"text","text":"hi"}]}}` + "\n")

	// Agent must be set so RestoreLogsOnly can resolve a session-write target.
	v1Store := checkpoint.NewGitStore(repo)
	if err := v1Store.WriteCommitted(ctx, checkpoint.WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "2025-01-01-test-session-uuid",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted(rawTranscript),
		Agent:        agent.AgentTypeClaudeCode,
		AuthorName:   "Test",
		AuthorEmail:  "test@example.com",
	}); err != nil {
		t.Fatalf("WriteCommitted: %v", err)
	}

	_ = makeLocalMetadataBranchStale(t, repo, initialHash)

	featureFile := filepath.Join(tmpDir, "feature.txt")
	if err := os.WriteFile(featureFile, []byte("feature content"), 0o644); err != nil {
		t.Fatalf("write feature file: %v", err)
	}
	if _, err := w.Add("feature.txt"); err != nil {
		t.Fatalf("add feature file: %v", err)
	}
	if _, err := w.Commit("Add feature\n\nEntire-Checkpoint: "+cpID.String(), &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com"},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if err := resumeFromCurrentBranch(ctx, &stdout, &stderr, "master", true); err != nil {
		t.Fatalf("resumeFromCurrentBranch error: %v\nstdout: %s\nstderr: %s",
			err, stdout.String(), stderr.String())
	}

	combined := stdout.String() + stderr.String()
	if strings.Contains(combined, "session log not available") {
		t.Errorf("resume reported missing log even though origin has the checkpoint metadata:\n%s", combined)
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
	if !strings.Contains(got, "\nTo continue this session:\n") {
		t.Fatalf("displayRestoredSessions() missing continuation header, got: %q", got)
	}
	wantCommand := "  " + ag.FormatResumeCommand(session.SessionID) + "  # Implement auth\n"
	if !strings.Contains(got, wantCommand) {
		t.Fatalf("displayRestoredSessions() missing command %q in %q", wantCommand, got)
	}
}

func TestDisplayRestoredSessions_CodexShowsResumeCommand(t *testing.T) {
	t.Parallel()

	session := strategy.RestoredSession{
		SessionID: "019d6d29-8cf7-7fe3-adc9-8c3e4d9d5603",
		Agent:     "Codex",
		Prompt:    "Can you take a look at the go code",
		CreatedAt: time.Date(2026, time.April, 8, 18, 46, 0, 0, time.UTC),
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
	if !strings.Contains(got, "✓ Restored session 019d6d29-8cf7-7fe3-adc9-8c3e4d9d5603.\n") {
		t.Fatalf("displayRestoredSessions() missing session header, got: %q", got)
	}
	if !strings.Contains(got, "\nTo continue this session:\n") {
		t.Fatalf("displayRestoredSessions() missing continuation header, got: %q", got)
	}
	wantCommand := "  " + ag.FormatResumeCommand(session.SessionID) + "  # Can you take a look at the go code\n"
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
	if !strings.Contains(got, "\nTo continue this session:\n") {
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
	if !strings.Contains(got, "\n✓ Restored 2 sessions. To continue:\n") {
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

// Not parallel: uses t.Chdir()
func TestGetMetadataTree_SucceedsWithLocalBranch(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	repo, _, _ := setupResumeTestRepo(t, tmpDir, false)

	// Create checkpoint metadata on the local metadata branch
	sessionID := "2025-01-01-metadata-tree-test"
	_ = createCheckpointOnMetadataBranch(t, repo, sessionID)

	// No origin remote, no checkpoint_remote — only local branch
	tree, freshRepo, err := getMetadataTree(context.Background())
	if err != nil {
		t.Fatalf("getMetadataTree() error = %v", err)
	}
	if tree == nil {
		t.Fatal("getMetadataTree() returned nil tree")
	}
	if freshRepo == nil {
		t.Fatal("getMetadataTree() returned nil repo")
	}
}
