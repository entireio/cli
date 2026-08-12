package cli

import (
	"context"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/binding"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/perf"
	gogitconfig "github.com/go-git/go-git/v6/config"
)

// Per-invocation bounds for the evidence tap. Both exist to bound git forks
// on the hook path: maxForeignResolutionsPerTurn caps how many DISTINCT
// DIRECTORIES are probed (each new directory costs one git fork — without
// this bound a turn full of non-repo paths forks unboundedly, while repeat
// dirs are free resolver-cache hits, so distinct dirs tracks forks
// precisely), and maxForeignReposPerTurn caps how many distinct foreign
// repos are recorded. The loop stops when either trips.
const (
	maxForeignReposPerTurn       = 8
	maxForeignResolutionsPerTurn = 16
)

// recordForeignEvidence resolves evidence paths to their repos and records
// every foreign repo in the machine-level session record — enabled or not:
// the Enabled flag preserves evidence for lazy-enable integration, and slice 2
// filters on it. Evidence comes from two sources:
//
//   - foreignPaths: absolute out-of-repo paths the FilterAndNormalizePaths
//     clamp discarded.
//   - keptRelGroups: currentWorktreeRoot-relative KEPT paths. Kept paths can
//     still be cross-repo evidence when they land inside a repo NESTED under
//     the session repo's root (e.g. $HOME is a dotfiles repo and the session
//     runs there: every other repo on the machine is path-wise inside it, so
//     nothing ever becomes foreign). Detection is a stat-only ancestor walk
//     (nestedRepoEvidencePaths) — the no-nested-repos common case costs zero
//     git forks — and registered submodules are excluded as part of the
//     parent project. The kept slices are only READ here: the lists feeding
//     checkpoint capture stay byte-identical (slice 1's reviewed invariant).
//
// Best-effort throughout: every failure logs Debug and moves on, and a panic
// is swallowed the same way — capture is never blocked. The empty-input early
// return keeps the hot path (every single-repo turn with no evidence
// candidates) nearly free; turns with kept paths additionally pay the stat
// walk, never a fork.
func recordForeignEvidence(ctx context.Context, sessionID string, meta binding.SessionMeta, currentWorktreeRoot string, foreignPaths []string, keptRelGroups ...[]string) {
	logCtx := logging.WithComponent(ctx, "binding")
	defer func() {
		if r := recover(); r != nil {
			logging.Debug(logCtx, "binding tap panicked; evidence dropped, capture unaffected",
				slog.Any("panic", r),
				slog.String("stack", string(debug.Stack())))
		}
	}()
	if len(foreignPaths) == 0 && !anyPaths(keptRelGroups) {
		return
	}
	// unknownSessionID passes ValidateSessionID and would create a
	// sessions/unknown.json aggregating unrelated sessions.
	if sessionID == "" || sessionID == unknownSessionID {
		return
	}
	_, span := perf.Start(ctx, "binding_tap")
	defer span.End()

	// Nested-repo detection shares the resolution/repo budget below: detected
	// paths simply join the same loop, so a pathological turn cannot fork more
	// than maxForeignResolutionsPerTurn git processes in total.
	if nested := nestedRepoEvidencePaths(currentWorktreeRoot, keptRelGroups); len(nested) > 0 {
		foreignPaths = append(append(make([]string, 0, len(foreignPaths)+len(nested)), foreignPaths...), nested...)
	}
	if len(foreignPaths) == 0 {
		return
	}

	// The resolver returns symlink-canonical roots; canonicalize ours the
	// same way (best-effort) so a symlink-aliased cwd cannot defeat the
	// own-worktree skip below.
	if canonical, err := filepath.EvalSymlinks(currentWorktreeRoot); err == nil {
		currentWorktreeRoot = canonical
	}

	found := make(map[string]binding.RepoIdentity)
	order := make([]string, 0, len(foreignPaths))
	// Budget by filepath.Dir(path): the resolver's real cache key is the
	// nearest EXISTING ancestor, but at this layer filepath.Dir is the right
	// cheap proxy — a path whose dir was already probed is a guaranteed cache
	// hit, so many paths in one foreign dir spend the budget exactly once and
	// cannot starve later repos.
	seenDirs := make(map[string]struct{})
	for _, p := range foreignPaths {
		if len(found) >= maxForeignReposPerTurn {
			logging.Debug(logCtx, "binding tap repo cap reached, skipping remaining evidence paths",
				slog.String("session_id", sessionID),
				slog.Int("found", len(found)))
			break
		}
		dir := filepath.Dir(p)
		if _, probed := seenDirs[dir]; !probed {
			if len(seenDirs) >= maxForeignResolutionsPerTurn {
				// A path in an already-probed dir can only re-yield a repo we
				// have, so nothing recordable remains once new dirs are out
				// of budget.
				logging.Debug(logCtx, "binding tap resolution budget exhausted, skipping remaining evidence paths",
					slog.String("session_id", sessionID),
					slog.Int("distinct_dirs", len(seenDirs)),
					slog.Int("found", len(found)))
				break
			}
			seenDirs[dir] = struct{}{}
		}
		id, ok := binding.ResolveRepoForPath(ctx, p)
		if !ok {
			continue
		}
		// Same worktree = not foreign. Worktree root alone identifies it: two
		// clones cannot share a worktree root, and a different worktree of
		// the same clone has a different root — that one IS recorded (slice 2
		// decides what it means).
		if id.WorktreeRoot == currentWorktreeRoot {
			continue
		}
		if _, seen := found[id.CommonDir]; !seen {
			found[id.CommonDir] = id
			order = append(order, id.CommonDir)
		}
	}

	for _, commonDir := range order {
		id := found[commonDir]
		enabled := settings.IsSetUpAtRoot(id.WorktreeRoot)
		if err := binding.RecordBinding(ctx, sessionID, meta, binding.Evidence{Repo: id, Enabled: enabled}); err != nil {
			logging.Debug(logCtx, "failed to record session binding",
				slog.String("session_id", sessionID),
				slog.String("repo", id.WorktreeRoot),
				slog.String("error", err.Error()))
			continue
		}
		logging.Debug(logCtx, "recorded foreign repo evidence",
			slog.String("session_id", sessionID),
			slog.String("repo", id.WorktreeRoot),
			slog.Bool("enabled", enabled))
	}
}

func anyPaths(groups [][]string) bool {
	for _, g := range groups {
		if len(g) > 0 {
			return true
		}
	}
	return false
}

// nestedRepoEvidencePaths returns, for the repoRoot-relative kept paths, one
// absolute path per distinct parent directory that lies inside a git repo
// NESTED under repoRoot — a `.git` entry (dir or gitfile: linked worktrees
// and submodules use gitfiles) strictly between the path's directory and
// repoRoot. Detection is stat-only; the returned paths are handed to the
// budgeted git resolver for canonical (innermost) worktree-root/common-dir
// resolution, so a false positive (stale .git junk) costs one fork and is
// then discarded by the resolver or the own-worktree skip.
//
// Nested repos registered as submodules in repoRoot's .gitmodules are part of
// the parent project and excluded; unregistered ones (the dotfiles/home-repo
// case) are evidence. Relative junk that escapes repoRoot after joining
// (../ traversal kept by the clamp) is skipped: it was never resolvable
// evidence.
func nestedRepoEvidencePaths(repoRoot string, keptRelGroups [][]string) []string {
	if repoRoot == "" || !anyPaths(keptRelGroups) {
		return nil
	}
	prefix := repoRoot + string(filepath.Separator)
	// nestedRootByDir memoizes the ancestor walk per visited directory
	// ("" = no intermediate .git), so one turn's stats are bounded by the
	// number of distinct directories in the kept tree, not by path count.
	nestedRootByDir := map[string]string{}
	var submodulePaths map[string]struct{}
	submodulesLoaded := false
	seenDirs := map[string]struct{}{}
	var out []string
	for _, group := range keptRelGroups {
		for _, rel := range group {
			if rel == "" {
				continue
			}
			abs := filepath.Join(repoRoot, filepath.FromSlash(rel))
			if !strings.HasPrefix(abs, prefix) {
				continue
			}
			dir := filepath.Dir(abs)
			if _, done := seenDirs[dir]; done {
				continue
			}
			seenDirs[dir] = struct{}{}
			nestedRoot := nestedRepoRootFor(dir, repoRoot, nestedRootByDir)
			if nestedRoot == "" {
				continue
			}
			if !submodulesLoaded {
				submodulePaths = registeredSubmodulePaths(repoRoot)
				submodulesLoaded = true
			}
			relRoot, err := filepath.Rel(repoRoot, nestedRoot)
			if err != nil {
				continue
			}
			if _, registered := submodulePaths[filepath.ToSlash(relRoot)]; registered {
				continue
			}
			out = append(out, abs)
		}
	}
	return out
}

// nestedRepoRootFor walks from dir up to (exclusive) repoRoot and returns the
// first (innermost) ancestor holding a .git entry, or "" when there is none.
// Nonexistent components (deleted files are evidence too) simply fail their
// Lstat and the walk continues upward. memo carries results across paths in
// one turn; every visited level shares the walk's outcome.
func nestedRepoRootFor(dir, repoRoot string, memo map[string]string) string {
	prefix := repoRoot + string(filepath.Separator)
	var visited []string
	nestedRoot := ""
	for d := dir; d != repoRoot && strings.HasPrefix(d, prefix); d = filepath.Dir(d) {
		if cached, ok := memo[d]; ok {
			nestedRoot = cached
			break
		}
		visited = append(visited, d)
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			nestedRoot = d
			break
		}
	}
	for _, d := range visited {
		memo[d] = nestedRoot
	}
	return nestedRoot
}

// registeredSubmodulePaths parses repoRoot/.gitmodules into the set of
// registered submodule paths (slash-normalized, repo-relative). Absence or a
// parse failure yields an empty set — then every nested repo counts as
// evidence, which errs toward recording, never toward blocking capture.
func registeredSubmodulePaths(repoRoot string) map[string]struct{} {
	data, err := os.ReadFile(filepath.Join(repoRoot, ".gitmodules")) //nolint:gosec // fixed well-known filename under the session repo root
	if err != nil {
		return nil
	}
	modules := gogitconfig.NewModules()
	if err := modules.Unmarshal(data); err != nil {
		return nil
	}
	out := make(map[string]struct{}, len(modules.Submodules))
	for _, sub := range modules.Submodules {
		if sub != nil && sub.Path != "" {
			out[path.Clean(filepath.ToSlash(sub.Path))] = struct{}{}
		}
	}
	return out
}
