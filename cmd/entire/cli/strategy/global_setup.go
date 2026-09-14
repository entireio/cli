package strategy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/entiredir"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/settings/repopolicy"
)

// primaryRefStamp marks that this worktree's primary metadata ref has been
// ensured once. It lives in the (git-side) runtime root, so it is never a
// worktree file, and it exists only to keep the per-hook cost at a stat:
// EnsurePrimaryRefTo opens the repository and may talk to remotes.
const primaryRefStamp = "primary-ref-ready"

// MaybeEnsureGlobalSetup performs lazy, invisible per-worktree setup for a
// globally tracked repo: a repo with no repo-level activation whose hooks run
// because the user-global tier is active. It installs the git hooks (skipped
// when core.hooksPath resolves inside the worktree — see below) and ensures
// the checkpoint metadata ref. Everything it writes lives inside .git/; it
// never creates a worktree file. Called from both hook routes (agent hooks in
// executeAgentHook, git hooks in the hooks-git PersistentPreRun), after hook
// logging is initialized so the failure ladder below is recorded.
//
// Best-effort by contract: this runs on hook hot paths, so every failure logs
// at Debug and returns — a lazy-enable failure must never break the user's
// commit or agent session. Git-hook presence is re-checked on every hook (a
// file read), so hooks a user deleted or that are out of date come back on
// the next activity without any marker bookkeeping.
func MaybeEnsureGlobalSetup(ctx context.Context) {
	logCtx := logging.WithComponent(ctx, "global-setup")

	policy, ok := repopolicy.RepoPolicyFromContext(ctx)
	if !ok {
		var err error
		policy, err = repopolicy.ClassifyRepoPolicy(ctx)
		if err != nil {
			logging.Debug(logCtx, "global lazy setup: policy unavailable", slog.String("error", err.Error()))
			return
		}
	}
	if !policy.Active || policy.ActivationSource != repopolicy.ActivationGlobal {
		return
	}

	if !IsGitHookInstalled(ctx) {
		worktreeResident, hooksDir, resolveErr := HooksDirIsWorktreeResident(ctx)
		switch {
		case resolveErr != nil:
			// Can't tell where the hooks would land — installing anyway could
			// write into the worktree. Return so the next hook retries.
			logging.Debug(logCtx, "global lazy setup: cannot resolve hooks dir",
				slog.String("error", resolveErr.Error()))
			return
		case worktreeResident:
			// core.hooksPath resolves inside the worktree (e.g. a committed
			// .husky directory). Installing would create worktree files,
			// which global tracking must never do — skip the hooks half.
			// Agent-side capture still works; commit-time trailers require a
			// repo-level `entire enable`. `entire doctor` explains this.
			logging.Debug(logCtx, "global lazy setup: hooks dir inside worktree; skipping git hook install",
				slog.String("hooks_dir", hooksDir))
		default:
			// installGitHooks with a discarded notice writer: lazy setup runs
			// inside agent hooks, where a "[entire] Backed up existing..."
			// stderr line would leak into the agent's output.
			if _, err := installGitHooks(ctx, true, hookSettingsFromConfig(ctx), io.Discard); err != nil {
				logging.Debug(logCtx, "global lazy setup: git hook install failed",
					slog.String("error", err.Error()))
				return
			}
		}
	}

	runtimeRoot, rootErr := entiredir.OpenForRead(ctx)
	if rootErr == nil {
		if _, err := entiredir.ReadFile(runtimeRoot, primaryRefStamp); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			logging.Debug(logCtx, "global lazy setup: primary-ref stamp unavailable",
				slog.String("error", err.Error()))
			return
		}
	} else if !errors.Is(rootErr, os.ErrNotExist) {
		logging.Debug(logCtx, "global lazy setup: runtime root unavailable",
			slog.String("error", rootErr.Error()))
		return
	}
	repo, openErr := OpenRepository(ctx)
	if openErr != nil {
		logging.Debug(logCtx, "global lazy setup: open repository failed",
			slog.String("error", openErr.Error()))
		return
	}
	defer repo.Close()
	if err := EnsurePrimaryRefTo(ctx, repo, io.Discard); err != nil {
		logging.Debug(logCtx, "global lazy setup: ensure primary metadata ref failed",
			slog.String("error", err.Error()))
		return
	}
	runtimeRoot, rootErr = entiredir.Open(ctx)
	if rootErr == nil {
		_ = entiredir.WriteFile(runtimeRoot, primaryRefStamp, nil, 0o600) //nolint:errcheck // a missing stamp only costs a repeat ensure next hook
	}
	logging.Debug(logCtx, "global lazy setup completed")
}

// errHooksDirProbeForTesting, when non-nil, forces HooksDirIsWorktreeResident
// to fail. Its real failure modes (git exec failing while git otherwise keeps
// working) cannot be produced portably on disk, so tests inject the failure
// here — same convention as paths.SetInvisibleProbeFailureForTesting.
var errHooksDirProbeForTesting error

// SetHooksDirProbeErrorForTesting forces HooksDirIsWorktreeResident to fail
// with err; nil restores real probing. Test-only.
func SetHooksDirProbeErrorForTesting(err error) {
	errHooksDirProbeForTesting = err
}

// HooksDirIsWorktreeResident reports whether the repo's active hooks
// directory (after core.hooksPath resolution) lives in the WORKING TREE —
// inside the worktree root but outside the git dir. The default .git/hooks is
// under the worktree root lexically but inside the git dir, so it is not
// worktree-resident; a committed hooks dir like core.hooksPath=.husky is.
// Returns the resolved hooks dir for logging.
func HooksDirIsWorktreeResident(ctx context.Context) (bool, string, error) {
	if errHooksDirProbeForTesting != nil {
		return false, "", errHooksDirProbeForTesting
	}
	hooksDir, err := GetHooksDir(ctx)
	if err != nil {
		return false, "", err
	}
	worktreeRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return false, "", fmt.Errorf("resolve worktree root: %w", err)
	}
	gitDir, err := GetGitDir(ctx)
	if err != nil {
		return false, "", err
	}
	// GetHooksDir and GetGitDir can return cwd-relative results (git emits
	// them relative when possible); absolutize before comparing.
	hooksDir, err = absPathBestEffort(hooksDir)
	if err != nil {
		return false, "", err
	}
	gitDir, err = absPathBestEffort(gitDir)
	if err != nil {
		return false, "", err
	}
	// Resolve symlinks on all sides so the containment check cannot be
	// defeated by e.g. macOS's /var → /private/var: a false "outside" here
	// would install hooks into the worktree of a repo that must stay
	// invisible.
	hooksDir = resolveExistingPrefix(hooksDir)
	worktreeRoot = resolveExistingPrefix(worktreeRoot)
	gitDir = resolveExistingPrefix(gitDir)
	resident := paths.IsSubpath(worktreeRoot, hooksDir) && !paths.IsSubpath(gitDir, hooksDir)
	return resident, hooksDir, nil
}

// absPathBestEffort absolutizes p against the cwd when it is relative.
func absPathBestEffort(p string) (string, error) {
	if filepath.IsAbs(p) {
		return p, nil
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err //nolint:wrapcheck // internal probe; caller logs it
	}
	return abs, nil
}

// resolveExistingPrefix resolves symlinks in p. If p itself does not exist
// (e.g. a configured-but-uncreated hooks dir), it resolves the nearest
// existing ancestor and rejoins the missing suffix, so containment checks
// still compare canonical paths.
func resolveExistingPrefix(p string) string {
	p = filepath.Clean(p)
	suffix := ""
	for {
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(resolved, suffix)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return filepath.Join(p, suffix)
		}
		suffix = filepath.Join(filepath.Base(p), suffix)
		p = parent
	}
}

// EnsureSetupForHook is the hook-path variant of EnsureSetup. Repos with
// repo-level setup get the full EnsureSetup (which may write .entire/.gitignore
// and other worktree files). Repos without any repo-level setup must never
// receive worktree writes: when the user-global tier is active they get the
// invisible MaybeEnsureGlobalSetup instead, otherwise nothing happens.
func EnsureSetupForHook(ctx context.Context) error {
	repoConfigured, err := settings.RepoActivationConfigured(ctx)
	if err != nil {
		// Hook setup is best-effort. A malformed settings file must fail
		// closed without turning the user's hook invocation into an error.
		logging.Debug(logging.WithComponent(ctx, "global-setup"),
			"hook setup: repo settings unreadable; skipping",
			slog.String("error", err.Error()))
		return nil
	}
	if !repoConfigured {
		MaybeEnsureGlobalSetup(ctx)
		return nil
	}
	// Hook context: discard ref-bootstrap notices — a ✓ line on stderr here
	// would leak into the agent's hook output.
	return ensureSetup(ctx, io.Discard)
}
