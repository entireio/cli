package cli

import (
	"context"
	"log/slog"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/perf"

	"github.com/spf13/cobra"
)

// gitHooksDisabled is set by PersistentPreRunE when Entire is not set up or disabled.
// When true, all git hook commands return early without doing any work.
var gitHooksDisabled bool

// gitHookContext holds common state for git hook logging.
type gitHookContext struct {
	hookName string
	ctx      context.Context
	span     *perf.Span
	strategy *strategy.ManualCommitStrategy
}

// newGitHookContext creates a new git hook context with logging and a root perf span.
// The perf span ensures all perf.Start calls in strategy methods become child spans,
// producing a single perf log line per hook with a full timing breakdown.
// Callers must defer g.span.End() to emit the perf log.
func newGitHookContext(ctx context.Context, hookName string) *gitHookContext {
	ctx = logging.WithComponent(ctx, "hooks")
	ctx, span := perf.Start(ctx, hookName,
		slog.String("hook_type", "git"))
	g := &gitHookContext{
		hookName: hookName,
		ctx:      ctx,
		span:     span,
	}
	g.strategy = GetStrategy(ctx)
	return g
}

// logInvoked logs that the hook was invoked.
func (g *gitHookContext) logInvoked(extraAttrs ...any) {
	attrs := []any{
		slog.String("hook", g.hookName),
		slog.String("hook_type", "git"),
		slog.String("strategy", strategy.StrategyNameManualCommit),
	}
	logging.Debug(g.ctx, g.hookName+" hook invoked", append(attrs, extraAttrs...)...)
}

// logCompleted records the error on the perf span.
func (g *gitHookContext) logCompleted(err error) {
	g.span.RecordError(err)
}

// initHookLogging initializes logging for hooks by finding the most recent session.
// Returns a cleanup function that should be deferred.
// If Entire is not set up or disabled, returns a no-op to avoid creating files.
func initHookLogging(ctx context.Context) func() {
	// Don't create any files if Entire is not set up or disabled.
	// This is checked here as defense-in-depth (also checked in PersistentPreRunE).
	if !settings.IsSetUpAndEnabled(ctx) {
		return func() {}
	}

	// Set up log level getter so logging can read from settings
	logging.SetLogLevelGetter(GetLogLevel)

	// Read session ID for the slog attribute (empty string is fine - log file is fixed)
	sessionID := strategy.FindMostRecentSession(ctx)
	if err := logging.Init(ctx, sessionID); err != nil {
		// Init failed - logging will use stderr fallback
		return func() {}
	}
	return logging.Close
}

// hookLogCleanup stores the cleanup function for hook logging.
// Set by PersistentPreRunE, called by PersistentPostRunE.
var hookLogCleanup func()

func newHooksGitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "git",
		Short:  "Git hook handlers",
		Long:   "Commands called by git hooks. These delegate to the current strategy.",
		Hidden: true, // Internal command, not for direct user use
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			// Check if Entire is set up and enabled before doing any work.
			// This prevents global git hooks from doing anything in repos where
			// Entire was never enabled or has been disabled.
			if !settings.IsSetUpAndEnabled(ctx) {
				gitHooksDisabled = true
				return nil
			}
			hookLogCleanup = initHookLogging(ctx)
			return nil
		},
		PersistentPostRunE: func(_ *cobra.Command, _ []string) error {
			if hookLogCleanup != nil {
				hookLogCleanup()
			}
			return nil
		},
	}

	cmd.AddCommand(newHooksGitPrepareCommitMsgCmd())
	cmd.AddCommand(newHooksGitCommitMsgCmd())
	cmd.AddCommand(newHooksGitPostCommitCmd())
	cmd.AddCommand(newHooksGitPrePushCmd())

	return cmd
}

func newHooksGitPrepareCommitMsgCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "prepare-commit-msg <commit-msg-file> [source]",
		Short: "Handle prepare-commit-msg git hook",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if gitHooksDisabled {
				return nil
			}

			commitMsgFile := args[0]
			var source string
			if len(args) > 1 {
				source = args[1]
			}

			g := newGitHookContext(cmd.Context(), "prepare-commit-msg")
			defer g.span.End()
			g.logInvoked(slog.String("source", source))

			hookErr := g.strategy.PrepareCommitMsg(g.ctx, commitMsgFile, source)
			g.logCompleted(hookErr)

			return nil
		},
	}
}

func newHooksGitCommitMsgCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "commit-msg <commit-msg-file>",
		Short: "Handle commit-msg git hook",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if gitHooksDisabled {
				return nil
			}

			commitMsgFile := args[0]

			g := newGitHookContext(cmd.Context(), "commit-msg")
			defer g.span.End()
			g.logInvoked()

			hookErr := g.strategy.CommitMsg(g.ctx, commitMsgFile)
			g.logCompleted(hookErr)
			return hookErr //nolint:wrapcheck // Thin delegation layer - wrapping adds no value
		},
	}
}

func newHooksGitPostCommitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "post-commit",
		Short: "Handle post-commit git hook",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if gitHooksDisabled {
				return nil
			}

			g := newGitHookContext(cmd.Context(), "post-commit")
			defer g.span.End()
			g.logInvoked()

			hookErr := g.strategy.PostCommit(g.ctx)
			g.logCompleted(hookErr)

			return nil
		},
	}
}

func newHooksGitPrePushCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pre-push <remote>",
		Short: "Handle pre-push git hook",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if gitHooksDisabled {
				return nil
			}

			remote := args[0]

			g := newGitHookContext(cmd.Context(), "pre-push")
			defer g.span.End()
			g.logInvoked(slog.String("remote", remote))

			hookErr := g.strategy.PrePush(g.ctx, remote)
			g.logCompleted(hookErr)

			return nil
		},
	}
}
