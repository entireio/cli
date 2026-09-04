package checkpoint

import (
	"errors"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"

	"github.com/go-git/go-git/v6"
)

func repositoryDirs(repo *git.Repository) (worktreeRoot, commonDir string, err error) {
	worktree, err := repo.Worktree()
	if err != nil {
		return "", "", fmt.Errorf("open worktree: %w", err)
	}
	worktreeRoot = worktree.Filesystem().Root()
	if worktreeRoot == "" {
		return "", "", errors.New("repository worktree filesystem has no root path")
	}
	metadata, err := gitrepo.ResolveWorktreeMetadata(worktreeRoot)
	if err != nil {
		return "", "", fmt.Errorf("resolve repository metadata: %w", err)
	}
	return worktreeRoot, metadata.CommonDir, nil
}
