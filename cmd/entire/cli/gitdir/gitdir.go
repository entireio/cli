// Package gitdir owns access to the git common directory — .git in an ordinary
// checkout, and the main repository's .git when running from a linked worktree.
//
// Entire keeps a lot of state there, deliberately: session state, advisory
// locks, the checkpoint push queue, the redaction prefix cache, investigation
// runs, review manifests, and the captured checkpoint-sync election all live in
// the common dir rather than under .entire, because they are per-clone and must
// not be committed or walked into a checkpoint tree. That makes this directory
// the same kind of trust surface .entire is, and it gets the same treatment: one
// *os.Root per common dir, opened once and shared, with every read and write
// resolved as a name inside it.
//
// Two properties come from that, and neither survives a call site reverting to
// os.ReadFile on a joined path:
//
//   - Containment. Names here are built from agent-supplied session IDs and
//     investigation run IDs. Several call sites carry hand-written comments
//     explaining that an unvalidated ID would be a path-traversal sink feeding
//     os.RemoveAll. A root makes that structural instead of a precondition each
//     caller has to keep honouring.
//   - One directory handle per clone. gitrepo owns physical metadata
//     resolution; this package owns confined access to the resolved directory.
//
// Unlike .entire there is no create/no-create split: the common dir is the
// repository, so it always exists by the time anything here is called. What
// needs creating are Entire's own subdirectories inside it, via
// osroot.MkdirAllNoSymlink.
package gitdir

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// OpenForCurrentWorktree returns the shared root over the current worktree's
// common Git directory. It deliberately resolves on-disk worktree metadata;
// Git environment overrides do not replace that metadata.
func OpenForCurrentWorktree(ctx context.Context) (*os.Root, error) {
	worktreeRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve worktree root: %w", err)
	}
	metadata, err := gitrepo.ResolveWorktreeMetadata(worktreeRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve worktree metadata: %w", err)
	}
	return OpenAt(metadata.CommonDir)
}

// OpenAt is Open for an explicit git directory — the common dir for callers that
// already resolved one or that act on another clone, and the per-worktree git
// dir for the few things that genuinely live there rather than in the common dir
// (the rebase/cherry-pick sequence markers).
func OpenAt(commonDir string) (*os.Root, error) {
	if commonDir == "" {
		return nil, errors.New("gitdir: common dir is required")
	}
	abs, err := filepath.Abs(commonDir)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", commonDir, err)
	}

	// osroot.Shared owns the open-at-most-once registry, shared with the other
	// anchors, and returns a missing directory unwrapped so callers can classify
	// it with os.IsNotExist — an ordinary outcome for a store asked about a repo
	// that has none.
	return osroot.Shared(abs) //nolint:wrapcheck // see comment
}

// Reset closes and forgets every cached root, and the resolved path with them.
// Call it after deleting or replacing a common dir: a root that outlives its
// directory is a handle to an unlinked inode, so writes through it succeed and
// land nowhere. The root registry is shared with the other anchors, so this
// clears those too.
func Reset() {
	osroot.ResetShared()
}

// OpenPathIn returns the shared root for commonDir together with absPath's name
// inside it, so a consumer holding an absolute path it was handed earlier can
// still read through the one root rather than through the path.
//
// It refuses a path that is not under commonDir instead of silently reading it.
// That refusal is only worth anything when commonDir is resolved INDEPENDENTLY
// of absPath: a caller that derives the base by walking up from the target makes
// the check vacuous, because the relative path is then correct by construction.
// settings.clonePreferencesRoot is the one caller, and it recovers the common
// dir by removing a compile-time constant suffix, which fails loudly on a path
// of the wrong shape rather than anchoring somewhere else.
//
// (The investigation stores used to be described here as callers. They are not:
// they hold a commonDir of their own and resolve run ids as names inside it,
// which is the stronger form — see investigate.StateStore.)
func OpenPathIn(commonDir, absPath string) (root *os.Root, name string, err error) {
	base, err := filepath.Abs(commonDir)
	if err != nil {
		return nil, "", fmt.Errorf("resolve %s: %w", commonDir, err)
	}
	target, err := filepath.Abs(absPath)
	if err != nil {
		return nil, "", fmt.Errorf("resolve %s: %w", absPath, err)
	}
	rel, err := filepath.Rel(base, target)
	if err != nil || rel == "." || paths.IsRelativeTraversal(rel) {
		return nil, "", fmt.Errorf("%q is not under %q", absPath, commonDir)
	}
	root, err = OpenAt(base)
	if err != nil {
		return nil, "", err
	}
	return root, filepath.ToSlash(rel), nil
}
