package cli

import (
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"

	"github.com/go-git/go-git/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMarkSessionEnded_SetsPhaseEnded verifies that markSessionEnded
// transitions the session phase to ENDED via the state machine.
func TestMarkSessionEnded_SetsPhaseEnded(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)

	// Create a session in ACTIVE phase
	state := &strategy.SessionState{
		SessionID:  "test-session-end-1",
		BaseCommit: "abc123",
		StartedAt:  time.Now(),
		Phase:      session.PhaseActive,
	}
	err := strategy.SaveSessionState(state)
	require.NoError(t, err)

	// Call markSessionEnded
	err = markSessionEnded("test-session-end-1")
	require.NoError(t, err)

	// Verify phase is ENDED
	loaded, err := strategy.LoadSessionState("test-session-end-1")
	require.NoError(t, err)
	require.NotNil(t, loaded)

	assert.Equal(t, session.PhaseEnded, loaded.Phase,
		"markSessionEnded should set phase to ENDED")
	require.NotNil(t, loaded.EndedAt,
		"markSessionEnded should set EndedAt")
	require.NotNil(t, loaded.LastInteractionTime,
		"markSessionEnded should set LastInteractionTime")
}

// TestMarkSessionEnded_IdleToEnded verifies IDLE → ENDED transition.
func TestMarkSessionEnded_IdleToEnded(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)

	state := &strategy.SessionState{
		SessionID:  "test-session-end-idle",
		BaseCommit: "abc123",
		StartedAt:  time.Now(),
		Phase:      session.PhaseIdle,
	}
	err := strategy.SaveSessionState(state)
	require.NoError(t, err)

	err = markSessionEnded("test-session-end-idle")
	require.NoError(t, err)

	loaded, err := strategy.LoadSessionState("test-session-end-idle")
	require.NoError(t, err)
	assert.Equal(t, session.PhaseEnded, loaded.Phase)
	require.NotNil(t, loaded.EndedAt)
}

// TestMarkSessionEnded_AlreadyEndedIsNoop verifies ENDED → ENDED (no-op).
func TestMarkSessionEnded_AlreadyEndedIsNoop(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)

	originalEndedAt := time.Now().Add(-time.Hour)
	state := &strategy.SessionState{
		SessionID:  "test-session-end-noop",
		BaseCommit: "abc123",
		StartedAt:  time.Now(),
		Phase:      session.PhaseEnded,
		EndedAt:    &originalEndedAt,
	}
	err := strategy.SaveSessionState(state)
	require.NoError(t, err)

	err = markSessionEnded("test-session-end-noop")
	require.NoError(t, err)

	loaded, err := strategy.LoadSessionState("test-session-end-noop")
	require.NoError(t, err)
	assert.Equal(t, session.PhaseEnded, loaded.Phase)
	// EndedAt should still be set (updated, not cleared)
	require.NotNil(t, loaded.EndedAt)
}

// TestMarkSessionEnded_EmptyPhaseBackwardCompat verifies that sessions
// without a Phase field get properly transitioned.
func TestMarkSessionEnded_EmptyPhaseBackwardCompat(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)

	state := &strategy.SessionState{
		SessionID:  "test-session-end-compat",
		BaseCommit: "abc123",
		StartedAt:  time.Now(),
		Phase:      "", // pre-state-machine
	}
	err := strategy.SaveSessionState(state)
	require.NoError(t, err)

	err = markSessionEnded("test-session-end-compat")
	require.NoError(t, err)

	loaded, err := strategy.LoadSessionState("test-session-end-compat")
	require.NoError(t, err)
	assert.Equal(t, session.PhaseEnded, loaded.Phase,
		"empty phase → IDLE → ENDED")
}

// TestMarkSessionEnded_NoState verifies that markSessionEnded is a no-op
// when no session state exists.
func TestMarkSessionEnded_NoState(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)

	err := markSessionEnded("nonexistent-session")
	assert.NoError(t, err, "should be a no-op when no state exists")
}

// setupGitRepoForPhaseTest creates a temp directory with an initialized git repo.
func setupGitRepoForPhaseTest(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	return dir
}
