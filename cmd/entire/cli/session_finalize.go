package cli

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
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
// stays false, PostCommit handles sessions with pending files, and doctor retries
// no-files ENDED sessions. A backlog therefore drains over successive
// invocations instead of stalling one.
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
// counted so PostCommit or doctor can retry it later, depending on pending files.
func finalizeExitedSessions(ctx context.Context, states []*session.State) int {
	// Nothing to do is overwhelmingly the common case, and returning here keeps
	// the sweep off the logging and store setup below entirely.
	if !slices.ContainsFunc(states, (*session.State).OwnerExited) {
		return 0
	}

	logCtx := logging.WithComponent(ctx, "session")
	condenseDeadline := time.Now().Add(sweepCondenseBudget)

	// This sweep writes checkpoints; a scanner-config failure must not silently
	// use the default scanner set, so skip the condense work (an already-expired
	// deadline) while still marking sessions ended below.
	if err := strategy.EnsureRedactionConfigured(ctx); err != nil {
		logging.Warn(logging.WithComponent(ctx, "redaction"),
			"skipping sweep condense: redaction scanner configuration failed",
			slog.String("error", err.Error()))
		condenseDeadline = time.Now()
	}

	// Created up front: the record-completion pre-check and the post-finalize
	// refresh below both need fresh loads.
	store, storeErr := session.NewStateStore(ctx)
	if storeErr != nil {
		store = nil
	}
	finalized := 0
	for _, st := range states {
		if !st.OwnerExited() {
			continue // cheap pre-filter on the (possibly stale) list snapshot
		}

		sweepCompleteLiveTaskRecords(ctx, store, st)

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

// sweepCompleteLiveTaskRecords completes a dead-owner session's live task
// records ahead of the sweep's endSessionNow, exactly as a clean SessionEnd
// does: the owner is gone, so no SubagentStop can ever arrive, and an
// un-completed record would otherwise keep the session carrying pending task
// content forever. Re-checks OwnerExited on a fresh load so a session revived
// since the caller's snapshot is left alone; best-effort throughout.
func sweepCompleteLiveTaskRecords(ctx context.Context, store *session.StateStore, st *session.State) {
	fresh := st
	if store != nil {
		if reloaded, err := store.Load(ctx, st.SessionID); err == nil && reloaded != nil {
			fresh = reloaded
		}
	}
	if !fresh.OwnerExited() || len(fresh.LiveTaskRecords()) == 0 {
		return
	}
	ag, err := agent.GetByAgentType(fresh.AgentType)
	if err != nil || ag == nil {
		return
	}
	completeLiveTaskRecords(ctx, ag, fresh.SessionID, fresh.TranscriptPath)
}
