package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/tokenreport"
)

const (
	// sessionTokensLiveDuration is the state-derived duration of the live
	// fixtures; the transcript's own 10s span wins whenever it is attributed.
	sessionTokensLiveDuration = 6*time.Hour + 10*time.Minute
	// sessionTokensTestSubagent is the subagent type of the fixture's Agent call.
	sessionTokensTestSubagent = "Explore"
)

// writeSessionTokensTranscript writes the Claude attribution fixture as a
// live transcript <dir>/<sessionID>.jsonl and, when subagentModel is set, a
// two-call subagent transcript for the fixture's Explore call (agent id
// "abc") on that model under the session's subagents directory. Returns the
// transcript path.
func writeSessionTokensTranscript(t *testing.T, sessionID, subagentModel string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, sessionID+".jsonl")
	if err := os.WriteFile(path, checkpointTokensClaudeFixture, 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	if subagentModel == "" {
		return path
	}
	subDir := paths.SubagentsDir(dir, sessionID)
	if err := os.MkdirAll(subDir, 0o750); err != nil {
		t.Fatalf("mkdir subagents: %v", err)
	}
	sub := fmt.Sprintf(`{"type":"user","uuid":"su-0","timestamp":"2026-08-27T10:00:07.100Z","message":{"role":"user","content":"Synthetic subagent prompt."}}
{"type":"assistant","uuid":"sa-1","timestamp":"2026-08-27T10:00:07.400Z","message":{"id":"msg_sub1","model":%[1]q,"role":"assistant","content":[{"type":"text","text":"Looking."}],"usage":{"input_tokens":5,"cache_creation_input_tokens":40,"cache_read_input_tokens":300,"output_tokens":50}}}
{"type":"assistant","uuid":"sa-2","timestamp":"2026-08-27T10:00:07.900Z","message":{"id":"msg_sub2","model":%[1]q,"role":"assistant","content":[{"type":"text","text":"Found it."}],"usage":{"input_tokens":7,"cache_creation_input_tokens":0,"cache_read_input_tokens":340,"output_tokens":60}}}
`, subagentModel)
	if err := os.WriteFile(filepath.Join(subDir, paths.AgentTranscriptFileName("abc")), []byte(sub), 0o600); err != nil {
		t.Fatalf("write subagent transcript: %v", err)
	}
	return path
}

// liveSessionTokensState is an active Claude Code session whose state
// carries a stale running total (999.9k input, 99 calls) and model, so a
// report that recomputes from the transcript is visibly different from one
// that does not.
func liveSessionTokensState(id, transcriptPath string) *strategy.SessionState {
	state := makeSessionState(id, session.PhaseActive)
	state.AgentType = agent.AgentTypeClaudeCode
	state.ModelName = testModelClaudeOpus
	state.TranscriptPath = transcriptPath
	state.StartedAt = time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC)
	last := state.StartedAt.Add(sessionTokensLiveDuration)
	state.LastInteractionTime = &last
	state.ContextTokens = 8500
	state.ContextWindowSize = 10000
	state.TokenUsage = &agent.TokenUsage{InputTokens: 999_900, APICallCount: 99}
	return state
}

// readLogDir returns the concatenated contents of every file a file-backed
// logger wrote under dir; "" when it wrote nothing.
func readLogDir(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	var b strings.Builder
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		b.Write(data)
	}
	return b.String()
}

// runSessionTokensCmd saves state and runs `session tokens <id> args...`,
// returning stdout.
func runSessionTokensCmd(ctx context.Context, t *testing.T, state *strategy.SessionState, args ...string) string {
	t.Helper()
	if err := strategy.SaveSessionState(ctx, state); err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}
	cmd := newTokensCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs(append([]string{state.SessionID}, args...))
	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("session tokens %v: %v\n%s", args, err, stdout.String())
	}
	return stdout.String()
}

func TestSessionTokens_TextRecomputesFromLiveTranscript(t *testing.T) {
	setupStopTestRepo(t)
	ctx := context.Background()
	state := liveSessionTokensState("live-tokens-text", writeSessionTokensTranscript(t, "live-tokens-text", checkpointTokensFixtureModel))

	out := runSessionTokensCmd(ctx, t, state)

	assertContainsAll(t, out,
		"Session tokens",
		"Status:   active",
		"Context:  85% full (8.5k of 10k)",
		// Where it went: the subagent record (on the parent model) leads.
		"Where it went",
		"est. cost share",
		"Subagent: Explore",
		"Context replay (cache read)",
		"Bash · during systematic-debugging",
		// Usage: Σ calls + Σ subagents, not the stale state total.
		"Usage",
		"Input (fresh)",
		"Cache write",
		"of which 1-hour",
		"Cache read",
		"Output",
		"of which thinking",
		"Total",
		"4.9k",
		"of which subagents",
		"802",
		"Recommendations",
		"Explore subagents ran on `claude-fable-5`",
		"Notes",
		"Cost shares use Anthropic list-price ratios",
		"1 call with no usage recorded",
	)
	// The modal call model replaces the stale state model in the header; the
	// transcript's span replaces the state-derived duration.
	assertLineContainsAll(t, out, "Session:  live-tokens-text", "Agent: Claude Code", "Model: claude-fable-5")
	assertLineContainsAll(t, out, "Duration: 10s so far · 3 API calls · 4.9k tokens", "Effort: high (4 calls)")
	for _, absent := range []string{"999.9k", testModelClaudeOpus, "6h 10m", "Likely contributors", "Limitations", "totals from session state", "Pattern:"} {
		if strings.Contains(out, absent) {
			t.Errorf("did not expect %q in output, got:\n%s", absent, out)
		}
	}
	whereIdx, usageIdx, recIdx, notesIdx := strings.Index(out, "Where it went"), strings.Index(out, "\nUsage"), strings.Index(out, "Recommendations"), strings.Index(out, "Notes")
	if whereIdx >= usageIdx || usageIdx >= recIdx || recIdx >= notesIdx {
		t.Fatalf("sections out of order (where=%d usage=%d rec=%d notes=%d):\n%s", whereIdx, usageIdx, recIdx, notesIdx, out)
	}
	assertRecommendationFiguresVisible(t, out)
}

func TestSessionTokens_JSONShapeFromLiveTranscript(t *testing.T) {
	setupStopTestRepo(t)
	ctx := context.Background()
	state := liveSessionTokensState("live-tokens-json", writeSessionTokensTranscript(t, "live-tokens-json", checkpointTokensFixtureModel))

	out := runSessionTokensCmd(ctx, t, state, "--json")

	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	for _, key := range []string{"session_id", "agent", "model", "status", "source", "duration_seconds", "effort", "tokens", "context", "cost", "contributors", "recommendations", "limitations"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("missing JSON key %q in:\n%s", key, out)
		}
	}
	for _, key := range []string{"legacy", "agent_reported_cost", "comparison"} {
		if _, ok := raw[key]; ok {
			t.Errorf("did not expect JSON key %q in:\n%s", key, out)
		}
	}

	var result sessionTokensReport
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.SessionID != "live-tokens-json" || result.Agent != testAgentClaude || result.Model != checkpointTokensFixtureModel || result.Status != string(session.PhaseActive) {
		t.Errorf("identity = %q %q %q %q", result.SessionID, result.Agent, result.Model, result.Status)
	}
	if result.Source != tokenSourceTranscript {
		t.Errorf("source = %q, want transcript", result.Source)
	}
	if result.DurationSeconds != 10 {
		t.Errorf("duration_seconds = %d, want 10 (the transcript span)", result.DurationSeconds)
	}
	if result.Effort == nil || result.Effort.Value != checkpointTokensFixtureEffort || result.Effort.Calls != 4 {
		t.Errorf("effort = %+v, want high/4", result.Effort)
	}
	// Parent: 60 / 380 (50 of them 1h) / 3500 / 115 over 3 calls, 17 thinking;
	// subagent: 12 / 40 / 640 / 110 over 2 calls.
	want := tokenUsageJSON{Total: 4857, Input: 72, CacheRead: 4140, CacheWrite: 420, Output: 225, APICalls: 5, SubagentTotal: 802, ThinkingTokens: 17, CacheCreation1hTokens: 50}
	if result.Tokens == nil || *result.Tokens != want {
		t.Errorf("tokens = %+v, want %+v", result.Tokens, want)
	}
	if result.Context == nil || result.Context.Percent != 85 || result.Context.Tokens != 8500 || result.Context.WindowSize != 10000 {
		t.Errorf("context = %+v, want 85%% of 10000", result.Context)
	}
	if result.Cost == nil || result.Cost.Provider != tokenreport.ProviderAnthropic || result.Cost.Weights == nil || result.Cost.Shares.CacheWriteUnpriced {
		t.Errorf("cost = %+v, want Anthropic weights with cache writes priced", result.Cost)
	}
	if len(result.Contributors) == 0 || result.Contributors[0].Kind != tokenreport.KindSubagent || result.Contributors[0].Label != sessionTokensTestSubagent {
		t.Errorf("contributors = %+v, want the Explore subagent row first", result.Contributors)
	}
	if len(result.Recommendations) != 1 {
		t.Fatalf("recommendations = %+v, want one", result.Recommendations)
	}
	rec := result.Recommendations[0]
	if rec.Cause != tokenreport.CauseSubagentModel || rec.ID != string(rec.Cause) || rec.Message != rec.Text || rec.Kind != tokenreport.RecommendationKindSession {
		t.Errorf("recommendation = %+v", rec)
	}
	if !strings.Contains(strings.Join(result.Limitations, "\n"), "Anthropic list-price ratios") {
		t.Errorf("limitations = %+v, want the pricing note", result.Limitations)
	}
}

func TestSessionTokens_AgentBriefFromLiveTranscript(t *testing.T) {
	setupStopTestRepo(t)
	ctx := context.Background()
	state := liveSessionTokensState("live-tokens-brief", writeSessionTokensTranscript(t, "live-tokens-brief", checkpointTokensFixtureModel))

	out := runSessionTokensCmd(ctx, t, state, "--agent-brief")

	assertContainsAll(t, out,
		"Session token brief\nSession: live-tokens-brief\n",
		"Token usage: 4.9k total; 3 API calls; 10s; cache read",
		"of cost.",
		"Next best action:\nExplore subagents ran on `claude-fable-5`",
		"Signals:\n- subagent_model\n",
	)
	for _, absent := range []string{"Pattern:", "Where it went", "Recommendations", "Notes", "Usage\n"} {
		if strings.Contains(out, absent) {
			t.Errorf("did not expect %q in the brief, got:\n%s", absent, out)
		}
	}
}

func TestSessionTokens_FallsBackToSessionStateWhenTranscriptUnreadable(t *testing.T) {
	setupStopTestRepo(t)
	ctx := context.Background()
	state := liveSessionTokensState("state-tokens", filepath.Join(t.TempDir(), "missing.jsonl"))
	state.ModelName = checkpointTokensFixtureModel
	state.TokenUsage = &agent.TokenUsage{
		InputTokens: 1000, CacheReadTokens: 10000, CacheCreationTokens: 500, OutputTokens: 100, APICallCount: 6,
		SubagentTokens: &agent.TokenUsage{InputTokens: 1000, OutputTokens: 1000, APICallCount: 2},
	}

	out := runSessionTokensCmd(ctx, t, state)

	assertContainsAll(t, out,
		"Status:   active",
		"Usage",
		"Input (fresh)",
		"2k",
		"Cache read",
		"10k",
		"Total",
		"13.6k",
		"of which subagents",
		"Notes",
		"transcript unavailable; totals from session state",
		"cache-write TTL not recorded; not priced",
	)
	assertLineContainsAll(t, out, "Session:  state-tokens", "Agent: Claude Code", "Model: claude-fable-5")
	// Duration is the state's span; calls exclude the subagent's two.
	assertLineContainsAll(t, out, "Duration: 6h 10m so far · 6 API calls · 13.6k tokens")
	if strings.Contains(out, "Where it went") {
		t.Errorf("no breakdown without a transcript:\n%s", out)
	}

	jsonOut := runSessionTokensCmd(ctx, t, state, "--json")
	var result sessionTokensReport
	if err := json.Unmarshal([]byte(jsonOut), &result); err != nil {
		t.Fatalf("decode: %v\n%s", err, jsonOut)
	}
	if result.Source != sessionTokensSourceState {
		t.Errorf("source = %q, want session_state", result.Source)
	}
	if result.DurationSeconds != int(sessionTokensLiveDuration/time.Second) {
		t.Errorf("duration_seconds = %d", result.DurationSeconds)
	}
	want := tokenUsageJSON{Total: 13600, Input: 2000, CacheRead: 10000, CacheWrite: 500, Output: 1100, APICalls: 8, SubagentTotal: 2000}
	if result.Tokens == nil || *result.Tokens != want {
		t.Errorf("tokens = %+v, want %+v", result.Tokens, want)
	}
	if result.Contributors == nil || len(result.Contributors) != 0 {
		t.Errorf("contributors = %v, want an empty array", result.Contributors)
	}
	if !strings.Contains(strings.Join(result.Limitations, "\n"), "transcript unavailable; totals from session state") {
		t.Errorf("limitations = %+v", result.Limitations)
	}
}

func TestSessionTokens_UnreadableTranscriptWarnOmitsPath(t *testing.T) {
	setupStopTestRepo(t)
	logDir := t.TempDir()
	l, err := logging.New(logging.Config{Dir: logDir})
	if err != nil {
		t.Fatalf("logging.New() error = %v", err)
	}
	ctx := logging.WithLogger(context.Background(), l)
	transcriptPath := filepath.Join(t.TempDir(), "secret-project-name.jsonl")
	state := liveSessionTokensState("warn-tokens", transcriptPath)

	out := runSessionTokensCmd(ctx, t, state)
	if err := l.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	assertContainsAll(t, out, "transcript unavailable; totals from session state")

	logged := readLogDir(t, logDir)
	assertContainsAll(t, logged, "session tokens: transcript unreadable", `"session_id":"warn-tokens"`, `"agent":"Claude Code"`, `"reason":"not_found"`)
	for _, absent := range []string{transcriptPath, "secret-project-name", `"error"`} {
		if strings.Contains(logged, absent) {
			t.Errorf("the log must not carry %q:\n%s", absent, logged)
		}
	}
}

func TestSessionTokens_NothingRecorded(t *testing.T) {
	setupStopTestRepo(t)
	logDir := t.TempDir()
	l, err := logging.New(logging.Config{Dir: logDir})
	if err != nil {
		t.Fatalf("logging.New() error = %v", err)
	}
	ctx := logging.WithLogger(context.Background(), l)
	state := makeSessionState("empty-tokens", session.PhaseIdle)
	state.AgentType = agent.AgentTypeGemini
	state.ContextTokens = 9000
	state.ContextWindowSize = 10000

	out := runSessionTokensCmd(ctx, t, state)
	assertContainsAll(t, out, "Status:   idle", "Context:  90% full (9k of 10k)", "Token usage: not recorded", "no transcript recorded; totals from session state")
	assertLineContainsAll(t, out, "Duration: not recorded · token usage not recorded")
	if strings.Contains(out, "Recommendations") || strings.Contains(out, "Where it went") || strings.Contains(out, "so far") {
		t.Errorf("nothing to recommend or attribute without usage, and no duration to say 'so far' about:\n%s", out)
	}
	// A state that never recorded a transcript path is normal: no warn.
	if err := l.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if logged := readLogDir(t, logDir); strings.Contains(logged, "session tokens:") {
		t.Errorf("no warning is due for a session without a transcript path, got:\n%s", logged)
	}

	brief := runSessionTokensCmd(ctx, t, state, "--agent-brief")
	assertContainsAll(t, brief, "Session token brief", "Session: empty-tokens", "Token usage: unavailable.", "Next best action:", "Token usage is not recorded here", "- token usage not recorded")

	jsonOut := runSessionTokensCmd(ctx, t, state, "--json")
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(jsonOut), &raw); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, jsonOut)
	}
	for _, key := range []string{"tokens", "cost", "effort", "recommendations", "duration_seconds"} {
		if _, ok := raw[key]; ok {
			t.Errorf("did not expect JSON key %q without usage:\n%s", key, jsonOut)
		}
	}
	for _, key := range []string{"context", "limitations", "contributors", "source"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("missing JSON key %q in:\n%s", key, jsonOut)
		}
	}
}

func TestSessionTokens_UnknownAgentIsANote(t *testing.T) {
	setupStopTestRepo(t)
	ctx := context.Background()
	state := makeSessionState("odd-agent-tokens", session.PhaseActive)
	state.AgentType = "Mystery Agent"
	state.TokenUsage = &agent.TokenUsage{InputTokens: 42, APICallCount: 1}

	out := runSessionTokensCmd(ctx, t, state)
	assertContainsAll(t, out, "Agent: Mystery Agent", `agent "Mystery Agent" is not known to this CLI; totals from session state`, "42")
}

func TestWriteSessionTokensHeader_EndedSessionDropsSoFar(t *testing.T) {
	t.Parallel()

	report := sessionTokensReport{SessionID: "done", Agent: testAgentClaude, Model: "claude-fable-5", Status: string(session.PhaseEnded), ended: true}
	report.applyView(claudeView(wideAttributed()))
	var b strings.Builder
	writeSessionTokensHeader(&b, &report)
	out := b.String()
	assertContainsAll(t, out, "Session:  done      Agent: Claude Code      Model: claude-fable-5\n", "Status:   ended\n", "Duration: 9h 42m · 43 API calls · 4.2M tokens      Effort: high (43 calls)\n")
	if strings.Contains(out, "so far") || strings.Contains(out, "Context:") {
		t.Errorf("no 'so far' on an ended session and no Context line without one:\n%s", out)
	}

	report.Status, report.ended = string(session.PhaseActive), false
	report.Context = buildSessionTokensContext(6200, 10000)
	b.Reset()
	writeSessionTokensHeader(&b, &report)
	assertContainsAll(t, b.String(), "Duration: 9h 42m so far · 43 API calls", "Context:  62% full (6.2k of 10k)")
}

func TestSessionStateDuration(t *testing.T) {
	t.Parallel()

	started := time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC)
	later := started.Add(90 * time.Minute)
	earlier := started.Add(-time.Minute)
	cases := map[string]struct {
		state *strategy.SessionState
		want  time.Duration
	}{
		"hook-reported wins":      {&strategy.SessionState{SessionDurationMs: 5000, StartedAt: started, LastInteractionTime: &later}, 5 * time.Second},
		"derived from timestamps": {&strategy.SessionState{StartedAt: started, LastInteractionTime: &later}, 90 * time.Minute},
		"ended without hooks":     {&strategy.SessionState{StartedAt: started, EndedAt: &later}, 90 * time.Minute},
		"clock skew clamps to 0":  {&strategy.SessionState{StartedAt: started, LastInteractionTime: &earlier}, 0},
		"no end yet":              {&strategy.SessionState{StartedAt: started}, 0},
	}
	for name, tc := range cases {
		if got := sessionStateDuration(tc.state); got != tc.want {
			t.Errorf("%s: sessionStateDuration = %v, want %v", name, got, tc.want)
		}
	}
}

func TestSessionUnmatchedSubagentNote(t *testing.T) {
	t.Parallel()

	if got := sessionUnmatchedSubagentNote(agent.AgentTypeCodex, 1); got != unmatchedSubagentNote(agent.AgentTypeCodex, 1) {
		t.Errorf("codex = %q, want the shared separate-sessions wording", got)
	}
	if got := sessionUnmatchedSubagentNote(agent.AgentTypeClaudeCode, 2); got != "2 subagent calls have no transcript in the subagents directory; that usage is not included." {
		t.Errorf("claude = %q", got)
	}
	if got := sessionUnmatchedSubagentNote(agent.AgentTypeClaudeCode, 1); !strings.HasPrefix(got, "1 subagent call has ") {
		t.Errorf("singular = %q", got)
	}
}

func TestResolveTokenAttributor(t *testing.T) {
	t.Parallel()

	if _, reason, ok := resolveTokenAttributor(""); ok || reason != "no agent recorded" {
		t.Errorf("empty agent = %q %v", reason, ok)
	}
	if _, reason, ok := resolveTokenAttributor(types.AgentType("Mystery Agent")); ok || reason != `agent "Mystery Agent" is not known to this CLI` {
		t.Errorf("unknown agent = %q %v", reason, ok)
	}
	// Cursor records session totals only: no attributor, and no note due.
	if _, reason, ok := resolveTokenAttributor(agent.AgentTypeCursor); ok || reason != "" {
		t.Errorf("cursor = %q %v, want no attributor and no reason", reason, ok)
	}
	if attributor, reason, ok := resolveTokenAttributor(agent.AgentTypeClaudeCode); !ok || attributor == nil || reason != "" {
		t.Errorf("claude = %v %q %v", attributor, reason, ok)
	}
}
