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
	maxCheckpointsToScan = 200

	// maxCommitsToScanPerBranch limits commit traversal per branch
	maxCommitsToScanPerBranch = 100
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

	// Build set of commits reachable from main (used to filter feature branches)
	mainCommits := getCommitsReachableFromMain(repo, mainBranch, maxCommitsToScanPerBranch*2)

	// Scan commits on current branch for checkpoint trailers
	if currentBranch != "" {
		// If current branch IS main, don't filter out main commits (we want to see them)
		filterCommits := mainCommits
		if currentBranch == mainBranch {
			filterCommits = nil
		}
		foundCheckpoints := findCheckpointsOnBranch(repo, currentBranch, checkpointInfoMap, filterCommits, isSessionActive, activeSessionStates)
		branchCheckpoints[currentBranch] = foundCheckpoints
	}

	// Also scan main branch if it's not the current branch
	if mainBranch != "" && mainBranch != currentBranch {
		// For main branch, pass nil to scan all commits (don't filter out main commits from main)
		foundCheckpoints := findCheckpointsOnBranch(repo, mainBranch, checkpointInfoMap, nil, isSessionActive, activeSessionStates)
		branchCheckpoints[mainBranch] = foundCheckpoints
	}

	// Scan all checkpoints to find which branches contain them
	for _, cp := range strategyCheckpoints {
		// Find which branch(es) contain the commit for this checkpoint
		branchCommits := findBranchesAndCommitsForCheckpoint(repo, cp.CheckpointID, mainBranch, mainCommits)
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
					Author:       commitInfo.Author,
					Insertions:   commitInfo.Insertions,
					Deletions:    commitInfo.Deletions,
					FileCount:    len(cp.FilesTouched),
					Agent:        cp.Agent,
					Sessions:     sessions,
				}
				branchCheckpoints[branchName] = append(branchCheckpoints[branchName], cpInfo)
			}
		}
	}

	// Fetch uncommitted shadow branch checkpoints for current branch (manual-commit strategy only)
	if currentBranch != "" {
		uncommittedCheckpoints := fetchUncommittedCheckpoints(checkpointInfoMap, isSessionActive)
		if len(uncommittedCheckpoints) > 0 {
			branchCheckpoints[currentBranch] = append(uncommittedCheckpoints, branchCheckpoints[currentBranch]...)
		}
	}

	// Build branch list
	var branches []BranchInfo
	for branchName := range branchSet {
		// Sort checkpoints by CreatedAt (newest first)
		cps := branchCheckpoints[branchName]
		sort.Slice(cps, func(i, j int) bool {
			return cps[i].CreatedAt.After(cps[j].CreatedAt)
		})

		info := BranchInfo{
			Name:        branchName,
			IsCurrent:   branchName == currentBranch,
			IsMerged:    isBranchMerged(repo, branchName, mainBranch),
			Checkpoints: cps,
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
	Hash       string
	Message    string
	Author     string
	Insertions int
	Deletions  int
}

// findCheckpointsOnBranch finds checkpoints associated with commits on a branch.
func findCheckpointsOnBranch(repo *git.Repository, branchName string, checkpointInfoMap map[string]strategy.CheckpointInfo, mainCommits map[plumbing.Hash]bool, isSessionActive func(string) bool, sessionStates map[string]*session.State) []CheckpointInfo {
	refName := plumbing.NewBranchReferenceName(branchName)
	ref, err := repo.Reference(refName, true)
	if err != nil {
		return nil
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

		// Skip commits that are reachable from main (shared commits)
		// This handles branches that have merged main into them
		if mainCommits != nil && mainCommits[c.Hash] {
			return nil
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

		// Get diff stats
		insertions, deletions := getCommitDiffStats(repo, c)

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
			Author:       c.Author.Name,
			Insertions:   insertions,
			Deletions:    deletions,
			FileCount:    len(cpInfo.FilesTouched),
			Agent:        cpInfo.Agent,
			Sessions:     sessions,
		})

		return nil
	})

	return checkpoints
}

// findBranchesAndCommitsForCheckpoint finds branches that contain a commit with this checkpoint ID.
// Returns a map of branch name -> commit info. Only searches commits unique to each branch (not reachable from main).
func findBranchesAndCommitsForCheckpoint(repo *git.Repository, checkpointID string, mainBranch string, mainCommits map[plumbing.Hash]bool) map[string]commitInfo {
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

		// Search commits unique to this branch
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

			// Skip commits that are reachable from main (shared commits)
			if mainCommits != nil && mainCommits[c.Hash] {
				return nil
			}

			cpID, hasTrailer := paths.ParseCheckpointTrailer(c.Message)
			if hasTrailer && cpID == checkpointID {
				// Extract first line of commit message
				commitMsg := c.Message
				if idx := strings.Index(commitMsg, "\n"); idx != -1 {
					commitMsg = commitMsg[:idx]
				}
				// Get diff stats
				insertions, deletions := getCommitDiffStats(repo, c)
				result[branchName] = commitInfo{
					Hash:       c.Hash.String()[:7],
					Message:    commitMsg,
					Author:     c.Author.Name,
					Insertions: insertions,
					Deletions:  deletions,
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
// Sessions are kept in the order they appear in the session_ids array from metadata.
func buildSessionsForCheckpoint(repo *git.Repository, cpInfo strategy.CheckpointInfo, checkpointID string, isSessionActive func(string) bool, _ map[string]*session.State) []SessionInfo {
	sessionIDs := cpInfo.GetSessionIDs()

	// Calculate per-session step count (estimate: divide evenly among sessions)
	totalSteps := cpInfo.GetStepsCount()
	sessionCount := len(sessionIDs)
	if sessionCount == 0 {
		sessionCount = 1
	}
	perSessionSteps := totalSteps / sessionCount

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
			Agent:       cpInfo.Agent,
			StepsCount:  perSessionSteps,
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

// getCommitsReachableFromMain returns a set of commit hashes reachable from the main branch.
// This is used to filter out commits that are shared with main when listing branch-specific checkpoints.
func getCommitsReachableFromMain(repo *git.Repository, mainBranch string, limit int) map[plumbing.Hash]bool {
	result := make(map[plumbing.Hash]bool)

	if mainBranch == "" {
		return result
	}

	refName := plumbing.NewBranchReferenceName(mainBranch)
	ref, err := repo.Reference(refName, true)
	if err != nil {
		return result
	}

	iter, err := repo.Log(&git.LogOptions{From: ref.Hash()})
	if err != nil {
		return result
	}

	count := 0
	_ = iter.ForEach(func(c *object.Commit) error { //nolint:errcheck // Best-effort
		count++
		if count > limit {
			return errors.New("limit reached")
		}
		result[c.Hash] = true
		return nil
	})

	return result
}

// getCommitDiffStats computes the number of insertions and deletions for a commit.
// Returns (0, 0) if stats cannot be computed.
func getCommitDiffStats(_ *git.Repository, c *object.Commit) (insertions, deletions int) {
	return getCommitDiffStatsFiltered(c, nil)
}

// getCommitDiffStatsFiltered computes diff stats, optionally excluding paths matching a prefix.
// Returns (insertions, deletions, fileCount).
func getCommitDiffStatsFiltered(c *object.Commit, excludePrefixes []string) (insertions, deletions int) {
	// Get parent tree (empty tree for initial commits)
	var parentTree *object.Tree
	if c.NumParents() > 0 {
		parent, err := c.Parent(0)
		if err == nil {
			parentTree, err = parent.Tree()
			if err != nil {
				return 0, 0
			}
		}
	}

	// Get current tree
	currentTree, err := c.Tree()
	if err != nil {
		return 0, 0
	}

	// Compute diff
	changes, err := parentTree.Diff(currentTree)
	if err != nil {
		return 0, 0
	}

	// Sum up stats from all changes
	for _, change := range changes {
		patch, err := change.Patch()
		if err != nil {
			continue
		}
		for _, fileStat := range patch.Stats() {
			// Check if file should be excluded
			excluded := false
			for _, prefix := range excludePrefixes {
				if strings.HasPrefix(fileStat.Name, prefix) {
					excluded = true
					break
				}
			}
			if excluded {
				continue
			}
			insertions += fileStat.Addition
			deletions += fileStat.Deletion
		}
	}

	return insertions, deletions
}

// getShadowCommitStats computes diff stats for a shadow commit, excluding metadata files.
func getShadowCommitStats(repo *git.Repository, commitHash string) (insertions, deletions, fileCount int) {
	hash := plumbing.NewHash(commitHash)
	commit, err := repo.CommitObject(hash)
	if err != nil {
		return 0, 0, 0
	}

	// Exclude .entire/ metadata directory
	excludePrefixes := []string{".entire/"}
	insertions, deletions = getCommitDiffStatsFiltered(commit, excludePrefixes)

	// Count files changed (excluding metadata)
	fileCount = countFilesChanged(commit, excludePrefixes)

	return insertions, deletions, fileCount
}

// countFilesChanged counts the number of files changed in a commit, excluding specified prefixes.
func countFilesChanged(c *object.Commit, excludePrefixes []string) int {
	var parentTree *object.Tree
	if c.NumParents() > 0 {
		parent, err := c.Parent(0)
		if err == nil {
			parentTree, err = parent.Tree()
			if err != nil {
				parentTree = nil
			}
		}
	}

	currentTree, err := c.Tree()
	if err != nil {
		return 0
	}

	changes, err := parentTree.Diff(currentTree)
	if err != nil {
		return 0
	}

	count := 0
	for _, change := range changes {
		// Get the file path from the change
		var filePath string
		if change.To.Name != "" {
			filePath = change.To.Name
		} else if change.From.Name != "" {
			filePath = change.From.Name
		}

		// Check if file should be excluded
		excluded := false
		for _, prefix := range excludePrefixes {
			if strings.HasPrefix(filePath, prefix) {
				excluded = true
				break
			}
		}
		if !excluded {
			count++
		}
	}

	return count
}

// fetchUncommittedCheckpoints retrieves shadow branch checkpoints that haven't been committed yet.
// Only works when manual-commit strategy is active.
// Task checkpoints are nested under their parent prompt checkpoint.
func fetchUncommittedCheckpoints(committedCheckpoints map[string]strategy.CheckpointInfo, isSessionActive func(string) bool) []CheckpointInfo {
	// Get current strategy
	strat := GetStrategy()
	if strat == nil {
		return nil
	}

	// Only manual-commit strategy has shadow branch checkpoints
	if strat.Name() != "manual-commit" {
		return nil
	}

	// Get rewind points from the strategy (includes uncommitted shadow branch checkpoints)
	rewindPoints, err := strat.GetRewindPoints(50)
	if err != nil {
		return nil
	}

	// Open repository for computing diff stats
	repo, err := strategy.OpenRepository()
	if err != nil {
		return nil
	}

	// Build set of committed checkpoint IDs to avoid duplicates
	committedIDs := make(map[string]bool)
	for cpID := range committedCheckpoints {
		committedIDs[cpID] = true
	}

	// First pass: separate prompt checkpoints and task checkpoints
	// Task checkpoints will be nested under their parent prompt checkpoint
	var promptCheckpoints []CheckpointInfo
	var taskCheckpoints []CheckpointInfo

	for _, point := range rewindPoints {
		// Skip logs-only points (these are already committed)
		if point.IsLogsOnly {
			continue
		}

		// Skip if this checkpoint ID is already in the committed list
		if point.CheckpointID != "" && committedIDs[point.CheckpointID] {
			continue
		}

		// This is an uncommitted shadow branch checkpoint
		shortHash := point.ID
		if len(shortHash) > 7 {
			shortHash = shortHash[:7]
		}

		// Get diff stats for this shadow commit (excluding .entire/ metadata)
		insertions, deletions, fileCount := getShadowCommitStats(repo, point.ID)

		// Build session info from the rewind point
		var sessions []SessionInfo
		if point.SessionID != "" {
			sessions = append(sessions, SessionInfo{
				SessionID:   point.SessionID,
				Description: point.SessionPrompt,
				IsActive:    isSessionActive(point.SessionID),
				StepsCount:  0, // Not available from rewind point
			})
		}

		cpInfo := CheckpointInfo{
			CheckpointID:  point.ID, // Use commit hash as ID for uncommitted checkpoints
			CommitHash:    shortHash,
			CommitMsg:     point.Message,
			CreatedAt:     point.Date,
			IsTask:        point.IsTaskCheckpoint,
			ToolUseID:     point.ToolUseID,
			Insertions:    insertions,
			Deletions:     deletions,
			FileCount:     fileCount,
			IsUncommitted: true,
			Sessions:      sessions,
		}

		if point.IsTaskCheckpoint {
			taskCheckpoints = append(taskCheckpoints, cpInfo)
		} else {
			promptCheckpoints = append(promptCheckpoints, cpInfo)
		}
	}

	// Second pass: nest task checkpoints under their parent prompt checkpoint
	// Task checkpoints belong to the most recent prompt checkpoint in the same session
	// that was created before them
	for i := range promptCheckpoints {
		prompt := &promptCheckpoints[i]
		promptSessionID := ""
		if len(prompt.Sessions) > 0 {
			promptSessionID = prompt.Sessions[0].SessionID
		}

		// Find task checkpoints that belong to this prompt
		// (same session, created after this prompt but before the next prompt)
		var nextPromptTime time.Time
		// Find the next prompt checkpoint in same session (if any)
		for j := i - 1; j >= 0; j-- { // Go backwards since list is newest-first
			if len(promptCheckpoints[j].Sessions) > 0 &&
				promptCheckpoints[j].Sessions[0].SessionID == promptSessionID {
				nextPromptTime = promptCheckpoints[j].CreatedAt
				break
			}
		}

		for _, task := range taskCheckpoints {
			taskSessionID := ""
			if len(task.Sessions) > 0 {
				taskSessionID = task.Sessions[0].SessionID
			}

			// Task belongs to this prompt if:
			// 1. Same session
			// 2. Created after this prompt
			// 3. Created before the next prompt (or no next prompt)
			if taskSessionID == promptSessionID &&
				task.CreatedAt.After(prompt.CreatedAt) &&
				(nextPromptTime.IsZero() || task.CreatedAt.Before(nextPromptTime)) {
				prompt.TaskCheckpoints = append(prompt.TaskCheckpoints, task)
			}
		}
	}

	return promptCheckpoints
}
