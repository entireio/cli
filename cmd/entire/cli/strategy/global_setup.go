package strategy

import (
	"context"
	"log/slog"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// MaybeEnsureGlobalSetup performs the lazy, invisible, once-per-clone setup
// for a globally tracked repo: a repo with no repo-level settings whose hooks
// run because the user-global tier is active (settings.GlobalModeActive).
// It installs the git hooks, ensures the checkpoint metadata ref, and marks
// the clone preferences with globally_enabled — everything inside .git/, never
// a worktree file. Called from both hook routes (agent hooks in
// executeAgentHook, git hooks in the hooks-git PersistentPreRunE).
//
// Best-effort by contract: this runs on hook hot paths, so every failure logs
// at Debug and returns — a lazy-enable failure must never break the user's
// commit or agent session. The clone-prefs marker makes the repeat call cheap:
// one preferences read, no git work.
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
		logging.Debug(logCtx, "global lazy setup: clone preferences unreadable",
			slog.String("error", err.Error()))
		return
	}
	if prefs.GloballyEnabled {
		return
	}
	if !settings.GlobalModeActive(ctx) {
		return
	}

	if !IsGitHookInstalled(ctx) {
		localDev, absoluteHookPath := hookSettingsFromConfig(ctx)
		if _, err := InstallGitHook(ctx, true, localDev, absoluteHookPath); err != nil {
			logging.Debug(logCtx, "global lazy setup: git hook install failed",
				slog.String("error", err.Error()))
			return
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
	// the next hook instead of being latched as done.
	if err := settings.ModifyClonePreferences(ctx, func(p *settings.ClonePreferences) error {
		p.GloballyEnabled = true
		return nil
	}); err != nil {
		logging.Debug(logCtx, "global lazy setup: marking clone preferences failed",
			slog.String("error", err.Error()))
		return
	}
	logging.Debug(logCtx, "global lazy setup completed")
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
