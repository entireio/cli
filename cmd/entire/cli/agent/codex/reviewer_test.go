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

	"github.com/entireio/cli/cmd/entire/cli/review"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

// Compile-time interface check: ReviewerTemplate implements AgentReviewer.
var _ reviewtypes.AgentReviewer = (*reviewtypes.ReviewerTemplate)(nil)

const wantCodexAgentName = "codex"

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

	// Expect: codex exec --skip-git-repo-check -
	want := []string{wantCodexAgentName, "exec", "--skip-git-repo-check", "-"}
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

	want := []string{wantCodexAgentName, "exec", "--skip-git-repo-check", "-"}
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
		"Scope: review only the commits unique to this branch vs main.",
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
	// Trigger bufio.Scanner's "token too long" error: produce a "line"
	// that exceeds the 16MB max buffer without containing a newline.
	// Strip's internal scanner has its own 16MB buffer, so we need to
	// exceed that to propagate the error through the pipe chain.
	r, w := io.Pipe()
	go func() {
		defer w.Close()
		// 17MB of contiguous bytes without a newline — exceeds Strip's scanner buffer
		buf := make([]byte, 1024*1024)
		for range 17 {
			_, _ = w.Write(buf) //nolint:errcheck // best-effort write in test goroutine
		}
	}()

	events := collectCodexEvents(parseCodexOutput(r))

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
	// Also assert at least one RunError event was emitted before Finished.
	sawRunError := false
	for _, ev := range events {
		if _, ok := ev.(reviewtypes.RunError); ok {
			sawRunError = true
			break
		}
	}
	if !sawRunError {
		t.Error("expected RunError event before Finished{Success: false}")
	}
}

func TestCodexReviewer_EventStream(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("testdata/canned_exec.txt")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	events := collectCodexEvents(parseCodexOutput(strings.NewReader(string(data))))

	if len(events) < 3 {
		t.Fatalf("expected at least 3 events (Started + AssistantText + Finished), got %d", len(events))
	}

	// First event must be Started.
	if _, ok := events[0].(reviewtypes.Started); !ok {
		t.Errorf("events[0] = %T, want Started", events[0])
	}

	// Last event must be Finished{Success: true}.
	last := events[len(events)-1]
	fin, ok := last.(reviewtypes.Finished)
	if !ok {
		t.Errorf("last event = %T, want Finished", last)
	} else if !fin.Success {
		t.Errorf("Finished.Success = false, want true")
	}

	// Verify narrative content appears and chrome is absent.
	var combined strings.Builder
	sawToolCall := false
	for _, ev := range events {
		switch e := ev.(type) {
		case reviewtypes.AssistantText:
			at := e
			combined.WriteString(at.Text)
			combined.WriteString("\n")
		case reviewtypes.ToolCall:
			if e.Name == codexExecCommand && strings.Contains(e.Args, "git status --short") {
				sawToolCall = true
			}
		}
	}
	text := combined.String()

	// Narrative should appear.
	if !strings.Contains(text, "No findings.") {
		t.Error("expected fixture final response in AssistantText events")
	}
	if !strings.Contains(text, "Important finding: all error paths are covered.") {
		t.Error("expected ANSI-cleaned fixture content in AssistantText events")
	}

	// Chrome must be absent.
	chromePatterns := []string{
		"OpenAI Codex",
		"workdir:",
		"[hooks]",
		"firing user-prompt-submit",
		"git status",
		"go test ./cmd/entire/cli/review",
		"TestExample",
		"tokens used",
	}
	for _, pattern := range chromePatterns {
		if strings.Contains(text, pattern) {
			t.Errorf("chrome pattern %q must not appear in AssistantText events", pattern)
		}
	}
	if strings.Count(text, "No findings.") != 1 {
		t.Errorf("final response should appear once after duplicate summary filtering; got:\n%s", text)
	}
	if !strings.Contains(text, "I will inspect the reviewer contracts.") {
		t.Error("expected live assistant progress in AssistantText events")
	}
	if !sawToolCall {
		t.Errorf("expected exec block to emit a ToolCall event; got %#v", events)
	}

	// CSI escape sequences must not leak into AssistantText events.
	for _, ev := range events {
		if at, ok := ev.(reviewtypes.AssistantText); ok {
			if strings.Contains(at.Text, "\x1b[") {
				t.Errorf("CSI bytes leaked into AssistantText: %q", at.Text)
			}
		}
	}
}

func TestParseCodexOutput_StreamsEventsBeforeEOF(t *testing.T) {
	t.Parallel()

	r, w := io.Pipe()
	events := parseCodexOutput(r)

	select {
	case ev, ok := <-events:
		if !ok {
			t.Fatal("events closed before Started event arrived")
		}
		if _, ok := ev.(reviewtypes.Started); !ok {
			t.Fatalf("first event = %T, want Started", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Started event")
	}

	input := strings.Join([]string{
		"codex",
		"I will inspect the code before finalizing.",
		"exec",
		`/bin/zsh -lc "git status --short" in /repo`,
	}, "\n") + "\n"
	if _, err := w.Write([]byte(input)); err != nil {
		t.Fatalf("write streaming input: %v", err)
	}

	sawText := false
	sawToolCall := false
	deadline := time.After(2 * time.Second)
	for !sawText || !sawToolCall {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatalf("events closed before streaming assertions passed")
			}
			switch e := ev.(type) {
			case reviewtypes.AssistantText:
				if strings.Contains(e.Text, "I will inspect the code") {
					sawText = true
				}
			case reviewtypes.ToolCall:
				if e.Name == codexExecCommand && strings.Contains(e.Args, "git status --short") {
					sawToolCall = true
				}
			}
		case <-deadline:
			t.Fatalf("timed out waiting for streaming events before EOF; sawText=%v sawToolCall=%v", sawText, sawToolCall)
		}
	}

	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	sawFinished := false
	for ev := range events {
		if fin, ok := ev.(reviewtypes.Finished); ok {
			if !fin.Success {
				t.Fatalf("Finished.Success = false, want true")
			}
			sawFinished = true
		}
	}
	if !sawFinished {
		t.Fatal("expected Finished event after EOF")
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
