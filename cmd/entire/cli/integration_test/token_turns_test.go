//go:build integration

package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tokenTurnRun drives one session through turns, commits and attaches, and
// records what each new checkpoint must hold: exactly the tokens of the turns
// (and turn tails) no earlier checkpoint stored.
type tokenTurnRun struct {
	t   *testing.T
	env *TestEnv
	s   *Session

	turn    int
	nextSub int
	// pending is what the next checkpoint must count.
	pendingOut, pendingSub int
	total, totalSub        int
	want                   []tokenTurnCheckpoint
}

type tokenTurnCheckpoint struct {
	id       string
	step     string
	out, sub int
}

func newTokenTurnRun(t *testing.T) *tokenTurnRun {
	t.Helper()
	env := NewFeatureBranchEnv(t)
	s := env.NewSession()
	// Attach finds the transcript in the agent's project dir; keep it there.
	s.TranscriptPath = filepath.Join(env.ClaudeProjectDir, s.ID+".jsonl")
	env.ExtraEnv = append(env.ExtraEnv,
		"PATH="+filepath.Dir(getTestBinary())+string(os.PathListSeparator)+os.Getenv("PATH"))
	return &tokenTurnRun{t: t, env: env, s: s}
}

// start begins a turn that spends tokens and writes files; sub > 0 also runs
// a subagent that spends sub tokens and writes its own file.
func (r *tokenTurnRun) start(tokens, sub int, files ...string) {
	r.t.Helper()
	r.turn++
	require.NoError(r.t, r.env.SimulateUserPromptSubmit(r.s.ID))
	changes := make([]FileChange, 0, len(files))
	for _, f := range files {
		content := fmt.Sprintf("turn %d\n", r.turn)
		r.env.WriteFile(f, content)
		changes = append(changes, FileChange{Path: f, Content: content})
	}
	appendUsageMessage(r.s, fmt.Sprintf("msg-%d", r.turn), tokens)
	r.s.CreateTranscript(fmt.Sprintf("turn %d", r.turn), changes)
	r.add(tokens, 0)
	if sub > 0 {
		r.subagent(sub)
	}
}

func (r *tokenTurnRun) subagent(tokens int) {
	r.t.Helper()
	r.nextSub++
	a := workflowAgentFixture{
		id:    fmt.Sprintf("a%016d", r.nextSub),
		file:  fmt.Sprintf("sub%d.txt", r.nextSub),
		usage: map[string]int{"input_tokens": tokens, "output_tokens": tokens},
	}
	require.NoError(r.t, r.env.SimulateSubagentStart(SubagentStartInput{
		SessionID: r.s.ID, TranscriptPath: r.s.TranscriptPath, AgentID: a.id, AgentType: "workflow-subagent",
	}))
	// Each subagent runs in its own Workflow run, which the parent transcript
	// names in the Workflow call's result, as Claude Code does.
	runID := fmt.Sprintf("wf_tokturn-%d", r.nextSub)
	runDir := filepath.Join(paths.SubagentsDir(filepath.Dir(r.s.TranscriptPath), r.s.ID),
		paths.SubagentWorkflowsDirName, runID)
	transcript := writeWorkflowAgentTranscript(r.t, runDir, r.nextSub, a)
	r.env.WriteFile(a.file, "from subagent\n")
	r.s.TranscriptBuilder.messages = append(r.s.TranscriptBuilder.messages, map[string]interface{}{
		"uuid": "workflow-result-" + runID,
		"type": "user",
		"message": map[string]interface{}{"role": "user", "content": []map[string]interface{}{{
			"type": "tool_result", "tool_use_id": "toolu_" + runID,
			"content": "Workflow launched in background.\nRun ID: " + runID + "\n",
		}}},
	})
	require.NoError(r.t, r.s.TranscriptBuilder.WriteToFile(r.s.TranscriptPath))
	require.NoError(r.t, r.env.SimulateSubagentStop(SubagentStopInput{
		SessionID: r.s.ID, TranscriptPath: r.s.TranscriptPath, AgentID: a.id,
		AgentType: "workflow-subagent", AgentTranscriptPath: transcript,
	}))
	r.add(0, tokens)
}

// more spends tokens in the running turn without ending it.
func (r *tokenTurnRun) more(tokens int) {
	r.t.Helper()
	appendUsageMessage(r.s, fmt.Sprintf("msg-%d-more-%d", r.turn, tokens), tokens)
	require.NoError(r.t, r.s.TranscriptBuilder.WriteToFile(r.s.TranscriptPath))
	r.add(tokens, 0)
}

func (r *tokenTurnRun) stop() {
	r.t.Helper()
	require.NoError(r.t, r.env.SimulateStop(r.s.ID, r.s.TranscriptPath))
}

func (r *tokenTurnRun) add(out, sub int) {
	r.pendingOut += out
	r.pendingSub += sub
	r.total += out
	r.totalSub += sub
}

// commit commits files through the hooks, condensing the session.
func (r *tokenTurnRun) commit(files ...string) {
	r.t.Helper()
	r.env.GitCommitWithHooks(fmt.Sprintf("commit after turn %d", r.turn), files...)
	r.record(fmt.Sprintf("commit after turn %d", r.turn))
}

// attach commits an unrelated file without hooks and attaches the session to it.
func (r *tokenTurnRun) attach() {
	r.t.Helper()
	name := fmt.Sprintf("notes-%d.txt", len(r.want))
	r.env.WriteFile(name, "notes")
	r.env.GitAdd(name)
	r.env.GitCommit("notes " + name)
	output := r.env.RunCLI("session", "attach", r.s.ID, "-a", agentClaudeCode, "-f")
	require.Contains(r.t, output, "Attached session")
	r.record(fmt.Sprintf("attach after turn %d", r.turn))
}

func (r *tokenTurnRun) record(step string) {
	r.t.Helper()
	cp := r.env.TryGetLatestCheckpointID()
	require.NotEmpty(r.t, cp, step)
	for _, w := range r.want {
		require.NotEqual(r.t, w.id, cp, "%s: expected a new checkpoint", step)
	}
	r.want = append(r.want, tokenTurnCheckpoint{id: cp, step: step, out: r.pendingOut, sub: r.pendingSub})
	r.pendingOut, r.pendingSub = 0, 0
}

func (r *tokenTurnRun) verify() {
	r.t.Helper()
	require.Zero(r.t, r.pendingOut+r.pendingSub, "every turn must end up in a checkpoint")
	var sumOut, sumSub int
	for _, w := range r.want {
		u := readCommittedTokenUsage(r.t, r.env, w.id)
		require.NotNil(r.t, u, w.step)
		gotSub := 0
		if u.SubagentTokens != nil {
			gotSub = u.SubagentTokens.OutputTokens
		}
		assert.Equal(r.t, w.out, u.OutputTokens, "%s: checkpoint must hold exactly the turns it newly covers", w.step)
		assert.Equal(r.t, w.sub, gotSub, "%s: subagent tokens", w.step)
		sumOut += u.OutputTokens
		sumSub += gotSub
	}
	assert.Equal(r.t, r.total, sumOut, "checkpoints must sum to the session's tokens")
	assert.Equal(r.t, r.totalSub, sumSub, "checkpoints must sum to the session's subagent tokens")
}

// TestTokenScope_EachTurnCountedOnce: every turn's tokens land in exactly one
// checkpoint across hook commits, partial commits (carry-forward), and
// repeated attaches to successive commits, for ended and running sessions.
func TestTokenScope_EachTurnCountedOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		run  func(r *tokenTurnRun)
	}{
		{
			name: "ended session",
			run: func(r *tokenTurnRun) {
				r.start(10, 0, "a.txt")
				r.stop()
				r.commit("a.txt")
				r.start(20, 0, "b.txt", "c.txt")
				r.stop()
				r.commit("b.txt") // partial: c.txt carries forward
				r.start(30, 0)
				r.stop()
				r.attach()
				r.start(40, 5)
				r.stop()
				r.attach()
				r.start(50, 0, "d.txt")
				r.stop()
				r.commit("c.txt", "d.txt", "sub1.txt")
			},
		},
		{
			name: "attach while the turn runs",
			run: func(r *tokenTurnRun) {
				r.start(10, 0, "a.txt")
				r.stop()
				r.commit("a.txt")
				r.start(20, 0, "b.txt", "c.txt")
				r.stop()
				r.commit("b.txt")
				r.start(30, 0)
				r.attach() // running: stores 30
				r.more(3)
				r.stop()
				r.start(40, 5)
				r.attach() // running: stores the 3-token tail, 40 and the subagent's 5
				r.more(4)
				r.stop()
				r.start(50, 0, "d.txt")
				r.stop()
				r.commit("c.txt", "d.txt", "sub1.txt") // 4-token tail + 50
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
