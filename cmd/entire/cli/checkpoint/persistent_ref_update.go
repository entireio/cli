package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/osroot"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
)

type persistentRefBuilder func() (newHash, expectedHash plumbing.Hash, err error)

// casUpdateRef adapts precise native Git CAS outcomes to the legacy retry
// sentinel used by checkpoint writers.
func casUpdateRef(ctx context.Context, repoRoot string, refName plumbing.ReferenceName, newHash, expectedHash plumbing.Hash) error {
	err := gitrepo.CompareAndSwapRef(ctx, repoRoot, refName, newHash, expectedHash)
	if err == nil {
		return nil
	}
	if !errors.Is(err, gitrepo.ErrRefSymbolic) && (errors.Is(err, gitrepo.ErrRefCASConflict) || errors.Is(err, gitrepo.ErrRefLocked)) {
		return fmt.Errorf("%w: %w", ErrShadowRefBusy, err)
	}
	return fmt.Errorf("compare and swap ref %s: %w", refName, err)
}

const persistentRefLockDirName = "entire-persistent-ref-locks"

// persistentRefLock keeps lock paths inside the shared Git directory and rejects
// symlinked directories. The caller must acquire the lock through flock's In API
// to also reject a symlink at the lock file.
func persistentRefLock(commonDir string, refName plumbing.ReferenceName) (*os.Root, string, error) {
	root, err := gitdir.OpenAt(commonDir)
	if err != nil {
		return nil, "", fmt.Errorf("open git common dir: %w", err)
	}
	if err := osroot.MkdirAllNoSymlink(root, persistentRefLockDirName, 0o750); err != nil {
		return nil, "", fmt.Errorf("create persistent ref lock directory: %w", err)
	}
	safe := strings.NewReplacer("/", "_", "\\", "_", ":", "_").Replace(refName.String())
	return root, persistentRefLockDirName + "/" + safe + ".lock", nil
}

func withPersistentRefFlock(ctx context.Context, commonDir string, refName plumbing.ReferenceName, fn func() error) error {
	root, name, err := persistentRefLock(commonDir, refName)
	if err != nil {
		return err
	}
	release, err := flock.AcquireContextIn(ctx, root, name)
	if err != nil {
		return fmt.Errorf("acquire persistent ref flock %s: %w", refName, err)
	}
	defer release()
	return fn()
}

func withLockedPersistentRef(
	ctx context.Context,
	repo *git.Repository,
	refName plumbing.ReferenceName,
	fn func(repoRoot, commonDir string) error,
) error {
	repoRoot, commonDir, err := repositoryDirs(repo)
	if err != nil {
		return fmt.Errorf("resolve repository directories: %w", err)
	}
	return withPersistentRefFlock(ctx, commonDir, refName, func() error {
		return fn(repoRoot, commonDir)
	})
}

// CASPersistentRef updates refName through native Git's cross-process
// compare-and-swap protocol. It deliberately does not acquire the persistent
// writer flock: the expected value predates this call, so native Git provides
// the safety boundary without making deadline-free pre-push contexts wait
// indefinitely. Native lock contention is retried with the original expected
// hash; a CAS conflict remains terminal.
func CASPersistentRef(
	ctx context.Context,
	repo *git.Repository,
	refName plumbing.ReferenceName,
	newHash, expectedHash plumbing.Hash,
) error {
	repoRoot, err := repositoryRoot(repo)
	if err != nil {
		return fmt.Errorf("resolve repository root: %w", err)
	}
	return retryPersistentRefLockContention(ctx, refName, func() error {
		return gitrepo.CompareAndSwapRef(ctx, repoRoot, refName, newHash, expectedHash)
	})
}

func retryPersistentRefLockContention(
	ctx context.Context,
	refName plumbing.ReferenceName,
	update func() error,
) error {
	var lockErr error
	for attempt := range shadowRefMaxRetries {
		refErr := update()
		if refErr == nil {
			return nil
		}
		if errors.Is(refErr, gitrepo.ErrRefSymbolic) || !errors.Is(refErr, gitrepo.ErrRefLocked) {
			return refErr
		}
		lockErr = refErr
		if attempt+1 == shadowRefMaxRetries {
			break
		}
		if backoffErr := shadowRefBackoff(ctx, attempt); backoffErr != nil {
			return backoffErr
		}
	}
	return fmt.Errorf("update persistent ref %s after %d lock attempts: %w", refName, shadowRefMaxRetries, lockErr)
}

// updatePersistentRef serializes Entire writers for one ref and retains a CAS
// retry for native Git or other external writers. build runs again after every
// conflict, so each retry reconstructs its tree and commit from the fresh tip.
func updatePersistentRef(ctx context.Context, repo *git.Repository, refName plumbing.ReferenceName, build persistentRefBuilder) error {
	return withLockedPersistentRef(ctx, repo, refName, func(repoRoot, commonDir string) error {
		for attempt := range shadowRefMaxRetries {
			newHash, expectedHash, buildErr := build()
			if buildErr != nil {
				return buildErr
			}

			refErr := casUpdateRef(ctx, repoRoot, refName, newHash, expectedHash)
			if refErr == nil {
				return nil
			}
			if !errors.Is(refErr, ErrShadowRefBusy) {
				return fmt.Errorf("update persistent ref %s: %w", refName, refErr)
			}
			if newHash != expectedHash {
				tryDeleteLooseObject(commonDir, newHash)
			}

			if backoffErr := shadowRefBackoff(ctx, attempt); backoffErr != nil {
				return backoffErr
			}
		}

		logging.Warn(logging.WithComponent(ctx, "checkpoint"),
			"persistent ref CAS retry budget exhausted",
			slog.String("ref", refName.String()),
			slog.Int("retries", shadowRefMaxRetries),
		)
		return fmt.Errorf("failed to update persistent ref %s after %d CAS retries: %w", refName, shadowRefMaxRetries, ErrShadowRefBusy)
	})
}
