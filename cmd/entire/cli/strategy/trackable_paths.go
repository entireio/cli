package strategy

import (
	"context"
	"log/slog"
	pathpkg "path"
	"sync"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// FilterTrackableChanges removes, from repo-relative path lists, the paths
// that are not the session's committable file work, so they never enter
// FilesTouched on any route (turn end, task-record completion, per-tool hooks,
// Codex child files, and the transcript extraction a mid-turn commit falls
// back to):
//
//   - gitignored files, from modified and added: no commit can carry them.
//     Deleted paths are never ignore-checked: a deletion reported by git
//     status is of a tracked path by construction, and `git check-ignore`
//     reports a path whose deletion is staged (it has left the index), which
//     would drop the deletion of a tracked file that matches an ignore rule;
//   - submodule gitlinks (mode 160000 in HEAD or the index), and every path
//     beneath one, from all three: a submodule pointer is not a file, the
//     prepare hook would stamp a trailer for it, and PostCommit could never
//     match it. Edits inside a submodule are not tracked by the superproject's
//     session at all: they belong to the nested repository, whose own session
//     records them. Each path's ancestor directories are part of the gitlink
//     query, so sub/lib.txt is recognized by its gitlink ancestor sub, and
//     such paths are dropped before the ignore check, where git would refuse
//     them;
//   - paths git refuses to ignore-check (gitrepo.IgnoredPaths' refused set),
//     such as one beneath a symlinked directory: git cannot commit them
//     through that path either.
//
// Either kind would keep the session pending forever: it stays in
// carry-forward, re-condenses the full transcript on every later commit, and
// leaves an ENDED session for the zombie sweep.
//
// If git cannot answer either question, the affected paths are kept: tracking
// a path that turns out to be uncommittable is recoverable, silently dropping
// real work is not.
//
// Answers are cached per process (trackablePathCache), so the several sessions
// one hook processes — a commit hook walks every session of the worktree — ask
// git about each path once.
func FilterTrackableChanges(ctx context.Context, repoRoot string, modified, added, deleted []string) (keptModified, keptAdded, keptDeleted []string) {
	changed := make([]string, 0, len(modified)+len(added))
	changed = append(changed, modified...)
	changed = append(changed, added...)
	all := append(append([]string(nil), changed...), deleted...)
	if len(all) == 0 {
		return modified, added, deleted
	}
	logCtx := logging.WithComponent(ctx, "checkpoint")

	gitlinks := trackablePaths.classifyGitlinks(ctx, logCtx, repoRoot, withAncestorDirs(all))
	ignoreCandidates := make([]string, 0, len(changed))
	for _, path := range changed {
		if !underGitlink(path, gitlinks) {
			ignoreCandidates = append(ignoreCandidates, path)
		}
	}
	ignored := trackablePaths.classifyIgnored(ctx, logCtx, repoRoot, ignoreCandidates)
	if len(ignored) == 0 && len(gitlinks) == 0 {
		return modified, added, deleted
	}

	dropped := 0
	keep := func(list []string, checkIgnore bool) []string {
		out := make([]string, 0, len(list))
		for _, path := range list {
			if underGitlink(path, gitlinks) {
				dropped++
				continue
			}
			if _, isIgnored := ignored[path]; checkIgnore && isIgnored {
				dropped++
				continue
			}
			out = append(out, path)
		}
		return out
	}
	keptModified, keptAdded, keptDeleted = keep(modified, true), keep(added, true), keep(deleted, false)
	logging.Debug(logCtx, "dropped gitignored, submodule, and refused paths from files touched", slog.Int("count", dropped))
	return keptModified, keptAdded, keptDeleted
}

// trackablePathCache remembers, per process, git's answers to "is this path
// ignored?" and "is this path a submodule gitlink?", keyed by repository root
// and path, so repeated FilterTrackableChanges calls only ask git about paths
// they have not seen.
//
// A hook is one short-lived process, and within it neither answer changes for
// a path, so caching for the process lifetime is exact for hooks. Longer-lived
// processes that classify paths (doctor, the sweep) also run against one
// repository state. A process that edits .gitignore or adds a submodule and
// then expects a different answer for an already-classified path in the same
// repository would see the stale one; nothing in the CLI does that. Failed
// lookups are not cached, so the next call asks again.
type trackablePathCache struct {
	mu      sync.Mutex
	ignored map[trackablePathKey]bool
	gitlink map[trackablePathKey]bool
}

type trackablePathKey struct {
	repoRoot string
	path     string
}

var trackablePaths = &trackablePathCache{}

// resetTrackablePathCacheForTesting drops every cached answer. Tests that
// rewrite ignore rules or submodules in a repository they already classified
// call it; it is process-global, so they must not run in parallel with tests
// that rely on cached answers.
func resetTrackablePathCacheForTesting() {
	trackablePaths.mu.Lock()
	defer trackablePaths.mu.Unlock()
	trackablePaths.ignored = nil
	trackablePaths.gitlink = nil
}

// classifyIgnored returns the paths among candidates that git ignores or
// refuses to ignore-check (both are dropped; see IgnoredPaths), asking git only
// about paths without a cached answer. The lock is not held while git runs.
func (c *trackablePathCache) classifyIgnored(ctx, logCtx context.Context, repoRoot string, candidates []string) map[string]struct{} {
	ignored := make(map[string]struct{})
	c.mu.Lock()
	query := c.lookup(c.ignored, repoRoot, candidates, ignored)
	c.mu.Unlock()
	if len(query) == 0 {
		return ignored
	}
	answers, refused, err := gitrepo.IgnoredPaths(ctx, repoRoot, query)
	if err != nil {
		logging.Warn(logCtx, "could not check ignore rules for touched files; keeping them",
			slog.String("error", err.Error()))
		return ignored
	}
	if len(refused) > 0 {
		logging.Debug(logCtx, "git refused to ignore-check some touched files (inside a submodule or beneath a symlinked directory); dropping them",
			slog.Int("count", len(refused)))
		for path := range refused {
			answers[path] = struct{}{}
		}
	}
	c.store(&c.ignored, repoRoot, query, answers, ignored)
	return ignored
}

// classifyGitlinks returns the gitlinks among candidates, asking git only
// about paths without a cached answer. The lock is not held while git runs.
func (c *trackablePathCache) classifyGitlinks(ctx, logCtx context.Context, repoRoot string, candidates []string) map[string]struct{} {
	gitlinks := make(map[string]struct{})
	c.mu.Lock()
	query := c.lookup(c.gitlink, repoRoot, candidates, gitlinks)
	c.mu.Unlock()
	if len(query) == 0 {
		return gitlinks
	}
	answers, err := gitrepo.GitlinkPaths(ctx, repoRoot, query)
	if err != nil {
		// Partial answers are not cached: the next call asks again.
		logging.Debug(logCtx, "could not check touched files for submodules; keeping them",
			slog.String("error", err.Error()))
		for path := range answers {
			gitlinks[path] = struct{}{}
		}
		return gitlinks
	}
	c.store(&c.gitlink, repoRoot, query, answers, gitlinks)
	return gitlinks
}

// withAncestorDirs returns paths plus every ancestor directory of each
// ("a/b/c.txt" adds "a" and "a/b"), deduplicated, so a gitlink query also finds
// the submodule a path lies inside.
func withAncestorDirs(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	out := make([]string, 0, len(paths))
	add := func(p string) {
		if _, dup := seen[p]; !dup {
			seen[p] = struct{}{}
			out = append(out, p)
		}
	}
	for _, path := range paths {
		add(path)
		for dir := pathpkg.Dir(path); dir != "." && dir != "/" && dir != ""; dir = pathpkg.Dir(dir) {
			add(dir)
		}
	}
	return out
}

// underGitlink reports whether path is a gitlink or lies beneath one.
func underGitlink(path string, gitlinks map[string]struct{}) bool {
	if len(gitlinks) == 0 {
		return false
	}
	for p := path; p != "." && p != "/" && p != ""; p = pathpkg.Dir(p) {
		if _, ok := gitlinks[p]; ok {
			return true
		}
	}
	return false
}

// lookup copies cached positive answers for paths into hits and returns the
// deduplicated paths with no cached answer. Caller holds c.mu.
func (c *trackablePathCache) lookup(cache map[trackablePathKey]bool, repoRoot string, paths []string, hits map[string]struct{}) []string {
	var missing []string
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if _, dup := seen[path]; dup {
			continue
		}
		seen[path] = struct{}{}
		answer, ok := cache[trackablePathKey{repoRoot: repoRoot, path: path}]
		switch {
		case !ok:
			missing = append(missing, path)
		case answer:
			hits[path] = struct{}{}
		}
	}
	return missing
}

// store records git's answers for every queried path and copies the positive
// ones into hits.
func (c *trackablePathCache) store(cache *map[trackablePathKey]bool, repoRoot string, queried []string, answers, hits map[string]struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if *cache == nil {
		*cache = make(map[trackablePathKey]bool)
	}
	for _, path := range queried {
		_, yes := answers[path]
		(*cache)[trackablePathKey{repoRoot: repoRoot, path: path}] = yes
		if yes {
			hits[path] = struct{}{}
		}
	}
}

// filterTrackableFiles is FilterTrackableChanges for one list of changed
// (modified or created) paths, such as transcript-extracted files.
func filterTrackableFiles(ctx context.Context, repoRoot string, files []string) []string {
	kept, _, _ := FilterTrackableChanges(ctx, repoRoot, files, nil, nil)
	return kept
}
