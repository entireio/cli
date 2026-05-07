package claudecode

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/review"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

// Compile-time interface check: ReviewerTemplate implements AgentReviewer.
var _ reviewtypes.AgentReviewer = (*reviewtypes.ReviewerTemplate)(nil)

const wantAgentName = "claude-code"

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

	// Expect: claude -p <prompt>
	if len(cmd.Args) < 3 {
		t.Fatalf("expected at least 3 args, got %d: %v", len(cmd.Args), cmd.Args)
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
	// Trigger bufio.Scanner's "token too long" error: produce a "line"
	// that exceeds the 16MB max buffer without containing a newline.
	r, w := io.Pipe()
	go func() {
		defer w.Close()
		// 17MB of contiguous bytes without a newline
		buf := make([]byte, 1024*1024)
		for range 17 {
			_, _ = w.Write(buf) //nolint:errcheck // best-effort write in test goroutine
		}
	}()

	events := collectEvents(parseClaudeOutput(r))

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

func TestReviewer_EventStream(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("testdata/canned_session.txt")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	events := collectEvents(parseClaudeOutput(strings.NewReader(string(data))))

	if len(events) < 3 {
		t.Fatalf("expected at least 3 events (Started + at least one AssistantText + Finished), got %d", len(events))
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

	// All middle events must be AssistantText (no empty lines emitted).
	for i := 1; i < len(events)-1; i++ {
		at, ok := events[i].(reviewtypes.AssistantText)
		if !ok {
			t.Errorf("events[%d] = %T, want AssistantText", i, events[i])
			continue
		}
		if at.Text == "" {
			t.Errorf("events[%d].Text is empty (empty lines must be skipped)", i)
		}
	}

	// Verify fixture content appears somewhere in the text events.
	var combined strings.Builder
	for _, ev := range events {
		if at, ok := ev.(reviewtypes.AssistantText); ok {
			combined.WriteString(at.Text)
			combined.WriteString("\n")
		}
	}
	if !strings.Contains(combined.String(), "AgentReviewer") {
		t.Error("expected fixture content mentioning 'AgentReviewer' to appear in AssistantText events")
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
