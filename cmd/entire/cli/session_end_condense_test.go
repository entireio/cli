package cli

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// spawnCall records one request to the detached-condense spawn seam.
type spawnCall struct {
	worktreeRoot string
	sessionID    string
}

// swapSessionEndCondenseSpawn replaces the detached-condense spawn seam with a
// recorder and restores it on cleanup. Tests using it must not run in parallel
// (package-level seam).
func swapSessionEndCondenseSpawn(t *testing.T) *[]spawnCall {
	t.Helper()
	var spawned []spawnCall
	orig := sessionEndCondenseSpawn
	sessionEndCondenseSpawn = func(worktreeRoot, sessionID string) error {
		spawned = append(spawned, spawnCall{worktreeRoot: worktreeRoot, sessionID: sessionID})
		return nil
	}
	t.Cleanup(func() { sessionEndCondenseSpawn = orig })
	return &spawned
}

func saveIdleSessionForEndTest(t *testing.T, sessionID string) {
	t.Helper()
	state := &strategy.SessionState{
		SessionID:  sessionID,
		BaseCommit: "abc123",
		StartedAt:  time.Now(),
		Phase:      session.PhaseIdle,
	}
	require.NoError(t, strategy.SaveSessionState(context.Background(), state))
}

// TestHandleLifecycleSessionEnd_SpawnsDetachedCondense verifies the SessionEnd
// hook path marks the session ended in-process (fast, within the agent's hook
// budget) and defers the eager condense to a detached child instead of running
// it inline.
func TestHandleLifecycleSessionEnd_SpawnsDetachedCondense(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)

	const sessionID = "test-session-end-detach"
	saveIdleSessionForEndTest(t, sessionID)
	spawned := swapSessionEndCondenseSpawn(t)

	event := &agent.Event{Type: agent.SessionEnd, SessionID: sessionID}
	require.NoError(t, handleLifecycleSessionEnd(context.Background(), newMockAgent(), event))

	loaded, err := strategy.LoadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, loaded)
	assert.Equal(t, session.PhaseEnded, loaded.Phase,
		"session must be marked ENDED synchronously")
	assert.False(t, loaded.FullyCondensed,
		"condense must not run inline on the hook path")
	require.Len(t, *spawned, 1,
		"exactly one detached condense must be requested for the ended session")
	assert.Equal(t, sessionID, (*spawned)[0].sessionID)
	// The child resolves the repo and session store from its working
	// directory, so spawning anywhere but the worktree root would make the
	// condense a silent no-op in the wrong place.
	wantRoot, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	gotRoot, err := filepath.EvalSymlinks((*spawned)[0].worktreeRoot)
	require.NoError(t, err)
	assert.Equal(t, wantRoot, gotRoot, "child must be spawned from the worktree root")
}

// TestHandleLifecycleSessionEnd_NoRepoFallsBackToSync verifies that when the
// worktree root can't be resolved there is nowhere to run the child, so the
// handler takes the synchronous endSessionNow path instead of spawning —
// dropping the condense silently would regress #591.
func TestHandleLifecycleSessionEnd_NoRepoFallsBackToSync(t *testing.T) {
	t.Chdir(t.TempDir()) // plain directory, not a git repo

	spawned := swapSessionEndCondenseSpawn(t)

	event := &agent.Event{Type: agent.SessionEnd, SessionID: "some-session"}
	require.NoError(t, handleLifecycleSessionEnd(context.Background(), newMockAgent(), event))

	assert.Empty(t, *spawned, "no detached condense without a resolvable worktree root")
}

// TestHandleLifecycleSessionEnd_SyncEnvCondensesInline verifies the
// ENTIRE_SESSION_END_SYNC escape hatch keeps the pre-detach synchronous
// behavior (used by integration tests for determinism).
func TestHandleLifecycleSessionEnd_SyncEnvCondensesInline(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)
	t.Setenv(envSessionEndSyncCondense, "1")

	const sessionID = "test-session-end-sync"
	saveIdleSessionForEndTest(t, sessionID)
	spawned := swapSessionEndCondenseSpawn(t)

	event := &agent.Event{Type: agent.SessionEnd, SessionID: sessionID}
	require.NoError(t, handleLifecycleSessionEnd(context.Background(), newMockAgent(), event))

	loaded, err := strategy.LoadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, loaded)
	assert.Equal(t, session.PhaseEnded, loaded.Phase)
	assert.True(t, loaded.FullyCondensed,
		"sync mode must condense inline (StepCount 0 → marked fully condensed)")
	assert.Empty(t, *spawned, "sync mode must not spawn a detached condense")
}

// TestHandleLifecycleSessionEnd_NoSpawnWhenSessionMissing verifies no detached
// child is forked when there is no session to condense.
func TestHandleLifecycleSessionEnd_NoSpawnWhenSessionMissing(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)

	spawned := swapSessionEndCondenseSpawn(t)

	event := &agent.Event{Type: agent.SessionEnd, SessionID: "no-such-session"}
	require.NoError(t, handleLifecycleSessionEnd(context.Background(), newMockAgent(), event))

	assert.Empty(t, *spawned, "no detached condense for a session that was never tracked")
}

// TestCondenseSessionCmd_MarksFullyCondensed verifies the hidden
// __condense_session command runs the eager condense for an ended session.
func TestCondenseSessionCmd_MarksFullyCondensed(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)

	const sessionID = "test-condense-session-cmd"
	now := time.Now()
	state := &strategy.SessionState{
		SessionID:  sessionID,
		BaseCommit: "abc123",
		StartedAt:  now,
		Phase:      session.PhaseEnded,
		EndedAt:    &now,
	}
	require.NoError(t, strategy.SaveSessionState(context.Background(), state))

	cmd := newCondenseSessionCmd()
	cmd.SetArgs([]string{sessionID})
	require.NoError(t, cmd.ExecuteContext(context.Background()))

	loaded, err := strategy.LoadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, loaded)
	assert.True(t, loaded.FullyCondensed,
		"__condense_session must condense the ended session (StepCount 0 → fully condensed)")
}

// TestCondenseSessionCmd_MissingSessionIsFailOpen verifies the detached child
// never errors on a session that disappeared between spawn and exec.
func TestCondenseSessionCmd_MissingSessionIsFailOpen(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)

	cmd := newCondenseSessionCmd()
	cmd.SetArgs([]string{"gone-session"})
	assert.NoError(t, cmd.ExecuteContext(context.Background()))
}
