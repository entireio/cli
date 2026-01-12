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

	// Get all sessions
	sessions, err := strategy.ListSessions()
	if err != nil {
		return nil, fmt.Errorf("failed to list sessions: %w", err)
	}

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
		// Only consider active if base commit is in recent history
		return isCommitInRecentHistory(repo, state.BaseCommit, currentBranch, maxRecentCommits)
	}

	// Build checkpoint -> session mapping
	checkpointToSession := make(map[string]*strategy.Session)
	for i := range sessions {
		for _, cp := range sessions[i].Checkpoints {
			if cp.CheckpointID != "" {
				checkpointToSession[cp.CheckpointID] = &sessions[i]
			}
		}
	}

	// Find branches and their associated sessions
	branchSessions := make(map[string][]SessionInfo)
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
		foundSessions := findSessionsOnBranch(repo, currentBranch, checkpointToSession, mainBranch)
		for _, sess := range foundSessions {
			sess.IsActive = isSessionActive(sess.Session.ID)
			branchSessions[currentBranch] = append(branchSessions[currentBranch], sess)
		}
	}

	// Also scan main branch if it's not the current branch
	if mainBranch != "" && mainBranch != currentBranch {
		// For main branch, pass empty string as mainBranch param to scan all commits
		foundSessions := findSessionsOnBranch(repo, mainBranch, checkpointToSession, "")
		for _, sess := range foundSessions {
			sess.IsActive = isSessionActive(sess.Session.ID)
			branchSessions[mainBranch] = append(branchSessions[mainBranch], sess)
		}
	}

	// Scan checkpoints from entire/sessions to find associated branches
	checkpoints, err := strategy.ListCheckpoints()
	if err == nil {
		// Limit to most recent checkpoints
		if len(checkpoints) > maxCheckpointsToScan {
			checkpoints = checkpoints[:maxCheckpointsToScan]
		}

		for _, cp := range checkpoints {
			sess, ok := checkpointToSession[cp.CheckpointID]
			if !ok {
				continue
			}

			// Find which branch(es) contain the commit for this checkpoint
			branches := findBranchesForCheckpoint(repo, cp.CheckpointID, mainBranch)
			for _, branch := range branches {
				branchSet[branch] = true

				// Check if we already have this session on this branch
				alreadyAdded := false
				for _, existing := range branchSessions[branch] {
					if existing.Session.ID == sess.ID {
						alreadyAdded = true
						break
					}
				}

				if !alreadyAdded {
					sessInfo := SessionInfo{
						Session:    *sess,
						IsActive:   isSessionActive(sess.ID),
						BranchName: branch,
					}
					branchSessions[branch] = append(branchSessions[branch], sessInfo)
				}
			}
		}
	}

	// Build branch list
	var branches []BranchInfo
	for branchName := range branchSet {
		info := BranchInfo{
			Name:      branchName,
			IsCurrent: branchName == currentBranch,
			IsMerged:  isBranchMerged(repo, branchName, mainBranch),
			Sessions:  branchSessions[branchName],
		}

		// Sort sessions by most recent activity
		sort.Slice(info.Sessions, func(i, j int) bool {
			iTime := info.Sessions[i].Session.StartTime
			for _, cp := range info.Sessions[i].Session.Checkpoints {
				if cp.Timestamp.After(iTime) {
					iTime = cp.Timestamp
				}
			}
			jTime := info.Sessions[j].Session.StartTime
			for _, cp := range info.Sessions[j].Session.Checkpoints {
				if cp.Timestamp.After(jTime) {
					jTime = cp.Timestamp
				}
			}
			return iTime.After(jTime)
		})

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
		// Then by most recent session activity (descending)
		iTime := getMostRecentActivity(branches[i])
		jTime := getMostRecentActivity(branches[j])
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

// findSessionsOnBranch finds sessions associated with commits on a branch.
func findSessionsOnBranch(repo *git.Repository, branchName string, checkpointToSession map[string]*strategy.Session, mainBranch string) []SessionInfo {
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

	var sessions []SessionInfo
	seenSessions := make(map[string]bool)

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

		sess, ok := checkpointToSession[checkpointID]
		if !ok || seenSessions[sess.ID] {
			return nil
		}

		seenSessions[sess.ID] = true
		sessions = append(sessions, SessionInfo{
			Session:    *sess,
			BranchName: branchName,
		})

		return nil
	})

	return sessions
}

// findBranchesForCheckpoint finds branches that contain a commit with this checkpoint ID.
// Only searches commits unique to each branch (not reachable from main).
func findBranchesForCheckpoint(repo *git.Repository, checkpointID string, mainBranch string) []string {
	var branches []string

	// Get all local branches
	refs, err := repo.References()
	if err != nil {
		return branches
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
		found := false
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
				found = true
				return errors.New("found")
			}
			return nil
		})

		if found {
			branches = append(branches, branchName)
		}
		return nil
	})

	return branches
}

// getMostRecentActivity returns the most recent timestamp from a branch's sessions.
func getMostRecentActivity(branch BranchInfo) time.Time {
	var mostRecent time.Time
	for _, sess := range branch.Sessions {
		if sess.Session.StartTime.After(mostRecent) {
			mostRecent = sess.Session.StartTime
		}
		for _, cp := range sess.Session.Checkpoints {
			if cp.Timestamp.After(mostRecent) {
				mostRecent = cp.Timestamp
			}
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
