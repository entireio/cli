package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/go-git/go-git/v6"
)

// Status is the single entry point for reading go-git worktree status; the
// forbidigo rule in .golangci.yaml enforces this and names this signature, so
// the context parameter is part of that contract and is where cancellation or a
// perf span would attach.
//
// Worktree.Status() walks the worktree, so its cost scales with working-set size
// rather than with the size of the change being inspected — it is the most
// expensive git read on the hook paths. Avoid calling it more than once per hook.
func Status(_ context.Context, repo *git.Repository) (git.Status, error) {
	worktree, err := repo.Worktree()
	if err != nil {
		return nil, err //nolint:wrapcheck // callers add their own context
	}

	status, err := worktree.Status() //nolint:forbidigo // the sanctioned call site
	if err != nil {
		return nil, err //nolint:wrapcheck // callers add their own context
	}
	filterNestedCheckouts(worktree.Filesystem().Root(), status)

	return status, nil
}

// StatusWalkBudget bounds the wall-clock time of one worktree status read on
// the agent-hook capture paths — the go-git walk in StatusWithBudget and the
// first-checkpoint `git status` subprocess in the checkpoint store. Agents
// time out their hooks at roughly 60s (Claude Code's default), but a
// timed-out hook PROCESS is not killed — an unbounded walk over a
// pathological worktree (e.g. a stray `git init` in $HOME with no .gitignore)
// has been observed grinding for hours at gigabytes of RSS after the agent
// gave up. 20s is chosen as far beyond any healthy repository (a warm walk
// over a large working set finishes in seconds) while leaving the remaining
// ~40s of the agent's timeout for the rest of the capture path — transcript
// copy, tree building, state writes — so the hook still degrades gracefully
// and exits instead of being orphaned.
const StatusWalkBudget = 20 * time.Second

// ErrStatusBudgetExceeded reports that a worktree status walk was abandoned
// because it exceeded StatusWalkBudget. Capture is fail-open: hook-path
// callers must treat this like any other status failure — warn and continue
// with transcript-derived data — never fail the hook.
var ErrStatusBudgetExceeded = errors.New("worktree status walk exceeded time budget")

// statusBudgetBreached makes every later StatusWithBudget call in this process
// fail fast once one walk has breached the budget. All walks cover the same
// worktree, so a second walk would burn the same wall-clock again and keep the
// hook process alive past the agent's own hook timeout — exactly the zombie
// process the budget exists to prevent. Hook processes are one-shot, so the
// latch's lifetime is a single hook invocation.
var statusBudgetBreached atomic.Bool

// SetStatusBudgetBreachedForTesting overrides the process-local breach latch
// so tests can exercise budget-breach degradation without a slow walk.
func SetStatusBudgetBreachedForTesting(breached bool) {
	statusBudgetBreached.Store(breached)
}

// StatusWithBudget is Status bounded by StatusWalkBudget, for agent-hook
// capture paths. go-git's Worktree.Status is not context-cancellable, so on
// breach the walk goroutine is abandoned — it dies with the short-lived hook
// process, which is the point — and the returned error wraps
// ErrStatusBudgetExceeded so callers' warn-and-continue degrade paths apply.
// Paths where a user is actively waiting on a command (review via
// review_target.go, and `session adopt` via detectFileChangesUnbounded) keep
// calling Status directly.
func StatusWithBudget(ctx context.Context, repo *git.Repository) (git.Status, error) {
	worktree, err := repo.Worktree()
	if err != nil {
		return nil, err //nolint:wrapcheck // callers add their own context
	}
	return statusWithBudget(ctx, worktree.Filesystem().Root(), StatusWalkBudget, func() (git.Status, error) {
		return Status(ctx, repo)
	})
}

// StatusWithIsolatedBudget is StatusWithBudget for a walk over a DIFFERENT
// worktree than the one the hook is capturing (cross-repo session adoption
// reads the target repo's status). It honors an earlier breach — the process
// is already past its budget — but a breach or cancellation of THIS walk does
// not arm the process-wide latch: the latch exists because every walk covers
// the same worktree, and a slow foreign repo must not put the launching repo's
// own capture into degraded mode. The caller supplies the budget, which should
// be small: the walk may run under a lock other hooks in the target wait on.
func StatusWithIsolatedBudget(ctx context.Context, repo *git.Repository, budget time.Duration) (git.Status, error) {
	worktree, err := repo.Worktree()
	if err != nil {
		return nil, err //nolint:wrapcheck // callers add their own context
	}
	return statusWithBudgetLatch(ctx, worktree.Filesystem().Root(), budget, false, func() (git.Status, error) {
		return Status(ctx, repo)
	})
}

// statusWithBudget is the testable core of StatusWithBudget: root and budget
// are parameters and walk stands in for the real status call.
func statusWithBudget(ctx context.Context, root string, budget time.Duration, walk func() (git.Status, error)) (git.Status, error) {
	return statusWithBudgetLatch(ctx, root, budget, true, walk)
}

// statusWithBudgetLatch is statusWithBudget with the latch arming explicit:
// armLatch=false is the isolated (foreign-worktree) variant.
func statusWithBudgetLatch(ctx context.Context, root string, budget time.Duration, armLatch bool, walk func() (git.Status, error)) (git.Status, error) {
	if statusBudgetBreached.Load() {
		return nil, fmt.Errorf("status walk of %s skipped: an earlier walk in this process breached its budget: %w",
			root, ErrStatusBudgetExceeded)
	}

	type walkResult struct {
		status git.Status
		err    error
	}
	resultCh := make(chan walkResult, 1)
	start := time.Now()
	go func() {
		// An abandoned walk can outlive its caller, which may close the
		// repository under it. The result is discarded after a breach, so a
		// panic in the orphaned goroutine must not crash the hook process.
		defer func() {
			if r := recover(); r != nil {
				resultCh <- walkResult{err: fmt.Errorf("status walk panicked: %v", r)}
			}
		}()
		status, err := walk()
		resultCh <- walkResult{status: status, err: err}
	}()

	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case res := <-resultCh:
		return res.status, res.err
	case <-ctx.Done():
		// Cancellation abandons the walk exactly like a timer breach, so it
		// must arm the latch and carry the sentinel too: otherwise a caller's
		// errors.Is degrade check misses, the hook fails hard, and a later
		// call in this process could re-enter the abandoned walk. ctx.Err()
		// stays in the chain so cancellation remains distinguishable.
		if armLatch {
			statusBudgetBreached.Store(true)
		}
		return nil, fmt.Errorf("worktree status walk of %s abandoned on cancellation: %w: %w",
			root, ctx.Err(), ErrStatusBudgetExceeded)
	case <-timer.C:
		if armLatch {
			statusBudgetBreached.Store(true)
		}
		elapsed := time.Since(start).Round(time.Millisecond)
		logging.Warn(logging.WithComponent(ctx, "gitrepo"),
			"worktree status walk exceeded budget; capture degraded: untracked/new file detection skipped this turn",
			slog.String("repo_root", root),
			slog.Duration("elapsed", elapsed),
			slog.Duration("budget", budget))
		return nil, fmt.Errorf("worktree status walk of %s abandoned after %s (budget %s): %w",
			root, elapsed, budget, ErrStatusBudgetExceeded)
	}
}

// filterNestedCheckouts removes untracked entries that live inside a nested
// git checkout: a directory under the worktree root containing a .git entry
// (a directory for full clones, a file for linked worktrees). git treats such
// a directory as a repository boundary and never descends into it; go-git's
// status walk has no such check, so untracked files from unrelated checkouts
// (agent worktrees, vendored clones) would otherwise be reported as if they
// were this repository's files. Tracked entries are kept regardless of
// location, matching git, which always reports index entries.
//
// Deliberately quieter than git in one respect: git reports the boundary
// itself as a single untracked entry ("?? vendor/"), while this filter drops
// it entirely. Consumers treat every status entry as a file to record, so a
// synthetic directory entry would be junk of the same kind this filter
// removes. Do not "fix" the missing boundary entry.
func filterNestedCheckouts(root string, status git.Status) {
	containsGit := make(map[string]bool)
	for relPath, fileStatus := range status {
		if fileStatus.Worktree != git.Untracked {
			continue
		}
		if insideNestedCheckout(root, relPath, containsGit) {
			delete(status, relPath)
		}
	}
}

// insideNestedCheckout reports whether any ancestor directory of relPath
// (a forward-slash path relative to root, as go-git status keys are) contains
// a .git entry. The walk stops before the root itself, so the host repo's own
// .git never counts. Lstat rather than Stat so a worktree's .git file counts
// without following it. containsGit memoizes per-directory answers across one
// status result, where thousands of paths can share a few ancestor directories.
func insideNestedCheckout(root, relPath string, containsGit map[string]bool) bool {
	for dir := path.Dir(relPath); dir != "." && dir != "/"; dir = path.Dir(dir) {
		nested, seen := containsGit[dir]
		if !seen {
			_, err := os.Lstat(filepath.Join(root, filepath.FromSlash(dir), ".git"))
			nested = err == nil
			containsGit[dir] = nested
		}
		if nested {
			return true
		}
	}
	return false
}
