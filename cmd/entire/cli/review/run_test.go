package review

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

// ctxReviewer's process hangs until its run context is done, then reports the
// context error — modeling an agent that would run forever until the
// orchestrator's per-reviewer deadline cancels (kills) it.
type ctxReviewer struct{ name string }

func (r *ctxReviewer) Name() string { return r.name }
func (r *ctxReviewer) Start(ctx context.Context, _ reviewtypes.RunConfig) (reviewtypes.Process, error) {
	return &ctxProcess{ctx: ctx}, nil
}

type ctxProcess struct{ ctx context.Context }

type deadlineHidingContext struct{ context.Context }

func (deadlineHidingContext) Deadline() (time.Time, bool) { return time.Time{}, false }

func (p *ctxProcess) Events() <-chan reviewtypes.Event {
	out := make(chan reviewtypes.Event)
	go func() {
		<-p.ctx.Done()
		close(out)
	}()
	return out
}

func (p *ctxProcess) Wait() error {
	<-p.ctx.Done()
	return p.ctx.Err()
}

type startBlockingReviewer struct {
	name       string
	stringWrap bool
}

func (r *startBlockingReviewer) Name() string { return r.name }
func (r *startBlockingReviewer) Start(ctx context.Context, _ reviewtypes.RunConfig) (reviewtypes.Process, error) {
	<-ctx.Done()
	if r.stringWrap {
		return nil, errors.New("start failed: " + ctx.Err().Error())
	}
	return nil, ctx.Err()
}

type stringWrappedCtxReviewer struct{ name string }

func (r *stringWrappedCtxReviewer) Name() string { return r.name }
func (r *stringWrappedCtxReviewer) Start(ctx context.Context, _ reviewtypes.RunConfig) (reviewtypes.Process, error) {
	return &stringWrappedCtxProcess{ctx: ctx}, nil
}

type stringWrappedCtxProcess struct{ ctx context.Context }

func (p *stringWrappedCtxProcess) Events() <-chan reviewtypes.Event {
	out := make(chan reviewtypes.Event)
	go func() {
		<-p.ctx.Done()
		close(out)
	}()
	return out
}

func (p *stringWrappedCtxProcess) Wait() error {
	<-p.ctx.Done()
	// Deliberately do NOT wrap with %w. This models an adapter that formats the
	// context error and loses the context.DeadlineExceeded sentinel.
	return errors.New("agent failed: " + p.ctx.Err().Error())
}

type delayedWaitReviewer struct {
	name    string
	delay   time.Duration
	waitErr error
}

func (r *delayedWaitReviewer) Name() string { return r.name }
func (r *delayedWaitReviewer) Start(context.Context, reviewtypes.RunConfig) (reviewtypes.Process, error) {
	return &delayedWaitProcess{delay: r.delay, waitErr: r.waitErr}, nil
}

type delayedWaitProcess struct {
	delay   time.Duration
	waitErr error
}

func (p *delayedWaitProcess) Events() <-chan reviewtypes.Event {
	out := make(chan reviewtypes.Event)
	close(out)
	return out
}

func (p *delayedWaitProcess) Wait() error {
	time.Sleep(p.delay)
	return p.waitErr
}

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

func TestRun_EmitsSyntheticRunErrorWhenWaitErrIsNonNil(t *testing.T) {
	t.Parallel()
	waitErr := errors.New("exit status 1: stderr: invalid_api_key")
	reviewer := &stubReviewer{
		name: "claude-code",
		events: []reviewtypes.Event{
			reviewtypes.Started{},
			reviewtypes.Finished{Success: true},
		},
		waitErr: waitErr,
	}
	rec := &stubSinkRecorder{}

	_, err := Run(context.Background(), reviewer, reviewtypes.RunConfig{}, []reviewtypes.Sink{rec})
	if err == nil {
		t.Fatal("expected non-nil error from failing run")
	}

	var found bool
	for _, evt := range rec.agentEvents {
		if re, ok := evt.ev.(reviewtypes.RunError); ok && re.Err != nil && re.Err.Error() == waitErr.Error() {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected synthetic RunError(waitErr) in live sink stream, got events: %+v", rec.agentEvents)
	}
}

func TestRun_DoesNotEmitSyntheticRunErrorOnCleanExit(t *testing.T) {
	t.Parallel()
	reviewer := &stubReviewer{
		name: "claude-code",
		events: []reviewtypes.Event{
			reviewtypes.Started{},
			reviewtypes.AssistantText{Text: "looks good"},
			reviewtypes.Finished{Success: true},
		},
	}
	rec := &stubSinkRecorder{}

	_, err := Run(context.Background(), reviewer, reviewtypes.RunConfig{}, []reviewtypes.Sink{rec})
	if err != nil {
		t.Fatalf("expected nil error on clean exit, got %v", err)
	}

	for _, evt := range rec.agentEvents {
		if _, ok := evt.ev.(reviewtypes.RunError); ok {
			t.Errorf("clean exit should not produce a synthetic RunError, got: %+v", evt.ev)
		}
	}
}

func TestRun_DoesNotEmitSyntheticRunErrorOnCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reviewer := &stubReviewer{
		name: "claude-code",
		events: []reviewtypes.Event{
			reviewtypes.Started{},
			reviewtypes.Finished{Success: true},
		},
		waitErr: context.Canceled,
	}
	rec := &stubSinkRecorder{}

	summary, err := Run(ctx, reviewer, reviewtypes.RunConfig{}, []reviewtypes.Sink{rec})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if got := summary.AgentRuns[0].Status; got != reviewtypes.AgentStatusCancelled {
		t.Fatalf("summary status = %v, want Cancelled", got)
	}
	for _, evt := range rec.agentEvents {
		if _, ok := evt.ev.(reviewtypes.RunError); ok {
			t.Errorf("cancelled run should not produce synthetic RunError, got: %+v", evt.ev)
		}
	}
}

func TestRun_EnrichesSummaryBeforeRunFinished(t *testing.T) {
	t.Parallel()
	reviewer := &stubReviewer{
		name:   "agent-a",
		events: []reviewtypes.Event{reviewtypes.Started{}, reviewtypes.Finished{Success: true}},
	}
	rec := &stubSinkRecorder{}
	cfg := reviewtypes.RunConfig{
		EnrichSummary: func(_ context.Context, summary reviewtypes.RunSummary) reviewtypes.RunSummary {
			summary.AgentRuns[0].Tokens = reviewtypes.Tokens{In: 42, Out: 7}
			return summary
		},
	}

	summary, err := Run(context.Background(), reviewer, cfg, []reviewtypes.Sink{rec})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := summary.AgentRuns[0].Tokens; got.In != 42 || got.Out != 7 {
		t.Fatalf("summary tokens = {%d %d}, want {42 7}", got.In, got.Out)
	}
	if len(rec.finishedCalls) != 1 {
		t.Fatalf("finished calls = %d, want 1", len(rec.finishedCalls))
	}
	if got := rec.finishedCalls[0].AgentRuns[0].Tokens; got.In != 42 || got.Out != 7 {
		t.Fatalf("sink summary tokens = {%d %d}, want {42 7}", got.In, got.Out)
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

func TestReviewerDeadlineFired_EqualParentDeadlineIsNotReviewerTimeout(t *testing.T) {
	t.Parallel()
	deadline := time.Now().Add(20 * time.Millisecond)
	parentCtx, cancelParent := context.WithDeadline(context.Background(), deadline)
	defer cancelParent()
	agentCtx, cancelAgent := context.WithDeadlineCause(parentCtx, deadline, errReviewerTimeoutCause)
	defer cancelAgent()

	select {
	case <-agentCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("agent context deadline did not fire")
	}
	waitErr := errors.New("agent failed: " + context.DeadlineExceeded.Error())
	if reviewerDeadlineFired(parentCtx, agentCtx, waitErr) {
		t.Fatal("equal parent/agent deadlines should be treated as parent deadline, not reviewer timeout")
	}
}

func TestReviewerDeadlineFired_EarlierAgentDeadlineIsReviewerTimeout(t *testing.T) {
	t.Parallel()
	parentCtx, cancelParent := context.WithDeadline(context.Background(), time.Now().Add(time.Second))
	defer cancelParent()
	agentCtx, cancelAgent := withReviewerTimeout(parentCtx, 20*time.Millisecond)
	defer cancelAgent()

	select {
	case <-agentCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("agent context deadline did not fire")
	}
	waitErr := errors.New("agent failed: " + context.DeadlineExceeded.Error())
	if !reviewerDeadlineFired(parentCtx, agentCtx, waitErr) {
		t.Fatal("earlier agent deadline should classify as reviewer timeout")
	}
}

func TestReviewerDeadlineFired_ContextCanceledIsNotReviewerTimeout(t *testing.T) {
	t.Parallel()
	parentCtx := context.Background()
	agentCtx, cancelAgent := withReviewerTimeout(parentCtx, 20*time.Millisecond)
	defer cancelAgent()

	select {
	case <-agentCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("agent context deadline did not fire")
	}
	if errors.Is(agentCtx.Err(), context.Canceled) {
		t.Fatal("deadline-fired context should report DeadlineExceeded, not Canceled")
	}
	if reviewerDeadlineFired(parentCtx, agentCtx, context.Canceled) {
		t.Fatal("context.Canceled wait error should not classify as reviewer timeout")
	}
}

func TestReviewerDeadlineFired_HiddenParentDeadlineIsNotReviewerTimeout(t *testing.T) {
	t.Parallel()
	underlyingParent, cancelParent := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelParent()
	parentCtx := deadlineHidingContext{Context: underlyingParent}
	agentCtx, cancelAgent := withReviewerTimeout(parentCtx, time.Hour)
	defer cancelAgent()

	select {
	case <-agentCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("agent context did not observe hidden parent deadline")
	}
	if errors.Is(context.Cause(agentCtx), errReviewerTimeoutCause) {
		t.Fatal("hidden parent deadline should not use the reviewer timeout cause")
	}
	waitErr := errors.New("agent failed: " + context.DeadlineExceeded.Error())
	if reviewerDeadlineFired(parentCtx, agentCtx, waitErr) {
		t.Fatal("hidden parent deadline should not classify as reviewer timeout")
	}
}

func TestRun_ReviewerTimeoutDuringStart(t *testing.T) {
	t.Parallel()
	rec := &stubSinkRecorder{}
	summary, err := Run(
		context.Background(),
		&startBlockingReviewer{name: "slow-start"},
		reviewtypes.RunConfig{ReviewerTimeout: 20 * time.Millisecond},
		[]reviewtypes.Sink{rec},
	)

	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want a 'timed out' error", err)
	}
	if summary.Cancelled {
		t.Error("Cancelled should be false for a per-reviewer timeout during Start")
	}
	if len(summary.AgentRuns) != 1 {
		t.Fatalf("AgentRuns = %d, want 1", len(summary.AgentRuns))
	}
	run := summary.AgentRuns[0]
	if run.Status != reviewtypes.AgentStatusFailed {
		t.Fatalf("status = %v, want Failed", run.Status)
	}
	if run.Err == nil || !strings.Contains(run.Err.Error(), "timed out") {
		t.Fatalf("run.Err = %v, want 'timed out'", run.Err)
	}
	if len(rec.finishedCalls) != 1 {
		t.Fatalf("RunFinished calls = %d, want 1", len(rec.finishedCalls))
	}
}

func TestRun_ReviewerTimeout(t *testing.T) {
	t.Parallel()
	rec := &stubSinkRecorder{}
	summary, err := Run(
		context.Background(),
		&ctxReviewer{name: "claude-code"},
		reviewtypes.RunConfig{ReviewerTimeout: 30 * time.Millisecond},
		[]reviewtypes.Sink{rec},
	)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want a 'timed out' error", err)
	}
	if summary.Cancelled {
		t.Error("Cancelled should be false for a per-reviewer timeout (parent ctx not cancelled)")
	}
	if len(summary.AgentRuns) != 1 {
		t.Fatalf("expected 1 AgentRun, got %d", len(summary.AgentRuns))
	}
	run := summary.AgentRuns[0]
	if run.Status != reviewtypes.AgentStatusFailed {
		t.Errorf("status = %v, want Failed", run.Status)
	}
	if run.Err == nil || !strings.Contains(run.Err.Error(), "timed out") {
		t.Errorf("run.Err = %v, want 'timed out'", run.Err)
	}
}

func TestRun_ReviewerTimeoutWithStringWrappedContextError(t *testing.T) {
	t.Parallel()
	rec := &stubSinkRecorder{}
	summary, err := Run(
		context.Background(),
		&stringWrappedCtxReviewer{name: "claude-code"},
		reviewtypes.RunConfig{ReviewerTimeout: 30 * time.Millisecond},
		[]reviewtypes.Sink{rec},
	)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want a 'timed out' error", err)
	}
	if summary.Cancelled {
		t.Error("Cancelled should be false for a per-reviewer timeout")
	}
	if len(summary.AgentRuns) != 1 {
		t.Fatalf("expected 1 AgentRun, got %d", len(summary.AgentRuns))
	}
	if run := summary.AgentRuns[0]; run.Status != reviewtypes.AgentStatusFailed || run.Err == nil || !strings.Contains(run.Err.Error(), "timed out") {
		t.Fatalf("run = {Status:%v Err:%v}, want Failed with timed-out error", run.Status, run.Err)
	}
	for _, evt := range rec.agentEvents {
		if _, ok := evt.ev.(reviewtypes.RunError); ok {
			t.Fatalf("timeout with string-formatted context error should not also emit synthetic RunError, got %+v", evt.ev)
		}
	}
}

func TestRun_DeadlineDuringOrdinaryWaitFailureIsNotTimeout(t *testing.T) {
	t.Parallel()
	ordinaryErr := errors.New("exit status 1")
	summary, err := Run(
		context.Background(),
		&delayedWaitReviewer{name: "claude-code", delay: 30 * time.Millisecond, waitErr: ordinaryErr},
		reviewtypes.RunConfig{ReviewerTimeout: 5 * time.Millisecond},
		nil,
	)
	if !errors.Is(err, ordinaryErr) {
		t.Fatalf("err = %v, want ordinary wait error", err)
	}
	if len(summary.AgentRuns) != 1 {
		t.Fatalf("expected 1 AgentRun, got %d", len(summary.AgentRuns))
	}
	run := summary.AgentRuns[0]
	if run.Status != reviewtypes.AgentStatusFailed {
		t.Fatalf("status = %v, want Failed", run.Status)
	}
	if run.Err == nil || strings.Contains(run.Err.Error(), "timed out") {
		t.Fatalf("run.Err = %v, want ordinary failure, not timeout", run.Err)
	}
}

func TestRunMulti_ReviewerTimeoutDuringStart(t *testing.T) {
	t.Parallel()
	rec := &stubSinkRecorder{}
	fast := &stubReviewer{name: "fast", events: []reviewtypes.Event{
		reviewtypes.Started{},
		reviewtypes.Finished{Success: true},
	}}
	summary, err := RunMulti(
		context.Background(),
		[]reviewtypes.AgentReviewer{&startBlockingReviewer{name: "slow-start", stringWrap: true}, fast},
		reviewtypes.RunConfig{ReviewerTimeout: 20 * time.Millisecond},
		[]reviewtypes.Sink{rec},
	)

	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want a 'timed out' error", err)
	}
	if summary.Cancelled {
		t.Error("Cancelled should be false for a per-reviewer timeout during Start")
	}
	if len(summary.AgentRuns) != 2 {
		t.Fatalf("AgentRuns = %d, want 2", len(summary.AgentRuns))
	}
	byName := map[string]reviewtypes.AgentRun{}
	for _, run := range summary.AgentRuns {
		byName[run.Name] = run
	}
	if run := byName["slow-start"]; run.Status != reviewtypes.AgentStatusFailed || run.Err == nil || !strings.Contains(run.Err.Error(), "timed out") {
		t.Fatalf("slow-start = {Status:%v Err:%v}, want Failed with timed-out error", run.Status, run.Err)
	}
	if run := byName["fast"]; run.Status != reviewtypes.AgentStatusSucceeded {
		t.Fatalf("fast status = %v, want Succeeded", run.Status)
	}
	if len(rec.finishedCalls) != 1 {
		t.Fatalf("RunFinished calls = %d, want 1", len(rec.finishedCalls))
	}
}

func TestRunMulti_ReviewerTimeoutIsolated(t *testing.T) {
	t.Parallel()
	// One reviewer hangs (times out); a sibling finishes cleanly. The run is
	// not cancelled, the hung one is failed-by-timeout, the sibling succeeds.
	hang := &ctxReviewer{name: "slow"}
	fast := &stubReviewer{name: "fast", events: []reviewtypes.Event{
		reviewtypes.Started{},
		reviewtypes.Finished{Success: true},
	}}
	rec := &stubSinkRecorder{}
	summary, err := RunMulti(
		context.Background(),
		[]reviewtypes.AgentReviewer{hang, fast},
		reviewtypes.RunConfig{ReviewerTimeout: 40 * time.Millisecond},
		[]reviewtypes.Sink{rec},
	)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want the timed-out agent's error", err)
	}
	if summary.Cancelled {
		t.Error("Cancelled should be false")
	}
	byName := map[string]reviewtypes.AgentRun{}
	for _, r := range summary.AgentRuns {
		byName[r.Name] = r
	}
	if got := byName["slow"]; got.Status != reviewtypes.AgentStatusFailed ||
		got.Err == nil || !strings.Contains(got.Err.Error(), "timed out") {
		t.Errorf("slow = %+v, want Failed with 'timed out'", got)
	}
	if byName["fast"].Status != reviewtypes.AgentStatusSucceeded {
		t.Errorf("fast status = %v, want Succeeded", byName["fast"].Status)
	}
}

// TestRun_ParentCancelIsNotTimeout pins the timeout-vs-cancel distinction: when
// the parent context is cancelled (user Ctrl+C) before a reviewer's deadline
// can fire, the reviewer is classified Cancelled, not failed-by-timeout. The
// detection reads only the agent context, whose Err() is immutable once set.
func TestRunMulti_ReviewerTimeoutWithStringWrappedContextError(t *testing.T) {
	t.Parallel()
	rec := &stubSinkRecorder{}
	summary, err := RunMulti(
		context.Background(),
		[]reviewtypes.AgentReviewer{&stringWrappedCtxReviewer{name: "slow"}},
		reviewtypes.RunConfig{ReviewerTimeout: 30 * time.Millisecond},
		[]reviewtypes.Sink{rec},
	)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want a 'timed out' error", err)
	}
	if summary.Cancelled {
		t.Error("Cancelled should be false for a per-reviewer timeout")
	}
	if len(summary.AgentRuns) != 1 {
		t.Fatalf("expected 1 AgentRun, got %d", len(summary.AgentRuns))
	}
	if run := summary.AgentRuns[0]; run.Status != reviewtypes.AgentStatusFailed || run.Err == nil || !strings.Contains(run.Err.Error(), "timed out") {
		t.Fatalf("run = {Status:%v Err:%v}, want Failed with timed-out error", run.Status, run.Err)
	}
	for _, evt := range rec.agentEvents {
		if _, ok := evt.ev.(reviewtypes.RunError); ok {
			t.Fatalf("timeout with string-formatted context error should not also emit synthetic RunError, got %+v", evt.ev)
		}
	}
}

func TestRunMulti_DeadlineDuringOrdinaryWaitFailureIsNotTimeout(t *testing.T) {
	t.Parallel()
	ordinaryErr := errors.New("exit status 1")
	summary, err := RunMulti(
		context.Background(),
		[]reviewtypes.AgentReviewer{&delayedWaitReviewer{name: "slow-fail", delay: 30 * time.Millisecond, waitErr: ordinaryErr}},
		reviewtypes.RunConfig{ReviewerTimeout: 5 * time.Millisecond},
		nil,
	)
	if !errors.Is(err, ordinaryErr) {
		t.Fatalf("err = %v, want ordinary wait error", err)
	}
	if len(summary.AgentRuns) != 1 {
		t.Fatalf("expected 1 AgentRun, got %d", len(summary.AgentRuns))
	}
	run := summary.AgentRuns[0]
	if run.Status != reviewtypes.AgentStatusFailed {
		t.Fatalf("status = %v, want Failed", run.Status)
	}
	if run.Err == nil || strings.Contains(run.Err.Error(), "timed out") {
		t.Fatalf("run.Err = %v, want ordinary failure, not timeout", run.Err)
	}
}

func TestRun_ParentCancelIsNotTimeout(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before the (1h) reviewer deadline can elapse
	rec := &stubSinkRecorder{}
	summary, err := Run(
		ctx,
		&ctxReviewer{name: "claude-code"},
		reviewtypes.RunConfig{ReviewerTimeout: time.Hour},
		[]reviewtypes.Sink{rec},
	)
	if err != nil && strings.Contains(err.Error(), "timed out") {
		t.Errorf("returned err = %v, must not be a timeout for a parent cancel", err)
	}
	if !summary.Cancelled {
		t.Error("expected Cancelled=true for a parent-cancelled run")
	}
	if len(summary.AgentRuns) != 1 {
		t.Fatalf("expected 1 AgentRun, got %d", len(summary.AgentRuns))
	}
	run := summary.AgentRuns[0]
	if run.Status != reviewtypes.AgentStatusCancelled {
		t.Errorf("status = %v, want Cancelled", run.Status)
	}
	if run.Err != nil && strings.Contains(run.Err.Error(), "timed out") {
		t.Errorf("err = %v, must not be a timeout for a parent cancel", run.Err)
	}
}

// lateNaturalReviewer's process ignores its context and completes naturally
// (Wait returns nil) only after a delay — modeling a reviewer that finishes
// just as (or after) its deadline elapses. It exercises that timeout
// classification keys off the wait error, not a late re-sample of the agent
// context (which would already read DeadlineExceeded and falsely flag it).
type lateNaturalReviewer struct {
	name  string
	delay time.Duration
}

func (r *lateNaturalReviewer) Name() string { return r.name }
func (r *lateNaturalReviewer) Start(_ context.Context, _ reviewtypes.RunConfig) (reviewtypes.Process, error) {
	return &lateNaturalProcess{delay: r.delay}, nil
}

type lateNaturalProcess struct{ delay time.Duration }

func (p *lateNaturalProcess) Events() <-chan reviewtypes.Event {
	ch := make(chan reviewtypes.Event)
	close(ch)
	return ch
}
func (p *lateNaturalProcess) Wait() error {
	time.Sleep(p.delay)
	return nil // completed cleanly, regardless of the (already-elapsed) deadline
}

func TestRun_NaturalCompletionPastDeadlineIsNotTimeout(t *testing.T) {
	t.Parallel()
	rec := &stubSinkRecorder{}
	summary, err := Run(
		context.Background(),
		&lateNaturalReviewer{name: "claude-code", delay: 30 * time.Millisecond},
		reviewtypes.RunConfig{ReviewerTimeout: 5 * time.Millisecond}, // deadline elapses during Wait
		[]reviewtypes.Sink{rec},
	)
	if err != nil && strings.Contains(err.Error(), "timed out") {
		t.Errorf("err = %v, must not be a timeout for a natural completion", err)
	}
	run := summary.AgentRuns[0]
	if run.Status != reviewtypes.AgentStatusSucceeded {
		t.Errorf("status = %v, want Succeeded (clean completion, not a false timeout)", run.Status)
	}
	if run.Err != nil {
		t.Errorf("run.Err = %v, want nil", run.Err)
	}
}

func TestReviewerTimeout(t *testing.T) {
	t.Parallel()
	if got := reviewerTimeout(reviewtypes.RunConfig{}); got != defaultReviewerTimeout {
		t.Errorf("unset = %v, want default %v", got, defaultReviewerTimeout)
	}
	if got := reviewerTimeout(reviewtypes.RunConfig{ReviewerTimeout: 5 * time.Minute}); got != 5*time.Minute {
		t.Errorf("explicit = %v, want 5m", got)
	}
	if got := reviewerTimeout(reviewtypes.RunConfig{ReviewerTimeout: -1}); got != 0 {
		t.Errorf("disabled (negative) = %v, want 0 (no timeout)", got)
	}
}

// TestResolveReviewerTimeoutArg pins the --timeout flag -> RunConfig sentinel
// mapping: a non-positive flag value (the user passed --timeout 0) becomes the
// negative "disabled" sentinel that turns off both the reviewer bound and the
// judge's deadline; a positive value passes through unchanged.
func TestResolveReviewerTimeoutArg(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{"explicit zero disables", 0, -1},
		{"negative disables", -5 * time.Minute, -1},
		{"positive passes through", 20 * time.Minute, 20 * time.Minute},
	}
	for _, tc := range cases {
		if got := resolveReviewerTimeoutArg(tc.in); got != tc.want {
			t.Errorf("%s: resolveReviewerTimeoutArg(%v) = %v, want %v", tc.name, tc.in, got, tc.want)
		}
	}
}

// TestDefaultReviewerTimeoutValue pins the literal default so an accidental edit
// to the constant is caught (the other timeout tests compare against the
// constant itself and would silently follow a change).
func TestDefaultReviewerTimeoutValue(t *testing.T) {
	t.Parallel()
	if defaultReviewerTimeout != 20*time.Minute {
		t.Errorf("defaultReviewerTimeout = %v, want 20m", defaultReviewerTimeout)
	}
}

// TestTimeoutFlag_ResolvesThroughCommand drives the real --timeout flag through
// the command (parse only, no RunE) and the resolver, covering the full
// flag -> resolveReviewerTimeoutArg chain: 0 (and the default) and a positive
// override. Guards the documented "0 disables" contract against a regression
// that bypasses the resolver.
func TestTimeoutFlag_ResolvesThroughCommand(t *testing.T) {
	t.Parallel()
	parseTimeout := func(args []string) time.Duration {
		cmd := NewCommand(Deps{})
		if err := cmd.ParseFlags(args); err != nil {
			t.Fatalf("ParseFlags(%v): %v", args, err)
		}
		d, err := cmd.Flags().GetDuration("timeout")
		if err != nil {
			t.Fatalf("GetDuration: %v", err)
		}
		return d
	}

	// Default (no flag) is the nonzero default and resolves to a positive bound.
	if d := parseTimeout(nil); d != defaultReviewerTimeout {
		t.Errorf("default --timeout = %v, want %v", d, defaultReviewerTimeout)
	} else if got := resolveReviewerTimeoutArg(d); got != defaultReviewerTimeout {
		t.Errorf("default resolves to %v, want %v", got, defaultReviewerTimeout)
	}
	// --timeout 0 resolves to the negative disable sentinel (reviewers + judge).
	if got := resolveReviewerTimeoutArg(parseTimeout([]string{"--timeout", "0"})); got != -1 {
		t.Errorf("--timeout 0 resolves to %v, want -1 (disabled)", got)
	}
	// A positive override passes through unchanged.
	if got := resolveReviewerTimeoutArg(parseTimeout([]string{"--timeout", "30m"})); got != 30*time.Minute {
		t.Errorf("--timeout 30m resolves to %v, want 30m", got)
	}
}
