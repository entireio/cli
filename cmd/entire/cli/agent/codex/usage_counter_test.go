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
// (its last token_count) and labelled with its model, apart from the parent's
// total, on every reading.
func TestUsageCounter_InventoriedChild(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ag := &CodexAgent{RolloutRoots: []string{root}}
	child := writeRollout(t, root, "rollout-child.jsonl", "child", []json.RawMessage{
		taskEvent("task_started", stringPointer("child-turn")),
		turnContextEvent("gpt-5.6-sol"),
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
	require.Equal(t, &agent.TokenUsage{InputTokens: 3, CacheReadTokens: 2, OutputTokens: 1, APICallCount: 1, Model: "gpt-5.6-sol"}, first.Children["child"],
		"the child records its turn_context model")

	again, _, err := agent.ReadCumulativeUsage(t.Context(), ag, in, first.Cursor)
	require.NoError(t, err)
	require.Equal(t, first.Children, again.Children, "an unchanged child reports the same total")

	// The child grows: its total is read again.
	f, err := os.OpenFile(child, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	line := tokenCountEvent(map[string]any{"total_token_usage": map[string]any{"input_tokens": 9, "cached_input_tokens": 2, "output_tokens": 4}})
	_, err = f.Write(append(line, '\n'))
	require.NoError(t, err)
	require.NoError(t, f.Close())
	grown, _, err := agent.ReadCumulativeUsage(t.Context(), ag, in, again.Cursor)
	require.NoError(t, err)
	require.Equal(t, 4, grown.Children["child"].OutputTokens)
}
