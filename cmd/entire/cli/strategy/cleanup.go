package strategy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/settings"

	"github.com/go-git/go-git/v6"
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
	CleanupTypeV2Generation CleanupType = "v2-generation"
)

// CleanupItem represents an item that can be cleaned up.
type CleanupItem struct {
	Type   CleanupType
	ID     string // Branch name, session ID, or checkpoint ID
	RefOID string // For ref-based items: the OID observed at listing time (compare-and-swap)
	Reason string // Why this item is being cleaned
}

// CleanupResult contains the results of a cleanup operation.
type CleanupResult struct {
	ShadowBranches    []string // Deleted shadow branches
	SessionStates     []string // Deleted session state files
	Checkpoints       []string // Deleted checkpoint metadata
	V2Generations     []string // Deleted archived v2 generation refs
	FailedBranches    []string // Shadow branches that failed to delete
	FailedStates      []string // Session states that failed to delete
	FailedCheckpoints []string // Checkpoints that failed to delete
	FailedV2Refs      []string // Archived v2 generation refs that failed to delete
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
	repo, err := OpenRepository(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to open git repository: %w", err)
	}

	refs, err := repo.References()
	if err != nil {
		return nil, fmt.Errorf("failed to get references: %w", err)
	}

	var shadowBranches []string

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
//   - No checkpoints on entire/checkpoints/v1 reference this session ID
//   - No shadow branch exists for the session's base commit
//
// This is strategy-agnostic as session states are shared by all strategies.
func ListOrphanedSessionStates(ctx context.Context) ([]CleanupItem, error) {
	repo, err := OpenRepository(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to open git repository: %w", err)
	}

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

	// Get all checkpoints to find which sessions have checkpoints
	cpStore := checkpoint.NewGitStore(repo)

	sessionsWithCheckpoints := make(map[string]bool)
	checkpoints, listErr := cpStore.ListCommitted(ctx)
	if listErr == nil {
		for _, cp := range checkpoints {
			sessionsWithCheckpoints[cp.SessionID] = true
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

		// Check if session has checkpoints on entire/checkpoints/v1
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

	// Update branch reference
	newRef := plumbing.NewHashReference(refName, commitHash)
	if err := repo.Storer.SetReference(newRef); err != nil {
		return nil, nil, fmt.Errorf("failed to update branch: %w", err)
	}

	// All checkpoints deleted successfully
	return checkpointIDs, []string{}, nil
}

// ListEligibleV2Generations returns archived checkpoints v2 /full/* generations
// eligible for deletion based on the configured retention window, along with
// warnings for malformed generations that were skipped.
func ListEligibleV2Generations(ctx context.Context, s *settings.EntireSettings) ([]CleanupItem, []string, error) {
	repo, err := OpenRepository(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open git repository: %w", err)
	}

	store := checkpoint.NewV2GitStore(repo, "origin")
	candidates, tempRefs, warnings, err := listArchivedV2GenerationCandidates(ctx, repo, store)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to list archived generations: %w", err)
	}
	defer removeTempRefs(repo, tempRefs)

	cutoff := time.Now().AddDate(0, 0, -s.GetFullTranscriptGenerationRetentionDays())
	cleanupItems := make([]CleanupItem, 0, len(candidates))

	for _, candidate := range candidates {
		commitHash, treeHash, refErr := store.GetRefState(candidate.RefName)
		if refErr != nil {
			warnings = append(warnings, fmt.Sprintf("generation %s: cannot read ref: %v", candidate.Name, refErr))
			continue
		}

		gen, foundCheckpointTimes, timestampErr := store.ComputeGenerationTimestampsFromTrees(treeHash, nil)
		if timestampErr != nil {
			warnings = append(warnings, fmt.Sprintf("generation %s: failed to compute raw transcript timestamps: %v", candidate.Name, timestampErr))
			continue
		}
		if !foundCheckpointTimes {
			var genErr error
			gen, genErr = store.ReadGeneration(treeHash)
			if genErr != nil {
				warnings = append(warnings, fmt.Sprintf("generation %s: failed to read generation.json: %v", candidate.Name, genErr))
				continue
			}
		}

		hasOldest := !gen.OldestCheckpointAt.IsZero()
		hasNewest := !gen.NewestCheckpointAt.IsZero()
		switch {
		case !hasOldest && !hasNewest:
			warnings = append(warnings, fmt.Sprintf("generation %s: missing generation.json", candidate.Name))
			continue
		case hasOldest != hasNewest:
			warnings = append(warnings, fmt.Sprintf("generation %s: incomplete generation.json", candidate.Name))
			continue
		case gen.OldestCheckpointAt.After(gen.NewestCheckpointAt):
			warnings = append(warnings, fmt.Sprintf("generation %s: invalid timestamps", candidate.Name))
			continue
		}
		if !gen.NewestCheckpointAt.Before(cutoff) {
			continue
		}

		refOID := candidate.RefOID
		if refOID == "" {
			refOID = commitHash.String()
		}
		cleanupItems = append(cleanupItems, CleanupItem{
			Type:   CleanupTypeV2Generation,
			ID:     candidate.Name,
			RefOID: refOID,
			Reason: "expired archived full transcript generation",
		})
	}

	return cleanupItems, warnings, nil
}

type archivedV2GenerationCandidate struct {
	Name      string
	RefName   plumbing.ReferenceName
	RefOID    string
	HasRemote bool
}

func listArchivedV2GenerationCandidates(
	ctx context.Context,
	repo *git.Repository,
	store *checkpoint.V2GitStore,
) ([]archivedV2GenerationCandidate, []plumbing.ReferenceName, []string, error) {
	localNames, err := store.ListArchivedGenerations()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("list local archived generations: %w", err)
	}

	candidatesByName := make(map[string]archivedV2GenerationCandidate, len(localNames))
	for _, name := range localNames {
		refName := plumbing.ReferenceName(paths.V2FullRefPrefix + name)
		ref, refErr := repo.Reference(refName, true)
		if refErr != nil {
			continue
		}
		candidatesByName[name] = archivedV2GenerationCandidate{
			Name:    name,
			RefName: refName,
			RefOID:  ref.Hash().String(),
		}
	}

	var warnings []string
	var tempRefs []plumbing.ReferenceName
	target, targetErr := remote.FetchURL(ctx)
	if targetErr == nil && target != "" {
		remoteRefs, remoteErr := listRemoteArchivedV2GenerationRefs(ctx, target)
		if remoteErr != nil {
			warnings = append(warnings, fmt.Sprintf("failed to list remote v2 generations: %v", remoteErr))
		} else {
			fetchTarget, fetchTargetErr := remote.ResolveFetchTarget(ctx, target)
			if fetchTargetErr != nil {
				warnings = append(warnings, fmt.Sprintf("failed to resolve remote for v2 generation fetch: %v", fetchTargetErr))
			} else {
				for name, remoteOID := range remoteRefs {
					if candidate, ok := candidatesByName[name]; ok {
						if candidate.RefOID == remoteOID {
							candidate.HasRemote = true
							candidatesByName[name] = candidate
							continue
						}
						warnings = append(warnings, fmt.Sprintf("generation %s: local archived ref OID %s differs from remote OID %s; skipping cleanup", name, candidate.RefOID, remoteOID))
						delete(candidatesByName, name)
						continue
					}
					tempRef, fetchErr := fetchArchivedV2Generation(ctx, fetchTarget, name)
					if fetchErr != nil {
						warnings = append(warnings, fmt.Sprintf("generation %s: failed to fetch remote ref: %v", name, fetchErr))
						continue
					}
					tempRefs = append(tempRefs, tempRef)
					candidatesByName[name] = archivedV2GenerationCandidate{
						Name:      name,
						RefName:   tempRef,
						RefOID:    remoteOID,
						HasRemote: true,
					}
				}
			}
		}
	}

	names := make([]string, 0, len(candidatesByName))
	for name := range candidatesByName {
		names = append(names, name)
	}
	sort.Strings(names)

	candidates := make([]archivedV2GenerationCandidate, 0, len(names))
	for _, name := range names {
		candidates = append(candidates, candidatesByName[name])
	}
	return candidates, tempRefs, warnings, nil
}

func listRemoteArchivedV2GenerationRefs(ctx context.Context, target string) (map[string]string, error) {
	output, err := remote.LsRemote(ctx, target, paths.V2FullRefPrefix+"*")
	if err != nil {
		return nil, fmt.Errorf("ls remote v2 generations: %w", err)
	}

	refs := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		refName := parts[1]
		suffix := strings.TrimPrefix(refName, paths.V2FullRefPrefix)
		if suffix == "current" || !checkpoint.GenerationRefPattern.MatchString(suffix) {
			continue
		}
		refs[suffix] = parts[0]
	}
	return refs, nil
}

func fetchArchivedV2Generation(ctx context.Context, fetchTarget, name string) (plumbing.ReferenceName, error) {
	refName := paths.V2FullRefPrefix + name
	tempRef := plumbing.ReferenceName("refs/entire-clean-tmp/v2/full/" + name)
	refSpec := fmt.Sprintf("+%s:%s", refName, tempRef)
	if output, err := remote.Fetch(ctx, remote.FetchOptions{
		Remote:   fetchTarget,
		RefSpecs: []string{refSpec},
		NoTags:   true,
		NoFilter: true,
	}); err != nil {
		return "", fmt.Errorf("%s: %w", strings.TrimSpace(string(output)), err)
	}
	return tempRef, nil
}

func removeTempRefs(repo *git.Repository, refs []plumbing.ReferenceName) {
	for _, ref := range refs {
		_ = repo.Storer.RemoveReference(ref) //nolint:errcheck // cleanup is best-effort
	}
}

// V2GenerationRef pairs a generation name with the OID observed at listing time.
type V2GenerationRef struct {
	Name   string
	RefOID string // Commit hash for compare-and-swap; empty skips the check
}

// DeleteV2Generations deletes archived checkpoints v2 /full/* generation refs.
// When RefOID is set, deletion uses compare-and-swap to avoid deleting a ref
// that was repointed after enumeration.
func DeleteV2Generations(ctx context.Context, generations []V2GenerationRef) (deleted []string, failed []string, err error) { //nolint:unparam // err kept for consistency with other Delete* functions
	if len(generations) == 0 {
		return []string{}, []string{}, nil
	}

	pushTarget, _, pushTargetErr := remote.PushURL(ctx, "origin")

	for _, gen := range generations {
		refName := plumbing.ReferenceName(paths.V2FullRefPrefix + gen.Name)
		localErr := DeleteRefCLI(ctx, refName.String(), gen.RefOID)
		if errors.Is(localErr, ErrRefNotFound) {
			localErr = nil
		}
		if localErr != nil {
			failed = append(failed, gen.Name)
			continue
		}
		if pushTargetErr == nil && pushTarget != "" {
			if remoteErr := deleteRemoteRef(ctx, pushTarget, refName.String(), gen.RefOID); remoteErr != nil {
				failed = append(failed, gen.Name)
				continue
			}
		}
		deleted = append(deleted, gen.Name)
	}

	return deleted, failed, nil
}

func deleteRemoteRef(ctx context.Context, target, refName, expectedOID string) error {
	return pushWithLease(ctx, target, ":"+refName, refName, expectedOID,
		"delete remote ref "+refName)
}

// pushWithLease runs `git push <target> <refSpec>` with an optional
// `--force-with-lease=<leaseRef>:<expectedOID>` guard. errCtx prefixes the
// error message when no stderr output is available from the push.
func pushWithLease(ctx context.Context, target, refSpec, leaseRef, expectedOID, errCtx string) error {
	extraArgs := []string{}
	if expectedOID != "" {
		extraArgs = append(extraArgs, fmt.Sprintf("--force-with-lease=%s:%s", leaseRef, expectedOID))
	}
	result, err := remote.PushWithOptions(ctx, remote.PushOptions{
		Remote:    target,
		RefSpecs:  []string{refSpec},
		ExtraArgs: extraArgs,
	})
	if err != nil {
		output := strings.TrimSpace(result.Output)
		if output != "" {
			return fmt.Errorf("%s: %w", output, err)
		}
		return fmt.Errorf("%s: %w", errCtx, err)
	}
	return nil
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
	var v2Generations []V2GenerationRef
	for _, item := range items {
		switch item.Type {
		case CleanupTypeShadowBranch:
			branches = append(branches, item.ID)
		case CleanupTypeSessionState:
			states = append(states, item.ID)
		case CleanupTypeCheckpoint:
			checkpoints = append(checkpoints, item.ID)
		case CleanupTypeV2Generation:
			v2Generations = append(v2Generations, V2GenerationRef{Name: item.ID, RefOID: item.RefOID})
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

	if len(v2Generations) > 0 {
		deleted, failed, err := DeleteV2Generations(ctx, v2Generations)
		if err != nil {
			return result, err
		}
		result.V2Generations = deleted
		result.FailedV2Refs = failed

		for _, id := range deleted {
			logging.Info(logCtx, "deleted v2 generation",
				slog.String("type", string(CleanupTypeV2Generation)),
				slog.String("id", id),
				slog.String("reason", reasonMap[id]),
			)
		}
		for _, id := range failed {
			logging.Warn(logCtx, "failed to delete v2 generation",
				slog.String("type", string(CleanupTypeV2Generation)),
				slog.String("id", id),
				slog.String("reason", reasonMap[id]),
			)
		}
	}

	// Log summary
	totalDeleted := len(result.ShadowBranches) + len(result.SessionStates) + len(result.Checkpoints) + len(result.V2Generations)
	totalFailed := len(result.FailedBranches) + len(result.FailedStates) + len(result.FailedCheckpoints) + len(result.FailedV2Refs)
	if totalDeleted > 0 || totalFailed > 0 {
		logging.Info(logCtx, "cleanup completed",
			slog.Int("deleted_branches", len(result.ShadowBranches)),
			slog.Int("deleted_session_states", len(result.SessionStates)),
			slog.Int("deleted_checkpoints", len(result.Checkpoints)),
			slog.Int("deleted_v2_generations", len(result.V2Generations)),
			slog.Int("failed_branches", len(result.FailedBranches)),
			slog.Int("failed_session_states", len(result.FailedStates)),
			slog.Int("failed_checkpoints", len(result.FailedCheckpoints)),
			slog.Int("failed_v2_generations", len(result.FailedV2Refs)),
		)
	}

	return result, nil
}
