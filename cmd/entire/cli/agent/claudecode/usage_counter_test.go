package claudecode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/stretchr/testify/require"
)

// A ledger reading labels each subagent's calls with the model that made
// them, so its usage can be split by model; the main agent's stays unlabelled.
func TestUsageCounter_SubagentModels(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	main := filepath.Join(dir, "sess.jsonl")
	writeLines(t, main,
		`{"type":"assistant","uuid":"a1","message":{"id":"m1","model":"claude-opus-4-5","usage":{"input_tokens":3,"output_tokens":20}}}`,
		`{"type":"assistant","uuid":"a2","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Task","input":{"prompt":"one"}}]}}`,
		`{"type":"user","uuid":"u1","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"agentId: steady"}]}}`,
		`{"type":"assistant","uuid":"a3","message":{"content":[{"type":"tool_use","id":"toolu_2","name":"Task","input":{"prompt":"two"}}]}}`,
		`{"type":"user","uuid":"u2","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_2","content":"agentId: switcher"}]}}`,
	)
	subagents := paths.SubagentsDir(dir, "sess")
	// The first fragment naming a real model wins: a <synthetic> fragment
	// names none, and a later fragment does not relabel the message.
	writeLines(t, filepath.Join(subagents, paths.AgentTranscriptFileName("steady")),
		`{"type":"assistant","uuid":"s1","message":{"id":"sm1","model":"<synthetic>","usage":{"input_tokens":7,"output_tokens":1}}}`,
		`{"type":"assistant","uuid":"s2","message":{"id":"sm1","model":"claude-haiku-4-5","usage":{"input_tokens":7,"output_tokens":3}}}`,
		`{"type":"assistant","uuid":"s3","message":{"id":"sm1","model":"claude-sonnet-4-5","usage":{"input_tokens":7,"output_tokens":3}}}`,
	)
	writeLines(t, filepath.Join(subagents, paths.AgentTranscriptFileName("switcher")),
		`{"type":"assistant","uuid":"w1","message":{"id":"wm1","model":"claude-haiku-4-5","usage":{"input_tokens":11,"output_tokens":5}}}`,
		`{"type":"assistant","uuid":"w2","message":{"id":"wm2","model":"claude-sonnet-4-5","usage":{"input_tokens":13,"output_tokens":2}}}`,
	)

	obs, ok, err := agent.ReadCumulativeUsage(t.Context(), &ClaudeCodeAgent{}, agent.CumulativeTokenInput{SessionID: "sess", TranscriptPath: main}, nil)
	require.True(t, ok)
	require.NoError(t, err)
	require.Equal(t, &agent.TokenUsage{InputTokens: 3, OutputTokens: 20, APICallCount: 1}, obs.Total, "the main agent's total is unlabelled")
	require.Equal(t, &agent.TokenUsage{InputTokens: 7, OutputTokens: 3, APICallCount: 1, Model: "claude-haiku-4-5"}, obs.Children["steady"])
	require.Equal(t, &agent.TokenUsage{InputTokens: 24, OutputTokens: 7, APICallCount: 2, ByModel: []agent.TokenUsage{
		{InputTokens: 11, OutputTokens: 5, APICallCount: 1, Model: "claude-haiku-4-5"},
		{InputTokens: 13, OutputTokens: 2, APICallCount: 1, Model: "claude-sonnet-4-5"},
	}}, obs.Children["switcher"])
}

func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600))
}
