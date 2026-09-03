package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/logging"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
)

type persistentRefBuilder func() (newHash, expectedHash plumbing.Hash, err error)

func repositoryDirs(ctx context.Context, repo *git.Repository) (worktreeRoot, commonDir string, err error) {
	wt, err := repo.Worktree()
	if err != nil {
		return "", "", fmt.Errorf("open worktree: %w", err)
	}
	worktreeRoot = wt.Filesystem().Root()
	if worktreeRoot == "" {
		return "", "", errors.New("repository worktree filesystem has no root path")
	}
	commonDir, err = resolveGitCommonDir(ctx, repo)
	if err != nil {
		return "", "", err
	}
	return worktreeRoot, commonDir, nil
}

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

// casDeleteRef atomically deletes refName through native Git's lock protocol,
// via `git update-ref -d <ref> <old>`. The delete only succeeds if the ref
// currently points at expectedHash; otherwise it returns ErrShadowRefBusy so
// the caller can decide whether that's a benign race (someone else already
// moved or removed it) or a real conflict.
func casDeleteRef(ctx context.Context, repoRoot string, refName plumbing.ReferenceName, expectedHash plumbing.Hash) error {
	cmd := exec.CommandContext(ctx, "git", "update-ref", "-d", refName.String(), expectedHash.String())
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
	return fmt.Errorf("git update-ref -d %s: %s: %w", refName, strings.TrimSpace(out), err)
}

func persistentRefLockPath(commonDir string, refName plumbing.ReferenceName) (string, error) {
	lockDir := filepath.Join(commonDir, "entire-persistent-ref-locks")
	if err := os.MkdirAll(lockDir, 0o750); err != nil {
		return "", fmt.Errorf("create persistent ref lock directory: %w", err)
	}
	safe := strings.NewReplacer("/", "_", "\\", "_", ":", "_").Replace(refName.String())
	return filepath.Join(lockDir, safe+".lock"), nil
}

func withPersistentRefFlock(ctx context.Context, commonDir string, refName plumbing.ReferenceName, fn func() error) error {
	path, err := persistentRefLockPath(commonDir, refName)
	if err != nil {
		return err
	}
	release, err := flock.AcquireContext(ctx, path)
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
	repoRoot, commonDir, err := repositoryDirs(ctx, repo)
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
