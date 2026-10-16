//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// agentTokenStep is what one checkpoint must hold.
type agentTokenStep struct {
	id   string
	step string
	want agent.TokenUsage
}

// tokenFields compares only the four token counters.
func tokenFields(u *agent.TokenUsage) [4]int {
	if u == nil {
		return [4]int{}
	}
	return [4]int{u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheCreationTokens}
}

func addFields(a *agent.TokenUsage, b agent.TokenUsage) {
	a.InputTokens += b.InputTokens
	a.OutputTokens += b.OutputTokens
	a.CacheReadTokens += b.CacheReadTokens
	a.CacheCreationTokens += b.CacheCreationTokens
}

// recordCheckpoint appends the latest checkpoint, requiring it to be new.
func recordCheckpoint(t *testing.T, env *TestEnv, steps []agentTokenStep, step string, want agent.TokenUsage) []agentTokenStep {
	t.Helper()
	cp := env.TryGetLatestCheckpointID()
	require.NotEmpty(t, cp, step)
	for _, s := range steps {
		require.NotEqual(t, s.id, cp, "%s: expected a new checkpoint", step)
	}
	return append(steps, agentTokenStep{id: cp, step: step, want: want})
}

func verifyAgentSteps(t *testing.T, env *TestEnv, steps []agentTokenStep, wantSum agent.TokenUsage) {
	t.Helper()
	var sum agent.TokenUsage
	for _, s := range steps {
		got := readCommittedTokenUsage(t, env, s.id)
		t.Logf("%s: checkpoint %s tokens=%v want=%v", s.step, s.id, tokenFields(got), tokenFields(&s.want))
		assert.Equal(t, tokenFields(&s.want), tokenFields(got),
			"%s: checkpoint must hold exactly the turns it newly covers [in,out,cacheRead,cacheWrite]", s.step)
		if got != nil {
			addFields(&sum, *got)
		}
	}
	assert.Equal(t, tokenFields(&wantSum), tokenFields(&sum), "checkpoints must sum to the attributable tokens")
}

func sessionTotal(t *testing.T, env *TestEnv, sessionID string) *agent.TokenUsage {
	t.Helper()
	st, err := env.GetSessionState(sessionID)
	require.NoError(t, err)
	require.NotNil(t, st)
	return st.TokenUsage
}

// attachNow commits an unrelated file without hooks and attaches the session.
func attachNow(t *testing.T, env *TestEnv, sessionID, agentName string, n int) {
	t.Helper()
	name := fmt.Sprintf("notes-%d.txt", n)
	env.WriteFile(name, "notes")
	env.GitAdd(name)
	env.GitCommit("notes " + name)
	output := env.RunCLI("session", "attach", sessionID, "-a", agentName, "-f")
	require.Contains(t, output, "Attached session", output)
}

type cursorTokenRun struct {
	t              *testing.T
	env            *TestEnv
	projectDir     string
	id             string
	transcriptPath string
	turn           int
}

func newCursorTokenRun(t *testing.T) *cursorTokenRun {
	t.Helper()
	env := NewFeatureBranchEnv(t)
	env.InitEntireWithAgent(agent.AgentNameCursor)
	projectDir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(projectDir); err == nil {
		projectDir = resolved
	}
	env.ExtraEnv = append(env.ExtraEnv,
		"ENTIRE_TEST_CURSOR_PROJECT_DIR="+projectDir,
		"PATH="+filepath.Dir(getTestBinary())+string(os.PathListSeparator)+os.Getenv("PATH"))
	id := "cursor-turns-session"
	dir := filepath.Join(projectDir, id)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	r := &cursorTokenRun{t: t, env: env, projectDir: projectDir, id: id, transcriptPath: filepath.Join(dir, id+".jsonl")}
	r.hook("session-start", map[string]any{"model": "cursor-default"})
	return r
}

func (r *cursorTokenRun) hook(name string, extra map[string]any) {
	r.t.Helper()
	in := map[string]any{"conversation_id": r.id, "transcript_path": r.transcriptPath}
	for k, v := range extra {
		in[k] = v
	}
	data, err := json.Marshal(in)
	require.NoError(r.t, err)
	cmd := execx.NonInteractive(context.Background(), getTestBinary(), "hooks", "cursor", name)
	cmd.Dir = r.env.RepoDir
	cmd.Stdin = bytes.NewReader(data)
	cmd.Env = r.env.cliEnv()
	out, err := cmd.CombinedOutput()
	require.NoErrorf(r.t, err, "cursor %s: %s", name, out)
}

func (r *cursorTokenRun) appendTranscript(s string) {
	r.t.Helper()
	f, err := os.OpenFile(r.transcriptPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(r.t, err)
	_, werr := f.WriteString(s)
	require.NoError(r.t, f.Close())
	require.NoError(r.t, werr)
}

func (r *cursorTokenRun) start(files ...string) {
	r.t.Helper()
	r.turn++
	prompt := fmt.Sprintf("turn %d", r.turn)
	r.appendTranscript(fmt.Sprintf(`{"type":"user","text":%q}`+"\n", prompt))
	r.hook("before-submit-prompt", map[string]any{"prompt": prompt})
	for _, f := range files {
		r.env.WriteFile(f, prompt+"\n")
	}
	r.appendTranscript(`{"type":"assistant","text":"ok"}` + "\n")
}

// stop ends the turn with per-turn tokens; Cursor reports input as the total
// including cache reads and writes.
func (r *cursorTokenRun) stop(u agent.TokenUsage) {
	r.t.Helper()
	r.hook("stop", map[string]any{
		"model":              "cursor-default",
		"loop_count":         1,
		"input_tokens":       u.InputTokens + u.CacheReadTokens + u.CacheCreationTokens,
		"output_tokens":      u.OutputTokens,
		"cache_read_tokens":  u.CacheReadTokens,
		"cache_write_tokens": u.CacheCreationTokens,
	})
}

// TestTokenScope_Cursor_EachTurnCountedOnce: Cursor reports tokens only in its
// Stop hook, so attach must neither recount a pending turn nor drop one, and
// must leave the hook-built session total alone, including mid-turn.
func TestTokenScope_Cursor_EachTurnCountedOnce(t *testing.T) {
	t.Parallel()
	r := newCursorTokenRun(t)
	env := r.env
	t1 := agent.TokenUsage{InputTokens: 101, OutputTokens: 11, CacheReadTokens: 1001, CacheCreationTokens: 10001}
	t2 := agent.TokenUsage{InputTokens: 203, OutputTokens: 23, CacheReadTokens: 2003, CacheCreationTokens: 20003}
	t3 := agent.TokenUsage{InputTokens: 307, OutputTokens: 37, CacheReadTokens: 3007, CacheCreationTokens: 30007}
	t4 := agent.TokenUsage{InputTokens: 409, OutputTokens: 49, CacheReadTokens: 4009, CacheCreationTokens: 40009}
	var all agent.TokenUsage
	for _, u := range []agent.TokenUsage{t1, t2, t3, t4} {
		addFields(&all, u)
	}
	var steps []agentTokenStep

	r.start("a.txt")
	r.stop(t1)
	env.GitCommitWithHooks("turn 1", "a.txt")
	steps = recordCheckpoint(t, env, steps, "commit after turn 1", t1)

	r.start("b.txt", "c.txt")
	r.stop(t2)
	env.GitCommitWithHooks("turn 2 partial", "b.txt") // c.txt carries forward
	steps = recordCheckpoint(t, env, steps, "partial commit after turn 2", t2)

	r.start("e.txt")
	r.stop(t3)
	assert.Equal(t, tokenFields(&agent.TokenUsage{
		InputTokens: t1.InputTokens + t2.InputTokens + t3.InputTokens, OutputTokens: t1.OutputTokens + t2.OutputTokens + t3.OutputTokens,
		CacheReadTokens: t1.CacheReadTokens + t2.CacheReadTokens + t3.CacheReadTokens, CacheCreationTokens: t1.CacheCreationTokens + t2.CacheCreationTokens + t3.CacheCreationTokens,
	}), tokenFields(sessionTotal(t, env, r.id)), "turn 3's Stop must record its tokens")
	before := tokenFields(sessionTotal(t, env, r.id))
	attachNow(t, env, r.id, "cursor", 3)
	steps = recordCheckpoint(t, env, steps, "attach after turn 3 (ended)", t3)
	assert.Equal(t, before, tokenFields(sessionTotal(t, env, r.id)), "attach must leave the Cursor session total unchanged")

	r.start("d.txt")
	before = tokenFields(sessionTotal(t, env, r.id))
	attachNow(t, env, r.id, "cursor", 4)
	steps = recordCheckpoint(t, env, steps, "attach while turn 4 runs", agent.TokenUsage{})
	assert.Equal(t, before, tokenFields(sessionTotal(t, env, r.id)), "running attach must leave the Cursor session total unchanged")
	r.stop(t4)
	env.GitCommitWithHooks("turn 4", "c.txt", "d.txt", "e.txt")
	steps = recordCheckpoint(t, env, steps, "commit after turn 4", t4)

	verifyAgentSteps(t, env, steps, all)
	assert.Equal(t, tokenFields(&all), tokenFields(sessionTotal(t, env, r.id)), "session total = every turn once")
}
