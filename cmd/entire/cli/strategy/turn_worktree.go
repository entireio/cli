package strategy

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
)

// SplitEditsByWorktree groups the files a turn's agent edited (absolute, or
// relative to hookRoot) by the registered worktree of the repository they are
// in, for every worktree other than hookRoot: root (as git lists it) to
// worktree-relative paths. An agent launched in one worktree fires its hooks
// there even when it edits another by absolute path; its edits there are
// recorded against that worktree (StepContext.WorktreeRoot), not dropped as
// outside the hook's tree.
//
// A nested worktree (.worktrees/x, .claude/worktrees/x) owns its files rather
// than the worktree it sits in. Edits outside every worktree (plan files,
// /tmp), inside .git or .entire, or that no commit can carry are left out.
// Containment compares symlink-resolved paths. Callers pass the main
// transcript's edits only: a subagent working in its own throwaway worktree is
// left to its task record.
func SplitEditsByWorktree(ctx context.Context, hookRoot string, edited []string) map[string][]string {
	if len(edited) == 0 {
		return nil
	}
	registered, err := gitrepo.ListWorktreePaths(ctx, hookRoot)
	if err != nil || len(registered) < 2 {
		return nil
	}
	type worktree struct{ printed, resolved string }
	trees := make([]worktree, 0, len(registered))
	for _, p := range registered {
		if filepath.IsAbs(p) {
			trees = append(trees, worktree{printed: p, resolved: resolveExisting(p)})
		}
	}
	hook := resolveExisting(hookRoot)
	byTree := map[string][]string{}
	for _, path := range edited {
		if !filepath.IsAbs(path) {
			continue // relative to the hook's tree, which records its own files
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
		if owner.printed == "" || owner.resolved == hook || inGitOrEntireDir(ownerRel) {
			continue
		}
		byTree[owner.printed] = append(byTree[owner.printed], filepath.ToSlash(ownerRel))
	}
	for tree, rels := range byTree {
		kept, _, _ := FilterTrackableChanges(ctx, tree, rels, nil, nil)
		if len(kept) == 0 {
			delete(byTree, tree)
			continue
		}
		byTree[tree] = kept
	}
	if len(byTree) == 0 {
		return nil
	}
	return byTree
}

// PruneRemovedWorktrees drops sessionID's records of edits in worktrees git
// no longer lists (removed, or a subagent's throwaway worktree), or lists
// under the same ID for a worktree added since: no commit can carry those
// edits, and the session would otherwise stay pending forever. git never
// lists an unregistered checkout sharing the repository through commondir,
// so a record made in one is kept while that checkout is still there.
func PruneRemovedWorktrees(ctx context.Context, sessionID, hookRoot string) {
	pruneRemovedWorktrees(ctx, sessionID, hookRoot, gitrepo.ListWorktreePaths)
}

// pruneRemovedWorktrees is PruneRemovedWorktrees with the worktree listing
// passed in, so a test can record an entry while it runs.
func pruneRemovedWorktrees(ctx context.Context, sessionID, hookRoot string, list func(context.Context, string) ([]string, error)) {
	state, err := LoadSessionState(ctx, sessionID)
	if err != nil || state == nil || len(state.OtherWorktrees) == 0 {
		return
	}
	// Listed under the lock: an entry another hook records for a worktree
	// added after a listing taken outside it would read as removed.
	err = MutateSessionState(ctx, sessionID, func(state *SessionState) error {
		if len(state.OtherWorktrees) == 0 {
			return ErrMutationSkip
		}
		registered, err := list(ctx, hookRoot)
		if err != nil {
			return ErrMutationSkip
		}
		// Entries are keyed by worktree ID, which survives `git worktree
		// move`; a stored path doesn't, so liveness is decided by key, and a
		// moved worktree's entry takes its new path.
		live := make(map[string]string, len(registered))
		for _, p := range registered {
			id, idErr := paths.GetWorktreeID(p)
			if idErr != nil {
				// Can't tell which entry this worktree is: prune nothing.
				return ErrMutationSkip
			}
			live[session.WorktreeKey(id)] = resolveExisting(p)
		}
		changed := false
		for key, work := range state.OtherWorktrees {
			path, ok := liveRecordPath(ctx, hookRoot, live, key, work)
			switch {
			case !ok:
				delete(state.OtherWorktrees, key)
				changed = true
			case work.Path != path:
				work.Path = path
				changed = true
			}
		}
		if !changed {
			return ErrMutationSkip
		}
		if len(state.OtherWorktrees) == 0 {
			state.OtherWorktrees = nil
		}
		return nil
	})
	if err != nil && !errors.Is(err, ErrStateNotFound) {
		logging.Debug(logging.WithComponent(ctx, "session"), "could not prune records of removed worktrees",
			slog.String("session_id", sessionID), slog.String("error", err.Error()))
	}
}

// liveRecordPath reports whether work, a session's record keyed key, is still
// a live worktree's, and the path that worktree is at now. live maps the keys
// of the worktrees git lists to their paths.
func liveRecordPath(ctx context.Context, hookRoot string, live map[string]string, key string, work *session.WorktreeWork) (string, bool) {
	if path, listed := live[key]; listed {
		// Dropped only when known to be another worktree's: a check that
		// failed says nothing about the record.
		if current, err := worktreeEntryCurrent(ctx, path, key, work); current || err != nil {
			return path, true
		}
	}
	if key != session.WorktreeKey("") {
		return "", false
	}
	// The empty ID is the main worktree's and also every unregistered
	// checkout's, which the listing leaves out, so the record is checked
	// where it was made, and keeps its path.
	checkout, err := liveUnregisteredCheckout(hookRoot, work.Path)
	return work.Path, checkout || err != nil
}

// liveUnregisteredCheckout reports whether recorded, the path of a record under
// the empty worktree ID, is still an unregistered checkout of hookRoot's
// repository: its Git directory has no worktrees/<id> registration and shares
// hookRoot's common directory through commondir. Directories are compared by
// identity, so a bind-mount alias of the common directory counts. A path that
// is gone, or whose Git metadata is no longer a checkout's, can take no
// commit, so it is not. err is set when that can't be told (a permission or
// I/O failure reading either worktree's metadata), which says nothing about
// the record.
func liveUnregisteredCheckout(hookRoot, recorded string) (bool, error) {
	if recorded == "" {
		return false, nil
	}
	checkout, err := gitrepo.ResolveWorktreeMetadata(recorded)
	if err != nil {
		if metadataReadFailed(err) {
			return false, fmt.Errorf("resolve recorded checkout: %w", err)
		}
		return false, nil // removed, or no longer a checkout
	}
	if checkout.WorktreeID != "" {
		return false, nil // a registered worktree now, which git lists
	}
	hook, err := gitrepo.ResolveWorktreeMetadata(hookRoot)
	if err != nil {
		return false, fmt.Errorf("resolve hook worktree: %w", err)
	}
	isMain, err := sameDirIdentity(checkout.GitDir, checkout.CommonDir)
	if err != nil || isMain {
		return false, err
	}
	return sameDirIdentity(checkout.CommonDir, hook.CommonDir)
}

// metadataReadFailed reports whether err, from resolving a worktree's Git
// metadata, is a failure to read it (permission, I/O) rather than an answer:
// a missing path or .git entry, a path component that is now a file, or
// metadata that doesn't parse as a checkout's.
func metadataReadFailed(err error) bool {
	var pathErr *fs.PathError
	return errors.As(err, &pathErr) &&
		!errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) && !errors.Is(err, gitrepo.ErrWorktreeMetadataNotFound)
}

// sameDirIdentity reports whether a and b are the same directory on disk,
// which also holds for two bind mounts of it, as gitrepo compares them. err is
// set when either can't be inspected, which leaves the answer unknown.
func sameDirIdentity(a, b string) (bool, error) {
	aInfo, err := os.Stat(a)
	if err != nil {
		return false, fmt.Errorf("inspect %s: %w", a, err)
	}
	bInfo, err := os.Stat(b)
	if err != nil {
		return false, fmt.Errorf("inspect %s: %w", b, err)
	}
	return os.SameFile(aInfo, bInfo), nil
}

// sameDir reports whether a and b name the same directory once symlinks are
// resolved.
func sameDir(a, b string) bool {
	return resolveExisting(a) == resolveExisting(b)
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
