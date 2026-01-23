package strategy

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"entire.io/cli/cmd/entire/cli/checkpoint"
	"entire.io/cli/cmd/entire/cli/checkpoint/id"
	"entire.io/cli/cmd/entire/cli/logging"
	"entire.io/cli/cmd/entire/cli/paths"
	"entire.io/cli/cmd/entire/cli/session"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

const (
	// sessionGracePeriod is the minimum age a session must have before it can be
	// considered orphaned. This protects active sessions that haven't created
	// their first checkpoint yet.
	sessionGracePeriod = 10 * time.Minute

	// shadowBranchHashLength is the number of hex characters used in shadow branch names.
	// Shadow branches are named "entire/<hash[:7]>" using a 7-char prefix of the commit hash.
	shadowBranchHashLength = 7
)

// CleanupType identifies the type of orphaned item.
type CleanupType string

const (
	CleanupTypeShadowBranch CleanupType = "shadow-branch"
	CleanupTypeSessionState CleanupType = "session-state"
	CleanupTypeCheckpoint   CleanupType = "checkpoint"
)

// CleanupItem represents an orphaned item that can be cleaned up.
type CleanupItem struct {
	Type   CleanupType
	ID     string // Branch name, session ID, or checkpoint ID
	Reason string // Why this item is considered orphaned
}

// CleanupResult contains the results of a cleanup operation.
type CleanupResult struct {
	ShadowBranches    []string // Deleted shadow branches
	SessionStates     []string // Deleted session state files
	Checkpoints       []string // Deleted checkpoint metadata
	FailedBranches    []string // Shadow branches that failed to delete
	FailedStates      []string // Session states that failed to delete
	FailedCheckpoints []string // Checkpoints that failed to delete
}

// shadowBranchPattern matches shadow branch names: entire/<7+ hex chars>
// The pattern requires at least 7 hex characters after "entire/"
var shadowBranchPattern = regexp.MustCompile(`^entire/[0-9a-fA-F]{7,}$`)

// IsShadowBranch returns true if the branch name matches the shadow branch pattern.
// Shadow branches have the format "entire/<commit-hash>" where the commit hash
// is at least 7 hex characters. The "entire/sessions" branch is NOT a shadow branch.
func IsShadowBranch(branchName string) bool {
	// Explicitly exclude entire/sessions
	if branchName == "entire/sessions" {
		return false
	}
	return shadowBranchPattern.MatchString(branchName)
}

// ListShadowBranches returns all shadow branches in the repository.
// Shadow branches match the pattern "entire/<commit-hash>" (7+ hex chars).
// The "entire/sessions" branch is excluded as it stores permanent metadata.
// Returns an empty slice (not nil) if no shadow branches exist.
func ListShadowBranches() ([]string, error) {
	repo, err := OpenRepository()
	if err != nil {
		return nil, fmt.Errorf("failed to open git repository: %w", err)
	}

	refs, err := repo.References()
	if err != nil {
		return nil, fmt.Errorf("failed to get references: %w", err)
	}

	var shadowBranches []string

	err = refs.ForEach(func(ref *plumbing.Reference) error {
		// Only look at branch references
		if !ref.Name().IsBranch() {
			return nil
		}

		// Extract branch name without refs/heads/ prefix
		branchName := strings.TrimPrefix(ref.Name().String(), "refs/heads/")

		if IsShadowBranch(branchName) {
			shadowBranches = append(shadowBranches, branchName)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to iterate references: %w", err)
	}

	// Ensure we return empty slice, not nil
	if shadowBranches == nil {
		shadowBranches = []string{}
	}

	return shadowBranches, nil
}

// DeleteShadowBranches deletes the specified branches from the repository.
// Returns two slices: successfully deleted branches and branches that failed to delete.
// Individual branch deletion failures do not stop the operation - all branches are attempted.
// Returns an error only if the repository cannot be opened.
func DeleteShadowBranches(branches []string) (deleted []string, failed []string, err error) {
	if len(branches) == 0 {
		return []string{}, []string{}, nil
	}

	repo, err := OpenRepository()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open git repository: %w", err)
	}

	for _, branch := range branches {
		refName := plumbing.NewBranchReferenceName(branch)

		// Check if reference exists before trying to delete
		ref, err := repo.Reference(refName, true)
		if err != nil {
			failed = append(failed, branch)
			continue
		}

		// Delete the reference
		if err := repo.Storer.RemoveReference(ref.Name()); err != nil {
			failed = append(failed, branch)
			continue
		}

		deleted = append(deleted, branch)
	}

	return deleted, failed, nil
}

// ListOrphanedSessionStates returns session state files that are orphaned.
// A session state is orphaned if:
//   - No checkpoints on entire/sessions reference this session ID
//   - No shadow branch exists for the session's base commit
//
// This is strategy-agnostic as session states are shared by all strategies.
func ListOrphanedSessionStates() ([]CleanupItem, error) {
	repo, err := OpenRepository()
	if err != nil {
		return nil, fmt.Errorf("failed to open git repository: %w", err)
	}

	// Get all session states
	store, err := session.NewStateStore()
	if err != nil {
		return nil, fmt.Errorf("failed to create state store: %w", err)
	}

	states, err := store.List(context.Background())
	if err != nil {
		return nil, fmt.Errorf("failed to list session states: %w", err)
	}

	if len(states) == 0 {
		return []CleanupItem{}, nil
	}

	// Get all checkpoints to find which sessions have checkpoints
	cpStore := checkpoint.NewGitStore(repo)

	sessionsWithCheckpoints := make(map[string]bool)
	checkpoints, listErr := cpStore.ListCommitted(context.Background())
	if listErr == nil {
		for _, cp := range checkpoints {
			sessionsWithCheckpoints[cp.SessionID] = true
		}
	}

	// Get all shadow branches
	shadowBranches, _ := ListShadowBranches() //nolint:errcheck // Best effort
	shadowBranchSet := make(map[string]bool)
	for _, branch := range shadowBranches {
		// Extract commit hash from branch name (entire/<hash>)
		if strings.HasPrefix(branch, "entire/") {
			hash := strings.TrimPrefix(branch, "entire/")
			shadowBranchSet[hash] = true
		}
	}

	var orphaned []CleanupItem
	now := time.Now()

	for _, state := range states {
		// Skip sessions that started recently - they may be actively in use
		// but haven't created their first checkpoint yet
		if now.Sub(state.StartedAt) < sessionGracePeriod {
			continue
		}

		// Check if session has checkpoints on entire/sessions
		hasCheckpoints := sessionsWithCheckpoints[state.SessionID]

		// Check if shadow branch exists for base commit
		// Shadow branches use 7-char hash prefixes, so we need to match by prefix
		hasShadowBranch := false
		if len(state.BaseCommit) >= shadowBranchHashLength {
			hasShadowBranch = shadowBranchSet[state.BaseCommit[:shadowBranchHashLength]]
		}

		// Session is orphaned if it has no checkpoints AND no shadow branch
		if !hasCheckpoints && !hasShadowBranch {
			reason := "no checkpoints or shadow branch found"
			orphaned = append(orphaned, CleanupItem{
				Type:   CleanupTypeSessionState,
				ID:     state.SessionID,
				Reason: reason,
			})
		}
	}

	return orphaned, nil
}

// DeleteOrphanedSessionStates deletes the specified session state files.
func DeleteOrphanedSessionStates(sessionIDs []string) (deleted []string, failed []string, err error) {
	if len(sessionIDs) == 0 {
		return []string{}, []string{}, nil
	}

	store, err := session.NewStateStore()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create state store: %w", err)
	}

	for _, sessionID := range sessionIDs {
		if err := store.Clear(context.Background(), sessionID); err != nil {
			failed = append(failed, sessionID)
		} else {
			deleted = append(deleted, sessionID)
		}
	}

	return deleted, failed, nil
}

// DeleteOrphanedCheckpoints removes checkpoint directories from the entire/sessions branch.
func DeleteOrphanedCheckpoints(checkpointIDs []string) (deleted []string, failed []string, err error) {
	if len(checkpointIDs) == 0 {
		return []string{}, []string{}, nil
	}

	repo, err := OpenRepository()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open git repository: %w", err)
	}

	// Get sessions branch
	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	ref, err := repo.Reference(refName, true)
	if err != nil {
		return nil, nil, fmt.Errorf("sessions branch not found: %w", err)
	}

	parentCommit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get commit: %w", err)
	}

	baseTree, err := parentCommit.Tree()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get tree: %w", err)
	}

	// Flatten tree to entries
	entries := make(map[string]object.TreeEntry)
	if err := checkpoint.FlattenTree(repo, baseTree, "", entries); err != nil {
		return nil, nil, fmt.Errorf("failed to flatten tree: %w", err)
	}

	// Remove entries for each checkpoint
	checkpointSet := make(map[string]bool)
	for _, id := range checkpointIDs {
		checkpointSet[id] = true
	}

	// Find and remove entries matching checkpoint paths
	for path := range entries {
		for checkpointIDStr := range checkpointSet {
			cpID, err := id.NewCheckpointID(checkpointIDStr)
			if err != nil {
				continue // Skip invalid checkpoint IDs
			}
			cpPath := cpID.Path()
			if strings.HasPrefix(path, cpPath+"/") {
				delete(entries, path)
			}
		}
	}

	// Build new tree
	newTreeHash, err := checkpoint.BuildTreeFromEntries(repo, entries)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to build tree: %w", err)
	}

	// Create commit
	commit := &object.Commit{
		Author: object.Signature{
			Name:  "Entire CLI",
			Email: "cli@entire.io",
			When:  parentCommit.Author.When,
		},
		Committer: object.Signature{
			Name:  "Entire CLI",
			Email: "cli@entire.io",
			When:  parentCommit.Committer.When,
		},
		Message:      fmt.Sprintf("Cleanup: removed %d orphaned checkpoints", len(checkpointIDs)),
		TreeHash:     newTreeHash,
		ParentHashes: []plumbing.Hash{ref.Hash()},
	}

	obj := repo.Storer.NewEncodedObject()
	if err := commit.Encode(obj); err != nil {
		return nil, nil, fmt.Errorf("failed to encode commit: %w", err)
	}

	commitHash, err := repo.Storer.SetEncodedObject(obj)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to store commit: %w", err)
	}

	// Update branch reference
	newRef := plumbing.NewHashReference(refName, commitHash)
	if err := repo.Storer.SetReference(newRef); err != nil {
		return nil, nil, fmt.Errorf("failed to update branch: %w", err)
	}

	// All checkpoints deleted successfully
	return checkpointIDs, []string{}, nil
}

// ListAllCleanupItems returns all orphaned items across all categories.
// It iterates over all registered strategies and calls ListOrphanedItems on those
// that implement OrphanedItemsLister.
// Returns an error if the repository cannot be opened.
func ListAllCleanupItems() ([]CleanupItem, error) {
	var items []CleanupItem
	var firstErr error

	// Iterate over all registered strategies
	for _, name := range List() {
		// Skip legacy names to avoid duplicates
		if IsLegacyStrategyName(name) {
			continue
		}

		strat, err := Get(name)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}

		// Check if strategy implements OrphanedItemsLister
		if lister, ok := strat.(OrphanedItemsLister); ok {
			stratItems, err := lister.ListOrphanedItems()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			items = append(items, stratItems...)
		}
	}

	// Orphaned session states (strategy-agnostic)
	states, err := ListOrphanedSessionStates()
	if err != nil {
		if firstErr == nil {
			firstErr = err
		}
	} else {
		items = append(items, states...)
	}

	return items, firstErr
}

// DeleteAllCleanupItems deletes all specified cleanup items.
// Logs each deletion for audit purposes.
func DeleteAllCleanupItems(items []CleanupItem) (*CleanupResult, error) {
	result := &CleanupResult{}
	logCtx := logging.WithComponent(context.Background(), "cleanup")

	// Build ID-to-Reason map for logging after deletion
	reasonMap := make(map[string]string)
	for _, item := range items {
		reasonMap[item.ID] = item.Reason
	}

	// Group items by type
	var branches, states, checkpoints []string
	for _, item := range items {
		switch item.Type {
		case CleanupTypeShadowBranch:
			branches = append(branches, item.ID)
		case CleanupTypeSessionState:
			states = append(states, item.ID)
		case CleanupTypeCheckpoint:
			checkpoints = append(checkpoints, item.ID)
		}
	}

	// Delete shadow branches
	if len(branches) > 0 {
		deleted, failed, err := DeleteShadowBranches(branches)
		if err != nil {
			return result, err
		}
		result.ShadowBranches = deleted
		result.FailedBranches = failed

		// Log deleted branches
		for _, id := range deleted {
			logging.Info(logCtx, "deleted orphaned shadow branch",
				slog.String("type", string(CleanupTypeShadowBranch)),
				slog.String("id", id),
				slog.String("reason", reasonMap[id]),
			)
		}
		// Log failed branches
		for _, id := range failed {
			logging.Warn(logCtx, "failed to delete orphaned shadow branch",
				slog.String("type", string(CleanupTypeShadowBranch)),
				slog.String("id", id),
				slog.String("reason", reasonMap[id]),
			)
		}
	}

	// Delete session states
	if len(states) > 0 {
		deleted, failed, err := DeleteOrphanedSessionStates(states)
		if err != nil {
			return result, err
		}
		result.SessionStates = deleted
		result.FailedStates = failed

		// Log deleted session states
		for _, id := range deleted {
			logging.Info(logCtx, "deleted orphaned session state",
				slog.String("type", string(CleanupTypeSessionState)),
				slog.String("id", id),
				slog.String("reason", reasonMap[id]),
			)
		}
		// Log failed session states
		for _, id := range failed {
			logging.Warn(logCtx, "failed to delete orphaned session state",
				slog.String("type", string(CleanupTypeSessionState)),
				slog.String("id", id),
				slog.String("reason", reasonMap[id]),
			)
		}
	}

	// Delete checkpoints
	if len(checkpoints) > 0 {
		deleted, failed, err := DeleteOrphanedCheckpoints(checkpoints)
		if err != nil {
			return result, err
		}
		result.Checkpoints = deleted
		result.FailedCheckpoints = failed

		// Log deleted checkpoints
		for _, id := range deleted {
			logging.Info(logCtx, "deleted orphaned checkpoint",
				slog.String("type", string(CleanupTypeCheckpoint)),
				slog.String("id", id),
				slog.String("reason", reasonMap[id]),
			)
		}
		// Log failed checkpoints
		for _, id := range failed {
			logging.Warn(logCtx, "failed to delete orphaned checkpoint",
				slog.String("type", string(CleanupTypeCheckpoint)),
				slog.String("id", id),
				slog.String("reason", reasonMap[id]),
			)
		}
	}

	// Log summary
	totalDeleted := len(result.ShadowBranches) + len(result.SessionStates) + len(result.Checkpoints)
	totalFailed := len(result.FailedBranches) + len(result.FailedStates) + len(result.FailedCheckpoints)
	if totalDeleted > 0 || totalFailed > 0 {
		logging.Info(logCtx, "cleanup completed",
			slog.Int("deleted_branches", len(result.ShadowBranches)),
			slog.Int("deleted_session_states", len(result.SessionStates)),
			slog.Int("deleted_checkpoints", len(result.Checkpoints)),
			slog.Int("failed_branches", len(result.FailedBranches)),
			slog.Int("failed_session_states", len(result.FailedStates)),
			slog.Int("failed_checkpoints", len(result.FailedCheckpoints)),
		)
	}

	return result, nil
}

// MetadataBranchExists checks if the entire/sessions branch exists.
func MetadataBranchExists() bool {
	repo, err := OpenRepository()
	if err != nil {
		return false
	}

	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	_, err = repo.Reference(refName, true)
	return err == nil
}

// DeleteMetadataBranch deletes the entire/sessions branch.
// Returns true if the branch was deleted, false if it didn't exist.
func DeleteMetadataBranch() (bool, error) {
	repo, err := OpenRepository()
	if err != nil {
		return false, err
	}

	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	ref, err := repo.Reference(refName, true)
	if err != nil {
		// Branch doesn't exist
		return false, nil //nolint:nilerr // Branch not existing is not an error
	}

	if err := repo.Storer.RemoveReference(ref.Name()); err != nil {
		return false, fmt.Errorf("failed to delete branch: %w", err)
	}
	return true, nil
}
