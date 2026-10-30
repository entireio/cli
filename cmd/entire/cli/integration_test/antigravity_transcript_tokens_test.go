//go:build integration

package integration

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/stretchr/testify/require"
)

// Since 1.4, agy writes each model call's usage on its PLANNER_RESPONSE
// transcript step, which is complete when Stop fires. The statusline snapshots
// are not: agy writes the one carrying a turn's last call just after Stop, and
// its total_input_tokens is how full the context is, not a running total. The
// per-call values and snapshots below are from a real agy 1.4.2 session, where
// the statusline path recorded 0 for the first two turns and 93 input tokens
// for the third.
func TestAntigravity_TokensComeFromTranscriptSteps(t *testing.T) {
	t.Parallel()
	env := newAntigravityEnv(t)

	statusDir := t.TempDir()
	conversationID := "antigravity-transcript-tokens-it"
	extraEnv := []string{
		"ENTIRE_ANTIGRAVITY_STATUS_DIR=" + statusDir,
		"ENTIRE_ANTIGRAVITY_CONFIG_DIR=" + t.TempDir(),
	}
	snapshotPath := filepath.Join(statusDir, conversationID+".jsonl")
	snapshot := func(ts string, totalInput, totalOutput int) {
		appendLines(t, snapshotPath, fmt.Sprintf(
			`{"ts":"2026-10-19T22:%s","conversation_id":%q,"context_window":{"total_input_tokens":%d,"total_output_tokens":%d}}`,
			ts, conversationID, totalInput, totalOutput))
	}

	transcriptPath := filepath.Join(antigravityBrainDir(env.RepoDir), conversationID,
		".system_generated", "logs", "transcript_full.jsonl")
	common := map[string]any{
		"conversationId":        conversationID,
		"workspacePaths":        []string{env.RepoDir},
		"transcriptPath":        transcriptPath,
		"artifactDirectoryPath": filepath.Join(env.RepoDir, ".gemini", "antigravity-cli", "artifacts"),
	}
	hook := func(name string, fields map[string]any) {
		t.Helper()
		require.NoError(t, runAntigravityHookWithEnv(t, env.RepoDir, name, mergeMaps(common, fields), extraEnv), name)
	}

	step := 0
	turn := func(file string, input, output int, snapshotBeforeStop func()) {
		t.Helper()
		hook("pre-invocation", map[string]any{"invocationNum": 0, "initialNumSteps": step})
		appendLines(t, transcriptPath, fmt.Sprintf(
			`{"step_index":%d,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","content":"<USER_REQUEST>\nturn %d\n</USER_REQUEST>"}`,
			step, step/2+1))
		if file != "" {
			env.WriteFile(file, "contents\n")
			hook("pre-tool-use", map[string]any{
				"toolCall": map[string]any{"name": "write_to_file", "args": map[string]any{"TargetFile": file}},
				"stepIdx":  step + 1,
			})
		}
		appendLines(t, transcriptPath, fmt.Sprintf(
			`{"step_index":%d,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","content":"ok","input_tokens":%d,"cache_read_tokens":0,"output_tokens":%d}`,
			step+1, input, output))
		step += 2
		if snapshotBeforeStop != nil {
			snapshotBeforeStop()
		}
		hook("stop", map[string]any{"executionNum": 0, "terminationReason": "NO_TOOL_CALL", "error": "", "fullyIdle": true})
	}

	snapshot("40:55.13894Z", 0, 0)
	turn("", 11934, 29, nil)
	snapshot("40:57.279103Z", 16956, 29) // lands after Stop
	turn("", 12046, 20, nil)
	snapshot("41:29.800119Z", 17059, 49) // lands after Stop
	turn("feature.txt", 12149, 22, func() { snapshot("42:53.50011Z", 17152, 71) })

	want := &agent.TokenUsage{InputTokens: 36129, OutputTokens: 71, APICallCount: 3}
	require.Equal(t, want, sessionTokenTotal(t, env, conversationID))

	// No commit came before, so the checkpoint holds all three turns.
	checkpointID, usage := commitCheckpointTokens(t, env, "", "Add feature", "feature.txt")
	require.Equal(t, want.InputTokens, usage.InputTokens, "checkpoint %s", checkpointID)
	require.Equal(t, want.OutputTokens, usage.OutputTokens, "checkpoint %s", checkpointID)
	require.Equal(t, want.APICallCount, usage.APICallCount, "checkpoint %s", checkpointID)
}
