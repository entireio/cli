package codex

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/review"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

// Compile-time interface check: ReviewerTemplate implements AgentReviewer.
var _ reviewtypes.AgentReviewer = (*reviewtypes.ReviewerTemplate)(nil)

const wantCodexAgentName = "codex"

// TestCodexReviewer_NameMatchesRegistryKey locks the reviewer's name to the
// agent registry's stable key. adoptReviewEnv compares ENTIRE_REVIEW_AGENT
// against string(ag.Name()); drift here silently breaks review-session
// tagging for this agent.
func TestCodexReviewer_NameMatchesRegistryKey(t *testing.T) {
	t.Parallel()
	if wantCodexAgentName != string(agent.AgentNameCodex) {
		t.Fatalf("wantCodexAgentName = %q, agent.AgentNameCodex = %q — keep these aligned",
			wantCodexAgentName, string(agent.AgentNameCodex))
	}
}

func TestCodexReviewer_Name(t *testing.T) {
	t.Parallel()
	r := NewReviewer()
	if got := r.Name(); got != wantCodexAgentName {
		t.Errorf("Name() = %q, want %q", got, wantCodexAgentName)
	}
}

func TestCodexReviewer_EnvVarsSet(t *testing.T) {
	t.Parallel()
	cfg := reviewtypes.RunConfig{
		Skills:       []string{"/codex:review", "/test-auditor"},
		AlwaysPrompt: "Always check error handling.",
		PerRunPrompt: "Focus on the storage layer.",
		StartingSHA:  "deadbeef1234",
	}
	cmd := buildCodexReviewCmd(context.Background(), cfg)

	wantKeys := []string{
		review.EnvSession,
		review.EnvAgent,
		review.EnvSkills,
		review.EnvPrompt,
		review.EnvStartingSHA,
	}
	envMap := envToMap(cmd.Env)

	for _, key := range wantKeys {
		if _, ok := envMap[key]; !ok {
			t.Errorf("env var %s not set on cmd", key)
		}
	}

	if envMap[review.EnvSession] != "1" {
		t.Errorf("%s = %q, want %q", review.EnvSession, envMap[review.EnvSession], "1")
	}
	if envMap[review.EnvAgent] != wantCodexAgentName {
		t.Errorf("%s = %q, want %q", review.EnvAgent, envMap[review.EnvAgent], wantCodexAgentName)
	}
	if envMap[review.EnvStartingSHA] != "deadbeef1234" {
		t.Errorf("%s = %q, want %q", review.EnvStartingSHA, envMap[review.EnvStartingSHA], "deadbeef1234")
	}
	if !strings.HasPrefix(envMap[review.EnvSkills], "[") {
		t.Errorf("%s = %q, want JSON array", review.EnvSkills, envMap[review.EnvSkills])
	}
}

func TestCodexReviewer_ArgvShape(t *testing.T) {
	t.Parallel()
	cfg := reviewtypes.RunConfig{Skills: []string{"/skill"}}
	cmd := buildCodexReviewCmd(context.Background(), cfg)

	// Expect: codex exec --skip-git-repo-check --json -
	want := []string{wantCodexAgentName, "exec", "--skip-git-repo-check", "--json", "-"}
	if len(cmd.Args) != len(want) {
		t.Fatalf("len(Args) = %d, want %d: %v", len(cmd.Args), len(want), cmd.Args)
	}
	for i, w := range want {
		if cmd.Args[i] != w {
			t.Errorf("Args[%d] = %q, want %q", i, cmd.Args[i], w)
		}
	}
	// Stdin must be non-nil — codex reads prompt from stdin.
	if cmd.Stdin == nil {
		t.Error("cmd.Stdin is nil; codex requires prompt via stdin")
	}
}

func TestCodexReviewer_BuiltinReviewExpandsToScopedExecPrompt(t *testing.T) {
	t.Parallel()
	cfg := reviewtypes.RunConfig{
		Skills:            []string{"/review"},
		AlwaysPrompt:      "Focus on auth regressions.",
		ScopeBaseRef:      "main",
		CheckpointContext: "Commits in scope (newest first):\n  abc123 summary\n",
	}
	cmd := buildCodexReviewCmd(context.Background(), cfg)

	want := []string{wantCodexAgentName, "exec", "--skip-git-repo-check", "--json", "-"}
	if len(cmd.Args) != len(want) {
		t.Fatalf("len(Args) = %d, want %d: %v", len(cmd.Args), len(want), cmd.Args)
	}
	for i, w := range want {
		if cmd.Args[i] != w {
			t.Errorf("Args[%d] = %q, want %q", i, cmd.Args[i], w)
		}
	}

	prompt := readCodexCmdStdin(t, cmd)
	if strings.Contains(prompt, "/review") {
		t.Fatalf("builtin review prompt should not include raw /review:\n%s", prompt)
	}
	for _, wantText := range []string{
		"Review the current branch changes and report actionable findings.",
		"Focus on auth regressions.",
		"Scope: review the commits unique to this branch vs main, plus any uncommitted changes in the working tree. Ignore code outside this scope.",
		"Commits in scope (newest first):",
		"abc123 summary",
	} {
		if !strings.Contains(prompt, wantText) {
			t.Fatalf("builtin review prompt missing %q:\n%s", wantText, prompt)
		}
	}
}

func TestCodexReviewer_NoBinaryRequiredAtConstruction(t *testing.T) {
	// No t.Parallel — uses t.Setenv.
	t.Setenv("PATH", "")

	r := NewReviewer()
	cfg := reviewtypes.RunConfig{
		Skills:      []string{"/test"},
		StartingSHA: "abc123",
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Construction (NewReviewer) MUST NOT touch PATH. Start may or may
	// not error depending on whether the OS-level cmd.Start tries to
	// resolve before fork — that's fine. The contract is just "no panic
	// and no upfront LookPath call".
	proc, err := r.Start(ctx, cfg)
	// Either Start succeeded (deferred lookup; binary error surfaces in Wait)
	// or Start failed with exec.ErrNotFound (immediate lookup at Cmd.Start).
	// Both satisfy the deferred-lookup contract — what we explicitly DON'T
	// want is a panic or error from NewReviewer itself.
	if err != nil && !errors.Is(err, exec.ErrNotFound) {
		// Tolerate "no such file" wrapping variations
		if !strings.Contains(err.Error(), "executable file not found") &&
			!strings.Contains(err.Error(), "no such file") {
			t.Errorf("unexpected error type: %v", err)
		}
	}
	if proc != nil {
		// Drain events to let parser goroutine exit cleanly.
		drainCodexEvents(proc.Events())
		_ = proc.Wait() //nolint:errcheck // best-effort cleanup in test
	}
}

func TestParseCodexOutput_ReportsScannerError(t *testing.T) {
	t.Parallel()
	// Trigger bufio.Scanner's "token too long" error via parseCodexOutputBuf
	// with a small cap, so we actually exercise the scanner.Err() branch
	// (not the json.Unmarshal-on-a-huge-blob branch the prod 64MB cap would
	// route us into). 8KB of contiguous bytes against a 4KB cap fires
	// ErrTooLong before any newline lets the scanner emit a token.
	const maxBuf = 4 * 1024
	const payload = 8 * 1024
	r, w := io.Pipe()
	go func() {
		defer w.Close()
		_, _ = w.Write(make([]byte, payload)) //nolint:errcheck // best-effort write in test goroutine
	}()

	events := collectCodexEvents(parseCodexOutputBuf(r, maxBuf))

	if len(events) < 2 {
		t.Fatalf("expected at least Started + Finished, got %d events", len(events))
	}
	last := events[len(events)-1]
	fin, ok := last.(reviewtypes.Finished)
	if !ok {
		t.Fatalf("last event must be Finished, got %T", last)
	}
	if fin.Success {
		t.Error("Finished.Success must be false on scanner error")
	}
	// Require a RunError from the scanner branch specifically ("read stdout"
	// prefix), not the unmarshal branch ("codex --json"). Without this the
	// test would pass even if the scanner cap were widened back out and the
	// huge blob just fell through json.Unmarshal — the exact regression the
	// parameterized buffer is meant to prevent.
	sawScannerErr := false
	for _, ev := range events {
		re, ok := ev.(reviewtypes.RunError)
		if !ok {
			continue
		}
		if strings.HasPrefix(re.Err.Error(), "read stdout:") {
			sawScannerErr = true
			break
		}
	}
	if !sawScannerErr {
		t.Errorf("expected RunError from scanner branch (read stdout: ...), got events: %v", events)
	}
}

func TestParseCodexOutput_DecodesJSONStream(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/json_session.jsonl")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	events := collectCodexEvents(parseCodexOutput(strings.NewReader(string(data))))

	if _, ok := events[0].(reviewtypes.Started); !ok {
		t.Errorf("events[0] = %T, want Started", events[0])
	}
	last := events[len(events)-1]
	fin, ok := last.(reviewtypes.Finished)
	if !ok || !fin.Success {
		t.Errorf("last event = %v, want Finished{Success:true}", last)
	}

	var sawTool, sawText bool
	for _, ev := range events {
		if tc, ok := ev.(reviewtypes.ToolCall); ok && tc.Name == "exec" {
			sawTool = true
		}
		if at, ok := ev.(reviewtypes.AssistantText); ok && at.Text == "Hi" {
			sawText = true
		}
	}
	if !sawTool {
		t.Error("expected ToolCall{Name: exec} from item.started/command_execution")
	}
	if !sawText {
		t.Error("expected AssistantText{Text: Hi} from item.completed/agent_message")
	}

	var tokensSeen int
	for _, ev := range events {
		if tk, ok := ev.(reviewtypes.Tokens); ok && tk.Out > 0 {
			tokensSeen++
		}
	}
	if tokensSeen != 1 {
		t.Errorf("Tokens with Out>0 count = %d, want 1", tokensSeen)
	}
}

func TestParseCodexOutput_StreamsEventsBeforeEOF(t *testing.T) {
	t.Parallel()
	pr, pw := io.Pipe()
	events := parseCodexOutput(pr)

	expect := func(t *testing.T, want string) reviewtypes.Event {
		t.Helper()
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatalf("event channel closed waiting for %s", want)
			}
			return ev
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %s — parser did not stream before EOF", want)
			return nil
		}
	}

	// First emitted event is always Started — before we even write anything.
	if _, ok := expect(t, "Started").(reviewtypes.Started); !ok {
		t.Fatal("first event must be Started")
	}

	// Write thread.started — swallowed, so no event read here.
	if _, err := pw.Write([]byte(`{"type":"thread.started","thread_id":"tid"}` + "\n")); err != nil {
		t.Fatalf("pipe write: %v", err)
	}

	// Write item.started/command_execution — expect ToolCall before EOF.
	if _, err := pw.Write([]byte(`{"type":"item.started","item":{"type":"command_execution","command":"git status"}}` + "\n")); err != nil {
		t.Fatalf("pipe write: %v", err)
	}
	ev := expect(t, "ToolCall")
	tc, ok := ev.(reviewtypes.ToolCall)
	if !ok {
		t.Fatalf("event = %T (%+v), want ToolCall", ev, ev)
	}
	if tc.Name != "exec" || tc.Args != "git status" {
		t.Errorf("ToolCall = %+v, want {Name: exec, Args: git status}", tc)
	}

	// Write item.completed/agent_message — expect AssistantText before EOF.
	if _, err := pw.Write([]byte(`{"type":"item.completed","item":{"type":"agent_message","text":"hello"}}` + "\n")); err != nil {
		t.Fatalf("pipe write: %v", err)
	}
	ev = expect(t, "AssistantText")
	at, ok := ev.(reviewtypes.AssistantText)
	if !ok {
		t.Fatalf("event = %T (%+v), want AssistantText", ev, ev)
	}
	if at.Text != "hello" {
		t.Errorf("AssistantText.Text = %q, want %q", at.Text, "hello")
	}

	// Write turn.completed and close — expect Tokens + Finished.
	if _, err := pw.Write([]byte(`{"type":"turn.completed","usage":{"input_tokens":100,"output_tokens":42}}` + "\n")); err != nil {
		t.Fatalf("pipe write: %v", err)
	}
	_ = pw.Close()

	ev = expect(t, "Tokens")
	tk, ok := ev.(reviewtypes.Tokens)
	if !ok {
		t.Fatalf("event = %T (%+v), want Tokens", ev, ev)
	}
	if tk.Out != 42 || tk.In != 100 {
		t.Errorf("Tokens = %+v, want {In:100, Out:42}", tk)
	}
	ev = expect(t, "Finished")
	fin, ok := ev.(reviewtypes.Finished)
	if !ok {
		t.Fatalf("event = %T (%+v), want Finished", ev, ev)
	}
	if !fin.Success {
		t.Error("Finished.Success = false, want true")
	}
}

func TestParseCodexOutput_NoTurnCompletedMeansFailed(t *testing.T) {
	t.Parallel()
	// A truncated session: thread starts and an item completes, but no
	// `turn.completed` envelope ever arrives. The parser must surface
	// this as Finished{Success: false}.
	input := `{"type":"thread.started","thread_id":"tid"}` + "\n" +
		`{"type":"item.completed","item":{"type":"agent_message","text":"partial"}}` + "\n"
	events := collectCodexEvents(parseCodexOutput(strings.NewReader(input)))

	last := events[len(events)-1]
	fin, ok := last.(reviewtypes.Finished)
	if !ok {
		t.Fatalf("last event = %T, want Finished", last)
	}
	if fin.Success {
		t.Error("Finished.Success = true, want false on missing turn.completed envelope")
	}
}

func TestParseCodexOutput_GarbledLineEmitsRunErrorAndContinues(t *testing.T) {
	t.Parallel()
	input := `{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}` + "\n" +
		"this is not json" + "\n" +
		`{"type":"turn.completed","usage":{"output_tokens":1}}` + "\n"
	events := collectCodexEvents(parseCodexOutput(strings.NewReader(input)))

	var sawRunError, sawSuccess bool
	for _, ev := range events {
		if _, ok := ev.(reviewtypes.RunError); ok {
			sawRunError = true
		}
		if fin, ok := ev.(reviewtypes.Finished); ok && fin.Success {
			sawSuccess = true
		}
	}
	if !sawRunError {
		t.Error("expected RunError for garbled line")
	}
	if !sawSuccess {
		t.Error("expected Finished{Success:true} after recovering from garbled line")
	}
}

func collectCodexEvents(ch <-chan reviewtypes.Event) []reviewtypes.Event {
	var events []reviewtypes.Event
	for ev := range ch {
		events = append(events, ev)
	}
	return events
}

// drainCodexEvents consumes all events from ch without recording them.
func drainCodexEvents(ch <-chan reviewtypes.Event) {
	for ev := range ch {
		_ = ev
	}
}

func readCodexCmdStdin(t *testing.T, cmd *exec.Cmd) string {
	t.Helper()
	b, err := io.ReadAll(cmd.Stdin)
	if err != nil {
		t.Fatalf("read stdin: %v", err)
	}
	return string(b)
}

func envToMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, e := range env {
		idx := strings.IndexByte(e, '=')
		if idx < 0 {
			continue
		}
		m[e[:idx]] = e[idx+1:]
	}
	return m
}
