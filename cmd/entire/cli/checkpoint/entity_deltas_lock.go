package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-git/go-git/v6"

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
	commonDir, err := gitCommonDirForWorktree(ctx, worktreePath)
	if err != nil {
		return "", err
	}
	return filepath.Join(commonDir, entityDeltasLockFile), nil
}

func gitCommonDirForWorktree(ctx context.Context, worktreePath string) (string, error) {
	if worktreePath == "" {
		return "", errors.New("empty worktree path")
	}
	cmd := exec.CommandContext(ctx, "git", "-C", worktreePath, "rev-parse", "--git-common-dir")
	cmd.Env = gitEnvWithoutRepoOverrides()
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get git common dir for %s: %w", worktreePath, err)
	}
	commonDir := strings.TrimSpace(string(output))
	if commonDir == "" {
		return "", fmt.Errorf("empty git common dir for %s", worktreePath)
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(worktreePath, commonDir)
	}
	commonDir = filepath.Clean(commonDir)
	if resolved, err := filepath.EvalSymlinks(commonDir); err == nil {
		commonDir = resolved
	}
	return commonDir, nil
}

func gitEnvWithoutRepoOverrides() []string {
	env := os.Environ()
	out := env[:0]
	for _, e := range env {
		if strings.HasPrefix(e, "GIT_DIR=") ||
			strings.HasPrefix(e, "GIT_WORK_TREE=") ||
			strings.HasPrefix(e, "GIT_INDEX_FILE=") {
			continue
		}
		out = append(out, e)
	}
	return out
}
