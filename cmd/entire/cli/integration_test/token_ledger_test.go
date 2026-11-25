//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/stretchr/testify/require"
)

// agentCommit commits files mid-turn through the hooks as the agent does, so
// the session condenses while its turn runs.
func (r *tokenTurnRun) agentCommit(files ...string) {
	r.t.Helper()
	r.env.GitCommitWithHooksAsAgent(fmt.Sprintf("agent commit in turn %d", r.turn), files...)
	r.record(fmt.Sprintf("agent commit in turn %d", r.turn))
}

// repeatLine writes a line of an assistant message the transcript already
// holds, as Claude Code does for each content block of one message: the same
// message ID and usage, which must count once.
func (r *tokenTurnRun) repeatLine(messageID string, tokens int) {
	r.t.Helper()
	appendUsageMessage(r.s, messageID, tokens)
	require.NoError(r.t, r.s.TranscriptBuilder.WriteToFile(r.s.TranscriptPath))
}

// rewind replaces the transcript with its first lines, as restoring an older
// copy of the session does. Tokens already counted stay counted.
func (r *tokenTurnRun) rewind(keep int) {
	r.t.Helper()
	r.s.TranscriptBuilder.messages = r.s.TranscriptBuilder.messages[:keep]
	require.NoError(r.t, os.Remove(r.s.TranscriptPath))
	require.NoError(r.t, r.s.TranscriptBuilder.WriteToFile(r.s.TranscriptPath))
}

// TestTokenLedger_ClaudeCode_EachCallCountedOnce runs the shapes that the
// per-turn accounting counted twice or dropped. Every one must leave the
// session total equal to the transcript's usage, and the checkpoints summing
// to it.
func TestTokenLedger_ClaudeCode_EachCallCountedOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		run  func(r *tokenTurnRun)
	}{
		{
			name: "a repeated Stop on a turn that changed files",
			run: func(r *tokenTurnRun) {
				r.start(10, 0, "a.txt")
				r.stop()
				r.stop()
				r.checkTotal()
				r.start(20, 0, "b.txt")
				r.stop()
				r.stop()
				r.checkTotal()
				r.commit("a.txt", "b.txt")
			},
		},
		{
			name: "turns that change nothing between commits",
			run: func(r *tokenTurnRun) {
				r.start(10, 0, "a.txt")
				r.stop()
				r.start(20, 0)
				r.stop()
				r.commit("a.txt")
				r.start(30, 0)
				r.stop()
				r.start(40, 0, "b.txt")
				r.stop()
				r.checkTotal()
				r.commit("b.txt")
			},
		},
		{
			name: "an agent commit in the middle of a turn",
			run: func(r *tokenTurnRun) {
				r.start(10, 0, "a.txt")
				r.agentCommit("a.txt")
				r.more(5)
				r.stop()
				r.checkTotal()
				r.start(20, 0, "b.txt")
				r.stop()
				r.commit("b.txt")
			},
		},
		{
			name: "a message whose lines straddle an agent commit",
			run: func(r *tokenTurnRun) {
				r.start(10, 0, "a.txt")
				r.agentCommit("a.txt")
				// The message that made the commit writes its next content
				// block after it: same ID, same usage, counted once.
				r.repeatLine(fmt.Sprintf("msg-%d", r.turn), 10)
				r.stop()
				r.checkTotal()
				r.start(20, 0, "b.txt")
				r.stop()
				r.commit("b.txt")
			},
		},
		{
			name: "a subagent that finishes in a later turn",
			run: func(r *tokenTurnRun) {
				r.start(10, 6, "a.txt")
				r.stop()
				r.commit("a.txt", "sub1.txt")
				r.start(20, 0)
				r.stop()
				r.start(30, 4, "b.txt")
				r.stop()
				r.checkTotal()
				r.commit("b.txt", "sub2.txt")
			},
		},
		{
			name: "a transcript rewritten to an older copy",
			run: func(r *tokenTurnRun) {
				r.start(10, 0, "a.txt")
				r.stop()
				keep := len(r.s.TranscriptBuilder.messages)
				r.start(20, 0, "b.txt")
				r.stop()
				r.commit("a.txt", "b.txt")
				r.rewind(keep)
				r.start(30, 0, "c.txt")
				r.stop()
				r.checkTotal()
				r.commit("c.txt")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newTokenTurnRun(t)
			tc.run(r)
			r.checkTotal()
			r.verify()
		})
	}
}

// writeDroidTokenUsage writes the running total Droid keeps in the session's
// settings.json beside its transcript.
func writeDroidTokenUsage(t *testing.T, transcriptPath string, input, output int) {
	t.Helper()
	settings := fmt.Sprintf(`{"tokenUsage":{"inputTokens":%d,"outputTokens":%d,"cacheCreationTokens":0,"cacheReadTokens":0,"thinkingTokens":0}}`, input, output)
	require.NoError(t, os.WriteFile(strings.TrimSuffix(transcriptPath, ".jsonl")+".settings.json", []byte(settings), 0o600))
}

// TestTokenLedger_FactoryDroid_WorkerAndSecondStop: a prompt that launches a
// Worker ends with two Stops and no turn start between them; the second
// counts what the parent used after the Worker returned. The Worker's usage
// reaches the parent once, credited as child-session tokens, never as a
// session total of its own.
func TestTokenLedger_FactoryDroid_WorkerAndSecondStop(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	env.InitRepo()
	env.InitEntire()
	env.WriteFile(".gitignore", ".entire/\n")
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd(".gitignore")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	parent := env.NewFactoryDroidSession()
	require.NoError(t, env.SimulateFactoryDroidUserPromptSubmitWithTranscript(parent.ID, parent.TranscriptPath))
	parent.CreateDroidTranscript("Run a Worker that writes docs/summary.md", nil)
	writeDroidTokenUsage(t, parent.TranscriptPath, 100, 10)
	require.NoError(t, env.SimulateFactoryDroidStop(parent.ID, parent.TranscriptPath))

	worker := env.NewFactoryDroidSession()
	require.NoError(t, env.SimulateFactoryDroidUserPromptSubmitWithTranscript(worker.ID, worker.TranscriptPath))
	env.WriteFile("docs/summary.md", "A short summary.\n")
	worker.CreateDroidTranscript("# Task Tool Invocation", []FileChange{{Path: "docs/summary.md", Content: "A short summary.\n"}})
	worker.MarkAsWorkerSession(parent.ID, "toolu_worker_01", "worker: Summarize the repo")
	writeDroidTokenUsage(t, worker.TranscriptPath, 40, 4)
	require.NoError(t, env.SimulateFactoryDroidStop(worker.ID, worker.TranscriptPath))

	// The Worker's result wakes the parent, which stops again.
	writeDroidTokenUsage(t, parent.TranscriptPath, 130, 15)
	require.NoError(t, env.SimulateFactoryDroidStop(parent.ID, parent.TranscriptPath))
	require.NoError(t, env.SimulateFactoryDroidStop(parent.ID, parent.TranscriptPath), "a repeated Stop")

	parentState, err := env.GetSessionState(parent.ID)
	require.NoError(t, err)
	total := parentState.TotalTokenUsage()
	require.Equal(t, 130, total.InputTokens, "the second Stop's usage is counted once")
	require.Equal(t, 15, total.OutputTokens)
	require.NotNil(t, total.SubagentTokens, "the Worker is credited to the parent once")
	require.Equal(t, 40, total.SubagentTokens.InputTokens)

	workerState, err := env.GetSessionState(worker.ID)
	require.NoError(t, err)
	if workerState != nil {
		require.Nil(t, workerState.TokenLedger, "a Worker's session keeps no total of its own")
		require.Nil(t, workerState.TokenUsage, "a Worker's session keeps no total of its own")
	}

	env.GitCommitWithHooks("Add summary", "docs/summary.md")
	cp := env.TryGetLatestCheckpointID()
	require.NotEmpty(t, cp)
	usage := readCommittedTokenUsage(t, env, cp)
	require.NotNil(t, usage)
	require.Equal(t, 130, usage.InputTokens)
	require.Equal(t, 15, usage.OutputTokens)
	require.NotNil(t, usage.SubagentTokens)
	require.Equal(t, 40, usage.SubagentTokens.InputTokens)
}

// TestTokenLedger_Codex_RepeatedStopAndMidTurnCommit: Codex reports running
// totals, so a repeated Stop on a turn that changed files, and an agent commit
// in the middle of a turn, each leave every call counted once.
func TestTokenLedger_Codex_RepeatedStopAndMidTurnCommit(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	const sessionID = "codex-ledger-session"
	rollout := filepath.Join(env.RepoDir, ".entire", "tmp", "codex-ledger-rollout.jsonl")
	appendLines(t, rollout,
		`{"timestamp":"2026-01-01T00:00:00Z","type":"session_meta","payload":{"id":"`+sessionID+`","thread_source":"user","cwd":"`+env.RepoDir+`"}}`)
	hook := codexHooker(t, env.RepoDir, sessionID, rollout)

	var cumInput, cumOutput, calls int
	call := func(input, output int) {
		t.Helper()
		calls++
		cumInput += input
		cumOutput += output
		appendLines(t, rollout, fmt.Sprintf(`{"timestamp":"2026-01-01T00:00:%02dZ","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":%d,"cached_input_tokens":0,"output_tokens":%d}}}}`, calls, cumInput, cumOutput))
	}
	write := func(name string) {
		t.Helper()
		env.WriteFile(name, name+"\n")
		applyPatchHook(hook, "call_"+strings.TrimSuffix(name, ".txt"), "*** Begin Patch\n*** Add File: "+name+"\n+"+name+"\n*** End Patch\n")
	}
	stop := func() { hook("stop", map[string]any{"hook_event_name": "Stop"}) }

	hook("user-prompt-submit", map[string]any{"prompt": "turn 1", "hook_event_name": "UserPromptSubmit"})
	call(100, 10)
	write("a.txt")
	stop()
	stop()
	require.Equal(t, 100, sessionTokenTotal(t, env, sessionID).InputTokens, "a repeated Stop adds nothing")

	hook("user-prompt-submit", map[string]any{"prompt": "turn 2", "hook_event_name": "UserPromptSubmit"})
	call(40, 4)
	write("b.txt")
	env.GitCommitWithHooksAsAgent("agent commit", "a.txt", "b.txt")
	cp1 := env.TryGetLatestCheckpointID()
	require.NotEmpty(t, cp1)
	call(7, 1) // after the commit, in the same turn
	stop()

	hook("user-prompt-submit", map[string]any{"prompt": "turn 3", "hook_event_name": "UserPromptSubmit"})
	call(5, 1)
	write("c.txt")
	stop()
	_, usage2 := commitCheckpointTokens(t, env, cp1, "user commit", "c.txt")
	usage1 := readCommittedTokenUsage(t, env, cp1)
	require.NotNil(t, usage1)
	require.Equal(t, 140, usage1.InputTokens, "the mid-turn checkpoint holds everything before it")
	require.Equal(t, 12, usage2.InputTokens, "the next checkpoint holds turn 2's tail and turn 3")
	require.Equal(t, 152, sessionTokenTotal(t, env, sessionID).InputTokens)
	content, found := env.ReadFileFromBranch(paths.MetadataBranchName, SessionMetadataPath(cp1))
	require.True(t, found)
	var meta checkpoint.Metadata
	require.NoError(t, json.Unmarshal([]byte(content), &meta))
	require.NotNil(t, meta.SessionTokenUsage)
	require.Equal(t, 140, meta.SessionTokenUsage.InputTokens, "the session's running total at that checkpoint")
	require.Equal(t, 16, sessionTokenTotal(t, env, sessionID).OutputTokens)
}
