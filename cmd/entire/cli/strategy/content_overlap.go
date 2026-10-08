package strategy

import (
	"context"
	"errors"
	"log/slog"
	"os"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/worktreedir"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// truncateStringSlice returns the first n elements of a slice, for concise logging.
func truncateStringSlice(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// Overlap detection for checkpoint linking and carry-forward.
//
// Linking (filesOverlapWithContent in PostCommit, stagedFilesOverlapWithContent
// in PrepareCommitMsg) decides whether a commit carries a session's work and so
// gets its Entire-Checkpoint trailer. It is by NAME: any committed (or staged)
// path in FilesTouched counts, whether the file is modified, deleted, or new,
// and whatever its content. A user editing an agent-created file before
// committing it is far more common than a user overwriting it wholesale, and
// comparing the committed blob with the recorded hash turned the common case
// into a missing trailer.
//
// The one exception is a recorded deletion (SessionState.TouchedFileHashes
// holds "" for the path): if the agent's last action on a path was to delete
// it and the commit adds that path as a new file, someone else re-created it,
// so it does not link. A later turn in which the agent re-creates the path
// records a hash for it and removes the exception.
//
// Carry-forward (filesWithRemainingAgentChanges) is a different question: what
// is LEFT of the agent's work after a commit. It does compare the committed blob
// and the worktree with the recorded turn-end hash (see touched_file_hashes.go)
// to tell fully committed from replaced from partially committed. A path with no
// recorded hash — one that reached FilesTouched through a task record, a
// per-tool hook, or a Codex child-file merge (MergeUnhashedFilesTouched), or a
// symlink or unhashable file — falls back to name matching there too.

// overlapOpts provides pre-resolved git objects to avoid redundant reads.
// When fields are non-nil, they are used directly instead of reading from the repo.
type overlapOpts struct {
	headTree      *object.Tree // HEAD commit tree
	parentTree    *object.Tree // HEAD's first parent tree (nil = initial commit or not provided)
	hasParentTree bool         // True if parentTree was explicitly resolved (distinguishes nil-not-resolved from nil-initial-commit)
}

// filesOverlapWithContent reports whether the commit touches any path in
// filesTouched: a modified, deleted, or new file, matched by name. A new file
// the session recorded as deleted does not count (see the header above).
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
		_, err := headTree.File(filePath)
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

		// New files count by name, unless the agent's last action was deleting it.
		if newFileIsSessionWork(logCtx, "filesOverlapWithContent", hashes, filePath) {
			return true
		}
	}

	logging.Debug(logCtx, "filesOverlapWithContent: no overlapping files found",
		slog.Int("files_checked", len(filesTouched)),
	)
	return false
}

// newFileIsSessionWork reports whether a file the commit adds counts as the
// session's work. It does, by name, unless the session recorded the path as
// deleted: then someone else re-created it after the agent deleted it.
func newFileIsSessionWork(logCtx context.Context, caller string, hashes map[string]string, filePath string) bool {
	if _, _, deleted := recordedFileHash(hashes, filePath); deleted {
		logging.Debug(logCtx, caller+": new file the session recorded as deleted",
			slog.String("file", filePath),
		)
		return false
	}
	logging.Debug(logCtx, caller+": new file counts as overlap",
		slog.String("file", filePath),
	)
	return true
}

// stagedFilesOverlapWithContent reports whether any staged path is in
// filesTouched, matched by name for modified, deleted, and new files alike. A
// new file the session recorded as deleted does not count (see the header
// above).
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

		// New files count by name, unless the agent's last action was deleting it.
		if newFileIsSessionWork(logCtx, "stagedFilesOverlapWithContent", hashes, stagedPath) {
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
// A file without a recorded hash (a session from an older CLI, or a path added
// by a task record, per-tool hook, or Codex child-file merge) is judged the
// same way against the commit, minus the recorded-hash shortcut: once
// committed, it stays while the working tree still differs from the committed
// blob and drops when they match. An uncommitted one stays unless it is
// absent from both the commit and the worktree (the phantom-path guard:
// transcript parsing can name files the agent never created, and carrying
// those forward would never end); one the commit still has but the worktree
// lacks is a pending deletion of a tracked file and stays.
//
// A recorded agent deletion stays while it is still pending: the commit tree
// still has the path and the worktree still lacks it, so the later commit that
// deletes it links the session. It drops once a commit removes the path, or
// when the file is back in the worktree (someone re-created it).
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

	classify := remainingClassifier{logCtx: logCtx, commitTree: commitTree, root: root, worktreeRoot: worktreeRoot}
	keep := make([]bool, len(filesTouched))
	var candidates []worktreeCandidate
	var vanished []int
	for i, filePath := range filesTouched {
		_, wasCommitted := committedFiles[filePath]
		kept, candidate, gone := classify.file(filePath, wasCommitted, hashes)
		keep[i] = kept
		if candidate != nil {
			candidate.index = i
			candidates = append(candidates, *candidate)
		}
		if gone {
			vanished = append(vanished, i)
		}
	}
	keepStagedVanished(ctx, logCtx, worktreeRoot, filesTouched, vanished, keep)

	// Whether the worktree still differs from what was committed is git
	// status's answer, not a comparison of `git hash-object` with the commit:
	// status applies the index-aware line-ending rule `git add` does (a file
	// committed with CRLF is not normalized by text=auto or core.autocrlf),
	// hash-object does not, and a hash comparison read every such file as
	// dirty forever. This runs right after the commit, so HEAD is the commit.
	// A path status does not report matches the index and HEAD and is done;
	// anything reported (modified, staged, untracked, or deleted in the
	// worktree — the pending deletion of a tracked file) keeps its place. If
	// status fails, every candidate is kept rather than dropped on a guess.
	var statuses map[string]string
	var statusErr error
	if len(candidates) > 0 {
		if worktreeRoot == "" {
			statusErr = errors.New("no worktree root")
		} else {
			candidatePaths := make([]string, 0, len(candidates))
			for _, candidate := range candidates {
				candidatePaths = append(candidatePaths, candidate.path)
			}
			statuses, statusErr = gitrepo.PathStatuses(ctx, worktreeRoot, candidatePaths)
		}
		if statusErr != nil {
			logging.Debug(logCtx, "filesWithRemainingAgentChanges: git status failed for carry-forward candidates, keeping them",
				slog.String("error", statusErr.Error()))
		}
	}

	for _, candidate := range candidates {
		code, reported := statuses[candidate.path]
		if statusErr == nil && !reported {
			logging.Debug(logCtx, "filesWithRemainingAgentChanges: content differs from recorded but working tree is clean, skipping",
				slog.String("file", candidate.path),
				slog.String("commit_hash", candidate.commitHash.String()[:7]),
				slog.String("recorded_hash", candidate.recordedHash.String()[:7]),
			)
			continue
		}

		keep[candidate.index] = true
		logging.Debug(logCtx, "filesWithRemainingAgentChanges: working tree still differs from the commit, keeping for carry-forward",
			slog.String("file", candidate.path),
			slog.String("status", code),
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

// keepStagedVanished keeps, among the vanished paths (hashed, uncommitted, and
// absent from both the worktree and the commit tree), those whose blob is still
// staged as a new file (gitrepo.PathsStagedAsNew; an intent-to-add entry holds
// no blob and does not count). The user staged such a file before it left the
// worktree,
// so the next commit adds the staged blob, which is the agent's content;
// dropping it would stop that commit linking the session. The rest were
// untracked files the agent removed and drop. When the index cannot be read,
// every vanished path is kept rather than dropped on a guess.
func keepStagedVanished(ctx, logCtx context.Context, worktreeRoot string, filesTouched []string, vanished []int, keep []bool) {
	if len(vanished) == 0 {
		return
	}
	paths := make([]string, 0, len(vanished))
	for _, i := range vanished {
		paths = append(paths, filesTouched[i])
	}
	var stagedNew map[string]struct{}
	var err error
	if worktreeRoot == "" {
		err = errors.New("no worktree root")
	} else {
		stagedNew, err = gitrepo.PathsStagedAsNew(ctx, worktreeRoot, paths)
	}
	for _, i := range vanished {
		filePath := filesTouched[i]
		_, staged := stagedNew[filePath]
		switch {
		case err != nil:
			keep[i] = true
			logging.Debug(logCtx, "filesWithRemainingAgentChanges: could not check the index for a file missing from the worktree, keeping",
				slog.String("file", filePath), slog.String("error", err.Error()))
		case staged:
			keep[i] = true
			logging.Debug(logCtx, "filesWithRemainingAgentChanges: file missing from the worktree is still staged, keeping",
				slog.String("file", filePath))
		default:
			logging.Debug(logCtx, "filesWithRemainingAgentChanges: untracked file removed from the worktree, skipping",
				slog.String("file", filePath))
		}
	}
}

// worktreeCandidate is a committed path whose fate depends on whether the
// working tree still differs from the committed blob.
type worktreeCandidate struct {
	index        int
	path         string
	commitHash   plumbing.Hash
	recordedHash plumbing.Hash // zero when no hash was recorded
}

// remainingClassifier holds what filesWithRemainingAgentChanges resolves once
// per commit for classifying each touched path.
type remainingClassifier struct {
	logCtx       context.Context
	commitTree   *object.Tree
	root         *os.Root
	worktreeRoot string
}

// file decides one touched path without hashing the worktree: keep reports a
// path that stays in FilesTouched outright; a non-nil candidate defers the
// decision to the worktree-versus-commit comparison. See
// filesWithRemainingAgentChanges for the rules.
//
// vanished reports a hashed, uncommitted path absent from both the worktree and
// the commit tree. It is dropped unless the index still has it, which the
// caller checks for all such paths at once (keepStagedVanished).
func (c remainingClassifier) file(filePath string, wasCommitted bool, hashes map[string]string) (keep bool, candidate *worktreeCandidate, vanished bool) {
	recorded, hasHash, deleted := recordedFileHash(hashes, filePath)
	switch {
	case deleted:
		if c.deletionPending(filePath) {
			logging.Debug(c.logCtx, "filesWithRemainingAgentChanges: recorded deletion not yet committed, keeping",
				slog.String("file", filePath))
			return true, nil, false
		}
		logging.Debug(c.logCtx, "filesWithRemainingAgentChanges: recorded deletion committed or path re-created, skipping",
			slog.String("file", filePath))
		return false, nil, false
	case !wasCommitted && !hasHash:
		// Phantom guard: a path the agent never actually produced is absent
		// from both the commit and the worktree. A path the commit still has
		// but the worktree lacks is a pending deletion of a tracked file
		// (an older CLI's state, or a route that records no hash), and stays.
		if c.worktreeState(filePath) == worktreeEntryAbsent && !c.inCommit(filePath) {
			logging.Debug(c.logCtx, "filesWithRemainingAgentChanges: file without recorded hash missing from commit and worktree, skipping",
				slog.String("file", filePath))
			return false, nil, false
		}
		return true, nil, false
	case !wasCommitted:
		// A hashed file absent from both the worktree and the commit tree was
		// an untracked file the agent created and then removed (git status
		// reports no deletion for it); nothing of it is left to carry
		// forward. See untrackedDeletionCandidates for the `git stash -u`
		// trade-off. One the index still has was staged by the user and the
		// next commit adds its blob; keepStagedVanished keeps that one.
		if c.worktreeState(filePath) == worktreeEntryAbsent && !c.inCommit(filePath) {
			return false, nil, true
		}
		logging.Debug(c.logCtx, "filesWithRemainingAgentChanges: file not committed, keeping",
			slog.String("file", filePath))
		return true, nil, false
	}

	commitFile, err := c.commitTree.File(filePath)
	if err != nil {
		// The commit removed the path. Only something still in the worktree
		// (re-created, or `git rm --cached`) is left to commit; a recorded hash
		// is history, not evidence of remaining work. When the worktree cannot
		// be inspected, keep the path rather than guess.
		if c.worktreeState(filePath) != worktreeEntryAbsent {
			logging.Debug(c.logCtx, "filesWithRemainingAgentChanges: file not in commit tree but may still be in the worktree, keeping",
				slog.String("file", filePath))
			return true, nil, false
		}
		logging.Debug(c.logCtx, "filesWithRemainingAgentChanges: commit removed the file and the worktree lacks it, skipping",
			slog.String("file", filePath))
		return false, nil, false
	}
	if hasHash && commitFile.Hash.Equal(recorded) {
		logging.Debug(c.logCtx, "filesWithRemainingAgentChanges: content fully committed",
			slog.String("file", filePath))
		return false, nil, false
	}
	// Without a recorded hash there is no shortcut: keep the path while the
	// worktree still differs from what was committed (a partial commit).
	return false, &worktreeCandidate{
		path:         filePath,
		commitHash:   commitFile.Hash,
		recordedHash: recorded,
	}, false
}

// deletionPending reports whether a recorded agent deletion of path has not
// reached a commit yet: the commit tree still has the path and the worktree
// still lacks it. Whether the commit touched the path does not matter — a
// commit of a staged edit to it leaves the deletion pending. When the worktree
// cannot be inspected, a deletion the commit tree still has counts as pending,
// so it is never dropped on a guess.
func (c remainingClassifier) deletionPending(path string) bool {
	return c.inCommit(path) && c.worktreeState(path) != worktreeEntryPresent
}

// inCommit reports whether the commit's tree has path.
func (c remainingClassifier) inCommit(path string) bool {
	_, err := c.commitTree.File(path)
	return err == nil
}

// worktreeState probes path in the worktree; see worktreeEntryState.
func (c remainingClassifier) worktreeState(path string) worktreeEntryState {
	return probeWorktreeEntry(c.root, c.worktreeRoot, path)
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
