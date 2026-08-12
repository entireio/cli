package paths

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// WorktreeIDHashLength is the number of hex characters HashWorktreeID
// returns (mirrored by checkpoint.WorktreeIDHashLength for shadow branch
// name parsing).
const WorktreeIDHashLength = 6

// HashWorktreeID returns a short stable hash of a worktree identifier (a
// GetWorktreeID result; "" for the main worktree). It is the per-worktree
// namespace key used both in shadow branch names
// ("entire/<commit>-<worktreeHash>", via checkpoint.HashWorktreeID, which
// delegates here) and in the invisible-routing runtime directory layout
// (<git-common-dir>/entire/worktree/<worktreeHash>/...).
func HashWorktreeID(worktreeID string) string {
	h := sha256.Sum256([]byte(worktreeID))
	return hex.EncodeToString(h[:])[:WorktreeIDHashLength]
}

// GetWorktreeID returns the internal git worktree identifier for the given path.
// For the main worktree, returns empty string — whether .git is a directory or
// a `git init/clone --separate-git-dir` .git file pointing at a full git dir.
// For linked worktrees (where .git is a file), extracts the name from
// .git/worktrees/<name>/ path. This name is stable across `git worktree move`.
func GetWorktreeID(worktreePath string) (string, error) {
	gitPath := filepath.Join(worktreePath, ".git")

	info, err := os.Stat(gitPath)
	if err != nil {
		return "", fmt.Errorf("failed to stat .git: %w", err)
	}

	// Main worktree has .git as a directory
	if info.IsDir() {
		return "", nil
	}

	// Linked worktree has .git as a file with content: "gitdir: /path/to/.git/worktrees/<name>"
	content, err := os.ReadFile(gitPath) //nolint:gosec // gitPath is constructed from worktreePath + ".git"
	if err != nil {
		return "", fmt.Errorf("failed to read .git file: %w", err)
	}

	line := strings.TrimSpace(string(content))
	if !strings.HasPrefix(line, "gitdir: ") {
		return "", fmt.Errorf("invalid .git file format: %s", line)
	}

	gitdir := strings.TrimPrefix(line, "gitdir: ")
	if worktreeID, found := parseWorktreeID(gitdir); found {
		return worktreeID, nil
	}

	// A .git file whose gitdir matches no known linked-worktree layout is not
	// automatically an error: `git init/clone --separate-git-dir` produces
	// exactly this shape for a MAIN worktree (a .git file pointing at a full
	// git dir at an arbitrary path), whose worktree ID is "". Distinguish on
	// disk rather than lexically: a linked-worktree admin dir
	// (<commondir>/worktrees/<id>) always contains a `commondir` file, a full
	// git dir never does.
	resolved := resolveGitdirPath(worktreePath, gitdir)
	if isMainWorktreeGitDir(resolved) {
		return "", nil
	}

	// A LINKED worktree of a separate-git-dir repo also matches no lexical
	// marker (its admin dir is <sepdir>/worktrees/<id>, and <sepdir> is not
	// named .git or .bare). When the resolved dir is a worktree admin dir
	// (has a commondir file), the ID is whatever follows the last
	// /worktrees/ segment — git always places admin dirs there.
	if isLinkedWorktreeAdminDir(resolved) {
		normalized := strings.TrimSuffix(strings.ReplaceAll(gitdir, "\\", "/"), "/")
		if idx := strings.LastIndex(normalized, "/worktrees/"); idx >= 0 {
			return normalized[idx+len("/worktrees/"):], nil
		}
	}

	return "", fmt.Errorf("unexpected gitdir format (no worktrees): %s", gitdir)
}

// resolveGitdirPath resolves a .git-file gitdir value (which may be relative
// to the worktree root) to a filesystem path suitable for probing.
func resolveGitdirPath(worktreePath, gitdir string) string {
	gitdir = filepath.FromSlash(strings.ReplaceAll(gitdir, "\\", "/"))
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(worktreePath, gitdir)
	}
	return filepath.Clean(gitdir)
}

// isMainWorktreeGitDir reports whether path is a full (main-worktree) git
// directory, as opposed to a linked-worktree admin dir. Both contain HEAD;
// only linked-worktree admin dirs contain a `commondir` file pointing back at
// the shared git dir. A path that is missing or has no HEAD is neither.
func isMainWorktreeGitDir(path string) bool {
	info, err := os.Stat(path) //nolint:gosec // path comes from the repo's own .git file; read-only probe
	if err != nil || !info.IsDir() {
		return false
	}
	if _, err := os.Stat(filepath.Join(path, "HEAD")); err != nil { //nolint:gosec // read-only probe of the repo's own gitdir
		return false
	}
	if _, err := os.Stat(filepath.Join(path, "commondir")); err == nil { //nolint:gosec // read-only probe of the repo's own gitdir
		return false // linked-worktree admin dir with an unrecognized layout
	}
	return true
}

// isLinkedWorktreeAdminDir reports whether path is a linked-worktree admin
// dir (<commondir>/worktrees/<id>): a directory with both HEAD and the
// commondir file pointing back at the shared git dir.
func isLinkedWorktreeAdminDir(path string) bool {
	info, err := os.Stat(path) //nolint:gosec // path comes from the repo's own .git file; read-only probe
	if err != nil || !info.IsDir() {
		return false
	}
	if _, err := os.Stat(filepath.Join(path, "HEAD")); err != nil { //nolint:gosec // read-only probe of the repo's own gitdir
		return false
	}
	_, err = os.Stat(filepath.Join(path, "commondir")) //nolint:gosec // read-only probe of the repo's own gitdir
	return err == nil
}

func parseWorktreeID(gitdir string) (string, bool) {
	gitdir = strings.TrimSuffix(strings.ReplaceAll(gitdir, "\\", "/"), "/")

	// Submodule gitdirs live under .git/modules/<path>. If that submodule
	// repository has its own linked worktree, the gitdir ends with
	// .git/modules/<path>/worktrees/<id>. A /worktrees/ segment before the
	// final /modules/ belongs to the superproject's worktree, not the submodule.
	if modulesIndex := strings.LastIndex(gitdir, "/modules/"); modulesIndex >= 0 {
		afterModules := gitdir[modulesIndex+len("/modules/"):]
		if _, worktreeID, found := strings.Cut(afterModules, "/worktrees/"); found {
			return strings.TrimSuffix(worktreeID, "/"), true
		}
		return "", true
	}

	// Extract worktree name from path like /repo/.git/worktrees/<name>
	// or /repo/.bare/worktrees/<name> (bare repo + worktree layout).
	// The path after the marker is the worktree identifier.
	for _, marker := range []string{".git/worktrees/", ".bare/worktrees/"} {
		if _, worktreeID, found := strings.Cut(gitdir, marker); found {
			return strings.TrimSuffix(worktreeID, "/"), true
		}
	}

	return "", false
}
