package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/stretchr/testify/require"
)

// A ledger reading counts an inventoried child with the inventory's own rule
// (its last token_count), apart from the parent's total, on every reading.
func TestUsageCounter_InventoriedChild(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ag := &CodexAgent{RolloutRoots: []string{root}}
	child := writeRollout(t, root, "rollout-child.jsonl", "child", []json.RawMessage{
		taskEvent("task_started", stringPointer("child-turn")),
		taskEvent("task_complete", stringPointer("child-turn")),
		tokenCountEvent(map[string]any{"total_token_usage": map[string]any{"input_tokens": 5, "cached_input_tokens": 2, "output_tokens": 1}}),
	})
	parent := filepath.Join(root, "rollout-parent.jsonl")
	require.NoError(t, os.WriteFile(parent, rolloutData(t, "parent", []json.RawMessage{
		tokenCountEvent(map[string]any{"total_token_usage": map[string]any{"input_tokens": 100, "cached_input_tokens": 0, "output_tokens": 10}}),
	}), 0o600))
	in := agent.CumulativeTokenInput{TranscriptPath: parent, Subagents: []agent.SubagentReference{{AgentID: "child", DeclaredTranscriptPath: child}}}

	first, ok, err := agent.ReadCumulativeUsage(t.Context(), ag, in, nil)
	require.True(t, ok)
	require.NoError(t, err)
	require.Equal(t, 100, first.Total.InputTokens, "the child is not in the parent's total")
	require.Equal(t, &agent.TokenUsage{InputTokens: 3, CacheReadTokens: 2, OutputTokens: 1, APICallCount: 1}, first.Children["child"])

	again, _, err := agent.ReadCumulativeUsage(t.Context(), ag, in, first.Cursor)
	require.NoError(t, err)
	require.Equal(t, first.Children, again.Children, "a later reading reports the same child total")
}
