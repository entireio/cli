package strategy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/settings/repopolicy"
)

const globalSetupComponentSpec = 1

// MaybeEnsureGlobalSetup performs lazy, invisible per-worktree setup
// for a globally tracked repo: a repo with no explicit repo activation whose
// hooks run because the user-global tier is active (settings.GlobalModeActive).
// It installs the git hooks (skipped when core.hooksPath resolves inside the
// worktree — see below), ensures the checkpoint metadata ref, and records the
// completed components in the worktree registry. Everything it writes lives
// inside .git/; it never creates a worktree file. Called from both hook routes
// (agent hooks in executeAgentHook, git hooks in the hooks-git
// PersistentPreRunE), in both cases after hook logging is initialized so the
// failure ladder below is actually recorded.
//
// Best-effort by contract: this runs on hook hot paths, so every failure logs
// at Debug and returns — a lazy-enable failure must never break the user's
// commit or agent session. Both hook routes initialize logging before calling
// this, so those Debug lines reach the routed log file; globally tracked
// repos have no repo settings to raise log_level, so set ENTIRE_LOG_LEVEL=debug
// to see them. The per-worktree component record makes repeat calls cheap and
// lets a later invocation repair only the component that became stale.
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

	repository := repopolicy.Repository{
		WorktreeRoot: policy.WorktreeRoot,
		GitCommonDir: policy.GitCommonDir,
		WorktreeKey:  policy.WorktreeKey,
	}
	record, _, err := repopolicy.ReadSetupRecord(repository)
	if err != nil {
		logging.Debug(logCtx, "global lazy setup: setup record unreadable; skipping",
			slog.String("error", err.Error()))
		return
	}
	changed := false
	if record.GitHooksSpec < globalSetupComponentSpec || !IsGitHookInstalled(ctx) {
		worktreeResident, hooksDir, resolveErr := HooksDirIsWorktreeResident(ctx)
		switch {
		case resolveErr != nil:
			// Can't tell where the hooks would land — installing anyway could
			// write into the worktree. Return without the marker so the next
			// hook retries.
			logging.Debug(logCtx, "global lazy setup: cannot resolve hooks dir",
				slog.String("error", resolveErr.Error()))
			return
		case worktreeResident:
			// core.hooksPath resolves inside the worktree (e.g. a committed
			// .husky directory). Installing would create worktree files,
			// which global tracking must never do — skip the hooks half.
			// Agent-side capture still works; commit-time trailers require a
			// repo-level `entire enable`. The marker is still set below:
			// setup did everything it safely could, and `entire doctor`
			// explains the hooksPath situation.
			logging.Debug(logCtx, "global lazy setup: hooks dir inside worktree; skipping git hook install",
				slog.String("hooks_dir", hooksDir))
		default:
			if IsGitHookInstalled(ctx) {
				record.GitHooksSpec = globalSetupComponentSpec
				changed = true
				break
			}
			absoluteHookPath := hookSettingsFromConfig(ctx)
			// installGitHooks with a discarded notice writer: lazy setup runs
			// inside agent hooks, where a "[entire] Backed up existing..."
			// stderr line would leak into the agent's output.
			if _, err := installGitHooks(ctx, true, absoluteHookPath, io.Discard); err != nil {
				logging.Debug(logCtx, "global lazy setup: git hook install failed",
					slog.String("error", err.Error()))
				return
			}
			record.GitHooksSpec = globalSetupComponentSpec
			changed = true
		}
	}

	if record.PrimaryRefSpec < globalSetupComponentSpec {
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
		record.PrimaryRefSpec = globalSetupComponentSpec
		changed = true
	}

	if !changed {
		return
	}
	if err := repopolicy.WriteSetupRecord(repository, record); err != nil {
		logging.Debug(logCtx, "global lazy setup: writing setup record failed",
			slog.String("error", err.Error()))
		return
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
