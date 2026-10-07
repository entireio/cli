package cli

import (
	"context"
	"log/slog"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/logging"

	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// dropUntrackablePaths removes, from each repo-relative path list, the paths
// that are not the session's committable file work, so they never enter
// FilesTouched:
//
//   - gitignored files: no commit can carry them;
//   - submodule gitlinks (mode 160000 in HEAD or the index): a dirty
//     submodule pointer is not a file, the prepare hook would stamp a trailer
//     for it, and PostCommit could never match it.
//
// Either kind would keep the session pending forever — it stays in
// carry-forward, re-condenses the full transcript on every later commit, and
// leaves an ENDED session for the zombie sweep.
//
// Ignore rules are checked in one `git check-ignore` batch. If git cannot
// answer either question, the affected paths are kept: tracking a path that
// turns out to be uncommittable is recoverable, silently dropping real work is
// not.
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
		ignored = nil
	}
	for path := range gitlinkPaths(logCtx, repoRoot, all) {
		if ignored == nil {
			ignored = make(map[string]struct{})
		}
		ignored[path] = struct{}{}
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
	logging.Debug(logCtx, "dropped gitignored and submodule paths from files touched", slog.Int("count", dropped))
	return out
}

// gitlinkPaths returns the paths that are submodule gitlinks (mode 160000) in
// HEAD's tree or in the index, so a submodule added but not yet committed
// counts too. Errors yield an empty set.
func gitlinkPaths(ctx context.Context, repoRoot string, paths []string) map[string]struct{} {
	found := make(map[string]struct{})
	repo, err := gitrepo.OpenPath(repoRoot)
	if err != nil {
		logging.Debug(ctx, "could not open repository to check for submodules", slog.String("error", err.Error()))
		return found
	}
	defer repo.Close()

	var headTree *object.Tree
	if head, headErr := repo.Head(); headErr == nil {
		if commit, commitErr := repo.CommitObject(head.Hash()); commitErr == nil {
			headTree, _ = commit.Tree() //nolint:errcheck // a missing tree just skips the HEAD check
		}
	}
	idx, idxErr := repo.Storer.Index()
	for _, path := range paths {
		if headTree != nil {
			if entry, findErr := headTree.FindEntry(path); findErr == nil && entry.Mode == filemode.Submodule {
				found[path] = struct{}{}
				continue
			}
		}
		if idxErr == nil {
			if entry, entryErr := idx.Entry(path); entryErr == nil && entry.Mode == filemode.Submodule {
				found[path] = struct{}{}
			}
		}
	}
	return found
}
