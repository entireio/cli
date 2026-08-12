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
)

// MaybeEnsureGlobalSetup performs the lazy, invisible, once-per-clone setup
// for a globally tracked repo: a repo with no repo-level settings whose hooks
// run because the user-global tier is active (settings.GlobalModeActive).
// It installs the git hooks (skipped when core.hooksPath resolves inside the
// worktree — see below), ensures the checkpoint metadata ref, and marks the
// clone preferences with global_setup_completed. Everything it writes lives
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
// to see them. The clone-prefs marker makes the repeat call cheap: one
// preferences read, no git work.
func MaybeEnsureGlobalSetup(ctx context.Context) {
	logCtx := logging.WithComponent(ctx, "global-setup")

	// Repo-level setup owns installation via EnsureSetup / `entire enable`.
	// Checked first so the common repo-enabled path exits on two Lstats,
	// without reading clone preferences.
	if settings.IsSetUpAny(ctx) {
		return
	}

	// Among the global-mode-only steps, the clone-prefs marker comes first:
	// it is the "already done" signal that makes the repeat call cheap.
	prefs, err := settings.LoadClonePreferences(ctx)
	if err != nil {
		// A corrupt or unreadable preferences file must not permanently kill
		// the lazy setup. Treat it as fresh and attempt setup; the marker
		// write at the end goes through ModifyClonePreferences, which
		// recreates a corrupt file from scratch under the preferences lock.
		logging.Debug(logCtx, "global lazy setup: clone preferences unreadable; treating as fresh",
			slog.String("error", err.Error()))
		prefs = &settings.ClonePreferences{}
	}
	if prefs.GlobalSetupCompleted {
		return
	}
	if !settings.GlobalModeActive(ctx) {
		return
	}

	if !IsGitHookInstalled(ctx) {
		worktreeResident, hooksDir, resolveErr := hooksDirIsWorktreeResident(ctx)
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
			localDev, absoluteHookPath := hookSettingsFromConfig(ctx)
			// installGitHooks with a discarded notice writer: lazy setup runs
			// inside agent hooks, where a "[entire] Backed up existing..."
			// stderr line would leak into the agent's output.
			if _, err := installGitHooks(ctx, true, localDev, absoluteHookPath, io.Discard); err != nil {
				logging.Debug(logCtx, "global lazy setup: git hook install failed",
					slog.String("error", err.Error()))
				return
			}
		}
	}

	repo, err := OpenRepository(ctx)
	if err != nil {
		logging.Debug(logCtx, "global lazy setup: open repository failed",
			slog.String("error", err.Error()))
		return
	}
	defer repo.Close()
	// Same ref bootstrap as EnsureSetup; no WithCheckpointRemoteBootstrap —
	// this runs on the hook hot path, which must stay network-free.
	if err := EnsurePrimaryRef(ctx, repo); err != nil {
		logging.Debug(logCtx, "global lazy setup: ensure primary metadata ref failed",
			slog.String("error", err.Error()))
		return
	}

	// Marked only after hooks and ref succeeded, so partial failures retry on
	// the next hook instead of being latched as done. (The deliberate
	// exception is the skipped-hooks case above, which is a stable property
	// of the repo, not a transient failure.)
	if err := settings.ModifyClonePreferences(ctx, func(p *settings.ClonePreferences) error {
		p.GlobalSetupCompleted = true
		return nil
	}); err != nil {
		logging.Debug(logCtx, "global lazy setup: marking clone preferences failed",
			slog.String("error", err.Error()))
		return
	}
	logging.Debug(logCtx, "global lazy setup completed")
}

// hooksDirIsWorktreeResident reports whether the repo's active hooks
// directory (after core.hooksPath resolution) lives in the WORKING TREE —
// inside the worktree root but outside the git dir. The default .git/hooks is
// under the worktree root lexically but inside the git dir, so it is not
// worktree-resident; a committed hooks dir like core.hooksPath=.husky is.
// Returns the resolved hooks dir for logging.
func hooksDirIsWorktreeResident(ctx context.Context) (bool, string, error) {
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
	if !settings.IsSetUpAny(ctx) {
		MaybeEnsureGlobalSetup(ctx)
		return nil
	}
	return EnsureSetup(ctx)
}
