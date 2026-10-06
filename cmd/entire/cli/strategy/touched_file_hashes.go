package strategy

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/worktreedir"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
)

// Touched-file hashes are what content-aware commit decisions compare against.
// At turn end, SaveStep records the git blob hash of every file the step
// modified or created (SessionState.TouchedFileHashes), and records the files
// it deleted as deletions. Commit hooks then compare the blob hash a commit (or
// the index) holds for a path with the recorded one, which is enough to tell
// "the human committed what the agent wrote" from "the human replaced it"
// without keeping a copy of the content anywhere.
//
// A path can be in FilesTouched with no recorded entry: hashing failed, it is a
// symlink or other non-regular file, or it reached FilesTouched through a task
// record or a per-tool hook rather than a turn-end step. Every decision falls
// back to the path name for such a path.

// recordedFileHash looks up path in state's recorded hashes. recorded is false
// when no hash is known for the path; deleted reports a recorded deletion.
func recordedFileHash(hashes map[string]string, path string) (hash plumbing.Hash, recorded, deleted bool) {
	value, ok := hashes[path]
	if !ok {
		return plumbing.ZeroHash, false, false
	}
	if value == touchedFileDeleted {
		return plumbing.ZeroHash, true, true
	}
	if !plumbing.IsHash(value) {
		return plumbing.ZeroHash, false, false
	}
	return plumbing.NewHash(value), true, false
}

// touchedFileDeleted is the TouchedFileHashes value for a path the agent
// deleted. See session.State.TouchedFileHashes.
const touchedFileDeleted = ""

// hashTouchedFiles returns the worktree blob hash (hex) of each path that is a
// regular file, for recording with applyTouchedFileHashes. It runs outside the
// session lock: it costs one `git hash-object` per batch of paths (see
// gitrepo.HashWorktreeFiles), which applies clean filters exactly as `git add`
// would, so a recorded hash is the blob a commit of the unchanged file holds.
// Symlinks and other non-regular entries are skipped (git hash-object follows a
// symlink, while a commit stores its target path), as is anything git cannot
// hash; those paths stay unrecorded and fall back to name matching.
func hashTouchedFiles(ctx context.Context, worktreeRoot string, paths []string) map[string]string {
	hashable := make([]string, 0, len(paths))
	for _, path := range paths {
		if worktreedir.HashableEntry(worktreeRoot, path, filemode.Regular) {
			hashable = append(hashable, path)
		}
	}
	if len(hashable) == 0 {
		return nil
	}
	slices.Sort(hashable)
	hashes, err := gitrepo.HashWorktreeFiles(ctx, worktreeRoot, slices.Compact(hashable))
	if err != nil {
		// Partial results are still filter-aware hashes; the paths git could
		// not hash simply stay unrecorded.
		logging.Warn(logging.WithComponent(ctx, "checkpoint"), "could not hash every touched file; unhashed paths fall back to name matching",
			slog.Int("hashed", len(hashes)),
			slog.Int("requested", len(hashable)),
			slog.String("error", err.Error()),
		)
	}
	out := make(map[string]string, len(hashes))
	for path, hash := range hashes {
		out[path] = hash.String()
	}
	return out
}

// applyTouchedFileHashes records a turn-end step into state.TouchedFileHashes:
// each changed path gets its hash from hashes (from hashTouchedFiles), and each
// deleted path is recorded as a deletion. A changed path missing from hashes
// loses any earlier entry, so it falls back to name matching instead of being
// compared against an older version of the file.
func applyTouchedFileHashes(state *SessionState, changed []string, hashes map[string]string, deleted []string) {
	if len(changed) == 0 && len(deleted) == 0 {
		return
	}
	if state.TouchedFileHashes == nil {
		state.TouchedFileHashes = make(map[string]string, len(changed)+len(deleted))
	}
	for _, path := range changed {
		if hash, ok := hashes[path]; ok {
			state.TouchedFileHashes[filepath.ToSlash(path)] = hash
		} else {
			delete(state.TouchedFileHashes, filepath.ToSlash(path))
		}
	}
	for _, path := range deleted {
		state.TouchedFileHashes[filepath.ToSlash(path)] = touchedFileDeleted
	}
}

// MergeUnhashedFilesTouched merges paths into state.FilesTouched for every
// route other than a turn-end step: task-record completion, per-tool hooks, and
// subagent child-file merges. Those routes do not hash what they add, so any
// hash recorded for a merged path by an earlier step is dropped and the path
// falls back to name matching, exactly as applyTouchedFileHashes does for a
// changed path it could not hash. Keeping the older hash would make a commit of
// the newer content read as "the human replaced the agent's work".
//
// Every non-SaveStep write that adds to FilesTouched must go through here.
func MergeUnhashedFilesTouched(state *SessionState, fileLists ...[]string) {
	state.FilesTouched = mergeFilesTouched(state.FilesTouched, fileLists...)
	if len(state.TouchedFileHashes) == 0 {
		return
	}
	for _, list := range fileLists {
		for _, path := range list {
			delete(state.TouchedFileHashes, filepath.ToSlash(path))
		}
	}
	if len(state.TouchedFileHashes) == 0 {
		state.TouchedFileHashes = nil
	}
}

// pruneTouchedFileHashes drops recorded hashes for paths no longer in
// FilesTouched, so a path that leaves the session and later comes back through
// a route that records no hash is never judged against a stale one.
func pruneTouchedFileHashes(state *SessionState) {
	if len(state.TouchedFileHashes) == 0 {
		state.TouchedFileHashes = nil
		return
	}
	keep := make(map[string]struct{}, len(state.FilesTouched))
	for _, path := range state.FilesTouched {
		keep[path] = struct{}{}
	}
	maps.DeleteFunc(state.TouchedFileHashes, func(path, _ string) bool {
		_, ok := keep[path]
		return !ok
	})
	if len(state.TouchedFileHashes) == 0 {
		state.TouchedFileHashes = nil
	}
}

// dropPhantomFilesTouched removes the step's changed paths from FilesTouched
// when they neither exist in the worktree nor are recorded deletions.
// Transcript parsing can name paths the agent never actually created (it wrote
// src/types.go, then created src/types/types.go instead), and a
// created-then-deleted untracked file never shows up as a git deletion. Left in
// FilesTouched, such a path is carried forward after every commit without ever
// being committable.
//
// Only this step's paths are checked: an earlier step's file that is missing
// now (stashed, or moved aside by the user) is still the agent's work and may
// come back to be committed.
func dropPhantomFilesTouched(worktreeRoot string, state *SessionState, stepPaths []string) {
	if len(state.FilesTouched) == 0 || len(stepPaths) == 0 {
		return
	}
	root, err := worktreedir.OpenAt(worktreeRoot)
	if err != nil {
		return
	}
	phantom := make(map[string]struct{})
	for _, path := range stepPaths {
		path = filepath.ToSlash(path)
		if _, _, deleted := recordedFileHash(state.TouchedFileHashes, path); deleted {
			continue
		}
		if !worktreeEntryExists(root, worktreeRoot, path) {
			phantom[path] = struct{}{}
		}
	}
	if len(phantom) == 0 {
		return
	}
	kept := make([]string, 0, len(state.FilesTouched))
	for _, path := range state.FilesTouched {
		if _, drop := phantom[path]; !drop {
			kept = append(kept, path)
		}
	}
	state.FilesTouched = kept
	pruneTouchedFileHashes(state)
}

// worktreeEntryExists reports whether path names an entry (of any type,
// without following a final symlink) in the worktree. Any error other than
// "does not exist" counts as existing, so an unreadable path is never dropped.
func worktreeEntryExists(root *os.Root, worktreeRoot, path string) bool {
	name, err := worktreedir.Name(worktreeRoot, path)
	if err != nil {
		return true
	}
	_, err = root.Lstat(name)
	return err == nil || !errors.Is(err, fs.ErrNotExist)
}
