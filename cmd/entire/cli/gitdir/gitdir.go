// Package gitdir owns shared rooted access to resolved Git directories.
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

// OpenForCurrentWorktree discovers the current worktree and opens its common
// directory. The returned root belongs to the shared registry; do not close it.
func OpenForCurrentWorktree(ctx context.Context) (*os.Root, error) {
	worktreeRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve worktree root: %w", err)
	}
	metadata, err := gitrepo.ResolveWorktreeMetadata(worktreeRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve git metadata: %w", err)
	}
	return OpenAt(metadata.CommonDir)
}

// OpenAt opens an explicit git directory — the common dir for callers that
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

// Reset closes and forgets every shared root.
// Call it after deleting or replacing a common dir: a root that outlives its
// directory is a handle to an unlinked inode, so writes through it succeed and
// land nowhere. The root registry is shared with the other anchors, so this
// clears those too.
func Reset() {
	osroot.ResetShared()
}

// OpenPathIn returns a shared root and a relative name for absPath.
// commonDir must be resolved independently of absPath so confinement is meaningful.
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
