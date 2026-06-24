package cli

import (
	"context"
	"log/slog"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/session"
)

// finalizeExitedSessions finalizes every ACTIVE session in states whose owning
// agent process has exited (clean /exit, crash, kill, terminal close, reboot)
// without a SessionStop hook firing. Each such session is finalized exactly as a
// clean session stop would be: the session-stop transition runs (PhaseEnded +
// EndedAt) and pending work is eagerly condensed.
//
// It refreshes the matched in-memory states from disk after finalizing — so
// callers can re-filter/re-render without their own reload — and returns the
// number finalized. Each session is best-effort: a failure to mark one ended is
// logged and skipped; a condense failure is logged but the session is still
// counted (PostCommit will retry the condense later).
func finalizeExitedSessions(ctx context.Context, states []*session.State) int {
	logCtx := logging.WithComponent(ctx, "session")

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
		})
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
		// condensed only if the eager condense actually succeeded (it is
		// fail-open, so StepCount/FullyCondensed must not be assumed). Fall back
		// to a minimal ended-marking if the reload fails — enough for the
		// caller's "active" filter to drop it.
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
			now := time.Now()
			st.Phase = session.PhaseEnded
			st.EndedAt = &now
		}
		finalized++
	}
	return finalized
}
