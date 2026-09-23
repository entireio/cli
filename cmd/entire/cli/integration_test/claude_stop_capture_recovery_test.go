//go:build integration

package integration

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClaudeStop_FailedContinuationCaptureRetainsRecoveryAtNextPrompt(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	sess := env.NewSession()
	const prompt = "Create retained.go"
	const firstResponse = "Initial response complete."
	require.NoError(t, sess.TranscriptBuilder.WriteToFile(sess.TranscriptPath))
	require.NoError(t, env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(sess.ID, prompt, sess.TranscriptPath))
	env.WriteFile("retained.go", "package retained\n")
	sess.TranscriptBuilder.AddUserMessage(prompt)
	toolID := sess.TranscriptBuilder.AddToolUse("mcp__acp__Write", "retained.go", "package retained\n")
	sess.TranscriptBuilder.AddToolResult(toolID)
	sess.TranscriptBuilder.AddAssistantMessage(firstResponse)
	require.NoError(t, sess.TranscriptBuilder.WriteToFile(sess.TranscriptPath))
	env.GitCommitWithShadowHooks("Add retained file", "retained.go")
	checkpointID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	require.NotEmpty(t, checkpointID)
	require.NoError(t, env.SimulateStopWithFinalResponse(sess.ID, sess.TranscriptPath, firstResponse))

	require.NoError(t, os.Remove(sess.TranscriptPath))
	require.Error(t, env.SimulateStopWithFinalResponse(sess.ID, sess.TranscriptPath, "Continuation complete."))
	require.NoError(t, sess.TranscriptBuilder.WriteToFile(sess.TranscriptPath))
	require.NoError(t, env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(sess.ID, "Next prompt", sess.TranscriptPath))
	state, err := env.GetSessionState(sess.ID)
	require.NoError(t, err)
	require.Equal(t, []string{checkpointID}, state.TurnCheckpointIDs)
	require.True(t, state.TurnEndRefreshRequired)
	require.False(t, state.TurnEndPending)
}
