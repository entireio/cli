package claudecode

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

const wantAgentName = "claude-code"

// TestReviewer_NameMatchesRegistryKey locks the reviewer's name to the
// agent registry's stable key. adoptReviewEnv compares ENTIRE_REVIEW_AGENT
// against string(ag.Name()); drift here silently breaks review-session
// tagging for this agent.
func TestReviewer_NameMatchesRegistryKey(t *testing.T) {
	t.Parallel()
	if wantAgentName != string(agent.AgentNameClaudeCode) {
		t.Fatalf("wantAgentName = %q, agent.AgentNameClaudeCode = %q — keep these aligned",
			wantAgentName, string(agent.AgentNameClaudeCode))
	}
}

func TestReviewer_Name(t *testing.T) {
	t.Parallel()
	r := NewReviewer()
	if got := r.Name(); got != wantAgentName {
		t.Errorf("Name() = %q, want %q", got, wantAgentName)
	}
}

func TestReviewer_EnvVarsSet(t *testing.T) {
	t.Parallel()
	cfg := reviewtypes.RunConfig{
		Skills:       []string{"/pr-review-toolkit:review-pr", "/test-auditor"},
		AlwaysPrompt: "Always check for security issues.",
		PerRunPrompt: "Focus on the auth module.",
		StartingSHA:  "abc123def456",
	}
	cmd := buildReviewCmd(context.Background(), cfg)

	wantEnvKeys := []string{
		review.EnvSession,
		review.EnvAgent,
		review.EnvSkills,
		review.EnvPrompt,
		review.EnvStartingSHA,
	}
	envMap := make(map[string]string)
	for _, e := range cmd.Env {
		idx := strings.IndexByte(e, '=')
		if idx < 0 {
			continue
		}
		envMap[e[:idx]] = e[idx+1:]
	}

	for _, key := range wantEnvKeys {
		if _, ok := envMap[key]; !ok {
			t.Errorf("env var %s not set on cmd", key)
		}
	}

	if envMap[review.EnvSession] != "1" {
		t.Errorf("%s = %q, want %q", review.EnvSession, envMap[review.EnvSession], "1")
	}
	if envMap[review.EnvAgent] != wantAgentName {
		t.Errorf("%s = %q, want %q", review.EnvAgent, envMap[review.EnvAgent], wantAgentName)
	}
	if envMap[review.EnvStartingSHA] != "abc123def456" {
		t.Errorf("%s = %q, want %q", review.EnvStartingSHA, envMap[review.EnvStartingSHA], "abc123def456")
	}
	// Skills must be a valid JSON array.
	if !strings.HasPrefix(envMap[review.EnvSkills], "[") {
		t.Errorf("%s = %q, want JSON array", review.EnvSkills, envMap[review.EnvSkills])
	}
}

func TestReviewer_ArgvShape(t *testing.T) {
	t.Parallel()
	cfg := reviewtypes.RunConfig{
		Skills:       []string{"/skill-a"},
		PerRunPrompt: "extra context",
	}
	cmd := buildReviewCmd(context.Background(), cfg)

	// Expect: claude -p <prompt> --output-format stream-json --verbose
	wantSuffix := []string{"--output-format", "stream-json", "--verbose"}
	if len(cmd.Args) != 3+len(wantSuffix) {
		t.Fatalf("expected %d args, got %d: %v", 3+len(wantSuffix), len(cmd.Args), cmd.Args)
	}
	if cmd.Args[0] != "claude" {
		t.Errorf("Args[0] = %q, want %q", cmd.Args[0], "claude")
	}
	if cmd.Args[1] != "-p" {
		t.Errorf("Args[1] = %q, want %q", cmd.Args[1], "-p")
	}
	// Args[2] is the composed prompt — must be non-empty.
	if cmd.Args[2] == "" {
		t.Error("Args[2] (prompt) is empty")
	}
	for i, want := range wantSuffix {
		got := cmd.Args[3+i]
		if got != want {
			t.Errorf("Args[%d] = %q, want %q", 3+i, got, want)
		}
	}
	for _, arg := range cmd.Args {
		if arg == "--continue" || arg == "-c" || arg == "--resume" || arg == "-r" {
			t.Fatalf("Args must start a fresh Claude review, got resume/continue flag in %v", cmd.Args)
		}
	}
	// Stdin must be nil — claude receives prompt via argv, not stdin.
	if cmd.Stdin != nil {
		t.Errorf("cmd.Stdin = %v, want nil (claude uses argv, not stdin)", cmd.Stdin)
	}
}

func TestReviewer_NoBinaryRequiredAtConstruction(t *testing.T) {
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
		drainEvents(proc.Events())
		_ = proc.Wait() //nolint:errcheck // best-effort cleanup in test
	}
}

func TestParseClaudeOutput_ReportsScannerError(t *testing.T) {
	t.Parallel()
	// Trigger bufio.Scanner's "token too long" error via parseClaudeOutputBuf
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

	events := collectEvents(parseClaudeOutputBuf(r, maxBuf))

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
	// prefix), not the unmarshal branch ("claude stream-json"). Without this
	// the test would pass even if the scanner cap were widened back out and
	// the huge blob just fell through json.Unmarshal — the exact regression
	// the parameterized buffer is meant to prevent.
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

func TestParseClaudeOutput_DecodesStreamJSON(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/stream_session.jsonl")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	events := collectEvents(parseClaudeOutput(strings.NewReader(string(data))))

	if _, ok := events[0].(reviewtypes.Started); !ok {
		t.Errorf("events[0] = %T, want Started", events[0])
	}
	last := events[len(events)-1]
	fin, ok := last.(reviewtypes.Finished)
	if !ok || !fin.Success {
		t.Errorf("last event = %v, want Finished{Success:true}", last)
	}

	var sawText bool
	for _, ev := range events {
		if at, ok := ev.(reviewtypes.AssistantText); ok && strings.Contains(at.Text, "Cats are") {
			sawText = true
		}
	}
	if !sawText {
		t.Error("expected AssistantText carrying fixture prose 'Cats are…'")
	}

	var tokensSeen int
	var tokensOut int
	for _, ev := range events {
		if tk, ok := ev.(reviewtypes.Tokens); ok {
			tokensSeen++
			tokensOut = tk.Out
		}
	}
	if tokensSeen != 1 {
		t.Errorf("Tokens count = %d, want 1", tokensSeen)
	}
	if tokensOut == 0 {
		t.Error("Tokens.Out = 0, want > 0")
	}
}

func TestParseClaudeOutput_StreamsEventsBeforeEOF(t *testing.T) {
	t.Parallel()
	pr, pw := io.Pipe()
	events := parseClaudeOutput(pr)

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

	// Write a system/init envelope — swallowed, no event read here.
	if _, err := pw.Write([]byte(`{"type":"system","subtype":"init","session_id":"sid"}` + "\n")); err != nil {
		t.Fatalf("pipe write: %v", err)
	}

	// Write an assistant text envelope — expect AssistantText before EOF.
	if _, err := pw.Write([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"hello"}]}}` + "\n")); err != nil {
		t.Fatalf("pipe write: %v", err)
	}
	ev := expect(t, "AssistantText")
	at, ok := ev.(reviewtypes.AssistantText)
	if !ok {
		t.Fatalf("event = %T (%+v), want AssistantText", ev, ev)
	}
	if at.Text != "hello" {
		t.Errorf("AssistantText.Text = %q, want %q", at.Text, "hello")
	}

	// Write an assistant tool_use envelope — expect ToolCall before EOF.
	// This also covers the unexercised tool_use branch that was flagged in
	// the PR's fixture-coverage review.
	if _, err := pw.Write([]byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu_1","name":"Read","input":{"file_path":"x"}}]}}` + "\n")); err != nil {
		t.Fatalf("pipe write: %v", err)
	}
	ev = expect(t, "ToolCall")
	tc, ok := ev.(reviewtypes.ToolCall)
	if !ok {
		t.Fatalf("event = %T (%+v), want ToolCall", ev, ev)
	}
	if tc.Name != "Read" {
		t.Errorf("ToolCall.Name = %q, want %q", tc.Name, "Read")
	}
	if !strings.Contains(tc.Args, `"file_path":"x"`) {
		t.Errorf("ToolCall.Args = %q, want to contain file_path:x", tc.Args)
	}

	// Write the result envelope and close — expect Tokens then Finished.
	if _, err := pw.Write([]byte(`{"type":"result","subtype":"success","is_error":false,"usage":{"input_tokens":100,"output_tokens":42,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}` + "\n")); err != nil {
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

func TestParseClaudeOutput_NoResultEnvelopeMeansFailed(t *testing.T) {
	t.Parallel()
	// A truncated session: assistant message but no `result` envelope.
	// The parser must surface this as Finished{Success: false} so the
	// caller distinguishes "agent exited mid-generation" from "agent
	// completed successfully".
	input := `{"type":"assistant","message":{"content":[{"type":"text","text":"partial"}]}}` + "\n"
	events := collectEvents(parseClaudeOutput(strings.NewReader(input)))

	last := events[len(events)-1]
	fin, ok := last.(reviewtypes.Finished)
	if !ok {
		t.Fatalf("last event = %T, want Finished", last)
	}
	if fin.Success {
		t.Error("Finished.Success = true, want false on missing result envelope")
	}
}

func TestParseClaudeOutput_GarbledLineEmitsRunErrorAndContinues(t *testing.T) {
	t.Parallel()
	// A garbled non-JSON line between valid envelopes must not abort the
	// parser. The bad line surfaces as RunError; the stream continues to
	// consume subsequent envelopes including a clean result.
	input := `{"type":"assistant","message":{"content":[{"type":"text","text":"ok"}]}}` + "\n" +
		"this is not json" + "\n" +
		`{"type":"result","subtype":"success","is_error":false,"usage":{"output_tokens":1}}` + "\n"
	events := collectEvents(parseClaudeOutput(strings.NewReader(input)))

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

// collectEvents drains an event channel into a slice.
func collectEvents(ch <-chan reviewtypes.Event) []reviewtypes.Event {
	var events []reviewtypes.Event
	for ev := range ch {
		events = append(events, ev)
	}
	return events
}

// drainEvents consumes all events from ch without recording them.
func drainEvents(ch <-chan reviewtypes.Event) {
	for ev := range ch {
		_ = ev
	}
}
