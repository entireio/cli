package cli

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"entire.io/cli/cmd/entire/cli/paths"
	"entire.io/cli/cmd/entire/cli/strategy"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
)

// openRepository opens the git repository with linked worktree support enabled.
// This is a convenience wrapper around strategy.OpenRepository() for use in the CLI package.
func openRepository() (*git.Repository, error) {
	repo, err := strategy.OpenRepository()
	if err != nil {
		return nil, fmt.Errorf("failed to open repository: %w", err)
	}
	return repo, nil
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
	repo, err := openRepository()
	if err != nil {
		return nil, fmt.Errorf("failed to open git repository: %w", err)
	}

	name, email := strategy.GetGitAuthorFromRepo(repo)

	// If go-git returned defaults, try using git command as fallback
	// This handles cases where go-git can't find the config (e.g., different HOME paths,
	// non-standard config locations, or environment issues in hook contexts)
	if name == "Unknown" {
		if gitName := getGitConfigValue("user.name"); gitName != "" {
			name = gitName
		}
	}
	if email == "unknown@local" {
		if gitEmail := getGitConfigValue("user.email"); gitEmail != "" {
			email = gitEmail
		}
	}

	return &GitAuthor{
		Name:  name,
		Email: email,
	}, nil
}

// getGitConfigValue retrieves a git config value using the git command.
// Returns empty string if the value is not set or on error.
func getGitConfigValue(key string) string {
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "git", "config", "--get", key)
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

// IsOnDefaultBranch checks if the repository is currently on the default branch.
// It determines the default branch by:
// 1. Checking the remote origin's HEAD reference
// 2. Falling back to common names (main, master) if remote HEAD is unavailable
// Returns (isDefault, branchName, error)
func IsOnDefaultBranch() (bool, string, error) {
	repo, err := openRepository()
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
	defaultBranch := getDefaultBranchFromRemote(repo)

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

// getDefaultBranchFromRemote tries to determine the default branch from the origin remote.
// Returns empty string if unable to determine.
func getDefaultBranchFromRemote(repo *git.Repository) string {
	// Try to get the symbolic reference for origin/HEAD
	ref, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", "HEAD"), true)
	if err == nil && ref != nil {
		// ref.Target() gives us something like "refs/remotes/origin/main"
		target := ref.Target().String()
		if strings.HasPrefix(target, "refs/remotes/origin/") {
			return strings.TrimPrefix(target, "refs/remotes/origin/")
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
// whether the current strategy allows operating on it.
// Returns (shouldSkip, branchName). If shouldSkip is true, the caller should
// skip the operation. Shadow strategies return false (allowing main branch).
func ShouldSkipOnDefaultBranchForStrategy() (bool, string) {
	isDefault, branchName, err := IsOnDefaultBranch()
	if err != nil {
		// If we can't determine, allow the operation
		return false, ""
	}
	if !isDefault {
		return false, branchName
	}

	// Check if the current strategy allows main branch
	start := GetStrategy()

	if start.AllowsMainBranch() {
		return false, branchName
	}

	return true, branchName
}

// GetCurrentBranch returns the name of the current branch.
// Returns an error if in detached HEAD state or if not in a git repository.
func GetCurrentBranch() (string, error) {
	repo, err := openRepository()
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
	repo, err := openRepository()
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

// HasUncommittedChanges checks if there are any uncommitted changes in the repository.
// This includes staged changes, unstaged changes, and untracked files.
func HasUncommittedChanges() (bool, error) {
	repo, err := openRepository()
	if err != nil {
		return false, fmt.Errorf("failed to open git repository: %w", err)
	}

	w, err := repo.Worktree()
	if err != nil {
		return false, fmt.Errorf("failed to get worktree: %w", err)
	}

	status, err := w.Status()
	if err != nil {
		return false, fmt.Errorf("failed to get status: %w", err)
	}

	// Check if status is clean (no changes of any kind)
	return !status.IsClean(), nil
}

// findNewUntrackedFiles finds files that are newly untracked (not in pre-existing list)
func findNewUntrackedFiles(current, preExisting []string) []string {
	preExistingSet := make(map[string]bool)
	for _, file := range preExisting {
		preExistingSet[file] = true
	}

	var newFiles []string
	for _, file := range current {
		if !preExistingSet[file] {
			newFiles = append(newFiles, file)
		}
	}
	return newFiles
}

// BranchExistsOnRemote checks if a branch exists on the origin remote.
// Returns true if the branch is tracked on origin, false otherwise.
func BranchExistsOnRemote(branchName string) (bool, error) {
	repo, err := openRepository()
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
	repo, err := openRepository()
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

// CheckoutBranch switches to the specified local branch or commit.
// Uses git CLI instead of go-git to work around go-git v5 bug where Checkout
// deletes untracked files (see https://github.com/go-git/go-git/issues/970).
// Should be switched back to go-git once we upgrade to go-git v6
// Returns an error if the ref doesn't exist or checkout fails.
func CheckoutBranch(ref string) error {
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "git", "checkout", ref)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("checkout failed: %s: %w", strings.TrimSpace(string(output)), err)
	}
	return nil
}

// FetchAndCheckoutRemoteBranch fetches a branch from origin and creates a local tracking branch.
func FetchAndCheckoutRemoteBranch(branchName string) error {
	repo, err := openRepository()
	if err != nil {
		return fmt.Errorf("failed to open repository: %w", err)
	}

	// Fetch the specific branch from origin
	remote, err := repo.Remote("origin")
	if err != nil {
		return fmt.Errorf("failed to get origin remote: %w", err)
	}

	refSpec := fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", branchName, branchName)
	err = remote.Fetch(&git.FetchOptions{
		RefSpecs: []config.RefSpec{config.RefSpec(refSpec)},
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return fmt.Errorf("failed to fetch branch from origin: %w", err)
	}

	// Get the remote branch reference
	remoteRef, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", branchName), true)
	if err != nil {
		return fmt.Errorf("branch '%s' not found on origin: %w", branchName, err)
	}

	// Create local branch pointing to the same commit
	localRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName(branchName), remoteRef.Hash())
	err = repo.Storer.SetReference(localRef)
	if err != nil {
		return fmt.Errorf("failed to create local branch: %w", err)
	}

	// Checkout the new local branch
	return CheckoutBranch(branchName)
}

// FetchMetadataBranch fetches the entire/sessions branch from origin and creates/updates the local branch.
// This is used when the metadata branch exists on remote but not locally.
func FetchMetadataBranch() error {
	repo, err := openRepository()
	if err != nil {
		return fmt.Errorf("failed to open repository: %w", err)
	}

	branchName := paths.MetadataBranchName

	// Fetch the specific branch from origin
	remote, err := repo.Remote("origin")
	if err != nil {
		return fmt.Errorf("failed to get origin remote: %w", err)
	}

	refSpec := fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", branchName, branchName)
	err = remote.Fetch(&git.FetchOptions{
		RefSpecs: []config.RefSpec{config.RefSpec(refSpec)},
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return fmt.Errorf("failed to fetch %s from origin: %w", branchName, err)
	}

	// Get the remote branch reference
	remoteRef, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", branchName), true)
	if err != nil {
		return fmt.Errorf("branch '%s' not found on origin: %w", branchName, err)
	}

	// Create or update local branch pointing to the same commit
	localRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName(branchName), remoteRef.Hash())
	if err := repo.Storer.SetReference(localRef); err != nil {
		return fmt.Errorf("failed to create local %s branch: %w", branchName, err)
	}

	return nil
}
