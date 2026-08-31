package strategy

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/antigravity"
	"github.com/stretchr/testify/require"
)

// TestCountTranscriptItems_AntigravitySkipsBlankLines verifies that the offset
// written into CheckpointTranscriptStart uses the same metric Antigravity's
// readers use. ExtractPrompts and GetTranscriptPosition skip blank lines before
// counting (the codex splitJSONL convention); if countTranscriptItems counted
// raw lines, an interior blank line would make the stored offset overshoot and
// resolvePromptsFromLateFlushedTranscript would silently drop the first
// prompt(s) of the next checkpoint.
func TestCountTranscriptItems_AntigravitySkipsBlankLines(t *testing.T) {
	t.Parallel()

	content := `{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","content":"hi"}

{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","content":"ok"}
`
	got := countTranscriptItems(agent.AgentTypeAntigravity, content)
	require.Equal(t, 2, got, "antigravity count must skip interior blank lines to match ExtractPrompts/GetTranscriptPosition")

	// Cross-check against the agent's own position metric — the two must agree,
	// since one is written into CheckpointTranscriptStart and the other reads it.
	ag := antigravity.NewAntigravityAgent()
	analyzer, ok := agent.AsTranscriptAnalyzer(ag)
	require.True(t, ok)
	dir := t.TempDir()
	path := dir + "/transcript_full.jsonl"
	require.NoError(t, writeTestFile(path, content))
	pos, err := analyzer.GetTranscriptPosition(path)
	require.NoError(t, err)
	require.Equal(t, pos, got, "countTranscriptItems and GetTranscriptPosition must use the same metric")
}
