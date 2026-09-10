//go:build integration

package integration

import (
	"encoding/json"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTokenWindow_SurvivesCarryForwardEndToEnd drives the partial-commit
// carry-forward through the real hooks and commit path and asserts what the
// committed metadata records about token scope:
//
//   - the second checkpoint's transcript is self-contained
//     (checkpoint_transcript_start == 0, the documented carry-forward behaviour), while
//   - its token window starts where the first condensation ended
//     (token_transcript_start == the offset the first commit advanced to), and
//   - the root summary stamps token_usage_version so readers can tell this
//     delta-scoped row from a legacy one.
//
// Before the window split, token_usage on such a checkpoint was computed from
// checkpoint_transcript_start == 0 and re-reported the whole session's tokens.
func TestTokenWindow_SurvivesCarryForwardEndToEnd(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	session := env.NewSession()

	// Turn 1: the agent creates two files; the user commits only one of them,
	// which leaves the session ACTIVE with file2 carried forward.
	if err := env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(
		session.ID, "Create two files", session.TranscriptPath,
	); err != nil {
		t.Fatalf("Turn 1 UserPromptSubmit failed: %v", err)
	}
	env.WriteFile("file1.txt", "one\n")
	env.WriteFile("file2.txt", "two\n")
	session.CreateTranscript("Create two files", []FileChange{
		{Path: "file1.txt", Content: "one\n"},
		{Path: "file2.txt", Content: "two\n"},
	})
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("Turn 1 Stop failed: %v", err)
	}
	env.GitCommitWithShadowHooks("Partial commit: only file1", "file1.txt")

	state1, err := env.GetSessionState(session.ID)
	require.NoError(t, err)
	require.NotNil(t, state1)
	require.Equal(t, 0, state1.CheckpointTranscriptStart,
		"carry-forward resets the transcript window so the next checkpoint is self-contained")
	require.Positive(t, state1.TokenTranscriptStart,
		"carry-forward must leave the token window at the end of the condensed transcript")
	tokenWindowStart := state1.TokenTranscriptStart

	// Turn 2: more transcript, then the user commits the carried-forward file.
	if err := env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(
		session.ID, "Now commit file2", session.TranscriptPath,
	); err != nil {
		t.Fatalf("Turn 2 UserPromptSubmit failed: %v", err)
	}
	session.TranscriptBuilder.AddUserMessage("Now commit file2")
	session.TranscriptBuilder.AddAssistantMessage("Committing file2.")
	require.NoError(t, session.TranscriptBuilder.WriteToFile(session.TranscriptPath))
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("Turn 2 Stop failed: %v", err)
	}
	env.GitCommitWithShadowHooks("Commit file2", "file2.txt")

	checkpointID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	require.NotEmpty(t, checkpointID, "second commit should carry a checkpoint trailer")

	// Session metadata: transcript self-contained, token window a delta.
	sessionJSON, found := env.ReadFileFromBranch(paths.MetadataBranchName, SessionMetadataPath(checkpointID))
	require.True(t, found, "session metadata should exist for checkpoint %s", checkpointID)
	var meta checkpoint.Metadata
	require.NoError(t, json.Unmarshal([]byte(sessionJSON), &meta))
	assert.Equal(t, 0, meta.CheckpointTranscriptStart, "carry-forward checkpoint transcript starts at line 0")
	assert.Equal(t, tokenWindowStart, meta.TokenTranscriptStart,
		"token_transcript_start must be the offset the first condensation advanced to, not 0")

	// Root summary: the version marker readers key on.
	rootJSON, found := env.ReadFileFromBranch(paths.MetadataBranchName, CheckpointSummaryPath(checkpointID))
	require.True(t, found, "root metadata should exist for checkpoint %s", checkpointID)
	var summary checkpoint.CheckpointSummary
	require.NoError(t, json.Unmarshal([]byte(rootJSON), &summary))
	assert.Equal(t, checkpoint.TokenUsageVersionDelta, summary.TokenUsageVersion,
		"every checkpoint written by this CLI stamps token_usage_version")
}
