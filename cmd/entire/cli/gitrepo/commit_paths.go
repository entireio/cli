package gitrepo

import (
	"context"
	"fmt"
	"io"
)

// CommitPaths commits only paths (relative to repoRoot) with message, so
// anything else already staged stays staged and out of the commit. `git commit
// --only` takes the working-tree state of tracked paths itself, including a
// deletion already staged, which `git add` would reject as an unmatched
// pathspec; so only untracked paths are added first. It runs git in the environment
// PathStatuses uses — repository selectors and pathspec magic stripped,
// literal pathspecs — so it commits the files PathStatuses reported. The
// repository's own hooks and commit signing apply, and git's output goes to
// out. Unlike the classification helpers it has no budget of its own: a
// pre-commit hook may legitimately take a while, so only ctx bounds it.
func CommitPaths(ctx context.Context, repoRoot string, paths []string, message string, out io.Writer) error {
	statuses, err := PathStatuses(ctx, repoRoot, paths)
	if err != nil {
		return err
	}
	var untracked []string
	for _, p := range paths {
		if statuses[p] == "??" {
			untracked = append(untracked, p)
		}
	}
	if len(untracked) > 0 {
		if err := runCommitStep(ctx, repoRoot, untracked, out, "add"); err != nil {
			return err
		}
	}
	return runCommitStep(ctx, repoRoot, paths, out, "commit", "-q", "--only", "-m", message)
}

func runCommitStep(ctx context.Context, repoRoot string, paths []string, out io.Writer, args ...string) error {
	cmd := literalPathspecCommand(ctx, repoRoot, paths, args...)
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w", args[0], err)
	}
	return nil
}
