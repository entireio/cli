// Package review — see env.go for package-level rationale.
//
// cmd.go provides NewCommand(), the cobra entry point for `entire review`.
// It routes through the new AgentReviewer / Sink / Run architecture for
// launchable agents (claude-code, codex, gemini) and falls back to
// RunMarkerFallback for non-launchable agents (cursor, opencode,
// factoryai-droid, copilot-cli).
package review

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/external"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/gitexec"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// ContextResult bundles the composed checkpoint/session context with
// the counts of checkpoints and in-progress sessions reflected in it.
// Counts power the transparency banner; Prompt is what flows into the
// agent's composed review prompt.
type ContextResult struct {
	// Prompt is the composed context text injected into the agent prompt.
	// Empty when no checkpoints or sessions contributed.
	Prompt string
	// Checkpoints is the number of unique committed checkpoints rendered in
	// the composed prompt (capped at the renderer's internal limit; the
	// truncation tail is not counted).
	Checkpoints int
	// Sessions is the number of in-progress sessions rendered in the
	// composed prompt.
	Sessions int
	// CheckpointItems lists the committed checkpoints in scope (short id +
	// one-line summary) for the human-facing scope banner. Len matches
	// Checkpoints.
	CheckpointItems []CheckpointScopeItem
	// SessionItems lists the in-progress sessions in scope (agent + latest
	// prompt) for the scope banner. Len matches Sessions.
	SessionItems []SessionScopeItem
}

// CheckpointScopeItem is one committed checkpoint shown in the scope banner.
type CheckpointScopeItem struct {
	ID      string // short checkpoint id (first 8 hex chars)
	Summary string // one-line commit subject / checkpoint summary
}

// SessionScopeItem is one in-progress session shown in the scope banner.
type SessionScopeItem struct {
	ID    string // short session id (first 8 chars) — stable, unlike a long prompt
	Agent string // display name of the agent that owns the session
}

// Deps collects the runtime-injectable hooks NewCommand needs from the
// parent cli package. Tests stub fields to drive branches that would
// otherwise require a real TTY or enabled repo. Production wiring is
// provided by buildReviewDeps in cmd/entire/cli/review_bridge.go and
// passed to NewCommand from root.go.
type Deps struct {
	// GetAgentsWithHooksInstalled returns the registry names of all agents
	// whose lifecycle hooks are installed in the current repo.
	GetAgentsWithHooksInstalled func(ctx context.Context) []types.AgentName

	// NewSilentError wraps an error so the cobra root does not double-print it.
	NewSilentError func(err error) error

	// HeadHasReviewCheckpoint checks whether HEAD's checkpoint metadata
	// includes a review session. Returns (true, infoString) if HasReview is set.
	// Injected to avoid an import cycle: review → checkpoint → codex → review.
	HeadHasReviewCheckpoint func(ctx context.Context) (bool, string)

	// ReviewCheckpointContext returns best-effort checkpoint context for the
	// branch review scope along with the counts of checkpoints and in-progress
	// sessions reflected in the composed prompt. Counts power the transparency
	// banner. Injected from the cli package because checkpoint readers cannot
	// be imported here without cycling through agent reviewers.
	ReviewCheckpointContext func(ctx context.Context, worktreeRoot string, scopeBaseRef string) ContextResult

	// ReviewerFor maps an agent registry name to its AgentReviewer
	// implementation. Returns nil for non-launchable agents (cursor, opencode,
	// factoryai-droid, copilot-cli). Injected to break the import cycle:
	// per-agent reviewer packages import review (for ComposeReviewPrompt /
	// AppendReviewEnv), so review/cmd.go cannot import them back.
	ReviewerFor func(agentName string) reviewtypes.AgentReviewer

	// AttachCmd, when non-nil, is registered as the `review attach`
	// subcommand. Callers in the cli package pass newReviewAttachCmd() here;
	// tests pass nil to skip the subcommand.
	AttachCmd *cobra.Command

	// SynthesisProvider, when non-nil, enables the synthesis sink in TTY mode.
	// Production wiring resolves the same provider entire explain uses.
	// When nil, the synthesis sink is not appended and synthesis is unavailable.
	SynthesisProvider SynthesisProvider

	// PromptYN overrides the y/N confirmation form used by SynthesisSink.
	// Nil means the real huh form is used (realPromptYN in synthesis_sink.go).
	// Tests inject a stub to avoid TTY interactions.
	PromptYN func(ctx context.Context, question string, def bool) (bool, error)
}

// NewCommand returns the `entire review` cobra command wired with the
// provided deps. Callers in the cli package pass a fully-populated Deps;
// tests pass a Deps with stub fields.
func NewCommand(deps Deps) *cobra.Command {
	var edit bool
	var agentOverride string
	var baseOverride string
	var findings bool
	var fix bool
	var all bool
	var perRunPrompt string
	var reviewersFlag []string
	var fixerFlag string

	cmd := &cobra.Command{
		Use: "review",
		// Hidden from `entire help` while the feature is still maturing —
		// users who know about it can still run `entire review` / `entire
		// review --help` and the command works normally.
		Hidden: true,
		Short:  "Run configured review skills against the current branch",
		Long: `Run configured review skills against the current branch. Review
preferences are loaded from Entire settings.

Labs entry: review is experimental. We are actively refining it based on user
feedback.

The review session is recorded as part of the next checkpoint, so the
review metadata is permanently attached to the commit it covers.

Flags:
  --edit             re-open the review config picker (alias for setup)
  --findings         browse local review findings
  --fix              DEPRECATED: use 'entire review fix' instead
  --all              with --fix, apply all sources/findings without selectors
  --agent NAME       select a specific configured agent (default: alphabetically first)
  --base REF         scope the review against REF instead of mainline.
  --prompt TEXT      per-run extra context (bypasses the inline ask)
  --reviewers LIST   one-off override: agents to use as reviewers (comma-separated)
  --fixer NAME       one-off override: agent to use as fixer

Subcommands:
  setup              configure reviewers, fixer, and per-agent skills
  fix [session-id]   apply review findings via the configured Fixer
  attach <id>        tag an existing session as a review`,
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 1 {
				return fmt.Errorf("accepts at most one review session id, received %d", len(args))
			}
			if len(args) == 1 && !fix {
				return errors.New("review session id is only valid with --fix")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			// Discover external agents so review configs that target them
			// resolve correctly — without this, GetAgentsWithHooksInstalled
			// and agent.Get can't see them.
			external.DiscoverAndRegister(ctx)

			if all && !fix {
				return errors.New("--all requires --fix")
			}
			modes := 0
			for _, enabled := range []bool{edit, findings, fix} {
				if enabled {
					modes++
				}
			}
			if modes > 1 {
				return errors.New("--edit, --findings, and --fix are mutually exclusive")
			}
			if findings {
				return runReviewFindings(ctx, cmd, deps.NewSilentError)
			}
			if fix {
				fmt.Fprintln(cmd.ErrOrStderr(),
					"Hint: `entire review --fix` is deprecated; use `entire review fix` instead.")
				target := ""
				if len(args) == 1 {
					target = args[0]
				}
				return runReviewFix(ctx, cmd, target, all, agentOverride, perRunPrompt, deps.NewSilentError)
			}
			// The migration prompt is only relevant for flows that write or
			// read picker config.
			if err := maybePromptReviewSettingsMigration(
				ctx,
				cmd.OutOrStdout(),
				cmd.ErrOrStderr(),
				interactive.IsTerminalWriter(cmd.OutOrStdout()) && interactive.CanPromptInteractively(),
				deps.PromptYN,
			); err != nil {
				return err
			}
			if edit {
				external.DiscoverAndRegister(ctx)
				_, err := RunSetup(ctx, cmd.OutOrStdout(),
					deps.GetAgentsWithHooksInstalled, SetupForms{})
				return err
			}
			return runReview(ctx, cmd, agentOverride, baseOverride, perRunPrompt, reviewersFlag, fixerFlag, deps)
		},
	}
	cmd.Flags().BoolVar(&edit, "edit", false, "re-open the review config picker (alias for `entire review setup`)")
	cmd.Flags().BoolVar(&findings, "findings", false, "browse local review findings")
	cmd.Flags().BoolVar(&fix, "fix", false, "DEPRECATED: use `entire review fix` instead")
	cmd.Flags().BoolVar(&all, "all", false, "with --fix, apply all sources/findings without selectors")
	cmd.Flags().StringVar(&agentOverride, "agent", "", "select a specific configured agent (default: alphabetically first)")
	cmd.Flags().StringVar(&baseOverride, "base", "", "git ref to scope the review against (default: origin/HEAD → origin/main → origin/master → main → master)")
	cmd.Flags().StringVar(&perRunPrompt, "prompt", "", "per-run extra context appended to reviewer instructions; bypasses the inline ask")
	cmd.Flags().StringSliceVar(&reviewersFlag, "reviewers", nil, "one-off override: agents to use as reviewers (comma-separated or repeatable; e.g. --reviewers claude-code,codex)")
	cmd.Flags().StringVar(&fixerFlag, "fixer", "", "one-off override: agent to use as fixer (e.g. --fixer codex)")
	// Hide the deprecated --fix flag from help but keep it functional for one
	// release; the RunE prints a hint redirecting users to `entire review fix`.
	if err := cmd.Flags().MarkHidden("fix"); err != nil {
		// Should not happen for a flag we just declared.
		logging.Debug(context.Background(), "mark --fix hidden failed", slog.String("error", err.Error()))
	}
	if deps.AttachCmd != nil {
		cmd.AddCommand(deps.AttachCmd)
	}
	cmd.AddCommand(newReviewSetupCmd(deps))
	cmd.AddCommand(newReviewFixCmd(deps))
	return cmd
}

// runReview executes the main review flow. perRunPrompt is the value of
// the --prompt flag (empty when not set; the inline ask runs when empty
// and stdin is promptable). reviewersFlag / fixerFlag carry the one-off
// role overrides; when either is non-empty, saved config is replaced
// for this invocation only and is NOT persisted.
func runReview(ctx context.Context, cmd *cobra.Command, agentOverride, baseOverride, perRunPrompt string, reviewersFlag []string, fixerFlag string, deps Deps) error {
	out := cmd.OutOrStdout()
	silentErr := deps.NewSilentError

	// 0. Recursion guard. Prevents fan-out loops when a reviewer agent's
	// prompt accidentally tries to re-invoke `entire review`.
	if os.Getenv(EnvSession) != "" {
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), "Already in a review session — refusing to start a nested review.")
		fmt.Fprintln(cmd.ErrOrStderr(), "If you reached this from a reviewer agent's prompt, this is likely a loop and you should exit the inner agent.")
		return silentErr(errors.New("nested review session refused"))
	}

	// 1. Pre-flight: must be in a git repo.
	if _, err := paths.WorktreeRoot(ctx); err != nil {
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), "Not a git repository. Run `entire enable` first.")
		return silentErr(errors.New("not a git repository"))
	}

	// 2. Load config. A load error means the settings file exists but is
	// malformed (Load returns a default-filled object when the file is
	// missing). Surface the error instead of silently overwriting.
	s, err := settings.Load(ctx)
	if err != nil {
		cmd.SilenceUsage = true
		fmt.Fprintf(cmd.ErrOrStderr(), "Failed to load settings: %v\n", err)
		fmt.Fprintln(cmd.ErrOrStderr(),
			"Fix your Entire settings and re-run `entire review`.")
		return silentErr(err)
	}

	installed := deps.GetAgentsWithHooksInstalled(ctx)

	// 3. Flag-based one-off overrides take precedence over saved config.
	override, overrideErr := resolveRolesFromFlags(reviewersFlag, fixerFlag, installed)
	if overrideErr != nil {
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), overrideErr.Error())
		return silentErr(overrideErr)
	}
	if override != nil {
		// `entire review` needs at least one Reviewer (or Both). Flag-based
		// override that only sets a Fixer is rejected here, not in
		// resolveRolesFromFlags, because `entire review fix` legitimately
		// resolves a fixer-only override (no reviewer needed).
		hasReviewer := false
		for _, cfg := range override {
			if cfg.Role.IsReviewer() {
				hasReviewer = true
				break
			}
		}
		if !hasReviewer {
			cmd.SilenceUsage = true
			msg := errors.New(
				"--fixer alone defines a fixer with no reviewer; entire review needs at least one " +
					"(pass --reviewers <agents> to fix this, or run `entire review fix` for fix-only operations)",
			)
			fmt.Fprintln(cmd.ErrOrStderr(), msg.Error())
			return silentErr(msg)
		}
		if s == nil {
			s = &settings.EntireSettings{}
		}
		s.Review = mergeFlagOverrideWithSavedSkills(ctx, override, s.Review)
	}

	// userExplicitlyOmittedFixer = `--reviewers` passed but `--fixer` empty.
	// Drives the post-review footer (offers `--fixer <agent>` hint instead
	// of the setup nag when no Fixer is configured).
	userExplicitlyOmittedFixer := len(reviewersFlag) > 0 && strings.TrimSpace(fixerFlag) == ""

	// 4. Replace the legacy auto-picker with the invoker-aware fallback.
	if s == nil || len(s.Review) == 0 {
		if interactive.CanPromptInteractively() {
			cmd.SilenceUsage = true
			fmt.Fprintln(cmd.ErrOrStderr(), "Not configured yet.")
			fmt.Fprintln(cmd.ErrOrStderr())
			fmt.Fprintln(cmd.ErrOrStderr(), "Run: entire review setup")
			return silentErr(errors.New("review not configured"))
		}
		// Non-interactive: try invoker-only fallback.
		invoker := DetectInvokingAgent()
		if invoker == "" {
			cmd.SilenceUsage = true
			fmt.Fprintln(cmd.ErrOrStderr(),
				"Cannot run review: no config, no --reviewers/--fixer flags, and no invoking agent detected (CI / piped).")
			fmt.Fprintln(cmd.ErrOrStderr(), "Run: entire review setup (from an interactive shell)")
			return silentErr(errors.New("review not configured and no invoker"))
		}
		cfg, bErr := invokerOnlyReviewConfig(ctx, invoker, installed)
		if bErr != nil {
			cmd.SilenceUsage = true
			fmt.Fprintln(cmd.ErrOrStderr(), "Cannot run review:", bErr)
			fmt.Fprintln(cmd.ErrOrStderr(), "Hint: from an interactive shell, run `entire review setup`.")
			return silentErr(bErr)
		}
		if s == nil {
			s = &settings.EntireSettings{}
		}
		s.Review = cfg
		// Intentionally do NOT persist — this is a one-off fallback like
		// --reviewers/--fixer. Persisting would create hidden state: a user
		// who runs first from Claude Code, later from Codex, would silently
		// get the saved Claude-Both config instead of switching to Codex-Both.
		fmt.Fprintln(out, formatInvokerOnlyNote(invoker))
	}

	// 5. Resolve reviewers from roles. Roles answer "who reviews" up front;
	// the spawn-time multi-agent picker is gone.
	reviewers := ReviewersOf(s)
	if len(reviewers) == 0 {
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), "No agents are configured as Reviewers.")
		fmt.Fprintln(cmd.ErrOrStderr(), "Run: entire review setup")
		return silentErr(errors.New("no reviewers configured"))
	}

	// 6. The optional inline per-run prompt ask happens inside the dispatch
	// paths, AFTER the scope + checkpoint/session banner is printed — so the
	// user sees everything that's about to be reviewed (the staging summary)
	// before deciding what extra context to add, and the banner isn't
	// immediately obscured by the live TUI.

	// 7. Dispatch:
	//   - With --agent override: single-agent path against the named agent.
	//   - Otherwise: all configured reviewers (1 = single, 2+ = multi).
	if agentOverride != "" {
		cfg, ok := s.Review[agentOverride]
		if !ok || cfg.IsZero() {
			err := fmt.Errorf("agent %q is not configured as a reviewer", agentOverride)
			cmd.SilenceUsage = true
			fmt.Fprintln(cmd.ErrOrStderr(), err.Error())
			return silentErr(err)
		}
		return runSingleAgentPath(ctx, cmd, agentOverride, baseOverride, perRunPrompt, cfg, installed, deps, out, s, userExplicitlyOmittedFixer)
	}

	if len(reviewers) == 1 {
		name := reviewers[0]
		cfg := s.Review[name]
		return runSingleAgentPath(ctx, cmd, name, baseOverride, perRunPrompt, cfg, installed, deps, out, s, userExplicitlyOmittedFixer)
	}

	choices := agentChoicesFrom(reviewers, s.Review)
	return runMultiAgentPath(ctx, cmd, choices, baseOverride, perRunPrompt, s, deps, out, userExplicitlyOmittedFixer)
}

// agentChoicesFrom converts a sorted slice of agent names into the
// AgentChoice slice consumed by runMultiAgentPath.
func agentChoicesFrom(names []string, m map[string]settings.ReviewConfig) []AgentChoice {
	out := make([]AgentChoice, len(names))
	for i, n := range names {
		out[i] = AgentChoice{Name: n, Label: labelForAgentChoice(n, m[n])}
	}
	return out
}

// runSingleAgentPath completes a single-agent review: verifies hooks + skills,
// guards against re-review, resolves scope, then dispatches via Run or
// RunMarkerFallback.
func runSingleAgentPath(
	ctx context.Context,
	cmd *cobra.Command,
	agentName, baseOverride, perRunPrompt string,
	cfg settings.ReviewConfig,
	installed []types.AgentName,
	deps Deps,
	out io.Writer,
	s *settings.EntireSettings,
	userExplicitlyOmittedFixer bool,
) error {
	silentErr := deps.NewSilentError

	// 3.5. Verify hooks are installed for the selected agent.
	found := false
	for _, n := range installed {
		if string(n) == agentName {
			found = true
			break
		}
	}
	if !found {
		cmd.SilenceUsage = true
		fmt.Fprintf(cmd.ErrOrStderr(),
			"Hooks are not installed for %q. Run `entire configure --agent %s` first, "+
				"or remove %q from review settings.\n",
			agentName, agentName, agentName)
		return silentErr(fmt.Errorf("hooks not installed for %s", agentName))
	}

	// 3.6. Verify configured skills are actually installed on disk.
	ag, agErr := agent.Get(types.AgentName(agentName))
	if agErr != nil {
		return fmt.Errorf("resolve agent %s: %w", agentName, agErr)
	}
	if err := VerifyConfiguredSkillsInstalled(ctx, ag, cfg); err != nil {
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), err.Error())
		return silentErr(err)
	}

	// 4. Re-run guard: check if HEAD's checkpoint already has a review.
	if reviewed, meta := deps.HeadHasReviewCheckpoint(ctx); reviewed {
		var proceed bool
		form := newAccessibleForm(huh.NewGroup(
			huh.NewConfirm().
				Title(fmt.Sprintf("Already reviewed: %s. Proceed anyway?", meta)).
				Value(&proceed),
		))
		if err := form.RunWithContext(ctx); err != nil {
			fmt.Fprintln(out, "prompt cancelled")
			return err //nolint:wrapcheck // propagate huh cancellation
		}
		if !proceed {
			fmt.Fprintln(out, "Review cancelled.")
			return nil
		}
	}

	// 5. Resolve HEAD SHA and worktree root.
	worktreeRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		cmd.SilenceUsage = true
		return fmt.Errorf("resolve worktree root: %w", err)
	}

	// 6. Resolve HEAD SHA and detect scope.
	headSHA, shaErr := currentHeadSHA(ctx, worktreeRoot)
	if shaErr != nil {
		cmd.SilenceUsage = true
		return fmt.Errorf("resolve HEAD: %w", shaErr)
	}
	scopeBaseRef, scopeBanner, scopeErr := detectScope(ctx, worktreeRoot, baseOverride)
	if scopeErr != nil {
		cmd.SilenceUsage = true
		return scopeErr
	}
	var ctxResult ContextResult
	if deps.ReviewCheckpointContext != nil {
		ctxResult = deps.ReviewCheckpointContext(ctx, worktreeRoot, scopeBaseRef)
	}
	// Staging step: present the scope + checkpoints/sessions and collect the
	// optional per-run prompt in one styled view, before fan-out.
	perRunPrompt = stagePerRunContext(ctx, out, scopeBanner, ctxResult, perRunPrompt)

	runCfg := reviewtypes.RunConfig{
		PerRunPrompt:      perRunPrompt,
		ScopeBaseRef:      scopeBaseRef,
		CheckpointContext: ctxResult.Prompt,
		StartingSHA:       headSHA,
	}
	applyReviewConfig(&runCfg, cfg)

	// 7. Branch on launchability.
	reviewer := deps.ReviewerFor(agentName)
	if reviewer == nil {
		// Non-launchable: write marker (with scope-aware prompt) and print guidance.
		return RunMarkerFallback(ctx, agentName, runCfg, worktreeRoot, out)
	}

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	runCfg.EnrichSummary = reviewSummaryTokenEnricher(worktreeRoot, headSHA)
	canPrompt := interactive.CanPromptInteractively()
	sinks := composeSingleAgentSinks(singleAgentSinkInputs{
		out:       out,
		isTTY:     interactive.IsTerminalWriter(out) && canPrompt,
		canPrompt: canPrompt,
		agentName: agentName,
		cancelRun: cancelRun,
	})
	if tuiSink, ok := findTUISink(sinks); ok {
		tuiSink.Start()
		defer tuiSink.Wait()
	}

	summary, waitErr := Run(runCtx, reviewer, runCfg, sinks)
	manifest := writePostReviewManifestAndReturn(ctx, out, worktreeRoot, headSHA, summary, "")
	if waitErr != nil && runCtx.Err() == nil && ctx.Err() == nil {
		// Non-cancellation error: surface to caller.
		return fmt.Errorf("review run: %w", waitErr)
	}
	if manifest != nil {
		if err := RunPostReviewFixPrompt(ctx, cmd, s, *manifest, perRunPrompt, silentErr, userExplicitlyOmittedFixer); err != nil {
			return err
		}
	}
	return nil
}

// detectScope computes the scope base ref for the current repo and prints
// a scope banner to out on success. baseOverride, when non-empty, comes from
// the `--base <ref>` flag and bypasses mainline auto-detection.
//
// Failure handling: when baseOverride is set and the ref is invalid,
// returns ("", err) so the caller can fail-loudly before spawning agents.
// Otherwise (auto-detection failed): returns "" and the caller proceeds in
// degraded mode without a scope banner.
// detectScope resolves the scope base ref and returns the human-facing scope
// banner string (e.g. "Reviewing feat/X vs main: 3 commits, …") for the caller
// to render — printing is left to the staging step so the banner can be folded
// into the styled per-run prompt. Returns ("", "", nil) in degraded mode (no
// override, auto-detection failed); a bad explicit --base aborts loudly.
func detectScope(ctx context.Context, worktreeRoot, baseOverride string) (baseRef, banner string, err error) {
	repo, openErr := gitrepo.OpenPath(worktreeRoot)
	if openErr != nil {
		logging.Debug(ctx, "review repo open failed", slog.String("error", openErr.Error()))
		if baseOverride != "" {
			return "", "", fmt.Errorf("--base %q given but cannot open repository at %q: %w", baseOverride, worktreeRoot, openErr)
		}
		return "", "", nil
	}
	defer repo.Close()
	stats, statsErr := ComputeScopeStats(ctx, repo, baseOverride)
	if statsErr != nil {
		// With an override, the user explicitly asked for a specific base.
		// A bad ref must abort before agents spawn so the user learns about
		// the typo immediately, not after a long review run.
		if baseOverride != "" {
			return "", "", statsErr
		}
		logging.Debug(ctx, "review scope detection failed", slog.String("error", statsErr.Error()))
		return "", "", nil
	}
	return stats.BaseRef, formatScopeBanner(stats), nil
}

// runMultiAgentPath handles the multi-agent review flow: builds per-agent
// RunConfigs and runs all selected agents concurrently via RunMulti.
//
// Roles answer "who reviews" up front (via `entire review setup` or the
// --reviewers flag), so this path no longer presents a spawn-time
// multi-select picker.
//
// This path skips the single-agent validation steps (3.5 hooks, 3.6 skills,
// re-run guard) for brevity — the caller has already ensured each agent in
// `reviewers` has been chosen via the role model.
func runMultiAgentPath(
	ctx context.Context,
	cmd *cobra.Command,
	choices []AgentChoice,
	baseOverride, perRunPrompt string,
	s *settings.EntireSettings,
	deps Deps,
	out io.Writer,
	userExplicitlyOmittedFixer bool,
) error {
	silentErr := deps.NewSilentError

	// Resolve worktree root and HEAD SHA for scope detection.
	worktreeRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		cmd.SilenceUsage = true
		return fmt.Errorf("resolve worktree root: %w", err)
	}
	headSHA, shaErr := currentHeadSHA(ctx, worktreeRoot)
	if shaErr != nil {
		cmd.SilenceUsage = true
		return fmt.Errorf("resolve HEAD: %w", shaErr)
	}

	scopeBaseRef, scopeBanner, scopeErr := detectScope(ctx, worktreeRoot, baseOverride)
	if scopeErr != nil {
		cmd.SilenceUsage = true
		return scopeErr
	}
	var ctxResult ContextResult
	if deps.ReviewCheckpointContext != nil {
		ctxResult = deps.ReviewCheckpointContext(ctx, worktreeRoot, scopeBaseRef)
	}
	// Staging step: present the scope + checkpoints/sessions and collect the
	// optional per-run prompt in one styled view, before fan-out.
	perRunPrompt = stagePerRunContext(ctx, out, scopeBanner, ctxResult, perRunPrompt)

	// Build per-agent reviewers with individual RunConfigs (each agent has
	// its own skills + always-prompt from s.Review[name]).
	reviewers := make([]reviewtypes.AgentReviewer, 0, len(choices))
	for _, choice := range choices {
		name := choice.Name
		agentCfg := s.Review[name] // zero value is safe (empty skills/prompt)
		reviewer := deps.ReviewerFor(name)
		if reviewer == nil {
			// Skip non-launchable agents — roles have already decided
			// the set, but a non-launchable reviewer just falls through
			// to marker fallback and the multi-agent dispatch path can't
			// represent that. Surface a clear error instead.
			cmd.SilenceUsage = true
			return silentErr(fmt.Errorf("agent %q is configured as a reviewer but is not launchable", name))
		}
		// Wrap the reviewer so it sees the per-agent RunConfig at Start time.
		// We cannot pass a different RunConfig per reviewer in RunMulti's
		// current API (all reviewers share one RunConfig). Instead, build a
		// configuredReviewer adapter that injects per-agent skills into
		// RunConfig before forwarding to the underlying reviewer.
		reviewers = append(reviewers, &perAgentConfiguredReviewer{
			inner: reviewer,
			cfg: runConfigWithReviewConfig(reviewtypes.RunConfig{
				PerRunPrompt:      perRunPrompt,
				ScopeBaseRef:      scopeBaseRef,
				CheckpointContext: ctxResult.Prompt,
				StartingSHA:       headSHA,
			}, agentCfg),
		})
	}

	// Compose sinks based on TTY detection.
	// TTY mode: [TUISink, DumpSink] — TUI owns the live dashboard; DumpSink
	// renders the post-run narrative after TUI dismisses (RunFinished is called
	// on each sink in order, and TUISink.RunFinished blocks until user dismisses).
	// Non-TTY mode: [DumpSink] alone.
	//
	// A derived context is used so the TUI's Ctrl+C handler can cancel the run
	// via the same cancelRun function that the orchestrator's context is built on.
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	agentNames := make([]string, len(reviewers))
	for i, r := range reviewers {
		agentNames[i] = r.Name()
	}
	aggregateOutput := ""

	// TUI requires both:
	//   - terminal stdout (otherwise ANSI codes corrupt redirected output)
	//   - a promptable stdin (otherwise the post-run dismissal loop blocks
	//     forever — happens when entire review is invoked from inside an
	//     agent like Claude Code or Gemini CLI, where stdout is a TTY but
	//     keypresses are never delivered)
	sinks := composeMultiAgentSinks(multiAgentSinkInputs{
		out:               out,
		isTTY:             interactive.IsTerminalWriter(out) && interactive.CanPromptInteractively(),
		canPrompt:         interactive.CanPromptInteractively(),
		agentNames:        agentNames,
		cancelRun:         cancelRun,
		runContext:        runCtx,
		synthesisProvider: deps.SynthesisProvider,
		promptYN:          deps.PromptYN,
		perRunPrompt:      perRunPrompt,
		onSynthesisResult: func(result string) {
			aggregateOutput = result
		},
	})
	if tuiSink, ok := findTUISink(sinks); ok {
		tuiSink.Start()
		defer tuiSink.Wait()
	}

	// Multi-agent only wires EnrichAgentRun. The per-agent enricher emits a
	// synthetic Tokens event as each agent finishes, which the dispatch loop
	// overwrites onto st.tokens (run_multi.go:168). That value flows into
	// agentRuns[i].Tokens in the final summary, so a summary-level pass would
	// redo the same store.List + token hydration once per run.
	summary, waitErr := RunMulti(runCtx, reviewers, reviewtypes.RunConfig{
		EnrichAgentRun: reviewAgentRunTokenEnricher(worktreeRoot, headSHA),
	}, sinks)
	manifest := writePostReviewManifestAndReturn(ctx, out, worktreeRoot, headSHA, summary, aggregateOutput)
	if waitErr != nil && runCtx.Err() == nil && ctx.Err() == nil {
		return fmt.Errorf("review run: %w", waitErr)
	}
	if manifest != nil {
		if err := RunPostReviewFixPrompt(ctx, cmd, s, *manifest, perRunPrompt, silentErr, userExplicitlyOmittedFixer); err != nil {
			return err
		}
	}
	return nil
}

// multiAgentSinkInputs collects the parameters composeMultiAgentSinks needs.
// It exists so tests can drive the helper with explicit isTTY / canPrompt
// values instead of monkey-patching interactive helpers at run time.
//
// isTTY here means "the TUI sink is safe to compose" — production callers
// AND IsTerminalWriter(out) with CanPromptInteractively() before passing
// it in, since the TUI both writes ANSI to stdout AND reads keypresses
// from stdin. A terminal-stdout-but-non-interactive-stdin scenario (an
// agent host like Claude Code invoking `entire review`) must NOT use the
// TUI — its dismissal loop would block forever.
type multiAgentSinkInputs struct {
	out               io.Writer
	isTTY             bool
	canPrompt         bool
	agentNames        []string
	cancelRun         context.CancelFunc
	runContext        context.Context
	synthesisProvider SynthesisProvider
	promptYN          func(ctx context.Context, question string, def bool) (bool, error)
	perRunPrompt      string
	onSynthesisResult func(result string)
}

type singleAgentSinkInputs struct {
	out       io.Writer
	isTTY     bool
	canPrompt bool
	agentName string
	cancelRun context.CancelFunc
}

// composeMultiAgentSinks builds the sink slice for a multi-agent run.
//
//   - Non-TTY: [DumpSink] alone — narrative dump only, no live UI, no prompts.
//   - TTY: [TUISink, DumpSink, SynthesisSink?] — TUI owns the live dashboard;
//     DumpSink renders the post-run narrative; SynthesisSink (if a provider is
//     configured AND stdin can prompt) appends the y/N synthesis offer.
//
// The synthesis sink is only appended when canPrompt is true: without a
// promptable stdin, the y/N form would never resolve. SynthesisSink also
// guards on InputTTY internally (defense in depth) but suppressing it here
// avoids constructing a sink that will silently no-op.
func composeMultiAgentSinks(in multiAgentSinkInputs) []reviewtypes.Sink {
	if !in.isTTY {
		return []reviewtypes.Sink{DumpSink{W: in.out}}
	}
	sinks := []reviewtypes.Sink{
		NewTUISink(in.agentNames, in.cancelRun, in.out, os.Stdin),
		DumpSink{W: in.out},
	}
	if in.synthesisProvider != nil && in.canPrompt {
		sinks = append(sinks, SynthesisSink{
			Provider:     in.synthesisProvider,
			Writer:       in.out,
			InputTTY:     in.canPrompt,
			PromptYN:     in.promptYN,
			PerRunPrompt: in.perRunPrompt,
			RunContext:   in.runContext,
			OnResult:     in.onSynthesisResult,
		})
	}
	return sinks
}

// writePostReviewManifestAndReturn persists the post-review manifest to
// disk and returns it so the caller can thread it into the post-review
// fix prompt. Returns nil when the manifest was not written
// (cancellation, no findings, write failure, or no matching review
// session state).
func writePostReviewManifestAndReturn(
	ctx context.Context,
	out io.Writer,
	worktreeRoot string,
	headSHA string,
	summary reviewtypes.RunSummary,
	aggregateOutput string,
) *LocalReviewManifest {
	if summary.Cancelled || len(summary.AgentRuns) == 0 {
		return nil
	}
	manifest, states, err := localReviewManifestFromCurrentState(ctx, worktreeRoot, headSHA, summary, aggregateOutput)
	if err != nil {
		logging.Debug(ctx, "review manifest not written", slog.String("error", err.Error()))
		warnManifestNotWritten(out, "could not load session state: "+err.Error())
		return nil
	}
	if len(manifest.Sources) == 0 {
		reason, sentinel := explainEmptyManifest(worktreeRoot, headSHA, summary, states)
		if sentinel {
			logging.Warn(ctx, "review manifest matcher/explainer drift detected",
				slog.String("reason", reason),
				slog.Int("tagged_state_count", len(states)),
				slog.Int("agent_run_count", len(summary.AgentRuns)))
		} else {
			logging.Debug(ctx, "review manifest not written: no matching review sessions",
				slog.String("reason", reason))
		}
		warnManifestNotWritten(out, reason)
		return nil
	}
	if err := writeLocalReviewManifest(ctx, manifest); err != nil {
		logging.Debug(ctx, "review manifest write failed", slog.String("error", err.Error()))
		warnManifestNotWritten(out, "write to disk failed: "+err.Error())
		return nil
	}
	writeReviewCompletionFooter(out, manifest)
	return &manifest
}

// warnManifestNotWritten prints a user-visible note explaining that the
// review skills ran but findings were not persisted, so `entire review
// --findings` and `entire review --fix` will not see this run. The reason
// string is appended verbatim and should describe the underlying cause in
// terms the user can act on (or at least diagnose with debug logs).
func warnManifestNotWritten(out io.Writer, reason string) {
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Note: review skills ran but findings were not persisted.")
	fmt.Fprintf(out, "  Reason: %s\n", reason)
	fmt.Fprintln(out, "  `entire review --findings` and `entire review --fix` will not see this run.")
	fmt.Fprintln(out, "  Re-run with `ENTIRE_LOG_LEVEL=debug` for diagnostic detail.")
}

func reviewSummaryTokenEnricher(worktreeRoot, headSHA string) func(context.Context, reviewtypes.RunSummary) reviewtypes.RunSummary {
	return func(ctx context.Context, summary reviewtypes.RunSummary) reviewtypes.RunSummary {
		enriched, err := hydrateReviewSummaryTokensFromCurrentState(ctx, worktreeRoot, headSHA, summary, agent.GetByAgentType)
		if err != nil {
			logging.Debug(ctx, "review token hydration skipped", slog.String("error", err.Error()))
			return summary
		}
		return enriched
	}
}

func reviewAgentRunTokenEnricher(worktreeRoot, headSHA string) func(context.Context, reviewtypes.AgentRun) reviewtypes.AgentRun {
	return func(ctx context.Context, run reviewtypes.AgentRun) reviewtypes.AgentRun {
		enriched, err := hydrateReviewAgentRunTokensFromCurrentState(ctx, worktreeRoot, headSHA, run, agent.GetByAgentType)
		if err != nil {
			logging.Debug(ctx, "review agent token hydration skipped", slog.String("error", err.Error()))
			return run
		}
		return enriched
	}
}

func composeSingleAgentSinks(in singleAgentSinkInputs) []reviewtypes.Sink {
	if !in.isTTY || !in.canPrompt {
		fmt.Fprintf(in.out, "Running review with %s...\n", in.agentName)
		return []reviewtypes.Sink{DumpSink{W: in.out}}
	}
	return []reviewtypes.Sink{
		NewTUISink([]string{in.agentName}, in.cancelRun, in.out, os.Stdin),
		DumpSink{W: in.out},
	}
}

func runConfigWithReviewConfig(base reviewtypes.RunConfig, cfg settings.ReviewConfig) reviewtypes.RunConfig {
	applyReviewConfig(&base, cfg)
	return base
}

func applyReviewConfig(runCfg *reviewtypes.RunConfig, cfg settings.ReviewConfig) {
	// Per-spawn tuning applies regardless of the skills-vs-prompt branch
	// below; reviewers that don't support these knobs ignore them.
	runCfg.Model = cfg.Model
	runCfg.ReasoningEffort = cfg.ReasoningEffort
	runCfg.Skills = cfg.Skills
	if len(cfg.Skills) == 0 {
		runCfg.PromptOverride = cfg.Prompt
		return
	}
	runCfg.AlwaysPrompt = cfg.Prompt
}

// findTUISink returns the first *TUISink in the slice (if any). Used by the
// caller to wire Start/Wait around the run without re-running composition.
func findTUISink(sinks []reviewtypes.Sink) (*TUISink, bool) {
	for _, s := range sinks {
		if t, ok := s.(*TUISink); ok {
			return t, true
		}
	}
	return nil, false
}

// perAgentConfiguredReviewer is an AgentReviewer adapter that overrides the
// RunConfig passed to the underlying reviewer's Start method. This lets
// RunMulti pass a single shared RunConfig at the API boundary while each
// agent in a multi-agent run still sees its own skills and always-prompt.
type perAgentConfiguredReviewer struct {
	inner reviewtypes.AgentReviewer
	cfg   reviewtypes.RunConfig
}

func (r *perAgentConfiguredReviewer) Name() string { return r.inner.Name() }
func (r *perAgentConfiguredReviewer) Start(ctx context.Context, _ reviewtypes.RunConfig) (reviewtypes.Process, error) {
	return r.inner.Start(ctx, r.cfg) //nolint:wrapcheck // transparent adapter; callers see inner's error type directly
}

// Compile-time interface check.
var _ reviewtypes.AgentReviewer = (*perAgentConfiguredReviewer)(nil)

// currentHeadSHA returns the current HEAD commit hash as a 40-char hex string.
func currentHeadSHA(ctx context.Context, repoRoot string) (string, error) {
	return gitexec.HeadSHA(ctx, repoRoot) //nolint:wrapcheck // gitexec already wraps
}
