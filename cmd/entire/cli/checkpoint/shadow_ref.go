package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/osroot"

	"github.com/go-git/go-git/v6/plumbing"
)

// ErrShadowRefBusy is returned by casUpdateShadowBranchRef when the ref has
// moved since the caller read it. Callers retry with a fresh parent.
var ErrShadowRefBusy = errors.New("shadow branch ref moved (CAS mismatch)")

// shadowRefMaxRetries bounds the WriteTemporary retry loop. With the
// per-shadow-branch flock held, our own writers never collide; this budget
// is purely a safety net against an external `git update-ref` writer that
// repeatedly beats us to the ref.
const shadowRefMaxRetries = 16

// shadowRefMaxJitter is the upper bound for randomized backoff between CAS
// retries. Random jitter avoids thundering-herd retry patterns when many
// sessions hit the same shadow branch simultaneously.
const shadowRefMaxJitter = 8 * time.Millisecond

// repoDirs returns the worktree root and git common dir for the store's
// repository. Callers use the worktree root as cmd.Dir for git invocations
// and the common dir to locate filesystem paths (lock files, loose objects)
// — both without depending on the process cwd.
func (s *ephemeralStore) repoDirs(ctx context.Context) (worktreeRoot, commonDir string, err error) {
	return repositoryDirs(ctx, s.repo)
}

// casUpdateShadowBranchRef atomically updates a shadow branch ref via
// `git update-ref <ref> <new> <old>`. Pass plumbing.ZeroHash as expectedHash
// to require the ref to NOT exist (first-checkpoint case).
//
// repoRoot is used as cmd.Dir so the update targets the same repository as
// the rest of WriteTemporary (i.e. s.repo) regardless of the process cwd.
//
// Returns ErrShadowRefBusy when git reports the ref moved since expectedHash
// was observed; callers retry with a fresh parent. Any other failure is
// returned wrapped.
//
// Why shell out: git's ref-locking is the canonical cross-process atomic
// CAS — go-git's CheckAndSetReference doesn't interoperate with native git's
// .lock files, and shadow branches can be touched concurrently by separate
// `entire` hook processes.
func casUpdateShadowBranchRef(ctx context.Context, repoRoot, branchName string, newHash, expectedHash plumbing.Hash) error {
	return casUpdateRef(ctx, repoRoot, plumbing.NewBranchReferenceName(branchName), newHash, expectedHash)
}

// shadowRefBackoff sleeps for a small random jitter before the next CAS
// retry. After several retries the upper bound doubles to slow the
// thundering herd further. Respects context cancellation.
func shadowRefBackoff(ctx context.Context, attempt int) error {
	base := shadowRefMaxJitter
	if attempt > 4 {
		base *= 2
	}
	// Add a 1ms floor so the chosen sleep is always non-trivial, even when
	// rand.Int64N happens to return 0.
	d := time.Duration(rand.Int64N(int64(base))) + time.Millisecond //nolint:gosec // jitter, not security-sensitive
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err() //nolint:wrapcheck // canonical context cancellation
	}
}

// shadowBranchLockPath returns the per-shadow-branch flock file path. Lock
// files live in <git-common-dir>/entire-shadow-locks/ so they don't pollute
// the session-state directory. Branch names are slash-escaped because the
// shadow-branch convention "entire/<hash>" would otherwise nest directories.
func shadowBranchLock(commonDir, branchName string) (*os.Root, string, error) {
	root, err := gitdir.OpenAt(commonDir)
	if err != nil {
		return nil, "", fmt.Errorf("open git common dir: %w", err)
	}
	if err := osroot.MkdirAllNoSymlink(root, shadowLockDirName, 0o750); err != nil {
		return nil, "", fmt.Errorf("create shadow lock directory: %w", err)
	}
	safe := strings.ReplaceAll(branchName, "/", "_")
	return root, shadowLockDirName + "/" + safe + ".lock", nil
}

// shadowLockDirName is the shadow-branch lock directory inside the git common dir.
const shadowLockDirName = "entire-shadow-locks"

// withShadowBranchFlock acquires the per-shadow-branch flock, runs fn, and
// releases the flock. Serializes all WriteTemporary callers that target the
// same shadow branch — across goroutines AND across processes — so the CAS
// in casUpdateShadowBranchRef only sees external writers as contention.
//
// commonDir is the git common directory (from s.repoDirs); it locates the
// lock file independently of the process cwd.
func withShadowBranchFlock(commonDir, branchName string, fn func() error) error {
	root, name, err := shadowBranchLock(commonDir, branchName)
	if err != nil {
		return err
	}
	release, err := flock.AcquireIn(root, name)
	if err != nil {
		return fmt.Errorf("acquire shadow flock %s: %w", branchName, err)
	}
	defer release()
	return fn()
}

// readRefHash resolves refName's current commit via a native git subprocess.
// Returns (hash, false, nil) when the ref doesn't exist -- not an error, since
// "old shadow branch is already gone" is a normal, expected outcome for
// MigrateShadowBranchRef's caller.
func readRefHash(ctx context.Context, repoRoot string, refName plumbing.ReferenceName) (plumbing.Hash, bool, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--verify", refName.String())
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return plumbing.ZeroHash, false, nil
		}
		return plumbing.ZeroHash, false, fmt.Errorf("git rev-parse --verify %s: %w", refName, err)
	}
	return plumbing.NewHash(strings.TrimSpace(string(out))), true, nil
}

// MigrateShadowBranchRef atomically renames a shadow branch from oldBranch to
// newBranch (both refs/heads branch names, not full ref names), used when a
// session's shadow branch needs to move to a new base-commit-derived name
// (see migrateShadowBranchToBaseCommit in the strategy package).
//
// This shares the exact locking/CAS discipline writeCheckpoint/writeTask use
// (withShadowBranchFlock + casUpdateShadowBranchRef) rather than the
// unlocked go-git Reference/SetReference/CLI-delete sequence this replaced:
// two sessions can legitimately share one worktree's shadow branch (a
// documented, supported configuration -- main agent + Task-tool subagent is
// the common case), and a checkpoint write racing an unlocked migration
// could silently orphan or overwrite committed checkpoint data. Both branch
// names are locked (in a stable, name-sorted order, so two migrations that
// happen to touch the same pair of branches from opposite directions can't
// deadlock) before either ref is touched.
//
// Returns (migrated, err). migrated is false with a nil error when there was
// nothing to do: the old branch no longer exists (first checkpoint after
// HEAD changed, or a concurrent migration already moved it), or the new
// branch already exists pointing at different content than the old one (a
// concurrent writer got there first -- left alone rather than clobbered).
func MigrateShadowBranchRef(ctx context.Context, repoRoot, commonDir, oldBranch, newBranch string) (bool, error) {
	first, second := oldBranch, newBranch
	if second < first {
		first, second = second, first
	}
	var migrated bool
	err := withShadowBranchFlock(commonDir, first, func() error {
		return withShadowBranchFlock(commonDir, second, func() error {
			m, innerErr := migrateShadowBranchRefLocked(ctx, repoRoot, oldBranch, newBranch)
			migrated = m
			return innerErr
		})
	})
	return migrated, err
}

// migrateShadowBranchRefLocked performs the actual rename. Callers must hold
// both branches' flocks (see MigrateShadowBranchRef).
func migrateShadowBranchRefLocked(ctx context.Context, repoRoot, oldBranch, newBranch string) (bool, error) {
	oldRefName := plumbing.NewBranchReferenceName(oldBranch)
	newRefName := plumbing.NewBranchReferenceName(newBranch)

	oldHash, exists, err := readRefHash(ctx, repoRoot, oldRefName)
	if err != nil {
		return false, fmt.Errorf("read old shadow branch %s: %w", oldBranch, err)
	}
	if !exists {
		return false, nil
	}

	newHash, newExists, err := readRefHash(ctx, repoRoot, newRefName)
	if err != nil {
		return false, fmt.Errorf("read new shadow branch %s: %w", newBranch, err)
	}
	if newExists {
		if newHash != oldHash {
			// A concurrent writer already created the destination with
			// different content; leave both alone rather than guess which
			// should win.
			return false, nil
		}
		// Destination already carries the old branch's content (a concurrent
		// migration finished the create half already) -- finish the cleanup
		// if the old ref is still exactly what we just read.
		if delErr := casDeleteRef(ctx, repoRoot, oldRefName, oldHash); delErr != nil && !errors.Is(delErr, ErrShadowRefBusy) {
			return true, fmt.Errorf("delete already-migrated old shadow branch %s: %w", oldBranch, delErr)
		}
		return true, nil
	}

	if err := casUpdateShadowBranchRef(ctx, repoRoot, newBranch, oldHash, plumbing.ZeroHash); err != nil {
		if errors.Is(err, ErrShadowRefBusy) {
			// Someone else created newBranch between our read above and now.
			return false, nil
		}
		return false, fmt.Errorf("create new shadow branch %s: %w", newBranch, err)
	}

	if err := casDeleteRef(ctx, repoRoot, oldRefName, oldHash); err != nil {
		if errors.Is(err, ErrShadowRefBusy) {
			// oldBranch moved since we read it -- a concurrent legitimate
			// writer (e.g. writeCheckpoint) advanced it after our read but
			// before our delete. newBranch already carries the content we
			// migrated; leave oldBranch for its own writer to retry its CAS
			// against (it will see the ref moved and follow its normal
			// retry path -- see casUpdateShadowBranchRef's callers).
			logging.Warn(logging.WithComponent(ctx, "checkpoint"),
				"shadow branch migration: old ref moved before delete, leaving in place",
				slog.String("old_branch", oldBranch), slog.String("new_branch", newBranch))
			return true, nil
		}
		return false, fmt.Errorf("delete old shadow branch %s: %w", oldBranch, err)
	}

	return true, nil
}

// tryDeleteLooseObject best-effort removes a loose object file. Used to
// clean up dangling commits created during a CAS-losing attempt. Failures
// (e.g. object already packed by a concurrent gc, or never written as a
// loose object) are ignored — the object will be picked up by the next gc
// pass either way.
func tryDeleteLooseObject(commonDir string, hash plumbing.Hash) {
	h := hash.String()
	if len(h) < 3 {
		return
	}
	// Through the common dir's root like everything else in this file: a delete
	// is the operation least worth leaving on a joined path, and the shadow
	// flock beside it already resolves its name inside this same root. The hash
	// is hex, so nothing here can traverse today; the root is what keeps that
	// true without depending on it.
	root, err := gitdir.OpenAt(commonDir)
	if err != nil {
		return
	}
	_ = osroot.RemoveNoSymlinks(root, "objects/"+h[:2]+"/"+h[2:]) //nolint:errcheck // best-effort; see doc comment
}
