package gitrepo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// PathClassificationBudget bounds each git call that classifies touched paths
// (IgnoredPaths, GitlinkPaths) on agent-hook paths. It matches
// WorktreeContentHashBudget, the per-call budget of the sibling hash-object
// step, and stays well inside StatusWalkBudget: these are pathspec-limited
// lookups, so a call that runs this long is wedged (a hung filesystem or a
// stuck index lock), not slow. The caller's context still applies on top.
const PathClassificationBudget = 5 * time.Second

// pathClassificationChunk caps how many paths go into one git argv, so a large
// path set cannot fail on argument-length limits before git inspects anything.
const pathClassificationChunk = 500

// treeGitlinkMode is the tree-entry mode git uses for a submodule commit
// pointer, as printed by ls-files -s and ls-tree.
const treeGitlinkMode = "160000"

// GitlinkPaths returns the subset of paths (relative to repoRoot) that are
// submodule gitlinks (mode 160000) in HEAD's tree or in the index, so a
// submodule added but not yet committed counts too.
//
// It runs pathspec-limited git commands — `git ls-files -s -z` for the index
// and `git ls-tree -z HEAD` for HEAD — with literal pathspecs, so only the
// named paths are read and a name containing glob characters matches only
// itself. An unborn HEAD contributes no entries. The call is bounded by
// PathClassificationBudget on top of the caller's context. On error the
// returned set holds what was found so far; callers keep the remaining paths
// (fail open).
func GitlinkPaths(ctx context.Context, repoRoot string, paths []string) (map[string]struct{}, error) {
	found := make(map[string]struct{})
	if len(paths) == 0 {
		return found, nil
	}
	ctx, cancel := context.WithTimeout(ctx, PathClassificationBudget)
	defer cancel()

	for start := 0; start < len(paths); start += pathClassificationChunk {
		chunk := paths[start:min(start+pathClassificationChunk, len(paths))]

		// Index records: "<mode> <object> <stage>\t<path>".
		indexOut, err := literalPathspecCommand(ctx, repoRoot, chunk, "ls-files", "-s", "-z").Output()
		if err != nil {
			return found, fmt.Errorf("git ls-files: %w", err)
		}
		collectGitlinks(indexOut, found)

		// Tree records: "<mode> <type> <object>\t<path>".
		treeOut, err := literalPathspecCommand(ctx, repoRoot, chunk, "ls-tree", "-z", "HEAD").Output()
		if err != nil {
			if unborn, checkErr := headIsUnborn(ctx, repoRoot); checkErr == nil && unborn {
				continue // nothing committed yet: the index is the only source
			}
			return found, fmt.Errorf("git ls-tree HEAD: %w", err)
		}
		collectGitlinks(treeOut, found)
	}
	return found, nil
}

// literalPathspecCommand builds a git command whose trailing pathspecs (after
// "--") are matched literally (GIT_LITERAL_PATHSPECS=1).
func literalPathspecCommand(ctx context.Context, repoRoot string, pathspecs []string, args ...string) *exec.Cmd {
	full := make([]string, 0, len(args)+1+len(pathspecs))
	full = append(full, args...)
	full = append(full, "--")
	full = append(full, pathspecs...)
	cmd := worktreeGitCommand(ctx, repoRoot, full...)
	cmd.Env = append(cmd.Env, "GIT_LITERAL_PATHSPECS=1")
	return cmd
}

// collectGitlinks adds the path of every NUL-terminated "<mode> ...\t<path>"
// record whose mode is the gitlink mode.
func collectGitlinks(out []byte, found map[string]struct{}) {
	for _, record := range bytes.Split(out, []byte{0}) {
		meta, path, ok := strings.Cut(string(record), "\t")
		if !ok || path == "" {
			continue
		}
		if mode, _, _ := strings.Cut(meta, " "); mode == treeGitlinkMode {
			found[path] = struct{}{}
		}
	}
}

// headIsUnborn reports whether HEAD names no commit yet (a fresh repository).
func headIsUnborn(ctx context.Context, repoRoot string) (bool, error) {
	err := worktreeGitCommand(ctx, repoRoot, "rev-parse", "--verify", "--quiet", "HEAD^{commit}").Run()
	if err == nil {
		return false, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return true, nil
	}
	return false, fmt.Errorf("git rev-parse HEAD: %w", err)
}
