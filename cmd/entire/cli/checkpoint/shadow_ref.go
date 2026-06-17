package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/internal/flock"

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
func (s *GitStore) repoDirs(ctx context.Context) (worktreeRoot, commonDir string, err error) {
	wt, err := s.repo.Worktree()
	if err != nil {
		return "", "", fmt.Errorf("open worktree: %w", err)
	}
	worktreeRoot = wt.Filesystem().Root()
	if worktreeRoot == "" {
		return "", "", errors.New("repository worktree filesystem has no root path")
	}
	commonDir, err = resolveGitCommonDir(ctx, s.repo)
	if err != nil {
		return "", "", err
	}
	return worktreeRoot, commonDir, nil
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
	refName := "refs/heads/" + branchName

	// All-zeros OID with the repo's object-format width means "must not
	// exist". SHA-1 repos want 40 zeros, SHA-256 repos want 64; mirror
	// newHash's hex width so we pick the right one without an extra git call.
	newValue := newHash.String()
	oldValue := strings.Repeat("0", newHash.HexSize())
	if expectedHash != plumbing.ZeroHash {
		oldValue = expectedHash.String()
	}

	cmd := exec.CommandContext(ctx, "git", "update-ref", refName, newValue, oldValue)
	cmd.Dir = repoRoot
	// Force English diagnostics so the CAS-conflict pattern match below
	// isn't defeated by a translated stderr message in a non-C locale.
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	output, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}

	out := string(output)
	// Git's CAS-failure messages: "cannot lock ref ..." (covers both
	// "is at X but expected Y" and "reference already exists" for the
	// zero-OID case). Other failures propagate.
	if strings.Contains(out, "cannot lock ref") || strings.Contains(out, "but expected") {
		return ErrShadowRefBusy
	}
	return fmt.Errorf("git update-ref %s: %s: %w", refName, strings.TrimSpace(out), err)
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
	path, err := shadowBranchLockPath(commonDir, branchName)
	if err != nil {
		return err
	}
	release, err := flock.Acquire(path)
	if err != nil {
		return fmt.Errorf("acquire shadow flock %s: %w", branchName, err)
	}
	defer release()
	return fn()
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
