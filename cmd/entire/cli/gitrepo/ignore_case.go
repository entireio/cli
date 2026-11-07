package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// IgnoresCase reports whether git treats paths in worktreeRoot's repository
// as case-insensitive: core.ignorecase, which `git init` and `git clone` set
// after probing the filesystem (true on the macOS and Windows defaults). Unset
// is false, git's default. Bounded by PathClassificationBudget on top of the
// caller's context.
func IgnoresCase(ctx context.Context, worktreeRoot string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, PathClassificationBudget)
	defer cancel()
	out, err := worktreeGitCommand(ctx, worktreeRoot, "config", "--type=bool", "--get", "core.ignorecase").Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return false, nil // not set
		}
		return false, fmt.Errorf("git config core.ignorecase: %w", err)
	}
	return strings.TrimSpace(string(out)) == "true", nil
}
