package strategy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"

	"github.com/go-git/go-git/v5"
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

// writeTestFile is a helper to create a test file with given content.
func writeTestFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
