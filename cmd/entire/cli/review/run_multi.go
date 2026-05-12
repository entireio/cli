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

// perAgentState tracks the mutable accumulation for one agent during a
// multi-agent run.
//
// Write paths (no mutex; the close-after-wait protocol below provides
// happens-before for both):
//   - waitErr and finishedAt are written by the per-agent forwarding
//     goroutine after its proc.Events range loop exits. That goroutine may
//     still send final derived events (synthetic RunError, enriched Tokens)
//     before wg.Done.
//   - All other mutable fields (events buffer, tokens, finishedSeen,
//     finishedOk, sawRunError) are written from the single dispatch loop
//     reading the fan-in channel.
//
// Read path: the main RunMulti goroutine reads every field only after
// `for ev := range fanIn` returns, which is sequenced after wg.Wait →
// close(fanIn) by the close goroutine. That sequencing is the
// happens-before for both write paths.
//
// DO NOT add new writers from goroutines outside this protocol — adding
// a third write path would require a mutex (or a redesign).
type perAgentState struct {
	name         string
	proc         reviewtypes.Process
	startErr     error
	startedAt    time.Time
	finishedAt   time.Time
	buffer       []reviewtypes.Event
	tokens       reviewtypes.Tokens
	finishedSeen bool
	finishedOk   bool
	sawRunError  bool
	waitErr      error
}

// taggedEvent associates a fan-in event with its originating agent index.
type taggedEvent struct {
	agentIdx int
	ev       reviewtypes.Event
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

	states := make([]*perAgentState, len(reviewers))
	for i, r := range reviewers {
		states[i] = &perAgentState{
			name:      r.Name(),
			startedAt: time.Now(),
		}
	}

	// fanIn carries tagged events from N agent goroutines into the single
	// dispatch loop. Buffer of len(reviewers)*16 amortises goroutine
	// scheduling jitter without holding an unbounded queue.
	fanIn := make(chan taggedEvent, len(reviewers)*16)

	var wg sync.WaitGroup
	for i, r := range reviewers {
		proc, err := r.Start(ctx, cfg)
		if err != nil {
			states[i].startErr = err
			states[i].finishedAt = time.Now()
			continue
		}
		states[i].proc = proc
		wg.Add(1)
		go func(idx int, p reviewtypes.Process) {
			defer wg.Done()
			for ev := range p.Events() {
				fanIn <- taggedEvent{agentIdx: idx, ev: ev}
			}
			waitErr := p.Wait()
			finishedAt := time.Now()
			states[idx].waitErr = waitErr
			states[idx].finishedAt = finishedAt
			if shouldEmitSyntheticRunError(ctx, waitErr) {
				fanIn <- taggedEvent{agentIdx: idx, ev: reviewtypes.RunError{Err: waitErr}}
			}
			emitEnrichedAgentTokens(ctx, cfg, fanIn, idx, reviewtypes.AgentRun{
				Name:      states[idx].name,
				StartedAt: states[idx].startedAt,
				Duration:  finishedAt.Sub(states[idx].startedAt),
				Err:       waitErr,
			})
		}(i, proc)
	}

	// Close fanIn after all forwarding goroutines finish. This goroutine
	// must be launched AFTER all wg.Add calls above so the WaitGroup
	// counter is correct before Wait is called.
	go func() {
		wg.Wait()
		close(fanIn)
	}()

	// Dispatch loop: runs on the caller's goroutine (no extra goroutine).
	// Ranges over fanIn until it is closed, updating per-agent state and
	// forwarding to sinks serially — the serial-dispatch contract holds
	// even though N agent goroutines emit concurrently.
	for tagged := range fanIn {
		st := states[tagged.agentIdx]
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

	// All goroutines have exited; all waitErr fields are set.
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
		agentRuns[i] = reviewtypes.AgentRun{
			Name:      st.name,
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
