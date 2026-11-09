package strategy

import (
	"context"
	"log/slog"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
)

// enterWorktreeView makes state's home fields describe its edits in the
// worktree at root instead of its home, so commit-time code written for the
// home — content checks, claims, condensation, carry-forward — works on that
// worktree's record unchanged: FilesTouched, TouchedFileHashes and BaseCommit
// come from its OtherWorktrees entry, WorktreePath and WorktreeID name it, and
// the home's record waits in OtherWorktrees under the home's key, so "pending
// anywhere" (HasPendingFiles) still sees it.
//
// ok is false, and nothing changes, when root is the session's home, the
// session has no home recorded, or no current entry for root (one made before
// git gave root's ID to a new worktree is not). Otherwise leave puts
// everything back, keeping what processing left in the view as root's entry
// (dropped once empty). Call leave before the state is saved.
func enterWorktreeView(ctx context.Context, state *SessionState, root string) (leave func(), ok bool) {
	if root == "" || state.WorktreePath == "" || len(state.OtherWorktrees) == 0 ||
		isSessionHomeWorktree(root, state) || sameDir(root, state.WorktreePath) {
		return nil, false
	}
	worktreeID, err := paths.GetWorktreeID(root)
	if err != nil {
		return nil, false
	}
	key, homeKey := session.WorktreeKey(worktreeID), session.WorktreeKey(state.WorktreeID)
	entry := state.OtherWorktrees[key]
	if key == homeKey || !worktreeEntryLinks(ctx, root, key, entry) {
		return nil, false
	}
	if _, clash := state.OtherWorktrees[homeKey]; clash {
		return nil, false
	}
	home := session.WorktreeWork{
		Path:              state.WorktreePath,
		BaseCommit:        state.BaseCommit,
		FilesTouched:      state.FilesTouched,
		TouchedFileHashes: state.TouchedFileHashes,
	}
	homeID := state.WorktreeID
	delete(state.OtherWorktrees, key)
	state.OtherWorktrees[homeKey] = &home
	state.WorktreePath, state.WorktreeID = root, worktreeID
	state.BaseCommit = entry.BaseCommit
	state.FilesTouched, state.TouchedFileHashes = entry.FilesTouched, entry.TouchedFileHashes
	state.InWorktreeView = true
	state.ViewHomePath = home.Path

	return func() {
		// The entry as it was, with what processing left in the view.
		left := *entry
		left.BaseCommit = state.BaseCommit
		left.FilesTouched, left.TouchedFileHashes = state.FilesTouched, state.TouchedFileHashes
		if left.Path == "" {
			left.Path = root
		}
		home := state.OtherWorktrees[homeKey]
		delete(state.OtherWorktrees, homeKey)
		state.WorktreePath, state.WorktreeID = home.Path, homeID
		state.BaseCommit = home.BaseCommit
		state.FilesTouched, state.TouchedFileHashes = home.FilesTouched, home.TouchedFileHashes
		state.InWorktreeView, state.ViewHomePath = false, ""
		if len(left.FilesTouched) > 0 {
			state.OtherWorktrees[key] = &left
		}
		if len(state.OtherWorktrees) == 0 {
			state.OtherWorktrees = nil
		}
	}, true
}

// hasWorktreeEntry reports whether state recorded edits in the worktree at
// root other than its home, while root is the worktree they were made in.
func hasWorktreeEntry(ctx context.Context, state *SessionState, root string) bool {
	if len(state.OtherWorktrees) == 0 || state.WorktreePath == "" || isSessionHomeWorktree(root, state) || sameDir(root, state.WorktreePath) {
		return false
	}
	worktreeID, err := paths.GetWorktreeID(root)
	if err != nil {
		return false
	}
	key := session.WorktreeKey(worktreeID)
	work := state.OtherWorktrees[key]
	return work != nil && len(work.FilesTouched) > 0 && worktreeEntryLinks(ctx, root, key, work)
}

// overlapsFiles reports whether any of files is in set.
func overlapsFiles(files []string, set map[string]struct{}) bool {
	for _, f := range files {
		if _, ok := set[filepath.ToSlash(f)]; ok {
			return true
		}
	}
	return false
}

// homedAt reports whether root is state's home worktree. A state with no
// recorded home is treated as homed wherever it is asked about, as before
// homes were recorded. It compares paths only, which holds for turn-end hooks
// because turn start already followed a moved home and re-homed a session in
// a worktree re-added at its home path (reconcileWorktreePathForResumedTurn).
func homedAt(state *SessionState, root string) bool {
	return state.WorktreePath == "" || isSessionHomeWorktree(root, state) || sameDir(root, state.WorktreePath)
}

// enterStepWorktree is enterWorktreeView for a turn-end step recorded in
// root, creating root's entry when the session has none yet, or only one
// left from an earlier worktree git gave root's ID to. A session with
// no recorded home is first homed in the hook's worktree, so a step elsewhere
// can't make that worktree its home by default.
func enterStepWorktree(ctx context.Context, state *SessionState, root string) (leave func(), ok bool) {
	if state.WorktreePath == "" {
		hookRoot, err := paths.WorktreeRoot(ctx)
		if err != nil {
			return nil, false
		}
		state.WorktreePath = hookRoot
		if worktreeID, err := paths.GetWorktreeID(hookRoot); err == nil {
			state.WorktreeID = worktreeID
		}
	}
	if homedAt(state, root) {
		return nil, false
	}
	worktreeID, err := paths.GetWorktreeID(root)
	if err != nil {
		return nil, false
	}
	key := session.WorktreeKey(worktreeID)
	if state.OtherWorktrees == nil {
		state.OtherWorktrees = map[string]*session.WorktreeWork{}
	}
	previous := state.OtherWorktrees[key]
	created := !worktreeEntryLinks(ctx, root, key, previous)
	if created {
		incarnation, err := worktreeIncarnation(ctx, root, worktreeID, true)
		if err != nil {
			// Unmarked, the record would never count; git keeps this
			// worktree's HEAD in the same directory, so it is broken anyway.
			logging.Debug(logging.WithComponent(ctx, "session"), "could not mark worktree; its edits are not recorded",
				slog.String("worktree", root), slog.String("error", err.Error()))
			return nil, false
		}
		if previous != nil && previous.Incarnation == incarnation {
			// The first check failed but the record is this worktree's.
			created = false
		} else {
			state.OtherWorktrees[key] = &session.WorktreeWork{Path: root, Incarnation: incarnation}
		}
	}
	leave, ok = enterWorktreeView(ctx, state, root)
	if !ok && created {
		if previous != nil {
			state.OtherWorktrees[key] = previous
		} else {
			delete(state.OtherWorktrees, key)
		}
		if len(state.OtherWorktrees) == 0 {
			state.OtherWorktrees = nil
		}
	}
	return leave, ok
}
