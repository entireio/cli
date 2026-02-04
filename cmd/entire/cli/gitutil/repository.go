// Package gitutil provides git repository utilities using native git CLI commands.
// This package exists because go-git has known issues with certain operations:
//   - worktree.Status() doesn't respect global gitignore (core.excludesfile)
//   - worktree.Reset() with HardReset deletes ignored directories
//   - worktree.Checkout() has similar issues with untracked files
//
// By using native git commands, we ensure full compatibility with git's behavior.
// See: https://github.com/entireio/cli/pull/129
package gitutil

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v5"
)

// Common directory names
const (
	GitDir = ".git"
)

// OpenRepository opens the git repository with linked worktree support enabled.
// Uses 'git rev-parse --show-toplevel' to find the repository root, which works
// correctly even when called from a subdirectory within the repo.
//
// This is the canonical way to open a repository in this codebase. It handles:
//   - Subdirectory execution (finds repo root automatically)
//   - Linked worktrees (enables EnableDotGitCommonDir)
func OpenRepository() (*git.Repository, error) {
	// Find the repository root using git rev-parse --show-toplevel
	// This works correctly from any subdirectory within the repository
	repoRoot, err := GetWorktreePath()
	if err != nil {
		// Fallback to current directory if git command fails
		// (e.g., if git is not installed or we're not in a repo)
		repoRoot = "."
	}

	repo, err := git.PlainOpenWithOptions(repoRoot, &git.PlainOpenOptions{
		EnableDotGitCommonDir: true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to open repository: %w", err)
	}
	return repo, nil
}

// GetWorktreePath returns the absolute path to the current worktree root.
// This is the working directory path, not the git directory.
// Works correctly from any subdirectory within the repository.
func GetWorktreePath() (string, error) {
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get worktree path: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

// IsInsideWorktree returns true if the current directory is inside a git worktree
// (as opposed to the main repository). Worktrees have .git as a file pointing
// to the main repo, while the main repo has .git as a directory.
// This function works correctly from any subdirectory within the repository.
func IsInsideWorktree() bool {
	// First find the repository root
	repoRoot, err := GetWorktreePath()
	if err != nil {
		return false
	}

	gitPath := filepath.Join(repoRoot, GitDir)
	gitInfo, err := os.Stat(gitPath)
	if err != nil {
		return false
	}
	return !gitInfo.IsDir()
}

// GetMainRepoRoot returns the root directory of the main repository.
// In the main repo, this is the worktree path (repo root).
// In a worktree, this parses the .git file to find the main repo.
// This function works correctly from any subdirectory within the repository.
//
// Per gitrepository-layout(5), a worktree's .git file is a "gitfile" containing
// "gitdir: <path>" pointing to $GIT_DIR/worktrees/<id> in the main repository.
// See: https://git-scm.com/docs/gitrepository-layout
func GetMainRepoRoot() (string, error) {
	// First find the worktree/repo root
	repoRoot, err := GetWorktreePath()
	if err != nil {
		return "", fmt.Errorf("failed to get worktree path: %w", err)
	}

	if !IsInsideWorktree() {
		return repoRoot, nil
	}

	// Worktree .git file contains: "gitdir: /path/to/main/.git/worktrees/<id>"
	gitFilePath := filepath.Join(repoRoot, GitDir)
	content, err := os.ReadFile(gitFilePath) //nolint:gosec // G304: gitFilePath is constructed from repo root, not user input
	if err != nil {
		return "", fmt.Errorf("failed to read .git file: %w", err)
	}

	gitdir := strings.TrimSpace(string(content))
	gitdir = strings.TrimPrefix(gitdir, "gitdir: ")

	// Extract main repo root: everything before "/.git/"
	idx := strings.LastIndex(gitdir, "/.git/")
	if idx < 0 {
		return "", fmt.Errorf("unexpected gitdir format: %s", gitdir)
	}
	return gitdir[:idx], nil
}

// GetGitCommonDir returns the path to the shared git directory.
// In a regular checkout, this is .git/
// In a worktree, this is the main repo's .git/ (not .git/worktrees/<name>/)
// Uses git rev-parse --git-common-dir for reliable handling of worktrees.
func GetGitCommonDir() (string, error) {
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--git-common-dir")
	cmd.Dir = "."
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get git common dir: %w", err)
	}

	commonDir := strings.TrimSpace(string(output))

	// git rev-parse --git-common-dir returns relative paths from the working directory,
	// so we need to make it absolute if it isn't already
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(".", commonDir)
	}

	return filepath.Clean(commonDir), nil
}
