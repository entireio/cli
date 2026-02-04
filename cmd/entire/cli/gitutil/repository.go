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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
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

// GetGitAuthorFromRepo retrieves the git user.name and user.email from the repository config.
// It checks local config first, then falls back to global config.
// Returns ("Unknown", "unknown@local") if no user is configured - this allows
// operations to proceed even without git user config, which is especially useful
// for internal metadata commits on branches like entire/sessions.
func GetGitAuthorFromRepo(repo *git.Repository) (name, email string) {
	// Get repository config (includes local settings)
	cfg, err := repo.Config()
	if err == nil {
		name = cfg.User.Name
		email = cfg.User.Email
	}

	// If not found in local config, try global config
	if name == "" || email == "" {
		globalCfg, err := config.LoadConfig(config.GlobalScope)
		if err == nil {
			if name == "" {
				name = globalCfg.User.Name
			}
			if email == "" {
				email = globalCfg.User.Email
			}
		}
	}

	// Provide sensible defaults if git user is not configured
	if name == "" {
		name = "Unknown"
	}
	if email == "" {
		email = "unknown@local"
	}

	return name, email
}

// GitAuthor represents the git user configuration
type GitAuthor struct {
	Name  string
	Email string
}

// GetGitAuthor retrieves the git user.name and user.email from the repository config.
// It checks local config first, then falls back to global config.
// If go-git can't find the config, it falls back to using the git command.
// Returns fallback defaults if no user is configured anywhere.
func GetGitAuthor() (*GitAuthor, error) {
	repo, err := OpenRepository()
	if err != nil {
		return nil, fmt.Errorf("failed to open git repository: %w", err)
	}

	name, email := GetGitAuthorFromRepo(repo)

	// If go-git returned defaults, try using git command as fallback
	// This handles cases where go-git can't find the config (e.g., different HOME paths,
	// non-standard config locations, or environment issues in hook contexts)
	if name == "Unknown" {
		if gitName := GetConfigValue("user.name"); gitName != "" {
			name = gitName
		}
	}
	if email == "unknown@local" {
		if gitEmail := GetConfigValue("user.email"); gitEmail != "" {
			email = gitEmail
		}
	}

	return &GitAuthor{
		Name:  name,
		Email: email,
	}, nil
}

// IsOnDefaultBranch checks if the repository is currently on the default branch.
// It determines the default branch by:
// 1. Checking the remote origin's HEAD reference
// 2. Falling back to common names (main, master) if remote HEAD is unavailable
// Returns (isDefault, branchName, error)
func IsOnDefaultBranch() (bool, string, error) {
	repo, err := OpenRepository()
	if err != nil {
		return false, "", fmt.Errorf("failed to open git repository: %w", err)
	}

	// Get current branch
	head, err := repo.Head()
	if err != nil {
		return false, "", fmt.Errorf("failed to get HEAD: %w", err)
	}

	if !head.Name().IsBranch() {
		// Detached HEAD - not on any branch
		return false, "", nil
	}

	currentBranch := head.Name().Short()

	// Try to get default branch from remote origin's HEAD
	defaultBranch := GetDefaultBranchFromRemote(repo)

	// If we couldn't determine from remote, use common defaults
	if defaultBranch == "" {
		// Check if current branch is a common default name
		if currentBranch == "main" || currentBranch == "master" {
			return true, currentBranch, nil
		}
		return false, currentBranch, nil
	}

	return currentBranch == defaultBranch, currentBranch, nil
}

// GetDefaultBranchFromRemote tries to determine the default branch from the origin remote.
// Returns empty string if unable to determine.
func GetDefaultBranchFromRemote(repo *git.Repository) string {
	// Try to get the symbolic reference for origin/HEAD
	ref, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", "HEAD"), true)
	if err == nil && ref != nil {
		// ref.Target() gives us something like "refs/remotes/origin/main"
		target := ref.Target().String()
		if after, found := strings.CutPrefix(target, "refs/remotes/origin/"); found {
			return after
		}
	}

	// Fallback: check if origin/main or origin/master exists
	if _, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", "main"), true); err == nil {
		return "main"
	}
	if _, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", "master"), true); err == nil {
		return "master"
	}

	return ""
}

// ShouldSkipOnDefaultBranch checks if we're on the default branch.
// Returns (shouldSkip, branchName). If shouldSkip is true, the caller should
// skip the operation to avoid polluting main/master history.
// If the branch cannot be determined, returns (false, "") to allow the operation.
func ShouldSkipOnDefaultBranch() (bool, string) {
	isDefault, branchName, err := IsOnDefaultBranch()
	if err != nil {
		// If we can't determine, allow the operation
		return false, ""
	}
	return isDefault, branchName
}

// ShouldSkipOnDefaultBranchForStrategy checks if we're on the default branch and
// whether the strategy allows operating on it.
// Returns (shouldSkip, branchName). If shouldSkip is true, the caller should
// skip the operation. Strategies that allow main branch return false.
func ShouldSkipOnDefaultBranchForStrategy(allowsMainBranch bool) (bool, string) {
	isDefault, branchName, err := IsOnDefaultBranch()
	if err != nil {
		// If we can't determine, allow the operation
		return false, ""
	}
	if !isDefault {
		return false, branchName
	}

	if allowsMainBranch {
		return false, branchName
	}

	return true, branchName
}

// GetCurrentBranch returns the name of the current branch.
// Returns an error if in detached HEAD state or if not in a git repository.
func GetCurrentBranch() (string, error) {
	repo, err := OpenRepository()
	if err != nil {
		return "", fmt.Errorf("failed to open git repository: %w", err)
	}

	head, err := repo.Head()
	if err != nil {
		return "", fmt.Errorf("failed to get HEAD: %w", err)
	}

	if !head.Name().IsBranch() {
		return "", errors.New("not on a branch (detached HEAD)")
	}

	return head.Name().Short(), nil
}

// GetMergeBase finds the common ancestor (merge-base) between two branches.
// Returns the hash of the merge-base commit.
func GetMergeBase(branch1, branch2 string) (*plumbing.Hash, error) {
	repo, err := OpenRepository()
	if err != nil {
		return nil, fmt.Errorf("failed to open git repository: %w", err)
	}

	// Resolve branch references
	ref1, err := repo.Reference(plumbing.NewBranchReferenceName(branch1), true)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve branch %s: %w", branch1, err)
	}

	ref2, err := repo.Reference(plumbing.NewBranchReferenceName(branch2), true)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve branch %s: %w", branch2, err)
	}

	// Get commit objects
	commit1, err := repo.CommitObject(ref1.Hash())
	if err != nil {
		return nil, fmt.Errorf("failed to get commit for %s: %w", branch1, err)
	}

	commit2, err := repo.CommitObject(ref2.Hash())
	if err != nil {
		return nil, fmt.Errorf("failed to get commit for %s: %w", branch2, err)
	}

	// Find common ancestor
	mergeBase, err := commit1.MergeBase(commit2)
	if err != nil {
		return nil, fmt.Errorf("failed to find merge base: %w", err)
	}

	if len(mergeBase) == 0 {
		return nil, errors.New("no common ancestor found")
	}

	hash := mergeBase[0].Hash
	return &hash, nil
}

// BranchExistsOnRemote checks if a branch exists on the origin remote.
// Returns true if the branch is tracked on origin, false otherwise.
func BranchExistsOnRemote(branchName string) (bool, error) {
	repo, err := OpenRepository()
	if err != nil {
		return false, fmt.Errorf("failed to open git repository: %w", err)
	}

	// Check for remote reference: refs/remotes/origin/<branchName>
	_, err = repo.Reference(plumbing.NewRemoteReferenceName("origin", branchName), true)
	if err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("failed to check remote branch: %w", err)
	}

	return true, nil
}

// BranchExistsLocally checks if a local branch exists.
func BranchExistsLocally(branchName string) (bool, error) {
	repo, err := OpenRepository()
	if err != nil {
		return false, fmt.Errorf("failed to open git repository: %w", err)
	}

	_, err = repo.Reference(plumbing.NewBranchReferenceName(branchName), true)
	if err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("failed to check branch: %w", err)
	}

	return true, nil
}
