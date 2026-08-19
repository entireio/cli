//go:build integration

package integration

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/stretchr/testify/require"
)

// TestEndedToIdle_PrepareCommitMsgAddsTrailer is the natural reproduction of a
// session-end (ENDED) → session-start (ENDED→IDLE) → new turn → git commit
// sequence. prepare-commit-msg must still attach Entire-Checkpoint after the
// session restarts from ENDED into IDLE.
func TestEndedToIdle_PrepareCommitMsgAddsTrailer(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	sess := env.NewSession()

	require.NoError(t, env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(
		sess.ID, "Initial work", sess.TranscriptPath))
	env.WriteFile("first.go", "package main\n")
	sess.CreateTranscript("Initial work", []FileChange{
		{Path: "first.go", Content: "package main\n"},
	})
	require.NoError(t, env.SimulateStop(sess.ID, sess.TranscriptPath))

	state, err := env.GetSessionState(sess.ID)
	require.NoError(t, err)
	require.NotNil(t, state)
	require.Equal(t, session.PhaseIdle, state.Phase)

	require.NoError(t, env.SimulateSessionEnd(sess.ID))
	state, err = env.GetSessionState(sess.ID)
	require.NoError(t, err)
	require.NotNil(t, state)
	require.Equal(t, session.PhaseEnded, state.Phase)

	// Reported trigger: SessionStart after ENDED (ended → idle).
	out := env.SimulateSessionStartWithOutput(sess.ID)
	require.NoError(t, out.Err)
	state, err = env.GetSessionState(sess.ID)
	require.NoError(t, err)
	require.NotNil(t, state)
	require.Equal(t, session.PhaseIdle, state.Phase)
	require.Nil(t, state.EndedAt)

	require.NoError(t, env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(
		sess.ID, "Work after restart", sess.TranscriptPath))
	env.WriteFile("second.go", "package main\n\nfunc Second() {}\n")
	sess.CreateTranscript("Work after restart", []FileChange{
		{Path: "second.go", Content: "package main\n\nfunc Second() {}\n"},
	})
	require.NoError(t, env.SimulateStop(sess.ID, sess.TranscriptPath))

	env.GitCommitWithShadowHooks("Add second after ended→idle restart", "second.go")
	cpID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	require.NotEmpty(t, cpID,
		"prepare-commit-msg must add Entire-Checkpoint after ended→idle session restart")
}
