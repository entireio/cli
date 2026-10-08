package gitrepo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
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
// pointer, as printed by ls-tree and diff-index --raw.
const treeGitlinkMode = "160000"

// GitlinkPaths returns the subset of paths (relative to repoRoot) that are
// submodule gitlinks (mode 160000) in HEAD's tree or in the index, so a
// submodule added but not yet committed counts too, and so does one whose
// removal is staged (it cannot be committed as a file either).
//
// Both sides are pathspec-limited with literal pathspecs, so a name containing
// glob characters matches only itself, and both are bounded by what differs
// rather than by repository size, because callers include ancestor directories
// (a monorepo's "packages") in paths:
//
//   - HEAD: `git ls-tree -z HEAD -- <paths>`, which without -r prints only the
//     named entries, never a directory's subtree.
//   - The index: `git diff-index --cached --raw -z --no-renames <base> --
//     <paths>`, where base is HEAD, or the empty tree when HEAD is unborn. It
//     prints only index entries that differ from base under the named paths,
//     so an unchanged directory costs nothing. (`git ls-files -s` would list
//     the directory's entire subtree on every call.) --ignore-submodules=none
//     keeps a `submodule.<name>.ignore` setting in .gitmodules or config from
//     hiding a staged gitlink. A record whose destination mode is 160000 is a
//     gitlink staged but not in HEAD; one whose source mode is 160000 and
//     destination 000000 is a gitlink whose removal is staged. A gitlink
//     present unchanged in both is found by the HEAD side.
//
// The call is bounded by PathClassificationBudget on top of the caller's
// context. On error the returned set holds what was found so far; callers keep
// the remaining paths (fail open).
func GitlinkPaths(ctx context.Context, repoRoot string, paths []string) (map[string]struct{}, error) {
	found := make(map[string]struct{})
	if len(paths) == 0 {
		return found, nil
	}
	ctx, cancel := context.WithTimeout(ctx, PathClassificationBudget)
	defer cancel()

	unborn, err := headIsUnborn(ctx, repoRoot)
	if err != nil {
		return found, err
	}
	base := "HEAD"
	if unborn {
		if base, err = emptyTreeID(ctx, repoRoot); err != nil {
			return found, err
		}
	}

	for start := 0; start < len(paths); start += pathClassificationChunk {
		chunk := paths[start:min(start+pathClassificationChunk, len(paths))]

		indexOut, err := literalPathspecCommand(ctx, repoRoot, chunk, "diff-index", "--cached", "--raw", "-z", "--no-renames", "--ignore-submodules=none", base).Output()
		if err != nil {
			return found, fmt.Errorf("git diff-index --cached: %w", err)
		}
		collectIndexGitlinks(indexOut, found)

		if unborn {
			continue // nothing committed yet: the index is the only source
		}
		// Tree records: "<mode> <type> <object>\t<path>".
		treeOut, err := literalPathspecCommand(ctx, repoRoot, chunk, "ls-tree", "-z", "HEAD").Output()
		if err != nil {
			return found, fmt.Errorf("git ls-tree HEAD: %w", err)
		}
		collectGitlinks(treeOut, found)
	}
	return found, nil
}

// indexDiffBase returns the tree an index diff compares against: HEAD, or the
// empty tree when HEAD is unborn.
func indexDiffBase(ctx context.Context, repoRoot string) (string, error) {
	unborn, err := headIsUnborn(ctx, repoRoot)
	if err != nil {
		return "", err
	}
	if unborn {
		return emptyTreeID(ctx, repoRoot)
	}
	return "HEAD", nil
}

// emptyTreeID returns the object ID of the empty tree in the repository's
// object format (sha1 or sha256), as git computes it.
func emptyTreeID(ctx context.Context, repoRoot string) (string, error) {
	cmd := worktreeGitCommand(ctx, repoRoot, "hash-object", "-t", "tree", "--stdin")
	cmd.Stdin = strings.NewReader("")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git hash-object empty tree: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// collectIndexGitlinks adds the path of every `git diff-index --raw -z`
// record that stages a gitlink (destination mode 160000) or stages a
// gitlink's removal (source mode 160000, destination 000000). Each record is
// ":<src mode> <dst mode> <src oid> <dst oid> <status>" NUL "<path>" NUL.
func collectIndexGitlinks(out []byte, found map[string]struct{}) {
	fields := bytes.Split(out, []byte{0})
	for i := 0; i+1 < len(fields); i += 2 {
		meta := strings.Fields(strings.TrimPrefix(string(fields[i]), ":"))
		path := string(fields[i+1])
		if len(meta) < 2 || path == "" {
			continue
		}
		src, dst := meta[0], meta[1]
		if dst == treeGitlinkMode || (src == treeGitlinkMode && dst == "000000") {
			found[path] = struct{}{}
		}
	}
}

// literalPathspecCommand builds a git command whose trailing pathspecs (after
// "--") are matched literally (GIT_LITERAL_PATHSPECS=1).
func literalPathspecCommand(ctx context.Context, repoRoot string, pathspecs []string, args ...string) *exec.Cmd {
	full := make([]string, 0, len(args)+1+len(pathspecs))
	full = append(full, args...)
	full = append(full, "--")
	full = append(full, pathspecs...)
	cmd := worktreeGitCommand(ctx, repoRoot, full...)
	cmd.Env = append(withoutPathspecMagicEnv(cmd.Env), "GIT_LITERAL_PATHSPECS=1")
	return cmd
}

// conflictingPathspecEnv are the pathspec-magic variables git refuses to
// combine with GIT_LITERAL_PATHSPECS (or that would change what a literal
// pathspec matches). Inherited from a user's shell, they would make every
// literal-pathspec classifier fail, and the callers fail open.
var conflictingPathspecEnv = []string{"GIT_GLOB_PATHSPECS", "GIT_NOGLOB_PATHSPECS", "GIT_ICASE_PATHSPECS"}

// withoutPathspecMagicEnv returns env minus every conflictingPathspecEnv entry.
func withoutPathspecMagicEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if !slices.ContainsFunc(conflictingPathspecEnv, func(v string) bool { return strings.EqualFold(v, name) }) {
			out = append(out, entry)
		}
	}
	return out
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

// PathsStagedAsNew returns the subset of paths (relative to repoRoot) whose
// blob is staged as a new file: in the index with content and absent from HEAD
// (the empty tree when HEAD is unborn), so the next commit adds it. It comes
// from pathspec-limited `git diff-index --cached --name-only -z
// --diff-filter=A --no-renames --ita-invisible-in-index <base>` calls with
// literal pathspecs; diff-index, unlike `git diff`, never refreshes the index.
//
// Index membership alone (`ls-files --cached`) is not enough: an intent-to-add
// entry (`git add -N`) is listed there but holds no blob and is never
// committed. diff-index would report it as added (with the empty blob, which a
// genuinely staged empty file shares), so --ita-invisible-in-index hides it, as
// `git diff --cached` and `git commit` do. Bounded by
// PathClassificationBudget on top of the caller's context.
func PathsStagedAsNew(ctx context.Context, repoRoot string, paths []string) (map[string]struct{}, error) {
	found := make(map[string]struct{})
	if len(paths) == 0 {
		return found, nil
	}
	ctx, cancel := context.WithTimeout(ctx, PathClassificationBudget)
	defer cancel()

	base, err := indexDiffBase(ctx, repoRoot)
	if err != nil {
		return found, err
	}
	for start := 0; start < len(paths); start += pathClassificationChunk {
		chunk := paths[start:min(start+pathClassificationChunk, len(paths))]
		out, err := literalPathspecCommand(ctx, repoRoot, chunk, "diff-index", "--cached", "--name-only", "-z", "--diff-filter=A", "--no-renames", "--ita-invisible-in-index", base).Output()
		if err != nil {
			return found, fmt.Errorf("git diff-index --cached: %w", err)
		}
		for _, name := range bytes.Split(out, []byte{0}) {
			if len(name) > 0 {
				found[string(name)] = struct{}{}
			}
		}
	}
	return found, nil
}

// PathsInHEAD returns the subset of paths (relative to repoRoot) that HEAD's
// tree has, from pathspec-limited `git ls-tree -z --name-only HEAD` calls with
// literal pathspecs. An unborn HEAD has no paths. Bounded by
// PathClassificationBudget on top of the caller's context.
func PathsInHEAD(ctx context.Context, repoRoot string, paths []string) (map[string]struct{}, error) {
	found := make(map[string]struct{})
	if len(paths) == 0 {
		return found, nil
	}
	ctx, cancel := context.WithTimeout(ctx, PathClassificationBudget)
	defer cancel()

	for start := 0; start < len(paths); start += pathClassificationChunk {
		chunk := paths[start:min(start+pathClassificationChunk, len(paths))]
		out, err := literalPathspecCommand(ctx, repoRoot, chunk, "ls-tree", "-z", "--name-only", "HEAD").Output()
		if err != nil {
			if unborn, checkErr := headIsUnborn(ctx, repoRoot); checkErr == nil && unborn {
				return found, nil
			}
			return found, fmt.Errorf("git ls-tree HEAD: %w", err)
		}
		for _, name := range bytes.Split(out, []byte{0}) {
			if len(name) > 0 {
				found[string(name)] = struct{}{}
			}
		}
	}
	return found, nil
}
