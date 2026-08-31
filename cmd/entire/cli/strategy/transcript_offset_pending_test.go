package strategy

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/stretchr/testify/require"

	// Register the Antigravity agent so GetByAgentType resolves it.
	_ "github.com/entireio/cli/cmd/entire/cli/agent/antigravity"
)

// Two non-blank agy step lines — the "previous turn" content.
const agyTurnOneContent = `{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","content":"<USER_REQUEST>first prompt</USER_REQUEST>"}
{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","content":"done"}
`

// Turn-one content plus a second turn appended — the post-flush file state.
const agyTwoTurnContent = agyTurnOneContent + `{"step_index":2,"source":"USER_EXPLICIT","type":"USER_INPUT","content":"<USER_REQUEST>second prompt</USER_REQUEST>"}
{"step_index":3,"source":"MODEL","type":"PLANNER_RESPONSE","content":"also done"}
`

func writeAgyTranscript(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "transcript_full.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

// TestAdvanceOffsetToTurnEnd_FlushLanded: the normal case — agy flushed before
// Stop, the advance moves the offset to the file end and no retry is pending.
func TestAdvanceOffsetToTurnEnd_FlushLanded(t *testing.T) {
	t.Parallel()
	state := &SessionState{
		SessionID:                 "agy-flush-landed",
		AgentType:                 agent.AgentTypeAntigravity,
		TranscriptPath:            writeAgyTranscript(t, agyTwoTurnContent),
		CheckpointTranscriptStart: 2,
	}
	advanceCheckpointTranscriptStartToTurnEnd(context.Background(), state)
	require.Equal(t, 4, state.CheckpointTranscriptStart, "offset must advance to the flushed file end")
	require.False(t, state.TranscriptOffsetPending, "successful advance must not leave a pending retry")
}

// TestAdvanceOffsetToTurnEnd_FlushLost: agy lost the flush race at Stop — the
// file still shows the previous state, so the advance can't move and the retry
// must be recorded for the next condensation.
func TestAdvanceOffsetToTurnEnd_FlushLost(t *testing.T) {
	t.Parallel()
	state := &SessionState{
		SessionID:                 "agy-flush-lost",
		AgentType:                 agent.AgentTypeAntigravity,
		TranscriptPath:            writeAgyTranscript(t, agyTurnOneContent),
		CheckpointTranscriptStart: 2, // already at the (stale) file end
	}
	advanceCheckpointTranscriptStartToTurnEnd(context.Background(), state)
	require.Equal(t, 2, state.CheckpointTranscriptStart, "stale file must not move the offset")
	require.True(t, state.TranscriptOffsetPending, "lost flush race must defer the advance to the next condensation")
}

// TestAdvanceOffsetToTurnEnd_StreamingAgentNeverPends: for streaming-transcript
// agents a non-growing transcript legitimately means "no new content" — the
// deferred-advance machinery must stay agy-only.
func TestAdvanceOffsetToTurnEnd_StreamingAgentNeverPends(t *testing.T) {
	t.Parallel()
	state := &SessionState{
		SessionID:                 "claude-no-growth",
		AgentType:                 agent.AgentTypeClaudeCode,
		TranscriptPath:            writeAgyTranscript(t, agyTurnOneContent),
		CheckpointTranscriptStart: 2,
	}
	advanceCheckpointTranscriptStartToTurnEnd(context.Background(), state)
	require.False(t, state.TranscriptOffsetPending, "streaming agents must never set TranscriptOffsetPending")
}

// TestResolvePendingTranscriptOffset_MidTurn is the a1 prompt-shift regression
// pin. Sequence being modeled: turn 1 commits mid-turn (condensation counted
// the then-empty transcript, offset stayed 0), turn 1's Stop lost the flush
// race (pending set), the flush landed between turns, and now turn 2 commits
// mid-turn. Mid-turn the file contains exactly turn 1 — agy flushes only after
// Stop — so without the deferred advance, prompt extraction at the stale
// offset attributes turn 1's prompt to turn 2's checkpoint.
func TestResolvePendingTranscriptOffset_MidTurn(t *testing.T) {
	t.Parallel()
	ag, err := agent.GetByAgentType(agent.AgentTypeAntigravity)
	require.NoError(t, err)

	path := writeAgyTranscript(t, agyTurnOneContent)
	state := &SessionState{
		SessionID:                 "agy-resolve-pending",
		AgentType:                 agent.AgentTypeAntigravity,
		Phase:                     session.PhaseActive,
		TranscriptPath:            path,
		CheckpointTranscriptStart: 0, // stale: counted from the unflushed file at turn 1's commit
		TranscriptOffsetPending:   true,
	}

	// The misattribution without the fix: at the stale offset, the previous
	// turn's prompt is what extraction hands to turn 2's checkpoint.
	stale := resolvePromptsFromLateFlushedTranscript(context.Background(), ag, path, state.CheckpointTranscriptStart)
	require.Equal(t, []string{"first prompt"}, stale,
		"sanity: the stale offset attributes the previous turn's prompt to the current checkpoint")

	resolvePendingTranscriptOffset(context.Background(), ag, state)
	require.Equal(t, 2, state.CheckpointTranscriptStart, "pending advance must complete against the flushed file end")
	require.False(t, state.TranscriptOffsetPending, "flag is one-shot")

	corrected := resolvePromptsFromLateFlushedTranscript(context.Background(), ag, path, state.CheckpointTranscriptStart)
	require.Empty(t, corrected,
		"at the corrected offset no already-condensed prompt leaks into the current checkpoint")
}

// TestResolvePendingTranscriptOffset_NotActive: outside ACTIVE phase the file
// may already contain the current turn's uncondensed content, so the advance
// must be skipped — and the one-shot flag still cleared.
func TestResolvePendingTranscriptOffset_NotActive(t *testing.T) {
	t.Parallel()
	ag, err := agent.GetByAgentType(agent.AgentTypeAntigravity)
	require.NoError(t, err)
	state := &SessionState{
		SessionID:                 "agy-resolve-idle",
		AgentType:                 agent.AgentTypeAntigravity,
		Phase:                     session.PhaseIdle,
		TranscriptPath:            writeAgyTranscript(t, agyTwoTurnContent),
		CheckpointTranscriptStart: 2,
		TranscriptOffsetPending:   true,
	}
	resolvePendingTranscriptOffset(context.Background(), ag, state)
	require.Equal(t, 2, state.CheckpointTranscriptStart, "idle session must not advance (file may hold uncondensed content)")
	require.False(t, state.TranscriptOffsetPending, "flag is one-shot even when the advance is skipped")
}

// TestResolvePendingTranscriptOffset_NoFlag: without the flag the helper must
// not touch the offset at all.
func TestResolvePendingTranscriptOffset_NoFlag(t *testing.T) {
	t.Parallel()
	ag, err := agent.GetByAgentType(agent.AgentTypeAntigravity)
	require.NoError(t, err)
	state := &SessionState{
		SessionID:                 "agy-resolve-noflag",
		AgentType:                 agent.AgentTypeAntigravity,
		Phase:                     session.PhaseActive,
		TranscriptPath:            writeAgyTranscript(t, agyTwoTurnContent),
		CheckpointTranscriptStart: 2,
	}
	resolvePendingTranscriptOffset(context.Background(), ag, state)
	require.Equal(t, 2, state.CheckpointTranscriptStart)
}
