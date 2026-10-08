package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// IgnoredPaths returns the subset of paths (relative to worktreeRoot) that
// git's ignore rules exclude, from one `git check-ignore -z --stdin` batch.
//
// Tracked paths are never reported (no --no-index): a tracked file that
// happens to match an ignore pattern can still be committed. An error means
// git could not answer; callers decide whether to fail open or closed. The
// call is bounded by PathClassificationBudget on top of the caller's context,
// so a wedged check-ignore cannot consume a hook's whole budget.
func IgnoredPaths(ctx context.Context, worktreeRoot string, paths []string) (map[string]struct{}, error) {
	ignored := make(map[string]struct{})
	if len(paths) == 0 {
		return ignored, nil
	}
	ctx, cancel := context.WithTimeout(ctx, PathClassificationBudget)
	defer cancel()
	cmd := worktreeGitCommand(ctx, worktreeRoot, "check-ignore", "-z", "--stdin")
	cmd.Stdin = strings.NewReader(strings.Join(paths, "\x00") + "\x00")
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return ignored, nil // exit 1: none of the paths is ignored
		}
		return nil, fmt.Errorf("git check-ignore: %w", err)
	}
	for _, name := range strings.Split(string(out), "\x00") {
		if name != "" {
			ignored[name] = struct{}{}
		}
	}
	return ignored, nil
}
