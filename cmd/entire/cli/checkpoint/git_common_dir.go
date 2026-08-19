package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/go-git/go-git/v6"
)

// commonDirCache memoizes resolved git common dirs by worktree root. The value
// cannot change for a worktree's lifetime, and resolving it costs a git
// subprocess (~10ms) — which the push-discovery queue pays on EVERY checkpoint
// ref write and the ephemeral store pays on every shadow-branch write, i.e.
// once per agent turn on the hook path and once per turn imported by `entire
// import`. Mirrors session.getGitCommonDir, which already caches this same
// value process-wide for its own callers.
//
// Only successes are cached: a failure may be transient (a canceled context
// fails the subprocess instantly), and caching it would poison every later
// caller in the process.
var (
	commonDirMu    sync.RWMutex
	commonDirCache = map[string]string{} // worktree root -> resolved common dir
)

// cachedCommonDir returns the memoized common dir for a worktree root.
func cachedCommonDir(root string) (string, bool) {
	commonDirMu.RLock()
	defer commonDirMu.RUnlock()
	dir, ok := commonDirCache[root]
	return dir, ok
}

func resolveGitCommonDir(ctx context.Context, repo *git.Repository) (string, error) {
	worktree, err := repo.Worktree()
	if err != nil {
		return "", fmt.Errorf("open worktree for git common dir: %w", err)
	}
	root := worktree.Filesystem().Root()
	if root == "" {
		return "", errors.New("resolve worktree root for git common dir")
	}
	if cached, ok := cachedCommonDir(root); ok {
		return cached, nil
	}

	cmd := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "--git-common-dir")
	// Use Output (not CombinedOutput) so stderr never pollutes the resolved
	// path on success. Output populates ExitError.Stderr when cmd.Stderr is
	// nil, so error detail is still available without merging streams.
	output, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if detail := strings.TrimSpace(string(exitErr.Stderr)); detail != "" {
				return "", fmt.Errorf("resolve git common dir: %w: %s", err, detail)
			}
		}
		return "", fmt.Errorf("resolve git common dir: %w", err)
	}
	commonDir := strings.TrimSpace(string(output))
	if commonDir == "" {
		return "", errors.New("resolve git common dir: empty output")
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(root, commonDir)
	}
	commonDir = filepath.Clean(commonDir)
	commonDirMu.Lock()
	commonDirCache[root] = commonDir
	commonDirMu.Unlock()
	return commonDir, nil
}
