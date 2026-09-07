package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/external"
	"github.com/entireio/cli/cmd/entire/cli/checkpointpolicy"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/telemetry"
	"github.com/entireio/cli/cmd/entire/cli/versioncheck"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/entireio/cli/perf"

	"github.com/spf13/cobra"
)

// gitHooksDisabled is set by PersistentPreRun when Entire is not set up or disabled.
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

func (g *gitHookContext) skipUnsupportedCheckpointPolicy() bool {
	// Callers return success when this is true because policy failures should
	// disable Entire checkpoint work, not make Git reject the user's operation.
	repo, err := gitrepo.OpenCurrent(g.ctx)
	if err != nil {
		return g.skipUnreadableCheckpointPolicy(err)
	}
	defer repo.Close()

	state, err := checkpointpolicy.ReadLocal(g.ctx, repo)
	if err != nil {
		return g.skipUnreadableCheckpointPolicy(err)
	}

	policy := state.Policy
	if checkpointpolicy.CanSatisfyPolicy(policy) {
		return false
	}

	logging.Warn(g.ctx, "checkpoint policy unsupported; skipping git hook",
		slog.String("checkpoint_version", policy.CheckpointVersion),
		slog.String("checkpoint_min_version", policy.CheckpointMinVersion))
	if interactive.CanPromptInteractively() {
		fmt.Fprint(os.Stderr, checkpointpolicy.UnsupportedPolicyMessage(
			policy,
			versioncheck.UpdateCommandForCurrentBinary(versioninfo.Version),
		))
	}
	emitCheckpointPolicyBlocked(g.ctx, telemetry.CheckpointPolicyBlockedEvent{
		Hook:                 g.hookName,
		HookType:             telemetry.PolicyBlockedHookTypeGit,
		Reason:               telemetry.PolicyBlockedReasonUnsupported,
		Outcome:              telemetry.PolicyBlockedOutcomeSkipped,
		CheckpointVersion:    policy.CheckpointVersion,
		CheckpointMinVersion: policy.CheckpointMinVersion,
	})
	return true
}

// skipUnreadableCheckpointPolicy warns, notifies the user, and reports the
// policy-blocked telemetry event for a hook whose checkpoint policy could not be
// read. It always returns true so callers skip Entire checkpoint work.
func (g *gitHookContext) skipUnreadableCheckpointPolicy(err error) bool {
	logging.Warn(g.ctx, "checkpoint policy read failed; skipping git hook",
		slog.String("error", err.Error()))
	if interactive.CanPromptInteractively() {
		fmt.Fprintf(os.Stderr, "[entire] Could not read checkpoint policy; skipping Entire checkpoint work: %v\n", err)
	}
	emitCheckpointPolicyBlocked(g.ctx, telemetry.CheckpointPolicyBlockedEvent{
		Hook:     g.hookName,
		HookType: telemetry.PolicyBlockedHookTypeGit,
		Reason:   telemetry.PolicyBlockedReasonUnreadable,
		Outcome:  telemetry.PolicyBlockedOutcomeSkipped,
	})
	return true
}

// withHookSession adds the session the root pre-run cannot know without
// scanning session state on every command, and configures redaction. Returns
// the context to pass down with cmd.SetContext.
//
// Every caller must gate on settings.IsSetUpAndEnabled first — it scans session
// state and loads redactors, neither of which may touch a repo that never
// enabled Entire. The check is not repeated here: it costs an uncached
// settings.Load, and this runs on the per-commit and per-turn paths.
func withHookSession(ctx context.Context) context.Context {
	ctx = logging.WithSessionID(ctx, strategy.FindMostRecentSession(ctx))

	// Hooks are the checkpoint-writing path, so this cannot be left to the root
	// pre-run: without it only always-on secret scanning would run.
	//
	// A scanner-config failure is logged, not propagated: callers gate on
	// IsSetUpAndEnabled, which already fails closed on the settings error that
	// carries ErrScannerConfig, and the built-in goredact config cannot fail to
	// construct — so this branch is unreachable from a hook. The loud paths for
	// a bad scanner config are status, doctor, import, and attach. If a future
	// engine constructor ever does fail here, the process falls back to the
	// default betterleaks-only set: different coverage, not merely less.
	if err := strategy.EnsureRedactionConfigured(ctx); err != nil {
		logging.Error(logging.WithComponent(ctx, "redaction"),
			"redaction scanner configuration failed",
			slog.String("error", err.Error()))
	}

	return ctx
}

func newHooksGitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "git",
		Short:  "Git hook handlers",
		Long:   "Commands called by git hooks. These delegate to the current strategy.",
		Hidden: true, // Internal command, not for direct user use
		PersistentPreRun: func(cmd *cobra.Command, _ []string) {
			ctx := cmd.Context()
			// Check if Entire is set up and enabled before doing any work.
			// This prevents global git hooks from doing anything in repos where
			// Entire was never enabled or has been disabled.
			if !settings.IsSetUpAndEnabled(ctx) {
				gitHooksDisabled = true
				return
			}
			// Discover external agent plugins so GetByAgentType works correctly
			// during condensation (e.g. post-commit). Without this, external agents
			// registered in the hook phase cannot be resolved here, causing token
			// usage and other agent-specific data to be missing from metadata.json.
			discoveryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			external.DiscoverAndRegister(discoveryCtx)
			// Cobra invokes this PersistentPreRun with the leaf command, so
			// SetContext hands the session-stamped context straight to the
			// verb's RunE via cmd.Context().
			cmd.SetContext(withHookSession(ctx))
		},
	}

	cmd.AddCommand(newHooksGitPrepareCommitMsgCmd())
	cmd.AddCommand(newHooksGitCommitMsgCmd())
	cmd.AddCommand(newHooksGitPostCommitCmd())
	cmd.AddCommand(newHooksGitPostRewriteCmd())
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

			// Ahead of the checkpoint-policy gate on purpose. The guard reads
			// nothing Entire owns and draws no conclusion that depends on the
			// checkpoint policy — it compares the index git is committing from
			// against HEAD — and it is the last moment at which the user can be
			// told before the commit deletes their tracked files.
			warnOnEmptyIndexCommit(g.ctx, cmd.OutOrStdout())

			if g.skipUnsupportedCheckpointPolicy() {
				return nil
			}
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

			if g.skipUnsupportedCheckpointPolicy() {
				return nil
			}
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

			// The prepare-commit-msg guard from the other side of the commit,
			// and the one place Entire can still decline to act on it. Ahead of
			// the checkpoint-policy gate for the same reason as its twin: the
			// question it answers — does HEAD record no files while those files
			// are still on disk — is about the user's commit and depends on no
			// Entire state, and a repo whose policy this binary must not act on
			// still wants to hear that its commit deleted everything.
			if warnOnEmptyTreeCommit(g.ctx, cmd.OutOrStdout()) {
				return nil
			}

			if g.skipUnsupportedCheckpointPolicy() {
				return nil
			}
			hookErr := g.strategy.PostCommit(g.ctx)
			g.logCompleted(hookErr)

			return nil
		},
	}
}

func newHooksGitPostRewriteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "post-rewrite <rewrite-type>",
		Short: "Handle post-rewrite git hook",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if gitHooksDisabled {
				return nil
			}

			g := newGitHookContext(cmd.Context(), "post-rewrite")
			defer g.span.End()
			g.logInvoked(slog.String("rewrite_type", args[0]))

			if g.skipUnsupportedCheckpointPolicy() {
				return nil
			}
			hookErr := g.strategy.PostRewrite(g.ctx, args[0], cmd.InOrStdin())
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
		// SilenceUsage/Errors so non-zero exits from privacy-critical
		// failures (OPF rewrite errors) print only the error message,
		// not cobra's usage banner. The error message itself already
		// includes user guidance (see ErrV1Diverged / ErrBootstrapTooLarge /
		// ErrV1RefMoved in strategy/manual_commit_opf_rewrite.go).
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			if gitHooksDisabled {
				return nil
			}

			remote := args[0]

			g := newGitHookContext(cmd.Context(), "pre-push")
			defer g.span.End()
			g.logInvoked(slog.String("remote", remote))

			hookErr := g.strategy.PrePushFromGitHook(g.ctx, remote)
			g.logCompleted(hookErr)

			// Propagate the error so the hook script exits non-zero and
			// git push aborts the entire batch. PrePush itself only
			// returns errors for privacy-critical failures (OPF rewrite —
			// e.g., V1DivergedError, BootstrapTooLargeError,
			// V1RefMovedError, OPFRuntimeFailedError,
			// OPFNoCategoriesError); transient
			// checkpoint-push failures are logged and swallowed before
			// reaching this point. See strategy/manual_commit_push.go
			// for the contract. We wrap with a short "pre-push:" prefix
			// so the user sees the source of the abort without losing
			// the underlying type (errors.As still finds the sentinels).
			if hookErr == nil {
				return nil
			}
			return fmt.Errorf("pre-push: %w", hookErr)
		},
	}
}

// warnOnEmptyIndexCommit tells the user, before the commit object exists, that
// the commit git is preparing records no files while those files are still on
// disk — the signature of git having read `.git/index` as missing and therefore
// empty (issue #2111).
//
// Read-only and silent by default: the detector reads the index header first
// and opens the repository only if it says zero entries, so a normal commit
// pays one open(2) and a 16-byte read on top of a worktree-root resolution the
// rest of this hook process performs anyway (paths.WorktreeRoot caches per
// working directory). Measured at +0.1ms per invocation. It cannot fail the
// hook — every unresolvable step returns without a word, because a guard that
// guesses is worse than no guard.
func warnOnEmptyIndexCommit(ctx context.Context, out io.Writer) {
	worktreeRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return
	}
	// GIT_INDEX_FILE is the index git is committing from. Git exports it to
	// pre-commit, prepare-commit-msg and commit-msg, and by the time this hook
	// runs it is authoritative: git re-reads the index after pre-commit and
	// then writes the commit's tree from that in-core copy, so nothing after
	// this point can change what lands.
	hazard := gitrepo.DetectEmptyIndexHazard(ctx, worktreeRoot, os.Getenv("GIT_INDEX_FILE"))
	if hazard == nil {
		return
	}
	hazard.Report(ctx, out)
}

// warnOnEmptyTreeCommit reports whether the commit at HEAD records no files at
// all while the files it removes are still in the worktree — the aftermath of
// the same condition warnOnEmptyIndexCommit catches before the fact (#2111) —
// warning the user when it does.
//
// A true return stops the whole post-commit hook. That is the point: condensing
// against such a commit deletes shadow branches and marks the session condensed
// on the strength of a commit the user is about to undo with `git reset --mixed
// HEAD~1`, and advances BaseCommit onto a commit that is about to stop
// existing. Skipping costs nothing on the other reading of the same evidence —
// someone deliberately untracking every file gets condensed on their next
// commit instead.
//
// Two hooks warn about one commit, so the messages differ: the pre-commit one
// carries the explanation, this one the receipt.
func warnOnEmptyTreeCommit(ctx context.Context, out io.Writer) bool {
	worktreeRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return false
	}
	repo, err := gitrepo.OpenCurrent(ctx)
	if err != nil {
		return false
	}
	defer repo.Close()

	head, err := repo.Head()
	if err != nil {
		return false
	}
	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		return false
	}

	hazard := gitrepo.DetectEmptyTreeCommitHazard(ctx, worktreeRoot, commit)
	if hazard == nil {
		return false
	}
	hazard.Report(ctx, out)
	return true
}
