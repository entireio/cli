package strategy

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"entire.io/cli/cmd/entire/cli/checkpoint"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// determineSuffix decides which shadow branch suffix to use for the next checkpoint.
//
// Decision logic:
//  1. If no suffix yet (ShadowBranchSuffix == 0), start at suffix 1
//  2. If continuing an existing session (CheckpointCount > 0), continue on same branch
//  3. If worktree is clean, previous work was dismissed → new suffix
//  4. If modified files don't overlap with shadow branch files → new suffix
//  5. If modified files overlap but agent's lines are gone → new suffix
//  6. Otherwise, continue on the same suffix (agent's work is preserved)
//
// Returns (suffix, isNew, error) where isNew indicates this is a fresh suffix.
func (s *ManualCommitStrategy) determineSuffix(repo *git.Repository, state *SessionState) (int, bool, error) {
	// Handle legacy sessions (suffix=0)
	if state.ShadowBranchSuffix == 0 {
		return s.handleLegacySuffix(repo, state)
	}

	// Check if current suffix's branch exists
	currentBranch := checkpoint.ShadowBranchNameForCommitWithSuffix(state.BaseCommit[:checkpoint.ShadowBranchHashLength], state.ShadowBranchSuffix)
	if !shadowBranchExists(repo, currentBranch) {
		// Branch doesn't exist yet, continue with current suffix
		return state.ShadowBranchSuffix, false, nil
	}

	// If continuing an existing session (CheckpointCount > 0), always continue on the same branch.
	// The "dismissal" detection logic only applies at the START of a new session when the user
	// might have dismissed the previous session's work. Within a session, the agent is expected
	// to modify its own work, so we should continue on the same branch.
	if state.CheckpointCount > 0 {
		return state.ShadowBranchSuffix, false, nil
	}

	// Step 1: Clean worktree → new suffix
	clean, err := isWorktreeClean(repo)
	if err != nil {
		return 0, false, fmt.Errorf("failed to check worktree status: %w", err)
	}
	if clean {
		return state.ShadowBranchSuffix + 1, true, nil
	}

	// Step 2: Get files from shadow branch and worktree
	shadowFiles, err := getFilesFromShadowBranch(repo, state.BaseCommit, currentBranch)
	if err != nil {
		// If we can't read shadow branch, continue with current suffix
		return state.ShadowBranchSuffix, false, nil //nolint:nilerr // Continue on error is by design
	}

	worktreeFiles, err := getModifiedWorktreeFiles(repo)
	if err != nil {
		// If we can't get worktree files, continue with current suffix
		return state.ShadowBranchSuffix, false, nil //nolint:nilerr // Continue on error is by design
	}

	// Step 3: Check overlap
	overlap := findFileOverlap(worktreeFiles, shadowFiles)
	if len(overlap) == 0 {
		return state.ShadowBranchSuffix + 1, true, nil
	}

	// Step 4: Check if agent's lines are preserved in overlapping files
	preserved, err := checkAgentLinesPreserved(repo, state.BaseCommit, currentBranch, overlap)
	if err != nil {
		// If we can't check, err on the side of continuing
		return state.ShadowBranchSuffix, false, nil //nolint:nilerr // Continue on error is by design
	}
	if preserved {
		return state.ShadowBranchSuffix, false, nil
	}

	// Agent's lines were dismissed
	return state.ShadowBranchSuffix + 1, true, nil
}

// handleLegacySuffix handles sessions with suffix=0 (legacy or new session).
// If a legacy branch exists (entire/<hash>), it renames it to suffixed format (entire/<hash>-1).
func (s *ManualCommitStrategy) handleLegacySuffix(repo *git.Repository, state *SessionState) (int, bool, error) {
	baseCommitShort := state.BaseCommit
	if len(baseCommitShort) > checkpoint.ShadowBranchHashLength {
		baseCommitShort = baseCommitShort[:checkpoint.ShadowBranchHashLength]
	}

	legacyBranch := checkpoint.ShadowBranchNameForCommit(state.BaseCommit)
	legacyRefName := plumbing.NewBranchReferenceName(legacyBranch)

	// Check if legacy branch exists
	ref, err := repo.Reference(legacyRefName, true)
	if err != nil {
		// No legacy branch, start fresh at suffix 1
		return 1, true, nil //nolint:nilerr // Reference not found is expected case
	}

	// Legacy branch exists - rename to suffixed format
	newBranch := checkpoint.ShadowBranchNameForCommitWithSuffix(baseCommitShort, 1)
	newRefName := plumbing.NewBranchReferenceName(newBranch)

	// Create new reference pointing to the same commit
	newRef := plumbing.NewHashReference(newRefName, ref.Hash())
	if err := repo.Storer.SetReference(newRef); err != nil {
		return 0, false, fmt.Errorf("failed to create suffixed branch: %w", err)
	}

	// Delete legacy reference
	if err := repo.Storer.RemoveReference(legacyRefName); err != nil {
		return 0, false, fmt.Errorf("failed to delete legacy branch: %w", err)
	}

	// Return suffix 1, continuing on the migrated branch
	return 1, false, nil
}

// shadowBranchExists checks if a shadow branch exists.
func shadowBranchExists(repo *git.Repository, branchName string) bool {
	refName := plumbing.NewBranchReferenceName(branchName)
	_, err := repo.Reference(refName, true)
	return err == nil
}

// isWorktreeClean checks if the worktree has any changes (excluding .entire/ and .git/).
// Includes untracked files because agent-created files appear as untracked relative to HEAD.
func isWorktreeClean(repo *git.Repository) (bool, error) {
	worktree, err := repo.Worktree()
	if err != nil {
		return false, fmt.Errorf("failed to get worktree: %w", err)
	}

	status, err := worktree.Status()
	if err != nil {
		return false, fmt.Errorf("failed to get status: %w", err)
	}

	for file, fileStatus := range status {
		// Skip .entire/ and .git/ directories
		if strings.HasPrefix(file, ".entire/") || strings.HasPrefix(file, ".git/") {
			continue
		}
		// Any change (including untracked files) means not clean
		// Untracked files are included because agent-created files appear as untracked
		if fileStatus.Worktree != ' ' {
			return false, nil
		}
		if fileStatus.Staging != ' ' {
			return false, nil
		}
	}

	return true, nil
}

// getFilesFromShadowBranch returns files that were modified between base commit and shadow branch tip.
func getFilesFromShadowBranch(repo *git.Repository, baseCommit, shadowBranch string) (map[string]bool, error) {
	// Get base tree
	baseHash := plumbing.NewHash(baseCommit)
	baseCommitObj, err := repo.CommitObject(baseHash)
	if err != nil {
		return nil, fmt.Errorf("failed to get base commit: %w", err)
	}
	baseTree, err := baseCommitObj.Tree()
	if err != nil {
		return nil, fmt.Errorf("failed to get base tree: %w", err)
	}

	// Get shadow branch tip tree
	shadowRef, err := repo.Reference(plumbing.NewBranchReferenceName(shadowBranch), true)
	if err != nil {
		return nil, fmt.Errorf("failed to get shadow branch: %w", err)
	}
	shadowCommit, err := repo.CommitObject(shadowRef.Hash())
	if err != nil {
		return nil, fmt.Errorf("failed to get shadow commit: %w", err)
	}
	shadowTree, err := shadowCommit.Tree()
	if err != nil {
		return nil, fmt.Errorf("failed to get shadow tree: %w", err)
	}

	// Compare trees to find modified files
	changes, err := baseTree.Diff(shadowTree)
	if err != nil {
		return nil, fmt.Errorf("failed to diff trees: %w", err)
	}

	files := make(map[string]bool)
	for _, change := range changes {
		// Get the file path (handles both additions and modifications)
		var path string
		if change.From.Name != "" {
			path = change.From.Name
		} else if change.To.Name != "" {
			path = change.To.Name
		}
		if path != "" && !strings.HasPrefix(path, ".entire/") {
			files[path] = true
		}
	}

	return files, nil
}

// getModifiedWorktreeFiles returns files that are modified or untracked in the worktree.
// Includes untracked files because files created by the agent but not committed to HEAD
// appear as untracked, and we need to detect overlap with shadow branch files.
func getModifiedWorktreeFiles(repo *git.Repository) (map[string]bool, error) {
	worktree, err := repo.Worktree()
	if err != nil {
		return nil, fmt.Errorf("failed to get worktree: %w", err)
	}

	status, err := worktree.Status()
	if err != nil {
		return nil, fmt.Errorf("failed to get status: %w", err)
	}

	files := make(map[string]bool)
	for file, fileStatus := range status {
		// Skip .entire/ and .git/
		if strings.HasPrefix(file, ".entire/") || strings.HasPrefix(file, ".git/") {
			continue
		}
		// Any change counts - including untracked files
		// Untracked files (Worktree == '?') are included because files created by
		// the agent appear as untracked relative to HEAD
		if fileStatus.Worktree != ' ' {
			files[file] = true
		}
		if fileStatus.Staging != ' ' {
			files[file] = true
		}
	}

	return files, nil
}

// findFileOverlap returns files that exist in both sets.
func findFileOverlap(worktreeFiles, shadowFiles map[string]bool) []string {
	var overlap []string
	for file := range worktreeFiles {
		if shadowFiles[file] {
			overlap = append(overlap, file)
		}
	}
	return overlap
}

// checkAgentLinesPreserved checks if any of the agent's added lines are still present
// in the worktree for the given overlapping files.
func checkAgentLinesPreserved(repo *git.Repository, baseCommit, shadowBranch string, files []string) (bool, error) {
	// Get base tree
	baseHash := plumbing.NewHash(baseCommit)
	baseCommitObj, err := repo.CommitObject(baseHash)
	if err != nil {
		return false, fmt.Errorf("failed to get base commit: %w", err)
	}
	baseTree, err := baseCommitObj.Tree()
	if err != nil {
		return false, fmt.Errorf("failed to get base tree: %w", err)
	}

	// Get shadow tree
	shadowRef, err := repo.Reference(plumbing.NewBranchReferenceName(shadowBranch), true)
	if err != nil {
		return false, fmt.Errorf("failed to get shadow branch: %w", err)
	}
	shadowCommit, err := repo.CommitObject(shadowRef.Hash())
	if err != nil {
		return false, fmt.Errorf("failed to get shadow commit: %w", err)
	}
	shadowTree, err := shadowCommit.Tree()
	if err != nil {
		return false, fmt.Errorf("failed to get shadow tree: %w", err)
	}

	// Get worktree root from the repo object
	worktree, err := repo.Worktree()
	if err != nil {
		return false, fmt.Errorf("failed to get worktree: %w", err)
	}
	repoRoot := worktree.Filesystem.Root()

	// Check each overlapping file
	for _, file := range files {
		baseContent := getFileContentFromTree(baseTree, file)
		shadowContent := getFileContentFromTree(shadowTree, file)
		workContent, err := os.ReadFile(filepath.Join(repoRoot, file)) //nolint:gosec // file is from git status
		if err != nil {
			continue // File might have been deleted
		}

		if agentLinesPreserved(baseContent, shadowContent, string(workContent)) {
			return true, nil
		}
	}

	return false, nil
}

// getFileContentFromTree reads file content from a git tree. Returns empty string if not found.
func getFileContentFromTree(tree *object.Tree, filename string) string {
	file, err := tree.File(filename)
	if err != nil {
		return ""
	}
	content, err := file.Contents()
	if err != nil {
		return ""
	}
	return content
}

// agentLinesPreserved checks if any lines added by the agent (present in shadow but not base)
// are still present in the worktree content.
func agentLinesPreserved(baseContent, shadowContent, workContent string) bool {
	baseLines := toLineSet(baseContent)
	shadowLines := toLineSet(shadowContent)
	workLines := toLineSet(workContent)

	// Find lines added by agent (in shadow but not in base)
	for line := range shadowLines {
		if !baseLines[line] && workLines[line] {
			// This line was added by agent and is still in worktree
			return true
		}
	}

	return false
}

// toLineSet converts content to a set of lines for comparison.
func toLineSet(content string) map[string]bool {
	lines := make(map[string]bool)
	for _, line := range strings.Split(content, "\n") {
		// Trim trailing whitespace for comparison
		trimmed := strings.TrimRight(line, " \t\r")
		if trimmed != "" {
			lines[trimmed] = true
		}
	}
	return lines
}
