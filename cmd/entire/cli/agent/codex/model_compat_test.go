package codex

import (
	"encoding/json"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	transcriptlib "github.com/entireio/cli/transcript"

	"github.com/stretchr/testify/require"
)

// A child's per-model usage must agree with the per-call model and usage the
// transcript library writes on the same rollout's lines, including across a
// mid-run model switch, so a reader pricing either one gets the same answer.
func TestChildUsageByModelMatchesTranscriptLibrary(t *testing.T) {
	t.Parallel()

	message := func(text string) json.RawMessage {
		data, err := json.Marshal(map[string]any{"type": "response_item", "payload": map[string]any{
			"type": "message", "role": "assistant", "content": []map[string]any{{"type": "output_text", "text": text}},
		}})
		require.NoError(t, err)
		return data
	}
	raw := rolloutData(t, "child", []json.RawMessage{
		turnContextEvent("gpt-5.6-sol"),
		message("one"), totalUsageEvent(10, 4, 2),
		message("two"), totalUsageEvent(25, 10, 3),
		turnContextEvent("gpt-6-luna"),
		message("three"), totalUsageEvent(55, 30, 7),
	})

	usage := analyzeRollout(raw).ExactTokenUsage
	require.NotNil(t, usage)
	got := map[string]agent.TokenUsage{}
	for _, entry := range usage.ByModel {
		got[entry.Model] = agent.TokenUsage{InputTokens: entry.InputTokens, CacheReadTokens: entry.CacheReadTokens,
			OutputTokens: entry.OutputTokens, APICallCount: entry.APICallCount}
	}

	compact, err := transcriptlib.Convert(raw, transcriptlib.Options{Agent: "codex"})
	require.NoError(t, err)
	lines, err := transcriptlib.Decode(compact)
	require.NoError(t, err)
	want := map[string]agent.TokenUsage{}
	for _, line := range lines {
		if line.Model == "" {
			continue
		}
		sum := want[line.Model]
		sum.InputTokens += int(line.InputTokens)
		sum.CacheReadTokens += int(line.CacheReadTokens)
		sum.OutputTokens += int(line.OutputTokens)
		sum.APICallCount++
		want[line.Model] = sum
	}

	require.Len(t, want, 2, "the library sees two models")
	require.Equal(t, want, got)
}
