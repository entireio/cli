package checkpoint

import (
	"errors"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"

	"github.com/go-git/go-git/v6"
)

func repositoryRoot(repo *git.Repository) (string, error) {
	worktree, err := repo.Worktree()
	if err != nil {
		return "", fmt.Errorf("open worktree: %w", err)
	}
	worktreeRoot := worktree.Filesystem().Root()
	if worktreeRoot == "" {
		return "", errors.New("repository worktree filesystem has no root path")
	}
	return worktreeRoot, nil
}

func repositoryDirs(repo *git.Repository) (worktreeRoot, commonDir string, err error) {
	worktreeRoot, err = repositoryRoot(repo)
	if err != nil {
		return "", "", err
	}
	metadata, err := gitrepo.ResolveWorktreeMetadata(worktreeRoot)
	if err != nil {
		return "", "", fmt.Errorf("resolve repository metadata: %w", err)
	}
	return worktreeRoot, metadata.CommonDir, nil
}
