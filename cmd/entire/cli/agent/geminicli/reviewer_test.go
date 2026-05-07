package geminicli

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

const wantGeminiAgentName = "gemini-cli"

func TestGeminiReviewer_Name(t *testing.T) {
	t.Parallel()
	r := NewReviewer()
	if got := r.Name(); got != wantGeminiAgentName {
		t.Errorf("Name() = %q, want %q", got, wantGeminiAgentName)
	}
}

func TestGeminiReviewer_EnvVarsSet(t *testing.T) {
	t.Parallel()
	cfg := reviewtypes.RunConfig{
		Skills:       []string{"/gemini:review", "/security-review"},
		AlwaysPrompt: "Always check for security vulnerabilities.",
		PerRunPrompt: "Focus on the API layer.",
		StartingSHA:  "cafebabe0000",
	}
	cmd := buildGeminiReviewCmd(context.Background(), cfg)

	wantKeys := []string{
		review.EnvSession,
		review.EnvAgent,
		review.EnvSkills,
		review.EnvPrompt,
		review.EnvStartingSHA,
	}
	envMap := geminiEnvToMap(cmd.Env)

	for _, key := range wantKeys {
		if _, ok := envMap[key]; !ok {
			t.Errorf("env var %s not set on cmd", key)
		}
	}

	if envMap[review.EnvSession] != "1" {
		t.Errorf("%s = %q, want %q", review.EnvSession, envMap[review.EnvSession], "1")
	}
	if envMap[review.EnvAgent] != wantGeminiAgentName {
		t.Errorf("%s = %q, want %q", review.EnvAgent, envMap[review.EnvAgent], wantGeminiAgentName)
	}
	if envMap[review.EnvStartingSHA] != "cafebabe0000" {
		t.Errorf("%s = %q, want %q", review.EnvStartingSHA, envMap[review.EnvStartingSHA], "cafebabe0000")
	}
	if !strings.HasPrefix(envMap[review.EnvSkills], "[") {
		t.Errorf("%s = %q, want JSON array", review.EnvSkills, envMap[review.EnvSkills])
	}
}

func TestGeminiReviewer_ArgvShape(t *testing.T) {
	t.Parallel()
	cfg := reviewtypes.RunConfig{Skills: []string{"/skill"}}
	cmd := buildGeminiReviewCmd(context.Background(), cfg)

	// Expect: gemini -p " "
	if len(cmd.Args) != 3 {
		t.Fatalf("len(Args) = %d, want 3: %v", len(cmd.Args), cmd.Args)
	}
	if cmd.Args[0] != "gemini" {
		t.Errorf("Args[0] = %q, want %q", cmd.Args[0], "gemini")
	}
	if cmd.Args[1] != "-p" {
		t.Errorf("Args[1] = %q, want %q", cmd.Args[1], "-p")
	}
	if cmd.Args[2] != " " {
		t.Errorf("Args[2] = %q, want %q (space placeholder)", cmd.Args[2], " ")
	}
	// Stdin must be non-nil — gemini receives prompt via stdin.
	if cmd.Stdin == nil {
		t.Error("cmd.Stdin is nil; gemini requires prompt via stdin")
	}
}

func TestGeminiReviewer_NoBinaryRequiredAtConstruction(t *testing.T) {
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
		drainGeminiEvents(proc.Events())
		_ = proc.Wait() //nolint:errcheck // best-effort cleanup in test
	}
}

func TestParseGeminiOutput_ReportsScannerError(t *testing.T) {
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

	events := collectGeminiEvents(parseGeminiOutput(r))

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

func TestGeminiReviewer_EventStream(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("testdata/canned_session.txt")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	events := collectGeminiEvents(parseGeminiOutput(strings.NewReader(string(data))))

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

	// All middle events must be AssistantText with non-empty text.
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

	// Verify fixture content appears in text events.
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

func collectGeminiEvents(ch <-chan reviewtypes.Event) []reviewtypes.Event {
	var events []reviewtypes.Event
	for ev := range ch {
		events = append(events, ev)
	}
	return events
}

// drainGeminiEvents consumes all events from ch without recording them.
func drainGeminiEvents(ch <-chan reviewtypes.Event) {
	for ev := range ch {
		_ = ev
	}
}

func geminiEnvToMap(env []string) map[string]string {
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
