package review

import (
	"context"
	"errors"
	"testing"

	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

// stubReviewer is a test double for reviewtypes.AgentReviewer.
type stubReviewer struct {
	name     string
	events   []reviewtypes.Event
	waitErr  error
	startErr error
}

func (s *stubReviewer) Name() string { return s.name }
func (s *stubReviewer) Start(_ context.Context, _ reviewtypes.RunConfig) (reviewtypes.Process, error) {
	if s.startErr != nil {
		return nil, s.startErr
	}
	return &stubProcess{events: s.events, waitErr: s.waitErr}, nil
}

// Compile-time interface check.
var _ reviewtypes.AgentReviewer = (*stubReviewer)(nil)

// stubProcess is a test double for reviewtypes.Process.
type stubProcess struct {
	events  []reviewtypes.Event
	waitErr error
}

func (p *stubProcess) Events() <-chan reviewtypes.Event {
	out := make(chan reviewtypes.Event, len(p.events))
	for _, ev := range p.events {
		out <- ev
	}
	close(out)
	return out
}

func (p *stubProcess) Wait() error { return p.waitErr }

// Compile-time interface check.
var _ reviewtypes.Process = (*stubProcess)(nil)

// stubSinkRecorder records every call for assertion.
type stubSinkRecorder struct {
	agentEvents   []stubAgentEvent
	finishedCalls []reviewtypes.RunSummary
}

type stubAgentEvent struct {
	agent string
	ev    reviewtypes.Event
}

func (r *stubSinkRecorder) AgentEvent(agent string, ev reviewtypes.Event) {
	r.agentEvents = append(r.agentEvents, stubAgentEvent{agent, ev})
}

func (r *stubSinkRecorder) RunFinished(summary reviewtypes.RunSummary) {
	r.finishedCalls = append(r.finishedCalls, summary)
}

// Compile-time interface check.
var _ reviewtypes.Sink = (*stubSinkRecorder)(nil)

func TestRun_SuccessfulRun(t *testing.T) {
	t.Parallel()
	reviewer := &stubReviewer{
		name: "claude-code",
		events: []reviewtypes.Event{
			reviewtypes.Started{},
			reviewtypes.AssistantText{Text: "looks good"},
			reviewtypes.Finished{Success: true},
		},
		waitErr: nil,
	}
	rec := &stubSinkRecorder{}

	summary, err := Run(context.Background(), reviewer, reviewtypes.RunConfig{}, []reviewtypes.Sink{rec})

	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if summary.Cancelled {
		t.Errorf("expected Cancelled=false")
	}
	if len(summary.AgentRuns) != 1 {
		t.Fatalf("expected 1 AgentRun, got %d", len(summary.AgentRuns))
	}
	run := summary.AgentRuns[0]
	if run.Status != reviewtypes.AgentStatusSucceeded {
		t.Errorf("expected Status=Succeeded, got %v", run.Status)
	}
	if len(run.Buffer) != 3 {
		t.Errorf("expected 3 events in buffer, got %d", len(run.Buffer))
	}
	if run.Duration <= 0 {
		t.Errorf("expected Duration > 0, got %v", run.Duration)
	}
}

func TestRun_CancelledRun(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())

	// Build a process that cancels the ctx when drained, then returns ctx.Err from Wait.
	proc := &cancellingProcess{cancel: cancel}
	reviewer := &funcReviewer{
		name:    "claude-code",
		process: proc,
	}

	summary, err := Run(ctx, reviewer, reviewtypes.RunConfig{}, nil)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled error, got %v", err)
	}
	if !summary.Cancelled {
		t.Errorf("expected summary.Cancelled=true")
	}
	if len(summary.AgentRuns) != 1 {
		t.Fatalf("expected 1 AgentRun, got %d", len(summary.AgentRuns))
	}
	if summary.AgentRuns[0].Status != reviewtypes.AgentStatusCancelled {
		t.Errorf("expected AgentStatusCancelled, got %v", summary.AgentRuns[0].Status)
	}
}

// cancellingProcess cancels the context when the Events channel is drained,
// then returns ctx.Err() from Wait — simulating a signal-killed process.
type cancellingProcess struct {
	cancel context.CancelFunc
}

func (p *cancellingProcess) Events() <-chan reviewtypes.Event {
	ch := make(chan reviewtypes.Event)
	close(ch) // no events; draining immediately triggers Wait
	return ch
}

func (p *cancellingProcess) Wait() error {
	p.cancel()
	return context.Canceled
}

// Compile-time interface check.
var _ reviewtypes.Process = (*cancellingProcess)(nil)

// funcReviewer lets tests inject an already-constructed Process.
type funcReviewer struct {
	name    string
	process reviewtypes.Process
}

func (r *funcReviewer) Name() string { return r.name }
func (r *funcReviewer) Start(_ context.Context, _ reviewtypes.RunConfig) (reviewtypes.Process, error) {
	return r.process, nil
}

// Compile-time interface check.
var _ reviewtypes.AgentReviewer = (*funcReviewer)(nil)

func TestRun_FailedRun(t *testing.T) {
	t.Parallel()
	fakeErr := errors.New("exit status 1")
	reviewer := &stubReviewer{
		name: "codex",
		events: []reviewtypes.Event{
			reviewtypes.Started{},
			reviewtypes.Finished{Success: false},
		},
		waitErr: fakeErr,
	}
	rec := &stubSinkRecorder{}

	summary, err := Run(context.Background(), reviewer, reviewtypes.RunConfig{}, []reviewtypes.Sink{rec})

	if !errors.Is(err, fakeErr) {
		t.Errorf("expected fakeErr, got %v", err)
	}
	if len(summary.AgentRuns) != 1 {
		t.Fatalf("expected 1 AgentRun, got %d", len(summary.AgentRuns))
	}
	if summary.AgentRuns[0].Status != reviewtypes.AgentStatusFailed {
		t.Errorf("expected AgentStatusFailed, got %v", summary.AgentRuns[0].Status)
	}
}

// TestRun_TornStreamCleanExitClassifiedAsFailed pins the orchestrator-level
// guarantee that a process exiting cleanly (waitErr == nil) but reporting
// agent-level failure via the event stream (RunError or
// Finished{Success: false}) is classified as Failed, not Succeeded. This is
// the upstream complement of the CU3 fix-loop's parser-side scanner.Err
// handling — the parser emits the right events; this test ensures the
// orchestrator honors them.
func TestRun_TornStreamCleanExitClassifiedAsFailed(t *testing.T) {
	t.Parallel()
	parserErr := errors.New("read stdout: token too long")
	reviewer := &stubReviewer{
		name: "codex",
		events: []reviewtypes.Event{
			reviewtypes.Started{},
			reviewtypes.AssistantText{Text: "partial response before tear..."},
			reviewtypes.RunError{Err: parserErr},
			reviewtypes.Finished{Success: false},
		},
		waitErr: nil, // process exited 0 despite torn stream
	}
	rec := &stubSinkRecorder{}

	summary, err := Run(context.Background(), reviewer, reviewtypes.RunConfig{}, []reviewtypes.Sink{rec})
	if err == nil {
		t.Fatal("expected parser failure to return an error even when process exits 0")
	}
	if !errors.Is(err, parserErr) {
		t.Fatalf("expected returned error to wrap parserErr, got %v", err)
	}
	if len(summary.AgentRuns) != 1 {
		t.Fatalf("expected 1 AgentRun, got %d", len(summary.AgentRuns))
	}
	if got := summary.AgentRuns[0].Status; got != reviewtypes.AgentStatusFailed {
		t.Errorf("expected AgentStatusFailed (torn-stream override), got %v", got)
	}
}

// TestRun_FinishedFailureCleanExitClassifiedAsFailed covers the case where
// the parser emitted Finished{Success: false} without a RunError preceding
// it. The orchestrator must still downgrade the run to Failed.
func TestRun_FinishedFailureCleanExitClassifiedAsFailed(t *testing.T) {
	t.Parallel()
	reviewer := &stubReviewer{
		name: "codex",
		events: []reviewtypes.Event{
			reviewtypes.Started{},
			reviewtypes.Finished{Success: false},
		},
		waitErr: nil,
	}
	rec := &stubSinkRecorder{}

	summary, err := Run(context.Background(), reviewer, reviewtypes.RunConfig{}, []reviewtypes.Sink{rec})
	if err == nil {
		t.Fatal("expected Finished{Success:false} to return an error even when process exits 0")
	}
	if got := summary.AgentRuns[0].Status; got != reviewtypes.AgentStatusFailed {
		t.Errorf("expected AgentStatusFailed (Finished{Success:false} override), got %v", got)
	}
}

func TestRun_StartError(t *testing.T) {
	t.Parallel()
	startErr := errors.New("binary not on PATH")
	reviewer := &stubReviewer{
		name:     "gemini-cli",
		startErr: startErr,
	}
	rec := &stubSinkRecorder{}

	summary, err := Run(context.Background(), reviewer, reviewtypes.RunConfig{}, []reviewtypes.Sink{rec})

	if !errors.Is(err, startErr) {
		t.Errorf("expected startErr, got %v", err)
	}
	if len(summary.AgentRuns) != 1 {
		t.Fatalf("expected 1 AgentRun, got %d", len(summary.AgentRuns))
	}
	run := summary.AgentRuns[0]
	if run.Status != reviewtypes.AgentStatusFailed {
		t.Errorf("expected AgentStatusFailed, got %v", run.Status)
	}
	if !errors.Is(run.Err, startErr) {
		t.Errorf("expected run.Err = startErr, got %v", run.Err)
	}
	// Sink must still receive RunFinished.
	if len(rec.finishedCalls) != 1 {
		t.Errorf("expected 1 RunFinished call to sink, got %d", len(rec.finishedCalls))
	}
}

func TestRun_StartErrorWithCancelledCtx(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel BEFORE calling Run

	r := &stubReviewer{
		name:     "claude-code",
		startErr: ctx.Err(), // simulate Start observing cancellation
	}
	rec := &stubSinkRecorder{}

	summary, err := Run(ctx, r, reviewtypes.RunConfig{}, []reviewtypes.Sink{rec})
	if err == nil {
		t.Fatal("expected Run to return the start error")
	}
	if len(summary.AgentRuns) != 1 {
		t.Fatalf("expected 1 AgentRun, got %d", len(summary.AgentRuns))
	}
	run := summary.AgentRuns[0]
	if run.Status != reviewtypes.AgentStatusCancelled {
		t.Errorf("Status: got %v, want Cancelled", run.Status)
	}
	if !summary.Cancelled {
		t.Error("summary.Cancelled should be true when start error is ctx.Err()")
	}
	// Sinks still get RunFinished even on start error.
	if len(rec.finishedCalls) != 1 {
		t.Errorf("RunFinished calls: got %d, want 1", len(rec.finishedCalls))
	}
}

func TestRun_TokenTracking(t *testing.T) {
	t.Parallel()
	// Two Tokens events — second should overwrite, not sum.
	reviewer := &stubReviewer{
		name: "claude-code",
		events: []reviewtypes.Event{
			reviewtypes.Started{},
			reviewtypes.Tokens{In: 10, Out: 5},
			reviewtypes.Tokens{In: 20, Out: 15}, // cumulative; second supersedes first
			reviewtypes.Finished{Success: true},
		},
		waitErr: nil,
	}

	summary, err := Run(context.Background(), reviewer, reviewtypes.RunConfig{}, nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	run := summary.AgentRuns[0]
	if run.Tokens.In != 20 {
		t.Errorf("expected Tokens.In=20 (latest), got %d", run.Tokens.In)
	}
	if run.Tokens.Out != 15 {
		t.Errorf("expected Tokens.Out=15 (latest), got %d", run.Tokens.Out)
	}
}

func TestRun_SinkFanOut(t *testing.T) {
	t.Parallel()
	events := []reviewtypes.Event{
		reviewtypes.Started{},
		reviewtypes.AssistantText{Text: "hello"},
		reviewtypes.Finished{Success: true},
	}
	reviewer := &stubReviewer{
		name:    "claude-code",
		events:  events,
		waitErr: nil,
	}
	rec1 := &stubSinkRecorder{}
	rec2 := &stubSinkRecorder{}

	_, err := Run(context.Background(), reviewer, reviewtypes.RunConfig{}, []reviewtypes.Sink{rec1, rec2})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Both sinks must receive exactly 3 AgentEvent calls.
	if len(rec1.agentEvents) != 3 {
		t.Errorf("sink1: expected 3 AgentEvent calls, got %d", len(rec1.agentEvents))
	}
	if len(rec2.agentEvents) != 3 {
		t.Errorf("sink2: expected 3 AgentEvent calls, got %d", len(rec2.agentEvents))
	}

	// Both sinks must receive exactly 1 RunFinished call.
	if len(rec1.finishedCalls) != 1 {
		t.Errorf("sink1: expected 1 RunFinished call, got %d", len(rec1.finishedCalls))
	}
	if len(rec2.finishedCalls) != 1 {
		t.Errorf("sink2: expected 1 RunFinished call, got %d", len(rec2.finishedCalls))
	}

	// Both sinks must see events in the same order (serial-dispatch contract).
	if len(rec1.agentEvents) != len(rec2.agentEvents) {
		t.Fatalf("sinks disagree on event count: %d vs %d", len(rec1.agentEvents), len(rec2.agentEvents))
	}
	for i := range rec1.agentEvents {
		if rec1.agentEvents[i] != rec2.agentEvents[i] {
			t.Errorf("sinks disagree at event %d: %v vs %v",
				i, rec1.agentEvents[i], rec2.agentEvents[i])
		}
	}
}
