package strategy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"entire.io/cli/cmd/entire/cli/checkpoint"
	"entire.io/cli/cmd/entire/cli/paths"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// pushSessionsBranchCommon is the shared implementation for pushing session branches.
// Used by both manual-commit and auto-commit strategies.
// Configuration options (stored in .entire/settings.json under strategy_options.push_sessions):
//   - "auto": always push automatically
//   - "prompt" (default): ask user with option to enable auto
//   - "false"/"off"/"no": never push
func pushSessionsBranchCommon(remote, branchName string) error {
	repo, err := OpenRepository()
	if err != nil {
		return nil //nolint:nilerr // Hook must be silent on failure
	}

	// Check if branch exists locally
	branchRef := plumbing.NewBranchReferenceName(branchName)
	localRef, err := repo.Reference(branchRef, true)
	if err != nil {
		// No branch, nothing to push
		return nil //nolint:nilerr // Expected when no sessions exist yet
	}

	// Check if there's actually something to push (local differs from remote)
	if !hasUnpushedSessionsCommon(repo, remote, localRef.Hash(), branchName) {
		// Nothing to push - skip silently
		return nil
	}

	// Get configuration
	pushMode := getPushSessionsConfig()

	switch pushMode {
	case "false", "off", "no":
		return nil
	case "auto":
		return doPushSessionsBranch(remote, branchName)
	default:
		// "prompt" or any other value - ask user
		return promptAndPushSessionsCommon(remote, branchName)
	}
}

// hasUnpushedSessionsCommon checks if the local branch differs from the remote.
// Returns true if there's any difference that needs syncing (local ahead, remote ahead, or diverged).
func hasUnpushedSessionsCommon(repo *git.Repository, remote string, localHash plumbing.Hash, branchName string) bool {
	// Check for remote tracking ref: refs/remotes/<remote>/<branch>
	remoteRefName := plumbing.NewRemoteReferenceName(remote, branchName)
	remoteRef, err := repo.Reference(remoteRefName, true)
	if err != nil {
		// Remote branch doesn't exist yet - we have content to push
		return true
	}

	// If local and remote point to same commit, nothing to sync
	// This is the only case where we skip - any difference needs handling
	return localHash != remoteRef.Hash()
}

// getPushSessionsConfig reads the push_sessions setting from strategy options.
// Checks settings.local.json first (user preference), then settings.json (shared).
func getPushSessionsConfig() string {
	// Use repo root to find settings files when run from a subdirectory
	localSettingsPath, err := paths.AbsPath(".entire/settings.local.json")
	if err != nil {
		localSettingsPath = ".entire/settings.local.json" // Fallback
	}
	sharedSettingsPath, err := paths.AbsPath(".entire/settings.json")
	if err != nil {
		sharedSettingsPath = ".entire/settings.json" // Fallback
	}

	// Try local settings first (user preference, not committed)
	if val := readPushSessionsFromFile(localSettingsPath); val != "" {
		return val
	}

	// Fall back to shared settings
	if val := readPushSessionsFromFile(sharedSettingsPath); val != "" {
		return val
	}

	return pushSessionsPrompt
}

// readPushSessionsFromFile reads push_sessions from a specific settings file.
func readPushSessionsFromFile(settingsPath string) string {
	//nolint:gosec // G304: settingsPath is always a hardcoded constant from this package
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return ""
	}

	var settings struct {
		StrategyOptions map[string]interface{} `json:"strategy_options"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return ""
	}

	if settings.StrategyOptions == nil {
		return ""
	}

	if val, ok := settings.StrategyOptions["push_sessions"].(string); ok {
		return val
	}
	return ""
}

// setPushSessionsConfig saves the push_sessions setting to settings.local.json.
// This is a user preference that should not be committed to the repository.
func setPushSessionsConfig(value string) error {
	// Use repo root to find settings file when run from a subdirectory
	localSettingsFile, err := paths.AbsPath(".entire/settings.local.json")
	if err != nil {
		localSettingsFile = ".entire/settings.local.json" // Fallback
	}

	// Read existing local settings or start fresh
	var settings map[string]interface{}
	data, err := os.ReadFile(localSettingsFile) //nolint:gosec // Path is controlled
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("failed to read local settings: %w", err)
		}
		// File doesn't exist, start with empty settings
		settings = make(map[string]interface{})
	} else {
		if err := json.Unmarshal(data, &settings); err != nil {
			return fmt.Errorf("failed to parse local settings: %w", err)
		}
	}

	// Update strategy_options
	strategyOptions, ok := settings["strategy_options"].(map[string]interface{})
	if !ok || strategyOptions == nil {
		strategyOptions = make(map[string]interface{})
	}
	strategyOptions["push_sessions"] = value
	settings["strategy_options"] = strategyOptions

	newData, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to serialize local settings: %w", err)
	}

	//nolint:gosec // G306: Settings file needs to be readable by user
	if err := os.WriteFile(localSettingsFile, newData, 0o644); err != nil {
		return fmt.Errorf("failed to write local settings: %w", err)
	}
	return nil
}

// promptAndPushSessionsCommon prompts the user and optionally pushes the sessions branch.
func promptAndPushSessionsCommon(remote, branchName string) error {
	// Open /dev/tty for interactive input in hook context
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		// Can't prompt (non-interactive), skip silently
		return nil
	}
	defer tty.Close()

	fmt.Fprintf(tty, "\n[entire] Push session logs to %s?\n", remote)
	fmt.Fprintf(tty, "  1) Yes, and always do this automatically\n")
	fmt.Fprintf(tty, "  2) Yes\n")
	fmt.Fprintf(tty, "  3) No\n")
	fmt.Fprintf(tty, "Choice [1]: ")

	// Read response with timeout to avoid hanging indefinitely
	type readResult struct {
		data []byte
		err  error
	}
	resultCh := make(chan readResult, 1)

	go func() {
		buf := make([]byte, 10)
		n, err := tty.Read(buf)
		resultCh <- readResult{data: buf[:n], err: err}
	}()

	const ttyReadTimeout = 60 * time.Second
	var choice string
	select {
	case result := <-resultCh:
		if result.err != nil {
			return nil
		}
		choice = strings.TrimSpace(string(result.data))
	case <-time.After(ttyReadTimeout):
		fmt.Fprintf(tty, "\n[entire] Timed out waiting for input, skipping.\n")
		return nil
	}

	if choice == "" {
		choice = "1" // Default
	}

	switch choice {
	case "1":
		// Save preference and push
		if err := setPushSessionsConfig("auto"); err != nil {
			fmt.Fprintf(tty, "[entire] Warning: couldn't save preference: %v\n", err)
		} else {
			fmt.Fprintf(tty, "[entire] Preference saved. Future pushes will be automatic.\n")
		}
		return doPushSessionsBranch(remote, branchName)
	case "2":
		return doPushSessionsBranch(remote, branchName)
	case "3":
		fmt.Fprintf(tty, "[entire] Skipping session logs push.\n")
		return nil
	default:
		fmt.Fprintf(tty, "[entire] Invalid choice, skipping.\n")
		return nil
	}
}

// doPushSessionsBranch pushes the sessions branch to the remote.
func doPushSessionsBranch(remote, branchName string) error {
	fmt.Fprintf(os.Stderr, "[entire] Pushing session logs to %s...\n", remote)

	// Try pushing first
	if err := tryPushSessionsCommon(remote, branchName); err == nil {
		return nil
	}

	// Push failed - likely non-fast-forward. Try to fetch and merge.
	fmt.Fprintf(os.Stderr, "[entire] Syncing with remote session logs...\n")

	if err := fetchAndMergeSessionsCommon(remote, branchName); err != nil {
		fmt.Fprintf(os.Stderr, "[entire] Warning: couldn't sync sessions: %v\n", err)
		return nil // Don't fail the main push
	}

	// Try pushing again after merge
	if err := tryPushSessionsCommon(remote, branchName); err != nil {
		fmt.Fprintf(os.Stderr, "[entire] Warning: failed to push sessions after sync: %v\n", err)
	}

	return nil
}

// tryPushSessionsCommon attempts to push the sessions branch.
func tryPushSessionsCommon(remote, branchName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Use --no-verify to prevent recursive hook calls
	cmd := exec.CommandContext(ctx, "git", "push", "--no-verify", remote, branchName)
	cmd.Stdin = nil // Disconnect stdin to prevent hanging in hook context

	output, err := cmd.CombinedOutput()
	if err != nil {
		// Check if it's a non-fast-forward error (we can try to recover)
		if strings.Contains(string(output), "non-fast-forward") ||
			strings.Contains(string(output), "rejected") {
			return errors.New("non-fast-forward")
		}
		return fmt.Errorf("push failed: %s", output)
	}
	return nil
}

// fetchAndMergeSessionsCommon fetches remote sessions and merges into local using go-git.
// Since session logs are append-only (unique cond-* directories), we just combine trees.
func fetchAndMergeSessionsCommon(remote, branchName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Use git CLI for fetch (go-git's fetch can be tricky with auth)
	fetchCmd := exec.CommandContext(ctx, "git", "fetch", remote, branchName)
	fetchCmd.Stdin = nil
	if output, err := fetchCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("fetch failed: %s", output)
	}

	repo, err := OpenRepository()
	if err != nil {
		return fmt.Errorf("failed to open git repository: %w", err)
	}

	// Get local branch
	localRef, err := repo.Reference(plumbing.NewBranchReferenceName(branchName), true)
	if err != nil {
		return fmt.Errorf("failed to get local ref: %w", err)
	}
	localCommit, err := repo.CommitObject(localRef.Hash())
	if err != nil {
		return fmt.Errorf("failed to get local commit: %w", err)
	}
	localTree, err := localCommit.Tree()
	if err != nil {
		return fmt.Errorf("failed to get local tree: %w", err)
	}

	// Get remote (FETCH_HEAD)
	fetchHeadRef, err := repo.Reference(plumbing.ReferenceName("FETCH_HEAD"), true)
	if err != nil {
		return fmt.Errorf("failed to get FETCH_HEAD: %w", err)
	}
	remoteCommit, err := repo.CommitObject(fetchHeadRef.Hash())
	if err != nil {
		return fmt.Errorf("failed to get remote commit: %w", err)
	}
	remoteTree, err := remoteCommit.Tree()
	if err != nil {
		return fmt.Errorf("failed to get remote tree: %w", err)
	}

	// Flatten both trees and combine entries
	// Session logs have unique cond-* directories, so no conflicts expected
	entries := make(map[string]object.TreeEntry)
	if err := checkpoint.FlattenTree(repo, localTree, "", entries); err != nil {
		return fmt.Errorf("failed to flatten local tree: %w", err)
	}
	if err := checkpoint.FlattenTree(repo, remoteTree, "", entries); err != nil {
		return fmt.Errorf("failed to flatten remote tree: %w", err)
	}

	// Build merged tree
	mergedTreeHash, err := checkpoint.BuildTreeFromEntries(repo, entries)
	if err != nil {
		return fmt.Errorf("failed to build merged tree: %w", err)
	}

	// Create merge commit with both parents
	mergeCommitHash, err := createMergeCommitCommon(repo, mergedTreeHash,
		[]plumbing.Hash{localRef.Hash(), fetchHeadRef.Hash()},
		"Merge remote session logs")
	if err != nil {
		return fmt.Errorf("failed to create merge commit: %w", err)
	}

	// Update branch ref
	newRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName(branchName), mergeCommitHash)
	if err := repo.Storer.SetReference(newRef); err != nil {
		return fmt.Errorf("failed to update branch ref: %w", err)
	}

	return nil
}

// createMergeCommitCommon creates a merge commit with multiple parents.
func createMergeCommitCommon(repo *git.Repository, treeHash plumbing.Hash, parents []plumbing.Hash, message string) (plumbing.Hash, error) {
	authorName, authorEmail := GetGitAuthorFromRepo(repo)
	now := time.Now()
	sig := object.Signature{
		Name:  authorName,
		Email: authorEmail,
		When:  now,
	}

	commit := &object.Commit{
		TreeHash:     treeHash,
		ParentHashes: parents,
		Author:       sig,
		Committer:    sig,
		Message:      message,
	}

	obj := repo.Storer.NewEncodedObject()
	if err := commit.Encode(obj); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to encode commit: %w", err)
	}

	hash, err := repo.Storer.SetEncodedObject(obj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to store commit: %w", err)
	}

	return hash, nil
}
