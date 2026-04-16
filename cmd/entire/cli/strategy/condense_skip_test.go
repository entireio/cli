package strategy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/session"

	"github.com/go-git/go-git/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests in this file use t.Chdir (process-global state),
// so they cannot use t.Parallel().

func TestCondenseSession_SkipsWhenNoTranscriptAndNoFiles(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}
	checkpointID := id.MustCheckpointID("a1b2c3d4e5f6")

	// Create a session with no transcript path and no files touched
	state := &SessionState{
		SessionID:  "test-skip-session",
		AgentType:  "Codex",
		BaseCommit: getHeadHash(t, repo),
		Phase:      session.PhaseActive,
	}

	result, err := s.CondenseSession(context.Background(), repo, checkpointID, state, nil)
	require.NoError(t, err)
	assert.True(t, result.Skipped, "should skip when no transcript and no files")
	assert.Equal(t, checkpointID, result.CheckpointID)
	assert.Equal(t, "test-skip-session", result.SessionID)
}

// Regression test: empty sessions must be skipped even when committedFiles is
// non-empty. Before the fix, filterFilesTouched's fallback assigned all committed
// files to sessions with empty FilesTouched, which made the skip gate think the
// session had meaningful data (checkpoint 12a9a7e2ffbe).
func TestCondenseSession_SkipsEmptySessionEvenWithCommittedFiles(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}
	checkpointID := id.MustCheckpointID("b2c3d4e5f6a1")

	// Session with no transcript path, no shadow branch, no FilesTouched (empty Codex companion)
	state := &SessionState{
		SessionID:  "empty-codex-with-committed-files",
		AgentType:  "Codex",
		BaseCommit: getHeadHash(t, repo),
		Phase:      session.PhaseActive,
	}

	// Non-nil committedFiles simulates PostCommit passing the committed file set.
	// Before the fix, filterFilesTouched's fallback would assign these to the
	// session, defeating the skip gate.
	committedFiles := map[string]struct{}{
		"cmd/entire/cli/strategy/manual_commit_condensation.go": {},
	}

	result, err := s.CondenseSession(context.Background(), repo, checkpointID, state, committedFiles)
	require.NoError(t, err)
	assert.True(t, result.Skipped, "should skip empty session even when committedFiles is non-empty")
}

func TestCondenseSession_DoesNotSkipWhenFilesTouchedButNoTranscript(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}
	checkpointID := id.MustCheckpointID("c3d4e5f6a1b2")

	// Modify and commit a file so committedFiles has content
	testFile := filepath.Join(dir, "test.txt")
	require.NoError(t, os.WriteFile(testFile, []byte("modified"), 0o644))
	wt, err := repo.Worktree()
	require.NoError(t, err)
	_, err = wt.Add("test.txt")
	require.NoError(t, err)
	commitHash, err := wt.Commit("modify test.txt", &git.CommitOptions{})
	require.NoError(t, err)

	state := &SessionState{
		SessionID:    "test-files-no-transcript",
		AgentType:    "Claude Code",
		BaseCommit:   commitHash.String(),
		Phase:        session.PhaseActive,
		FilesTouched: []string{"test.txt"},
	}

	committedFiles := map[string]struct{}{"test.txt": {}}
	result, err := s.CondenseSession(context.Background(), repo, checkpointID, state, committedFiles)
	require.NoError(t, err)
	assert.False(t, result.Skipped, "should not skip when files are touched even without transcript")
}

func TestCondenseSessionByID_SkippedPreservesState(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	s := &ManualCommitStrategy{}
	sessionID := "test-byid-skip"

	// Create a metadata dir with NO transcript (empty dir)
	metadataDir := ".entire/metadata/" + sessionID
	metadataDirAbs := filepath.Join(dir, metadataDir)
	require.NoError(t, os.MkdirAll(metadataDirAbs, 0o755))

	// Write a dummy file so SaveStep has something to commit to the shadow branch
	require.NoError(t, os.WriteFile(filepath.Join(dir, "dummy.txt"), []byte("x"), 0o644))

	// SaveStep creates the shadow branch (so CondenseSessionByID gets past the
	// hasShadowBranch check), but there's no transcript in the metadata dir.
	err := s.SaveStep(context.Background(), StepContext{
		SessionID:      sessionID,
		ModifiedFiles:  []string{},
		NewFiles:       []string{"dummy.txt"},
		DeletedFiles:   []string{},
		MetadataDir:    metadataDir,
		MetadataDirAbs: metadataDirAbs,
		CommitMessage:  "Checkpoint without transcript",
		AuthorName:     "Test",
		AuthorEmail:    "test@test.com",
	})
	require.NoError(t, err)

	// Clear FilesTouched so the skip gate fires (no transcript + no files = skip)
	state, err := s.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	state.Phase = session.PhaseIdle
	state.FilesTouched = nil
	require.NoError(t, s.saveSessionState(context.Background(), state))
	originalStepCount := state.StepCount

	// CondenseSessionByID should return nil (no error) and mark fully condensed
	err = s.CondenseSessionByID(context.Background(), sessionID)
	require.NoError(t, err)

	// State should be marked FullyCondensed so doctor doesn't retry
	state, err = s.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, state, "session state should still exist after skipped condensation")
	assert.True(t, state.FullyCondensed, "should be marked FullyCondensed when condensation is skipped")
	assert.Equal(t, originalStepCount, state.StepCount, "StepCount should be preserved when condensation is skipped")
	assert.Equal(t, session.PhaseIdle, state.Phase, "Phase should be preserved when condensation is skipped")
}

func TestCondenseAndMarkFullyCondensed_SkippedMarksFullyCondensed(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	s := &ManualCommitStrategy{}
	sessionID := "test-eager-skip"

	// Create a metadata dir with NO transcript
	metadataDir := ".entire/metadata/" + sessionID
	metadataDirAbs := filepath.Join(dir, metadataDir)
	require.NoError(t, os.MkdirAll(metadataDirAbs, 0o755))

	// Write a dummy file so SaveStep has something to commit to the shadow branch
	require.NoError(t, os.WriteFile(filepath.Join(dir, "dummy.txt"), []byte("x"), 0o644))

	// SaveStep creates the shadow branch
	err := s.SaveStep(context.Background(), StepContext{
		SessionID:      sessionID,
		ModifiedFiles:  []string{},
		NewFiles:       []string{"dummy.txt"},
		DeletedFiles:   []string{},
		MetadataDir:    metadataDir,
		MetadataDirAbs: metadataDirAbs,
		CommitMessage:  "Checkpoint without transcript",
		AuthorName:     "Test",
		AuthorEmail:    "test@test.com",
	})
	require.NoError(t, err)

	// Set phase to ENDED with no files (skip gate: no transcript + no files = skip)
	state, err := s.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	now := time.Now()
	state.Phase = session.PhaseEnded
	state.EndedAt = &now
	state.FilesTouched = nil
	require.NoError(t, s.saveSessionState(context.Background(), state))

	err = s.CondenseAndMarkFullyCondensed(context.Background(), sessionID)
	require.NoError(t, err)

	state, err = s.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, state)
	assert.True(t, state.FullyCondensed, "should be marked FullyCondensed when condensation is skipped")
	assert.Equal(t, session.PhaseEnded, state.Phase, "Phase should remain ENDED")
}

func TestTryAgentCommitFastPath_SkipsEmptySession(t *testing.T) {
	t.Setenv("ENTIRE_TEST_TTY", "0")
	dir := setupGitRepo(t)
	t.Chdir(dir)

	s := &ManualCommitStrategy{}

	// Create a commit message file
	commitMsgFile := filepath.Join(dir, "COMMIT_EDITMSG")
	require.NoError(t, os.WriteFile(commitMsgFile, []byte("test commit\n"), 0o644))

	// Empty session: no transcript path, no files, no step count (Codex companion pattern)
	emptySession := &SessionState{
		SessionID: "empty-codex-session",
		AgentType: "Codex",
		Phase:     session.PhaseActive,
		// TranscriptPath: "" (not set by Codex hooks)
		// FilesTouched:   nil
		// StepCount:      0
	}

	// Fast path should NOT add a trailer for the empty session
	result := s.tryAgentCommitFastPath(context.Background(), commitMsgFile, []*SessionState{emptySession}, "message")
	assert.False(t, result, "fast path should not fire for empty session")

	// Verify no trailer was added
	content, err := os.ReadFile(commitMsgFile)
	require.NoError(t, err)
	assert.NotContains(t, string(content), "Entire-Checkpoint", "should not add trailer for empty session")
}

func TestTryAgentCommitFastPath_AcceptsSessionWithContent(t *testing.T) {
	t.Setenv("ENTIRE_TEST_TTY", "0")
	dir := setupGitRepo(t)
	t.Chdir(dir)

	s := &ManualCommitStrategy{}

	commitMsgFile := filepath.Join(dir, "COMMIT_EDITMSG")
	require.NoError(t, os.WriteFile(commitMsgFile, []byte("test commit\n"), 0o644))

	// Session with content: has transcript path and step count
	contentSession := &SessionState{
		SessionID:      "claude-session",
		AgentType:      "Claude Code",
		Phase:          session.PhaseActive,
		TranscriptPath: "/some/path/to/transcript.jsonl",
		StepCount:      1,
	}

	result := s.tryAgentCommitFastPath(context.Background(), commitMsgFile, []*SessionState{contentSession}, "message")
	assert.True(t, result, "fast path should fire for session with content")

	// Verify trailer was added
	content, err := os.ReadFile(commitMsgFile)
	require.NoError(t, err)
	assert.Contains(t, string(content), "Entire-Checkpoint", "should add trailer for session with content")
}

func TestTryAgentCommitFastPath_SkipsEmptyButAcceptsContentSession(t *testing.T) {
	t.Setenv("ENTIRE_TEST_TTY", "0")
	dir := setupGitRepo(t)
	t.Chdir(dir)

	s := &ManualCommitStrategy{}

	commitMsgFile := filepath.Join(dir, "COMMIT_EDITMSG")
	require.NoError(t, os.WriteFile(commitMsgFile, []byte("test commit\n"), 0o644))

	// Two sessions: empty Codex companion + Claude Code with content
	emptySession := &SessionState{
		SessionID: "empty-codex-session",
		AgentType: "Codex",
		Phase:     session.PhaseActive,
	}
	contentSession := &SessionState{
		SessionID:      "claude-session",
		AgentType:      "Claude Code",
		Phase:          session.PhaseActive,
		TranscriptPath: "/some/path/to/transcript.jsonl",
		StepCount:      1,
	}

	result := s.tryAgentCommitFastPath(context.Background(), commitMsgFile, []*SessionState{emptySession, contentSession}, "message")
	assert.True(t, result, "fast path should fire for the content session")

	content, err := os.ReadFile(commitMsgFile)
	require.NoError(t, err)
	assert.Contains(t, string(content), "Entire-Checkpoint", "should add trailer from the content session")
}

// getHeadHash returns the HEAD commit hash as a string.
func getHeadHash(t *testing.T, repo *git.Repository) string {
	t.Helper()
	head, err := repo.Head()
	require.NoError(t, err)
	return head.Hash().String()
}
