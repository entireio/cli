package strategy

import (
	"context"
	"log/slog"

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
func FilterTrackableChanges(ctx context.Context, repoRoot string, modified, added, deleted []string) (keptModified, keptAdded, keptDeleted []string) {
	changed := make([]string, 0, len(modified)+len(added))
	changed = append(changed, modified...)
	changed = append(changed, added...)
	all := append(append([]string(nil), changed...), deleted...)
	if len(all) == 0 {
		return modified, added, deleted
	}
	logCtx := logging.WithComponent(ctx, "checkpoint")

	ignored, err := gitrepo.IgnoredPaths(ctx, repoRoot, changed)
	if err != nil {
		logging.Warn(logCtx, "could not check ignore rules for touched files; keeping them",
			slog.String("error", err.Error()))
		ignored = nil
	}
	gitlinks, err := gitrepo.GitlinkPaths(repoRoot, all)
	if err != nil {
		logging.Debug(logCtx, "could not check touched files for submodules; keeping them",
			slog.String("error", err.Error()))
	}
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

// filterTrackableFiles is FilterTrackableChanges for one list of changed
// (modified or created) paths, such as transcript-extracted files.
func filterTrackableFiles(ctx context.Context, repoRoot string, files []string) []string {
	kept, _, _ := FilterTrackableChanges(ctx, repoRoot, files, nil, nil)
	return kept
}
