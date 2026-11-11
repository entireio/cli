package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/stretchr/testify/require"
)

// A Claude Code subagent transcript the reading lists but cannot read is a
// nil entry, so the ledger does not call the children's total complete.
func TestUsageCounter_ClaudeCodeUnreadableSubagentIsANilEntry(t *testing.T) {
	t.Parallel()
	ag, err := agent.Get(agent.AgentNameClaudeCode)
	require.NoError(t, err)
	dir := t.TempDir()
	require.NoError(t, os.CopyFS(dir, os.DirFS(filepath.Join("testdata", "usage", "claude-code"))))
	sub2 := filepath.Join(dir, "sess-1", "subagents", "agent-sub2.jsonl")
	require.NoError(t, os.Remove(sub2))
	require.NoError(t, os.Mkdir(sub2, 0o755)) // not a regular file: never read

	obs, ok, err := agent.ReadCumulativeUsage(context.Background(), ag, agent.CumulativeTokenInput{
		SessionID: "sess-1", TranscriptPath: filepath.Join(dir, "sess-1.jsonl"),
	}, nil)
	require.True(t, ok)
	require.NoError(t, err)
	require.Equal(t, &agent.TokenUsage{InputTokens: 7, OutputTokens: 3, APICallCount: 1}, obs.Children["sub1"])
	missing, listed := obs.Children["sub2"]
	require.True(t, listed, "the unreadable subagent is in the reading")
	require.Nil(t, missing)
}
