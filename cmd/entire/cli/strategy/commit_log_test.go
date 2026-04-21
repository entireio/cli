package strategy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAppendCommitLogEntry_CreatesAndAppends(t *testing.T) {
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	t.Chdir(tmpDir)

	sessionID := "2026-04-16-test-session"
	metadataDir := filepath.Join(tmpDir, paths.SessionMetadataDirFromSessionID(sessionID))
	require.NoError(t, os.MkdirAll(metadataDir, 0o755))

	ctx := t.Context()

	entry1 := CommitLogEntry{
		Hash:         "abc123def456abc123def456abc123def456abc1",
		ShortHash:    "abc123d",
		Subject:      "Add login feature",
		Timestamp:    time.Date(2026, 4, 16, 12, 0, 0, 0, time.UTC),
		CheckpointID: "a3b2c4d5e6f7",
		SessionID:    sessionID,
	}
	entry2 := CommitLogEntry{
		Hash:         "def789abc012def789abc012def789abc012def7",
		ShortHash:    "def789a",
		Subject:      "Fix validation bug",
		Timestamp:    time.Date(2026, 4, 16, 13, 0, 0, 0, time.UTC),
		CheckpointID: "b4c3d5e6f7a8",
		SessionID:    sessionID,
	}

	require.NoError(t, appendCommitLogEntry(ctx, sessionID, entry1))
	require.NoError(t, appendCommitLogEntry(ctx, sessionID, entry2))

	entries, err := readCommitLog(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, entries, 2)

	assert.Equal(t, "abc123d", entries[0].ShortHash)
	assert.Equal(t, "Add login feature", entries[0].Subject)
	assert.Equal(t, "a3b2c4d5e6f7", entries[0].CheckpointID)
	assert.Equal(t, sessionID, entries[0].SessionID)

	assert.Equal(t, "def789a", entries[1].ShortHash)
	assert.Equal(t, "Fix validation bug", entries[1].Subject)
	assert.Equal(t, "b4c3d5e6f7a8", entries[1].CheckpointID)
}

func TestReadCommitLog_NonexistentFile(t *testing.T) {
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	t.Chdir(tmpDir)

	ctx := t.Context()
	entries, err := readCommitLog(ctx, "nonexistent-session")
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestCommitSubject(t *testing.T) {
	tests := []struct {
		name    string
		message string
		want    string
	}{
		{
			name:    "single line",
			message: "Add login feature",
			want:    "Add login feature",
		},
		{
			name:    "multi-line with body",
			message: "Add login feature\n\nThis adds OAuth support.",
			want:    "Add login feature",
		},
		{
			name:    "message with trailers",
			message: "Add login feature\n\nEntire-Checkpoint: a3b2c4d5e6f7",
			want:    "Add login feature",
		},
		{
			name:    "empty message",
			message: "",
			want:    "",
		},
		{
			name:    "whitespace padding",
			message: "  Add login feature  \n\nbody",
			want:    "Add login feature",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, commitSubject(tt.message))
		})
	}
}

// TestPostCommit_CommitLogInCheckpointTree verifies that after PostCommit
// condensation, the commits.jsonl sidecar file is present in the
// entire/checkpoints/v1 tree with the correct entry for this commit.
func TestPostCommit_CommitLogInCheckpointTree(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}
	sessionID := "test-commitlog-in-tree"

	setupSessionWithCheckpoint(t, s, repo, dir, sessionID)

	// Set phase to ACTIVE
	state, err := s.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	state.Phase = session.PhaseActive
	require.NoError(t, s.saveSessionState(context.Background(), state))

	cpID := string(testTrailerCheckpointID)
	commitWithCheckpointTrailer(t, repo, dir, cpID)

	err = s.PostCommit(context.Background())
	require.NoError(t, err)

	// Read the commits.jsonl from the v1 checkpoint tree
	v1Ref, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err, "entire/checkpoints/v1 should exist after condensation")

	v1Commit, err := repo.CommitObject(v1Ref.Hash())
	require.NoError(t, err)

	v1Tree, err := v1Commit.Tree()
	require.NoError(t, err)

	checkpointID := id.MustCheckpointID(cpID)
	commitLogPath := checkpointID.Path() + "/0/" + paths.CommitLogFileName

	commitLogFile, err := v1Tree.File(commitLogPath)
	require.NoError(t, err, "commits.jsonl should exist in checkpoint tree at %s", commitLogPath)

	content, err := commitLogFile.Contents()
	require.NoError(t, err)

	// Parse the JSONL content
	var entry CommitLogEntry
	require.NoError(t, json.Unmarshal([]byte(content), &entry))

	assert.Equal(t, cpID, entry.CheckpointID)
	assert.Equal(t, sessionID, entry.SessionID)
	assert.Equal(t, "test commit", entry.Subject)
	assert.NotEmpty(t, entry.Hash)
	assert.Len(t, entry.ShortHash, 7)
}

// TestPostCommit_CommitLogAccumulatesAcrossCheckpoints verifies that the second
// checkpoint's commits.jsonl contains entries for BOTH commits, not just the latest.
func TestPostCommit_CommitLogAccumulatesAcrossCheckpoints(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}
	sessionID := "test-commitlog-accumulate"

	setupSessionWithCheckpoint(t, s, repo, dir, sessionID)

	// Set phase to ACTIVE
	state, err := s.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	state.Phase = session.PhaseActive
	require.NoError(t, s.saveSessionState(context.Background(), state))

	// First commit + PostCommit
	cpID1 := string(testTrailerCheckpointID)
	commitWithCheckpointTrailer(t, repo, dir, cpID1)
	require.NoError(t, s.PostCommit(context.Background()))

	// Reload state for next checkpoint setup
	_, err = s.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)

	// Create new file for second checkpoint
	secondFile := filepath.Join(dir, "second.txt")
	require.NoError(t, os.WriteFile(secondFile, []byte("second file"), 0o644))

	metadataDir := ".entire/metadata/" + sessionID
	metadataDirAbs := filepath.Join(dir, metadataDir)
	require.NoError(t, os.WriteFile(
		filepath.Join(metadataDirAbs, paths.TranscriptFileName),
		[]byte(testTranscriptPromptResponse), 0o644))

	err = s.SaveStep(context.Background(), StepContext{
		SessionID:      sessionID,
		ModifiedFiles:  []string{},
		NewFiles:       []string{"second.txt"},
		DeletedFiles:   []string{},
		MetadataDir:    metadataDir,
		MetadataDirAbs: metadataDirAbs,
		CommitMessage:  "Checkpoint 2",
		AuthorName:     "Test",
		AuthorEmail:    "test@test.com",
	})
	require.NoError(t, err)

	// Second commit + PostCommit
	cpID2 := "b2c3d4e5f6a7"
	secondFilePath := filepath.Join(dir, "second.txt")
	require.NoError(t, os.WriteFile(secondFilePath, []byte("second file content"), 0o644))
	commitFilesWithTrailer(t, repo, dir, cpID2, "second.txt")
	require.NoError(t, s.PostCommit(context.Background()))

	// Read commits.jsonl from the SECOND checkpoint tree
	v1Ref, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err)

	v1Commit, err := repo.CommitObject(v1Ref.Hash())
	require.NoError(t, err)

	v1Tree, err := v1Commit.Tree()
	require.NoError(t, err)

	checkpointID2 := id.MustCheckpointID(cpID2)
	commitLogPath := checkpointID2.Path() + "/0/" + paths.CommitLogFileName

	commitLogFile, err := v1Tree.File(commitLogPath)
	require.NoError(t, err, "commits.jsonl should exist in second checkpoint tree")

	content, err := commitLogFile.Contents()
	require.NoError(t, err)

	// Parse all entries — should have 2 (one from each commit)
	var entries []CommitLogEntry
	for _, line := range splitNonEmptyLines(content) {
		var entry CommitLogEntry
		require.NoError(t, json.Unmarshal([]byte(line), &entry))
		entries = append(entries, entry)
	}

	require.Len(t, entries, 2, "second checkpoint should contain entries for both commits")
	assert.Equal(t, cpID1, entries[0].CheckpointID, "first entry should be from first commit")
	assert.Equal(t, cpID2, entries[1].CheckpointID, "second entry should be from second commit")
}

// splitNonEmptyLines splits a string by newlines and returns non-empty lines.
func splitNonEmptyLines(s string) []string {
	var result []string
	for _, line := range splitLines([]byte(s)) {
		if len(line) > 0 {
			result = append(result, line)
		}
	}
	return result
}
