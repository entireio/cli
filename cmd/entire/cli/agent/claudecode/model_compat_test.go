package claudecode

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	transcriptlib "github.com/entireio/cli/transcript"

	"github.com/stretchr/testify/require"
)

// A subagent's per-model token usage must agree with the per-call model and
// usage the transcript library writes on the same transcript's lines, so a
// reader pricing either one gets the same answer. The fixture streams one
// message in fragments (the first naming the model wins in both), mixes two
// models, and includes a <synthetic> API-error entry.
func TestSubagentUsageByModelMatchesTranscriptLibrary(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"type":"assistant","uuid":"a1","message":{"id":"m1","model":"claude-haiku-4-5","content":[{"type":"text","text":"one"}],"usage":{"input_tokens":3,"cache_read_input_tokens":100,"cache_creation_input_tokens":20,"output_tokens":5}}}
{"type":"assistant","uuid":"a2","message":{"id":"m1","model":"claude-sonnet-4-5","content":[{"type":"text","text":"two"}],"usage":{"input_tokens":3,"cache_read_input_tokens":100,"cache_creation_input_tokens":20,"output_tokens":9}}}
{"type":"user","uuid":"u1","message":{"content":"next"}}
{"type":"assistant","uuid":"a3","message":{"id":"m2","model":"claude-sonnet-4-5","content":[{"type":"text","text":"three"}],"usage":{"input_tokens":7,"cache_read_input_tokens":300,"output_tokens":11}}}
{"type":"assistant","uuid":"a4","message":{"id":"m3","model":"<synthetic>","content":[{"type":"text","text":"error"}],"usage":{"input_tokens":0,"output_tokens":0}}}
`)
	usage, err := (&ClaudeCodeAgent{}).CalculateSubagentTokenUsage(raw)
	require.NoError(t, err)
	got := map[string]agent.TokenUsage{}
	for _, entry := range append(usage.ByModel, agent.TokenUsage{}) {
		if entry.Model != "" {
			got[entry.Model] = agent.TokenUsage{InputTokens: entry.InputTokens, CacheReadTokens: entry.CacheReadTokens,
				CacheCreationTokens: entry.CacheCreationTokens, OutputTokens: entry.OutputTokens, APICallCount: entry.APICallCount}
		}
	}

	compact, err := transcriptlib.Convert(raw, transcriptlib.Options{Agent: "claude-code"})
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
		sum.CacheCreationTokens += int(line.CacheWriteTokens)
		sum.OutputTokens += int(line.OutputTokens)
		sum.APICallCount++
		want[line.Model] = sum
	}

	require.Len(t, want, 2, "the library sees two models")
	require.Equal(t, want, got)
}
