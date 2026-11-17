package types

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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

// TestReviewerTemplate_WaitDoesNotBlockOnInheritedStderr covers an agent that
// exits while a grandchild it spawned still holds stderr open (a language
// server, an MCP helper). Wait must return when the agent exits instead of
// blocking on a stderr EOF that may never come.
func TestReviewerTemplate_WaitDoesNotBlockOnInheritedStderr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell and inherited file descriptors")
	}
	t.Parallel()

	releaseReader, releaseWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = releaseReader.Close(); _ = releaseWriter.Close() })
	tmpl := &ReviewerTemplate{
		AgentName: "grandchild-agent",
		BuildCmd: func(ctx context.Context, _ RunConfig) *exec.Cmd {
			// The helper holds stderr until the test closes releaseWriter. Unlike
			// a timed sleep, it cannot exit early on a slow CI worker.
			cmd := exec.CommandContext(ctx, "sh", "-c", "cat <&3 >/dev/null & exit 0")
			cmd.ExtraFiles = []*os.File{releaseReader}
			return cmd
		},
		Parser: func(stdout io.Reader) <-chan Event {
			if _, err := io.Copy(io.Discard, stdout); err != nil {
				t.Errorf("draining reviewer stdout: %v", err)
			}
			ch := make(chan Event, 1)
			ch <- Finished{Success: true}
			close(ch)
			return ch
		},
	}
	proc, err := tmpl.Start(t.Context(), RunConfig{})
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	_ = releaseReader.Close()
	for range proc.Events() {
	}
	done := make(chan struct{})
	var waitErr error
	go func() { waitErr = proc.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = releaseWriter.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Wait did not finish after releasing inherited stderr")
		}
	})
	select {
	case <-done:
		if waitErr != nil {
			t.Fatalf("Wait() error: %v", waitErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait() blocked on stderr held by a grandchild after the agent exited")
	}
}

func TestReviewerTemplate_StartFailureClosesStderr(t *testing.T) {
	t.Parallel()

	var cmd *exec.Cmd
	cleaned := false
	tmpl := &ReviewerTemplate{
		AgentName: "missing-agent",
		Prepare: func(_ context.Context, cfg RunConfig) (RunConfig, func(), error) {
			return cfg, func() { cleaned = true }, nil
		},
		BuildCmd: func(ctx context.Context, _ RunConfig) *exec.Cmd {
			cmd = exec.CommandContext(ctx, filepath.Join(t.TempDir(), "missing-agent"))
			return cmd
		},
		Parser: func(_ io.Reader) <-chan Event {
			t.Error("parser called after failed Start")
			return nil
		},
	}
	proc, err := tmpl.Start(t.Context(), RunConfig{})
	if err == nil || proc != nil {
		t.Fatalf("Start() = (%v, %v), want nil process and launch error", proc, err)
	}
	if !cleaned {
		t.Error("Start did not release prepared resources")
	}
	writer, ok := cmd.Stderr.(*os.File)
	if !ok {
		t.Fatalf("stderr writer = %T, want *os.File", cmd.Stderr)
	}
	t.Cleanup(func() { _ = writer.Close() })
	if _, err := writer.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("stderr writer remains open after failed Start: %v", err)
	}
}

func TestReviewerTemplate_StartRejectsConfiguredStderr(t *testing.T) {
	t.Parallel()

	var cmd *exec.Cmd
	tmpl := &ReviewerTemplate{
		AgentName: "configured-stderr",
		BuildCmd: func(ctx context.Context, _ RunConfig) *exec.Cmd {
			cmd = exec.CommandContext(ctx, filepath.Join(t.TempDir(), "missing-agent"))
			cmd.Stderr = io.Discard
			return cmd
		},
		Parser: func(_ io.Reader) <-chan Event {
			t.Error("parser called for a rejected command")
			return nil
		},
	}
	proc, err := tmpl.Start(t.Context(), RunConfig{})
	if proc != nil || err == nil || !strings.Contains(err.Error(), "Stderr already set") {
		t.Fatalf("Start() = (%v, %v), want configured-stderr error", proc, err)
	}
	if cmd.Stderr != io.Discard || cmd.Process != nil {
		t.Fatal("Start changed or launched the rejected command")
	}
}

func TestReviewerTemplate_WaitDrainsLargeStderr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell")
	}
	t.Parallel()

	// More than a pipe buffer: the capture must drain concurrently, preserve
	// the bounded diagnostic and join before Wait exposes its contents.
	diagnostic := strings.Repeat("x", maxProcessStderrBytes+4096)
	tmpl := &ReviewerTemplate{
		AgentName: "verbose-agent",
		BuildCmd: func(ctx context.Context, _ RunConfig) *exec.Cmd {
			return exec.CommandContext(ctx, "sh", "-c", `printf '%s' "$1" >&2; exit 7`, "agent", diagnostic)
		},
		Parser: func(stdout io.Reader) <-chan Event {
			if _, err := io.Copy(io.Discard, stdout); err != nil {
				t.Errorf("draining reviewer stdout: %v", err)
			}
			ch := make(chan Event)
			close(ch)
			return ch
		},
	}
	proc, err := tmpl.Start(t.Context(), RunConfig{})
	if err != nil {
		t.Fatal(err)
	}
	for range proc.Events() {
	}
	var processErr *ProcessError
	if err := proc.Wait(); !errors.As(err, &processErr) {
		t.Fatalf("Wait() = %v, want ProcessError", err)
	}
	want := diagnostic[:maxProcessStderrBytes] + "\n[stderr truncated]"
	if processErr.Stderr != want {
		t.Fatalf("stderr: got %d bytes, want %d bytes including truncation marker", len(processErr.Stderr), len(want))
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

func TestReviewerTemplate_PreflightFailureStopsBeforeBuildCmd(t *testing.T) {
	t.Parallel()

	errTooOld := errors.New("agent CLI too old")
	built := false
	tmpl := &ReviewerTemplate{
		AgentName: "old-agent",
		Preflight: func(context.Context) error { return errTooOld },
		BuildCmd: func(ctx context.Context, _ RunConfig) *exec.Cmd {
			built = true
			return exec.CommandContext(ctx, "true")
		},
		Parser: func(_ io.Reader) <-chan Event {
			ch := make(chan Event)
			close(ch)
			return ch
		},
	}

	proc, err := tmpl.Start(context.Background(), RunConfig{})
	if !errors.Is(err, errTooOld) {
		t.Fatalf("Start() error = %v, want the preflight error", err)
	}
	if proc != nil {
		t.Fatal("Start() returned a process despite the preflight failure")
	}
	if built {
		t.Fatal("BuildCmd ran despite the preflight failure")
	}
}
