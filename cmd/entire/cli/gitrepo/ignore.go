package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// IgnoredPaths returns the subset of paths (relative to worktreeRoot) that
// git's ignore rules exclude, from `git check-ignore -z --stdin`.
//
// Tracked paths are never reported (no --no-index): a tracked file that
// happens to match an ignore pattern can still be committed.
//
// git refuses some paths outright and then fails the whole batch with exit
// 128, after printing only some answers: a path inside a submodule ("Pathspec
// is in submodule") or beneath a symlinked directory ("beyond a symbolic
// link"). Neither can be committed through the superproject's worktree, and
// both are returned in refused for the caller to drop:
//
//   - A path with a symlinked ancestor directory is found in process first
//     (symlinkedAncestorPaths, no-follow Lstat through worktree, the caller's
//     anchor root for worktreeRoot, one stat per distinct ancestor) and never
//     sent to git, so the common case costs no extra process. A nil worktree
//     skips this layer; gitrepo cannot open the anchor itself (worktreedir's
//     tests import gitrepo).
//   - If a batch is still refused, a probe with a harmless path establishes
//     that git can answer at all, and the batch is then bisected: halves are
//     asked recursively and only a single-path batch that git refuses lands in
//     refused, so k refused paths among n cost O(k log n) processes rather
//     than n. If the probe fails too, git cannot answer here and an error is
//     returned with nothing refused, so callers keep the paths rather than
//     drop them on a systemic failure.
//
// The call is bounded by PathClassificationBudget on top of the caller's
// context, so a wedged check-ignore cannot consume a hook's whole budget.
func IgnoredPaths(ctx context.Context, worktreeRoot string, worktree *os.Root, paths []string) (ignored, refused map[string]struct{}, err error) {
	ignored = make(map[string]struct{})
	refused = symlinkedAncestorPaths(worktree, paths)
	if len(paths) == 0 {
		return ignored, refused, nil
	}
	ctx, cancel := context.WithTimeout(ctx, PathClassificationBudget)
	defer cancel()

	ask := make([]string, 0, len(paths))
	for _, path := range paths {
		if _, skip := refused[path]; !skip {
			ask = append(ask, path)
		}
	}
	if len(ask) == 0 {
		return ignored, refused, nil
	}

	answered, batchErr := checkIgnore(ctx, worktreeRoot, ask, ignored)
	if answered {
		return ignored, refused, nil
	}
	if !isRefusal(batchErr) {
		return nil, nil, batchErr
	}
	// The batch was refused. Make sure git can answer here at all before
	// blaming individual paths.
	if probeOK, probeErr := checkIgnore(ctx, worktreeRoot, []string{ignoreProbePath}, map[string]struct{}{}); !probeOK {
		return nil, nil, fmt.Errorf("git check-ignore cannot answer here: %w", probeErr)
	}
	if err := bisectRefused(ctx, worktreeRoot, ask, ignored, refused); err != nil {
		return nil, nil, err
	}
	return ignored, refused, nil
}

// bisectRefused asks git about paths, a batch it already refused, by halves:
// a half git answers contributes its ignored paths, a refused half is split
// again, and a refused single path is recorded in refused. Any other failure
// is returned.
func bisectRefused(ctx context.Context, worktreeRoot string, paths []string, ignored, refused map[string]struct{}) error {
	if len(paths) == 1 {
		refused[paths[0]] = struct{}{}
		return nil
	}
	mid := len(paths) / 2
	for _, half := range [][]string{paths[:mid], paths[mid:]} {
		ok, err := checkIgnore(ctx, worktreeRoot, half, ignored)
		switch {
		case ok:
		case isRefusal(err):
			if err := bisectRefused(ctx, worktreeRoot, half, ignored, refused); err != nil {
				return err
			}
		default:
			return err
		}
	}
	return nil
}

// symlinkedAncestorPaths returns the paths that have a symlinked ancestor
// directory inside worktree, which git refuses to ignore-check. Ancestors are
// Lstat'ed without following, top down, each distinct ancestor once. Anything
// that cannot be stat'ed is left for git to answer; a nil worktree finds
// nothing.
func symlinkedAncestorPaths(worktree *os.Root, paths []string) map[string]struct{} {
	refused := make(map[string]struct{})
	if worktree == nil {
		return refused
	}
	symlinked := make(map[string]bool)
	isSymlink := func(dir string) bool {
		if v, ok := symlinked[dir]; ok {
			return v
		}
		info, statErr := worktree.Lstat(filepath.FromSlash(dir))
		v := statErr == nil && info.Mode()&os.ModeSymlink != 0
		symlinked[dir] = v
		return v
	}
	for _, path := range paths {
		parts := strings.Split(filepath.ToSlash(path), "/")
		for i := 1; i < len(parts); i++ {
			if isSymlink(strings.Join(parts[:i], "/")) {
				refused[path] = struct{}{}
				break
			}
		}
	}
	return refused
}

// checkIgnoreRuns counts check-ignore processes, so tests can bound how many
// a call spawns. Production code never reads it.
var checkIgnoreRuns atomic.Int64

// ignoreProbePath is a path git can always ignore-check, used to tell a
// refused path from a git that cannot answer at all.
const ignoreProbePath = ".entire-check-ignore-probe"

// checkIgnore runs one check-ignore batch and adds the ignored paths to
// ignored. answered is false when git exited with an error other than exit 1
// ("none of the paths is ignored"); err then describes it.
func checkIgnore(ctx context.Context, worktreeRoot string, paths []string, ignored map[string]struct{}) (answered bool, err error) {
	checkIgnoreRuns.Add(1)
	cmd := worktreeGitCommand(ctx, worktreeRoot, "check-ignore", "-z", "--stdin")
	cmd.Stdin = strings.NewReader(strings.Join(paths, "\x00") + "\x00")
	out, runErr := cmd.Output()
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) && exitErr.ExitCode() == 1 {
			return true, nil // exit 1: none of the paths is ignored
		}
		return false, fmt.Errorf("git check-ignore: %w", runErr)
	}
	for _, name := range strings.Split(string(out), "\x00") {
		if name != "" {
			ignored[name] = struct{}{}
		}
	}
	return true, nil
}

// isRefusal reports whether err is git's fatal exit (128), which check-ignore
// uses both for a path it refuses and for a repository it cannot read; the
// probe in IgnoredPaths tells the two apart.
func isRefusal(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == 128
}
