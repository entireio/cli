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

	env.GitCommitWithHooks("Add feature", "feature.go")

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
	env.GitCommitWithHooks("Turn 1", "turn1.go")
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
	env.GitCommitWithHooks("Turn 2", "turn2.go")
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

// TestCursorTokenUsage_TurnWithoutFileChanges covers the checkpoint-less
// turn-end path: a Stop that changes no files skips SaveStep, so the stop
// payload's tokens have to be recorded there or they are lost from both the
// session total and the next checkpoint (Cursor's transcript carries no usage
// for condensation to recompute from).
func TestCursorTokenUsage_TurnWithoutFileChanges(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	env.InitEntireWithAgent(agent.AgentNameCursor)

	cursorProjectDir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(cursorProjectDir); err == nil {
		cursorProjectDir = resolved
	}

	const conversationID = "cursor-nochange-session"

	transcriptDir := filepath.Join(cursorProjectDir, conversationID)
	require.NoError(t, os.MkdirAll(transcriptDir, 0o755))
	transcriptPath := filepath.Join(transcriptDir, conversationID+".jsonl")

	hook := func(name string, extra map[string]any) {
		t.Helper()
		input := map[string]any{"conversation_id": conversationID, "transcript_path": transcriptPath}
		for k, v := range extra {
			input[k] = v
		}
		runCursorHook(t, env, cursorProjectDir, name, input)
	}
	// turn appends a prompt/answer pair, runs before-submit-prompt, writes
	// files (none for a read-only turn), and stops with the given counts.
	// Cursor's input_tokens includes cache reads and writes.
	turn := func(prompt string, files []string, input, output, cacheRead, cacheWrite int) {
		t.Helper()
		f, err := os.OpenFile(transcriptPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		require.NoError(t, err)
		_, werr := f.WriteString(`{"type":"user","text":"` + prompt + `"}` + "\n")
		require.NoError(t, f.Close())
		require.NoError(t, werr)
		hook("before-submit-prompt", map[string]any{"prompt": prompt})
		for _, name := range files {
			env.WriteFile(name, "package main\n// "+prompt+"\n")
		}
		hook("stop", map[string]any{
			"model":              "cursor-default",
			"loop_count":         1,
			"input_tokens":       input,
			"output_tokens":      output,
			"cache_read_tokens":  cacheRead,
			"cache_write_tokens": cacheWrite,
		})
	}
	sessionTokens := func() *agent.TokenUsage {
		t.Helper()
		state, err := env.GetSessionState(conversationID)
		require.NoError(t, err)
		require.NotNil(t, state, "session state must exist")
		require.NotNil(t, state.TokenUsage, "session state must carry token usage")
		return state.TokenUsage
	}

	hook("session-start", map[string]any{"model": "cursor-default"})

	// Turn 1 writes a file: fresh input 200, output 50, cache 4000/800.
	turn("turn one", []string{"turn1.go"}, 5000, 50, 4000, 800)
	// Turn 2 changes nothing: fresh input 100, output 7, cache 300/20.
	turn("turn two", nil, 420, 7, 300, 20)

	total := sessionTokens()
	require.Equal(t, 300, total.InputTokens, "session total must include the no-change turn (200+100)")
	require.Equal(t, 57, total.OutputTokens, "session total must include the no-change turn (50+7)")
	require.Equal(t, 4300, total.CacheReadTokens)
	require.Equal(t, 820, total.CacheCreationTokens)

	env.GitCommitWithHooks("Turn 1", "turn1.go")
	checkpoint1 := env.TryGetLatestCheckpointID()
	require.NotEmpty(t, checkpoint1, "expected a checkpoint after the commit")

	cp1 := readCommittedTokenUsage(t, env, checkpoint1)
	require.NotNil(t, cp1, "checkpoint must carry token usage")
	require.Equal(t, 300, cp1.InputTokens, "checkpoint must hold both turns, the no-change turn once")
	require.Equal(t, 57, cp1.OutputTokens, "checkpoint must hold both turns, the no-change turn once")
	require.Equal(t, 4300, cp1.CacheReadTokens)
	require.Equal(t, 820, cp1.CacheCreationTokens)
	require.Equal(t, 300, sessionTokens().InputTokens, "condensation must not change the session total")

	// Turn 3 writes a file: the next checkpoint must not repeat turn 2.
	turn("turn three", []string{"turn3.go"}, 1000, 9, 0, 0)
	env.GitCommitWithHooks("Turn 3", "turn3.go")
	checkpoint2 := env.TryGetLatestCheckpointID()
	require.NotEmpty(t, checkpoint2)
	require.NotEqual(t, checkpoint1, checkpoint2, "turn 3 must produce a distinct checkpoint")

	cp2 := readCommittedTokenUsage(t, env, checkpoint2)
	require.NotNil(t, cp2, "checkpoint 2 must carry turn 3 token usage")
	require.Equal(t, 1000, cp2.InputTokens, "checkpoint 2 must hold turn 3 only")
	require.Equal(t, 9, cp2.OutputTokens, "checkpoint 2 must hold turn 3 only")
	require.Equal(t, 1300, sessionTokens().InputTokens, "session total = every turn once")
}

func readCommittedTokenUsage(t *testing.T, env *TestEnv, checkpointID string) *agent.TokenUsage {
	t.Helper()
	content, found := env.ReadFileFromBranch(paths.MetadataBranchName, SessionMetadataPath(checkpointID))
	require.Truef(t, found, "session metadata should exist for checkpoint %s", checkpointID)
	var meta checkpoint.Metadata
	require.NoError(t, json.Unmarshal([]byte(content), &meta))
	return meta.TokenUsage
}
