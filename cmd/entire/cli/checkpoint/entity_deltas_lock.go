package checkpoint

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/go-git/go-git/v6"

	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
)

const entityDeltasLockFile = "entire-entity-deltas.lock"

// entityDeltasLockWait bounds how long a backfill waits for the shared lock.
// Matches strategy/entity_deltas.go so detached children and OPF rewrite contend
// on the same file with the same patience.
const entityDeltasLockWait = 2 * time.Minute

// acquireEntityDeltasLockFromRepo serializes entity-delta backfill writes with
// the detached child and the pre-push OPF rewrite (git-branch backend). The
// git-refs per-checkpoint CAS must hold the same lock for its critical section.
func acquireEntityDeltasLockFromRepo(ctx context.Context, repo *git.Repository) (func(), error) {
	wt, err := repo.Worktree()
	if err != nil {
		return nil, fmt.Errorf("resolve worktree for entity deltas lock: %w", err)
	}
	path, err := entityDeltasLockPath(ctx, wt.Filesystem().Root())
	if err != nil {
		return nil, err
	}
	waitCtx, cancel := context.WithTimeout(ctx, entityDeltasLockWait)
	defer cancel()
	release, err := flock.AcquireContext(waitCtx, path)
	if err != nil {
		return nil, fmt.Errorf("acquire entity deltas lock: %w", err)
	}
	return release, nil
}

func entityDeltasLockPath(ctx context.Context, worktreePath string) (string, error) {
	commonDir, err := gitdir.CommonDirForWorktree(ctx, worktreePath)
	if err != nil {
		return "", err
	}
	return filepath.Join(commonDir, entityDeltasLockFile), nil
}
