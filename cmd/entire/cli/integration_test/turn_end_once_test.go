//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/execx"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A Stop hook can reach Entire more than once for the same turn: a retried
// hook, a duplicate delivery, or two hook processes racing. Each one used to
// run SaveStep again, adding the turn's tokens to the session total and the
// pending checkpoint window a second time and counting a second step. These
// tests drive a file-changing turn followed by a repeated (or concurrent)
// Stop and assert the turn is counted once.

// writeTurnEndOnceTranscript appends JSONL lines to path, creating it.
func writeTurnEndOnceTranscript(t *testing.T, path string, lines ...string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	for _, line := range lines {
		if _, err := f.WriteString(line + "\n"); err != nil {
			_ = f.Close()
			require.NoError(t, err)
		}
	}
	require.NoError(t, f.Close())
}

// claudeTurnLines returns one Claude Code turn's transcript lines: the prompt
// and an assistant message carrying the given usage.
func claudeTurnLines(n, input, output int) []string {
	return []string{
		fmt.Sprintf(`{"uuid":"user-%d","type":"user","message":{"content":"turn %d"}}`, n, n),
		fmt.Sprintf(`{"uuid":"asst-%d","type":"assistant","message":{"id":"msg_%d","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":%d,"output_tokens":%d}}}`, n, n, input, output),
	}
}

// requireTurnCountedOnce asserts the session's total, its pending checkpoint
// window, and its step count.
func requireTurnCountedOnce(t *testing.T, env *TestEnv, sessionID string, input, output, steps int) {
	t.Helper()
	state, err := env.GetSessionState(sessionID)
	require.NoError(t, err)
	require.NotNil(t, state, "session state must exist")
	require.NotNil(t, state.TokenUsage, "session total must carry token usage")
	require.NotNil(t, state.CheckpointTokenUsage, "pending checkpoint window must carry token usage")
	assert.Equal(t, input, state.TokenUsage.InputTokens, "session total input")
	assert.Equal(t, output, state.TokenUsage.OutputTokens, "session total output")
	assert.Equal(t, input, state.CheckpointTokenUsage.InputTokens, "checkpoint window input")
	assert.Equal(t, output, state.CheckpointTokenUsage.OutputTokens, "checkpoint window output")
	assert.Equal(t, steps, state.StepCount, "step count")
}

func TestClaudeCode_RepeatedStopCountsTurnOnce(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	session := env.NewSession()
	transcript := session.TranscriptPath

	require.NoError(t, env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(session.ID, "turn 1", transcript))
	writeTurnEndOnceTranscript(t, transcript, claudeTurnLines(1, 100, 10)...)
	env.WriteFile("turn1.txt", "turn 1\n")
	require.NoError(t, env.SimulateStop(session.ID, transcript))
	require.NoError(t, env.SimulateStop(session.ID, transcript), "repeated Stop")

	requireTurnCountedOnce(t, env, session.ID, 100, 10, 1)

	env.GitCommitWithHooks("Turn 1", "turn1.txt")
	cp1 := env.TryGetLatestCheckpointID()
	require.NotEmpty(t, cp1)
	usage := readCommittedTokenUsage(t, env, cp1)
	require.NotNil(t, usage)
	require.Equal(t, 100, usage.InputTokens, "checkpoint must hold the turn once")
	require.Equal(t, 10, usage.OutputTokens, "checkpoint must hold the turn once")

	// The next turn is a new turn and counts normally.
	require.NoError(t, env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(session.ID, "turn 2", transcript))
	writeTurnEndOnceTranscript(t, transcript, claudeTurnLines(2, 7, 1)...)
	env.WriteFile("turn2.txt", "turn 2\n")
	require.NoError(t, env.SimulateStop(session.ID, transcript))

	state, err := env.GetSessionState(session.ID)
	require.NoError(t, err)
	require.Equal(t, 107, state.TokenUsage.InputTokens, "session total = both turns once")
	require.Equal(t, 7, state.CheckpointTokenUsage.InputTokens, "window = turn 2 only")
	require.Equal(t, 1, state.StepCount, "turn 2 is the window's one step")
}

func TestClaudeCode_ConcurrentStopsCountTurnOnce(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	session := env.NewSession()
	transcript := session.TranscriptPath

	require.NoError(t, env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(session.ID, "turn 1", transcript))
	writeTurnEndOnceTranscript(t, transcript, claudeTurnLines(1, 100, 10)...)
	env.WriteFile("turn1.txt", "turn 1\n")

	runConcurrently(t, 2, func() error { return env.SimulateStop(session.ID, transcript) })

	requireTurnCountedOnce(t, env, session.ID, 100, 10, 1)
}

// Without a TurnStart (an agent whose hooks never report prompt submission,
// or the first Stop after Entire was enabled mid-session) every Stop is the
// only evidence of a turn, so each one still records a step.
func TestClaudeCode_StopsWithoutTurnStartStillCountEachTurn(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	session := env.NewSession()
	transcript := session.TranscriptPath

	writeTurnEndOnceTranscript(t, transcript, claudeTurnLines(1, 100, 10)...)
	env.WriteFile("turn1.txt", "turn 1\n")
	require.NoError(t, env.SimulateStop(session.ID, transcript))

	state, err := env.GetSessionState(session.ID)
	require.NoError(t, err)
	require.NotNil(t, state)
	require.Equal(t, 1, state.StepCount, "first Stop after a mid-session enable records its step")
	require.NotNil(t, state.TokenUsage)
	require.Equal(t, 100, state.TokenUsage.InputTokens)

	writeTurnEndOnceTranscript(t, transcript, claudeTurnLines(2, 7, 1)...)
	env.WriteFile("turn2.txt", "turn 2\n")
	require.NoError(t, env.SimulateStop(session.ID, transcript))

	state, err = env.GetSessionState(session.ID)
	require.NoError(t, err)
	require.Equal(t, 2, state.StepCount, "a second turn without TurnStart records its own step")

	// A repeat of that Stop has grown nothing, so it is not a new turn.
	require.NoError(t, env.SimulateStop(session.ID, transcript))
	state, err = env.GetSessionState(session.ID)
	require.NoError(t, err)
	require.Equal(t, 2, state.StepCount, "a repeated Stop without TurnStart is not counted again")
}

// A turn the agent starts on its own after a prompted turn (a background
// notification, or a prompt hook that never reached Entire) has no TurnStart
// of its own. It must still count, not be mistaken for a repeat of the
// prompted turn's Stop.
func TestClaudeCode_UnpromptedTurnAfterPromptedTurnCounts(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	session := env.NewSession()
	transcript := session.TranscriptPath

	require.NoError(t, env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(session.ID, "turn 1", transcript))
	writeTurnEndOnceTranscript(t, transcript, claudeTurnLines(1, 100, 10)...)
	env.WriteFile("turn1.txt", "turn 1\n")
	require.NoError(t, env.SimulateStop(session.ID, transcript))

	writeTurnEndOnceTranscript(t, transcript, claudeTurnLines(2, 7, 1)...)
	env.WriteFile("turn2.txt", "turn 2\n")
	require.NoError(t, env.SimulateStop(session.ID, transcript))

	state, err := env.GetSessionState(session.ID)
	require.NoError(t, err)
	require.NotNil(t, state)
	require.Equal(t, 2, state.StepCount, "the unprompted turn records its own step")
	require.NotNil(t, state.TokenUsage)
	require.GreaterOrEqual(t, state.TokenUsage.InputTokens, 107, "the unprompted turn's tokens are counted")
}

func TestCursor_RepeatedStopCountsTurnOnce(t *testing.T) {
	t.Parallel()

	for _, concurrent := range []bool{false, true} {
		t.Run(fmt.Sprintf("concurrent=%v", concurrent), func(t *testing.T) {
			t.Parallel()

			env := NewFeatureBranchEnv(t)
			env.InitEntireWithAgent(agent.AgentNameCursor)

			cursorProjectDir := t.TempDir()
			if resolved, err := filepath.EvalSymlinks(cursorProjectDir); err == nil {
				cursorProjectDir = resolved
			}
			const conversationID = "cursor-turn-end-once"
			transcript := filepath.Join(cursorProjectDir, conversationID, conversationID+".jsonl")
			writeTurnEndOnceTranscript(t, transcript,
				`{"type":"user","text":"add a feature"}`,
				`{"type":"assistant","text":"done"}`)

			runCursorHook(t, env, cursorProjectDir, "session-start", map[string]any{
				"conversation_id": conversationID,
				"transcript_path": transcript,
			})
			runCursorHook(t, env, cursorProjectDir, "before-submit-prompt", map[string]any{
				"conversation_id": conversationID,
				"transcript_path": transcript,
				"prompt":          "add a feature",
			})
			env.WriteFile("feature.go", "package main\n")

			stop := map[string]any{
				"conversation_id":    conversationID,
				"transcript_path":    transcript,
				"loop_count":         1,
				"input_tokens":       5000,
				"output_tokens":      50,
				"cache_read_tokens":  4000,
				"cache_write_tokens": 800,
			}
			if concurrent {
				runConcurrently(t, 2, func() error {
					return runCursorHookErr(env, cursorProjectDir, "stop", stop)
				})
			} else {
				runCursorHook(t, env, cursorProjectDir, "stop", stop)
				runCursorHook(t, env, cursorProjectDir, "stop", stop)
			}

			// Fresh input = 5000 - 4000 cache read - 800 cache write.
			requireTurnCountedOnce(t, env, conversationID, 200, 50, 1)

			env.GitCommitWithHooks("Add feature", "feature.go")
			cp := env.TryGetLatestCheckpointID()
			require.NotEmpty(t, cp)
			usage := readCommittedTokenUsage(t, env, cp)
			require.NotNil(t, usage)
			require.Equal(t, 200, usage.InputTokens, "checkpoint must hold the turn once")
			require.Equal(t, 50, usage.OutputTokens, "checkpoint must hold the turn once")
		})
	}
}

// runConcurrently runs fn n times at once and requires every call succeed.
func runConcurrently(t *testing.T, n int, fn func() error) {
	t.Helper()
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = fn()
		}()
	}
	wg.Wait()
	for i, err := range errs {
		require.NoErrorf(t, err, "concurrent call %d", i)
	}
}

// runCursorHookErr is runCursorHook for goroutines: it reports failure as an
// error instead of failing the test from another goroutine.
func runCursorHookErr(env *TestEnv, cursorProjectDir, hookName string, input map[string]any) error {
	inputJSON, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("marshal %s input: %w", hookName, err)
	}
	cmd := execx.NonInteractive(context.Background(), getTestBinary(), "hooks", "cursor", hookName)
	cmd.Dir = env.RepoDir
	cmd.Stdin = bytes.NewReader(inputJSON)
	cmd.Env = append(env.cliEnv(), "ENTIRE_TEST_CURSOR_PROJECT_DIR="+cursorProjectDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("cursor %s hook: %w\noutput: %s", hookName, err, out)
	}
	return nil
}
