package cli

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// endedSessionSweepAge is how long an ENDED-but-uncondensed session must sit
// before the background sweep may condense it. Condensing early would forfeit
// PostCommit's per-commit carry-forward linkage for sessions whose user is
// still about to commit — CondenseAndMarkFullyCondensed deliberately skips
// FilesTouched sessions for exactly that reason. 24h matches
// activeSessionInteractionThreshold: after a day with no commit, the linkage
// is clearly not coming and the session is just O(N) drag on every commit.
const endedSessionSweepAge = 24 * time.Hour

// isSweepableZombie reports whether the state marks this session as safe for
// the background sweep to fix. It deliberately needs no repository access —
// the priciest thing it does is OwnerExited()'s per-non-ended-state process
// probe, which the `entire status` path already pays per state — so the
// session-start hook caller stays cheap. The sweep itself re-validates
// before acting (see runSessionSweep's safety notes).
//
// Condense-only contract: ENDED sessions whose steps turn out to have no
// shadow branch are doctor's discard case, filtered later — this
// predicate only ever nominates sessions, it never acts.
func isSweepableZombie(st *session.State, now time.Time) bool {
	// Imported sessions are historical records: complete by design, never
	// condensable (no BaseCommit), and exempt from the stale purge — so they
	// may look like zombies forever. Nominating one would spawn a doomed sweep
	// on every session start, forever.
	if st.Kind.IsImported() {
		return false
	}
	if !st.IsEnded() {
		return st.OwnerExited()
	}
	if st.Phase != session.PhaseEnded || st.FullyCondensed ||
		(st.StepCount <= 0 && !st.HasTaskContent()) {
		return false
	}
	ref := st.EndedAt
	if ref == nil {
		ref = st.LastInteractionTime
	}
	if ref == nil {
		return false // legacy state without timestamps: leave it to doctor
	}
	return now.Sub(*ref) >= endedSessionSweepAge
}

// runSessionSweep is the detached background pass that fixes zombie sessions:
// Non-ended sessions whose owning agent process is gone are finalized exactly
// as a clean SessionStop would (finalizeExitedSessions, which re-validates
// OwnerExited under the per-session lock), and ENDED sessions past the
// carry-forward window with condensable data are condensed via the same
// engine `entire doctor --force` uses.
//
// Safety contract: condense-only, enforced by OUR pre-checks, not by the
// engine.
// CondenseSessionByID's locked closure re-checks only shadow-branch
// existence; we therefore re-load each candidate and re-run the full zombie
// predicate immediately before condensing. That narrows — does not close —
// the window in which a resumed (ENDED→ACTIVE) session gets condensed
// anyway: acceptable, because the precondition is >24h idle (a seconds-wide
// race against a day-old zombie) and a condense of a just-resumed session is
// coherent. The cost of losing that race: the resumed turn's phase is reset to
// IDLE and its pending prompt attribution is lost — recoverable, and
// acceptable at a seconds-vs-24h race. If the shadow branch vanishes between
// our check and the engine's lock, the engine clears the state — correct
// cleanup, since in practice that typically happens when a concurrent condense
// already succeeded; an out-of-band branch deletion (git branch -D,
// entire clean) in the window also lands here. The reverse
// direction is also safe: an ACTIVE session with a dead owner that is being
// resumed right now can be finalized by the sweep mid-resume, because
// finalizeExitedSessions re-validates under the per-session lock and the
// resume's first TurnStart re-establishes state — owner capture happens at
// TurnStart, not SessionStart. Everything is best-effort: a failed session is
// logged and retried by the next sweep.
func runSessionSweep(ctx context.Context) error {
	logCtx := logging.WithComponent(ctx, "session-sweep")

	states, err := strategy.ListSessionStates(ctx)
	if err != nil {
		return fmt.Errorf("failed to list session states: %w", err)
	}

	// Non-ended sessions with a dead owner: finalize + eager condense. This
	// command is detached, so a zero deadline lets the backlog drain in one pass
	// instead of inheriting the interactive status/doctor budget. This also
	// refreshes the matched entries in `states` from disk.
	if n := finalizeExitedSessions(ctx, states, time.Time{}); n > 0 {
		logging.Info(logCtx, "sweep finalized exited sessions", slog.Int("count", n))
	}

	repo, err := openRepository(ctx)
	if err != nil {
		return err
	}
	defer repo.Close()

	store, err := session.NewStateStore(ctx)
	if err != nil {
		return fmt.Errorf("failed to open session state store: %w", err)
	}

	now := time.Now()
	for _, st := range states {
		// ENDED-only here: non-ended zombies were finalizeExitedSessions' job
		// above. isSweepableZombie returns true for a non-ended dead-owner
		// session (its finalize failed above), so this phase check is what
		// keeps those out of the condense path — and skips their pointless
		// re-load.
		if st.Phase != session.PhaseEnded || !isSweepableZombie(st, now) {
			continue
		}
		// Re-load and re-check right before acting: the snapshot may be stale
		// (a resumed session flips ENDED→ACTIVE; a concurrent sweep may have
		// condensed it). See the safety contract above for the residual race.
		fresh, lerr := store.Load(ctx, st.SessionID)
		if lerr != nil {
			logging.Debug(logCtx, "sweep re-load failed",
				slog.String("session_id", st.SessionID),
				slog.String("error", lerr.Error()))
			continue
		}
		if fresh == nil {
			continue // benign: state removed/cleaned up between list and load
		}
		if fresh.Phase != session.PhaseEnded || !isSweepableZombie(fresh, now) {
			continue
		}
		if !strategy.IsCondensableEndedSession(repo, fresh) {
			// Uncondensed steps but no shadow branch: fixing this means
			// discarding state, which the sweep never initiates — it is
			// doctor's discard case.
			logging.Info(logCtx, "sweep skipping non-condensable ended session: uncondensed steps but no shadow branch — run `entire doctor` to resolve",
				slog.String("session_id", fresh.SessionID))
			continue
		}
		if condErr := GetStrategy(ctx).CondenseSessionByID(ctx, fresh.SessionID); condErr != nil {
			logging.Warn(logCtx, "sweep condense failed",
				slog.String("session_id", fresh.SessionID),
				slog.String("error", condErr.Error()))
			continue
		}
		logging.Info(logCtx, "sweep condensed ended zombie session",
			slog.String("session_id", fresh.SessionID))
	}
	return nil
}

// countSweepableZombies counts the sessions in states that nominate a
// background sweep. Pure so the spawn decision is unit-testable.
func countSweepableZombies(states []*session.State, now time.Time) int {
	n := 0
	for _, st := range states {
		if isSweepableZombie(st, now) {
			n++
		}
	}
	return n
}

// sweepSpawn is the process-spawn seam used by maybeSpawnSessionSweep.
// Swapped in tests so they can assert the spawn decision (including the
// throttle) without forking a real subprocess (a real `go test` binary doesn't
// understand `__sweep_sessions` as an argument). Production code always uses
// spawnDetachedSessionSweepProcess.
var sweepSpawn = spawnDetachedSessionSweepProcess

// sweepGitCommonDir is the repository-scope seam used by
// maybeSpawnSessionSweep. Swapped in tests to prove a throttle-key failure
// fails closed without depending on a malformed repository fixture.
var sweepGitCommonDir = session.GetGitCommonDir

// spawnDetachedSessionSweepProcess starts `entire __sweep_sessions` as a
// detached child so the sweep's repo work can't add latency to the
// session-start hook that spawned it. The child runs from the worktree root
// because the sweep resolves the repo and session-state directory from its
// working directory.
func spawnDetachedSessionSweepProcess(worktreeRoot string) {
	execx.SpawnDetached(worktreeRoot, "__sweep_sessions")
}

// sessionSweepSpawnThrottle bounds how often the session-start hook forks a
// detached sweep child for a given repo. A zombie that persistently fails to
// condense re-nominates a spawn on every session start; without this guard a
// burst of hooks (or an agent that restarts sessions rapidly) would fork one
// child per hook, each re-opening the repo just to fail the same way. 15
// minutes comfortably covers a burst and a slow in-flight sweep while still
// retrying a transient failure within the same working session.
const sessionSweepSpawnThrottle = 15 * time.Minute

// sweepRecentlySpawned reports whether a detached sweep was spawned for this
// repo within sessionSweepSpawnThrottle and, when it wasn't, records now as
// the most recent spawn. Same flock-serialized marker mechanism as the
// trail-enablement refresh (see recentlySpawnedMarker), with its own marker.
func sweepRecentlySpawned(commonDir string, now time.Time) bool {
	return recentlySpawnedMarker(commonDir, "session-sweep-spawn", sessionSweepSpawnThrottle, now)
}

// maybeSpawnSessionSweep fires one detached __sweep_sessions child when the
// session-state files nominate a zombie. Called from the session-start hook:
// listing the state files is a small shared-directory read, and the sweep
// itself runs detached, so the hook's latency budget is untouched. Best-effort
// throughout — a failure here must never fail the hook.
//
// Spawns are throttled through a flock-serialized marker in the shared
// git-common-dir (sweepRecentlySpawned), so a burst of concurrent or rapid
// session starts collapses to one child per sessionSweepSpawnThrottle window.
// Redundant sweeps re-check under the per-session lock and no-op once one has
// succeeded; a zombie that persistently fails to condense is retried — and
// fails — once per throttle window, which we accept.
func maybeSpawnSessionSweep(ctx context.Context) {
	logCtx := logging.WithComponent(ctx, "session-sweep")
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		logging.Debug(logCtx, "skipping sweep spawn: could not resolve worktree root",
			slog.String("error", err.Error()))
		return
	}
	commonDir, err := sweepGitCommonDir(ctx)
	if err != nil {
		logging.Debug(logCtx, "skipping sweep spawn: could not resolve git common dir",
			slog.String("error", err.Error()))
		return
	}
	// Zombie discovery deliberately precedes the throttle check. The shared
	// marker is check-and-record, so consulting it before discovery would burn
	// the whole throttle window on the overwhelmingly common no-zombie case. A
	// read-only marker check would permit reordering if this scan becomes a
	// measurable hook-latency cost.
	states, err := strategy.ListSessionStates(ctx)
	if err != nil {
		logging.Warn(logCtx, "skipping sweep check", slog.String("error", err.Error()))
		return
	}
	n := countSweepableZombies(states, time.Now())
	if n == 0 {
		return
	}
	if sweepRecentlySpawned(commonDir, time.Now()) {
		return
	}
	sweepSpawn(root)
	logging.Info(logCtx, "zombie sessions detected, spawned detached sweep",
		slog.Int("count", n))
}
