package strategy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeSessionMetadataFile writes one of a session's staged metadata files
// (prompt.txt, full.jsonl) under .entire/metadata/<session>/.
func writeSessionMetadataFile(t *testing.T, repoDir, sessionID, name, content string) {
	t.Helper()
	dir := filepath.Join(repoDir, paths.SessionMetadataDirFromSessionID(sessionID))
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
}

// PreviewNextCheckpoint is the `checkpoint list --pending` preview: sessions
// in this worktree with pending work, newest activity first, each with its
// turns, files, task records, and prompts since the last checkpoint. Prompts
// come from prompt.txt, then the agent's transcript extractor from
// CheckpointTranscriptStart. Uses t.Chdir — do NOT add t.Parallel().
func TestPreviewNextCheckpoint(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "seed.txt", "seed")
	testutil.GitAdd(t, dir, "seed.txt")
	testutil.GitCommit(t, dir, "seed")
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	ctx := context.Background()
	worktree, err := paths.WorktreeRoot(ctx)
	require.NoError(t, err)

	newer := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	older := newer.Add(-time.Hour)
	save := func(state *SessionState) {
		t.Helper()
		if state.StartedAt.IsZero() {
			state.StartedAt = older.Add(-time.Hour)
		}
		require.NoError(t, SaveSessionState(ctx, state))
	}

	// Pending work with prompt.txt and task records.
	save(&SessionState{
		SessionID:           "preview-with-prompts",
		AgentType:           agent.AgentTypeAntigravity,
		WorktreePath:        worktree,
		Phase:               session.PhaseIdle,
		StepCount:           2,
		FilesTouched:        []string{"b.go", "a.go"},
		LastPrompt:          "second ask",
		LastInteractionTime: &newer,
		TaskRecords: []session.TaskRecord{
			{ToolUseID: "toolu_running", SubagentType: "Explore", TaskDescription: "look around", StartedAt: older},
			{ToolUseID: "toolu_done", SubagentType: "general-purpose", TaskDescription: "fix it", StartedAt: older, CompletedAt: newer},
		},
	})
	writeSessionMetadataFile(t, dir, "preview-with-prompts", paths.PromptFileName, "first ask\n\n---\n\nsecond ask")

	// Pending work with no prompt.txt: prompts come from the stored transcript
	// after the last checkpoint's offset.
	save(&SessionState{
		SessionID:                 "preview-from-transcript",
		AgentType:                 agent.AgentTypeAntigravity,
		WorktreePath:              worktree,
		Phase:                     session.PhaseIdle,
		StepCount:                 1,
		FilesTouched:              []string{"c.go"},
		CheckpointTranscriptStart: 2,
		LastInteractionTime:       &older,
	})
	writeSessionMetadataFile(t, dir, "preview-from-transcript", paths.TranscriptFileName,
		`{"type":"USER_INPUT","content":"<USER_REQUEST>\nalready checkpointed\n</USER_REQUEST>"}
{"type":"PLANNER_RESPONSE"}
{"type":"USER_INPUT","content":"<USER_REQUEST>\nsince the checkpoint\n</USER_REQUEST>"}
`)

	// Left out: nothing pending, fully condensed and ended, another worktree.
	save(&SessionState{SessionID: "nothing-pending", AgentType: agent.AgentTypeAntigravity, WorktreePath: worktree, Phase: session.PhaseIdle})
	save(&SessionState{
		SessionID: "fully-condensed", AgentType: agent.AgentTypeAntigravity, WorktreePath: worktree,
		Phase: session.PhaseEnded, FullyCondensed: true,
		TaskRecords: []session.TaskRecord{{ToolUseID: "toolu_left", StartedAt: older}},
	})
	save(&SessionState{SessionID: "other-worktree", AgentType: agent.AgentTypeAntigravity, WorktreePath: filepath.Join(t.TempDir(), "elsewhere"), Phase: session.PhaseIdle, StepCount: 1, FilesTouched: []string{"x.go"}})

	previews, err := (&ManualCommitStrategy{}).PreviewNextCheckpoint(ctx)
	require.NoError(t, err)
	require.Len(t, previews, 2)

	first := previews[0]
	assert.Equal(t, "preview-with-prompts", first.SessionID, "newest activity first")
	assert.Equal(t, agent.AgentTypeAntigravity, first.Agent)
	assert.Equal(t, 2, first.Turns)
	assert.Equal(t, []string{"a.go", "b.go"}, first.FilesTouched, "files are sorted")
	assert.Equal(t, []string{"first ask", "second ask"}, first.Prompts)
	assert.Equal(t, "second ask", first.LastPrompt)
	assert.True(t, first.LastActivity.Equal(newer))
	assert.Equal(t, []NextCheckpointTaskRecord{
		{ToolUseID: "toolu_running", SubagentType: "Explore", Description: "look around"},
		{ToolUseID: "toolu_done", SubagentType: "general-purpose", Description: "fix it", Completed: true},
	}, first.TaskRecords)

	second := previews[1]
	assert.Equal(t, "preview-from-transcript", second.SessionID)
	assert.Equal(t, 1, second.Turns)
	assert.Equal(t, []string{"c.go"}, second.FilesTouched)
	assert.Equal(t, []string{"since the checkpoint"}, second.Prompts)
	assert.Empty(t, second.TaskRecords)
}
