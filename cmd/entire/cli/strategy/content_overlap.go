package strategy

import (
	"context"
	"log/slog"
	"os"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/worktreedir"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/format/config"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// truncateStringSlice returns the first n elements of a slice, for concise logging.
func truncateStringSlice(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// Content-aware overlap detection for checkpoint management.
//
// These functions determine whether a commit contains session-related work by comparing
// committed (or staged) blob hashes against the hashes recorded at turn end in
// SessionState.TouchedFileHashes (see touched_file_hashes.go). This enables
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
// - New files (don't exist in parent): Require the committed blob to equal the
//   recorded one. If it differs, the session's work was likely reverted & replaced.
//
// A path with no recorded hash is matched by name, so "reverted and replaced"
// detection does not apply to it. That covers every path that reached
// FilesTouched through a task record or a per-tool hook rather than a turn-end
// step (MergeUnhashedFilesTouched), as well as symlinks and unhashable files.

// overlapOpts provides pre-resolved git objects to avoid redundant reads.
// When fields are non-nil, they are used directly instead of reading from the repo.
type overlapOpts struct {
	headTree      *object.Tree // HEAD commit tree
	parentTree    *object.Tree // HEAD's first parent tree (nil = initial commit or not provided)
	hasParentTree bool         // True if parentTree was explicitly resolved (distinguishes nil-not-resolved from nil-initial-commit)
}

// filesOverlapWithContent checks if any file in filesTouched overlaps with the committed
// content, using the recorded turn-end hashes to detect the "reverted and
// replaced" scenario.
//
// This is used in PostCommit to determine if a session has work in the commit.
func filesOverlapWithContent(ctx context.Context, hashes map[string]string, headCommit *object.Commit, filesTouched []string, opts ...overlapOpts) bool {
	logCtx := logging.WithComponent(ctx, "checkpoint")

	// Use pre-resolved trees if provided, otherwise resolve from repo.
	var o overlapOpts
	if len(opts) > 0 {
		o = opts[0]
	}

	headTree := o.headTree
	if headTree == nil {
		var err error
		headTree, err = headCommit.Tree()
		if err != nil {
			logging.Debug(logCtx, "filesOverlapWithContent: failed to get HEAD tree, falling back to filename check",
				slog.String("error", err.Error()),
			)
			return len(filesTouched) > 0
		}
	}

	parentTree := o.parentTree
	if parentTree == nil && !o.hasParentTree {
		if headCommit.NumParents() > 0 {
			if parent, err := headCommit.Parent(0); err == nil {
				if pTree, err := parent.Tree(); err == nil {
					parentTree = pTree
				}
			}
		}
	}

	// Check each file in filesTouched
	for _, filePath := range filesTouched {
		// Get file from HEAD tree (the committed content)
		headFile, err := headTree.File(filePath)
		if err != nil {
			// File not in HEAD commit. Check if this is a deletion (existed in parent).
			// Deletions count as overlap because the agent's action (deleting the file)
			// is being committed - we want the session context linked to this commit.
			if parentTree != nil {
				if _, parentErr := parentTree.File(filePath); parentErr == nil {
					// File existed in parent but not in HEAD - this is a deletion
					logging.Debug(logCtx, "filesOverlapWithContent: deleted file counts as overlap",
						slog.String("file", filePath),
					)
					return true
				}
			}
			// File didn't exist in parent either - not a deletion, skip
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

		// For new files, compare the committed blob with the recorded one.
		if newFileMatchesRecorded(logCtx, "filesOverlapWithContent", hashes, filePath, headFile.Hash) {
			return true
		}
	}

	logging.Debug(logCtx, "filesOverlapWithContent: no overlapping files found",
		slog.Int("files_checked", len(filesTouched)),
	)
	return false
}

// newFileMatchesRecorded reports whether a newly added file's committed or
// staged blob is the session's work: equal to the hash recorded at turn end,
// or — when no hash was recorded — present by name. A recorded deletion never
// matches: the agent deleted the path and someone else re-created it.
func newFileMatchesRecorded(logCtx context.Context, caller string, hashes map[string]string, filePath string, blobHash plumbing.Hash) bool {
	recorded, ok, deleted := recordedFileHash(hashes, filePath)
	switch {
	case !ok:
		logging.Debug(logCtx, caller+": new file without recorded hash, matching by name",
			slog.String("file", filePath),
		)
		return true
	case deleted:
		logging.Debug(logCtx, caller+": new file the session recorded as deleted",
			slog.String("file", filePath),
		)
		return false
	case blobHash.Equal(recorded):
		// Equal, not ==: plumbing.Hash carries an object-format field
		// alongside its bytes; see filesWithRemainingAgentChanges.
		logging.Debug(logCtx, caller+": new file content match found",
			slog.String("file", filePath),
			slog.String("hash", blobHash.String()),
		)
		return true
	default:
		logging.Debug(logCtx, caller+": new file content mismatch (may be reverted & replaced)",
			slog.String("file", filePath),
			slog.String("committed_hash", blobHash.String()),
			slog.String("recorded_hash", recorded.String()),
		)
		return false
	}
}

// stagedFilesOverlapWithContent checks if any staged file overlaps with filesTouched,
// distinguishing between modified files (always overlap) and new files (check content).
//
// For modified files (already exist in HEAD), we count as overlap because the user
// is editing the session's work. For new files (don't exist in HEAD), the
// staged blob must equal the recorded turn-end hash, which detects the
// "reverted and replaced" scenario. A new file staged with different content
// than the agent left does not count, including a partially staged one.
//
// This is used in PrepareCommitMsg.
func stagedFilesOverlapWithContent(ctx context.Context, repo *git.Repository, hashes map[string]string, stagedFiles, filesTouched []string) bool {
	logCtx := logging.WithComponent(ctx, "checkpoint")

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
			logging.Debug(logCtx, "stagedFilesOverlapWithContent: staged file not in files_touched, skipping",
				slog.String("staged_file", stagedPath),
			)
			continue // Not in filesTouched, skip
		}

		// Check if this is a modified file (exists in HEAD) or new file
		_, headErr := headTree.File(stagedPath)
		isModified := headErr == nil

		// Modified files always count as overlap (user edited session's work)
		// This includes deletions - if file exists in HEAD and is being deleted,
		// that's the agent's work being committed.
		if isModified {
			logging.Debug(logCtx, "stagedFilesOverlapWithContent: modified file counts as overlap",
				slog.String("file", stagedPath),
			)
			return true
		}

		// For new files, compare the staged blob with the recorded one.
		stagedHash, found := indexEntries[stagedPath]
		if !found {
			continue // Not in index (shouldn't happen but be safe)
		}
		if newFileMatchesRecorded(logCtx, "stagedFilesOverlapWithContent", hashes, stagedPath, stagedHash) {
			return true
		}
	}

	logging.Debug(logCtx, "stagedFilesOverlapWithContent: no overlapping files found",
		slog.Int("staged_files", len(stagedFiles)),
		slog.Int("files_touched", len(filesTouched)),
		slog.Any("staged_paths", truncateStringSlice(stagedFiles, 10)),
		slog.Any("touched_paths", truncateStringSlice(filesTouched, 10)),
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
// hashes is the session's TouchedFileHashes as they were before condensation.
//
// A file with a recorded hash has remaining agent changes if:
//   - It wasn't committed at all (not in committedFiles), OR
//   - It was committed but the committed blob doesn't match the recorded hash
//     AND the working tree still has changes (e.g., user did git add -p)
//
// When the committed blob differs from the recorded one but the working tree
// is clean (matches the commit), the user intentionally wrote different
// content — there is nothing left to carry forward.
//
// Files recorded as agent deletions are never carried forward, and neither is
// a file without a recorded hash that was committed or is missing from the
// worktree (the phantom-path guard: transcript parsing can name files the
// agent never created, and carrying those forward would never end).
func filesWithRemainingAgentChanges(
	ctx context.Context,
	repo *git.Repository,
	hashes map[string]string,
	headCommit *object.Commit,
	filesTouched []string,
	committedFiles map[string]struct{},
	opts ...overlapOpts,
) []string {
	logCtx := logging.WithComponent(ctx, "checkpoint")

	// Use pre-resolved trees if provided, otherwise resolve from repo.
	var o overlapOpts
	if len(opts) > 0 {
		o = opts[0]
	}

	commitTree := o.headTree
	if commitTree == nil {
		var err error
		commitTree, err = headCommit.Tree()
		if err != nil {
			logging.Debug(logCtx, "filesWithRemainingAgentChanges: failed to get commit tree, falling back to file subtraction",
				slog.String("error", err.Error()),
			)
			return subtractFilesByName(ctx, filesTouched, committedFiles)
		}
	}

	// Get worktree root for working tree checks when committed content
	// differs from the recorded hash (distinguishes replacement from partial
	// staging), and for the phantom-path guard.
	var worktreeRoot string
	if wt, wtErr := repo.Worktree(); wtErr == nil {
		worktreeRoot = wt.Filesystem().Root()
	}
	var root *os.Root
	if worktreeRoot != "" {
		if r, rootErr := worktreedir.OpenAt(worktreeRoot); rootErr == nil {
			root = r
		}
	}

	type worktreeCandidate struct {
		index        int
		path         string
		commitHash   plumbing.Hash
		commitMode   filemode.FileMode
		recordedHash plumbing.Hash
	}
	keep := make([]bool, len(filesTouched))
	var candidates []worktreeCandidate

	for i, filePath := range filesTouched {
		_, wasCommitted := committedFiles[filePath]
		recorded, hasHash, deleted := recordedFileHash(hashes, filePath)

		switch {
		case deleted:
			// Agent deletion: there is no content on disk to carry forward.
			logging.Debug(logCtx, "filesWithRemainingAgentChanges: recorded deletion, skipping",
				slog.String("file", filePath),
			)
			continue
		case !hasHash && wasCommitted:
			logging.Debug(logCtx, "filesWithRemainingAgentChanges: committed file without recorded hash, dropping by name",
				slog.String("file", filePath),
			)
			continue
		case !hasHash:
			// Phantom guard: a path the agent never actually produced.
			if root != nil && !worktreeEntryExists(root, worktreeRoot, filePath) {
				logging.Debug(logCtx, "filesWithRemainingAgentChanges: file without recorded hash missing from worktree, skipping",
					slog.String("file", filePath),
				)
				continue
			}
			keep[i] = true
			continue
		case !wasCommitted:
			// File wasn't committed at all — it has remaining changes
			keep[i] = true
			logging.Debug(logCtx, "filesWithRemainingAgentChanges: file not committed, keeping",
				slog.String("file", filePath),
			)
			continue
		}

		commitFile, err := commitTree.File(filePath)
		if err != nil {
			// File not in commit tree (the commit deleted it) but the agent
			// left content for it — keep it.
			keep[i] = true
			logging.Debug(logCtx, "filesWithRemainingAgentChanges: file not in commit tree but has recorded content, keeping",
				slog.String("file", filePath),
			)
			continue
		}

		if commitFile.Hash.Equal(recorded) {
			logging.Debug(logCtx, "filesWithRemainingAgentChanges: content fully committed",
				slog.String("file", filePath),
			)
			continue
		}

		candidates = append(candidates, worktreeCandidate{
			index:        i,
			path:         filePath,
			commitHash:   commitFile.Hash,
			commitMode:   commitFile.Mode,
			recordedHash: recorded,
		})
	}

	worktreeHashes := make(map[string]plumbing.Hash)
	if worktreeRoot != "" && len(candidates) > 0 {
		paths := make([]string, 0, len(candidates))
		for _, candidate := range candidates {
			// hash-object follows symlinks and hashes target content, while a Git
			// symlink blob stores the target path. Compare either side of a mode
			// mismatch through the confined fallback instead.
			if worktreedir.HashableEntry(worktreeRoot, candidate.path, candidate.commitMode) {
				paths = append(paths, candidate.path)
			}
		}
		var err error
		worktreeHashes, err = gitrepo.HashWorktreeFiles(ctx, worktreeRoot, paths)
		if err != nil {
			logging.Warn(logCtx, "native git could not hash every carry-forward candidate; checking failed paths conservatively without clean filters",
				slog.String("error", err.Error()),
			)
		}
	}

	for _, candidate := range candidates {
		workingTreeClean := false
		if worktreeHash, ok := worktreeHashes[candidate.path]; ok {
			// Equal, not ==: plumbing.Hash carries an object-format field
			// alongside its bytes, and `==` compares that field too. FromHex
			// leaves it unset for a 40-char hash while stamping SHA256 on a
			// 64-char one, so `==` only works while the tree decoder happens to
			// agree. If it ever stamped "sha1", every candidate would read dirty
			// and the phantom carry-forward would return with no test failing.
			workingTreeClean = worktreeHash.Equal(candidate.commitHash)
		} else if worktreeRoot != "" {
			workingTreeClean = workingTreeMatchesBlob(worktreeRoot, candidate.path, candidate.commitMode, candidate.commitHash)
		}
		if workingTreeClean {
			logging.Debug(logCtx, "filesWithRemainingAgentChanges: content differs from recorded but working tree is clean, skipping",
				slog.String("file", candidate.path),
				slog.String("commit_hash", candidate.commitHash.String()[:7]),
				slog.String("recorded_hash", candidate.recordedHash.String()[:7]),
			)
			continue
		}

		keep[candidate.index] = true
		logging.Debug(logCtx, "filesWithRemainingAgentChanges: content mismatch with dirty working tree, keeping for carry-forward",
			slog.String("file", candidate.path),
			slog.String("commit_hash", candidate.commitHash.String()[:7]),
			slog.String("recorded_hash", candidate.recordedHash.String()[:7]),
		)
	}

	remaining := make([]string, 0, len(filesTouched))
	for i, filePath := range filesTouched {
		if keep[i] {
			remaining = append(remaining, filePath)
		}
	}

	logging.Debug(logCtx, "filesWithRemainingAgentChanges: result",
		slog.Int("files_touched", len(filesTouched)),
		slog.Int("committed_files", len(committedFiles)),
		slog.Int("remaining_files", len(remaining)),
	)

	return remaining
}

// workingTreeMatchesBlob checks whether the raw file representation hashes to
// commitHash. It is the filter-unaware fallback for when native Git cannot hash
// a regular file and the symlink-aware path for Git symlink blobs.
func workingTreeMatchesBlob(worktreeRoot, filePath string, commitMode filemode.FileMode, commitHash plumbing.Hash) bool {
	root, err := worktreedir.OpenAt(worktreeRoot)
	if err != nil {
		return false
	}
	name, err := worktreedir.Name(worktreeRoot, filePath)
	if err != nil {
		return false
	}
	var diskContent []byte
	if commitMode == filemode.Symlink {
		target, readErr := root.Readlink(name)
		if readErr != nil {
			return false
		}
		diskContent = []byte(target)
	} else {
		diskContent, err = osroot.ReadFileNoFollow(root, name)
		if err != nil {
			return false
		}
	}
	of := config.SHA1
	if commitHash.Size() == config.SHA256.Size() {
		of = config.SHA256
	}
	h := plumbing.NewHasher(of, plumbing.BlobObject, int64(len(diskContent)))
	if _, err := h.Write(diskContent); err != nil {
		return false
	}
	return commitHash.Equal(h.Sum())
}

// subtractFilesByName returns files from filesTouched that are NOT in committedFiles.
// This is a fallback when content-aware comparison isn't possible.
func subtractFilesByName(ctx context.Context, filesTouched []string, committedFiles map[string]struct{}) []string {
	logCtx := logging.WithComponent(ctx, "checkpoint")
	logging.Debug(logCtx, "subtractFilesByName: ",
		slog.Any("filesTouched", filesTouched),
		slog.Any("committedFiles", committedFiles),
	)
	var remaining []string
	for _, f := range filesTouched {
		if _, committed := committedFiles[f]; !committed {
			remaining = append(remaining, f)
		}
	}
	return remaining
}
