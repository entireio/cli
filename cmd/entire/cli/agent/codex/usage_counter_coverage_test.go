package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/stretchr/testify/require"
)

// An inventoried child whose rollout can't be read is a nil entry in the
// reading, so the ledger knows the children's total lacks it, as the per-turn
// inventory extraction marks it incomplete.
func TestUsageCounter_UnreadableChildIsANilEntry(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ag := &CodexAgent{RolloutRoots: []string{root}}
	read := writeRollout(t, root, "rollout-read.jsonl", "read", []json.RawMessage{
		taskEvent("task_started", stringPointer("read-turn")),
		taskEvent("task_complete", stringPointer("read-turn")),
		tokenCountEvent(map[string]any{"total_token_usage": map[string]any{"input_tokens": 5, "cached_input_tokens": 2, "output_tokens": 1}}),
	})
	parent := filepath.Join(root, "rollout-parent.jsonl")
	require.NoError(t, os.WriteFile(parent, rolloutData(t, "parent", []json.RawMessage{
		tokenCountEvent(map[string]any{"total_token_usage": map[string]any{"input_tokens": 100, "cached_input_tokens": 0, "output_tokens": 10}}),
	}), 0o600))
	refs := []agent.SubagentReference{
		{AgentID: "read", DeclaredTranscriptPath: read},
		{AgentID: "missing", DeclaredTranscriptPath: filepath.Join(root, "rollout-missing.jsonl")},
	}
	in := agent.CumulativeTokenInput{TranscriptPath: parent, Subagents: refs}

	extraction, ok := agent.ExtractWithSubagentInventory(t.Context(), ag, nil, 0, refs, "")
	require.True(t, ok)
	require.False(t, *extraction.TokenUsage.SubagentTokensComplete, "the per-turn extraction does not claim the missing child")

	obs, _, err := agent.ReadCumulativeUsage(t.Context(), ag, in, nil)
	require.NoError(t, err)
	require.Equal(t, 3, obs.Children["read"].InputTokens, "the readable child is counted")
	missing, listed := obs.Children["missing"]
	require.True(t, listed, "the unreadable child is in the reading")
	require.Nil(t, missing)

	again, _, err := agent.ReadCumulativeUsage(t.Context(), ag, in, obs.Cursor)
	require.NoError(t, err)
	require.Equal(t, obs.Children, again.Children, "the cached inventory keeps the nil entry")
}
