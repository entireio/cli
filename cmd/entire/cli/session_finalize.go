package cli

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/session"
)

// sweepCondenseBudget caps how much wall clock one sweep may spend condensing,
// across all sessions it finalizes.
//
// Marking a session ENDED is a single atomic state-file rename; condensing it
// costs ~120ms at best and seconds for a large transcript. Since sessions stay
// un-finalized for up to StaleSessionThreshold (7 days), a first sweep after
// upgrade can have a large backlog, and the callers here are interactive
// commands — including `entire status --json`, which the MCP entire_status tool
// calls, where nothing prints until the sweep returns.
//
// So every candidate is always marked ended (that is what un-sticks it from
// `entire status`), while condensing runs only while the budget lasts. Skipping
// it is the same fail-open path a failed condense already takes: FullyCondensed
// stays false and PostCommit retries, with `entire doctor` reporting the session
// as "ended with uncondensed checkpoint data" in the meantime. A backlog
// therefore drains over successive invocations instead of stalling one.
const sweepCondenseBudget = time.Second

// finalizeExitedSessions finalizes every non-ended session in states whose
// owning agent process has exited (clean /exit, crash, kill, terminal close,
// reboot) without a SessionStop hook firing — ACTIVE mid-turn, or IDLE because
// the agent finished its turn before quitting. Each is finalized exactly as a
// clean session stop would be: the session-stop transition runs (PhaseEnded +
// EndedAt) and pending work is eagerly condensed, subject to
// sweepCondenseBudget.
//
// It refreshes the matched in-memory states from disk after finalizing — so
// callers can re-filter/re-render without their own reload — and returns the
// number finalized. Each session is best-effort: a failure to mark one ended is
// logged and skipped; a condense failure is logged but the session is still
// counted (PostCommit will retry the condense later).
//
// Callers that keep logging after this returns must install their own
// command-scoped logging (ensureCommandLogging) rather than rely on the sweep's:
// the early return below leaves logging untouched, so on the common
// nothing-to-do path there is none. `entire doctor` does exactly that — it goes
// on to condense and discard sessions, whose handlers log.
func finalizeExitedSessions(ctx context.Context, states []*session.State) int {
	// Nothing to do is overwhelmingly the common case, and returning here keeps
	// the sweep off the logging and store setup below entirely.
	if !slices.ContainsFunc(states, (*session.State).OwnerExited) {
		return 0
	}

	// Neither caller (`entire status`, `entire doctor`) initializes logging, so
	// the phase-transition and condense lines below would land on the user's
	// terminal via slog.Default() instead of the log file. A no-op when the
	// caller already installed one for the whole command.
	defer ensureCommandLogging(ctx)()

	logCtx := logging.WithComponent(ctx, "session")
	condenseDeadline := time.Now().Add(sweepCondenseBudget)

	var store *session.StateStore // lazily created on first finalize
	finalized := 0
	for _, st := range states {
		if !st.OwnerExited() {
			continue // cheap pre-filter on the (possibly stale) list snapshot
		}

		// Finalize via the same path a clean SessionStop hook would take, but
		// re-validate OwnerExited on the freshly-loaded state under the lock:
		// a turn may have started since the snapshot and replaced the dead
		// owner with a live one, in which case ended is false and we leave it be.
		ended, err := endSessionNow(ctx, nil, st.SessionID, func(s *session.State) bool {
			return s.OwnerExited()
		}, condenseDeadline, endedWhenLastSeen)
		if err != nil {
			logging.Warn(logCtx, "failed to finalize exited session",
				slog.String("session_id", st.SessionID),
				slog.String("error", err.Error()))
			continue
		}
		if !ended {
			continue
		}

		// Refresh the in-memory snapshot from disk so downstream filtering and
		// doctor classification see the true post-finalize state: ended, and
		// condensed only if the eager condense actually ran and succeeded (it is
		// fail-open and budget-capped, so StepCount/FullyCondensed must not be
		// assumed). Fall back to a minimal ended-marking if the reload fails —
		// enough for the caller's "active" filter to drop it.
		if store == nil {
			if s, serr := session.NewStateStore(ctx); serr == nil {
				store = s
			}
		}
		refreshed := false
		if store != nil {
			if reloaded, lerr := store.Load(ctx, st.SessionID); lerr == nil && reloaded != nil {
				*st = *reloaded
				refreshed = true
			}
		}
		if !refreshed {
			endedAt := sessionEndedAt(st, endedWhenLastSeen)
			st.Phase = session.PhaseEnded
			st.EndedAt = &endedAt
		}
		finalized++
	}
	return finalized
}
