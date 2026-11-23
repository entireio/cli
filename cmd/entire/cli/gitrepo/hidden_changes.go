package gitrepo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// HiddenFromStatus reports whether a `git ls-files -v` tag marks a path
// skip-worktree ("S", or "s" with assume-unchanged too) or assume-unchanged
// (a lowercase tag): paths whose worktree changes git status does not report.
func HiddenFromStatus(tag string) bool {
	return tag == "S" || tag != strings.ToUpper(tag)
}

// HiddenWorktreeChanges returns the tracked paths marked assume-unchanged or
// skip-worktree whose worktree entry differs from the index: edited, deleted,
// or replaced by another kind of entry (a file by a symlink, or the reverse).
// git status does not report them. A skip-worktree path missing from the
// worktree is a sparse checkout's omission and is not returned. Unmerged
// entries are left out.
//
// A flagged submodule is returned when it has any change, committed, staged,
// unstaged or untracked. Each checked-out submodule is also searched,
// recursively, for the paths its own index hides and for the changes its own
// settings keep from the superproject's git status (see
// submoduleStatusHiddenChanges); those are returned below the submodule's path
// ("sub/file.txt").
//
// The entries are copied, without those flags, into a temporary index, which
// is refreshed and read with `git diff-files`. git therefore compares them by
// its own rules: clean filters and the index-aware line-ending conversion
// apply as they do for git status (see PathStatuses), which hashing the files
// would not do. The repository's own index is only read.
//
// This is for commands the user runs: unlike the package's hook-path reads,
// it keeps the repository selectors the caller exported (GIT_DIR,
// GIT_WORK_TREE, GIT_INDEX_FILE), so it reads the same index git status does
// in that environment.
func HiddenWorktreeChanges(ctx context.Context, worktreeRoot string) ([]string, error) {
	gitDir, err := callerGitCommand(ctx, worktreeRoot, nil, "rev-parse", "--absolute-git-dir").Output()
	if err != nil {
		return nil, gitCommandError("rev-parse", err)
	}
	return hiddenChanges(ctx, worktreeRoot, callerGitCommand, map[string]bool{strings.TrimSpace(string(gitDir)): true})
}

// gitCommandFunc builds a git command run in worktreeRoot, with env's entries
// added to its environment.
type gitCommandFunc func(ctx context.Context, worktreeRoot string, env []string, args ...string) *exec.Cmd

// hiddenChanges is HiddenWorktreeChanges for one repository's worktree, with
// git commands built by git. searched holds the git directories of the
// repositories searched so far, this one's included.
func hiddenChanges(ctx context.Context, worktreeRoot string, git gitCommandFunc, searched map[string]bool) ([]string, error) {
	out, err := git(ctx, worktreeRoot, nil, "ls-files", "-z", "-v", "--stage").Output()
	if err != nil {
		return nil, gitCommandError("ls-files", err)
	}
	var entries strings.Builder
	sparse := make(map[string]bool)
	var submodules []string
	for _, record := range strings.Split(string(out), "\x00") {
		// "<tag> <mode> <object> <stage>\t<path>"
		tag, entry, _ := strings.Cut(record, " ")
		meta, path, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 || fields[2] != "0" {
			continue
		}
		if fields[0] == indexGitlinkMode {
			submodules = append(submodules, path)
		}
		if !HiddenFromStatus(tag) || !worktreeEntryMode(fields[0]) {
			continue
		}
		sparse[path] = strings.EqualFold(tag, "S")
		entries.WriteString(entry)
		entries.WriteByte(0)
	}
	var changed []string
	if len(sparse) > 0 {
		changes, err := diffFilesWithoutFlags(ctx, worktreeRoot, entries.String(), git)
		if err != nil {
			return nil, err
		}
		for path, status := range changes {
			if status != "D" || !sparse[path] {
				changed = append(changed, path)
			}
		}
	}
	for _, sub := range submodules {
		nested, err := submoduleHiddenChanges(ctx, filepath.Join(worktreeRoot, filepath.FromSlash(sub)), searched)
		if err != nil {
			return nil, fmt.Errorf("submodule %s: %w", sub, err)
		}
		for _, file := range nested {
			changed = append(changed, sub+"/"+file)
		}
	}
	return changed, nil
}

// submoduleHiddenChanges returns the paths a checked-out submodule's own index
// hides, relative to the submodule. A submodule that is not checked out (its
// directory is missing, or holds no repository of its own) has none: git
// status reports a missing one, and an empty directory has nothing to hide.
// Nor does a repository already searched, such as one a symlink at the
// submodule's path leads back to (git status reports the symlink).
func submoduleHiddenChanges(ctx context.Context, submoduleRoot string, searched map[string]bool) ([]string, error) {
	// From the top of its own worktree, --show-cdup prints an empty line; from
	// an empty directory it prints the way up to the superproject's.
	out, err := submoduleGitCommand(ctx, submoduleRoot, nil, "rev-parse", "--show-cdup", "--absolute-git-dir").Output()
	if err != nil {
		return nil, nil //nolint:nilerr // not checked out: nothing of its own to hide
	}
	cdup, gitDir, _ := strings.Cut(strings.TrimSuffix(string(out), "\n"), "\n")
	if cdup != "" || searched[gitDir] {
		return nil, nil
	}
	searched[gitDir] = true
	unshown, err := submoduleStatusHiddenChanges(ctx, submoduleRoot)
	if err != nil {
		return nil, err
	}
	flagged, err := hiddenChanges(ctx, submoduleRoot, submoduleGitCommand, searched)
	if err != nil {
		return nil, err
	}
	return append(unshown, flagged...), nil
}

// submoduleStatusHiddenChanges returns the changes in a checked-out submodule
// that its own settings keep from the superproject's git status, which asks
// the submodule's own git status: untracked files its
// status.showUntrackedFiles hides, and changes in its submodules that their
// ignore settings there hide. They are returned only when that status shows
// nothing; otherwise the superproject's git status reports the submodule.
func submoduleStatusHiddenChanges(ctx context.Context, submoduleRoot string) ([]string, error) {
	all, err := submoduleStatusPaths(ctx, submoduleRoot, "--untracked-files=all", "--ignore-submodules=none")
	if err != nil || len(all) == 0 {
		return nil, err
	}
	shown, err := submoduleStatusPaths(ctx, submoduleRoot)
	if err != nil || len(shown) > 0 {
		return nil, err
	}
	return all, nil
}

// submoduleStatusPaths returns the paths a submodule's git status, run with
// args, reports.
func submoduleStatusPaths(ctx context.Context, submoduleRoot string, args ...string) ([]string, error) {
	out, err := submoduleGitCommand(ctx, submoduleRoot, nil, append([]string{"--no-optional-locks", "status", "--porcelain=v1", "-z"}, args...)...).Output()
	if err != nil {
		return nil, gitCommandError("status", err)
	}
	codes := make(map[string]string)
	paths := ParsePorcelainV1Z(out, codes)
	for path := range codes {
		paths = append(paths, path)
	}
	return paths, nil
}

// callerGitCommand is worktreeGitCommand with the caller's own environment,
// selectors included, plus env, whose entries win over the inherited ones.
func callerGitCommand(ctx context.Context, worktreeRoot string, env []string, args ...string) *exec.Cmd {
	cmd := worktreeGitCommand(ctx, worktreeRoot, args...)
	cmd.Env = append(os.Environ(), env...)
	return cmd
}

// submoduleGitCommand is worktreeGitCommand plus env. A submodule is read
// without the caller's repository selectors, which name the superproject's
// repository and index.
func submoduleGitCommand(ctx context.Context, worktreeRoot string, env []string, args ...string) *exec.Cmd {
	cmd := worktreeGitCommand(ctx, worktreeRoot, args...)
	cmd.Env = append(cmd.Env, env...)
	return cmd
}

// indexGitlinkMode is the index mode of a submodule's commit pointer.
const indexGitlinkMode = "160000"

// worktreeEntryMode reports whether an index mode is one git diff-files
// compares with the worktree: a regular file, a symlink or a submodule.
func worktreeEntryMode(mode string) bool {
	return mode == "100644" || mode == "100755" || mode == "120000" || mode == indexGitlinkMode
}

// diffFilesWithoutFlags writes entries (NUL-terminated `git ls-files --stage`
// records) into a temporary index and returns git diff-files' status for each
// path whose worktree entry differs.
func diffFilesWithoutFlags(ctx context.Context, worktreeRoot, entries string, gitCommand gitCommandFunc) (map[string]string, error) {
	dir, err := os.MkdirTemp("", "entire-hidden-index-")
	if err != nil {
		return nil, fmt.Errorf("create a temporary index: %w", err)
	}
	defer os.RemoveAll(dir)
	index, err := filepath.Abs(filepath.Join(dir, "index"))
	if err != nil {
		return nil, fmt.Errorf("create a temporary index: %w", err)
	}
	git := func(stdin string, args ...string) ([]byte, error) {
		// core.ignoreStat would mark every imported entry assume-unchanged
		// again, a split index would leave a shared index file in the
		// repository, and fsmonitor could start its daemon for a throwaway
		// index.
		cmd := gitCommand(ctx, worktreeRoot, []string{"GIT_INDEX_FILE=" + index},
			append([]string{"-c", "core.ignoreStat=false", "-c", "core.splitIndex=false", "-c", "core.fsmonitor=false"}, args...)...)
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.Output()
		if err != nil {
			return nil, gitCommandError(args[0], err)
		}
		return out, nil
	}
	if _, err := git(entries, "update-index", "-z", "--index-info"); err != nil {
		return nil, err
	}
	// The copied entries carry no stat data, so the refresh compares each
	// one's contents and records the stat of those that match.
	if _, err := git("", "update-index", "-q", "--refresh"); err != nil {
		return nil, err
	}
	// A flagged submodule with any change counts, whatever its ignore
	// setting, as it does for the review's git status.
	out, err := git("", "diff-files", "-z", "--name-status", "--ignore-submodules=none")
	if err != nil {
		return nil, err
	}
	// "<status>\0<path>\0" per entry.
	records := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	changes := make(map[string]string, len(records)/2)
	for i := 0; i+1 < len(records); i += 2 {
		changes[records[i+1]] = records[i]
	}
	return changes, nil
}

// gitCommandError wraps a failed `git <name>` run with its stderr, which
// exec.Cmd.Output keeps on the exit error.
func gitCommandError(name string, err error) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && len(bytes.TrimSpace(exitErr.Stderr)) > 0 {
		return fmt.Errorf("git %s: %w: %s", name, err, bytes.TrimSpace(exitErr.Stderr))
	}
	return fmt.Errorf("git %s: %w", name, err)
}
