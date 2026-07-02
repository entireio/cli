//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"

	"github.com/stretchr/testify/require"
)

func runCursorHook(t *testing.T, env *TestEnv, cursorProjectDir, hookName string, input map[string]any) {
	t.Helper()

	inputJSON, err := json.Marshal(input)
	require.NoError(t, err)

	cmd := execx.NonInteractive(context.Background(), getTestBinary(), "hooks", "cursor", hookName)
	cmd.Dir = env.RepoDir
	cmd.Stdin = bytes.NewReader(inputJSON)
	cmd.Env = append(env.cliEnv(), "ENTIRE_TEST_CURSOR_PROJECT_DIR="+cursorProjectDir)

	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "cursor %s hook failed\ninput: %s\noutput: %s", hookName, inputJSON, out)
	t.Logf("cursor %s output: %s", hookName, out)
}

func TestCursorTokenUsage_SurvivesCondensation(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	env.InitEntireWithAgent(agent.AgentNameCursor)

	cursorProjectDir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(cursorProjectDir); err == nil {
		cursorProjectDir = resolved
	}

	const conversationID = "cursor-tok-session"

	transcriptDir := filepath.Join(cursorProjectDir, conversationID)
	require.NoError(t, os.MkdirAll(transcriptDir, 0o755))
	transcriptPath := filepath.Join(transcriptDir, conversationID+".jsonl")
	require.NoError(t, os.WriteFile(transcriptPath,
		[]byte(`{"type":"user","text":"add a feature"}`+"\n"+
			`{"type":"assistant","text":"done"}`+"\n"), 0o600))

	runCursorHook(t, env, cursorProjectDir, "session-start", map[string]any{
		"conversation_id": conversationID,
		"transcript_path": transcriptPath,
		"model":           "cursor-default",
	})

	runCursorHook(t, env, cursorProjectDir, "before-submit-prompt", map[string]any{
		"conversation_id": conversationID,
		"transcript_path": transcriptPath,
		"prompt":          "add a feature",
	})

	env.WriteFile("feature.go", "package main\n// new feature\n")

	runCursorHook(t, env, cursorProjectDir, "stop", map[string]any{
		"conversation_id":    conversationID,
		"transcript_path":    transcriptPath,
		"model":              "cursor-default",
		"loop_count":         1,
		"input_tokens":       5000,
		"output_tokens":      50,
		"cache_read_tokens":  4000,
		"cache_write_tokens": 800,
	})

	statePath := filepath.Join(env.RepoDir, ".git", "entire-sessions", conversationID+".json")
	stateBytes, err := os.ReadFile(statePath)
	require.NoError(t, err, "session state file should exist after stop")

	var liveState strategy.SessionState
	require.NoError(t, json.Unmarshal(stateBytes, &liveState))
	require.NotNil(t, liveState.TokenUsage, "PRECONDITION: stop hook tokens must reach live session state")
	require.Equal(t, 200, liveState.TokenUsage.InputTokens, "fresh input = 5000-4000-800")
	require.Equal(t, 50, liveState.TokenUsage.OutputTokens)
	require.Equal(t, 4000, liveState.TokenUsage.CacheReadTokens)
	require.Equal(t, 800, liveState.TokenUsage.CacheCreationTokens)

	env.GitCommitWithShadowHooks("Add feature", "feature.go")

	checkpointID := env.TryGetLatestCheckpointID()
	require.NotEmpty(t, checkpointID, "expected a condensed checkpoint after commit")

	metadataPath := SessionMetadataPath(checkpointID)
	content, found := env.ReadFileFromBranch(paths.MetadataBranchName, metadataPath)
	require.True(t, found, "session metadata should exist at %s", metadataPath)

	var meta checkpoint.Metadata
	require.NoError(t, json.Unmarshal([]byte(content), &meta))

	require.NotNilf(t, meta.TokenUsage,
		"committed checkpoint metadata dropped Cursor's hook-provided token usage "+
			"(condensation recomputed TokenUsage from a transcript Cursor never populates)\nmetadata: %s",
		content)
	require.Equal(t, 200, meta.TokenUsage.InputTokens, "committed InputTokens must match the stop hook")
	require.Equal(t, 50, meta.TokenUsage.OutputTokens, "committed OutputTokens must match the stop hook")
	require.Equal(t, 4000, meta.TokenUsage.CacheReadTokens)
	require.Equal(t, 800, meta.TokenUsage.CacheCreationTokens)
}

func TestCursorTokenUsage_PerCheckpointScoping(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	env.InitEntireWithAgent(agent.AgentNameCursor)

	cursorProjectDir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(cursorProjectDir); err == nil {
		cursorProjectDir = resolved
	}

	const conversationID = "cursor-scope-session"

	transcriptDir := filepath.Join(cursorProjectDir, conversationID)
	require.NoError(t, os.MkdirAll(transcriptDir, 0o755))
	transcriptPath := filepath.Join(transcriptDir, conversationID+".jsonl")

	appendTranscript := func(lines string) {
		t.Helper()
		f, err := os.OpenFile(transcriptPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		require.NoError(t, err)
		_, werr := f.WriteString(lines)
		require.NoError(t, f.Close())
		require.NoError(t, werr)
	}

	appendTranscript(`{"type":"user","text":"turn one"}` + "\n" + `{"type":"assistant","text":"ok"}` + "\n")

	runCursorHook(t, env, cursorProjectDir, "session-start", map[string]any{
		"conversation_id": conversationID,
		"transcript_path": transcriptPath,
		"model":           "cursor-default",
	})

	runCursorHook(t, env, cursorProjectDir, "before-submit-prompt", map[string]any{
		"conversation_id": conversationID,
		"transcript_path": transcriptPath,
		"prompt":          "turn one",
	})
	env.WriteFile("turn1.go", "package main\n// turn 1\n")
	runCursorHook(t, env, cursorProjectDir, "stop", map[string]any{
		"conversation_id":    conversationID,
		"transcript_path":    transcriptPath,
		"model":              "cursor-default",
		"loop_count":         1,
		"input_tokens":       5000,
		"output_tokens":      50,
		"cache_read_tokens":  4000,
		"cache_write_tokens": 800,
	})
	env.GitCommitWithShadowHooks("Turn 1", "turn1.go")
	checkpoint1 := env.TryGetLatestCheckpointID()
	require.NotEmpty(t, checkpoint1, "expected a checkpoint after turn 1 commit")

	appendTranscript(`{"type":"user","text":"turn two"}` + "\n" + `{"type":"assistant","text":"ok"}` + "\n")
	runCursorHook(t, env, cursorProjectDir, "before-submit-prompt", map[string]any{
		"conversation_id": conversationID,
		"transcript_path": transcriptPath,
		"prompt":          "turn two",
	})
	env.WriteFile("turn2.go", "package main\n// turn 2\n")
	runCursorHook(t, env, cursorProjectDir, "stop", map[string]any{
		"conversation_id":    conversationID,
		"transcript_path":    transcriptPath,
		"model":              "cursor-default",
		"loop_count":         1,
		"input_tokens":       3000,
		"output_tokens":      30,
		"cache_read_tokens":  2000,
		"cache_write_tokens": 500,
	})
	env.GitCommitWithShadowHooks("Turn 2", "turn2.go")
	checkpoint2 := env.TryGetLatestCheckpointID()
	require.NotEmpty(t, checkpoint2, "expected a checkpoint after turn 2 commit")
	require.NotEqual(t, checkpoint1, checkpoint2, "turn 2 must produce a distinct checkpoint")

	cp1 := readCommittedTokenUsage(t, env, checkpoint1)
	require.NotNil(t, cp1, "checkpoint 1 must carry turn 1 token usage")
	require.Equal(t, 200, cp1.InputTokens, "checkpoint 1 InputTokens = turn 1 only")
	require.Equal(t, 50, cp1.OutputTokens, "checkpoint 1 OutputTokens = turn 1 only")

	cp2 := readCommittedTokenUsage(t, env, checkpoint2)
	require.NotNil(t, cp2, "checkpoint 2 must carry turn 2 token usage")
	require.Equal(t, 500, cp2.InputTokens,
		"checkpoint 2 InputTokens must be turn 2 only (500), not the cumulative session total (700)")
	require.Equal(t, 30, cp2.OutputTokens,
		"checkpoint 2 OutputTokens must be turn 2 only (30), not the cumulative session total (80)")
}

func readCommittedTokenUsage(t *testing.T, env *TestEnv, checkpointID string) *agent.TokenUsage {
	t.Helper()
	content, found := env.ReadFileFromBranch(paths.MetadataBranchName, SessionMetadataPath(checkpointID))
	require.Truef(t, found, "session metadata should exist for checkpoint %s", checkpointID)
	var meta checkpoint.Metadata
	require.NoError(t, json.Unmarshal([]byte(content), &meta))
	return meta.TokenUsage
}
