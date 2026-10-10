package strategy

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/entiredir"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// SettleTurnWorktree decides which worktree of the repository a turn's work is
// in, from the files the agent's own transcript says it edited (absolute, or
// relative to hookRoot), and returns that worktree's root for the caller to
// measure the turn against. It re-homes the session there when needed.
//
// An agent launched in one worktree fires its hooks there even when it edits
// another (by absolute path, never cd'ing), so the hook's tree is not where
// the work is. The turn's worktree is the single registered worktree holding
// all of its edits; edits outside every worktree (plan files, /tmp) and paths
// no commit can carry don't count. A nested worktree (.worktrees/x,
// .claude/worktrees/x) wins over the one it sits in.
//
// When that worktree is not the session's home, the session moves there,
// unless it still has work pending at its home (moving would strand it), the
// worktree has no valid .entire for its stored copy, or the edits span several
// worktrees (then nothing changes and hookRoot is returned). Callers pass only
// the main transcript's edits: a subagent's edits in its own throwaway
// worktree must not move the parent session.
//
// The returned root is hookRoot unless the turn's work is in the session's
// home elsewhere (moved now or earlier).
func SettleTurnWorktree(ctx context.Context, sessionID, hookRoot string, edited []string) string {
	logCtx := logging.WithComponent(ctx, "session")
	target := turnWorktree(ctx, hookRoot, edited)
	if target == "" {
		return hookRoot
	}
	state, err := LoadSessionState(ctx, sessionID)
	if err != nil || state == nil {
		return hookRoot
	}
	if isSessionHomeWorktree(target, state) {
		return target
	}
	if paths.ValidateEntireDirAt(target) != nil {
		logging.Debug(logCtx, "turn's work is in another worktree without a valid .entire; session stays",
			slog.String("session_id", sessionID), slog.String("worktree", target))
		return hookRoot
	}
	moved := false
	err = MutateSessionState(ctx, sessionID, func(state *SessionState) error {
		if isSessionHomeWorktree(target, state) {
			moved = true
			return ErrMutationSkip
		}
		if state.HasPendingWork() {
			return ErrMutationSkip
		}
		if err := rehomeSession(ctx, state, target); err != nil {
			return err
		}
		moved = true
		return nil
	})
	if err != nil && !errors.Is(err, ErrStateNotFound) {
		logging.Warn(logCtx, "failed to move session to the worktree its turn worked in",
			slog.String("session_id", sessionID), slog.String("worktree", target), slog.String("error", err.Error()))
		return hookRoot
	}
	if !moved {
		logging.Debug(logCtx, "turn's work is in another worktree, but the session has pending work at home; it stays",
			slog.String("session_id", sessionID), slog.String("worktree", target))
		return hookRoot
	}
	return target
}

// rehomeSession points state at worktreeRoot: its path and ID, and that
// worktree's HEAD and branch as the base. Called under the session lock on a
// session with no pending work, so nothing recorded refers to the old home.
// The stored prompts move along (moveStoredPrompts) so the next condensation
// still has them.
func rehomeSession(ctx context.Context, state *SessionState, worktreeRoot string) error {
	worktreeID, err := paths.GetWorktreeID(worktreeRoot)
	if err != nil {
		return err //nolint:wrapcheck // names the worktree
	}
	repo, err := gitrepo.OpenPath(worktreeRoot)
	if err != nil {
		return err //nolint:wrapcheck // names the path
	}
	defer repo.Close()
	head, err := repo.Head()
	if err != nil {
		return err //nolint:wrapcheck // go-git names the failure
	}
	old := state.WorktreePath
	moveStoredPrompts(ctx, state, worktreeRoot)
	state.WorktreePath = worktreeRoot
	state.WorktreeID = worktreeID
	state.BaseCommit = head.Hash().String()
	captureSessionBranch(repo, state)
	state.UntrackedFilesAtStart = nil
	state.TouchedFileHashes = nil
	logging.Info(logging.WithComponent(ctx, "session"), "moved session to the worktree its agent works in",
		slog.String("session_id", state.SessionID), slog.String("from", old), slog.String("to", worktreeRoot))
	return nil
}

// moveStoredPrompts appends the prompts stored at state's current home to
// the ones at worktreeRoot and releases the old copy. full.jsonl is not moved:
// the caller writes this turn's copy into the new home after the move.
// Best-effort; a failure leaves the old copy where it was.
func moveStoredPrompts(ctx context.Context, state *SessionState, worktreeRoot string) {
	from := storedSessionRootOrNil(ctx, state)
	if from == nil {
		return
	}
	name, err := storedSessionFileName(state.SessionID, paths.PromptFileName)
	if err != nil {
		return
	}
	moving, err := entiredir.ReadFile(from, name)
	if err != nil || len(moving) == 0 {
		clearStagedFilesIn(ctx, from, state.SessionID, state.WorktreePath)
		return
	}
	to, err := entiredir.OpenAt(worktreeRoot)
	if err != nil {
		return
	}
	dir := entiredir.MustName(paths.SessionMetadataDirFromSessionID(state.SessionID))
	if err := osroot.MkdirAllNoSymlink(to, dir, 0o750); err != nil {
		return
	}
	content := string(moving)
	if existing, readErr := entiredir.ReadFile(to, name); readErr == nil && len(existing) > 0 {
		content = string(existing) + "\n\n---\n\n" + content
	} else if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
		return
	}
	if err := entiredir.WriteFile(to, name, []byte(content), 0o600); err != nil {
		logging.Warn(logging.WithComponent(ctx, "session"), "failed to move stored prompts to the session's new worktree",
			slog.String("session_id", state.SessionID), slog.String("error", err.Error()))
		return
	}
	clearStagedFilesIn(ctx, from, state.SessionID, state.WorktreePath)
}

// turnWorktree returns the registered worktree holding every counted edit, or
// "" when there is none or more than one. Containment compares resolved paths;
// the root returned is the one git printed.
func turnWorktree(ctx context.Context, hookRoot string, edited []string) string {
	if len(edited) == 0 {
		return ""
	}
	registered, err := gitrepo.ListWorktreePaths(ctx, hookRoot)
	if err != nil || len(registered) < 2 {
		return ""
	}
	type worktree struct{ printed, resolved string }
	trees := make([]worktree, 0, len(registered))
	for _, p := range registered {
		if filepath.IsAbs(p) {
			trees = append(trees, worktree{printed: p, resolved: resolveExisting(p)})
		}
	}
	byTree := map[string][]string{}
	for _, path := range edited {
		if !filepath.IsAbs(path) {
			path = filepath.Join(hookRoot, path)
		}
		resolved := resolveExisting(path)
		var owner worktree
		ownerRel := ""
		for _, tree := range trees {
			rel, relErr := filepath.Rel(tree.resolved, resolved)
			if relErr != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				continue
			}
			if len(tree.resolved) > len(owner.resolved) {
				owner, ownerRel = tree, rel
			}
		}
		if owner.printed == "" || inGitOrEntireDir(ownerRel) {
			continue
		}
		byTree[owner.printed] = append(byTree[owner.printed], filepath.ToSlash(ownerRel))
	}
	target := ""
	for tree, rels := range byTree {
		if kept, _, _ := FilterTrackableChanges(ctx, tree, rels, nil, nil); len(kept) == 0 {
			continue
		}
		if target != "" {
			return ""
		}
		target = tree
	}
	return target
}

// resolveExisting resolves symlinks in the longest existing prefix of path
// and keeps the rest, so a file that doesn't exist yet still resolves under
// its worktree's real path.
func resolveExisting(path string) string {
	cleaned := filepath.Clean(path)
	var missing []string
	for cur := cleaned; ; {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return resolved
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return cleaned
		}
		missing = append(missing, filepath.Base(cur))
		cur = parent
	}
}

// inGitOrEntireDir reports whether rel is inside a worktree's .git or .entire,
// which no commit carries and git status never reports.
func inGitOrEntireDir(rel string) bool {
	first, _, _ := strings.Cut(filepath.ToSlash(rel), "/")
	return first == ".git" || first == paths.EntireDir
}
