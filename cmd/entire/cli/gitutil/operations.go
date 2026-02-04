package gitutil

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
)

// CheckoutBranch switches to the specified local branch or commit.
// Uses git CLI instead of go-git to work around go-git v5 bug where Checkout
// deletes untracked files (see https://github.com/go-git/go-git/issues/970).
// Should be switched back to go-git once we upgrade to go-git v6.
// Returns an error if the ref doesn't exist or checkout fails.
func CheckoutBranch(ref string) error {
	// Prevent option injection: refs starting with "-" would be interpreted as flags
	if strings.HasPrefix(ref, "-") {
		return fmt.Errorf("invalid ref: %q (cannot start with dash)", ref)
	}
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "git", "checkout", ref)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("checkout failed: %s: %w", strings.TrimSpace(string(output)), err)
	}
	return nil
}

// HardResetWithProtection performs a git reset --hard to the specified commit.
// Uses the git CLI instead of go-git because go-git's HardReset incorrectly
// deletes untracked directories (like .entire/) even when they're in .gitignore.
// Returns the short commit ID (7 chars) on success for display purposes.
func HardResetWithProtection(commitHash plumbing.Hash) (shortID string, err error) {
	ctx := context.Background()
	hashStr := commitHash.String()
	cmd := exec.CommandContext(ctx, "git", "reset", "--hard", hashStr) //nolint:gosec // hashStr is a plumbing.Hash, not user input
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("reset failed: %s: %w", strings.TrimSpace(string(output)), err)
	}

	// Return short commit ID for display
	shortID = hashStr
	if len(shortID) > 7 {
		shortID = shortID[:7]
	}
	return shortID, nil
}

// ValidateBranchName checks if a branch name is valid using git check-ref-format.
// Returns an error if the name is invalid or contains unsafe characters.
func ValidateBranchName(branchName string) error {
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "git", "check-ref-format", "--branch", branchName)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("invalid branch name %q", branchName)
	}
	return nil
}

// HasUncommittedChanges checks if there are any uncommitted changes in the repository.
// This includes staged changes, unstaged changes, and untracked files.
// Uses git CLI instead of go-git because go-git doesn't respect global gitignore
// (core.excludesfile) which can cause false positives for globally ignored files.
func HasUncommittedChanges() (bool, error) {
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "git", "status", "--porcelain")
	output, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("failed to get git status: %w", err)
	}

	// If output is empty, there are no changes
	return len(strings.TrimSpace(string(output))) > 0, nil
}

// GetConfigValue retrieves a git config value using the git command.
// Returns empty string if the value is not set or on error.
func GetConfigValue(key string) string {
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "git", "config", "--get", key)
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

// FetchBranch fetches a branch from origin and updates the remote tracking ref (refs/remotes/origin/<branch>).
// It does NOT create a local branch - use CreateLocalBranchFromRemote for that.
// Uses git CLI instead of go-git for fetch because go-git doesn't use credential helpers,
// which breaks HTTPS URLs that require authentication.
func FetchBranch(branchName string) error {
	// Validate branch name before using in shell command
	if err := ValidateBranchName(branchName); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	refSpec := fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", branchName, branchName)
	//nolint:gosec // G204: branchName validated above via git check-ref-format
	fetchCmd := exec.CommandContext(ctx, "git", "fetch", "origin", refSpec)
	if output, err := fetchCmd.CombinedOutput(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return errors.New("fetch timed out after 2 minutes")
		}
		return fmt.Errorf("failed to fetch branch from origin: %s: %w", strings.TrimSpace(string(output)), err)
	}

	return nil
}

// CreateLocalBranchFromRemote creates a local branch pointing to the same commit as the remote branch.
// The branch should have been fetched first using FetchBranch.
func CreateLocalBranchFromRemote(branchName string) error {
	repo, err := OpenRepository()
	if err != nil {
		return fmt.Errorf("failed to open repository: %w", err)
	}

	// Get the remote branch reference
	remoteRef, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", branchName), true)
	if err != nil {
		return fmt.Errorf("branch '%s' not found on origin: %w", branchName, err)
	}

	// Create local branch pointing to the same commit
	localRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName(branchName), remoteRef.Hash())
	if err := repo.Storer.SetReference(localRef); err != nil {
		return fmt.Errorf("failed to create local branch: %w", err)
	}

	return nil
}

// HardReset performs a git reset --hard to the specified commit hash string.
// Uses the git CLI instead of go-git because go-git's HardReset incorrectly
// deletes untracked directories (like .entire/) even when they're in .gitignore.
func HardReset(commitHash string) error {
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "git", "reset", "--hard", commitHash)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("reset failed: %s: %w", strings.TrimSpace(string(output)), err)
	}
	return nil
}

// GetGitDirInPath returns the git directory for a repository at the given path.
// It delegates to `git rev-parse --git-dir` to leverage git's own validation.
func GetGitDirInPath(dir string) (string, error) {
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--git-dir")
	cmd.Dir = dir
	output, err := cmd.Output()
	if err != nil {
		return "", errors.New("not a git repository")
	}

	gitDir := strings.TrimSpace(string(output))

	// git rev-parse --git-dir returns relative paths from the working directory,
	// so we need to make it absolute if it isn't already
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(dir, gitDir)
	}

	return filepath.Clean(gitDir), nil
}

// Push pushes a branch to a remote.
// Uses --no-verify to prevent recursive hook calls when used from hooks.
func Push(remote, branchName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "push", "--no-verify", remote, branchName)
	cmd.Stdin = nil // Disconnect stdin to prevent hanging in hook context

	output, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return errors.New("push timed out after 2 minutes")
		}
		// Check if it's a non-fast-forward error
		if strings.Contains(string(output), "non-fast-forward") ||
			strings.Contains(string(output), "rejected") {
			return ErrNonFastForward
		}
		return fmt.Errorf("push failed: %s: %w", strings.TrimSpace(string(output)), err)
	}
	return nil
}

// ErrNonFastForward is returned when a push is rejected due to non-fast-forward.
var ErrNonFastForward = errors.New("non-fast-forward")

// FetchFromRemote fetches a branch from a specified remote.
// Unlike FetchBranch which always uses "origin", this allows specifying the remote.
func FetchFromRemote(remote, branchName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "fetch", remote, branchName)
	cmd.Stdin = nil
	if output, err := cmd.CombinedOutput(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return errors.New("fetch timed out after 2 minutes")
		}
		return fmt.Errorf("fetch failed: %s: %w", strings.TrimSpace(string(output)), err)
	}
	return nil
}
