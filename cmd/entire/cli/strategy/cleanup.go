package strategy

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// CleanupType identifies the type of item to clean up.
type CleanupType string

const (
	// CleanupTypeShadowBranch is a legacy shadow branch: older CLIs kept each
	// session's in-progress work on a local "entire/<commit>-<worktree>" branch.
	// Nothing writes them anymore; `entire clean` and a one-time pass at session
	// start remove what upgraded repositories still carry.
	CleanupTypeShadowBranch CleanupType = "shadow-branch"
	CleanupTypeSessionState CleanupType = "session-state"
	CleanupTypeCheckpoint   CleanupType = "checkpoint"
	// CleanupTypeRedactCache is the redaction prefix cache in the git common dir.
	// Purely derived data -- it is rebuilt on the next checkpoint -- so it is
	// removed wholesale rather than per entry.
	CleanupTypeRedactCache CleanupType = "redact-cache"
)

// cleanAllReason marks an item discovered by the unfiltered sweep, as opposed
// to one selected by an orphan or staleness rule.
const cleanAllReason = "clean all"

// CleanupItem represents an item that can be cleaned up.
type CleanupItem struct {
	Type   CleanupType
	ID     string // Branch name, session ID, or checkpoint ID
	Reason string // Why this item is being cleaned
}

// CleanupResult contains the results of a cleanup operation.
type CleanupResult struct {
	RedactCaches      []string // Deleted redaction prefix cache directories
	ShadowBranches    []string // Deleted legacy shadow branches
	SessionStates     []string // Deleted session state files
	Checkpoints       []string // Deleted checkpoint metadata
	FailedBranches    []string // Legacy shadow branches that failed to delete
	FailedStates      []string // Session states that failed to delete
	FailedCheckpoints []string // Checkpoints that failed to delete
	FailedRedactCache []string // Redaction caches that failed to delete
}

// legacyShadowBranchPattern matches the shadow branch names older CLIs wrote,
// in both of their formats:
//   - Old format: entire/<commit[:7+]>
//   - Newer format: entire/<commit[:7+]>-<worktreeHash[:6]>
//
// The pattern requires at least 7 hex characters for the commit, optionally followed
// by a dash and exactly 6 hex characters for the worktree hash.
//
// This pattern is name-shape ONLY -- matching it is not proof Entire created the
// branch. In particular the "old format" half (bare "entire/<hex>", no worktree
// suffix) is also a plausible human branch-naming convention (e.g. tracking a
// short commit SHA), and nothing here is namespace-reserved. Use this broad
// pattern only for listing/reporting paths that a human confirms before any
// deletion happens (ListLegacyShadowBranches, ListAllItems, `entire clean
// --all`'s interactive picker). For unattended deletion, use
// isAutoDeletableLegacyShadowBranch instead.
var legacyShadowBranchPattern = regexp.MustCompile(`^entire/[0-9a-fA-F]{7,}(-[0-9a-fA-F]{6})?$`)

// autoDeletableLegacyShadowBranchPattern is the SAME pattern with the
// worktree-hash suffix made mandatory. Every shadow branch the CLI wrote since
// the suffix was introduced has this shape (the worktree hash was a real 6-hex
// value even for the main worktree), and a human branch would have to
// coincidentally match "entire/<7+ hex>-<exactly 6 hex>" to be at risk. The
// bare "entire/<hex>" form is deliberately excluded from unattended deletion
// even though it is Entire's own oldest naming: a human short-SHA branch looks
// exactly like it. Old-format branches stay listed and deletable through the
// interactive `entire clean --all` path, where a human confirms first.
var autoDeletableLegacyShadowBranchPattern = regexp.MustCompile(`^entire/[0-9a-fA-F]{7,}-[0-9a-fA-F]{6}$`)

// IsLegacyShadowBranch reports whether the branch name has the shape of a
// shadow branch written by an older CLI. The metadata and trails branches are
// never shadow branches.
//
// This is a name-shape check, not an ownership check -- see the
// legacyShadowBranchPattern doc comment. Do not use it to gate unattended
// deletion; use isAutoDeletableLegacyShadowBranch for that.
func IsLegacyShadowBranch(branchName string) bool {
	if branchName == paths.MetadataBranchName || branchName == paths.TrailsBranchName {
		return false
	}
	return legacyShadowBranchPattern.MatchString(branchName)
}

// isAutoDeletableLegacyShadowBranch returns true only for the strict,
// worktree-suffixed shadow branch shape; see
// autoDeletableLegacyShadowBranchPattern.
func isAutoDeletableLegacyShadowBranch(branchName string) bool {
	if branchName == paths.MetadataBranchName || branchName == paths.TrailsBranchName {
		return false
	}
	return autoDeletableLegacyShadowBranchPattern.MatchString(branchName)
}

// ListLegacyShadowBranches returns the local branches that look like shadow
// branches written by an older CLI (IsLegacyShadowBranch), sorted. Returns an
// empty slice (not nil) if there are none.
func ListLegacyShadowBranches(ctx context.Context) ([]string, error) {
	heads, err := listLegacyShadowBranchHeads(ctx, IsLegacyShadowBranch)
	if err != nil {
		return nil, err
	}
	return sortedBranchNames(heads), nil
}

func sortedBranchNames(heads map[string]plumbing.Hash) []string {
	branches := make([]string, 0, len(heads))
	for branch := range heads {
		branches = append(branches, branch)
	}
	sort.Strings(branches)
	return branches
}

func listLegacyShadowBranchHeads(ctx context.Context, match func(string) bool) (map[string]plumbing.Hash, error) {
	repo, err := OpenRepository(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to open git repository: %w", err)
	}
	defer repo.Close()

	refs, err := repo.References()
	if err != nil {
		return nil, fmt.Errorf("failed to get references: %w", err)
	}

	heads := map[string]plumbing.Hash{}
	err = refs.ForEach(func(ref *plumbing.Reference) error {
		if err := ctx.Err(); err != nil {
			return err //nolint:wrapcheck // Propagating context cancellation
		}
		if !ref.Name().IsBranch() {
			return nil
		}
		if branchName := ref.Name().Short(); match(branchName) {
			heads[branchName] = ref.Hash()
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to iterate references: %w", err)
	}
	return heads, nil
}

// legacyShadowCleanupMarker records, in the git common dir, that the one-time
// unattended legacy shadow branch cleanup has run for this repository.
const legacyShadowCleanupMarker = "entire-legacy-shadow-branches-removed"

// CleanupLegacyShadowBranches deletes, once per repository, the shadow
// branches older CLIs left behind. The real protection for other people's
// branches is the name: only the strict worktree-suffixed shape
// `entire/<7+hex>-<6hex>` (isAutoDeletableLegacyShadowBranch) is touched.
// Each delete is `git update-ref -d <ref> <observed hash>`, which only guards
// against a branch moving during this pass; a branch that moved fails once,
// leaves the marker unwritten, and is deleted at its new hash on the next
// session start. Nothing reads or writes these branches anymore — session
// work in progress lives in session state — so there is no session to protect.
//
// A branch checked out in any worktree is left alone: `git update-ref -d`,
// unlike `git branch -D`, does not refuse one, and deleting it would leave that
// worktree's HEAD pointing at a missing ref. Someone checked it out on purpose,
// so it is the user's branch now; skipping it is not a failure and does not
// hold back the marker. If the worktree list cannot be read, nothing is
// deleted and the pass is retried next time.
//
// A marker in the git common dir makes the pass one-time: the first call that
// finishes without failures records it, and every later call returns
// immediately. Old-format bare "entire/<hex>" branches are never touched here;
// `entire clean --all` lists them for a human to confirm.
//
// Best-effort: callers log the error and continue.
func CleanupLegacyShadowBranches(ctx context.Context) (int, error) {
	root, err := gitdir.Open(ctx)
	if err != nil {
		return 0, fmt.Errorf("open git common dir: %w", err)
	}
	if _, statErr := root.Lstat(legacyShadowCleanupMarker); statErr == nil {
		return 0, nil
	}

	heads, err := listLegacyShadowBranchHeads(ctx, isAutoDeletableLegacyShadowBranch)
	if err != nil {
		return 0, err
	}
	if len(heads) > 0 {
		checkedOut, listErr := checkedOutBranches(ctx)
		if listErr != nil {
			return 0, listErr
		}
		for branch := range heads {
			if _, held := checkedOut[branch]; held {
				logging.Info(logging.WithComponent(ctx, "cleanup"), "leaving checked-out legacy shadow branch in place",
					slog.String("branch", branch))
				delete(heads, branch)
			}
		}
	}
	deleted, failed := deleteLegacyShadowBranchesIfUnchanged(ctx, heads)
	if len(failed) > 0 {
		return len(deleted), fmt.Errorf("%d legacy shadow branch(es) could not be deleted", len(failed))
	}
	if err := jsonutil.WriteFileAtomicIn(root, legacyShadowCleanupMarker, []byte{}, 0o644); err != nil {
		return len(deleted), fmt.Errorf("record legacy shadow branch cleanup: %w", err)
	}
	return len(deleted), nil
}

// checkedOutBranches returns the short names of the branches checked out in
// any worktree of the current repository, from `git worktree list
// --porcelain`.
func checkedOutBranches(ctx context.Context) (map[string]struct{}, error) {
	out, err := exec.CommandContext(ctx, "git", "worktree", "list", "--porcelain").Output()
	if err != nil {
		return nil, fmt.Errorf("list worktrees: %w", err)
	}
	branches := make(map[string]struct{})
	for _, wt := range gitrepo.ParseWorktreeBranches(string(out)) {
		branches[wt.Branch] = struct{}{}
	}
	return branches, nil
}

// deleteLegacyShadowBranchesIfUnchanged deletes each branch only if it still
// points at the hash the caller observed. That protects only against a
// concurrent move between this pass's scan and its delete (e.g. an older CLI
// version still writing the branch); it is not a lasting exemption, since the
// next pass lists the branch at its new hash.
func deleteLegacyShadowBranchesIfUnchanged(ctx context.Context, branches map[string]plumbing.Hash) (deleted []string, failed []string) {
	for branch, expected := range branches {
		if !isAutoDeletableLegacyShadowBranch(branch) || expected.IsZero() {
			failed = append(failed, branch)
			continue
		}
		cmd := exec.CommandContext(ctx, "git", "update-ref", "-d", "refs/heads/"+branch, expected.String())
		if output, runErr := cmd.CombinedOutput(); runErr != nil {
			logging.Debug(ctx, "legacy shadow branch unchanged-delete skipped",
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

// DeleteLegacyShadowBranches deletes the specified branches from the repository.
// Returns two slices: successfully deleted branches and branches that failed to delete.
// Individual branch deletion failures do not stop the operation - all branches are attempted.
func DeleteLegacyShadowBranches(ctx context.Context, branches []string) (deleted []string, failed []string) {
	if len(branches) == 0 {
		return []string{}, []string{}
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

	return deleted, failed
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

	refs := checkpoint.ResolveRefs(ctx)
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

// ListAllItems returns all Entire items for full cleanup: legacy shadow
// branches left by older CLIs, all session states, and the redaction cache.
func ListAllItems(ctx context.Context) ([]CleanupItem, error) {
	var cleanupItems []CleanupItem

	branches, err := ListLegacyShadowBranches(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing legacy shadow branches: %w", err)
	}
	for _, branch := range branches {
		cleanupItems = append(cleanupItems, CleanupItem{
			Type:   CleanupTypeShadowBranch,
			ID:     branch,
			Reason: cleanAllReason,
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
			Reason: cleanAllReason,
		})
	}

	// The redaction prefix cache accumulates one small entry per session and is
	// never superseded, so without this it would survive every `entire clean`.
	if dir, err := redactCacheDir(ctx); err == nil && dir != "" {
		if _, statErr := os.Stat(dir); statErr == nil {
			cleanupItems = append(cleanupItems, CleanupItem{
				Type:   CleanupTypeRedactCache,
				ID:     checkpoint.RedactCacheDirName,
				Reason: cleanAllReason,
			})
		}
	}

	return cleanupItems, nil
}

// redactCacheDir resolves the redaction prefix cache directory, or "" when the
// git common dir cannot be resolved.
func redactCacheDir(ctx context.Context) (string, error) {
	commonDir, err := session.GetGitCommonDir(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve git common dir: %w", err)
	}
	return filepath.Join(commonDir, checkpoint.RedactCacheDirName), nil
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
	var branches, states, checkpoints, redactCaches []string
	for _, item := range items {
		switch item.Type {
		case CleanupTypeShadowBranch:
			branches = append(branches, item.ID)
		case CleanupTypeSessionState:
			states = append(states, item.ID)
		case CleanupTypeRedactCache:
			redactCaches = append(redactCaches, item.ID)
		case CleanupTypeCheckpoint:
			checkpoints = append(checkpoints, item.ID)
		}
	}

	// Remove the redaction prefix cache. Derived data, so a failure is recorded
	// but never blocks the rest of the cleanup.
	if len(redactCaches) > 0 {
		if err := deleteRedactCache(ctx); err != nil {
			result.FailedRedactCache = redactCaches
			logging.Warn(logCtx, "failed to delete redaction cache",
				slog.String("type", string(CleanupTypeRedactCache)),
				slog.String("error", err.Error()))
		} else {
			result.RedactCaches = redactCaches
			logging.Info(logCtx, "deleted redaction cache",
				slog.String("type", string(CleanupTypeRedactCache)))
		}
	}

	// Delete legacy shadow branches
	if len(branches) > 0 {
		deleted, failed := DeleteLegacyShadowBranches(ctx, branches)
		result.ShadowBranches = deleted
		result.FailedBranches = failed

		// Log deleted branches
		for _, id := range deleted {
			logging.Info(logCtx, "deleted legacy shadow branch",
				slog.String("type", string(CleanupTypeShadowBranch)),
				slog.String("id", id),
				slog.String("reason", reasonMap[id]),
			)
		}
		// Log failed branches
		for _, id := range failed {
			logging.Warn(logCtx, "failed to delete legacy shadow branch",
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

// deleteRedactCache removes the redaction prefix cache directory. Every entry is
// derived data rebuilt on the next checkpoint, so removing the whole directory is
// always safe; a missing directory is not an error.
func deleteRedactCache(ctx context.Context) error {
	dir, err := redactCacheDir(ctx)
	if err != nil {
		return err
	}
	root, err := gitdir.Open(ctx)
	if err != nil {
		return fmt.Errorf("open git common dir: %w", err)
	}
	if err := root.RemoveAll(checkpoint.RedactCacheDirName); err != nil {
		return fmt.Errorf("remove redaction cache %s: %w", dir, err)
	}
	return nil
}
