package strategy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/worktreedir"
)

func installLefthookFiles(ctx context.Context, absolutePath bool) (int, error) { //nolint:unparam // Task 3 wires the settings-selected true path.
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return 0, fmt.Errorf("resolve worktree root: %w", err)
	}
	return installLefthookFilesAt(ctx, repoRoot, absolutePath, nil)
}

func installLefthookFilesAt(ctx context.Context, repoRoot string, absolutePath bool, beforePublish func(string) error) (int, error) {
	root, err := worktreedir.OpenAt(repoRoot)
	if err != nil {
		return 0, fmt.Errorf("open worktree: %w", err)
	}

	cmdPrefix, err := hookCmdPrefix(absolutePath)
	if err != nil {
		return 0, err
	}
	specs := buildHookSpecs(cmdPrefix)
	for _, spec := range specs {
		name := lefthookScriptPath(spec.name)
		current, _, readErr := readOptionalRegular(root, name)
		if readErr != nil {
			return 0, fmt.Errorf("read %s: %w", name, readErr)
		}
		if current != nil && !lefthookScriptOwned(string(current)) {
			return 0, fmt.Errorf("%w: script path %s already exists", ErrLefthookOwnedEntryConflict, name)
		}
	}

	commonDir, err := gitdir.CommonDirForWorktree(ctx, repoRoot)
	if err != nil {
		return 0, fmt.Errorf("resolve git common directory: %w", err)
	}
	gitRoot, err := gitdir.OpenAt(commonDir)
	if err != nil {
		return 0, fmt.Errorf("open git common directory: %w", err)
	}
	exclude, _, err := readOptionalRegular(gitRoot, "info/exclude")
	if err != nil {
		return 0, fmt.Errorf("read git exclude: %w", err)
	}
	mergedExclude := mergeLefthookInfoExclude(exclude)

	createdDirs, gitInfoCreated, err := prepareLefthookDirectories(root, gitRoot, specs)
	if err != nil {
		return 0, err
	}
	bail := func(e error) (int, error) {
		cleanupLefthookDirs(root, createdDirs)
		cleanupGitInfoDir(gitRoot, gitInfoCreated)
		return 0, e
	}

	// Write order is the transaction. Scripts, Entire's own config, and the
	// exclude entry land first; the extends entry in the user's local config
	// lands last, because that entry is what makes Lefthook load any of this —
	// so no run can see a config pointing at a script that is not there yet.
	// A failure part way through leaves orphan artifacts the next install
	// overwrites, and EnsureGitHookIntegration runs at every turn start.
	//
	// This has no rollback of its own: EnsureGitHookIntegration snapshots
	// every path touched here, plus the native hooks and their backups, and
	// restores all of them on any error.
	writes := make([]lefthookWrite, 0, len(specs)+2)
	for _, spec := range specs {
		writes = append(writes, lefthookWrite{
			root: root, name: lefthookScriptPath(spec.name),
			data: []byte(renderLefthookScript(spec)), mode: 0o755, counted: true,
		})
	}
	writes = append(writes,
		lefthookWrite{root: gitRoot, name: "info/exclude", data: mergedExclude, mode: 0o644},
		lefthookWrite{root: root, name: entireLefthookConfigName, data: renderEntireLefthookConfig(), mode: 0o644, counted: true},
	)

	written := 0
	for _, w := range writes {
		changed, writeErr := applyLefthookWrite(w, beforePublish)
		if writeErr != nil {
			return bail(writeErr)
		}
		if changed && w.counted {
			written++
		}
	}

	if _, err := ensureLefthookExtends(root); err != nil {
		return bail(err)
	}
	return written, nil
}

// lefthookWrite is one artifact to place. counted marks the files whose
// creation the caller reports as "installed"; info/exclude is bookkeeping.
type lefthookWrite struct {
	root    *os.Root
	name    string
	data    []byte
	mode    os.FileMode
	counted bool
}

// applyLefthookWrite writes one artifact atomically, skipping the write when
// the content and mode already match so repeated installs are no-ops.
func applyLefthookWrite(w lefthookWrite, beforeWrite func(string) error) (bool, error) {
	current, info, err := readOptionalRegular(w.root, w.name)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", w.name, err)
	}
	if current != nil && bytes.Equal(current, w.data) && info != nil && info.Mode().Perm() == w.mode {
		return false, nil
	}
	if beforeWrite != nil {
		if err := beforeWrite(w.name); err != nil {
			return false, err
		}
	}
	if err := jsonutil.WriteFileAtomicIn(w.root, w.name, w.data, w.mode); err != nil {
		return false, fmt.Errorf("write %s: %w", w.name, err)
	}
	return true, nil
}

func prepareLefthookDirectories(root, gitRoot *os.Root, specs []hookSpec) ([]string, bool, error) {
	created := make([]string, 0, len(specs)+1)
	dirs := []string{lefthookLocalDir}
	for _, spec := range specs {
		dirs = append(dirs, filepath.ToSlash(filepath.Join(lefthookLocalDir, spec.name)))
	}
	for _, dir := range dirs {
		info, err := osroot.LstatNoSymlinks(root, dir)
		switch {
		case os.IsNotExist(err):
			created = append(created, dir)
		case err != nil:
			cleanupLefthookDirs(root, created)
			return nil, false, fmt.Errorf("inspect Lefthook directory %s: %w", dir, err)
		case !info.IsDir():
			cleanupLefthookDirs(root, created)
			return nil, false, fmt.Errorf("%s is not a directory", dir)
		}
		if err := osroot.MkdirAllNoSymlink(root, dir, 0o755); err != nil {
			cleanupLefthookDirs(root, created)
			return nil, false, fmt.Errorf("create Lefthook directory %s: %w", dir, err)
		}
	}
	gitInfoCreated := false
	info, err := osroot.LstatNoSymlinks(gitRoot, "info")
	switch {
	case os.IsNotExist(err):
		gitInfoCreated = true
	case err != nil:
		cleanupLefthookDirs(root, created)
		return nil, false, fmt.Errorf("inspect git info directory: %w", err)
	case !info.IsDir():
		cleanupLefthookDirs(root, created)
		return nil, false, errors.New("git info path is not a directory")
	}
	if err := osroot.MkdirAllNoSymlink(gitRoot, "info", 0o755); err != nil {
		cleanupLefthookDirs(root, created)
		return nil, false, fmt.Errorf("create git info directory: %w", err)
	}
	return created, gitInfoCreated, nil
}

func cleanupGitInfoDir(root *os.Root, created bool) {
	if created {
		_ = root.Remove("info") //nolint:errcheck // best-effort cleanup; non-empty means the directory was not ours to remove
	}
}

func cleanupLefthookDirs(root *os.Root, dirs []string) {
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = root.Remove(dirs[i]) //nolint:errcheck // best-effort cleanup of directories created during a failed transaction
	}
}
