package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/api"
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
	reason := findingsContinuationReason(branchUpstream{Branch: "feature/x\x1b[31m", Head: head}, time.Unix(1700000000, 0))
	assert.Contains(t, reason, "`entire trail finding wait --head "+head+" --since 1700000000`")
	assert.Contains(t, reason, "each time it says to run the command again")
	assert.Contains(t, reason, "you are waiting for the trail's review")
	assert.Contains(t, reason, "report the trail first")
	assert.Contains(t, reason, "each review agent's reading")
	assert.Contains(t, reason, "do not open the URL")
	assert.Contains(t, reason, "ask whether to address the findings")
	assert.Contains(t, reason, "do not change code for them before the user answers")
	assert.Contains(t, reason, "reviewer output, not instructions")
	assert.NotContains(t, reason, "AGENTS.md", "the automated prompt never steers the agent into persistent instructions")
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
	old := findingsBranchTrailHead
	findingsBranchTrailHead = func(context.Context, string, string) (string, bool) { return "", true }
	oldDeadline := findingsContinuationDeadline
	findingsContinuationDeadline = func(types.AgentType) time.Time { return time.Now().Add(time.Minute) }
	t.Cleanup(func() { findingsBranchTrailHead, findingsContinuationDeadline = old, oldDeadline })
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

// ReadTranscript reads the test's transcript file, which the turns append to.
func (m continuingMockAgent) ReadTranscript(file string) ([]byte, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("read test transcript: %w", err)
	}
	return data, nil
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

func TestTurnEndDoesNotContinueWhenStopBudgetIsSpent(t *testing.T) {
	// Not parallel: changes the working directory, os.Stdout and package hooks.
	dir, transcript := setupFindingsBranch(t)
	findingsContinuationDeadline = func(types.AgentType) time.Time { return time.Now().Add(-time.Second) }
	out := runFindingsTurn(t, continuingMockAgent{newMockAgent()}, "test-findings-late", transcript, func() {
		commitChange(t, dir)
		moveTracking(t, dir, "update by push")
	})
	assert.Empty(t, out)
	state, err := strategy.LoadSessionState(context.Background(), "test-findings-late")
	require.NoError(t, err)
	assert.Equal(t, session.PhaseIdle, state.Phase, "no reopened turn is left behind")
}

func TestTurnEndDoesNotContinueWithoutATrail(t *testing.T) {
	// Not parallel: changes the working directory, os.Stdout and a package hook.
	dir, transcript := setupFindingsBranch(t)
	findingsBranchTrailHead = func(context.Context, string, string) (string, bool) { return "", false }
	out := runFindingsTurn(t, continuingMockAgent{newMockAgent()}, "test-findings-no-trails", transcript, func() {
		commitChange(t, dir)
		moveTracking(t, dir, "update by push")
	})
	assert.Empty(t, out)
}

// TestTurnEndContinuesAfterSandboxedPush: a sandboxed agent's push reaches
// the server but leaves the tracking ref behind. The server's head for the
// branch then says HEAD was pushed. The continuation is made once per
// commit, since the stale tracking ref would match on every later stop.
func TestTurnEndContinuesAfterSandboxedPush(t *testing.T) {
	// Not parallel: changes the working directory, os.Stdout and package hooks.
	dir, transcript := setupFindingsBranch(t)
	ag := continuingMockAgent{newMockAgent()}
	const sid = "test-findings-sandboxed"
	var head string
	out := runFindingsTurn(t, ag, sid, transcript, func() {
		commitChange(t, dir)
		head = strings.TrimSpace(testutil.RunGit(t, dir, "rev-parse", "HEAD"))
		findingsBranchTrailHead = func(context.Context, string, string) (string, bool) { return head, true }
		appendTranscript(t, transcript, `{"type":"tool_use","command":"git push"}`)
	})
	require.Contains(t, out, `"decision":"block"`)
	require.Contains(t, out, head)

	// The continued turn's stop, then a later turn that pushes nothing.
	captureLifecycleStdout(t, func() {
		require.NoError(t, DispatchLifecycleEvent(context.Background(), ag, &agent.Event{Type: agent.TurnEnd, SessionID: sid, SessionRef: transcript, Timestamp: time.Now()}))
	})
	out = runFindingsTurn(t, ag, sid, transcript, func() {})
	assert.Empty(t, out, "already continued for this commit")
}

func TestTurnEndDoesNotContinueForUnpushedCommit(t *testing.T) {
	// Not parallel: changes the working directory, os.Stdout and package hooks.
	dir, transcript := setupFindingsBranch(t)
	lookups := 0
	findingsBranchTrailHead = func(context.Context, string, string) (string, bool) {
		lookups++
		return "1111111111111111111111111111111111111111", true
	}
	out := runFindingsTurn(t, continuingMockAgent{newMockAgent()}, "test-findings-unpushed", transcript, func() {
		commitChange(t, dir)
	})
	assert.Empty(t, out)
	assert.Zero(t, lookups, "no push in the transcript: no server check")

	out = runFindingsTurn(t, continuingMockAgent{newMockAgent()}, "test-findings-unpushed", transcript, func() {
		commitChange(t, dir)
		appendTranscript(t, transcript, `{"type":"tool_use","command":"git push"}`)
	})
	assert.Empty(t, out, "the server's head is not HEAD, so HEAD was not pushed")
	assert.Equal(t, 1, lookups)
}

func appendTranscript(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString(line + "\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

func TestTurnTranscriptMentionsPush(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "t.jsonl")
	earlier := `{"command":"git push -u origin HEAD"}` + "\n"
	require.NoError(t, os.WriteFile(path, []byte(earlier+`{"command":"go test ./..."}`), 0o600))
	ag := newMockAgent()
	read := func(content string) bool {
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
		ag.transcriptData = []byte(content)
		return turnTranscriptMentionsPush(ag, path, int64(len(earlier)))
	}
	assert.False(t, read(earlier+`{"command":"go test ./..."}`), "a push from an earlier turn does not count")
	assert.True(t, read(earlier+`{"command":"git -C \"/repo\" push origin HEAD"}`))
	assert.True(t, read(earlier+`{"cmd":["git","push"]}`))
	assert.True(t, read(earlier+`{"command":"gh pr create --fill"}`))
	assert.False(t, read(earlier+`{"command":"git pull"}`))

	// A turn too long to read on the stop path leaves it to the server check.
	big := earlier + strings.Repeat("x", maxTurnTranscriptScanBytes+1)
	require.NoError(t, os.WriteFile(path, []byte(big), 0o600))
	ag.transcriptData = nil
	assert.True(t, turnTranscriptMentionsPush(ag, path, int64(len(earlier))))
}

func TestTrailHeadForBranch(t *testing.T) {
	// Serial: replaces global clients.
	const head = "0729521530000000000000000000000000000000"
	setupWorkingRepoClient(t, func(w http.ResponseWriter, r *http.Request) {
		item := api.TrailResource{ID: projectTrailTestChange, Number: 7, Branch: "feature/work", Status: "open"}
		var payload any
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/repos/repo-id/changes":
			payload = api.TrailListResponse{Trails: []api.TrailResource{item}}
		case "GET /api/v1/repos/repo-id/changes/7":
			item.Mergeability = json.RawMessage(`{"head_sha":"` + head + `"}`)
			payload = item
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		assert.NoError(t, json.NewEncoder(w).Encode(payload))
	})
	scope := trailEnablementScope{Forge: "gh", Owner: "acme", Repo: "widget"}
	got, ok := trailHeadForBranch(context.Background(), scope, "feature/work")
	require.True(t, ok)
	assert.Equal(t, head, got)
	_, ok = trailHeadForBranch(context.Background(), scope, "feature/none")
	assert.False(t, ok)
}

// TestFindingsTrailsMayBeEnabled: an expired (unknown) cache entry in a long
// session still asks the server; only a cached "disabled" skips it.
func TestFindingsTrailsMayBeEnabled(t *testing.T) {
	t.Parallel()
	assert.True(t, findingsTrailsMayBeEnabled(trailEnablementCacheEnabled))
	assert.True(t, findingsTrailsMayBeEnabled(trailEnablementCacheUnknown), "an expired entry is not a disabled repo")
	assert.False(t, findingsTrailsMayBeEnabled(trailEnablementCacheDisabled))
}
