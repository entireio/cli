// Package review — see env.go for package-level rationale.
//
// run_multi.go implements RunMulti, the N-agent orchestrator. It runs each
// AgentReviewer in its own goroutine under a shared context, fans their
// event streams into a single dispatch loop that calls each Sink serially
// (preserving the serial-dispatch contract from CU4), and aggregates the
// per-agent results into a single RunSummary.
//
// Cancellation: when ctx is cancelled (typically by the cobra command's
// SIGINT handling), all per-agent reviewers see ctx.Err() via their
// individual Process implementations and exit. RunMulti waits for every
// goroutine to drain before returning, ensuring no AgentEvent calls fire
// after RunFinished.
//
// Goroutine accounting for N agents:
//   - N forwarding goroutines: one per agent that started successfully,
//     reads proc.Events() and sends tagged events to the fan-in channel.
//   - 1 channel-close goroutine: waits for all N forwarders via WaitGroup
//     then closes the fan-in channel so the dispatch loop terminates.
//   - 1 dispatch goroutine (the caller's): ranges over fan-in, updates
//     per-agent state, calls sinks serially. RunMulti does NOT spawn a
//     separate dispatch goroutine — the fan-in loop runs on the RunMulti
//     call stack, keeping the goroutine count low.
package review

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

// perAgentState tracks the accumulation for one agent during a multi-agent run.
//
// Concurrency: perAgentState has a single writer after initialization. The
// immutable fields (name/agentName/model/startedAt) are set before launch; all
// terminal state (startErr/waitErr/finishedAt/timedOut) and event-derived state
// are written by the dispatch loop as events and terminal markers arrive over
// fanIn. The per-agent forwarding goroutines NEVER touch perAgentState; they
// only send on fanIn. So there is no cross-goroutine field sharing, and the
// post-loop accounting reads are safe by construction (the dispatch loop has
// already returned).
type perAgentState struct {
	name         string
	agentName    string
	model        string
	proc         reviewtypes.Process
	startErr     error
	startedAt    time.Time
	finishedAt   time.Time
	buffer       []reviewtypes.Event
	tokens       reviewtypes.Tokens
	finishedSeen bool
	finishedOk   bool
	sawRunError  bool
	timedOut     bool
	waitErr      error
}

// taggedEvent associates a fan-in item with its originating agent index. It is
// either an agent event (ev set) or the agent's terminal marker (terminal set),
// the latter carrying end-of-run fields so the dispatch loop stays the sole
// writer of perAgentState.
type taggedEvent struct {
	agentIdx int
	ev       reviewtypes.Event
	terminal *agentTerminal
}

// agentTerminal carries an agent's end-of-run results to the dispatch loop,
// which writes them into perAgentState. Forwarding goroutines send this after
// Wait; the setup loop queues the same marker shape for Start failures.
type agentTerminal struct {
	startErr   error
	waitErr    error
	finishedAt time.Time
	timedOut   bool
}

// RunMulti executes a multi-agent review. Each reviewer runs concurrently;
// events from N agents are funneled into a single dispatch loop that calls
// every sink's AgentEvent in arrival order from a single goroutine
// (serial-dispatch guarantee).
//
// Returns the aggregated RunSummary plus the FIRST non-cancellation
// per-agent error encountered. Callers that want individual errors should
// inspect summary.AgentRuns[i].Err. The returned summary is always
// populated, even on error; sinks are always notified via RunFinished.
//
// Cancellation propagates through ctx; each reviewer's Process honors it.
// When ctx is cancelled all per-agent goroutines will eventually drain and
// the channel will close; RunMulti waits for all before calling RunFinished.
func RunMulti(
	ctx context.Context,
	reviewers []reviewtypes.AgentReviewer,
	cfg reviewtypes.RunConfig,
	sinks []reviewtypes.Sink,
) (reviewtypes.RunSummary, error) {
	started := time.Now()

	if len(reviewers) == 0 {
		summary := reviewtypes.RunSummary{
			StartedAt:  started,
			FinishedAt: started,
			Cancelled:  ctx.Err() != nil,
		}
		for _, sink := range sinks {
			sink.RunFinished(summary)
		}
		return summary, nil
	}

	plannedRuns := plannedAgentRunsForReviewers(reviewers, cfg)
	states := make([]*perAgentState, len(reviewers))
	for i, run := range plannedRuns {
		states[i] = &perAgentState{
			name:      run.Name,
			agentName: run.AgentName,
			model:     run.Model,
			startedAt: time.Now(),
		}
	}

	// fanIn carries tagged events from N agent goroutines into the single
	// dispatch loop. Reserve event-burst slack plus one terminal-marker slot per
	// reviewer, so the worst case of every reviewer failing Start fits entirely in
	// the terminal reservation without consuming event slack.
	const eventBurstSlotsPerReviewer = 16
	reviewerCount := len(reviewers)
	terminalSlots := reviewerCount
	fanInCapacity := reviewerCount*eventBurstSlotsPerReviewer + terminalSlots
	fanIn := make(chan taggedEvent, fanInCapacity)

	// Each reviewer runs under its own deadline (unless reviewerTimeout returns
	// 0, meaning disabled) so a stuck agent is cancelled without hanging the run;
	// siblings and the judge proceed.
	timeout := reviewerTimeout(cfg)
	var wg sync.WaitGroup
	startTerminals := make([]taggedEvent, 0)
	for i, r := range reviewers {
		agentCtx := ctx
		var cancelAgent context.CancelFunc = func() {}
		if timeout > 0 {
			agentCtx, cancelAgent = withReviewerTimeout(ctx, timeout)
		}
		proc, err := r.Start(agentCtx, cfg)
		if err != nil {
			// No Process exists, so there is no Events/Wait lifecycle to preserve.
			// Build the terminal marker before cancelAgent so a Start call that blocked
			// until the per-reviewer deadline keeps the reviewer-timeout cause visible.
			// Then cancel immediately to release the per-agent timeout timer while
			// siblings continue running.
			startTerminals = append(startTerminals, startFailureTerminal(ctx, agentCtx, i, err))
			cancelAgent()
			continue
		}
		states[i].proc = proc
		wg.Add(1)
		go func(idx int, p reviewtypes.Process, runCtx context.Context, cancel context.CancelFunc) {
			defer wg.Done()
			defer cancel()
			for ev := range p.Events() {
				fanIn <- taggedEvent{agentIdx: idx, ev: ev}
			}
			waitErr := p.Wait()
			finishedAt := time.Now()
			if shouldEmitSyntheticRunError(runCtx, waitErr) {
				fanIn <- taggedEvent{agentIdx: idx, ev: reviewtypes.RunError{Err: waitErr}}
			}
			emitEnrichedAgentTokens(ctx, cfg, fanIn, idx, reviewtypes.AgentRun{
				Name:      states[idx].name,
				AgentName: states[idx].agentName,
				Model:     states[idx].model,
				StartedAt: states[idx].startedAt,
				Duration:  finishedAt.Sub(states[idx].startedAt),
				Err:       waitErr,
			})
			// Hand the terminal result to the dispatch loop so perAgentState has a
			// single writer. Classify the timeout from waitErr (the cause the
			// Process captured at Wait). If an implementation formats ctx.Err()
			// without preserving the sentinel, fall back to the per-agent context only
			// when Wait returned an error; nil Wait still means natural completion.
			fanIn <- taggedEvent{agentIdx: idx, terminal: &agentTerminal{
				waitErr:    waitErr,
				finishedAt: finishedAt,
				timedOut:   reviewerDeadlineFired(ctx, runCtx, waitErr),
			}}
		}(i, proc, agentCtx, cancelAgent)
	}

	// Close fanIn after queued Start-failure markers are delivered and all
	// forwarding goroutines finish. This goroutine must be launched AFTER all
	// wg.Add calls above so the WaitGroup counter is correct before Wait is
	// called. Sending startTerminals here (instead of from the setup loop) avoids
	// blocking setup if an early-started agent fills fanIn before dispatch begins.
	go func() {
		for _, tagged := range startTerminals {
			fanIn <- tagged
		}
		wg.Wait()
		close(fanIn)
	}()

	// Dispatch loop: runs on the caller's goroutine (no extra goroutine).
	// Ranges over fanIn until it is closed, updating per-agent state and
	// forwarding to sinks serially — the serial-dispatch contract holds
	// even though N agent goroutines emit concurrently.
	for tagged := range fanIn {
		st := states[tagged.agentIdx]
		if tagged.terminal != nil {
			// End-of-run marker (internal): record terminal fields, don't forward
			// to sinks.
			st.startErr = tagged.terminal.startErr
			st.waitErr = tagged.terminal.waitErr
			st.finishedAt = tagged.terminal.finishedAt
			st.timedOut = tagged.terminal.timedOut
			continue
		}
		st.buffer = append(st.buffer, tagged.ev)
		switch e := tagged.ev.(type) {
		case reviewtypes.Tokens:
			st.tokens = e // Tokens is cumulative — overwrite, don't sum.
		case reviewtypes.Finished:
			st.finishedSeen = true
			st.finishedOk = e.Success
		case reviewtypes.RunError:
			st.sawRunError = true
		default:
			// Started, AssistantText, ToolCall — no state to update.
		}
		for _, sink := range sinks {
			sink.AgentEvent(st.name, tagged.ev)
		}
	}

	// The dispatch loop above is the sole writer of perAgentState and has
	// returned (fanIn closed after wg.Wait()), so reading the per-agent fields
	// below is safe.
	finished := time.Now()
	cancelled := ctx.Err() != nil

	agentRuns := make([]reviewtypes.AgentRun, len(states))
	var firstErr error
	for i, st := range states {
		var status reviewtypes.AgentStatus
		if st.startErr != nil {
			status = classifyStatus(ctx, st.startErr, eventOutcome{})
		} else {
			status = classifyStatus(ctx, st.waitErr, eventOutcome{
				finishedSeen: st.finishedSeen,
				finishedOk:   st.finishedOk,
				sawRunError:  st.sawRunError,
			})
		}
		agentErr := st.startErr
		if agentErr == nil {
			agentErr = st.waitErr
		}
		if st.timedOut {
			status = reviewtypes.AgentStatusFailed
			agentErr = timedOutError(st.name, timeout)
		}
		agentRuns[i] = reviewtypes.AgentRun{
			Name:      st.name,
			AgentName: st.agentName,
			Model:     st.model,
			Status:    status,
			Tokens:    st.tokens,
			Buffer:    st.buffer,
			StartedAt: st.startedAt,
			Duration:  st.finishedAt.Sub(st.startedAt),
			Err:       agentErr,
		}
		if firstErr == nil && agentErr != nil && status != reviewtypes.AgentStatusCancelled {
			firstErr = agentErr
		}
	}

	summary := reviewtypes.RunSummary{
		StartedAt:  started,
		FinishedAt: finished,
		Cancelled:  cancelled,
		AgentRuns:  agentRuns,
	}
	summary = enrichRunSummary(ctx, cfg, summary)
	for _, sink := range sinks {
		sink.RunFinished(summary)
	}

	return summary, firstErr
}

func plannedAgentRunsForReviewers(reviewers []reviewtypes.AgentReviewer, cfg reviewtypes.RunConfig) []reviewtypes.AgentRun {
	planned := make([]reviewtypes.AgentRun, len(reviewers))
	for i, r := range reviewers {
		// Mirror Run's fallback: when a reviewer carries no model metadata, use
		// the run config's model so session-to-manifest matching still sees the
		// model that was actually requested.
		model := reviewerModelName(r)
		if model == "" {
			model = cfg.Model
		}
		planned[i] = reviewtypes.AgentRun{
			Name:      r.Name(),
			AgentName: reviewerActualAgentName(r),
			Model:     model,
		}
	}
	return planned
}

func startFailureTerminal(parentCtx, agentCtx context.Context, agentIdx int, startErr error) taggedEvent {
	return taggedEvent{agentIdx: agentIdx, terminal: &agentTerminal{
		startErr:   startErr,
		finishedAt: time.Now(),
		timedOut:   reviewerDeadlineFired(parentCtx, agentCtx, startErr),
	}}
}

func emitEnrichedAgentTokens(
	ctx context.Context,
	cfg reviewtypes.RunConfig,
	fanIn chan<- taggedEvent,
	agentIdx int,
	run reviewtypes.AgentRun,
) {
	if cfg.EnrichAgentRun == nil {
		return
	}
	enriched, ok := callEnrichAgentRun(ctx, cfg.EnrichAgentRun, run)
	if !ok {
		return
	}
	if enriched.Tokens.In == 0 && enriched.Tokens.Out == 0 {
		return
	}
	fanIn <- taggedEvent{agentIdx: agentIdx, ev: enriched.Tokens}
}

// callEnrichAgentRun invokes the caller-supplied EnrichAgentRun callback
// with panic recovery. A panic in user-supplied enrichment must not leak
// into the per-agent forwarding goroutine and bring down the whole run.
func callEnrichAgentRun(ctx context.Context, fn func(context.Context, reviewtypes.AgentRun) reviewtypes.AgentRun, run reviewtypes.AgentRun) (out reviewtypes.AgentRun, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			logging.Warn(ctx, "review EnrichAgentRun panicked", slog.Any("panic", r))
			ok = false
		}
	}()
	return fn(ctx, run), true
}
