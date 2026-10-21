package strategy

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/antigravity"
	"github.com/stretchr/testify/require"
)

// TestResolvePromptsFromLateFlushedTranscript verifies that the condensation-time
// fallback re-extracts user prompts directly from a populated transcript via the
// agent's PromptExtractor. This covers late-flushing agents (e.g. Antigravity)
// whose transcript is empty at TurnEnd but populated by condensation time, so
// prompt.txt is empty and the only remaining source is the live transcript.
func TestResolvePromptsFromLateFlushedTranscript(t *testing.T) {
	t.Parallel()

	// Real agy transcript with a USER_INPUT step wrapping the prompt in a
	// <USER_REQUEST> block, matching agy's on-disk step schema.
	transcript := `{"step_index":0,"source":"SYSTEM","type":"CONVERSATION_HISTORY","content":"boot"}
{"step_index":1,"source":"USER_EXPLICIT","type":"USER_INPUT","content":"<USER_REQUEST>Add a login button</USER_REQUEST>"}
{"step_index":2,"source":"MODEL","type":"PLANNER_RESPONSE","content":"working on it"}
`
	dir := t.TempDir()
	transcriptPath := filepath.Join(dir, "transcript.jsonl")
	require.NoError(t, os.WriteFile(transcriptPath, []byte(transcript), 0o600))

	ag := antigravity.NewAntigravityAgent()
	// Sanity: the real agent must be a PromptExtractor for this fallback to fire.
	_, ok := agent.AsPromptExtractor(ag)
	require.True(t, ok, "antigravity agent must implement PromptExtractor")

	got := resolvePromptsFromLateFlushedTranscript(context.Background(), ag, transcriptPath, 0)
	require.Equal(t, []string{"Add a login button"}, got)
}

// TestResolvePromptsFromLateFlushedTranscript_Guards verifies the helper returns
// nil for the no-op cases callers rely on (empty path, non-extractor agent).
func TestResolvePromptsFromLateFlushedTranscript_Guards(t *testing.T) {
	t.Parallel()

	ag := antigravity.NewAntigravityAgent()

	// Empty path → nil, no extraction attempted.
	require.Nil(t, resolvePromptsFromLateFlushedTranscript(context.Background(), ag, "", 0))

	// Nil agent → nil (AsPromptExtractor returns false).
	require.Nil(t, resolvePromptsFromLateFlushedTranscript(context.Background(), nil, "/nonexistent", 0))
}
