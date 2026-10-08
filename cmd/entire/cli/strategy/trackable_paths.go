package strategy

import (
	"context"
	"log/slog"
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
//   - submodule gitlinks (mode 160000 in HEAD or the index), from all three:
//     a submodule pointer is not a file, the prepare hook would stamp a
//     trailer for it, and PostCommit could never match it.
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

	ignored, gitlinks := trackablePaths.classify(ctx, logCtx, repoRoot, changed, all)
	if len(ignored) == 0 && len(gitlinks) == 0 {
		return modified, added, deleted
	}

	dropped := 0
	keep := func(list []string, checkIgnore bool) []string {
		out := make([]string, 0, len(list))
		for _, path := range list {
			if _, isLink := gitlinks[path]; isLink {
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
	logging.Debug(logCtx, "dropped gitignored and submodule paths from files touched", slog.Int("count", dropped))
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

// classify returns the ignored paths among ignoreCandidates and the gitlinks
// among gitlinkCandidates, asking git only about paths without a cached
// answer. The lock is not held while git runs.
func (c *trackablePathCache) classify(ctx, logCtx context.Context, repoRoot string, ignoreCandidates, gitlinkCandidates []string) (ignored, gitlinks map[string]struct{}) {
	ignored = make(map[string]struct{})
	gitlinks = make(map[string]struct{})

	c.mu.Lock()
	ignoreQuery := c.lookup(c.ignored, repoRoot, ignoreCandidates, ignored)
	gitlinkQuery := c.lookup(c.gitlink, repoRoot, gitlinkCandidates, gitlinks)
	c.mu.Unlock()

	if len(ignoreQuery) > 0 {
		answers, err := gitrepo.IgnoredPaths(ctx, repoRoot, ignoreQuery)
		if err != nil {
			logging.Warn(logCtx, "could not check ignore rules for touched files; keeping them",
				slog.String("error", err.Error()))
		} else {
			c.store(&c.ignored, repoRoot, ignoreQuery, answers, ignored)
		}
	}
	if len(gitlinkQuery) > 0 {
		answers, err := gitrepo.GitlinkPaths(ctx, repoRoot, gitlinkQuery)
		if err != nil {
			// Partial answers are not cached: the next call asks again.
			logging.Debug(logCtx, "could not check touched files for submodules; keeping them",
				slog.String("error", err.Error()))
			for path := range answers {
				gitlinks[path] = struct{}{}
			}
		} else {
			c.store(&c.gitlink, repoRoot, gitlinkQuery, answers, gitlinks)
		}
	}
	return ignored, gitlinks
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
