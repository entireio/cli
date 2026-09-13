package strategy

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/go-git/go-git/v6/plumbing"
)

// stagedChanges pairs paths and blob IDs from one read of the commit's index.
type stagedChanges struct {
	paths  []string
	hashes map[string]plumbing.Hash
}

func getStagedChanges(ctx context.Context) (stagedChanges, error) {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return stagedChanges{}, fmt.Errorf("resolve worktree root: %w", err)
	}

	// Git owns this index. In prepare-commit-msg, -a and partial commits use
	// GIT_INDEX_FILE to select a temporary index; stripping it reads other work.
	cmd := exec.CommandContext(ctx, "git", "diff", "--cached", "--raw", "-z", "--no-abbrev", "--no-ext-diff")
	cmd.Dir = repoRoot
	output, err := cmd.Output()
	if err != nil {
		return stagedChanges{}, fmt.Errorf("git diff --cached: %w", err)
	}
	return parseStagedChanges(string(output))
}

func parseStagedChanges(raw string) (stagedChanges, error) {
	staged := stagedChanges{paths: []string{}, hashes: make(map[string]plumbing.Hash)}
	for raw != "" {
		header, rest, ok := strings.Cut(raw, "\x00")
		fields := strings.Fields(header)
		if !ok || len(fields) != 5 || !strings.HasPrefix(fields[0], ":") {
			return stagedChanges{}, errors.New("invalid staged diff header")
		}
		path, rest, ok := strings.Cut(rest, "\x00")
		if !ok || path == "" {
			return stagedChanges{}, errors.New("missing staged diff path")
		}
		// Like --name-only, renames and copies report the destination path.
		if fields[4][0] == 'R' || fields[4][0] == 'C' {
			path, rest, ok = strings.Cut(rest, "\x00")
			if !ok || path == "" {
				return stagedChanges{}, errors.New("missing staged diff destination")
			}
		}
		hash, ok := plumbing.FromHex(fields[3])
		if !ok {
			return stagedChanges{}, errors.New("invalid staged blob ID")
		}
		staged.paths = append(staged.paths, path)
		staged.hashes[path] = hash
		raw = rest
	}
	return staged, nil
}
