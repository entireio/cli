// Package review — see env.go for package-level rationale.
//
// run.go implements the single-agent orchestrator. CU8 generalizes
// to N agents with a sibling RunMulti that re-uses the same
// AgentRun classification and Sink fan-out.
package review

import (
	"context"
	"fmt"
	"time"

	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

// Run executes a single-agent review. Events from the agent are forwarded
// to all sinks via AgentEvent as they arrive; on completion, RunFinished
// is called on each sink with the populated RunSummary.
//
// Returns the summary plus the agent's terminal error (nil on clean exit,
// ctx.Err() on cancellation, an error wrapping *exec.ExitError on non-zero
// exit, or an agent-level error when the event stream reports failure).
// Callers can inspect both: the summary is always populated even on error.
func Run(
	ctx context.Context,
	reviewer reviewtypes.AgentReviewer,
	cfg reviewtypes.RunConfig,
	sinks []reviewtypes.Sink,
) (reviewtypes.RunSummary, error) {
	started := time.Now()

	proc, err := reviewer.Start(ctx, cfg)
	if err != nil {
		// Construction failed — classify (cancellation vs failure), fan out, return.
		// No event-stream signals available since Start failed before producing any.
		finished := time.Now()
		status := classifyStatus(ctx, err, eventOutcome{})
		summary := reviewtypes.RunSummary{
			StartedAt:  started,
			FinishedAt: finished,
			Cancelled:  status == reviewtypes.AgentStatusCancelled,
			AgentRuns: []reviewtypes.AgentRun{{
				Name:      reviewer.Name(),
				Status:    status,
				Err:       err,
				StartedAt: started,
				Duration:  finished.Sub(started),
			}},
		}
		for _, sink := range sinks {
			sink.RunFinished(summary)
		}
		return summary, err //nolint:wrapcheck // interface-boundary passthrough; wrapping breaks classifyStatus's ctx.Err() identity check for cancelled-during-Start scenarios
	}

	var (
		buffer       []reviewtypes.Event
		tokens       reviewtypes.Tokens
		finishedSeen bool // saw Finished{...} event from the parser
		finishedOk   bool // its Success field
		sawRunError  bool // saw at least one RunError event
		firstRunErr  error
	)
	for ev := range proc.Events() {
		buffer = append(buffer, ev)
		switch e := ev.(type) {
		case reviewtypes.Tokens:
			tokens = e // Tokens is cumulative per CU2 contract — overwrite.
		case reviewtypes.Finished:
			finishedSeen = true
			finishedOk = e.Success
		case reviewtypes.RunError:
			sawRunError = true
			if firstRunErr == nil {
				firstRunErr = e.Err
			}
		}
		for _, sink := range sinks {
			sink.AgentEvent(reviewer.Name(), ev)
		}
	}

	waitErr := proc.Wait()
	finished := time.Now()
	status := classifyStatus(ctx, waitErr, eventOutcome{finishedSeen: finishedSeen, finishedOk: finishedOk, sawRunError: sawRunError})
	runErr := waitErr
	if runErr == nil && status == reviewtypes.AgentStatusFailed {
		runErr = agentRunFailureError(reviewer.Name(), firstRunErr)
	}

	summary := reviewtypes.RunSummary{
		StartedAt:  started,
		FinishedAt: finished,
		Cancelled:  status == reviewtypes.AgentStatusCancelled,
		AgentRuns: []reviewtypes.AgentRun{{
			Name:      reviewer.Name(),
			Status:    status,
			Tokens:    tokens,
			Buffer:    buffer,
			StartedAt: started,
			Duration:  finished.Sub(started),
			Err:       runErr,
		}},
	}
	for _, sink := range sinks {
		sink.RunFinished(summary)
	}
	return summary, runErr
}

func agentRunFailureError(agent string, cause error) error {
	if cause != nil {
		return fmt.Errorf("review agent %s reported failure: %w", agent, cause)
	}
	return fmt.Errorf("review agent %s reported failure", agent)
}

// eventOutcome summarises agent-level signals observed in the event stream.
// Used by classifyStatus to downgrade a process-level "exit 0" run that the
// parser flagged as torn (RunError + Finished{Success: false}) — a clean
// process exit doesn't imply a clean review result.
type eventOutcome struct {
	finishedSeen bool // a Finished event was emitted (parser ran to completion)
	finishedOk   bool // Finished.Success
	sawRunError  bool // at least one RunError event was emitted
}

// classifyStatus maps a process Wait error AND the event-stream outcome to
// a terminal AgentStatus.
//
// Order matters: ctx-cancelled + signal-killed delivers an *exec.ExitError
// with exit code -1, which would otherwise be misclassified as Failed.
// Check ctx.Err() FIRST so cancelled runs are reported as Cancelled.
// (Reference: PR #1018 commit 8e82e407a.)
//
// Event-level signals override process-level success: if the parser saw a
// RunError or emitted Finished{Success: false}, the run is Failed even when
// the process exits 0. This catches torn stdout streams the parser surfaces
// via RunError + Finished{Success: false} (CU3 fix-loop guarantee).
func classifyStatus(ctx context.Context, waitErr error, outcome eventOutcome) reviewtypes.AgentStatus {
	if ctx.Err() != nil {
		return reviewtypes.AgentStatusCancelled
	}
	if waitErr != nil {
		return reviewtypes.AgentStatusFailed
	}
	// Process exited cleanly (exit 0). Honor agent-level signals.
	if outcome.sawRunError {
		return reviewtypes.AgentStatusFailed
	}
	if outcome.finishedSeen && !outcome.finishedOk {
		return reviewtypes.AgentStatusFailed
	}
	return reviewtypes.AgentStatusSucceeded
}
