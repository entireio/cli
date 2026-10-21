//go:build integration

package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/stretchr/testify/require"
)

// Transcript-token agents keep their usage in the transcript, so a turn that
// changes no files is still covered by the next checkpoint's transcript slice.
// What it used to miss is the session total: the checkpoint-less turn-end path
// skipped SaveStep and recorded nothing. These tests drive a file-changing
// turn, a no-change turn, a commit, then one more turn and commit, and assert
// each turn lands once in the session total and once in a checkpoint.

// appendLines appends JSONL lines to path, creating it if needed.
func appendLines(t *testing.T, path string, lines ...string) {
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

// sessionTokenTotal returns the session-wide token total from session state.
func sessionTokenTotal(t *testing.T, env *TestEnv, sessionID string) *agent.TokenUsage {
	t.Helper()
	state, err := env.GetSessionState(sessionID)
	require.NoError(t, err)
	require.NotNil(t, state, "session state must exist")
	require.NotNil(t, state.TokenUsage, "session state must carry token usage")
	return state.TokenUsage
}

// commitCheckpointTokens commits files with hooks and returns the new
// checkpoint's committed token usage, requiring it differ from previous.
func commitCheckpointTokens(t *testing.T, env *TestEnv, previous, message string, files ...string) (string, *agent.TokenUsage) {
	t.Helper()
	env.GitCommitWithHooks(message, files...)
	checkpointID := env.TryGetLatestCheckpointID()
	require.NotEmpty(t, checkpointID, "%s: expected a checkpoint", message)
	require.NotEqual(t, previous, checkpointID, "%s: expected a new checkpoint", message)
	usage := readCommittedTokenUsage(t, env, checkpointID)
	require.NotNil(t, usage, "%s: checkpoint must carry token usage", message)
	return checkpointID, usage
}

func TestClaudeCodeTokenUsage_TurnWithoutFileChanges(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	session := env.NewSession()
	transcript := session.TranscriptPath

	// turn runs one Claude Code turn: prompt hook, then the turn's transcript
	// lines (one assistant message with the given usage), its files, and Stop.
	n := 0
	turn := func(files []string, input, output int) {
		t.Helper()
		n++
		prompt := fmt.Sprintf("turn %d", n)
		require.NoError(t, env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(session.ID, prompt, transcript))
		appendLines(t, transcript,
			fmt.Sprintf(`{"uuid":"user-%d","type":"user","message":{"content":%q}}`, n, prompt),
			fmt.Sprintf(`{"uuid":"asst-%d","type":"assistant","message":{"id":"msg_%d","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":%d,"output_tokens":%d}}}`, n, n, input, output))
		for _, name := range files {
			env.WriteFile(name, prompt+"\n")
		}
		require.NoError(t, env.SimulateStop(session.ID, transcript))
	}

	turn([]string{"turn1.txt"}, 100, 10)
	turn(nil, 40, 4) // changes nothing

	total := sessionTokenTotal(t, env, session.ID)
	require.Equal(t, 140, total.InputTokens, "session total must include the no-change turn (100+40)")
	require.Equal(t, 14, total.OutputTokens, "session total must include the no-change turn (10+4)")

	cp1, usage1 := commitCheckpointTokens(t, env, "", "Turn 1", "turn1.txt")
	require.Equal(t, 140, usage1.InputTokens, "checkpoint must hold both turns, the no-change turn once")
	require.Equal(t, 14, usage1.OutputTokens, "checkpoint must hold both turns, the no-change turn once")
	require.Equal(t, 140, sessionTokenTotal(t, env, session.ID).InputTokens, "condensation must not change the session total")

	turn([]string{"turn3.txt"}, 7, 1)
	_, usage2 := commitCheckpointTokens(t, env, cp1, "Turn 3", "turn3.txt")
	require.Equal(t, 7, usage2.InputTokens, "checkpoint 2 must hold turn 3 only")
	require.Equal(t, 1, usage2.OutputTokens, "checkpoint 2 must hold turn 3 only")
	total = sessionTokenTotal(t, env, session.ID)
	require.Equal(t, 147, total.InputTokens, "session total = every turn once")
	require.Equal(t, 15, total.OutputTokens, "session total = every turn once")
}

func TestCodexTokenUsage_TurnWithoutFileChanges(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	const sessionID = "codex-nochange-session"
	rollout := filepath.Join(env.RepoDir, ".entire", "tmp", "codex-nochange-rollout.jsonl")
	appendLines(t, rollout,
		`{"timestamp":"2026-01-01T00:00:00Z","type":"session_meta","payload":{"id":"`+sessionID+`","thread_source":"user","cwd":"`+env.RepoDir+`"}}`)
	hook := codexHooker(t, env.RepoDir, sessionID, rollout)

	// Codex reports cumulative totals; each turn appends the new cumulative
	// snapshot, so the per-turn delta is the difference from the previous one.
	var cumInput, cumCached, cumOutput int
	n := 0
	turn := func(files []string, input, cached, output int) {
		t.Helper()
		n++
		prompt := fmt.Sprintf("turn %d", n)
		hook("user-prompt-submit", map[string]any{"prompt": prompt, "hook_event_name": "UserPromptSubmit"})
		cumInput += input
		cumCached += cached
		cumOutput += output
		appendLines(t, rollout,
			fmt.Sprintf(`{"timestamp":"2026-01-01T00:0%d:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":%q}]}}`, n, prompt),
			fmt.Sprintf(`{"timestamp":"2026-01-01T00:0%d:02Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":%d,"cached_input_tokens":%d,"output_tokens":%d}}}}`, n, cumInput, cumCached, cumOutput),
			fmt.Sprintf(`{"timestamp":"2026-01-01T00:0%d:03Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}}`, n))
		for i, name := range files {
			env.WriteFile(name, prompt+"\n")
			applyPatchHook(hook, fmt.Sprintf("call_%d_%d", n, i), "*** Begin Patch\n*** Add File: "+name+"\n+"+prompt+"\n*** End Patch\n")
		}
		hook("stop", map[string]any{"hook_event_name": "Stop"})
	}

	// Fresh input is input minus cached: turn 1 = 80, turn 2 = 30, turn 3 = 5.
	turn([]string{"turn1.txt"}, 100, 20, 10)
	turn(nil, 40, 10, 4) // changes nothing

	total := sessionTokenTotal(t, env, sessionID)
	require.Equal(t, 110, total.InputTokens, "session total must include the no-change turn (80+30)")
	require.Equal(t, 30, total.CacheReadTokens, "session total must include the no-change turn (20+10)")
	require.Equal(t, 14, total.OutputTokens, "session total must include the no-change turn (10+4)")

	cp1, usage1 := commitCheckpointTokens(t, env, "", "Turn 1", "turn1.txt")
	require.Equal(t, 110, usage1.InputTokens, "checkpoint must hold both turns, the no-change turn once")
	require.Equal(t, 14, usage1.OutputTokens, "checkpoint must hold both turns, the no-change turn once")
	require.Equal(t, 110, sessionTokenTotal(t, env, sessionID).InputTokens, "condensation must not change the session total")

	turn([]string{"turn3.txt"}, 5, 0, 1)
	_, usage2 := commitCheckpointTokens(t, env, cp1, "Turn 3", "turn3.txt")
	require.Equal(t, 5, usage2.InputTokens, "checkpoint 2 must hold turn 3 only")
	require.Equal(t, 1, usage2.OutputTokens, "checkpoint 2 must hold turn 3 only")
	total = sessionTokenTotal(t, env, sessionID)
	require.Equal(t, 115, total.InputTokens, "session total = every turn once")
	require.Equal(t, 15, total.OutputTokens, "session total = every turn once")
}

// requireTurnTokens asserts the session total and the pending checkpoint
// window both hold exactly the given usage.
func requireTurnTokens(t *testing.T, env *TestEnv, sessionID string, input, output int, msg string) {
	t.Helper()
	state, err := env.GetSessionState(sessionID)
	require.NoError(t, err)
	require.NotNil(t, state, "session state must exist")
	require.NotNil(t, state.TokenUsage, "%s: session total", msg)
	require.Equal(t, input, state.TokenUsage.InputTokens, "%s: session total input", msg)
	require.Equal(t, output, state.TokenUsage.OutputTokens, "%s: session total output", msg)
	require.NotNil(t, state.CheckpointTokenUsage, "%s: pending checkpoint usage", msg)
	require.Equal(t, input, state.CheckpointTokenUsage.InputTokens, "%s: pending checkpoint input", msg)
	require.Equal(t, output, state.CheckpointTokenUsage.OutputTokens, "%s: pending checkpoint output", msg)
}

// runConcurrently runs each fn at once and returns their errors.
func runConcurrently(fns ...func() error) []error {
	errs := make([]error, len(fns))
	var wg sync.WaitGroup
	for i, fn := range fns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = fn()
		}()
	}
	wg.Wait()
	return errs
}

// TestClaudeCodeTokenUsage_RepeatedStopCountsNoChangeTurnOnce pins that a
// no-change turn's tokens are recorded by the Stop that ends the turn and by
// no other. A retried Stop runs after the pre-prompt state is gone, so it
// resolves its transcript offset from the session instead and would re-add
// every earlier turn; concurrent Stops would each add the turn.
func TestClaudeCodeTokenUsage_RepeatedStopCountsNoChangeTurnOnce(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	session := env.NewSession()
	transcript := session.TranscriptPath

	n := 0
	startTurn := func(input, output int) {
		t.Helper()
		n++
		prompt := fmt.Sprintf("turn %d", n)
		require.NoError(t, env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(session.ID, prompt, transcript))
		appendLines(t, transcript,
			fmt.Sprintf(`{"uuid":"user-%d","type":"user","message":{"content":%q}}`, n, prompt),
			fmt.Sprintf(`{"uuid":"asst-%d","type":"assistant","message":{"id":"msg_%d","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":%d,"output_tokens":%d}}}`, n, n, input, output))
	}
	stop := func() error { return env.SimulateStop(session.ID, transcript) }

	startTurn(100, 10)
	require.NoError(t, stop())
	startTurn(40, 4)
	require.NoError(t, stop())
	requireTurnTokens(t, env, session.ID, 140, 14, "after two no-change turns")

	require.NoError(t, stop(), "a repeated Stop must still succeed")
	requireTurnTokens(t, env, session.ID, 140, 14, "a repeated Stop must add nothing")

	startTurn(7, 1)
	for i, err := range runConcurrently(stop, stop) {
		require.NoError(t, err, "concurrent Stop %d", i)
	}
	requireTurnTokens(t, env, session.ID, 147, 15, "concurrent Stops must record the turn once")
}
