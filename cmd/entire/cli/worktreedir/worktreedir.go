// Package worktreedir owns access to the files of the working tree itself.
//
// It is the third of Entire's three anchors, alongside entiredir (.entire) and
// gitdir (the git common dir), and the loosest of them: a root here contains
// operations to the repository rather than to a directory Entire owns. That is
// still worth having, because the paths that reach these reads and writes are
// not Entire's own:
//
//   - Checkpoint writes read working files named by `git status` output, on the
//     hook path, to turn them into blobs.
//   - Rewind writes working files named by git TREE ENTRIES out of a checkpoint,
//     which may have been fetched from a remote. Its restore half already opened
//     a root for exactly this reason; the reads beside it did not.
//   - Diff-stat and gather read working files named from status output too.
//
// A root makes "cannot leave the repository" a property of the handle rather
// than of each caller remembering to validate. It is not a substitute for the
// tree-path validation those callers do — normalizeRepoRelativeTreePath still
// rejects names that are not repo-relative before they are used as git paths —
// it is the layer underneath it.
package worktreedir

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// Open returns the shared *os.Root over the current worktree root. The returned
// root is owned by the registry and shared with every other caller; do not close
// it.
func Open(ctx context.Context) (*os.Root, error) {
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve worktree root: %w", err)
	}
	return OpenAt(root)
}

// OpenAt is Open for an explicit worktree root, for callers that resolved one
// already or that act on a worktree other than the current directory.
func OpenAt(worktreeRoot string) (*os.Root, error) {
	if worktreeRoot == "" {
		return nil, errors.New("worktreedir: worktree root is required")
	}
	abs, err := filepath.Abs(worktreeRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", worktreeRoot, err)
	}
	return osroot.Shared(abs) //nolint:wrapcheck // Shared names the directory and returns a missing one unwrapped
}

// Name converts a path inside the worktree to a name relative to its root,
// accepting either an absolute path or one already relative to the root.
//
// Git hands out slash-separated repo-relative paths and Entire assembles
// absolute ones from them; this is the single conversion back, so callers stop
// joining a root path onto a git path and reading the result.
func Name(worktreeRoot, p string) (string, error) {
	// filepath.IsLocal, the single primitive os.Root itself uses one layer down,
	// rather than an IsAbs/VolumeName pair. Windows has two forms that are
	// neither absolute nor volume-prefixed yet do not name anything inside the
	// worktree: the drive-relative "C:foo", which IsAbs reports false for while
	// filepath.Join drops the base directory when the appended element carries a
	// volume; and the rooted-relative "\foo", where volumeNameLen is 0 because a
	// single leading backslash is neither a volume nor a UNC prefix. Sending
	// both down the absolute branch makes filepath.Rel reject them, which is the
	// answer this containment check exists to give. IsLocal also rejects "..",
	// which the traversal check below covers anyway, and the Windows reserved
	// device names, which nothing here wants either.
	//
	// os.Root rejects such a name one layer down, so neither is a reachable
	// escape today. They are closed anyway because Name exists to answer "is
	// this inside the worktree?" independently of the caller, and
	// readWorktreeFileSafely feeds it paths that arrive from the API.
	if filepath.IsLocal(p) {
		cleaned := filepath.ToSlash(filepath.Clean(filepath.FromSlash(p)))
		if cleaned == "." || paths.IsRelativeTraversal(cleaned) {
			return "", fmt.Errorf("%q does not name a file in the worktree", p)
		}
		return cleaned, nil
	}
	base, err := filepath.Abs(worktreeRoot)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", worktreeRoot, err)
	}
	rel, err := filepath.Rel(base, p)
	if err != nil || rel == "." || paths.IsRelativeTraversal(rel) {
		return "", fmt.Errorf("%q is not inside %q", p, worktreeRoot)
	}
	return filepath.ToSlash(rel), nil
}

// NameFollowingLinks is Name for a path that may itself be, or sit below, a
// symlink: it resolves the link first and then answers for the target.
//
// os.Root refuses an ABSOLUTE symlink target unconditionally, including one
// resolving inside the root, so a root alone cannot express "follow a link that
// stays in the worktree, refuse one that leaves". That distinction is what a
// user-owned working-tree file needs — pointing vercel.json at a monorepo's
// shared config is a real setup, and it is written absolute as often as
// relative — while Entire's own trees (.entire, an agent's hook config) refuse
// a link either way and must not use this.
//
// The name returned is worktree-relative and symlink-free, so the caller's read
// still goes through the root: a link repointed between the resolve and the read
// changes which in-worktree file is read and cannot escape the worktree.
//
// A dangling link reports os.ErrNotExist, matching what os.Stat gives for one.
func NameFollowingLinks(worktreeRoot, p string) (string, error) {
	base, err := filepath.Abs(worktreeRoot)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", worktreeRoot, err)
	}
	target := p
	if !filepath.IsAbs(target) {
		target = filepath.Join(base, filepath.FromSlash(target))
	}
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", p, err)
	}
	// The BASE has to be resolved too, against the same rules. Otherwise every
	// repository living below a symlinked component is judged from a path that
	// no longer matches its own resolved children: on macOS /var is a link to
	// /private/var, so a worktree under /var/folders/... would have each of its
	// own files reported as outside itself. The relative answer is unaffected by
	// which spelling the caller's root was opened under.
	resolvedBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", worktreeRoot, err)
	}
	return Name(resolvedBase, resolved)
}
