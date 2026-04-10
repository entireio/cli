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

// TestCheckpointTranscriptStart_IncludesUncondensedTurns verifies that
// checkpoint_transcript_start correctly includes un-condensed intermediate
// turns in the next checkpoint's scoped transcript.
//
// When Turn N modifies files but the user doesn't commit, and then Turn N+1
// triggers a commit, the checkpoint should include Turn N's transcript content
// because Turn N's file changes are part of the commit.
func TestCheckpointTranscriptStart_IncludesUncondensedTurns(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)

	session := env.NewSession()

	// ============================
	// Turn 1: Modify files, commit
	// ============================
	if err := env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(
		session.ID, "Create auth module", session.TranscriptPath,
	); err != nil {
		t.Fatalf("Turn 1 UserPromptSubmit failed: %v", err)
	}

	env.WriteFile("auth.go", "package auth\n\nfunc Login() {}\n")
	session.CreateTranscript("Create auth module", []FileChange{
		{Path: "auth.go", Content: "package auth\n\nfunc Login() {}\n"},
	})

	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("Turn 1 Stop failed: %v", err)
	}

	env.GitCommitWithShadowHooks("Add auth module", "auth.go")

	state1, err := env.GetSessionState(session.ID)
	require.NoError(t, err)
	require.NotNil(t, state1)
	offsetAfterCommit1 := state1.CheckpointTranscriptStart
	t.Logf("After commit 1: CheckpointTranscriptStart=%d", offsetAfterCommit1)

	// ====================================
	// Turn 2: Modify files, stop, NO commit
	// ====================================
	if err := env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(
		session.ID, "Update README", session.TranscriptPath,
	); err != nil {
		t.Fatalf("Turn 2 UserPromptSubmit failed: %v", err)
	}

	env.WriteFile("README.md", "# Updated README\n\nNew content from Turn 2.\n")
	session.CreateTranscript("Update README", []FileChange{
		{Path: "README.md", Content: "# Updated README\n\nNew content from Turn 2.\n"},
	})

	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("Turn 2 Stop failed: %v", err)
	}
	// NO commit — Turn 2's file changes stay uncommitted

	// ==========================================
	// Turn 3: Trigger commit (like "commit/push")
	// ==========================================
	if err := env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(
		session.ID, "commit/push", session.TranscriptPath,
	); err != nil {
		t.Fatalf("Turn 3 UserPromptSubmit failed: %v", err)
	}

	// No file changes in Turn 3 — agent just commits
	session.TranscriptBuilder.AddUserMessage("commit/push")
	session.TranscriptBuilder.AddAssistantMessage("I'll commit and push the changes.")
	session.TranscriptBuilder.AddAssistantMessage("Done! Changes committed and pushed.")
	if err := session.TranscriptBuilder.WriteToFile(session.TranscriptPath); err != nil {
		t.Fatalf("Failed to write Turn 3 transcript: %v", err)
	}

	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("Turn 3 Stop failed: %v", err)
	}

	// User commits the README changes from Turn 2
	env.GitCommitWithShadowHooks("Update README", "README.md")

	checkpointID2 := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	require.NotEmpty(t, checkpointID2, "Second commit should have checkpoint trailer")

	// ==========================================
	// ASSERTION: Turn 2's content should be included
	// ==========================================
	metadataPath := SessionMetadataPath(checkpointID2)
	content, found := env.ReadFileFromBranch(paths.MetadataBranchName, metadataPath)
	require.True(t, found, "Session metadata should exist for checkpoint %s", checkpointID2)

	var metadata checkpoint.CommittedMetadata
	require.NoError(t, json.Unmarshal([]byte(content), &metadata))

	t.Logf("Checkpoint 2: checkpoint_transcript_start=%d (commit 1 offset was %d)",
		metadata.GetTranscriptStart(), offsetAfterCommit1)

	// checkpoint_transcript_start should equal the offset from the first condensation,
	// because Turn 2's content (which modified the committed file) should be included
	// in this checkpoint's scoped transcript.
	assert.Equal(t, offsetAfterCommit1, metadata.GetTranscriptStart(),
		"checkpoint_transcript_start should include un-condensed Turn 2 content "+
			"(Turn 2 modified README.md which is part of this commit)")
}

// TestCheckpointTranscriptStart_AdvancesPastMidTurnCommit verifies that when
// an agent commits mid-turn (before Stop fires), CheckpointTranscriptStart
// advances to the actual end of the turn — not just the transcript length at
// commit time.
//
// Reproduces a bug observed with Codex: the agent's response continues writing
// to the transcript after the commit hooks fire (tool results, token counts,
// task_complete events). Without the fix, the next checkpoint's scoped transcript
// starts mid-turn, including a tail of already-condensed content.
func TestCheckpointTranscriptStart_AdvancesPastMidTurnCommit(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)

	session := env.NewSession()

	// ============================
	// Turn 1: Agent commits mid-turn
	// ============================

	// UserPromptSubmit with transcript path
	if err := env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(
		session.ID, "Create a file and commit it", session.TranscriptPath,
	); err != nil {
		t.Fatalf("Turn 1 UserPromptSubmit failed: %v", err)
	}

	// Agent creates a file
	env.WriteFile("feature.go", "package feature\n\nfunc New() {}\n")

	// Write a partial transcript (as it would be at commit time — agent still responding)
	session.TranscriptBuilder.AddUserMessage("Create a file and commit it")
	session.TranscriptBuilder.AddAssistantMessage("I'll create the file and commit it.")
	toolID := session.TranscriptBuilder.AddToolUse("mcp__acp__Write", "feature.go", "package feature\n\nfunc New() {}\n")
	session.TranscriptBuilder.AddToolResult(toolID)
	if err := session.TranscriptBuilder.WriteToFile(session.TranscriptPath); err != nil {
		t.Fatalf("Failed to write partial transcript: %v", err)
	}

	// Agent commits mid-turn (before Stop)
	env.GitCommitWithShadowHooksAsAgent("Add feature", "feature.go")

	// Record CheckpointTranscriptStart set by condensation
	stateAfterCommit, err := env.GetSessionState(session.ID)
	require.NoError(t, err)
	require.NotNil(t, stateAfterCommit)
	offsetAtCommitTime := stateAfterCommit.CheckpointTranscriptStart
	t.Logf("After mid-turn commit: CheckpointTranscriptStart=%d", offsetAtCommitTime)

	// Agent continues writing AFTER the commit (more tool calls, summary, etc.)
	session.TranscriptBuilder.AddAssistantMessage("File created and committed successfully.")
	session.TranscriptBuilder.AddAssistantMessage("The commit includes feature.go with the New() function.")
	if err := session.TranscriptBuilder.WriteToFile(session.TranscriptPath); err != nil {
		t.Fatalf("Failed to write extended transcript: %v", err)
	}

	// Stop fires — turn ends, finalization happens
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("Turn 1 Stop failed: %v", err)
	}

	// Check that CheckpointTranscriptStart advanced past the mid-turn commit position
	stateAfterStop, err := env.GetSessionState(session.ID)
	require.NoError(t, err)
	require.NotNil(t, stateAfterStop)
	offsetAfterStop := stateAfterStop.CheckpointTranscriptStart
	t.Logf("After Stop: CheckpointTranscriptStart=%d (was %d at commit time)",
		offsetAfterStop, offsetAtCommitTime)

	assert.Greater(t, offsetAfterStop, offsetAtCommitTime,
		"CheckpointTranscriptStart should advance past mid-turn commit position; "+
			"at commit time it was %d, but the turn continued writing — "+
			"Stop should advance it to the full transcript length to avoid "+
			"including already-condensed tail content in the next checkpoint",
		offsetAtCommitTime)
}
