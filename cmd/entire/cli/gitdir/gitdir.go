// Package gitdir resolves git directory paths for a worktree without inheriting
// hook-scoped GIT_DIR overrides.
package gitdir

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// CommonDirForWorktree returns the absolute git common directory for worktreePath.
func CommonDirForWorktree(ctx context.Context, worktreePath string) (string, error) {
	if worktreePath == "" {
		return "", errors.New("empty worktree path")
	}

	cmd := exec.CommandContext(ctx, "git", "-C", worktreePath, "rev-parse", "--git-common-dir")
	cmd.Env = EnvWithoutRepoOverrides()
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

// EnvWithoutRepoOverrides returns the current environment minus git's repo-selector
// variables so `git -C <worktree>` resolves against the requested path.
func EnvWithoutRepoOverrides() []string {
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
