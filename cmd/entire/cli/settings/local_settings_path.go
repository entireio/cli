package settings

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/entiredir"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// LocalSettingsPath returns the settings.local.json that applies to the
// current worktree (see localSettingsPathIn) and whether it is the main
// worktree's. Every reader and writer of the local layer resolves through
// here, so a write made in a linked worktree lands in the file that worktree
// actually reads instead of creating one that hides it.
func LocalSettingsPath(ctx context.Context) (string, bool, error) {
	if root, ok := worktreeRootFromContext(ctx); ok {
		path, inherited := localSettingsPathIn(root)
		return path, inherited, nil
	}
	own, err := entiredir.PathTo(ctx, EntireSettingsLocalFile)
	if err != nil {
		return "", false, fmt.Errorf("resolve local settings path: %w", err)
	}
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return own, false, nil //nolint:nilerr // no repository: nothing to inherit from
	}
	path, inherited := localSettingsPathIn(root)
	return path, inherited, nil
}

// localSettingsPathIn returns the local settings file that applies to
// worktreeRoot, and whether it belongs to the main worktree.
//
// A linked worktree is created without the gitignored settings.local.json, so
// without this every developer-only setting (external_agents, an `enable
// --local` enablement, the OPF command) silently stops applying in it. The
// main worktree's file is used only when the linked worktree has none of its
// own; there is no merging. Any failure to establish the main worktree falls
// back to the worktree's own path, which is the behavior before inheritance.
//
// Trust needs nothing extra: the trust checks open the repository the file
// lives in, so an inherited file is verified against the main worktree's own
// index and HEAD.
func localSettingsPathIn(worktreeRoot string) (string, bool) {
	own := filepath.Join(worktreeRoot, EntireSettingsLocalFile)
	if present, err := localFilePresent(own); err != nil || present {
		return own, false
	}
	mainRoot, ok := mainWorktreeOf(worktreeRoot)
	if !ok {
		return own, false
	}
	inherited := filepath.Join(mainRoot, EntireSettingsLocalFile)
	if present, err := localFilePresent(inherited); err != nil || !present {
		return own, false
	}
	return inherited, true
}

// mainWorktreeOf returns the main worktree of a linked worktree. The main
// worktree is the parent of a common dir named .git (the derivation
// trailWorktreeBaseRoot uses), confirmed by resolving its own metadata back to
// the same common dir. Bare repositories and --separate-git-dir layouts have
// no such parent and report false.
func mainWorktreeOf(worktreeRoot string) (string, bool) {
	meta, err := gitrepo.ResolveWorktreeMetadata(worktreeRoot)
	if err != nil || meta.WorktreeID == "" || filepath.Base(meta.CommonDir) != ".git" {
		return "", false
	}
	mainRoot := filepath.Dir(meta.CommonDir)
	mainMeta, err := gitrepo.ResolveWorktreeMetadata(mainRoot)
	if err != nil || mainMeta.WorktreeID != "" || !sameDirectory(mainMeta.CommonDir, meta.CommonDir) {
		return "", false
	}
	return mainRoot, true
}

func sameDirectory(a, b string) bool {
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && filepath.Clean(ra) == filepath.Clean(rb)
}

// localFilePresent reports whether a settings.local.json exists, through the
// .entire root of the worktree it belongs to. A missing .entire is absent; a
// .entire that cannot be opened (a symlink, a permission failure) is an error.
func localFilePresent(path string) (bool, error) {
	root, name, err := entiredir.OpenPathForRead(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err //nolint:wrapcheck // callers only branch on presence
	}
	return fileExists(root, name)
}
