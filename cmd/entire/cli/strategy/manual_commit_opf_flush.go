// Detached OPF flush worker for the git-refs checkpoint backend.
//
// The OPF rewrite is a model call: on a real backlog it can run for tens of
// seconds, and until now it ran inline in the pre-push hook, where the user's
// `git push` waits on it. This file moves that work off the normal push path.
// Pre-push resolves the user's OPF decision, delivers only checkpoint
// generations that already carry the trailer, and hands the remaining rewrite
// work to a detached `entire __opf_flush` child.
//
// The worker REWRITES; it never pushes. That division is the whole reason this
// is safe to run unattended: rewriting only ever makes checkpoint content more
// redacted and stamps commits the user's push authorized it to stamp, while the
// decision about whether content may leave the machine stays with delivery on
// a user-initiated push. A background process must not be able to ship
// anything.
//
// Same detached one-shot shape as the zombie-session sweep and the
// trail-enablement refresh in package cli (maybeSpawnSessionSweep,
// spawnDetachedTrailEnablementRefresh): a cheap "is there work?" pre-check, a
// shared throttle marker, a spawn seam for tests, and a best-effort worker that
// logs every internal error rather than returning it.
package strategy

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/spawnmarker"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/entireio/cli/redact"
)

// opfFlushComponent names this worker in the logs, in both the parent hook that
// spawns it and the child that runs it.
const opfFlushComponent = "opf-flush"

// opfFlushSpawnThrottle bounds how often the pre-push path forks a detached
// flush child for a given repo. A ref that cannot be rewritten (over the
// per-ref inference cap, or a runtime that keeps failing) re-nominates a spawn
// on every push, so without this guard a user pushing repeatedly would fork one
// child per push, each re-opening the repo and re-failing identically. Five
// minutes comfortably covers a burst of pushes and a slow in-flight flush while
// still retrying a transient failure within the same working session.
const opfFlushSpawnThrottle = 5 * time.Minute

// opfFlushMaxPasses bounds the worker's retry loop. The loop already stops as
// soon as a pass makes no progress, so this only exists so that a pathological
// oscillation (a ref that alternates between needing and not needing OPF
// because something else is writing it concurrently) cannot spin a detached
// process nobody is watching.
const opfFlushMaxPasses = 10

// opfFlushSpawn is the process-spawn seam used by maybeSpawnOPFFlush. Swapped
// in tests so they can assert the spawn decision (including the throttle)
// without forking a real subprocess — a real `go test` binary doesn't
// understand `__opf_flush` as an argument, and execx.SpawnDetached is a no-op
// under testing anyway, which would make the decision unobservable. Production
// code always uses spawnDetachedOPFFlushProcess.
var opfFlushSpawn = spawnDetachedOPFFlushProcess

// spawnDetachedOPFFlushProcess starts `entire __opf_flush` as a detached child
// so the OPF model call can't add latency to the `git push` that spawned it.
// The child runs from the worktree root because the flush resolves the repo and
// the push queue from its working directory.
func spawnDetachedOPFFlushProcess(worktreeRoot string) {
	execx.SpawnDetached(worktreeRoot, "__opf_flush")
}

// opfFlushRecentlySpawned reports whether a detached flush was spawned for this
// repo within opfFlushSpawnThrottle and, when it wasn't, records now as the
// most recent spawn. Same flock-serialized marker mechanism the session sweep
// and trail refresh use, with its own marker.
func opfFlushRecentlySpawned(commonDir string, now time.Time) bool {
	return spawnmarker.RecentlySpawned(commonDir, "opf-flush-spawn", opfFlushSpawnThrottle, now)
}

// RefsAwaitingOPF returns the queued checkpoint refs whose tip does not yet
// carry the OPF trailer — exactly the refs a flush would still have work to do
// on. This is the flush's unit of progress: RewriteQueuedCheckpointRefsWithOPF
// never adds to or removes from the push queue (it peeks), so queue LENGTH
// cannot report whether a pass achieved anything, and "still needs OPF" can.
//
// One commit load per queued ref, and no ancestry walk: unappliedAncestry stops
// at the first trailered commit walking back from the tip, so a trailered tip
// is precisely an empty chain. Refs no longer present locally are not awaiting
// anything; flushCheckpointRefsQueue owns pruning them.
//
// Exported because `entire status` reports the same backlog to the user, and
// with the rewrite moved into a detached worker that report is the only place
// pending redaction work is visible. It reads local refs and the queue file and
// nothing else, so it is safe on a read-only status path.
func RefsAwaitingOPF(ctx context.Context, repo *git.Repository) ([]plumbing.ReferenceName, error) {
	queue, err := checkpoint.PushQueueForRepo(ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("resolve push queue: %w", err)
	}
	// Peek, not Drain: the queue belongs to the flush, and a mere progress
	// check must never be able to strand a ref by consuming it.
	queued, err := queue.Peek()
	if err != nil {
		return nil, fmt.Errorf("peek push queue: %w", err)
	}
	if len(queued) == 0 {
		return nil, nil
	}
	existing, _ := partitionLocalRefs(repo, queued)
	awaiting := make([]plumbing.ReferenceName, 0, len(existing))
	for _, refName := range existing {
		// An unknown trailer state (the ref vanished between the peek and here,
		// or its tip will not load) is not this worker's backlog: there is no
		// rewrite it could usefully attempt. Delivery reads the same state with
		// the opposite default — see refTipCarriesOPFApplied.
		applied, known := refTipCarriesOPFApplied(repo, refName)
		if known && !applied {
			awaiting = append(awaiting, refName)
		}
	}
	return awaiting, nil
}

// refTipCarriesOPFApplied reports whether refName's tip commit carries the
// Entire-OPF-Applied trailer, and whether that question could be answered at
// all: a ref that has vanished, or a tip commit that will not load, has no
// trailer state to report.
//
// Both callers need the second return and they choose opposite defaults for it,
// which is why it is not folded into the first. RefsAwaitingOPF treats an
// unanswerable ref as not its work; delivery treats it as not shippable, because
// "we could not read the trailer" must never ship as "the trailer is there".
// One shared reader so the two can never disagree about what carrying the
// trailer means.
func refTipCarriesOPFApplied(repo *git.Repository, refName plumbing.ReferenceName) (applied, known bool) {
	ref, err := repo.Reference(refName, true)
	if err != nil {
		return false, false
	}
	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return false, false
	}
	return trailers.HasOPFApplied(commit.Message), true
}

// maybeSpawnOPFFlush fires one detached __opf_flush child when OPF is enabled
// and queued checkpoint refs still need rewriting. Normal pre-push always
// assigns that backlog to the worker; the explicit migration path may first
// have attempted a synchronous rewrite. Both call this only when the decision
// was OPFRun — the user asked for OPF on this push. An explicit OPFSkip (or an
// unresolvable decision, mapped to
// OPFAbort) must not, since running OPF in the background after the user
// declined it for this push would redact content they chose to flush as-is
// and diverge the local ref from what was actually pushed. Not permanent: a
// later push whose decision resolves to OPFRun re-evaluates the same backlog
// from the queue and ref trailers, with no memory of an earlier skip.
//
// Best-effort throughout — a failure here must never fail the push. Everything
// it does is either a local read or a fork; nothing it can do changes what this
// push delivers.
//
// Spawns are throttled through a flock-serialized marker in the shared
// git-common-dir (opfFlushRecentlySpawned), so a burst of pushes across
// worktrees collapses to one child per opfFlushSpawnThrottle window.
func maybeSpawnOPFFlush(ctx context.Context, repo *git.Repository) {
	logCtx := logging.WithComponent(ctx, opfFlushComponent)
	if !redact.OPFEnabled() {
		return
	}
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		logging.Debug(logCtx, "skipping OPF flush spawn: could not resolve worktree root",
			slog.String("error", err.Error()))
		return
	}
	commonDir, err := gitdir.CommonDir(ctx)
	if err != nil {
		logging.Debug(logCtx, "skipping OPF flush spawn: could not resolve git common dir",
			slog.String("error", err.Error()))
		return
	}
	// Backlog discovery deliberately precedes the throttle check, for the same
	// reason the session sweep's does: the shared marker is check-and-record, so
	// consulting it first would burn the whole throttle window on the common
	// nothing-to-do case.
	awaiting, err := RefsAwaitingOPF(ctx, repo)
	if err != nil {
		logging.Warn(logCtx, "skipping OPF flush spawn: could not read push queue",
			slog.String("error", err.Error()))
		return
	}
	if len(awaiting) == 0 {
		return
	}
	if opfFlushRecentlySpawned(commonDir, time.Now()) {
		return
	}
	opfFlushSpawn(root)
	logging.Info(logCtx, "OPF work remains queued, spawned detached flush",
		slog.Int("refs", len(awaiting)))
}

// RunOPFFlush is the detached background pass that rewrites queued checkpoint
// refs assigned to it by normal pre-push, plus any leftovers from an explicit
// synchronous migration attempt. It retries until a pass makes
// no further progress — either every queued ref now carries the OPF trailer, or
// the refs that remain fail identically every time and nothing will change
// without new input. A ref that never clears simply stays in the backlog
// `entire status` reports (RefsAwaitingOPF); nothing here persists per-ref
// outcomes.
//
// Best-effort by construction: every internal error is logged and swallowed,
// never returned as a process failure. Nothing watches this child's exit code,
// and a non-zero exit would only produce a failure signal no one can act on.
//
// It deliberately does NOT push, and does not resolve an OPF decision. Delivery
// stays with the delivery path on the user's own push, where the decision was
// already made and where a withheld flush is reported to the user's terminal.
// It also does not touch the git-branch entire/checkpoints/v1 rewrite: that
// rewrite's unpushed set is computed against a specific push target
// (RewriteUnpushedV1WithOPF's remote tip lookup), and a detached child has no
// remote argument to resolve it from — guessing one would rewrite the wrong
// commit range.
func RunOPFFlush(ctx context.Context) error {
	logCtx := logging.WithComponent(ctx, opfFlushComponent)

	// OPFEnabled reads process-global config that only EnsureRedactionConfigured
	// sets. Without this the child would read "OPF off" and silently do nothing,
	// while opfDecisionForCheckpointRefs in the parent had resolved OPFRun.
	//
	// Unlike the hook path, a scanner-config error stops this worker rather than
	// being survived: the hook survives it so the user's own push is not blocked
	// by a settings problem, and there is no such cost here. Stamping commits
	// Entire-OPF-Applied using a scanner set the team's settings did not choose
	// is not a trade a background process gets to make on anyone's behalf.
	if err := EnsureRedactionConfigured(ctx); err != nil {
		logging.Warn(logCtx, "opf flush skipped: redaction could not be configured",
			slog.String("error", err.Error()))
		return nil
	}
	if !redact.OPFEnabled() {
		logging.Debug(logCtx, "opf flush skipped: OPF is not enabled")
		return nil
	}

	repo, err := OpenRepository(ctx)
	if err != nil {
		logging.Debug(logCtx, "opf flush skipped: could not open repo",
			slog.String("error", err.Error()))
		return nil
	}
	defer repo.Close()

	// Progress is measured against the previous pass's backlog: the loop exists
	// only to keep going while each pass is still clearing refs.
	before, err := RefsAwaitingOPF(ctx, repo)
	if err != nil {
		logging.Warn(logCtx, "opf flush: could not read push queue",
			slog.String("error", err.Error()))
		return nil
	}
	if len(before) == 0 {
		return nil
	}

	for range opfFlushMaxPasses {
		if rewriteErr := RewriteQueuedCheckpointRefsWithOPF(ctx, repo); rewriteErr != nil {
			logging.Warn(logCtx, "opf flush: git-refs pass failed",
				slog.String("error", rewriteErr.Error()))
		}
		after, afterErr := RefsAwaitingOPF(ctx, repo)
		if afterErr != nil {
			// Whether the pass achieved anything is now unknown, so there is
			// nothing to base another attempt on.
			logging.Warn(logCtx, "opf flush: could not re-read push queue",
				slog.String("error", afterErr.Error()))
			return nil
		}
		if len(after) == 0 || sameCheckpointRefSet(before, after) {
			// Fully rewritten, or this pass left the same refs pending: every
			// ref left fails on its own terms and a retry would fail identically.
			return nil
		}
		before = after
	}
	logging.Warn(logCtx, "opf flush: stopping after the maximum number of passes",
		slog.Int("passes", opfFlushMaxPasses))
	return nil
}

func sameCheckpointRefSet(a, b []plumbing.ReferenceName) bool {
	if len(a) != len(b) {
		return false
	}
	a = slices.Clone(a)
	b = slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}
