package gitrepo

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
)

// IndexPathsIgnoringCase returns the paths git's index tracks that equal one
// of paths (relative to repoRoot) when case is ignored, grouped by their
// lower-cased form (strings.ToLower), from `git ls-files -z --cached` calls
// with literal, case-insensitive pathspecs. A path the index tracks under its
// own spelling is returned too. Bounded by PathClassificationBudget on top of
// the caller's context.
//
// It reads repoRoot's own index, whatever index an enclosing hook names. A
// commit hook asking about the commit it is preparing uses
// CommitIndexPathsIgnoringCase instead.
func IndexPathsIgnoringCase(ctx context.Context, repoRoot string, paths []string) (map[string][]string, error) {
	return indexPathsIgnoringCase(ctx, repoRoot, paths, EnvWithoutRepoOverrides())
}

// CommitIndexPathsIgnoringCase is IndexPathsIgnoringCase for a commit hook
// running in repoRoot, asking about the commit git is preparing: it keeps the
// hook's GIT_INDEX_FILE. `git commit -a` and path-limited commits prepare the
// commit in a temporary index, which can spell a path otherwise than the
// worktree's index does. The other repository selectors are still dropped,
// so repoRoot names the repository.
func CommitIndexPathsIgnoringCase(ctx context.Context, repoRoot string, paths []string) (map[string][]string, error) {
	env := EnvWithoutRepoOverrides()
	if index, ok := os.LookupEnv("GIT_INDEX_FILE"); ok {
		env = append(env, "GIT_INDEX_FILE="+index)
	}
	return indexPathsIgnoringCase(ctx, repoRoot, paths, env)
}

// indexPathsIgnoringCase is IndexPathsIgnoringCase with the environment the
// git calls run in.
func indexPathsIgnoringCase(ctx context.Context, repoRoot string, paths, env []string) (map[string][]string, error) {
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
		cmd.Env = slices.DeleteFunc(withoutPathspecMagicEnv(env), func(entry string) bool {
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
