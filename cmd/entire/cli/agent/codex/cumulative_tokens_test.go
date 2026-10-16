package codex

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/testutil"
)

func TestCumulativeTokenUsage(t *testing.T) {
	t.Parallel()

	record := func(id string, in, cached, out int) string {
		return fmt.Sprintf(`{"type":"token_usage_record","payload":{"turn_id":"t1","response_id":%q,"usage":{"input_tokens":%d,"cached_input_tokens":%d,"cache_write_input_tokens":0,"output_tokens":%d,"reasoning_output_tokens":0}}}`, id, in, cached, out)
	}
	total := func(in, cached, out int) string {
		return fmt.Sprintf(`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":%d,"cached_input_tokens":%d,"cache_write_input_tokens":0,"output_tokens":%d}}}}`, in, cached, out)
	}
	tests := []struct {
		name  string
		lines []string
		want  *agent.TokenUsage
	}{
		{"records beat a stale replayed token_count total",
			[]string{record("r1", 1000, 600, 50), total(1000, 600, 50), record("r2", 1200, 900, 30), record("r2", 1200, 900, 30), total(1000, 600, 50)},
			&agent.TokenUsage{InputTokens: 700, CacheReadTokens: 1500, OutputTokens: 80, APICallCount: 2}},
		{"usage from before records began is kept",
			[]string{total(3000, 2000, 30), record("r1", 3500, 3000, 5), total(6500, 5000, 35)},
			&agent.TokenUsage{InputTokens: 1500, CacheReadTokens: 5000, OutputTokens: 35, APICallCount: 2}},
		{"token_count fallback: replay ignored, restart starts a segment",
			[]string{total(1000, 600, 50), total(2200, 1500, 80), total(2200, 1500, 80), total(300, 100, 10)},
			&agent.TokenUsage{InputTokens: 900, CacheReadTokens: 1600, OutputTokens: 90, APICallCount: 3}},
		{"no usage is nil", []string{`{"type":"session_meta","payload":{"id":"s"}}`}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data := []byte(strings.Join(tt.lines, "\n") + "\n")
			got, children, err := (&CodexAgent{}).CumulativeTokenUsage(t.Context(), agent.CumulativeTokenInput{TranscriptData: data})
			require.NoError(t, err)
			testutil.AssertCumulative(t, got, children, tt.want, nil)
		})
	}
}

func TestCumulativeTokenUsage_InventoryChildren(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	child := writeRollout(t, root, "rollout-child.jsonl", "child", []json.RawMessage{
		tokenCountEvent(map[string]any{"total_token_usage": map[string]any{"input_tokens": 5, "cached_input_tokens": 2, "output_tokens": 1}}),
	})
	total, children, err := (&CodexAgent{RolloutRoots: []string{root}}).CumulativeTokenUsage(t.Context(), agent.CumulativeTokenInput{
		TranscriptData: rolloutData(t, "parent", nil),
		Subagents:      []agent.SubagentReference{{AgentID: "child", DeclaredTranscriptPath: child}},
	})
	require.NoError(t, err)
	testutil.AssertCumulative(t, total, children, nil, map[string]*agent.TokenUsage{
		"child": {InputTokens: 3, CacheReadTokens: 2, OutputTokens: 1, APICallCount: 1},
	})
}
