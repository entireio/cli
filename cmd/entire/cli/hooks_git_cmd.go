package cli

import (
	"context"
	"log/slog"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/strategy"

	"github.com/spf13/cobra"
)

const unknownStrategyName = "unknown"

// gitHookContext holds common state for git hook logging.
type gitHookContext struct {
	hookName     string
	ctx          context.Context
	start        time.Time
	strategy     strategy.Strategy
	strategyName string
}

// newGitHookContext creates a new git hook context with logging initialized.
func newGitHookContext(hookName string) *gitHookContext {
	g := &gitHookContext{
		hookName:     hookName,
		start:        time.Now(),
		ctx:          logging.WithComponent(context.Background(), "hooks"),
		strategyName: unknownStrategyName,
	}
	g.strategy = GetStrategy()
	g.strategyName = g.strategy.Name()
	return g
}

// logInvoked logs that the hook was invoked.
func (g *gitHookContext) logInvoked(extraAttrs ...any) {
	attrs := []any{
		slog.String("hook", g.hookName),
		slog.String("hook_type", "git"),
		slog.String("strategy", g.strategyName),
	}
	logging.Debug(g.ctx, g.hookName+" hook invoked", append(attrs, extraAttrs...)...)
}

// logCompleted logs hook completion with duration at DEBUG level.
// The actual work logging (checkpoint operations) happens at INFO level in the handlers.
func (g *gitHookContext) logCompleted(err error, extraAttrs ...any) {
	attrs := []any{
		slog.String("hook", g.hookName),
		slog.String("hook_type", "git"),
		slog.String("strategy", g.strategyName),
		slog.Bool("success", err == nil),
	}
	logging.LogDuration(g.ctx, slog.LevelDebug, g.hookName+" hook completed", g.start, append(attrs, extraAttrs...)...)
}

// initHookLogging initializes logging for hooks by finding the most recent session.
// Returns a cleanup function that should be deferred.
func initHookLogging() func() {
	// Set up log level getter so logging can read from settings
	logging.SetLogLevelGetter(GetLogLevel)

	// Read session ID for the slog attribute (empty string is fine - log file is fixed)
	sessionID := strategy.FindMostRecentSession()
	if err := logging.Init(sessionID); err != nil {
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
		PersistentPreRunE: func(_ *cobra.Command, _ []string) error {
			hookLogCleanup = initHookLogging()
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
		RunE: func(_ *cobra.Command, args []string) error {
			commitMsgFile := args[0]
			var source string
			if len(args) > 1 {
				source = args[1]
			}

			g := newGitHookContext("prepare-commit-msg")
			g.logInvoked(slog.String("source", source))

			if handler, ok := g.strategy.(strategy.PrepareCommitMsgHandler); ok {
				hookErr := handler.PrepareCommitMsg(commitMsgFile, source)
				g.logCompleted(hookErr, slog.String("source", source))
			}

			return nil
		},
	}
}

func newHooksGitCommitMsgCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "commit-msg <commit-msg-file>",
		Short: "Handle commit-msg git hook",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			commitMsgFile := args[0]

			g := newGitHookContext("commit-msg")
			g.logInvoked()

			if handler, ok := g.strategy.(strategy.CommitMsgHandler); ok {
				hookErr := handler.CommitMsg(commitMsgFile)
				g.logCompleted(hookErr)
				return hookErr //nolint:wrapcheck // Thin delegation layer - wrapping adds no value
			}

			return nil
		},
	}
}

func newHooksGitPostCommitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "post-commit",
		Short: "Handle post-commit git hook",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			g := newGitHookContext("post-commit")
			g.logInvoked()

			if handler, ok := g.strategy.(strategy.PostCommitHandler); ok {
				hookErr := handler.PostCommit()
				g.logCompleted(hookErr)
			}

			// Trigger wingman review after commit (manual-commit strategy).
			// Auto-commit triggers from the stop hook instead.
			triggerWingmanFromCommit()

			return nil
		},
	}
}

func newHooksGitPrePushCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pre-push <remote>",
		Short: "Handle pre-push git hook",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			remote := args[0]

			g := newGitHookContext("pre-push")
			g.logInvoked(slog.String("remote", remote))

			if handler, ok := g.strategy.(strategy.PrePushHandler); ok {
				hookErr := handler.PrePush(remote)
				g.logCompleted(hookErr, slog.String("remote", remote))
			}

			return nil
		},
	}
}
