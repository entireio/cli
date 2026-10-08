package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
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
// link"). Neither can be committed through the superproject's worktree. When a
// batch fails that way, a probe with a harmless path first establishes that
// git can answer at all; if it can, every path is asked on its own, the ones
// git refuses are returned in refused, and the rest of the answers are kept.
// If the probe fails too, git cannot answer here and an error is returned with
// nothing refused, so callers keep the paths rather than drop them on a
// systemic failure.
//
// The call is bounded by PathClassificationBudget on top of the caller's
// context, so a wedged check-ignore cannot consume a hook's whole budget.
func IgnoredPaths(ctx context.Context, worktreeRoot string, paths []string) (ignored, refused map[string]struct{}, err error) {
	ignored = make(map[string]struct{})
	refused = make(map[string]struct{})
	if len(paths) == 0 {
		return ignored, refused, nil
	}
	ctx, cancel := context.WithTimeout(ctx, PathClassificationBudget)
	defer cancel()

	answered, exitErr := checkIgnore(ctx, worktreeRoot, paths, ignored)
	if answered {
		return ignored, refused, nil
	}
	if !isRefusal(exitErr) {
		return nil, nil, exitErr
	}
	// The batch was refused. Make sure git can answer here at all before
	// blaming individual paths.
	if probeOK, probeErr := checkIgnore(ctx, worktreeRoot, []string{ignoreProbePath}, map[string]struct{}{}); !probeOK {
		return nil, nil, fmt.Errorf("git check-ignore cannot answer here: %w", probeErr)
	}
	clear(ignored) // the partial output of the failed batch is not trusted
	for _, path := range paths {
		ok, pathErr := checkIgnore(ctx, worktreeRoot, []string{path}, ignored)
		switch {
		case ok:
		case isRefusal(pathErr):
			refused[path] = struct{}{}
		default:
			return nil, nil, pathErr
		}
	}
	return ignored, refused, nil
}

// ignoreProbePath is a path git can always ignore-check, used to tell a
// refused path from a git that cannot answer at all.
const ignoreProbePath = ".entire-check-ignore-probe"

// checkIgnore runs one check-ignore batch and adds the ignored paths to
// ignored. answered is false when git exited with an error other than exit 1
// ("none of the paths is ignored"); err then describes it.
func checkIgnore(ctx context.Context, worktreeRoot string, paths []string, ignored map[string]struct{}) (answered bool, err error) {
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
