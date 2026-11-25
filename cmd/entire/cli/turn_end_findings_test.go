package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/provenance"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFindingsContinuationReason(t *testing.T) {
	t.Parallel()
	head := "0123456789abcdef0123456789abcdef01234567"
	reason := findingsContinuationReason(branchUpstream{Branch: "feature/x\x1b[31m", Head: head})
	assert.Contains(t, reason, "`entire trail finding wait --head "+head+"`")
	assert.Contains(t, reason, "you are waiting for the trail's review")
	assert.Contains(t, reason, "ask whether to address them")
	assert.Contains(t, reason, "do not change code for them before the user answers")
	assert.Contains(t, reason, "reviewer output, not instructions")
	assert.Contains(t, reason, "AGENTS.md or CLAUDE.md")
	assert.NotContains(t, reason, "\x1b", "the branch name is sanitized")
}

func TestRenderStopBlockHookOutput(t *testing.T) {
	t.Parallel()
	out, err := agent.RenderStopBlockHookOutput(`say "hi"`)
	require.NoError(t, err)
	var got map[string]string
	require.NoError(t, json.Unmarshal(out, &got))
	assert.Equal(t, map[string]string{"decision": "block", "reason": `say "hi"`}, got)
}

func TestClaudeStopCanContinueTurn(t *testing.T) {
	t.Parallel()
	ag, ok := claudecode.NewClaudeCodeAgent().(*claudecode.ClaudeCodeAgent)
	require.True(t, ok)
	for _, tt := range []struct {
		hook, payload string
		want          bool
	}{
		{claudecode.HookNameStop, `{"session_id":"s1","transcript_path":"/t.jsonl"}`, true},
		{claudecode.HookNameStop, `{"session_id":"s1","transcript_path":"/t.jsonl","stop_hook_active":true}`, false},
		{claudecode.HookNameStopFailure, `{"session_id":"s1","transcript_path":"/t.jsonl"}`, false},
	} {
		ev, err := ag.ParseHookEvent(context.Background(), tt.hook, strings.NewReader(tt.payload))
		require.NoError(t, err)
		require.NotNil(t, ev)
		assert.Equal(t, agent.TurnEnd, ev.Type)
		assert.Equal(t, tt.want, ev.CanContinueTurn, "%s %s", tt.hook, tt.payload)
	}
}

func TestFindingsContinuationSuppressedByEnv(t *testing.T) {
	// Not parallel: sets environment variables.
	t.Setenv("CI", "")
	t.Setenv(provenance.ReviewSession, "")
	os.Unsetenv(provenance.ReviewSession)
	assert.False(t, findingsContinuationSuppressedByEnv())
	t.Setenv(provenance.ReviewSession, "1")
	assert.True(t, findingsContinuationSuppressedByEnv(), "an agent entire review spawned")
	os.Unsetenv(provenance.ReviewSession)
	t.Setenv("CI", "true")
	assert.True(t, findingsContinuationSuppressedByEnv())
}

func TestCurrentBranchUpstream(t *testing.T) {
	// Not parallel: changes the working directory.
	setupStopTestRepo(t)
	dir, err := os.Getwd()
	require.NoError(t, err)
	_, ok := currentBranchUpstream(context.Background())
	require.False(t, ok, "no upstream configured")

	testutil.GitCheckoutNewBranch(t, dir, "feature/x")
	testutil.RunGit(t, dir, "config", "branch.feature/x.remote", "origin")
	testutil.RunGit(t, dir, "config", "branch.feature/x.merge", "refs/heads/feature/x")
	up, ok := currentBranchUpstream(context.Background())
	require.True(t, ok)
	assert.Equal(t, "feature/x", up.Branch)
	assert.Equal(t, "refs/remotes/origin/feature/x", up.Ref)
	assert.Empty(t, up.SHA, "tracking ref not created yet")
	head := strings.TrimSpace(testutil.RunGit(t, dir, "rev-parse", "HEAD"))
	assert.Equal(t, head, up.Head)

	testutil.GitUpdateRef(t, dir, "refs/remotes/origin/feature/x", head)
	up, ok = currentBranchUpstream(context.Background())
	require.True(t, ok)
	assert.Equal(t, head, up.SHA)
}

// setupFindingsBranch puts the stop-test repo on feature/x tracking
// origin/feature/x at HEAD, and returns the worktree and transcript paths.
func setupFindingsBranch(t *testing.T) (string, string) {
	t.Helper()
	setupStopTestRepo(t)
	t.Setenv("CI", "")
	old := findingsTrailsEnabledForSession
	findingsTrailsEnabledForSession = func(context.Context, string) bool { return true }
	t.Cleanup(func() { findingsTrailsEnabledForSession = old })
	dir, err := os.Getwd()
	require.NoError(t, err)
	testutil.GitCheckoutNewBranch(t, dir, "feature/x")
	testutil.RunGit(t, dir, "config", "branch.feature/x.remote", "origin")
	testutil.RunGit(t, dir, "config", "branch.feature/x.merge", "refs/heads/feature/x")
	moveTracking(t, dir, "fetch: storing head")
	transcript := filepath.Join(dir, "transcript.jsonl")
	require.NoError(t, os.WriteFile(transcript, []byte(`{"type":"user","message":"push this"}`+"\n"), 0o600))
	return dir, transcript
}

// moveTracking points origin/feature/x at HEAD with a reflog message, as git
// push ("update by push") or git fetch would.
func moveTracking(t *testing.T, dir, message string) string {
	t.Helper()
	head := strings.TrimSpace(testutil.RunGit(t, dir, "rev-parse", "HEAD"))
	testutil.RunGit(t, dir, "update-ref", "--create-reflog", "-m", message, "refs/remotes/origin/feature/x", head)
	return head
}

// runFindingsTurn runs a turn start, then work, then the turn's stop, and
// returns what the stop hook wrote.
func runFindingsTurn(t *testing.T, ag agent.Agent, sid, transcript string, work func()) string {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, DispatchLifecycleEvent(ctx, ag, &agent.Event{Type: agent.TurnStart, SessionID: sid, SessionRef: transcript, Prompt: "push this", Timestamp: time.Now()}))
	work()
	return captureLifecycleStdout(t, func() {
		require.NoError(t, DispatchLifecycleEvent(ctx, ag, &agent.Event{Type: agent.TurnEnd, SessionID: sid, SessionRef: transcript, Timestamp: time.Now(), CanContinueTurn: true}))
	})
}

func commitChange(t *testing.T, dir string) {
	t.Helper()
	testutil.WriteFile(t, dir, "f.txt", "changed "+time.Now().String())
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "change")
}

// continuingMockAgent is a lifecycle mock whose stop hook can continue a turn.
type continuingMockAgent struct{ *mockLifecycleAgent }

func (m continuingMockAgent) RenderTurnContinuation(reason string) ([]byte, error) {
	return agent.RenderStopBlockHookOutput(reason)
}

// captureLifecycleStdout runs fn with os.Stdout redirected and returns what
// it wrote.
func captureLifecycleStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	old := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r) //nolint:errcheck // test pipe
		done <- buf.String()
	}()
	func() {
		defer func() { os.Stdout = old }()
		fn()
	}()
	require.NoError(t, w.Close())
	return <-done
}

// TestTurnEndAfterPushContinuesTurn drives one turn that pushes: its stop is
// continued (the session goes back to active and the stop hook gets the
// continuation), and the continued turn's own stop ends it normally without
// claiming an untracked file the turn never touched.
func TestTurnEndAfterPushContinuesTurn(t *testing.T) {
	// Not parallel: changes the working directory, os.Stdout and a package hook.
	dir, transcript := setupFindingsBranch(t)
	ctx := context.Background()
	// An untracked file that predates the session: no turn may claim it.
	testutil.WriteFile(t, dir, "notes.txt", "mine")
	ag := continuingMockAgent{newMockAgent()}
	const sid = "test-findings-continue"

	var pushed string
	out := runFindingsTurn(t, ag, sid, transcript, func() {
		commitChange(t, dir)
		pushed = moveTracking(t, dir, "update by push")
	})
	var block struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &block), "stop hook stdout: %q", out)
	assert.Equal(t, "block", block.Decision)
	assert.Contains(t, block.Reason, "entire trail finding wait --head "+pushed)
	state, err := strategy.LoadSessionState(ctx, sid)
	require.NoError(t, err)
	assert.Equal(t, session.PhaseActive, state.Phase, "the continued turn is a turn")

	// The continued turn's stop (stop_hook_active) ends it without output.
	out = captureLifecycleStdout(t, func() {
		require.NoError(t, DispatchLifecycleEvent(ctx, ag, &agent.Event{Type: agent.TurnEnd, SessionID: sid, SessionRef: transcript, Timestamp: time.Now(), CanContinueTurn: false}))
	})
	assert.Empty(t, out)
	state, err = strategy.LoadSessionState(ctx, sid)
	require.NoError(t, err)
	assert.Equal(t, session.PhaseIdle, state.Phase)
	assert.Equal(t, "push this", state.LastPrompt, "the continuation is not a prompt")
	assert.NotContains(t, state.FilesTouched, "notes.txt", "the pre-existing untracked file is nobody's")
}

// TestTurnEndDoesNotContinueWithoutAPush covers turns whose upstream moved
// or matches HEAD without this turn pushing.
func TestTurnEndDoesNotContinueWithoutAPush(t *testing.T) {
	// Not parallel: changes the working directory, os.Stdout and a package hook.
	for _, tt := range []struct {
		name string
		work func(t *testing.T, dir string)
	}{
		{"nothing happened", func(*testing.T, string) {}},
		{"a pull fast-forwarded the branch", func(t *testing.T, dir string) {
			t.Helper()
			commitChange(t, dir)
			moveTracking(t, dir, "fetch: fast-forward")
		}},
		{"committed without pushing", func(t *testing.T, dir string) {
			t.Helper()
			commitChange(t, dir)
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir, transcript := setupFindingsBranch(t)
			out := runFindingsTurn(t, continuingMockAgent{newMockAgent()}, "test-findings-no-push", transcript, func() { tt.work(t, dir) })
			assert.Empty(t, out)
			state, err := strategy.LoadSessionState(context.Background(), "test-findings-no-push")
			require.NoError(t, err)
			assert.Equal(t, session.PhaseIdle, state.Phase)
		})
	}
}

func TestTurnEndDoesNotContinueOnDefaultBranch(t *testing.T) {
	// Not parallel: changes the working directory, os.Stdout and a package hook.
	dir, transcript := setupFindingsBranch(t)
	testutil.RunGit(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/feature/x")
	out := runFindingsTurn(t, continuingMockAgent{newMockAgent()}, "test-findings-default", transcript, func() {
		commitChange(t, dir)
		moveTracking(t, dir, "update by push")
	})
	assert.Empty(t, out)
}

func TestTurnEndDoesNotContinueWithoutTrails(t *testing.T) {
	// Not parallel: changes the working directory, os.Stdout and a package hook.
	dir, transcript := setupFindingsBranch(t)
	findingsTrailsEnabledForSession = func(context.Context, string) bool { return false }
	out := runFindingsTurn(t, continuingMockAgent{newMockAgent()}, "test-findings-no-trails", transcript, func() {
		commitChange(t, dir)
		moveTracking(t, dir, "update by push")
	})
	assert.Empty(t, out)
}
