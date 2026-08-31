package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/internal/flock"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
)

// ErrShadowRefBusy is returned by casUpdateShadowBranchRef when the ref has
// moved since the caller read it. Callers retry with a fresh parent.
var ErrShadowRefBusy = errors.New("shadow branch ref moved (CAS mismatch)")

// ErrShadowBranchMoved means the caller's base no longer names the current
// shadow branch and migration must be retried before publishing.
var ErrShadowBranchMoved = errors.New("shadow branch base changed during write")

// shadowRefMaxRetries bounds the WriteTemporary retry loop. With the
// per-shadow-branch flock held, our own writers never collide; this budget
// is purely a safety net against an external `git update-ref` writer that
// repeatedly beats us to the ref.
const shadowRefMaxRetries = 16

// shadowRefMaxJitter is the upper bound for randomized backoff between CAS
// retries. Random jitter avoids thundering-herd retry patterns when many
// sessions hit the same shadow branch simultaneously.
const shadowRefMaxJitter = 8 * time.Millisecond

// repoCommonDir returns the git common dir used for shadow-ref locks and loose
// object cleanup without depending on the process working directory.
func (s *ephemeralStore) repoCommonDir(ctx context.Context) (string, error) {
	commonDir, err := resolveGitCommonDir(ctx, s.repo)
	if err != nil {
		return "", err
	}
	return commonDir, nil
}

// casUpdateShadowBranchRef atomically updates a shadow branch ref via
// `git update-ref <ref> <new> <old>`. Pass plumbing.ZeroHash as expectedHash
// to require the ref to NOT exist (first-checkpoint case).
//
// Returns ErrShadowRefBusy when git reports the ref moved since expectedHash
// was observed; callers retry with a fresh parent. Any other failure is
// returned wrapped.
//
// Why shell out: git's ref-locking is the canonical cross-process atomic
// CAS — go-git's CheckAndSetReference doesn't interoperate with native git's
// .lock files, and shadow branches can be touched concurrently by separate
// `entire` hook processes.
func casUpdateShadowBranchRef(ctx context.Context, repo *git.Repository, branchName string, newHash, expectedHash plumbing.Hash) error {
	err := CompareAndSwapRef(ctx, repo, plumbing.NewBranchReferenceName(branchName), newHash, expectedHash)
	if errors.Is(err, ErrRefConflict) {
		return ErrShadowRefBusy
	}
	return err
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
func shadowBranchLockPath(commonDir, branchName string) (string, error) {
	lockDir := filepath.Join(commonDir, "entire-shadow-locks")
	if err := os.MkdirAll(lockDir, 0o750); err != nil {
		return "", fmt.Errorf("create shadow lock directory: %w", err)
	}
	safe := strings.ReplaceAll(branchName, "/", "_")
	return filepath.Join(lockDir, safe+".lock"), nil
}

// withShadowBranchFlock acquires the per-shadow-branch flock, runs fn, and
// releases the flock. Serializes all WriteTemporary callers that target the
// same shadow branch — across goroutines AND across processes — so the CAS
// in casUpdateShadowBranchRef only sees external writers as contention.
//
// commonDir is the git common directory (from s.repoDirs); it locates the
// lock file independently of the process cwd.
func withShadowBranchFlock(commonDir, branchName string, fn func() error) error {
	return withShadowBranchFlocks(commonDir, []string{branchName}, fn)
}

func withShadowBranchFlocks(commonDir string, branchNames []string, fn func() error) error {
	ordered := append([]string(nil), branchNames...)
	sort.Strings(ordered)
	unique := ordered[:0]
	for _, branchName := range ordered {
		if len(unique) == 0 || unique[len(unique)-1] != branchName {
			unique = append(unique, branchName)
		}
	}

	releases := make([]func(), 0, len(unique))
	for _, branchName := range unique {
		path, err := shadowBranchLockPath(commonDir, branchName)
		if err != nil {
			for i := len(releases) - 1; i >= 0; i-- {
				releases[i]()
			}
			return err
		}
		release, err := flock.Acquire(path)
		if err != nil {
			for i := len(releases) - 1; i >= 0; i-- {
				releases[i]()
			}
			return fmt.Errorf("acquire shadow flock %s: %w", branchName, err)
		}
		releases = append(releases, release)
	}
	defer func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}()
	return fn()
}

// MoveShadowBranch locks both shadow branches before observing and moving the
// source ref. A missing source is an idempotent no-op.
func MoveShadowBranch(
	ctx context.Context,
	repo *git.Repository,
	sourceBranch, destinationBranch string,
) (bool, error) {
	commonDir, err := resolveGitCommonDir(ctx, repo)
	if err != nil {
		return false, err
	}
	moved := false
	err = withShadowBranchFlocks(commonDir, []string{sourceBranch, destinationBranch}, func() error {
		sourceRef := plumbing.NewBranchReferenceName(sourceBranch)
		expectedSource, err := ReadRefHash(repo, sourceRef)
		if err != nil {
			return err
		}
		if expectedSource.IsZero() {
			return nil
		}
		if err := MoveRefIfUnchanged(ctx, repo,
			sourceRef,
			plumbing.NewBranchReferenceName(destinationBranch),
			expectedSource); err != nil {
			return err
		}
		moved = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return moved, nil
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
	_ = os.Remove(filepath.Join(commonDir, "objects", h[:2], h[2:]))
}
