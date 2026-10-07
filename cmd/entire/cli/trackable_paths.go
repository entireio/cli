package cli

import (
	"context"
	"log/slog"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// dropUntrackablePaths removes, from each repo-relative path list, the paths
// no commit can ever carry, so they never enter a session's FilesTouched:
// gitignored files. Such a path would keep the session pending forever — it
// stays in carry-forward, re-condenses the full transcript on every later
// commit, and leaves an ENDED session for the zombie sweep.
//
// All lists are checked in one `git check-ignore` batch. If git cannot
// answer, the lists are returned unchanged: tracking a path that turns out to
// be uncommittable is recoverable, silently dropping real work is not.
func dropUntrackablePaths(ctx context.Context, repoRoot string, lists ...[]string) [][]string {
	var all []string
	for _, list := range lists {
		all = append(all, list...)
	}
	if len(all) == 0 {
		return lists
	}
	logCtx := logging.WithComponent(ctx, "checkpoint")
	ignored, err := gitrepo.IgnoredPaths(ctx, repoRoot, all)
	if err != nil {
		logging.Warn(logCtx, "could not check ignore rules for touched files; keeping them",
			slog.String("error", err.Error()))
		return lists
	}
	if len(ignored) == 0 {
		return lists
	}
	out := make([][]string, len(lists))
	dropped := 0
	for i, list := range lists {
		kept := make([]string, 0, len(list))
		for _, path := range list {
			if _, skip := ignored[path]; skip {
				dropped++
				continue
			}
			kept = append(kept, path)
		}
		out[i] = kept
	}
	logging.Debug(logCtx, "dropped gitignored paths from files touched", slog.Int("count", dropped))
	return out
}
