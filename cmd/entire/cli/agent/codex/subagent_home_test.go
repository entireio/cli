package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

func TestSubagentInventoryUnderHome_UsesHistoricalHome(t *testing.T) {
	for _, tree := range []string{"sessions", "archived_sessions"} {
		t.Run(tree, func(t *testing.T) {
			t.Setenv("CODEX_HOME", t.TempDir())
			t.Setenv("ENTIRE_TEST_CODEX_SESSION_DIR", "")
			home := t.TempDir()
			path := writeRollout(t, home, filepath.Join(tree, "rollout-child.jsonl"), "child", []json.RawMessage{
				patchEvent("child.txt"),
				taskEvent("task_started", stringPointer("turn")),
				taskEvent("task_complete", stringPointer("turn")),
				tokenCountEvent(map[string]any{"total_token_usage": map[string]any{"input_tokens": 5, "cached_input_tokens": 2, "output_tokens": 1}}),
			})
			ag := &CodexAgent{}
			for _, declared := range []string{path, ""} {
				result, ok := agent.ExtractWithSubagentInventoryUnderHome(t.Context(), ag, nil, 0,
					[]agent.SubagentReference{{AgentID: "child", DeclaredTranscriptPath: declared}}, home)
				require.True(t, ok)
				require.Equal(t, path, result.Children[0].ResolvedPath)
				require.Equal(t, []string{"child.txt"}, result.Children[0].ModifiedFiles)
				require.True(t, *result.TokenUsage.SubagentTokensComplete)
				require.Equal(t, 3, result.TokenUsage.SubagentTokens.InputTokens)
			}
			require.Empty(t, ag.agentHome, "scoping must not mutate the shared agent")
		})
	}
}

func TestSubagentInventoryUnderHome_RejectsLinksAndOtherLayouts(t *testing.T) {
	t.Parallel()
	testutil.SkipWithoutSymlinks(t)
	home, outside := t.TempDir(), t.TempDir()
	path := writeRollout(t, outside, "rollout-child.jsonl", "child", nil)
	require.NoError(t, os.Symlink(outside, filepath.Join(home, "sessions")))
	ag := &CodexAgent{}
	for _, declared := range []string{filepath.Join(home, "sessions", filepath.Base(path)), ""} {
		result, err := ag.ExtractWithSubagentInventoryUnderHome(t.Context(), nil, 0,
			[]agent.SubagentReference{{AgentID: "child", DeclaredTranscriptPath: declared}}, home)
		require.NoError(t, err)
		require.Empty(t, result.Children[0].ResolvedPath)
		require.False(t, *result.TokenUsage.SubagentTokensComplete)
	}
	other := writeRollout(t, home, "other/rollout-child.jsonl", "child", nil)
	result, err := ag.ExtractWithSubagentInventoryUnderHome(t.Context(), nil, 0,
		[]agent.SubagentReference{{AgentID: "child", DeclaredTranscriptPath: other}}, home)
	require.NoError(t, err)
	require.Empty(t, result.Children[0].ResolvedPath)
}
