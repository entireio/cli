package cli

import (
	"context"
	"log/slog"
	"path/filepath"
	"runtime/debug"

	"github.com/entireio/cli/cmd/entire/cli/binding"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/perf"
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

// recordForeignEvidence resolves out-of-repo evidence paths (the paths the
// FilterAndNormalizePaths clamp discarded) to their repos and records every
// foreign repo in the machine-level session record — enabled or not: the
// Enabled flag preserves evidence for lazy-enable integration, and slice 2
// filters on it. Best-effort throughout: every failure logs Debug and moves
// on, and a panic is swallowed the same way — capture is never blocked.
// Callers guard with len(foreign) > 0 so the hot path (every single-repo
// session) pays nothing.
func recordForeignEvidence(ctx context.Context, sessionID string, meta binding.SessionMeta, currentWorktreeRoot string, foreignPaths []string) {
	logCtx := logging.WithComponent(ctx, "binding")
	defer func() {
		if r := recover(); r != nil {
			logging.Debug(logCtx, "binding tap panicked; evidence dropped, capture unaffected",
				slog.Any("panic", r),
				slog.String("stack", string(debug.Stack())))
		}
	}()
	if len(foreignPaths) == 0 {
		return
	}
	// unknownSessionID passes ValidateSessionID and would create a
	// sessions/unknown.json aggregating unrelated sessions.
	if sessionID == "" || sessionID == unknownSessionID {
		return
	}
	_, span := perf.Start(ctx, "binding_tap")
	defer span.End()

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
