package types

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// Compile-time interface check.
var _ AgentReviewer = (*ReviewerTemplate)(nil)

func TestReviewerTemplate_Name(t *testing.T) {
	t.Parallel()
	tmpl := &ReviewerTemplate{
		AgentName: "test-agent",
		BuildCmd: func(ctx context.Context, _ RunConfig) *exec.Cmd {
			return exec.CommandContext(ctx, "true")
		},
		Parser: func(_ io.Reader) <-chan Event {
			ch := make(chan Event)
			close(ch)
			return ch
		},
	}
	if got := tmpl.Name(); got != "test-agent" {
		t.Errorf("Name() = %q, want %q", got, "test-agent")
	}
}

func TestReviewerTemplate_EventsForwarded(t *testing.T) {
	t.Parallel()

	// A stub parser that emits a fixed sequence of events.
	wantEvents := []Event{
		Started{},
		AssistantText{Text: "line 1"},
		AssistantText{Text: "line 2"},
		Finished{Success: true},
	}

	stubParser := func(_ io.Reader) <-chan Event {
		ch := make(chan Event, len(wantEvents))
		for _, ev := range wantEvents {
			ch <- ev
		}
		close(ch)
		return ch
	}

	tmpl := &ReviewerTemplate{
		AgentName: "stub-agent",
		BuildCmd: func(ctx context.Context, _ RunConfig) *exec.Cmd {
			// "true" exits 0 immediately; stdin/stdout pipes are still valid.
			return exec.CommandContext(ctx, "true")
		},
		Parser: stubParser,
	}

	ctx := context.Background()
	proc, err := tmpl.Start(ctx, RunConfig{Skills: []string{"/test"}})
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	var got []Event
	for ev := range proc.Events() {
		got = append(got, ev)
	}

	if err := proc.Wait(); err != nil {
		t.Fatalf("Wait() error: %v", err)
	}

	if len(got) != len(wantEvents) {
		t.Fatalf("got %d events, want %d: %v", len(got), len(wantEvents), got)
	}
	for i, want := range wantEvents {
		if got[i] != want {
			t.Errorf("events[%d] = %v, want %v", i, got[i], want)
		}
	}
}

func TestReviewerTemplate_ProcessWaitReturnsExitError(t *testing.T) {
	t.Parallel()

	tmpl := &ReviewerTemplate{
		AgentName: "exit1-agent",
		BuildCmd: func(ctx context.Context, _ RunConfig) *exec.Cmd {
			return exec.CommandContext(ctx, "false") // exits with code 1
		},
		Parser: func(_ io.Reader) <-chan Event {
			ch := make(chan Event)
			close(ch)
			return ch
		},
	}

	ctx := context.Background()
	proc, err := tmpl.Start(ctx, RunConfig{})
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	// Drain events.
	for ev := range proc.Events() {
		_ = ev
	}
	waitErr := proc.Wait()
	if waitErr == nil {
		t.Error("Wait() should return non-nil error for exit code 1")
	}
}

func TestReviewerTemplate_WaitReturnsContextErrorOnCancellation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell")
	}
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	tmpl := &ReviewerTemplate{
		AgentName: "cancel-agent",
		BuildCmd: func(ctx context.Context, _ RunConfig) *exec.Cmd {
			return exec.CommandContext(ctx, "sh", "-c", "sleep 10")
		},
		Parser: func(_ io.Reader) <-chan Event {
			ch := make(chan Event)
			close(ch)
			return ch
		},
	}

	proc, err := tmpl.Start(ctx, RunConfig{})
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	for ev := range proc.Events() {
		_ = ev
	}
	cancel()

	waitErr := proc.Wait()
	if !errors.Is(waitErr, context.Canceled) {
		t.Fatalf("Wait() = %T %v, want context.Canceled", waitErr, waitErr)
	}
}

func TestReviewerTemplate_WaitIncludesStderrOnFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell")
	}
	t.Parallel()

	tmpl := &ReviewerTemplate{
		AgentName: "stderr-agent",
		BuildCmd: func(ctx context.Context, _ RunConfig) *exec.Cmd {
			return exec.CommandContext(ctx, "sh", "-c", "echo 'auth failed: login required' >&2; exit 7")
		},
		Parser: func(_ io.Reader) <-chan Event {
			ch := make(chan Event)
			close(ch)
			return ch
		},
	}

	proc, err := tmpl.Start(context.Background(), RunConfig{})
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	for ev := range proc.Events() {
		_ = ev
	}

	waitErr := proc.Wait()
	if waitErr == nil {
		t.Fatal("Wait() returned nil, want failure")
	}
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) {
		t.Fatalf("Wait() = %T %v, want error wrapping *exec.ExitError", waitErr, waitErr)
	}
	if !strings.Contains(waitErr.Error(), "auth failed: login required") {
		t.Fatalf("Wait() error missing stderr diagnostics: %v", waitErr)
	}
}

// TestReviewerTemplate_StartReturnsErrTemplateMisconfigured pins the typed
// validation errors Start returns when required fields are missing. The
// previous behaviour panicked here, which would crash a whole multi-agent
// fan-out (CU8) when one agent's template is misconfigured. Returning a
// typed error lets callers skip that agent and continue.
func TestReviewerTemplate_StartReturnsErrTemplateMisconfigured(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		tmpl ReviewerTemplate
	}{
		{
			name: "empty AgentName",
			tmpl: ReviewerTemplate{
				BuildCmd: func(ctx context.Context, _ RunConfig) *exec.Cmd { return exec.CommandContext(ctx, "true") },
				Parser:   func(_ io.Reader) <-chan Event { c := make(chan Event); close(c); return c },
			},
		},
		{
			name: "nil BuildCmd",
			tmpl: ReviewerTemplate{
				AgentName: "test",
				Parser:    func(_ io.Reader) <-chan Event { c := make(chan Event); close(c); return c },
			},
		},
		{
			name: "nil Parser",
			tmpl: ReviewerTemplate{
				AgentName: "test",
				BuildCmd:  func(ctx context.Context, _ RunConfig) *exec.Cmd { return exec.CommandContext(ctx, "true") },
			},
		},
		{
			name: "BuildCmd returns nil",
			tmpl: ReviewerTemplate{
				AgentName: "test",
				BuildCmd:  func(_ context.Context, _ RunConfig) *exec.Cmd { return nil },
				Parser:    func(_ io.Reader) <-chan Event { c := make(chan Event); close(c); return c },
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tmpl := tc.tmpl
			_, err := tmpl.Start(context.Background(), RunConfig{})
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !errors.Is(err, ErrTemplateMisconfigured) {
				t.Errorf("expected ErrTemplateMisconfigured, got %v", err)
			}
		})
	}
}
