package gitrepo

import (
	"bytes"
	"context"
	"fmt"
	"strings"
)

// PathStatuses returns the porcelain v1 status code ("XY", e.g. " M", "A ",
// " D", "??") of each of paths (relative to repoRoot) that `git status`
// reports, from pathspec-limited `git --no-optional-locks status
// --porcelain=v1 -z --untracked-files=all --no-renames` calls with literal
// pathspecs. A path absent from the result matches both the index and HEAD.
// Bounded by PathClassificationBudget on top of the caller's context.
//
// git status applies the same index-aware conversion rules `git add` does (a
// file already committed with CRLF is not normalized by text=auto or
// core.autocrlf), which `git hash-object` does not. That is why "does the
// worktree still differ from what was committed" is asked of status rather
// than answered by hashing the worktree file.
func PathStatuses(ctx context.Context, repoRoot string, paths []string) (map[string]string, error) {
	found := make(map[string]string)
	if len(paths) == 0 {
		return found, nil
	}
	ctx, cancel := context.WithTimeout(ctx, PathClassificationBudget)
	defer cancel()

	for start := 0; start < len(paths); start += pathClassificationChunk {
		chunk := paths[start:min(start+pathClassificationChunk, len(paths))]
		out, err := literalPathspecCommand(ctx, repoRoot, chunk, "--no-optional-locks", "status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames").Output()
		if err != nil {
			return found, fmt.Errorf("git status: %w", err)
		}
		ParsePorcelainV1Z(out, found)
	}
	return found, nil
}

// ParsePorcelainV1Z adds each "XY <path>" record of `git status
// --porcelain=v1 -z` output to found, which must be non-nil, keyed by path
// with its "XY" code. Records shorter than "XY <path>" are ignored. A rename
// or copy record is followed by its origin path, which is not added; the
// origins of renames are returned, since those paths no longer hold the file
// git tracked there.
func ParsePorcelainV1Z(out []byte, found map[string]string) (renamedFrom []string) {
	records := bytes.Split(out, []byte{0})
	for i := 0; i < len(records); i++ {
		record := records[i]
		if len(record) < 4 || record[2] != ' ' {
			continue
		}
		code := string(record[:2])
		found[string(record[3:])] = code
		if strings.ContainsAny(code, "RC") && i+1 < len(records) {
			i++
			if strings.Contains(code, "R") && len(records[i]) > 0 {
				renamedFrom = append(renamedFrom, string(records[i]))
			}
		}
	}
	return renamedFrom
}
