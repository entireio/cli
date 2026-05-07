package review

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

// TestRunMulti_BothSucceed verifies that two agents that complete cleanly
// produce two AgentRuns with status Succeeded, that all events from both
// agents reach the sink, and that sinks see RunFinished exactly once.
func TestRunMulti_BothSucceed(t *testing.T) {
	t.Parallel()
	eventsA := []reviewtypes.Event{
		reviewtypes.Started{},
		reviewtypes.AssistantText{Text: "agent-a review"},
		reviewtypes.Finished{Success: true},
	}
	eventsB := []reviewtypes.Event{
		reviewtypes.Started{},
		reviewtypes.AssistantText{Text: "agent-b review"},
		reviewtypes.Finished{Success: true},
	}
	ra := &stubReviewer{name: "agent-a", events: eventsA}
	rb := &stubReviewer{name: "agent-b", events: eventsB}
	rec := &stubSinkRecorder{}

	summary, err := RunMulti(context.Background(), []reviewtypes.AgentReviewer{ra, rb}, reviewtypes.RunConfig{}, []reviewtypes.Sink{rec})
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if summary.Cancelled {
		t.Error("expected Cancelled=false")
	}
	if len(summary.AgentRuns) != 2 {
		t.Fatalf("expected 2 AgentRuns, got %d", len(summary.AgentRuns))
	}
	for _, run := range summary.AgentRuns {
		if run.Status != reviewtypes.AgentStatusSucceeded {
			t.Errorf("agent %s: expected Succeeded, got %v", run.Name, run.Status)
		}
	}
	// Total events from both agents: 3 + 3 = 6.
	if len(rec.agentEvents) != 6 {
		t.Errorf("expected 6 AgentEvent calls total, got %d", len(rec.agentEvents))
	}
	// RunFinished called exactly once.
	if len(rec.finishedCalls) != 1 {
		t.Errorf("expected 1 RunFinished call, got %d", len(rec.finishedCalls))
	}
}

// TestRunMulti_OneSucceedsOneFails verifies that an agent failure does not
// block the other agent, firstErr is non-nil, and both AgentRuns are
// populated.
func TestRunMulti_OneSucceedsOneFails(t *testing.T) {
	t.Parallel()
	fakeErr := errors.New("exit status 1")
	ra := &stubReviewer{
		name:   "ok-agent",
		events: []reviewtypes.Event{reviewtypes.Started{}, reviewtypes.Finished{Success: true}},
	}
	rb := &stubReviewer{
		name:    "fail-agent",
		events:  []reviewtypes.Event{reviewtypes.Started{}, reviewtypes.Finished{Success: false}},
		waitErr: fakeErr,
	}
	rec := &stubSinkRecorder{}

	summary, err := RunMulti(context.Background(), []reviewtypes.AgentReviewer{ra, rb}, reviewtypes.RunConfig{}, []reviewtypes.Sink{rec})
	if err == nil {
		t.Fatal("expected non-nil firstErr")
	}
	if len(summary.AgentRuns) != 2 {
		t.Fatalf("expected 2 AgentRuns, got %d", len(summary.AgentRuns))
	}
	// Both agents delivered events to the sink.
	if len(rec.agentEvents) != 4 {
		t.Errorf("expected 4 AgentEvent calls (2 per agent), got %d", len(rec.agentEvents))
	}
	// Verify per-agent statuses.
	statusFor := func(name string) reviewtypes.AgentStatus {
		for _, r := range summary.AgentRuns {
			if r.Name == name {
				return r.Status
			}
		}
		return reviewtypes.AgentStatusUnknown
	}
	if got := statusFor("ok-agent"); got != reviewtypes.AgentStatusSucceeded {
		t.Errorf("ok-agent: expected Succeeded, got %v", got)
	}
	if got := statusFor("fail-agent"); got != reviewtypes.AgentStatusFailed {
		t.Errorf("fail-agent: expected Failed, got %v", got)
	}
}

// TestRunMulti_StartErrorForOneAgent verifies that an agent that fails to
// start produces an AgentRun with startErr and Failed status, while the other
// agent's events are still delivered normally.
func TestRunMulti_StartErrorForOneAgent(t *testing.T) {
	t.Parallel()
	startErr := errors.New("binary not on PATH")
	ra := &stubReviewer{
		name:   "good-agent",
		events: []reviewtypes.Event{reviewtypes.Started{}, reviewtypes.Finished{Success: true}},
	}
	rb := &stubReviewer{
		name:     "bad-start-agent",
		startErr: startErr,
	}
	rec := &stubSinkRecorder{}

	summary, err := RunMulti(context.Background(), []reviewtypes.AgentReviewer{ra, rb}, reviewtypes.RunConfig{}, []reviewtypes.Sink{rec})
	if !errors.Is(err, startErr) {
		t.Errorf("expected startErr in firstErr, got %v", err)
	}
	if len(summary.AgentRuns) != 2 {
		t.Fatalf("expected 2 AgentRuns, got %d", len(summary.AgentRuns))
	}
	// The agent that started successfully still delivers its events.
	if len(rec.agentEvents) != 2 {
		t.Errorf("expected 2 AgentEvent calls from good-agent, got %d", len(rec.agentEvents))
	}
	// RunFinished still called once.
	if len(rec.finishedCalls) != 1 {
		t.Errorf("expected 1 RunFinished call, got %d", len(rec.finishedCalls))
	}
	// Bad-start agent has the start error on its run.
	for _, r := range summary.AgentRuns {
		if r.Name == "bad-start-agent" {
			if r.Status != reviewtypes.AgentStatusFailed {
				t.Errorf("bad-start-agent: expected Failed, got %v", r.Status)
			}
			if !errors.Is(r.Err, startErr) {
				t.Errorf("bad-start-agent: Err = %v, want startErr", r.Err)
			}
		}
	}
}

// TestRunMulti_ContextCancellation verifies that context cancellation causes
// summary.Cancelled=true and all AgentRuns to have status Cancelled.
func TestRunMulti_ContextCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before starting

	ra := &stubReviewer{
		name:     "agent-a",
		startErr: context.Canceled,
	}
	rb := &stubReviewer{
		name:     "agent-b",
		startErr: context.Canceled,
	}
	rec := &stubSinkRecorder{}

	summary, err := RunMulti(ctx, []reviewtypes.AgentReviewer{ra, rb}, reviewtypes.RunConfig{}, []reviewtypes.Sink{rec})
	if err != nil && ctx.Err() == nil {
		t.Logf("RunMulti returned non-nil error (may be cancellation-related): %v", err)
	}
	if !summary.Cancelled {
		t.Error("expected summary.Cancelled=true")
	}
	for _, r := range summary.AgentRuns {
		if r.Status != reviewtypes.AgentStatusCancelled {
			t.Errorf("agent %s: expected Cancelled, got %v", r.Name, r.Status)
		}
	}
	if len(rec.finishedCalls) != 1 {
		t.Errorf("sinks must receive RunFinished even on cancellation: got %d calls", len(rec.finishedCalls))
	}
}

// TestRunMulti_EmptyReviewers verifies that passing no reviewers returns an
// empty RunSummary and still calls RunFinished once on each sink.
func TestRunMulti_EmptyReviewers(t *testing.T) {
	t.Parallel()
	rec := &stubSinkRecorder{}

	summary, err := RunMulti(context.Background(), nil, reviewtypes.RunConfig{}, []reviewtypes.Sink{rec})
	if err != nil {
		t.Fatalf("expected nil error for empty reviewers, got %v", err)
	}
	if len(summary.AgentRuns) != 0 {
		t.Errorf("expected 0 AgentRuns, got %d", len(summary.AgentRuns))
	}
	if len(rec.finishedCalls) != 1 {
		t.Errorf("expected 1 RunFinished call even for empty reviewers, got %d", len(rec.finishedCalls))
	}
}

// TestRunMulti_WithinAgentEventOrder verifies that events within a single
// agent's stream are delivered to the sink in the order emitted. Order across
// agents is non-deterministic (concurrent goroutines), but within-agent
// ordering is guaranteed by the channel + serial dispatch loop.
func TestRunMulti_WithinAgentEventOrder(t *testing.T) {
	t.Parallel()
	eventsA := []reviewtypes.Event{
		reviewtypes.Started{},
		reviewtypes.AssistantText{Text: "a1"},
		reviewtypes.AssistantText{Text: "a2"},
		reviewtypes.Finished{Success: true},
	}
	eventsB := []reviewtypes.Event{
		reviewtypes.Started{},
		reviewtypes.AssistantText{Text: "b1"},
		reviewtypes.Finished{Success: true},
	}
	ra := &stubReviewer{name: "alpha", events: eventsA}
	rb := &stubReviewer{name: "beta", events: eventsB}
	rec := &stubSinkRecorder{}

	_, err := RunMulti(context.Background(), []reviewtypes.AgentReviewer{ra, rb}, reviewtypes.RunConfig{}, []reviewtypes.Sink{rec})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Total events: 4 + 3 = 7.
	if len(rec.agentEvents) != 7 {
		t.Fatalf("expected 7 AgentEvent calls, got %d", len(rec.agentEvents))
	}

	// Verify within-agent ordering: events from "alpha" must appear in their
	// original order (not necessarily contiguous, but in sequence).
	var alphaEvents []reviewtypes.Event
	for _, ae := range rec.agentEvents {
		if ae.agent == "alpha" {
			alphaEvents = append(alphaEvents, ae.ev)
		}
	}
	if len(alphaEvents) != len(eventsA) {
		t.Fatalf("alpha: expected %d events, got %d", len(eventsA), len(alphaEvents))
	}
	for i, ev := range alphaEvents {
		if ev != eventsA[i] {
			t.Errorf("alpha event[%d]: got %v, want %v", i, ev, eventsA[i])
		}
	}

	var betaEvents []reviewtypes.Event
	for _, ae := range rec.agentEvents {
		if ae.agent == "beta" {
			betaEvents = append(betaEvents, ae.ev)
		}
	}
	if len(betaEvents) != len(eventsB) {
		t.Fatalf("beta: expected %d events, got %d", len(eventsB), len(betaEvents))
	}
	for i, ev := range betaEvents {
		if ev != eventsB[i] {
			t.Errorf("beta event[%d]: got %v, want %v", i, ev, eventsB[i])
		}
	}
}

// TestRunMulti_SerialDispatchNoConcurrentSinkCalls verifies the serial-dispatch
// contract: AgentEvent is never called concurrently from two goroutines.
// Uses an atomic counter that would trigger the race detector if two goroutines
// entered AgentEvent simultaneously — run with `go test -race`.
func TestRunMulti_SerialDispatchNoConcurrentSinkCalls(t *testing.T) {
	t.Parallel()
	const numAgents = 4
	const eventsPerAgent = 20

	reviewers := make([]reviewtypes.AgentReviewer, numAgents)
	for i := range reviewers {
		events := make([]reviewtypes.Event, eventsPerAgent)
		for j := range events {
			events[j] = reviewtypes.AssistantText{Text: "x"}
		}
		reviewers[i] = &stubReviewer{name: "agent", events: events}
	}

	sink := &concurrencyCheckSink{}
	_, err := RunMulti(context.Background(), reviewers, reviewtypes.RunConfig{}, []reviewtypes.Sink{sink})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sink.maxConcurrent.Load() > 1 {
		t.Errorf("serial-dispatch contract violated: %d concurrent AgentEvent calls observed", sink.maxConcurrent.Load())
	}
}

// concurrencyCheckSink uses an atomic in-flight counter to detect concurrent
// AgentEvent calls. Under the serial-dispatch guarantee the counter should
// never exceed 1. Running with -race catches any missed synchronization in
// the counter itself.
type concurrencyCheckSink struct {
	inFlight      atomic.Int64
	maxConcurrent atomic.Int64
}

func (s *concurrencyCheckSink) AgentEvent(_ string, _ reviewtypes.Event) {
	current := s.inFlight.Add(1)
	// Update maxConcurrent with a CAS-based max (atomic, no mutex needed).
	for {
		prev := s.maxConcurrent.Load()
		if current <= prev {
			break
		}
		if s.maxConcurrent.CompareAndSwap(prev, current) {
			break
		}
	}
	// Simulate a tiny amount of work to increase the chance of catching
	// a race in the absence of the -race detector.
	time.Sleep(time.Microsecond)
	s.inFlight.Add(-1)
}

func (s *concurrencyCheckSink) RunFinished(_ reviewtypes.RunSummary) {}

// Compile-time interface check.
var _ reviewtypes.Sink = (*concurrencyCheckSink)(nil)

// TestRunMulti_SinkFanOut verifies that two sinks both receive all events
// and both receive RunFinished once.
func TestRunMulti_SinkFanOut(t *testing.T) {
	t.Parallel()
	ra := &stubReviewer{
		name:   "agent-a",
		events: []reviewtypes.Event{reviewtypes.Started{}, reviewtypes.Finished{Success: true}},
	}
	rec1 := &stubSinkRecorder{}
	rec2 := &stubSinkRecorder{}

	_, err := RunMulti(context.Background(), []reviewtypes.AgentReviewer{ra}, reviewtypes.RunConfig{}, []reviewtypes.Sink{rec1, rec2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rec1.agentEvents) != 2 {
		t.Errorf("sink1: expected 2 AgentEvent calls, got %d", len(rec1.agentEvents))
	}
	if len(rec2.agentEvents) != 2 {
		t.Errorf("sink2: expected 2 AgentEvent calls, got %d", len(rec2.agentEvents))
	}
	if len(rec1.finishedCalls) != 1 {
		t.Errorf("sink1: expected 1 RunFinished call, got %d", len(rec1.finishedCalls))
	}
	if len(rec2.finishedCalls) != 1 {
		t.Errorf("sink2: expected 1 RunFinished call, got %d", len(rec2.finishedCalls))
	}
}

// TestRunMulti_TokenTracking verifies Tokens overwrite semantics in multi-agent
// runs: the last Tokens event per agent supersedes earlier ones.
func TestRunMulti_TokenTracking(t *testing.T) {
	t.Parallel()
	ra := &stubReviewer{
		name: "tokenizer",
		events: []reviewtypes.Event{
			reviewtypes.Started{},
			reviewtypes.Tokens{In: 10, Out: 5},
			reviewtypes.Tokens{In: 20, Out: 15}, // supersedes
			reviewtypes.Finished{Success: true},
		},
	}
	rec := &stubSinkRecorder{}

	summary, err := RunMulti(context.Background(), []reviewtypes.AgentReviewer{ra}, reviewtypes.RunConfig{}, []reviewtypes.Sink{rec})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(summary.AgentRuns) != 1 {
		t.Fatalf("expected 1 AgentRun, got %d", len(summary.AgentRuns))
	}
	run := summary.AgentRuns[0]
	if run.Tokens.In != 20 {
		t.Errorf("Tokens.In: got %d, want 20", run.Tokens.In)
	}
	if run.Tokens.Out != 15 {
		t.Errorf("Tokens.Out: got %d, want 15", run.Tokens.Out)
	}
}
