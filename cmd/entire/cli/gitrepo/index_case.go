package gitrepo

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
)

// IndexPathsIgnoringCase returns the paths git's index tracks that equal one
// of paths (relative to repoRoot) when case is ignored, grouped by their
// lower-cased form (strings.ToLower), from `git ls-files -z --cached` calls
// with literal, case-insensitive pathspecs. A path the index tracks under its
// own spelling is returned too. Bounded by PathClassificationBudget on top of
// the caller's context.
func IndexPathsIgnoringCase(ctx context.Context, repoRoot string, paths []string) (map[string][]string, error) {
	found := make(map[string][]string)
	if len(paths) == 0 {
		return found, nil
	}
	ctx, cancel := context.WithTimeout(ctx, PathClassificationBudget)
	defer cancel()

	for start := 0; start < len(paths); start += pathClassificationChunk {
		chunk := paths[start:min(start+pathClassificationChunk, len(paths))]
		args := make([]string, 0, len(chunk)+4)
		args = append(args, "ls-files", "-z", "--cached", "--")
		for _, path := range chunk {
			args = append(args, ":(literal,icase)"+path)
		}
		cmd := worktreeGitCommand(ctx, repoRoot, args...)
		// Per-pathspec magic: an inherited GIT_LITERAL_PATHSPECS would make
		// git read ":(literal,icase)" as part of the name.
		cmd.Env = slices.DeleteFunc(withoutPathspecMagicEnv(cmd.Env), func(entry string) bool {
			name, _, _ := strings.Cut(entry, "=")
			return strings.EqualFold(name, "GIT_LITERAL_PATHSPECS")
		})
		out, err := cmd.Output()
		if err != nil {
			return found, fmt.Errorf("git ls-files: %w", err)
		}
		for _, name := range bytes.Split(out, []byte{0}) {
			if len(name) == 0 {
				continue
			}
			key := strings.ToLower(string(name))
			if !slices.Contains(found[key], string(name)) {
				found[key] = append(found[key], string(name))
			}
		}
	}
	return found, nil
}
