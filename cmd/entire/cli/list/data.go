package list

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"entire.io/cli/cmd/entire/cli/paths"
	"entire.io/cli/cmd/entire/cli/session"
	"entire.io/cli/cmd/entire/cli/strategy"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

const (
	// maxCheckpointsToScan limits how many checkpoints we scan from entire/sessions
	maxCheckpointsToScan = 50

	// maxCommitsToScanPerBranch limits commit traversal per branch
	maxCommitsToScanPerBranch = 50
)

// FetchTreeData gathers all the data needed for the hierarchical view.
// The hierarchy is: Branch → Checkpoint → Session
// Checkpoints belong to commits, and the same session may appear in multiple checkpoints.
func FetchTreeData() (*TreeData, error) {
	repo, err := strategy.OpenRepository()
	if err != nil {
		return nil, fmt.Errorf("failed to open repository: %w", err)
	}

	// Get current branch
	currentBranch, err := getCurrentBranch(repo)
	if err != nil {
		currentBranch = ""
	}

	// Get main branch name
	mainBranch := getMainBranchName(repo)

	// Get active session states (includes base commit info)
	activeSessionStates, err := getActiveSessionStates()
	if err != nil {
		activeSessionStates = make(map[string]*session.State)
	}

	// Get current session ID (if any)
	currentSessionID, _ := paths.ReadCurrentSession() //nolint:errcheck // Empty string is fine

	// Helper to check if a session is truly active
	// (has state file AND base commit is in recent history of current branch)
	const maxRecentCommits = 20
	isSessionActive := func(sessionID string) bool {
		if sessionID == currentSessionID {
			return true
		}
		state, hasState := activeSessionStates[sessionID]
		if !hasState {
			return false
		}
		// If session was condensed but has no new checkpoints, it's not actively running
		// (just resumable). Only show as active if it's the current session.
		if state.CondensedTranscriptLines > 0 && state.CheckpointCount == 0 {
			return false
		}
		// Only consider active if base commit is in recent history
		return isCommitInRecentHistory(repo, state.BaseCommit, currentBranch, maxRecentCommits)
	}

	// Fetch checkpoints from entire/sessions branch
	strategyCheckpoints, err := strategy.ListCheckpoints()
	if err != nil {
		return nil, fmt.Errorf("failed to list checkpoints: %w", err)
	}

	// Limit to most recent checkpoints
	if len(strategyCheckpoints) > maxCheckpointsToScan {
		strategyCheckpoints = strategyCheckpoints[:maxCheckpointsToScan]
	}

	// Build checkpoint ID -> strategy.CheckpointInfo mapping
	checkpointInfoMap := make(map[string]strategy.CheckpointInfo)
	for _, cp := range strategyCheckpoints {
		checkpointInfoMap[cp.CheckpointID] = cp
	}

	// Group checkpoints by branch
	branchCheckpoints := make(map[string][]CheckpointInfo)
	branchSet := make(map[string]bool)

	// Always include main and current branch
	if mainBranch != "" {
		branchSet[mainBranch] = true
	}
	if currentBranch != "" {
		branchSet[currentBranch] = true
	}

	// Scan commits on current branch for checkpoint trailers
	if currentBranch != "" {
		foundCheckpoints := findCheckpointsOnBranch(repo, currentBranch, checkpointInfoMap, mainBranch, isSessionActive, activeSessionStates)
		branchCheckpoints[currentBranch] = foundCheckpoints
	}

	// Also scan main branch if it's not the current branch
	if mainBranch != "" && mainBranch != currentBranch {
		// For main branch, pass empty string as mainBranch param to scan all commits
		foundCheckpoints := findCheckpointsOnBranch(repo, mainBranch, checkpointInfoMap, "", isSessionActive, activeSessionStates)
		branchCheckpoints[mainBranch] = foundCheckpoints
	}

	// Scan all checkpoints to find which branches contain them
	for _, cp := range strategyCheckpoints {
		// Find which branch(es) contain the commit for this checkpoint
		branchCommits := findBranchesAndCommitsForCheckpoint(repo, cp.CheckpointID, mainBranch)
		for branchName, commitInfo := range branchCommits {
			branchSet[branchName] = true

			// Check if we already have this checkpoint on this branch
			alreadyAdded := false
			for _, existing := range branchCheckpoints[branchName] {
				if existing.CheckpointID == cp.CheckpointID {
					alreadyAdded = true
					break
				}
			}

			if !alreadyAdded {
				// Build sessions list for this checkpoint
				sessions := buildSessionsForCheckpoint(repo, cp, cp.CheckpointID, isSessionActive, activeSessionStates)

				cpInfo := CheckpointInfo{
					CheckpointID: cp.CheckpointID,
					CommitHash:   commitInfo.Hash,
					CommitMsg:    commitInfo.Message,
					CreatedAt:    cp.CreatedAt,
					StepsCount:   cp.GetStepsCount(),
					IsTask:       cp.IsTask,
					ToolUseID:    cp.ToolUseID,
					Sessions:     sessions,
				}
				branchCheckpoints[branchName] = append(branchCheckpoints[branchName], cpInfo)
			}
		}
	}

	// Build branch list
	// Note: checkpoints are already in git commit order (newest first) from findCheckpointsOnBranch
	var branches []BranchInfo
	for branchName := range branchSet {
		info := BranchInfo{
			Name:        branchName,
			IsCurrent:   branchName == currentBranch,
			IsMerged:    isBranchMerged(repo, branchName, mainBranch),
			Checkpoints: branchCheckpoints[branchName],
		}

		branches = append(branches, info)
	}

	// Sort branches: current first, then main, then by most recent activity
	sort.Slice(branches, func(i, j int) bool {
		// Current branch always first
		if branches[i].IsCurrent {
			return true
		}
		if branches[j].IsCurrent {
			return false
		}
		// Main branch second
		if branches[i].Name == mainBranch {
			return true
		}
		if branches[j].Name == mainBranch {
			return false
		}
		// Then by most recent checkpoint activity (descending)
		iTime := getMostRecentCheckpointActivity(branches[i])
		jTime := getMostRecentCheckpointActivity(branches[j])
		return iTime.After(jTime)
	})

	return &TreeData{
		Branches:      branches,
		CurrentBranch: currentBranch,
		MainBranch:    mainBranch,
	}, nil
}

// getCurrentBranch returns the current branch name.
func getCurrentBranch(repo *git.Repository) (string, error) {
	head, err := repo.Head()
	if err != nil {
		return "", fmt.Errorf("failed to get HEAD: %w", err)
	}

	if !head.Name().IsBranch() {
		return "", errors.New("HEAD is not a branch")
	}

	return head.Name().Short(), nil
}

// getMainBranchName returns the default branch name (main or master).
func getMainBranchName(repo *git.Repository) string {
	// Try to get from remote HEAD
	remotes, err := repo.Remotes()
	if err == nil {
		for _, remote := range remotes {
			if remote.Config().Name == "origin" {
				refs, err := remote.List(&git.ListOptions{})
				if err == nil {
					for _, ref := range refs {
						if ref.Name() == plumbing.HEAD {
							target := ref.Target()
							if target.IsBranch() {
								return target.Short()
							}
						}
					}
				}
			}
		}
	}

	// Fallback: check for main or master
	for _, name := range []string{"main", "master"} {
		refName := plumbing.NewBranchReferenceName(name)
		if _, err := repo.Reference(refName, true); err == nil {
			return name
		}
	}

	return "main"
}

// getActiveSessionStates returns session states that have active state files.
func getActiveSessionStates() (map[string]*session.State, error) {
	store, err := session.NewStateStore()
	if err != nil {
		return nil, fmt.Errorf("failed to create state store: %w", err)
	}

	states, err := store.List(context.Background())
	if err != nil {
		return nil, fmt.Errorf("failed to list states: %w", err)
	}

	result := make(map[string]*session.State)
	for _, state := range states {
		result[state.SessionID] = state
	}
	return result, nil
}

// isCommitInRecentHistory checks if a commit hash is within the last N commits of a branch.
func isCommitInRecentHistory(repo *git.Repository, commitHash string, branchName string, maxCommits int) bool {
	if commitHash == "" || branchName == "" {
		return false
	}

	refName := plumbing.NewBranchReferenceName(branchName)
	ref, err := repo.Reference(refName, true)
	if err != nil {
		return false
	}

	iter, err := repo.Log(&git.LogOptions{From: ref.Hash()})
	if err != nil {
		return false
	}

	count := 0
	found := false
	_ = iter.ForEach(func(c *object.Commit) error { //nolint:errcheck // Best-effort search
		count++
		if count > maxCommits {
			return errors.New("limit reached")
		}
		if c.Hash.String() == commitHash || strings.HasPrefix(c.Hash.String(), commitHash) {
			found = true
			return errors.New("found")
		}
		return nil
	})

	return found
}

// commitInfo holds basic commit information for display.
type commitInfo struct {
	Hash    string
	Message string
}

// findCheckpointsOnBranch finds checkpoints associated with commits on a branch.
func findCheckpointsOnBranch(repo *git.Repository, branchName string, checkpointInfoMap map[string]strategy.CheckpointInfo, mainBranch string, isSessionActive func(string) bool, sessionStates map[string]*session.State) []CheckpointInfo {
	refName := plumbing.NewBranchReferenceName(branchName)
	ref, err := repo.Reference(refName, true)
	if err != nil {
		return nil
	}

	// Find merge-base with main to know where to stop
	var mergeBaseHash plumbing.Hash
	if mainBranch != "" && mainBranch != branchName {
		mergeBaseHash = getMergeBaseHash(repo, branchName, mainBranch)
	}

	var checkpoints []CheckpointInfo
	seenCheckpoints := make(map[string]bool)

	iter, err := repo.Log(&git.LogOptions{From: ref.Hash()})
	if err != nil {
		return nil
	}

	count := 0
	_ = iter.ForEach(func(c *object.Commit) error { //nolint:errcheck // Best-effort search, errors are intentional stops
		count++
		if count > maxCommitsToScanPerBranch {
			return errors.New("limit reached")
		}

		// Stop at merge-base (commits at and before this are shared with main)
		if mergeBaseHash != plumbing.ZeroHash && c.Hash == mergeBaseHash {
			return errors.New("reached merge-base")
		}

		// Check for checkpoint trailer
		checkpointID, found := paths.ParseCheckpointTrailer(c.Message)
		if !found {
			return nil
		}

		// Skip if already seen
		if seenCheckpoints[checkpointID] {
			return nil
		}
		seenCheckpoints[checkpointID] = true

		// Get checkpoint info from map
		cpInfo, ok := checkpointInfoMap[checkpointID]
		if !ok {
			// Checkpoint exists in commit but not in entire/sessions (may have been pruned)
			return nil
		}

		// Extract first line of commit message for display
		commitMsg := c.Message
		if idx := strings.Index(commitMsg, "\n"); idx != -1 {
			commitMsg = commitMsg[:idx]
		}

		// Build sessions list for this checkpoint
		sessions := buildSessionsForCheckpoint(repo, cpInfo, checkpointID, isSessionActive, sessionStates)

		checkpoints = append(checkpoints, CheckpointInfo{
			CheckpointID: checkpointID,
			CommitHash:   c.Hash.String()[:7],
			CommitMsg:    commitMsg,
			CreatedAt:    cpInfo.CreatedAt,
			StepsCount:   cpInfo.GetStepsCount(),
			IsTask:       cpInfo.IsTask,
			ToolUseID:    cpInfo.ToolUseID,
			Sessions:     sessions,
		})

		return nil
	})

	return checkpoints
}

// findBranchesAndCommitsForCheckpoint finds branches that contain a commit with this checkpoint ID.
// Returns a map of branch name -> commit info. Only searches commits unique to each branch (not reachable from main).
func findBranchesAndCommitsForCheckpoint(repo *git.Repository, checkpointID string, mainBranch string) map[string]commitInfo {
	result := make(map[string]commitInfo)

	// Get all local branches
	refs, err := repo.References()
	if err != nil {
		return result
	}

	_ = refs.ForEach(func(ref *plumbing.Reference) error { //nolint:errcheck // Best-effort search
		if !ref.Name().IsBranch() {
			return nil
		}

		// Skip shadow branches
		branchName := ref.Name().Short()
		if strings.HasPrefix(branchName, "entire/") {
			return nil
		}

		// Skip main branch itself (it gets added separately)
		if branchName == mainBranch {
			return nil
		}

		// Find merge-base with main for this specific branch
		var mergeBaseHash plumbing.Hash
		if mainBranch != "" {
			mergeBaseHash = getMergeBaseHash(repo, branchName, mainBranch)
		}

		// Search commits unique to this branch (stop at merge-base)
		iter, err := repo.Log(&git.LogOptions{From: ref.Hash()})
		if err != nil {
			return nil //nolint:nilerr // Continue to next branch on error
		}

		count := 0
		_ = iter.ForEach(func(c *object.Commit) error { //nolint:errcheck // Best-effort search, errors are intentional stops
			count++
			if count > maxCommitsToScanPerBranch {
				return errors.New("limit reached")
			}

			// Stop at merge-base - commits at and before this are shared with main
			if mergeBaseHash != plumbing.ZeroHash && c.Hash == mergeBaseHash {
				return errors.New("reached merge-base")
			}

			cpID, hasTrailer := paths.ParseCheckpointTrailer(c.Message)
			if hasTrailer && cpID == checkpointID {
				// Extract first line of commit message
				commitMsg := c.Message
				if idx := strings.Index(commitMsg, "\n"); idx != -1 {
					commitMsg = commitMsg[:idx]
				}
				result[branchName] = commitInfo{
					Hash:    c.Hash.String()[:7],
					Message: commitMsg,
				}
				return errors.New("found")
			}
			return nil
		})

		return nil
	})

	return result
}

// getDescriptionForCheckpoint reads the description for a checkpoint.
func getDescriptionForCheckpoint(repo *git.Repository, checkpointID string) string {
	return strategy.GetDescriptionForCheckpoint(repo, checkpointID)
}

// buildSessionsForCheckpoint creates SessionInfo entries for all sessions in a checkpoint.
// Sessions are sorted by start time with newest first.
func buildSessionsForCheckpoint(repo *git.Repository, cpInfo strategy.CheckpointInfo, checkpointID string, isSessionActive func(string) bool, sessionStates map[string]*session.State) []SessionInfo {
	sessionIDs := cpInfo.GetSessionIDs()

	// Sort session IDs by start time (newest first)
	sort.Slice(sessionIDs, func(i, j int) bool {
		stateI, hasI := sessionStates[sessionIDs[i]]
		stateJ, hasJ := sessionStates[sessionIDs[j]]
		if !hasI && !hasJ {
			return sessionIDs[i] > sessionIDs[j] // Fallback to reverse lexicographic
		}
		if !hasI {
			return false // Sessions without state go last
		}
		if !hasJ {
			return true // Sessions without state go last
		}
		return stateI.StartedAt.After(stateJ.StartedAt)
	})

	sessions := make([]SessionInfo, 0, len(sessionIDs))
	for i, sessionID := range sessionIDs {
		// For the first (newest) session, get description from root
		// For older sessions, use a placeholder
		var description string
		if i == 0 {
			description = getDescriptionForCheckpoint(repo, checkpointID)
		} else {
			description = "Archived session"
		}

		sessions = append(sessions, SessionInfo{
			SessionID:   sessionID,
			Description: description,
			IsActive:    isSessionActive(sessionID),
		})
	}

	return sessions
}

// getMostRecentCheckpointActivity returns the most recent timestamp from a branch's checkpoints.
func getMostRecentCheckpointActivity(branch BranchInfo) time.Time {
	var mostRecent time.Time
	for _, cp := range branch.Checkpoints {
		if cp.CreatedAt.After(mostRecent) {
			mostRecent = cp.CreatedAt
		}
	}
	return mostRecent
}

// isBranchMerged checks if a branch has been merged into the main branch.
func isBranchMerged(repo *git.Repository, branchName, mainBranch string) bool {
	if branchName == mainBranch || mainBranch == "" {
		return false
	}

	branchRef, err := repo.Reference(plumbing.NewBranchReferenceName(branchName), true)
	if err != nil {
		return false
	}

	mainRef, err := repo.Reference(plumbing.NewBranchReferenceName(mainBranch), true)
	if err != nil {
		return false
	}

	branchCommit, err := repo.CommitObject(branchRef.Hash())
	if err != nil {
		return false
	}

	mainCommit, err := repo.CommitObject(mainRef.Hash())
	if err != nil {
		return false
	}

	// Check if branch commit is an ancestor of main
	isAncestor, err := branchCommit.IsAncestor(mainCommit)
	if err != nil {
		return false
	}

	return isAncestor
}

// getMergeBaseHash finds the merge-base between two branches.
// Returns ZeroHash if merge-base cannot be determined.
func getMergeBaseHash(repo *git.Repository, branch1, branch2 string) plumbing.Hash {
	ref1, err := repo.Reference(plumbing.NewBranchReferenceName(branch1), true)
	if err != nil {
		return plumbing.ZeroHash
	}

	ref2, err := repo.Reference(plumbing.NewBranchReferenceName(branch2), true)
	if err != nil {
		return plumbing.ZeroHash
	}

	commit1, err := repo.CommitObject(ref1.Hash())
	if err != nil {
		return plumbing.ZeroHash
	}

	commit2, err := repo.CommitObject(ref2.Hash())
	if err != nil {
		return plumbing.ZeroHash
	}

	mergeBase, err := commit1.MergeBase(commit2)
	if err != nil || len(mergeBase) == 0 {
		return plumbing.ZeroHash
	}

	return mergeBase[0].Hash
}
