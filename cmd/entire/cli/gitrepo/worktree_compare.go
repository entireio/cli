package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v6/plumbing/object"
)

// MatchWorktreeFiles compares regular files with supplied commit entries using
// Git's index-aware clean conversion. Unlike hash-object, this preserves legacy
// CRLF blobs when Git exempts them from normalization. The private index has no
// cached stat data and never reads or updates the user's staged file content.
// Indexed attributes are copied because Git falls back to them when a working
// tree .gitattributes file is absent. Executable-bit changes are ignored.
//
// This costs four Git processes regardless of candidate count; callers should
// use it only for ambiguous hash-object mismatches. Errors match nothing.
func MatchWorktreeFiles(ctx context.Context, worktreeRoot string, files map[string]*object.File) (map[string]bool, error) {
	if len(files) == 0 {
		return map[string]bool{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, WorktreeContentHashBudget)
	defer cancel()
	dir, err := os.MkdirTemp("", "entire-worktree-index-")
	if err != nil {
		return nil, fmt.Errorf("create comparison index directory: %w", err)
	}
	defer os.RemoveAll(dir) // best-effort temporary index cleanup

	attrsCmd := exec.CommandContext(ctx, "git", "-C", worktreeRoot, "ls-files", "--stage", "-z", "--", ":(glob)**/.gitattributes")
	attrsCmd.Env = EnvWithoutRepoOverrides()
	attrs, err := attrsCmd.Output()
	if err != nil {
		return nil, fmt.Errorf("read indexed attributes: %w", err)
	}
	var entries strings.Builder
	entries.Write(attrs)
	for path, file := range files {
		fmt.Fprintf(&entries, "%o %s\t%s\x00", file.Mode, file.Hash, path)
	}
	command := func(args ...string) *exec.Cmd {
		// Disable index extensions that could write outside the temporary
		// directory or let a cached monitor verdict skip content comparison.
		base := []string{"-C", worktreeRoot, "-c", "core.filemode=false",
			"-c", "core.fsmonitor=false", "-c", "core.ignorestat=false",
			"-c", "core.splitIndex=false", "-c", "index.sparse=false"}
		cmd := exec.CommandContext(ctx, "git", append(base, args...)...)
		cmd.Env = append(EnvWithoutRepoOverrides(), "GIT_INDEX_FILE="+filepath.Join(dir, "index"))
		return cmd
	}
	seed := command("update-index", "-z", "--index-info")
	seed.Stdin = strings.NewReader(entries.String())
	if out, err := seed.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("populate comparison index: %w: %s", err, out)
	}
	// Fresh entries have zero stat data. Refresh hashes their content with
	// index-aware conversion; exit 1 means some entries really are dirty.
	if out, err := command("update-index", "--refresh").CombinedOutput(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			return nil, fmt.Errorf("refresh comparison index: %w: %s", err, out)
		}
	}
	out, err := command("diff-files", "--name-only", "-z", "--no-ext-diff", "--no-textconv", "--no-renames").Output()
	if err != nil {
		return nil, fmt.Errorf("diff comparison index: %w", err)
	}
	matches := make(map[string]bool, len(files))
	for path := range files {
		matches[path] = true
	}
	for path := range strings.SplitSeq(string(out), "\x00") {
		delete(matches, path)
	}
	return matches, nil
}
