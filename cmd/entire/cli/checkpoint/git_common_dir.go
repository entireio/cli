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
	output, err := cmd.Output()
	if err != nil {
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
