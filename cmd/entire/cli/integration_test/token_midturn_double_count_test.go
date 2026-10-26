//go:build integration

package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// agentCommit commits files through the hooks as the agent would mid-turn.
func (r *tokenTurnRun) agentCommit(files ...string) {
	r.t.Helper()
	r.env.GitCommitWithHooksAsAgent("agent commit in turn", files...)
	r.record("agent commit mid-turn")
}

// straddlingTail writes the last message's remaining line: Claude Code writes
// one line per content block of a message, all with the message's id and
// usage, and runs the message's tool calls between them.
func (r *tokenTurnRun) straddlingTail() {
	r.t.Helper()
	b := r.s.TranscriptBuilder
	b.messages = append(b.messages, b.messages[len(b.messages)-1])
	require.NoError(r.t, r.s.TranscriptBuilder.WriteToFile(r.s.TranscriptPath))
}

// rewriteTranscriptEmpty rewrites the transcript shorter than the token offset
// a checkpoint left, as an undo or a forced resume can.
func (r *tokenTurnRun) rewriteTranscriptEmpty() {
	r.t.Helper()
	r.s.TranscriptBuilder.messages = nil
	require.NoError(r.t, r.s.TranscriptBuilder.WriteToFile(r.s.TranscriptPath))
}

// requireLastCheckpointOut asserts the checkpoint the last step recorded
// exists and holds out tokens, so a turn's pre-commit tokens cannot slide into
// a later checkpoint and still sum correctly.
func (r *tokenTurnRun) requireLastCheckpointOut(out int) {
	r.t.Helper()
	require.NotEmpty(r.t, r.want, "no checkpoint recorded")
	last := r.want[len(r.want)-1]
	u := readCommittedTokenUsage(r.t, r.env, last.id)
	require.NotNil(r.t, u, "%s: checkpoint holds no token usage", last.step)
	require.Equal(r.t, out, u.OutputTokens, "%s: checkpoint must hold the pre-commit tokens", last.step)
}

// dropPrePromptState removes the turn's pre-prompt state, as a lost or
// unreadable one would be, so Stop counts the turn from the transcript start.
func (r *tokenTurnRun) dropPrePromptState() {
	r.t.Helper()
	require.NoError(r.t, os.Remove(filepath.Join(r.env.RepoDir, ".entire", "tmp", "pre-prompt-"+r.s.ID+".json")))
}

// TestTokenScope_MidTurnConsumptionThenStop covers a mid-turn token
// consumption (attach while the turn runs, or an agent commit that condenses
// the session) followed by the turn's Stop. Stop adds the whole turn — counted
// from turn start — to the pending checkpoint usage. The next condensation
// recounts from the token offset and only falls back to that pending usage
// when the recount finds no usage lines, which is when the part of the turn
// the consumption already stored would be counted a second time.
func TestTokenScope_MidTurnConsumptionThenStop(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		run  func(r *tokenTurnRun)
	}{
		{
			name: "attach mid-turn, no usage after, commit",
			run: func(r *tokenTurnRun) {
				r.start(10, 0, "a.txt")
				r.stop()
				r.commit("a.txt")
				r.start(30, 0, "e.txt")
				r.attach() // running: stores 30
				r.stop()   // no usage lines written after attach
				r.commit("e.txt")
			},
		},
		{
			name: "attach mid-turn, no usage after, next turn then commit",
			run: func(r *tokenTurnRun) {
				r.start(10, 0, "a.txt")
				r.stop()
				r.commit("a.txt")
				r.start(30, 0, "e.txt")
				r.attach()
				r.stop()
				r.start(50, 0, "d.txt")
				r.stop()
				r.commit("e.txt", "d.txt")
			},
		},
		{
			name: "agent commit mid-turn, no usage after, user commits rest",
			run: func(r *tokenTurnRun) {
				r.start(100, 0, "a.txt", "b.txt")
				r.agentCommit("a.txt") // condenses while ACTIVE: stores 100
				r.stop()               // no usage lines written after the commit
				r.commit("b.txt")
			},
		},
		{
			name: "agent commit mid-turn, no usage after, attach after stop",
			run: func(r *tokenTurnRun) {
				r.start(100, 0, "a.txt", "b.txt")
				r.agentCommit("a.txt")
				r.stop()
				r.attach() // AttachTokenUsage has the same pending-usage fallback
			},
		},
		{
			name: "control: agent commit mid-turn, usage after, user commits rest",
			run: func(r *tokenTurnRun) {
				r.start(100, 0, "a.txt", "b.txt")
				r.agentCommit("a.txt")
				r.more(5)
				r.stop()
				r.commit("b.txt")
			},
		},
		{
			name: "agent commits everything mid-turn, clean-tree stop, attach",
			run: func(r *tokenTurnRun) {
				r.start(100, 0, "a.txt")
				r.agentCommit("a.txt")
				r.stop() // nothing left to checkpoint: the turn ends without a step
				r.attach()
			},
		},
		{
			name: "two agent commits in one turn",
			run: func(r *tokenTurnRun) {
				r.start(100, 0, "a.txt", "b.txt", "c.txt")
				r.agentCommit("a.txt")
				r.more(5)
				r.agentCommit("b.txt")
				r.stop()
				r.commit("c.txt")
			},
		},
		{
			name: "agent commit mid-turn without pre-prompt state",
			run: func(r *tokenTurnRun) {
				r.start(100, 0, "a.txt", "b.txt")
				r.dropPrePromptState()
				r.agentCommit("a.txt")
				r.stop()
				r.commit("b.txt")
			},
		},
		{
			name: "subagent finishes after the agent commit, no main usage after",
			run: func(r *tokenTurnRun) {
				r.start(100, 0, "a.txt", "b.txt")
				r.agentCommit("a.txt")
				r.subagent(20)
				r.stop()
				r.commit("b.txt", "sub1.txt")
			},
		},
		{
			name: "only a straddling message's tail follows the agent commit",
			run: func(r *tokenTurnRun) {
				r.start(0, 0, "a.txt", "b.txt")
				r.more(100) // a message whose first line the commit stores
				r.agentCommit("a.txt")
				r.straddlingTail()
				r.stop()
				r.commit("b.txt")
			},
		},
		{
			name: "transcript rewritten shorter than the token offset before the turn",
			run: func(r *tokenTurnRun) {
				r.start(10, 0, "a.txt")
				r.stop()
				r.commit("a.txt")
				r.rewriteTranscriptEmpty()
				r.start(30, 0, "b.txt") // the offset lies past this turn's start, but no condensation moved it
				r.stop()
				r.commit("b.txt")
			},
		},
		{
			name: "transcript rewritten shorter, then an agent commit mid-turn",
			run: func(r *tokenTurnRun) {
				r.start(10, 0, "a.txt") // 6 lines: the token offset lands at 6
				r.stop()
				r.commit("a.txt")
				r.rewriteTranscriptEmpty()
				r.start(30, 0, "b.txt", "c.txt") // 8 lines, counted from 0
				r.agentCommit("b.txt")
				r.requireLastCheckpointOut(30)
				r.stop()
				r.commit("c.txt")
			},
		},
		{
			name: "transcript rewritten shorter, then two agent commits mid-turn",
			run: func(r *tokenTurnRun) {
				r.start(10, 0, "a.txt")
				r.stop()
				r.commit("a.txt")
				r.rewriteTranscriptEmpty()
				r.start(30, 0, "b.txt", "c.txt", "d.txt")
				r.agentCommit("b.txt")
				r.requireLastCheckpointOut(30)
				r.more(5)
				r.agentCommit("c.txt")
				r.requireLastCheckpointOut(5)
				r.stop()
				r.commit("d.txt")
			},
		},
		{
			name: "pending turn rewritten away, then an agent commit mid-turn",
			run: func(r *tokenTurnRun) {
				r.start(10, 0, "a.txt")
				r.stop()
				r.commit("a.txt") // the token offset lands at 6
				r.start(20, 0, "b.txt")
				r.stop() // pending: its lines lie past the offset
				r.rewriteTranscriptEmpty()
				r.start(30, 0, "c.txt", "d.txt")
				r.agentCommit("c.txt")
				r.requireLastCheckpointOut(50) // the rewritten turn's pending tokens and this turn's
				r.stop()
				r.commit("b.txt", "d.txt")
			},
		},
		{
			name: "transcript rewritten to as many lines as the token offset, then an agent commit",
			run: func(r *tokenTurnRun) {
				r.start(10, 0, "a.txt", "x.txt") // 8 lines: the token offset lands at 8
				r.stop()
				r.commit("a.txt", "x.txt")
				r.rewriteTranscriptEmpty()
				r.start(30, 0, "b.txt", "c.txt") // 8 lines again: the commit leaves the offset where it was
				r.agentCommit("b.txt")
				r.requireLastCheckpointOut(30)
				r.stop()
				r.commit("c.txt")
			},
		},
		{
			name: "agent commit mid-turn, no usage after, next turn then commit",
			run: func(r *tokenTurnRun) {
				r.start(100, 0, "a.txt", "b.txt")
				r.agentCommit("a.txt")
				r.stop()
				r.start(7, 0, "c.txt")
				r.stop()
				r.commit("b.txt", "c.txt")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newTokenTurnRun(t)
			tc.run(r)
			r.verify()
		})
	}
}

// TestTokenScope_Cursor_MidTurnAgentCommitLeavesTurnPending: Cursor reports a
// turn's tokens only in its Stop hook, so a mid-turn agent commit stores none
// of them and the whole turn must stay pending for the next checkpoint, even
// though the commit moved the token offset past the turn start.
func TestTokenScope_Cursor_MidTurnAgentCommitLeavesTurnPending(t *testing.T) {
	t.Parallel()
	r := newCursorTokenRun(t)
	env := r.env
	turn := agent.TokenUsage{InputTokens: 101, OutputTokens: 11, CacheReadTokens: 1001, CacheCreationTokens: 10001}
	var steps []agentTokenStep

	r.start("a.txt", "b.txt")
	env.GitCommitWithHooksAsAgent("agent commit in turn", "a.txt")
	steps = recordCheckpoint(t, env, steps, "agent commit mid-turn", agent.TokenUsage{})
	r.stop(turn)
	env.GitCommitWithHooks("rest of the turn", "b.txt")
	steps = recordCheckpoint(t, env, steps, "commit after the turn", turn)

	verifyAgentSteps(t, env, steps, turn)
	assert.Equal(t, tokenFields(&turn), tokenFields(sessionTotal(t, env, r.id)), "session total = the turn once")
}

// TestTokenScope_Codex_MidTurnAgentCommitNoTokenCountAfter: the agent commits
// mid-turn and makes no model call before Stop. The commit stored the turn's
// tokens; Stop must not leave them pending for the next checkpoint.
func TestTokenScope_Codex_MidTurnAgentCommitNoTokenCountAfter(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	const sessionID = "codex-midturn-session"
	rollout := filepath.Join(env.RepoDir, ".entire", "tmp", "codex-midturn-rollout.jsonl")
	appendLines(t, rollout,
		`{"timestamp":"2026-01-01T00:00:00Z","type":"session_meta","payload":{"id":"`+sessionID+`","thread_source":"user","cwd":"`+env.RepoDir+`"}}`)
	hook := codexHooker(t, env.RepoDir, sessionID, rollout)

	hook("user-prompt-submit", map[string]any{"prompt": "turn 1", "hook_event_name": "UserPromptSubmit"})
	appendLines(t, rollout,
		`{"timestamp":"2026-01-01T00:01:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"turn 1"}]}}`,
		`{"timestamp":"2026-01-01T00:01:02Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"cached_input_tokens":20,"output_tokens":10}}}}`,
		`{"timestamp":"2026-01-01T00:01:03Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}}`)
	for i, name := range []string{"a.txt", "b.txt"} {
		env.WriteFile(name, "turn 1\n")
		applyPatchHook(hook, fmt.Sprintf("call_%d", i), "*** Begin Patch\n*** Add File: "+name+"\n+turn 1\n*** End Patch\n")
	}
	env.GitCommitWithHooksAsAgent("agent commit in turn", "a.txt")
	cp1 := env.TryGetLatestCheckpointID()
	require.NotEmpty(t, cp1)
	hook("stop", map[string]any{"hook_event_name": "Stop"})

	env.GitCommitWithHooks("rest of the turn", "b.txt")
	cp2 := env.TryGetLatestCheckpointID()
	require.NotEqual(t, cp1, cp2, "expected a new checkpoint")
	assert.Equal(t, [4]int{80, 10, 20, 0}, tokenFields(readCommittedTokenUsage(t, env, cp1)), "the agent commit stores the turn's tokens")
	assert.Equal(t, [4]int{}, tokenFields(readCommittedTokenUsage(t, env, cp2)), "the next checkpoint must not store them again")
	assert.Equal(t, 10, sessionTokenTotal(t, env, sessionID).OutputTokens, "session total = the turn once")
}

// TestTokenScope_Antigravity_MidTurnCommitLeavesTurnPending: Antigravity's
// tokens come out of band and are measured at Stop, so a mid-turn commit
// stores none of the turn and the whole turn must stay pending.
func TestTokenScope_Antigravity_MidTurnCommitLeavesTurnPending(t *testing.T) {
	t.Parallel()
	env := newAntigravityEnv(t)
	statusDir := t.TempDir()
	const conversationID = "antigravity-midturn-tokens"
	extraEnv := []string{
		"ENTIRE_ANTIGRAVITY_STATUS_DIR=" + statusDir,
		"ENTIRE_ANTIGRAVITY_CONFIG_DIR=" + t.TempDir(),
	}
	env.ExtraEnv = append(env.ExtraEnv, extraEnv...)
	snapshot := func(input, output int) {
		t.Helper()
		appendLines(t, filepath.Join(statusDir, conversationID+".jsonl"), fmt.Sprintf(
			`{"ts":"2026-06-03T10:00:00Z","conversation_id":%q,"context_window":{"total_input_tokens":%d,"total_output_tokens":%d}}`,
			conversationID, input, output))
	}
	transcriptPath := filepath.Join(antigravityBrainDir(env.RepoDir), conversationID, ".system_generated", "logs", "transcript.jsonl")
	appendLines(t, transcriptPath, `{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","content":"write a and b, commit a"}`)
	common := map[string]any{
		"conversationId":        conversationID,
		"workspacePaths":        []string{env.RepoDir},
		"transcriptPath":        transcriptPath,
		"artifactDirectoryPath": filepath.Join(env.RepoDir, ".gemini", "antigravity-cli", "artifacts"),
	}
	hook := func(name string, extra map[string]any) {
		t.Helper()
		require.NoError(t, runAntigravityHookWithEnv(t, env.RepoDir, name, mergeMaps(common, extra), extraEnv))
	}

	snapshot(1000, 100)
	hook("pre-invocation", map[string]any{"invocationNum": 0, "initialNumSteps": 1})
	for i, name := range []string{"a.txt", "b.txt"} {
		env.WriteFile(name, "from agy\n")
		hook("pre-tool-use", map[string]any{
			"toolCall": map[string]any{"name": "write_to_file", "args": map[string]any{"TargetFile": name, "Overwrite": false}},
			"stepIdx":  i + 1,
		})
	}
	gitCLICommitWithEntireHooks(t, env, "agent commit in turn", "a.txt")
	cp1 := env.TryGetLatestCheckpointID()
	require.NotEmpty(t, cp1)
	appendLines(t, transcriptPath, `{"step_index":3,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","content":"done"}`)
	snapshot(4500, 400)
	hook("stop", map[string]any{"executionNum": 1, "terminationReason": "model_stop", "error": "", "fullyIdle": true})

	_, usage := commitCheckpointTokens(t, env, cp1, "rest of the turn", "b.txt")
	assert.Equal(t, 3500, usage.InputTokens, "the whole turn stays pending after the mid-turn commit")
	assert.Equal(t, 300, usage.OutputTokens, "the whole turn stays pending after the mid-turn commit")
	if committed := readCommittedTokenUsage(t, env, cp1); committed != nil {
		assert.Zero(t, committed.OutputTokens, "the mid-turn commit stores none of the turn")
	}
	assert.Equal(t, 300, sessionTokenTotal(t, env, conversationID).OutputTokens, "session total = the turn once")
}
