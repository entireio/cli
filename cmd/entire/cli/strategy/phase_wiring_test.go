package strategy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInitializeSession_SetsPhaseActive verifies that InitializeSession
// transitions the session phase to ACTIVE via the state machine.
func TestInitializeSession_SetsPhaseActive(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	s := &ManualCommitStrategy{}

	err := s.InitializeSession(context.Background(), "test-session-phase-1", "Claude Code", "", "", "")
	require.NoError(t, err)

	state, err := s.loadSessionState(context.Background(), "test-session-phase-1")
	require.NoError(t, err)
	require.NotNil(t, state)

	assert.Equal(t, session.PhaseActive, state.Phase,
		"InitializeSession should set phase to ACTIVE")
	require.NotNil(t, state.LastInteractionTime,
		"InitializeSession should set LastInteractionTime")
	assert.NotEmpty(t, state.TurnID,
		"InitializeSession should set TurnID")
}

// TestInitializeSession_IdleToActive verifies a second call (existing IDLE session)
// transitions from IDLE to ACTIVE.
func TestInitializeSession_IdleToActive(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	s := &ManualCommitStrategy{}

	// First call initializes
	err := s.InitializeSession(context.Background(), "test-session-idle", "Claude Code", "", "", "")
	require.NoError(t, err)

	// Manually set to IDLE (simulating post-Stop state)
	state, err := s.loadSessionState(context.Background(), "test-session-idle")
	require.NoError(t, err)
	state.Phase = session.PhaseIdle
	state.LastInteractionTime = nil
	err = s.saveSessionState(context.Background(), state)
	require.NoError(t, err)

	// Second call should transition IDLE → ACTIVE
	err = s.InitializeSession(context.Background(), "test-session-idle", "Claude Code", "", "", "")
	require.NoError(t, err)

	state, err = s.loadSessionState(context.Background(), "test-session-idle")
	require.NoError(t, err)
	assert.Equal(t, session.PhaseActive, state.Phase)
	require.NotNil(t, state.LastInteractionTime)
}

// TestInitializeSession_ActiveToActive_CtrlCRecovery verifies Ctrl-C recovery:
// ACTIVE → ACTIVE transition when a new prompt arrives while already active.
func TestInitializeSession_ActiveToActive_CtrlCRecovery(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	s := &ManualCommitStrategy{}

	// First call
	err := s.InitializeSession(context.Background(), "test-session-ctrlc", "Claude Code", "", "", "")
	require.NoError(t, err)

	state, err := s.loadSessionState(context.Background(), "test-session-ctrlc")
	require.NoError(t, err)
	assert.Equal(t, session.PhaseActive, state.Phase)

	// Capture the first interaction time
	firstInteraction := state.LastInteractionTime
	require.NotNil(t, firstInteraction)

	// Small delay to ensure time differs
	time.Sleep(time.Millisecond)

	// Second call (Ctrl-C recovery) - should stay ACTIVE with updated time
	err = s.InitializeSession(context.Background(), "test-session-ctrlc", "Claude Code", "", "", "")
	require.NoError(t, err)

	state, err = s.loadSessionState(context.Background(), "test-session-ctrlc")
	require.NoError(t, err)
	assert.Equal(t, session.PhaseActive, state.Phase,
		"should stay ACTIVE on Ctrl-C recovery")
	require.NotNil(t, state.LastInteractionTime)
	assert.True(t, state.LastInteractionTime.After(*firstInteraction) ||
		state.LastInteractionTime.Equal(*firstInteraction),
		"LastInteractionTime should be updated on Ctrl-C recovery")
}

// TestInitializeSession_EndedToActive verifies that re-entering an ENDED session
// transitions to ACTIVE and clears EndedAt.
func TestInitializeSession_EndedToActive(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	s := &ManualCommitStrategy{}

	// First call initializes
	err := s.InitializeSession(context.Background(), "test-session-ended-reenter", "Claude Code", "", "", "")
	require.NoError(t, err)

	// Manually set to ENDED
	state, err := s.loadSessionState(context.Background(), "test-session-ended-reenter")
	require.NoError(t, err)
	endedAt := time.Now().Add(-time.Hour)
	state.Phase = session.PhaseEnded
	state.EndedAt = &endedAt
	err = s.saveSessionState(context.Background(), state)
	require.NoError(t, err)

	// Call InitializeSession again - should transition ENDED → ACTIVE
	err = s.InitializeSession(context.Background(), "test-session-ended-reenter", "Claude Code", "", "", "")
	require.NoError(t, err)

	state, err = s.loadSessionState(context.Background(), "test-session-ended-reenter")
	require.NoError(t, err)
	assert.Equal(t, session.PhaseActive, state.Phase,
		"should transition from ENDED to ACTIVE")
	assert.Nil(t, state.EndedAt,
		"EndedAt should be cleared when re-entering ENDED session")
	require.NotNil(t, state.LastInteractionTime)
}

// TestInitializeSession_EmptyPhaseBackwardCompat verifies that sessions
// without a Phase field (pre-state-machine) get treated as IDLE → ACTIVE.
func TestInitializeSession_EmptyPhaseBackwardCompat(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	s := &ManualCommitStrategy{}

	// First call initializes
	err := s.InitializeSession(context.Background(), "test-session-empty-phase", "Claude Code", "", "", "")
	require.NoError(t, err)

	// Manually clear the phase (simulating pre-state-machine file)
	state, err := s.loadSessionState(context.Background(), "test-session-empty-phase")
	require.NoError(t, err)
	state.Phase = ""
	err = s.saveSessionState(context.Background(), state)
	require.NoError(t, err)

	// Call again - empty phase treated as IDLE → should go to ACTIVE
	err = s.InitializeSession(context.Background(), "test-session-empty-phase", "Claude Code", "", "", "")
	require.NoError(t, err)

	state, err = s.loadSessionState(context.Background(), "test-session-empty-phase")
	require.NoError(t, err)
	assert.Equal(t, session.PhaseActive, state.Phase,
		"empty phase should be treated as IDLE → ACTIVE")
}

// setupGitRepo creates a temp directory with an initialized git repo and initial commit.
// Returns the directory path.
func setupGitRepo(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()

	repo, err := git.PlainInit(dir, false)
	require.NoError(t, err)

	// Configure git for commits
	cfg, err := repo.Config()
	require.NoError(t, err)
	cfg.User.Name = "Test User"
	cfg.User.Email = "test@test.com"
	err = repo.SetConfig(cfg)
	require.NoError(t, err)

	// Create initial commit (required for HEAD to exist)
	wt, err := repo.Worktree()
	require.NoError(t, err)

	// Create a test file
	testFile := filepath.Join(dir, "test.txt")
	require.NoError(t, writeTestFile(testFile, "initial content"))

	_, err = wt.Add("test.txt")
	require.NoError(t, err)

	_, err = wt.Commit("initial commit", &git.CommitOptions{})
	require.NoError(t, err)

	return dir
}

// TestInitializeSession_SetsCLIVersion verifies that InitializeSession
// persists versioninfo.Version in the session state.
func TestInitializeSession_SetsCLIVersion(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	s := &ManualCommitStrategy{}

	err := s.InitializeSession(context.Background(), "test-session-cli-version", "Claude Code", "", "", "")
	require.NoError(t, err)

	state, err := s.loadSessionState(context.Background(), "test-session-cli-version")
	require.NoError(t, err)
	require.NotNil(t, state)

	assert.Equal(t, versioninfo.Version, state.CLIVersion,
		"InitializeSession should set CLIVersion to versioninfo.Version")
}

// TestInitializeSession_SetsModelName verifies that InitializeSession
// persists the model name in the session state.
func TestInitializeSession_SetsModelName(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	s := &ManualCommitStrategy{}

	err := s.InitializeSession(context.Background(), "test-session-model", "OpenCode", "", "", "claude-sonnet-4-20250514")
	require.NoError(t, err)

	state, err := s.loadSessionState(context.Background(), "test-session-model")
	require.NoError(t, err)
	require.NotNil(t, state)

	assert.Equal(t, "claude-sonnet-4-20250514", state.ModelName,
		"InitializeSession should set ModelName from model parameter")
}

// TestInitializeSession_UpdatesModelOnSubsequentTurn verifies that model
// is updated when the user switches models between turns.
func TestInitializeSession_UpdatesModelOnSubsequentTurn(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	s := &ManualCommitStrategy{}

	// First turn with model A
	err := s.InitializeSession(context.Background(), "test-session-model-update", "OpenCode", "", "", "gpt-4o")
	require.NoError(t, err)

	state, err := s.loadSessionState(context.Background(), "test-session-model-update")
	require.NoError(t, err)
	assert.Equal(t, "gpt-4o", state.ModelName)

	// Transition to idle so second InitializeSession can transition back to active
	state.Phase = session.PhaseIdle
	require.NoError(t, s.saveSessionState(context.Background(), state))

	// Second turn with model B — should update
	err = s.InitializeSession(context.Background(), "test-session-model-update", "OpenCode", "", "", "claude-sonnet-4-20250514")
	require.NoError(t, err)

	state, err = s.loadSessionState(context.Background(), "test-session-model-update")
	require.NoError(t, err)
	assert.Equal(t, "claude-sonnet-4-20250514", state.ModelName,
		"InitializeSession should update ModelName when model changes between turns")
}

// TestInitializeSession_EmptyModelDoesNotOverwrite verifies that an empty
// model parameter does not clear a previously set model name.
func TestInitializeSession_EmptyModelDoesNotOverwrite(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	s := &ManualCommitStrategy{}

	// First turn with a model
	err := s.InitializeSession(context.Background(), "test-session-model-keep", "OpenCode", "", "", "gpt-4o")
	require.NoError(t, err)

	state, err := s.loadSessionState(context.Background(), "test-session-model-keep")
	require.NoError(t, err)
	assert.Equal(t, "gpt-4o", state.ModelName)

	// Transition to idle
	state.Phase = session.PhaseIdle
	require.NoError(t, s.saveSessionState(context.Background(), state))

	// Second turn with empty model — should preserve existing
	err = s.InitializeSession(context.Background(), "test-session-model-keep", "OpenCode", "", "", "")
	require.NoError(t, err)

	state, err = s.loadSessionState(context.Background(), "test-session-model-keep")
	require.NoError(t, err)
	assert.Equal(t, "gpt-4o", state.ModelName,
		"InitializeSession should not clear ModelName when model parameter is empty")
}

// TestCondenseAndMarkFullyCondensed_Guards verifies the two early-exit conditions
// in CondenseAndMarkFullyCondensed: sessions with no data are marked FullyCondensed
// immediately, and sessions with FilesTouched are left untouched for PostCommit.
func TestCondenseAndMarkFullyCondensed_Guards(t *testing.T) {
	t.Run("no data marks FullyCondensed immediately", func(t *testing.T) {
		dir := setupGitRepo(t)
		t.Chdir(dir)

		s := &ManualCommitStrategy{}
		require.NoError(t, s.InitializeSession(context.Background(), "test-session", "Claude Code", "", "", ""))

		state, err := s.loadSessionState(context.Background(), "test-session")
		require.NoError(t, err)
		now := time.Now()
		state.Phase = session.PhaseEnded
		state.EndedAt = &now
		state.StepCount = 0
		state.FilesTouched = nil
		require.NoError(t, s.saveSessionState(context.Background(), state))

		require.NoError(t, s.CondenseAndMarkFullyCondensed(context.Background(), "test-session"))

		state, err = s.loadSessionState(context.Background(), "test-session")
		require.NoError(t, err)
		require.NotNil(t, state)
		assert.True(t, state.FullyCondensed)
		assert.Equal(t, session.PhaseEnded, state.Phase)
	})

	t.Run("with FilesTouched is a no-op", func(t *testing.T) {
		dir := setupGitRepo(t)
		t.Chdir(dir)

		s := &ManualCommitStrategy{}
		require.NoError(t, s.InitializeSession(context.Background(), "test-session", "Claude Code", "", "", ""))

		state, err := s.loadSessionState(context.Background(), "test-session")
		require.NoError(t, err)
		now := time.Now()
		state.Phase = session.PhaseEnded
		state.EndedAt = &now
		state.FilesTouched = []string{"some_file.txt"}
		require.NoError(t, s.saveSessionState(context.Background(), state))

		require.NoError(t, s.CondenseAndMarkFullyCondensed(context.Background(), "test-session"))

		state, err = s.loadSessionState(context.Background(), "test-session")
		require.NoError(t, err)
		require.NotNil(t, state)
		assert.False(t, state.FullyCondensed)
		assert.Equal(t, []string{"some_file.txt"}, state.FilesTouched)
	})
}

// writeTestFile is a helper to create a test file with given content.
func writeTestFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

// TestCondenseAndMarkFullyCondensed_WithDataNoFiles verifies that a session with
// uncondensed data (StepCount > 0, shadow branch exists) but no FilesTouched
// is condensed and marked FullyCondensed. This is the subagent case from #591:
// the subagent's files were already committed by the parent session.
func TestCondenseAndMarkFullyCondensed_WithDataNoFiles(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}
	sessionID := "eager-condense-with-data"

	// Create metadata directory with a transcript file
	metadataDir := ".entire/metadata/" + sessionID
	metadataDirAbs := filepath.Join(dir, metadataDir)
	require.NoError(t, os.MkdirAll(metadataDirAbs, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(metadataDirAbs, paths.TranscriptFileName), []byte(testTranscriptPromptResponse), 0o644))

	// Write a file the agent "modified" (but it will be committed by parent)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "agent_file.txt"), []byte("agent work"), 0o644))

	// SaveStep creates the shadow branch
	err = s.SaveStep(context.Background(), StepContext{
		SessionID:      sessionID,
		ModifiedFiles:  []string{},
		NewFiles:       []string{"agent_file.txt"},
		DeletedFiles:   []string{},
		MetadataDir:    metadataDir,
		MetadataDirAbs: metadataDirAbs,
		CommitMessage:  "Checkpoint 1",
		AuthorName:     "Test",
		AuthorEmail:    "test@test.com",
	})
	require.NoError(t, err)

	// Set phase to ENDED with NO files (parent already committed them)
	state, err := s.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	now := time.Now()
	state.Phase = session.PhaseEnded
	state.EndedAt = &now
	state.FilesTouched = nil // Parent committed the files
	require.NoError(t, s.saveSessionState(context.Background(), state))

	require.Positive(t, state.StepCount, "StepCount should be > 0 before eager condense")

	// Run CondenseAndMarkFullyCondensed
	err = s.CondenseAndMarkFullyCondensed(context.Background(), sessionID)
	require.NoError(t, err)

	// Verify session state after eager condense
	state, err = s.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, state)

	assert.True(t, state.FullyCondensed,
		"session with no FilesTouched should be marked FullyCondensed after eager condense")
	assert.Equal(t, session.PhaseEnded, state.Phase,
		"Phase should stay ENDED (not IDLE like CondenseSessionByID)")
	assert.Equal(t, 0, state.StepCount,
		"StepCount should be reset after condensation")

	// Verify checkpoints branch was created (data condensed)
	_, err = repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err, "entire/checkpoints/v1 should exist after condensation")
}
