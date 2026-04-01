package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
)

// openRepository opens the git repository with linked worktree support enabled.
// This is a convenience wrapper around strategy.OpenRepository() for use in the CLI package.
func openRepository(ctx context.Context) (*git.Repository, error) {
	repo, err := strategy.OpenRepository(ctx)
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
func GetGitAuthor(ctx context.Context) (*GitAuthor, error) {
	repo, err := openRepository(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to open git repository: %w", err)
	}

	name, email := strategy.GetGitAuthorFromRepo(repo)

	// If go-git returned defaults, try using git command as fallback
	// This handles cases where go-git can't find the config (e.g., different HOME paths,
	// non-standard config locations, or environment issues in hook contexts)
	if name == "Unknown" {
		if gitName := getGitConfigValue(ctx, "user.name"); gitName != "" {
			name = gitName
		}
	}
	if email == "unknown@local" {
		if gitEmail := getGitConfigValue(ctx, "user.email"); gitEmail != "" {
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
func getGitConfigValue(ctx context.Context, key string) string {
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
func IsOnDefaultBranch(ctx context.Context) (bool, string, error) {
	repo, err := openRepository(ctx)
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
func ShouldSkipOnDefaultBranch(ctx context.Context) (bool, string) {
	isDefault, branchName, err := IsOnDefaultBranch(ctx)
	if err != nil {
		// If we can't determine, allow the operation
		return false, ""
	}
	return isDefault, branchName
}

// GetCurrentBranch returns the name of the current branch.
// Returns an error if in detached HEAD state or if not in a git repository.
func GetCurrentBranch(ctx context.Context) (string, error) {
	repo, err := openRepository(ctx)
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
func GetMergeBase(ctx context.Context, branch1, branch2 string) (*plumbing.Hash, error) {
	repo, err := openRepository(ctx)
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
// Uses git CLI instead of go-git because go-git doesn't respect global gitignore
// (core.excludesfile) which can cause false positives for globally ignored files.
func HasUncommittedChanges(ctx context.Context) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "status", "--porcelain")
	output, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("failed to get git status: %w", err)
	}

	// If output is empty, there are no changes
	return len(strings.TrimSpace(string(output))) > 0, nil
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
// First checks local remote-tracking refs, then queries the actual remote
// via git ls-remote in case local refs are stale (e.g., after a fresh clone
// that didn't fetch all branches).
func BranchExistsOnRemote(ctx context.Context, branchName string) (bool, error) {
	repo, err := openRepository(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to open git repository: %w", err)
	}

	// Check for remote reference: refs/remotes/origin/<branchName>
	_, err = repo.Reference(plumbing.NewRemoteReferenceName("origin", branchName), true)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, plumbing.ErrReferenceNotFound) {
		return false, fmt.Errorf("failed to check remote branch: %w", err)
	}

	// Local remote-tracking ref not found — query the actual remote.
	lsCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	lsCmd := exec.CommandContext(lsCtx, "git", "ls-remote", "--heads", "origin", "refs/heads/"+branchName)
	output, lsErr := lsCmd.Output()
	if lsErr != nil {
		// ls-remote failed (no network, no remote, etc.) — treat as not found
		return false, nil
	}

	return len(bytes.TrimSpace(output)) > 0, nil
}

// BranchExistsLocally checks if a local branch exists.
func BranchExistsLocally(ctx context.Context, branchName string) (bool, error) {
	repo, err := openRepository(ctx)
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
func CheckoutBranch(ctx context.Context, ref string) error {
	if strings.HasPrefix(ref, "-") {
		return fmt.Errorf("checkout failed: invalid ref %q", ref)
	}
	cmd := exec.CommandContext(ctx, "git", "checkout", ref)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("checkout failed: %s: %w", strings.TrimSpace(string(output)), err)
	}
	return nil
}

// ValidateBranchName checks if a branch name is valid using git check-ref-format.
// Returns an error if the name is invalid or contains unsafe characters.
func ValidateBranchName(ctx context.Context, branchName string) error {
	cmd := exec.CommandContext(ctx, "git", "check-ref-format", "--branch", branchName)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("invalid branch name %q", branchName)
	}
	return nil
}

// FetchAndCheckoutRemoteBranch fetches a branch from origin and creates a local tracking branch.
// Uses git CLI instead of go-git for fetch because go-git doesn't use credential helpers,
// which breaks HTTPS URLs that require authentication.
func FetchAndCheckoutRemoteBranch(ctx context.Context, branchName string) error {
	// Validate branch name before using in shell command (branchName comes from user CLI input)
	if err := ValidateBranchName(ctx, branchName); err != nil {
		return err
	}

	// Use git CLI for fetch (go-git's fetch can be tricky with auth)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	refSpec := fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", branchName, branchName)

	fetchCmd := strategy.CheckpointGitCommand(ctx, "origin", "fetch", "origin", refSpec)
	if output, err := fetchCmd.CombinedOutput(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return errors.New("fetch timed out after 2 minutes")
		}
		return fmt.Errorf("failed to fetch branch from origin: %s: %w", strings.TrimSpace(string(output)), err)
	}

	repo, err := openRepository(ctx)
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
	err = repo.Storer.SetReference(localRef)
	if err != nil {
		return fmt.Errorf("failed to create local branch: %w", err)
	}

	// Checkout the new local branch
	return CheckoutBranch(ctx, branchName)
}

// FetchMetadataBranch fetches the entire/checkpoints/v1 branch from origin and creates/updates the local branch.
// This is used when the metadata branch exists on remote but not locally.
// Uses git CLI instead of go-git for fetch because go-git doesn't use credential helpers,
// which breaks HTTPS URLs that require authentication.
func FetchMetadataBranch(ctx context.Context) error {
	branchName := paths.MetadataBranchName

	// Use git CLI for fetch (go-git's fetch can be tricky with auth)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	refSpec := fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", branchName, branchName)

	fetchCmd := strategy.CheckpointGitCommand(ctx, "origin", "fetch", "origin", refSpec)
	if output, err := fetchCmd.CombinedOutput(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return errors.New("fetch timed out after 2 minutes")
		}
		return fmt.Errorf("failed to fetch %s from origin: %s: %w", branchName, strings.TrimSpace(string(output)), err)
	}

	repo, err := openRepository(ctx)
	if err != nil {
		return fmt.Errorf("failed to open repository: %w", err)
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

// FetchMetadataTreeOnly fetches the tip of the entire/checkpoints/v1 branch
// from origin with --depth=1 --filter=blob:none, downloading only the latest
// commit and its tree objects (no blobs, no history).
// After this call, tree navigation via go-git works but blob reads will fail
// for objects that weren't previously fetched.
// Uses git CLI for credential helper support.
func FetchMetadataTreeOnly(ctx context.Context) error {
	branchName := paths.MetadataBranchName

	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	refSpec := fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", branchName, branchName)

	fetchCmd := strategy.CheckpointGitCommand(ctx, "origin", "fetch", "--depth=1", "--filter=blob:none", "origin", refSpec)
	if output, err := fetchCmd.CombinedOutput(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return errors.New("treeless fetch timed out after 2 minutes")
		}
		return fmt.Errorf("failed to treeless-fetch %s from origin: %s: %w", branchName, strings.TrimSpace(string(output)), err)
	}

	repo, err := openRepository(ctx)
	if err != nil {
		return fmt.Errorf("failed to open repository: %w", err)
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

// FetchBlobsByHash fetches specific blob objects from the remote by their SHA-1 hashes.
// Uses "git fetch origin <hash>" which goes through normal credential helpers,
// unlike fetch-pack which bypasses them. Requires the server to support
// uploadpack.allowReachableSHA1InWant (GitHub, GitLab, Bitbucket all do).
//
// If fetching by hash fails, falls back to a full metadata branch fetch.
func FetchBlobsByHash(ctx context.Context, hashes []plumbing.Hash) error {
	if len(hashes) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	// Build fetch args: "git fetch origin <hash1> <hash2> ..."
	// This uses the normal transport + credential helpers, unlike fetch-pack.
	args := []string{"fetch", "--no-write-fetch-head", "origin"}
	for _, h := range hashes {
		args = append(args, h.String())
	}

	fetchCmd := strategy.CheckpointGitCommand(ctx, "origin", args...)
	if _, fetchErr := fetchCmd.CombinedOutput(); fetchErr != nil {
		logging.Debug(ctx, "fetch-by-hash failed, falling back to full metadata fetch",
			slog.Int("blob_count", len(hashes)),
			slog.String("error", fetchErr.Error()),
		)
		// Fallback: full metadata branch fetch (pack negotiation skips already-local objects)
		if fallbackErr := FetchMetadataBranch(ctx); fallbackErr != nil {
			return fmt.Errorf("fetch-by-hash failed: %w; fallback fetch also failed: %w",
				fetchErr, fallbackErr)
		}
	}

	return nil
}
