// Package types — see reviewer.go for package-level rationale.
//
// sink.go defines the consumer-side abstractions for review runs:
// Sink (event consumer interface), RunSummary (post-run record),
// AgentRun (per-agent post-run data), and AgentStatus (terminal state enum).
package types

import "time"

// Sink consumes events from a review run. Sinks are passed to the
// orchestrator at construction time; selecting which sinks compose
// is the caller's job (TUI vs. non-TTY, synthesis on/off).
//
// Concurrency: AgentEvent and RunFinished are guaranteed to be invoked
// from a single goroutine — the orchestrator serializes calls even under
// multi-agent runs (CU8 fans events from N agents into one dispatch
// loop). Sinks need not internally synchronize.
//
// Sinks MUST NOT block. AgentEvent runs on the dispatch goroutine; a
// slow sink stalls the entire run and starves all other sinks.
type Sink interface {
	// AgentEvent is called for every event emitted by an agent's
	// process during the run. Events are delivered in-order within a
	// single agent; across agents (CU8 multi-agent) the orchestrator
	// serializes calls so sinks see one event at a time.
	AgentEvent(agent string, ev Event)

	// RunFinished is called once when the run terminates (cleanly,
	// failed, or cancelled). Sinks that only act post-run (DumpSink,
	// synthesis sink) implement only this method (with a no-op
	// AgentEvent).
	RunFinished(summary RunSummary)
}

// RunSummary is the post-run record passed to all sinks.
type RunSummary struct {
	StartedAt  time.Time
	FinishedAt time.Time

	// Cancelled is true iff the orchestrator's run context was cancelled
	// during the run (or before it started). For single-agent runs this
	// is equivalent to AgentRuns[0].Status == AgentStatusCancelled. Under
	// multi-agent runs (CU8), Cancelled means "the run as a whole was
	// cancelled" — individual AgentRuns may have other terminal states
	// if they finished before cancellation propagated.
	Cancelled bool

	AgentRuns []AgentRun
}

// AgentRun is per-agent post-run data.
type AgentRun struct {
	Name   string
	Status AgentStatus
	Tokens Tokens

	// Buffer accumulates the full event log per agent for post-hoc
	// rendering (DumpSink, TUI dump, synthesis).
	//
	// TODO(memory): At review lengths typical today (~100s of events,
	// ~few KB) this is fine. If profiling shows reviews regularly
	// exceed ~10MB of buffered events or ~10000 events, swap to a
	// token-budgeted ring or stream events to sinks incrementally
	// and drop the buffer.
	Buffer []Event

	StartedAt time.Time
	Duration  time.Duration
	Err       error
}

// AgentStatus is the terminal state for an agent.
type AgentStatus int

const (
	AgentStatusUnknown AgentStatus = iota
	AgentStatusSucceeded
	AgentStatusFailed
	AgentStatusCancelled
)

// String returns a human-readable status (used in dump output and TUI).
func (s AgentStatus) String() string {
	switch s {
	case AgentStatusUnknown:
		return "unknown"
	case AgentStatusSucceeded:
		return "succeeded"
	case AgentStatusFailed:
		return "failed"
	case AgentStatusCancelled:
		return "cancelled"
	default:
		return "unknown"
	}
}
