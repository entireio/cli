package strategy

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/buildinfo"
	"github.com/entireio/cli/cmd/entire/cli/session"

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

	err := s.InitializeSession("test-session-phase-1", "Claude Code", "", "")
	require.NoError(t, err)

	state, err := s.loadSessionState("test-session-phase-1")
	require.NoError(t, err)
	require.NotNil(t, state)

	assert.Equal(t, session.PhaseActive, state.Phase,
		"InitializeSession should set phase to ACTIVE")
	require.NotNil(t, state.LastInteractionTime,
		"InitializeSession should set LastInteractionTime")
}

// TestInitializeSession_IdleToActive verifies a second call (existing IDLE session)
// transitions from IDLE to ACTIVE.
func TestInitializeSession_IdleToActive(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	s := &ManualCommitStrategy{}

	// First call initializes
	err := s.InitializeSession("test-session-idle", "Claude Code", "", "")
	require.NoError(t, err)

	// Manually set to IDLE (simulating post-Stop state)
	state, err := s.loadSessionState("test-session-idle")
	require.NoError(t, err)
	state.Phase = session.PhaseIdle
	state.LastInteractionTime = nil
	err = s.saveSessionState(state)
	require.NoError(t, err)

	// Second call should transition IDLE → ACTIVE
	err = s.InitializeSession("test-session-idle", "Claude Code", "", "")
	require.NoError(t, err)

	state, err = s.loadSessionState("test-session-idle")
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
	err := s.InitializeSession("test-session-ctrlc", "Claude Code", "", "")
	require.NoError(t, err)

	state, err := s.loadSessionState("test-session-ctrlc")
	require.NoError(t, err)
	assert.Equal(t, session.PhaseActive, state.Phase)

	// Capture the first interaction time
	firstInteraction := state.LastInteractionTime
	require.NotNil(t, firstInteraction)

	// Small delay to ensure time differs
	time.Sleep(time.Millisecond)

	// Second call (Ctrl-C recovery) - should stay ACTIVE with updated time
	err = s.InitializeSession("test-session-ctrlc", "Claude Code", "", "")
	require.NoError(t, err)

	state, err = s.loadSessionState("test-session-ctrlc")
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
	err := s.InitializeSession("test-session-ended-reenter", "Claude Code", "", "")
	require.NoError(t, err)

	// Manually set to ENDED
	state, err := s.loadSessionState("test-session-ended-reenter")
	require.NoError(t, err)
	endedAt := time.Now().Add(-time.Hour)
	state.Phase = session.PhaseEnded
	state.EndedAt = &endedAt
	err = s.saveSessionState(state)
	require.NoError(t, err)

	// Call InitializeSession again - should transition ENDED → ACTIVE
	err = s.InitializeSession("test-session-ended-reenter", "Claude Code", "", "")
	require.NoError(t, err)

	state, err = s.loadSessionState("test-session-ended-reenter")
	require.NoError(t, err)
	assert.Equal(t, session.PhaseActive, state.Phase,
		"should transition from ENDED to ACTIVE")
	assert.Nil(t, state.EndedAt,
		"EndedAt should be cleared when re-entering ENDED session")
	require.NotNil(t, state.LastInteractionTime)
}

// TestInitializeSession_OldActiveCommittedToActive verifies backward compatibility:
// if a state file from an older CLI version has phase="active_committed",
// PhaseFromString normalizes it to ACTIVE, so the next TurnStart stays ACTIVE.
func TestInitializeSession_OldActiveCommittedToActive(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	s := &ManualCommitStrategy{}

	// First call initializes
	err := s.InitializeSession("test-session-ac-recovery", "Claude Code", "", "")
	require.NoError(t, err)

	// Manually set phase to the old "active_committed" string (simulating old state file)
	state, err := s.loadSessionState("test-session-ac-recovery")
	require.NoError(t, err)
	state.Phase = "active_committed" // raw string, not using the deprecated var
	err = s.saveSessionState(state)
	require.NoError(t, err)

	// Call InitializeSession again - PhaseFromString normalizes to ACTIVE,
	// then TurnStart keeps it ACTIVE
	err = s.InitializeSession("test-session-ac-recovery", "Claude Code", "", "")
	require.NoError(t, err)

	state, err = s.loadSessionState("test-session-ac-recovery")
	require.NoError(t, err)
	assert.Equal(t, session.PhaseActive, state.Phase,
		"old active_committed phase should normalize to ACTIVE")
}

// TestInitializeSession_EmptyPhaseBackwardCompat verifies that sessions
// without a Phase field (pre-state-machine) get treated as IDLE → ACTIVE.
func TestInitializeSession_EmptyPhaseBackwardCompat(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	s := &ManualCommitStrategy{}

	// First call initializes
	err := s.InitializeSession("test-session-empty-phase", "Claude Code", "", "")
	require.NoError(t, err)

	// Manually clear the phase (simulating pre-state-machine file)
	state, err := s.loadSessionState("test-session-empty-phase")
	require.NoError(t, err)
	state.Phase = ""
	err = s.saveSessionState(state)
	require.NoError(t, err)

	// Call again - empty phase treated as IDLE → should go to ACTIVE
	err = s.InitializeSession("test-session-empty-phase", "Claude Code", "", "")
	require.NoError(t, err)

	state, err = s.loadSessionState("test-session-empty-phase")
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
// persists buildinfo.Version in the session state.
func TestInitializeSession_SetsCLIVersion(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	s := &ManualCommitStrategy{}

	err := s.InitializeSession("test-session-cli-version", "Claude Code", "", "")
	require.NoError(t, err)

	state, err := s.loadSessionState("test-session-cli-version")
	require.NoError(t, err)
	require.NotNil(t, state)

	assert.Equal(t, buildinfo.Version, state.CLIVersion,
		"InitializeSession should set CLIVersion to buildinfo.Version")
}

// writeTestFile is a helper to create a test file with given content.
func writeTestFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
