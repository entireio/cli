package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/osroot"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
)

type persistentRefBuilder func() (newHash, expectedHash plumbing.Hash, err error)

// casUpdateRef atomically updates refName through native Git's lock protocol.
// Pass ZeroHash as expectedHash to require that the ref does not exist.
func casUpdateRef(ctx context.Context, repoRoot string, refName plumbing.ReferenceName, newHash, expectedHash plumbing.Hash) error {
	newValue := newHash.String()
	oldValue := strings.Repeat("0", newHash.HexSize())
	if expectedHash != plumbing.ZeroHash {
		oldValue = expectedHash.String()
	}

	cmd := exec.CommandContext(ctx, "git", "update-ref", refName.String(), newValue, oldValue)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	output, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}

	out := string(output)
	if strings.Contains(out, "cannot lock ref") || strings.Contains(out, "but expected") {
		return ErrShadowRefBusy
	}
	return fmt.Errorf("git update-ref %s: %s: %w", refName, strings.TrimSpace(out), err)
}

// PersistentRefLockDirName is the persistent-ref lock directory inside the git
// common dir, alongside ShadowLockDirName. Under the git-refs backend this
// accrues one file per checkpoint ref and nothing supersedes them, so it is
// exported for `entire clean` to reclaim: a file is safe to remove once no
// process holds its flock, and the next ref update recreates it.
const PersistentRefLockDirName = "entire-persistent-ref-locks"

// persistentRefLock returns the git common dir's root and the per-ref lock
// file's name inside it, mirroring shadowBranchLock. Ref names are escaped
// because "refs/entire/checkpoints/v1" would otherwise nest directories.
//
// Through the root like every other .git-resident lock: this was the last one
// still opening by path, and flock's os.OpenFile(path, O_RDWR|O_CREATE) follows
// a symlink at the lock path. Git will not check a path out into .git, so this
// is defence in depth rather than a reachable escape — but it is the reason
// openLockFileIn exists, and leaving one caller outside it is how the next one
// gets written that way too.
func persistentRefLock(commonDir string, refName plumbing.ReferenceName) (*os.Root, string, error) {
	root, err := gitdir.OpenAt(commonDir)
	if err != nil {
		return nil, "", fmt.Errorf("open git common dir: %w", err)
	}
	if err := osroot.MkdirAllNoSymlink(root, PersistentRefLockDirName, 0o750); err != nil {
		return nil, "", fmt.Errorf("create persistent ref lock directory: %w", err)
	}
	safe := strings.NewReplacer("/", "_", "\\", "_", ":", "_").Replace(refName.String())
	return root, PersistentRefLockDirName + "/" + safe + ".lock", nil
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

// updatePersistentRef serializes Entire writers for one ref and retains a CAS
// retry for native Git or other external writers. build runs again after every
// conflict, so each retry reconstructs its tree and commit from the fresh tip.
func updatePersistentRef(ctx context.Context, repo *git.Repository, refName plumbing.ReferenceName, build persistentRefBuilder) error {
	repoRoot, commonDir, err := repositoryDirs(repo)
	if err != nil {
		return fmt.Errorf("resolve repository directories: %w", err)
	}

	return withPersistentRefFlock(ctx, commonDir, refName, func() error {
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
