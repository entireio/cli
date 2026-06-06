package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/paths"

	"github.com/go-git/go-billy/v6"
	"github.com/go-git/go-billy/v6/osfs"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/cache"
	gitfilesystem "github.com/go-git/go-git/v6/storage/filesystem"
	"github.com/go-git/go-git/v6/storage/filesystem/dotgit"
)

const gitDir = ".git"

// OpenCurrent opens the current git worktree with object alternates enabled.
// The caller owns the returned repository and must close it.
func OpenCurrent(ctx context.Context) (*git.Repository, error) {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		// Fallback to current directory if git command fails
		// (e.g., if git is not installed or we're not in a repo).
		repoRoot = "."
	}
	return OpenPath(repoRoot)
}

// OpenPath opens a git repository with object alternates enabled.
// The caller owns the returned repository and must close it.
func OpenPath(repoRoot string) (*git.Repository, error) {
	repo, err := openPathWithAlternates(repoRoot)
	if err != nil {
		if hasAlternates, altErr := hasObjectAlternates(repoRoot); altErr == nil && hasAlternates {
			return nil, fmt.Errorf("failed to open repository with alternates support: %w", err)
		}

		// Intentional PlainOpen fallback for unusual layouts that do not use
		// alternates. Repositories with alternates must not silently downgrade
		// because PlainOpen cannot read absolute alternate object directories.
		if fallbackRepo, fallbackErr := git.PlainOpen(repoRoot); fallbackErr == nil {
			return fallbackRepo, nil
		}
		return nil, fmt.Errorf("failed to open repository: %w", err)
	}
	return repo, nil
}

func hasObjectAlternates(repoRoot string) (bool, error) {
	repoRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return false, fmt.Errorf("resolve repository root: %w", err)
	}

	dotGitPath, err := resolveDotGitPath(repoRoot)
	if err != nil {
		return false, fmt.Errorf("resolve .git path: %w", err)
	}

	commonGitPath, err := resolveCommonGitPath(dotGitPath)
	if err != nil {
		return false, fmt.Errorf("resolve common git path: %w", err)
	}

	candidates := []string{filepath.Join(dotGitPath, "objects", "info", "alternates")}
	if commonGitPath != "" {
		candidates = append(candidates, filepath.Join(commonGitPath, "objects", "info", "alternates"))
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			return true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("stat alternates file: %w", err)
		}
	}
	return false, nil
}

func openPathWithAlternates(repoRoot string) (*git.Repository, error) {
	repoRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve repository root: %w", err)
	}

	dotGitPath, err := resolveDotGitPath(repoRoot)
	if err != nil {
		return nil, err
	}

	commonGitPath, err := resolveCommonGitPath(dotGitPath)
	if err != nil {
		return nil, err
	}

	// Wrap the git-dir filesystems so relative alternate object directories are
	// rewritten to absolute paths on read; go-git cannot follow relative
	// alternates on its own. The alternates file lives under the common git dir
	// for linked worktrees, so wrap both.
	dotGitFS := wrapAlternatesRewrite(osfs.New(dotGitPath, osfs.WithBoundOS()))
	var commonGitFS billy.Filesystem
	if commonGitPath != "" {
		commonGitFS = wrapAlternatesRewrite(osfs.New(commonGitPath, osfs.WithBoundOS()))
	}

	repositoryFS := dotgit.NewRepositoryFilesystem(dotGitFS, commonGitFS)
	// Shared clones write absolute object directories to objects/info/alternates;
	// an OS-rooted filesystem lets go-git follow those paths outside .git.
	storage := gitfilesystem.NewStorageWithOptions(
		repositoryFS,
		cache.NewObjectLRUDefault(),
		gitfilesystem.Options{
			AlternatesFS: newAlternatesFilesystem(),
		},
	)
	repo, err := git.Open(storage, osfs.New(repoRoot, osfs.WithBoundOS()))
	if err != nil {
		_ = storage.Close()
		return nil, fmt.Errorf("open repository storage: %w", err)
	}
	return repo, nil
}

func resolveDotGitPath(repoRoot string) (string, error) {
	gitPath := filepath.Join(repoRoot, gitDir)
	info, err := os.Stat(gitPath)
	if err != nil {
		return "", fmt.Errorf("stat .git path: %w", err)
	}
	if info.IsDir() {
		return gitPath, nil
	}

	content, err := os.ReadFile(gitPath) //nolint:gosec // gitPath is resolved from the git worktree root.
	if err != nil {
		return "", fmt.Errorf("read .git file: %w", err)
	}

	line, _, _ := strings.Cut(string(content), "\n")
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(line), "gitdir:")
	if !ok {
		return "", errors.New(".git file has no gitdir prefix")
	}

	gitdir = strings.TrimSpace(gitdir)
	if filepath.IsAbs(gitdir) {
		return filepath.Clean(gitdir), nil
	}
	return filepath.Clean(filepath.Join(repoRoot, gitdir)), nil
}

func resolveCommonGitPath(dotGitPath string) (string, error) {
	content, err := os.ReadFile(filepath.Join(dotGitPath, "commondir")) //nolint:gosec // dotGitPath is resolved from the git worktree root.
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read commondir file: %w", err)
	}

	commonPath := strings.TrimSpace(string(content))
	if commonPath == "" {
		return "", nil
	}
	if filepath.IsAbs(commonPath) {
		return filepath.Clean(commonPath), nil
	}
	return filepath.Clean(filepath.Join(dotGitPath, commonPath)), nil
}
