//go:build integration

package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/stretchr/testify/require"
)

func TestClaudeStop_CapturedTranscriptOwnsFinalizationAndPosition(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	sess := env.NewSession()
	const (
		prompt        = "Create captured.go"
		finalResponse = "Captured response complete."
	)
	require.NoError(t, sess.TranscriptBuilder.WriteToFile(sess.TranscriptPath))
	require.NoError(t, env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(sess.ID, prompt, sess.TranscriptPath))

	env.WriteFile("captured.go", "package captured\n")
	sess.TranscriptBuilder.AddUserMessage(prompt)
	toolID := sess.TranscriptBuilder.AddToolUse("mcp__acp__Write", "captured.go", "package captured\n")
	sess.TranscriptBuilder.AddToolResult(toolID)
	sess.TranscriptBuilder.AddAssistantMessage(finalResponse)
	require.NoError(t, sess.TranscriptBuilder.WriteToFile(sess.TranscriptPath))
	captured := []byte(sess.TranscriptBuilder.String())

	env.GitCommitWithShadowHooks("Add captured file", "captured.go")
	checkpointID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	require.NotEmpty(t, checkpointID)

	ownedPath := filepath.Join(env.RepoDir, paths.SessionMetadataDirFromSessionID(sess.ID), paths.TranscriptFileName)
	mutationResult := make(chan mutationObservation, 1)
	go replaceSourceAfterOwnedCopy(ownedPath, sess.TranscriptPath, captured, mutationResult)

	stopStarted := time.Now()
	require.NoError(t, env.SimulateStopWithFinalResponse(sess.ID, sess.TranscriptPath, finalResponse))
	stopReturned := time.Now()
	observation := <-mutationResult
	require.NoError(t, observation.err)
	require.True(t, observation.at.After(stopStarted))
	require.True(t, observation.at.Before(stopReturned), "live source was not mutated while Stop was still running")

	stored, err := os.ReadFile(ownedPath)
	require.NoError(t, err)
	require.Equal(t, captured, stored)

	finalized, found := env.ReadFileFromBranch(paths.MetadataBranchName, SessionFilePath(checkpointID, paths.TranscriptFileName))
	require.True(t, found)
	require.Equal(t, string(captured), finalized)
	require.NotContains(t, finalized, "mutated live source")

	state, err := env.GetSessionState(sess.ID)
	require.NoError(t, err)
	require.Equal(t, session.PhaseIdle, state.Phase)
	require.Equal(t, []string{checkpointID}, state.TurnCheckpointIDs)
	require.True(t, state.TurnEndPending)
	require.Equal(t, strings.Count(string(captured), "\n"), state.CheckpointTranscriptStart)
}

func TestClaudeSessionEnd_RefreshesPendingStopSnapshot(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	sess := env.NewSession()
	const (
		prompt            = "Create session_end.go"
		firstResponse     = "Initial response complete."
		continuedResponse = "Continuation captured at session end."
	)
	require.NoError(t, sess.TranscriptBuilder.WriteToFile(sess.TranscriptPath))
	require.NoError(t, env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(sess.ID, prompt, sess.TranscriptPath))

	env.WriteFile("session_end.go", "package sessionend\n")
	sess.TranscriptBuilder.AddUserMessage(prompt)
	toolID := sess.TranscriptBuilder.AddToolUse("mcp__acp__Write", "session_end.go", "package sessionend\n")
	sess.TranscriptBuilder.AddToolResult(toolID)
	sess.TranscriptBuilder.AddAssistantMessage(firstResponse)
	require.NoError(t, sess.TranscriptBuilder.WriteToFile(sess.TranscriptPath))

	env.GitCommitWithShadowHooks("Add session-end fixture", "session_end.go")
	checkpointID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	require.NotEmpty(t, checkpointID)
	require.NoError(t, env.SimulateStopWithFinalResponse(sess.ID, sess.TranscriptPath, firstResponse))

	sess.TranscriptBuilder.AddAssistantMessage(continuedResponse)
	require.NoError(t, sess.TranscriptBuilder.WriteToFile(sess.TranscriptPath))
	require.NoError(t, env.SimulateSessionEnd(sess.ID))

	finalized, found := env.ReadFileFromBranch(
		paths.MetadataBranchName,
		SessionFilePath(checkpointID, paths.TranscriptFileName),
	)
	require.True(t, found)
	require.Contains(t, finalized, continuedResponse)

	state, err := env.GetSessionState(sess.ID)
	require.NoError(t, err)
	require.Equal(t, session.PhaseEnded, state.Phase)
	require.Empty(t, state.TurnCheckpointIDs)
	require.False(t, state.TurnEndPending)
	require.False(t, state.TurnEndRefreshRequired)
	require.Equal(t, 1, state.SessionTurnCount)
}

func TestClaudeSessionEnd_FailedRefreshRetriesAfterResume(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	sess := env.NewSession()
	const (
		prompt            = "Create recover.go"
		firstResponse     = "Initial checkpoint response."
		continuedResponse = "Response written before interruption."
		resumePrompt      = "Resume recovery"
		recoveredResponse = "Recovery complete."
	)
	require.NoError(t, sess.TranscriptBuilder.WriteToFile(sess.TranscriptPath))
	require.NoError(t, env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(sess.ID, prompt, sess.TranscriptPath))

	env.WriteFile("recover.go", "package recover\n")
	sess.TranscriptBuilder.AddUserMessage(prompt)
	toolID := sess.TranscriptBuilder.AddToolUse("mcp__acp__Write", "recover.go", "package recover\n")
	sess.TranscriptBuilder.AddToolResult(toolID)
	sess.TranscriptBuilder.AddAssistantMessage(firstResponse)
	require.NoError(t, sess.TranscriptBuilder.WriteToFile(sess.TranscriptPath))
	env.GitCommitWithShadowHooks("Add recovery fixture", "recover.go")
	checkpointID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	require.NotEmpty(t, checkpointID)
	require.NoError(t, env.SimulateStopWithFinalResponse(sess.ID, sess.TranscriptPath, firstResponse))

	sess.TranscriptBuilder.AddAssistantMessage(continuedResponse)
	require.NoError(t, os.Remove(sess.TranscriptPath))
	require.NoError(t, env.SimulateSessionEnd(sess.ID))

	state, err := env.GetSessionState(sess.ID)
	require.NoError(t, err)
	require.Equal(t, session.PhaseEnded, state.Phase)
	require.Equal(t, []string{checkpointID}, state.TurnCheckpointIDs)
	require.True(t, state.TurnEndRefreshRequired)
	require.False(t, state.TurnEndPending)

	require.NoError(t, sess.TranscriptBuilder.WriteToFile(sess.TranscriptPath))
	require.NoError(t, env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(sess.ID, resumePrompt, sess.TranscriptPath))
	state, err = env.GetSessionState(sess.ID)
	require.NoError(t, err)
	require.Equal(t, []string{checkpointID}, state.TurnCheckpointIDs)
	require.True(t, state.TurnEndRefreshRequired)

	sess.TranscriptBuilder.AddUserMessage(resumePrompt)
	sess.TranscriptBuilder.AddAssistantMessage(recoveredResponse)
	require.NoError(t, sess.TranscriptBuilder.WriteToFile(sess.TranscriptPath))
	require.NoError(t, env.SimulateStopWithFinalResponse(sess.ID, sess.TranscriptPath, recoveredResponse))

	finalized, found := env.ReadFileFromBranch(
		paths.MetadataBranchName,
		SessionFilePath(checkpointID, paths.TranscriptFileName),
	)
	require.True(t, found)
	require.Contains(t, finalized, continuedResponse)
	require.Contains(t, finalized, recoveredResponse)

	state, err = env.GetSessionState(sess.ID)
	require.NoError(t, err)
	require.False(t, state.TurnEndRefreshRequired)
	require.Equal(t, []string{checkpointID}, state.TurnCheckpointIDs)
}

func TestClaudeStop_UnmatchedFinalResponsePreservesProvisionalCheckpoint(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	sess := env.NewSession()
	const prompt = "Create provisional.go"
	require.NoError(t, sess.TranscriptBuilder.WriteToFile(sess.TranscriptPath))
	require.NoError(t, env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(sess.ID, prompt, sess.TranscriptPath))

	env.WriteFile("provisional.go", "package provisional\n")
	sess.CreateTranscript(prompt, []FileChange{{Path: "provisional.go", Content: "package provisional\n"}})
	env.GitCommitWithShadowHooks("Add provisional file", "provisional.go")
	checkpointID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	require.NotEmpty(t, checkpointID)

	checkpointPath := SessionFilePath(checkpointID, paths.TranscriptFileName)
	provisional, found := env.ReadFileFromBranch(paths.MetadataBranchName, checkpointPath)
	require.True(t, found)

	const actualFinalResponse = "Actual final response."
	sess.TranscriptBuilder.AddAssistantMessage(actualFinalResponse)
	require.NoError(t, sess.TranscriptBuilder.WriteToFile(sess.TranscriptPath))

	err := env.SimulateStopWithFinalResponse(sess.ID, sess.TranscriptPath, "Response that was never produced.")
	require.Error(t, err)
	require.Contains(t, err.Error(), "transcript not ready")

	unchanged, found := env.ReadFileFromBranch(paths.MetadataBranchName, checkpointPath)
	require.True(t, found)
	require.Equal(t, provisional, unchanged)
	require.NotContains(t, unchanged, actualFinalResponse)

	state, err := env.GetSessionState(sess.ID)
	require.NoError(t, err)
	require.Equal(t, session.PhaseActive, state.Phase)
	require.Contains(t, state.TurnCheckpointIDs, checkpointID)
}

type mutationObservation struct {
	at  time.Time
	err error
}

func replaceSourceAfterOwnedCopy(ownedPath, sourcePath string, expected []byte, result chan<- mutationObservation) {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		owned, err := os.ReadFile(ownedPath)
		if err == nil && string(owned) == string(expected) {
			mutated := strings.Repeat(`{"type":"assistant","message":{"content":"mutated live source"}}`+"\n", 20)
			writeErr := os.WriteFile(sourcePath, []byte(mutated), 0o600)
			result <- mutationObservation{at: time.Now(), err: writeErr}
			return
		}
		time.Sleep(time.Millisecond)
	}
	result <- mutationObservation{err: fmt.Errorf("owned transcript %s did not appear", ownedPath)}
}
