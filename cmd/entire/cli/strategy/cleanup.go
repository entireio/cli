package strategy

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

const (
	// sessionGracePeriod is the minimum age a session must have before it can be
	// considered orphaned. This protects active sessions that haven't created
	// their first checkpoint yet.
	sessionGracePeriod = 10 * time.Minute
)

// CleanupType identifies the type of item to clean up.
type CleanupType string

const (
	CleanupTypeShadowBranch CleanupType = "shadow-branch"
	CleanupTypeSessionState CleanupType = "session-state"
	CleanupTypeCheckpoint   CleanupType = "checkpoint"
)

// CleanupItem represents an item that can be cleaned up.
type CleanupItem struct {
	Type   CleanupType
	ID     string // Branch name, session ID, or checkpoint ID
	Reason string // Why this item is being cleaned
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

// shadowBranchPattern matches shadow branch names in both old and new formats:
//   - Old format: entire/<commit[:7+]>
//   - New format: entire/<commit[:7+]>-<worktreeHash[:6]>
//
// The pattern requires at least 7 hex characters for the commit, optionally followed
// by a dash and exactly 6 hex characters for the worktree hash.
var shadowBranchPattern = regexp.MustCompile(`^entire/[0-9a-fA-F]{7,}(-[0-9a-fA-F]{6})?$`)

// IsShadowBranch returns true if the branch name matches the shadow branch pattern.
// Shadow branches have the format "entire/<commit-hash>-<worktree-hash>" where the
// commit hash is at least 7 hex characters and worktree hash is 6 hex characters.
// The "entire/checkpoints/v1" branch is NOT a shadow branch.
func IsShadowBranch(branchName string) bool {
	// Explicitly exclude metadata and trails branches
	if branchName == paths.MetadataBranchName || branchName == paths.TrailsBranchName {
		return false
	}
	return shadowBranchPattern.MatchString(branchName)
}

// ListShadowBranches returns all shadow branches in the repository.
// Shadow branches match the pattern "entire/<commit-hash>" (7+ hex chars).
// The "entire/checkpoints/v1" branch is excluded as it stores permanent metadata.
// Returns an empty slice (not nil) if no shadow branches exist.
func ListShadowBranches(ctx context.Context) ([]string, error) {
	heads, err := listShadowBranchHeads(ctx)
	if err != nil {
		return nil, err
	}
	branches := make([]string, 0, len(heads))
	for branch := range heads {
		branches = append(branches, branch)
	}
	sort.Strings(branches)
	return branches, nil
}

func listShadowBranchHeads(ctx context.Context) (map[string]plumbing.Hash, error) {
	repo, err := OpenRepository(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to open git repository: %w", err)
	}
	defer repo.Close()

	refs, err := repo.References()
	if err != nil {
		return nil, fmt.Errorf("failed to get references: %w", err)
	}

	shadowBranches := map[string]plumbing.Hash{}

	err = refs.ForEach(func(ref *plumbing.Reference) error {
		if err := ctx.Err(); err != nil {
			return err //nolint:wrapcheck // Propagating context cancellation
		}
		// Only look at branch references
		if !ref.Name().IsBranch() {
			return nil
		}

		// Extract branch name without refs/heads/ prefix
		branchName := strings.TrimPrefix(ref.Name().String(), "refs/heads/")

		if IsShadowBranch(branchName) {
			shadowBranches[branchName] = ref.Hash()
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to iterate references: %w", err)
	}

	return shadowBranches, nil
}

// CleanupPushedShadowBranches deletes shadow branches whose sessions
// have fully condensed into committed checkpoint metadata (no active
// session referencing them, no pending turn-checkpoints awaiting
// finalization, and no ended-but-uncondensed session still relying on
// shadow-only data). Intended to be called only after a successful push
// so the caller knows any condensed checkpoint data already reached
// the remote.
//
// Returns the count of branches deleted. Failures (e.g., one branch
// fails to delete due to a stale lock) are logged but don't abort
// the operation — remaining branches are still attempted.
//
// Safety properties:
//   - Skips any shadow branch referenced by a session with EndedAt
//     == nil (still active).
//   - Skips any shadow branch whose session has TurnCheckpointIDs
//     pending (mid-finalize race window).
//   - Skips ended sessions until PhaseEnded and FullyCondensed prove the
//     shadow branch contents have been copied to committed metadata.
//   - Multiple sessions can share the same shadow branch (same base
//     commit + worktree); ALL must satisfy the criteria above.
//   - Shadow branches with no associated session state are deleted
//     (no session to lose data from).
func CleanupPushedShadowBranches(ctx context.Context) (int, error) {
	branchHeads, err := listShadowBranchHeads(ctx)
	if err != nil {
		return 0, fmt.Errorf("list shadow branches: %w", err)
	}
	if len(branchHeads) == 0 {
		return 0, nil
	}

	states, err := ListSessionStates(ctx)
	if err != nil {
		return 0, fmt.Errorf("list session states: %w", err)
	}

	// Build a set of shadow branch names that must be preserved
	// because at least one session still depends on them.
	protected := map[string]bool{}
	for _, s := range states {
		shadow, ok := protectedShadowBranchForSession(s)
		if ok {
			protected[shadow] = true
		}
	}

	toDelete := map[string]plumbing.Hash{}
	for b, hash := range branchHeads {
		if !protected[b] {
			toDelete[b] = hash
		}
	}
	if len(toDelete) == 0 {
		return 0, nil
	}

	deleted, failed := DeleteShadowBranchesIfUnchanged(ctx, toDelete)
	if len(failed) > 0 {
		logging.Warn(ctx, "some shadow branches failed to delete during post-push cleanup",
			slog.Int("failed_count", len(failed)),
			slog.Int("deleted_count", len(deleted)),
		)
	}
	return len(deleted), nil
}

// DeleteShadowBranchesIfUnchanged deletes shadow branches only if each branch
// still points at the hash observed by the caller. This avoids deleting a
// branch that another session advanced after cleanup's initial scan.
func DeleteShadowBranchesIfUnchanged(ctx context.Context, branches map[string]plumbing.Hash) (deleted []string, failed []string) {
	if len(branches) == 0 {
		return []string{}, []string{}
	}
	for branch, expected := range branches {
		if !IsShadowBranch(branch) || expected.IsZero() {
			failed = append(failed, branch)
			continue
		}
		protected, err := shadowBranchProtectedByCurrentState(ctx, branch)
		if err != nil {
			logging.Debug(ctx, "shadow branch unchanged-delete skipped after protection recheck failed",
				slog.String("branch", branch),
				slog.String("error", err.Error()),
			)
			failed = append(failed, branch)
			continue
		}
		if protected {
			logging.Debug(ctx, "shadow branch unchanged-delete skipped because current session state protects it",
				slog.String("branch", branch),
			)
			failed = append(failed, branch)
			continue
		}
		ref := "refs/heads/" + branch
		cmd := exec.CommandContext(ctx, "git", "update-ref", "-d", ref, expected.String())
		if output, runErr := cmd.CombinedOutput(); runErr != nil {
			logging.Debug(ctx, "shadow branch unchanged-delete skipped",
				slog.String("branch", branch),
				slog.String("expected", expected.String()),
				slog.String("output", strings.TrimSpace(string(output))),
				slog.String("error", runErr.Error()),
			)
			failed = append(failed, branch)
			continue
		}
		deleted = append(deleted, branch)
	}
	return deleted, failed
}

func protectedShadowBranchForSession(s *SessionState) (string, bool) {
	if s.Phase == session.PhaseEnded && s.FullyCondensed && len(s.TurnCheckpointIDs) == 0 {
		return "", false // safe — session ended cleanly and finalized
	}
	return getShadowBranchNameForCommit(s.BaseCommit, s.WorktreeID), true
}

func shadowBranchProtectedByCurrentState(ctx context.Context, branch string) (bool, error) {
	states, err := ListSessionStates(ctx)
	if err != nil {
		return false, err
	}
	for _, s := range states {
		shadow, ok := protectedShadowBranchForSession(s)
		if ok && shadow == branch {
			return true, nil
		}
	}
	return false, nil
}

// DeleteShadowBranches deletes the specified branches from the repository.
// Returns two slices: successfully deleted branches and branches that failed to delete.
// Individual branch deletion failures do not stop the operation - all branches are attempted.
func DeleteShadowBranches(ctx context.Context, branches []string) (deleted []string, failed []string, err error) { //nolint:unparam // already present in codebase
	if len(branches) == 0 {
		return []string{}, []string{}, nil
	}

	for _, branch := range branches {
		// Use git CLI to delete branches because go-git v5's RemoveReference
		// doesn't properly persist deletions with packed refs or worktrees
		if err := DeleteBranchCLI(ctx, branch); err != nil {
			failed = append(failed, branch)
			continue
		}

		deleted = append(deleted, branch)
	}

	return deleted, failed, nil
}

// ListOrphanedSessionStates returns session state files that are orphaned.
// A session state is orphaned if:
//   - No checkpoints on the configured committed read ref reference this session ID
//   - No shadow branch exists for the session's base commit
//
// This is strategy-agnostic as session states are shared by all strategies.
func ListOrphanedSessionStates(ctx context.Context) ([]CleanupItem, error) {
	repo, err := OpenRepository(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to open git repository: %w", err)
	}
	defer repo.Close()

	// Get all session states
	store, err := session.NewStateStore(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create state store: %w", err)
	}

	states, err := store.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list session states: %w", err)
	}

	if len(states) == 0 {
		return []CleanupItem{}, nil
	}

	// Get all committed checkpoints from the configured read ref to find which sessions have checkpoints
	cpStores, err := checkpoint.Open(ctx, repo, checkpoint.OpenOptions{})
	if err != nil {
		return nil, fmt.Errorf("open checkpoint store: %w", err)
	}

	sessionsWithCheckpoints := make(map[string]bool)
	checkpoints, listErr := cpStores.Primary.ListCommitted(ctx)
	if listErr == nil {
		for _, cp := range checkpoints {
			// cp.SessionID is the most-recent session in a multi-session checkpoint;
			// cp.SessionIDs lists every session that contributed. Track all of them so
			// archived sessions of condensed checkpoints aren't flagged as orphaned.
			sessionsWithCheckpoints[cp.SessionID] = true
			for _, sid := range cp.SessionIDs {
				sessionsWithCheckpoints[sid] = true
			}
		}
	}

	// Get all shadow branches as a set for quick lookup
	shadowBranches, _ := ListShadowBranches(ctx) //nolint:errcheck // Best effort
	shadowBranchSet := make(map[string]bool)
	for _, branch := range shadowBranches {
		shadowBranchSet[branch] = true
	}

	var orphaned []CleanupItem
	now := time.Now()

	for _, state := range states {
		// Skip sessions that started recently - they may be actively in use
		// but haven't created their first checkpoint yet
		if now.Sub(state.StartedAt) < sessionGracePeriod {
			continue
		}

		// Check if session has checkpoints in committed checkpoint storage
		hasCheckpoints := sessionsWithCheckpoints[state.SessionID]

		// Check if shadow branch exists for this session's base commit and worktree
		// Shadow branches are now worktree-specific: entire/<commit[:7]>-<worktreeHash[:6]>
		expectedBranch := checkpoint.ShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)
		hasShadowBranch := shadowBranchSet[expectedBranch]

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
func DeleteOrphanedSessionStates(ctx context.Context, sessionIDs []string) (deleted []string, failed []string, err error) {
	if len(sessionIDs) == 0 {
		return []string{}, []string{}, nil
	}

	store, err := session.NewStateStore(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create state store: %w", err)
	}

	for _, sessionID := range sessionIDs {
		if err := store.Clear(ctx, sessionID); err != nil {
			failed = append(failed, sessionID)
		} else {
			deleted = append(deleted, sessionID)
		}
	}

	return deleted, failed, nil
}

// DeleteOrphanedCheckpoints removes checkpoint directories from the entire/checkpoints/v1 branch.
func DeleteOrphanedCheckpoints(ctx context.Context, checkpointIDs []string) (deleted []string, failed []string, err error) {
	if len(checkpointIDs) == 0 {
		return []string{}, []string{}, nil
	}

	repo, err := OpenRepository(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open git repository: %w", err)
	}
	defer repo.Close()

	refs := checkpoint.ResolveCommittedRefs(ctx)
	ref, err := repo.Reference(refs.Primary, true)
	if err != nil {
		return nil, nil, fmt.Errorf("primary metadata ref %s not found: %w", refs.Primary, err)
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
	newTreeHash, err := checkpoint.BuildTreeFromEntries(ctx, repo, entries)
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

	if err := setRefHash(repo, refs.Primary, commitHash); err != nil {
		return nil, nil, fmt.Errorf("failed to update branch: %w", err)
	}

	// All checkpoints deleted successfully
	return checkpointIDs, []string{}, nil
}

// ListAllItems returns all Entire items for full cleanup.
// This includes all shadow branches and all session states regardless of
// whether they have checkpoints or active shadow branches.
func ListAllItems(ctx context.Context) ([]CleanupItem, error) {
	var cleanupItems []CleanupItem

	// All shadow branches (using ListShadowBranches directly, not
	// ListOrphanedItems, so this won't break if orphan filtering is added)
	branches, err := ListShadowBranches(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing shadow branches: %w", err)
	}
	for _, branch := range branches {
		cleanupItems = append(cleanupItems, CleanupItem{
			Type:   CleanupTypeShadowBranch,
			ID:     branch,
			Reason: "clean all",
		})
	}

	// All session states (not just orphaned)
	store, err := session.NewStateStore(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create state store: %w", err)
	}

	states, err := store.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list session states: %w", err)
	}

	for _, state := range states {
		cleanupItems = append(cleanupItems, CleanupItem{
			Type:   CleanupTypeSessionState,
			ID:     state.SessionID,
			Reason: "clean all",
		})
	}

	return cleanupItems, nil
}

// DeleteAllCleanupItems deletes all specified cleanup items.
// Logs each deletion for audit purposes.
func DeleteAllCleanupItems(ctx context.Context, items []CleanupItem) (*CleanupResult, error) {
	result := &CleanupResult{}
	logCtx := logging.WithComponent(ctx, "cleanup")

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
		deleted, failed, err := DeleteShadowBranches(ctx, branches)
		if err != nil {
			return result, err
		}
		result.ShadowBranches = deleted
		result.FailedBranches = failed

		// Log deleted branches
		for _, id := range deleted {
			logging.Info(logCtx, "deleted shadow branch",
				slog.String("type", string(CleanupTypeShadowBranch)),
				slog.String("id", id),
				slog.String("reason", reasonMap[id]),
			)
		}
		// Log failed branches
		for _, id := range failed {
			logging.Warn(logCtx, "failed to delete shadow branch",
				slog.String("type", string(CleanupTypeShadowBranch)),
				slog.String("id", id),
				slog.String("reason", reasonMap[id]),
			)
		}
	}

	// Delete session states
	if len(states) > 0 {
		deleted, failed, err := DeleteOrphanedSessionStates(ctx, states)
		if err != nil {
			return result, err
		}
		result.SessionStates = deleted
		result.FailedStates = failed

		// Log deleted session states
		for _, id := range deleted {
			logging.Info(logCtx, "deleted session state",
				slog.String("type", string(CleanupTypeSessionState)),
				slog.String("id", id),
				slog.String("reason", reasonMap[id]),
			)
		}
		// Log failed session states
		for _, id := range failed {
			logging.Warn(logCtx, "failed to delete session state",
				slog.String("type", string(CleanupTypeSessionState)),
				slog.String("id", id),
				slog.String("reason", reasonMap[id]),
			)
		}
	}

	// Delete checkpoints
	if len(checkpoints) > 0 {
		deleted, failed, err := DeleteOrphanedCheckpoints(ctx, checkpoints)
		if err != nil {
			return result, err
		}
		result.Checkpoints = deleted
		result.FailedCheckpoints = failed

		// Log deleted checkpoints
		for _, id := range deleted {
			logging.Info(logCtx, "deleted checkpoint",
				slog.String("type", string(CleanupTypeCheckpoint)),
				slog.String("id", id),
				slog.String("reason", reasonMap[id]),
			)
		}
		// Log failed checkpoints
		for _, id := range failed {
			logging.Warn(logCtx, "failed to delete checkpoint",
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
