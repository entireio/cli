package claudecode

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/stretchr/testify/require"
)

// A Workflow agent whose transcript is gone is not read, so the reading
// leaves it out rather than report the total an earlier reading counted:
// the ledger keeps that total, but does not take it as counted now.
func TestUsageCounter_DeletedWorkflowAgentIsNotInTheReading(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	runDir := filepath.Join(dir, "sess", "subagents", "workflows", "wf_e5264e60-494")
	require.NoError(t, os.MkdirAll(runDir, 0o755))
	lines := makeWorkflowLaunchLines(t, "toolu_wf", "wf_e5264e60-494")
	lines = append(lines, `{"type":"assistant","uuid":"a2","message":{"id":"m-main","usage":{"input_tokens":1000,"output_tokens":100}}}`)
	transcript := filepath.Join(dir, "sess.jsonl")
	require.NoError(t, os.WriteFile(transcript, buildJSONL(lines...), 0o600))
	workflowAgent := filepath.Join(runDir, "agent-ae3d7b8f2930c8787.jsonl")
	writeJSONLFile(t, workflowAgent,
		`{"type":"assistant","isSidechain":true,"agentId":"ae3d7b8f2930c8787","message":{"id":"m-w1","usage":{"input_tokens":10,"output_tokens":40}}}`)
	in := agent.CumulativeTokenInput{SessionID: "sess", TranscriptPath: transcript}

	first, ok, err := agent.ReadCumulativeUsage(context.Background(), &ClaudeCodeAgent{}, in, nil)
	require.True(t, ok)
	require.NoError(t, err)
	require.Equal(t, &agent.TokenUsage{InputTokens: 10, OutputTokens: 40, APICallCount: 1}, first.Children["ae3d7b8f2930c8787"])

	require.NoError(t, os.Remove(workflowAgent))
	obs, ok, err := agent.ReadCumulativeUsage(context.Background(), &ClaudeCodeAgent{}, in, first.Cursor)
	require.True(t, ok)
	require.NoError(t, err)
	_, listed := obs.Children["ae3d7b8f2930c8787"]
	require.False(t, listed, "the deleted Workflow agent's transcript was not read")
	require.Equal(t, first.Total, obs.Total)
}
