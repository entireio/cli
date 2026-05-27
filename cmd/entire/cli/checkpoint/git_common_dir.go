package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v6"
)

func resolveGitCommonDir(ctx context.Context, repo *git.Repository) (string, error) {
	worktree, err := repo.Worktree()
	if err != nil {
		return "", fmt.Errorf("open worktree for git common dir: %w", err)
	}
	root := worktree.Filesystem().Root()
	if root == "" {
		return "", errors.New("resolve worktree root for git common dir")
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
	return filepath.Clean(commonDir), nil
}
