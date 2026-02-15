package strategy

import (
	"context"
	"log/slog"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// Content-aware overlap detection for checkpoint management.
//
// These functions determine whether a commit contains session-related work by comparing
// file content (not just filenames) against the shadow branch. This enables accurate
// detection of the "reverted and replaced" scenario where a user:
// 1. Reverts session changes (e.g., git checkout -- file.txt)
// 2. Creates completely different content in the same file
// 3. Commits the new content
//
// In this scenario, the commit should NOT get a checkpoint trailer because the
// session's work was discarded, not incorporated.
//
// The key distinction:
// - Modified files (exist in parent commit): Always count as overlap, regardless of
//   content changes. The user is editing session's work.
// - New files (don't exist in parent): Require content match against shadow branch.
//   If content differs completely, the session's work was likely reverted & replaced.

// filesOverlapWithContent checks if any file in filesTouched overlaps with the committed
// content, using content-aware comparison to detect the "reverted and replaced" scenario.
//
// This is used in PostCommit to determine if a session has work in the commit.
func filesOverlapWithContent(repo *git.Repository, shadowBranchName string, headCommit *object.Commit, filesTouched []string) bool {
	logCtx := logging.WithComponent(context.Background(), "checkpoint")

	// Build set of filesTouched for quick lookup
	touchedSet := make(map[string]bool)
	for _, f := range filesTouched {
		touchedSet[f] = true
	}

	// Get HEAD commit tree (the committed content)
	headTree, err := headCommit.Tree()
	if err != nil {
		logging.Debug(logCtx, "filesOverlapWithContent: failed to get HEAD tree, falling back to filename check",
			slog.String("error", err.Error()),
		)
		return len(filesTouched) > 0 // Fall back: assume overlap if any files touched
	}

	// Get shadow branch tree (the session's content)
	refName := plumbing.NewBranchReferenceName(shadowBranchName)
	shadowRef, err := repo.Reference(refName, true)
	if err != nil {
		logging.Debug(logCtx, "filesOverlapWithContent: shadow branch not found, falling back to filename check",
			slog.String("branch", shadowBranchName),
			slog.String("error", err.Error()),
		)
		return len(filesTouched) > 0 // Fall back: assume overlap if any files touched
	}

	shadowCommit, err := repo.CommitObject(shadowRef.Hash())
	if err != nil {
		logging.Debug(logCtx, "filesOverlapWithContent: failed to get shadow commit, falling back to filename check",
			slog.String("error", err.Error()),
		)
		return len(filesTouched) > 0
	}

	shadowTree, err := shadowCommit.Tree()
	if err != nil {
		logging.Debug(logCtx, "filesOverlapWithContent: failed to get shadow tree, falling back to filename check",
			slog.String("error", err.Error()),
		)
		return len(filesTouched) > 0
	}

	// Get the parent commit tree to determine if files are modified vs newly created.
	// For modified files (exist in parent), we count as overlap regardless of content
	// because the user is editing the session's work.
	// For newly created files (don't exist in parent), we check content to detect
	// the "reverted and replaced" scenario where user deleted session's work and
	// created something completely different.
	var parentTree *object.Tree
	if headCommit.NumParents() > 0 {
		if parent, err := headCommit.Parent(0); err == nil {
			if pTree, err := parent.Tree(); err == nil {
				parentTree = pTree
			}
		}
	}

	// Check each file in filesTouched
	for _, filePath := range filesTouched {
		// Get file from HEAD tree (the committed content)
		headFile, err := headTree.File(filePath)
		if err != nil {
			// File not in HEAD commit. This happens when:
			// - The session created/modified the file but user deleted it before committing
			// - The file was staged as a deletion (git rm)
			// In both cases, the session's work on this file is not in the commit,
			// so it doesn't contribute to overlap. Continue checking other files.
			continue
		}

		// Check if this is a modified file (exists in parent) or new file
		isModified := false
		if parentTree != nil {
			if _, err := parentTree.File(filePath); err == nil {
				isModified = true
			}
		}

		// Modified files always count as overlap (user edited session's work)
		if isModified {
			logging.Debug(logCtx, "filesOverlapWithContent: modified file counts as overlap",
				slog.String("file", filePath),
			)
			return true
		}

		// For new files, check content against shadow branch
		shadowFile, err := shadowTree.File(filePath)
		if err != nil {
			// File not in shadow branch - this shouldn't happen but skip it
			logging.Debug(logCtx, "filesOverlapWithContent: file in filesTouched but not in shadow branch",
				slog.String("file", filePath),
			)
			continue
		}

		// Compare by hash (blob hash) - exact content match required for new files
		if headFile.Hash == shadowFile.Hash {
			logging.Debug(logCtx, "filesOverlapWithContent: new file content match found",
				slog.String("file", filePath),
				slog.String("hash", headFile.Hash.String()),
			)
			return true
		}

		logging.Debug(logCtx, "filesOverlapWithContent: new file content mismatch (may be reverted & replaced)",
			slog.String("file", filePath),
			slog.String("head_hash", headFile.Hash.String()),
			slog.String("shadow_hash", shadowFile.Hash.String()),
		)
	}

	logging.Debug(logCtx, "filesOverlapWithContent: no overlapping files found",
		slog.Int("files_checked", len(filesTouched)),
	)
	return false
}

// stagedFilesOverlapWithContent checks if any staged file overlaps with filesTouched,
// distinguishing between modified files (always overlap) and new files (check content).
//
// For modified files (already exist in HEAD), we count as overlap because the user
// is editing the session's work. For new files (don't exist in HEAD), we require
// content match to detect the "reverted and replaced" scenario.
//
// This is used in PrepareCommitMsg for carry-forward scenarios.
func stagedFilesOverlapWithContent(repo *git.Repository, shadowTree *object.Tree, stagedFiles, filesTouched []string) bool {
	logCtx := logging.WithComponent(context.Background(), "checkpoint")

	// Build set of filesTouched for quick lookup
	touchedSet := make(map[string]bool)
	for _, f := range filesTouched {
		touchedSet[f] = true
	}

	// Get HEAD tree to determine if files are being modified or newly created
	head, err := repo.Head()
	if err != nil {
		logging.Debug(logCtx, "stagedFilesOverlapWithContent: failed to get HEAD, falling back to filename check",
			slog.String("error", err.Error()),
		)
		return hasOverlappingFiles(stagedFiles, filesTouched)
	}
	headCommit, err := repo.CommitObject(head.Hash())
	if err != nil {
		logging.Debug(logCtx, "stagedFilesOverlapWithContent: failed to get HEAD commit, falling back to filename check",
			slog.String("error", err.Error()),
		)
		return hasOverlappingFiles(stagedFiles, filesTouched)
	}
	headTree, err := headCommit.Tree()
	if err != nil {
		logging.Debug(logCtx, "stagedFilesOverlapWithContent: failed to get HEAD tree, falling back to filename check",
			slog.String("error", err.Error()),
		)
		return hasOverlappingFiles(stagedFiles, filesTouched)
	}

	// Get the git index to access staged file hashes
	idx, err := repo.Storer.Index()
	if err != nil {
		logging.Debug(logCtx, "stagedFilesOverlapWithContent: failed to get index, falling back to filename check",
			slog.String("error", err.Error()),
		)
		return hasOverlappingFiles(stagedFiles, filesTouched)
	}

	// Build a map of index entries for O(1) lookup (avoid O(n*m) nested loop)
	indexEntries := make(map[string]plumbing.Hash, len(idx.Entries))
	for _, entry := range idx.Entries {
		indexEntries[entry.Name] = entry.Hash
	}

	// Check each staged file
	for _, stagedPath := range stagedFiles {
		if !touchedSet[stagedPath] {
			continue // Not in filesTouched, skip
		}

		// Check if this is a modified file (exists in HEAD) or new file
		_, headErr := headTree.File(stagedPath)
		isModified := headErr == nil

		// Modified files always count as overlap (user edited session's work)
		if isModified {
			logging.Debug(logCtx, "stagedFilesOverlapWithContent: modified file counts as overlap",
				slog.String("file", stagedPath),
			)
			return true
		}

		// For new files, check content against shadow branch
		stagedHash, found := indexEntries[stagedPath]
		if !found {
			continue // Not in index (shouldn't happen but be safe)
		}

		// Get file from shadow branch tree
		shadowFile, err := shadowTree.File(stagedPath)
		if err != nil {
			// File not in shadow branch - doesn't count as content match
			logging.Debug(logCtx, "stagedFilesOverlapWithContent: file not in shadow tree",
				slog.String("file", stagedPath),
			)
			continue
		}

		// Compare hashes - for new files, require exact content match
		if stagedHash == shadowFile.Hash {
			logging.Debug(logCtx, "stagedFilesOverlapWithContent: new file content match found",
				slog.String("file", stagedPath),
				slog.String("hash", stagedHash.String()),
			)
			return true
		}

		logging.Debug(logCtx, "stagedFilesOverlapWithContent: new file content mismatch (may be reverted & replaced)",
			slog.String("file", stagedPath),
			slog.String("staged_hash", stagedHash.String()),
			slog.String("shadow_hash", shadowFile.Hash.String()),
		)
	}

	logging.Debug(logCtx, "stagedFilesOverlapWithContent: no overlapping files found",
		slog.Int("staged_files", len(stagedFiles)),
		slog.Int("files_touched", len(filesTouched)),
	)
	return false
}

// hasOverlappingFiles checks if any file in stagedFiles appears in filesTouched.
// This is a fallback when content-aware comparison isn't possible.
func hasOverlappingFiles(stagedFiles, filesTouched []string) bool {
	touchedSet := make(map[string]bool)
	for _, f := range filesTouched {
		touchedSet[f] = true
	}

	for _, staged := range stagedFiles {
		if touchedSet[staged] {
			return true
		}
	}
	return false
}

// filesWithRemainingAgentChanges returns files from filesTouched that still have
// uncommitted agent changes. This is used for carry-forward after partial commits.
//
// A file has remaining agent changes if:
//   - It wasn't committed at all (not in committedFiles), OR
//   - It was committed but the committed content doesn't match the shadow branch
//     (user committed partial changes, e.g., via git add -p)
//
// Falls back to file-level subtraction if shadow branch is unavailable.
func filesWithRemainingAgentChanges(
	repo *git.Repository,
	shadowBranchName string,
	headCommit *object.Commit,
	filesTouched []string,
	committedFiles map[string]struct{},
) []string {
	logCtx := logging.WithComponent(context.Background(), "checkpoint")

	// Get HEAD commit tree (the committed content)
	commitTree, err := headCommit.Tree()
	if err != nil {
		logging.Debug(logCtx, "filesWithRemainingAgentChanges: failed to get commit tree, falling back to file subtraction",
			slog.String("error", err.Error()),
		)
		return subtractFilesByName(filesTouched, committedFiles)
	}

	// Get shadow branch tree (the session's full content)
	refName := plumbing.NewBranchReferenceName(shadowBranchName)
	shadowRef, err := repo.Reference(refName, true)
	if err != nil {
		logging.Debug(logCtx, "filesWithRemainingAgentChanges: shadow branch not found, falling back to file subtraction",
			slog.String("branch", shadowBranchName),
			slog.String("error", err.Error()),
		)
		return subtractFilesByName(filesTouched, committedFiles)
	}

	shadowCommit, err := repo.CommitObject(shadowRef.Hash())
	if err != nil {
		logging.Debug(logCtx, "filesWithRemainingAgentChanges: failed to get shadow commit, falling back to file subtraction",
			slog.String("error", err.Error()),
		)
		return subtractFilesByName(filesTouched, committedFiles)
	}

	shadowTree, err := shadowCommit.Tree()
	if err != nil {
		logging.Debug(logCtx, "filesWithRemainingAgentChanges: failed to get shadow tree, falling back to file subtraction",
			slog.String("error", err.Error()),
		)
		return subtractFilesByName(filesTouched, committedFiles)
	}

	var remaining []string

	for _, filePath := range filesTouched {
		// If file wasn't committed at all, it definitely has remaining changes
		if _, wasCommitted := committedFiles[filePath]; !wasCommitted {
			remaining = append(remaining, filePath)
			logging.Debug(logCtx, "filesWithRemainingAgentChanges: file not committed, keeping",
				slog.String("file", filePath),
			)
			continue
		}

		// File was committed - check if committed content matches shadow branch
		shadowFile, err := shadowTree.File(filePath)
		if err != nil {
			// File not in shadow branch - nothing to carry forward for this file
			logging.Debug(logCtx, "filesWithRemainingAgentChanges: file not in shadow branch, skipping",
				slog.String("file", filePath),
			)
			continue
		}

		commitFile, err := commitTree.File(filePath)
		if err != nil {
			// File not in commit tree (deleted?) - keep it if it's in shadow
			remaining = append(remaining, filePath)
			logging.Debug(logCtx, "filesWithRemainingAgentChanges: file not in commit tree but in shadow, keeping",
				slog.String("file", filePath),
			)
			continue
		}

		// Compare hashes - if different, there are still uncommitted agent changes
		if commitFile.Hash != shadowFile.Hash {
			remaining = append(remaining, filePath)
			logging.Debug(logCtx, "filesWithRemainingAgentChanges: content mismatch, keeping for carry-forward",
				slog.String("file", filePath),
				slog.String("commit_hash", commitFile.Hash.String()[:7]),
				slog.String("shadow_hash", shadowFile.Hash.String()[:7]),
			)
		} else {
			logging.Debug(logCtx, "filesWithRemainingAgentChanges: content fully committed",
				slog.String("file", filePath),
			)
		}
	}

	logging.Debug(logCtx, "filesWithRemainingAgentChanges: result",
		slog.Int("files_touched", len(filesTouched)),
		slog.Int("committed_files", len(committedFiles)),
		slog.Int("remaining_files", len(remaining)),
	)

	return remaining
}

// subtractFilesByName returns files from filesTouched that are NOT in committedFiles.
// This is a fallback when content-aware comparison isn't possible.
func subtractFilesByName(filesTouched []string, committedFiles map[string]struct{}) []string {
	var remaining []string
	for _, f := range filesTouched {
		if _, committed := committedFiles[f]; !committed {
			remaining = append(remaining, f)
		}
	}
	return remaining
}
