// Package review — see env.go for package-level rationale.
//
// cmd.go provides NewCommand(), the cobra entry point for `entire review`.
// It routes through the new AgentReviewer / Sink / Run architecture for
// launchable agents (claude-code, codex, gemini-cli) and falls back to
// RunMarkerFallback for non-launchable agents (cursor, opencode,
// factoryai-droid, copilot-cli).
package review

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"charm.land/huh/v2"
	git "github.com/go-git/go-git/v6"
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/external"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

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

	// PromptForAgentFn overrides the interactive agent picker. Nil means
	// PromptForAgent is used (the real huh form). Tests inject a stub.
	PromptForAgentFn func(ctx context.Context, eligible []AgentChoice) (string, error)

	// MultiPickerFn overrides PickAgents for the multi-agent picker. Nil
	// means PickAgents is used (the real huh form). Tests inject a stub.
	MultiPickerFn func(ctx context.Context, eligible []AgentChoice) (PickedAgents, error)

	// HeadHasReviewCheckpoint checks whether HEAD's checkpoint metadata
	// includes a review session. Returns (true, infoString) if HasReview is set.
	// Injected to avoid an import cycle: review → checkpoint → codex → review.
	HeadHasReviewCheckpoint func(ctx context.Context) (bool, string)

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

// runReviewDeps carries the subset of Deps that runReview itself reads
// directly (vs. NewCommand's wiring). Kept unexported so tests construct a
// Deps value at the package boundary; runReview unpacks the relevant fields.
type runReviewDeps struct {
	promptForAgentFn func(ctx context.Context, eligible []AgentChoice) (string, error)
	multiPickerFn    func(ctx context.Context, eligible []AgentChoice) (PickedAgents, error)
}

// NewCommand returns the `entire review` cobra command wired with the
// provided deps. Callers in the cli package pass a fully-populated Deps;
// tests pass a Deps with stub fields.
func NewCommand(deps Deps) *cobra.Command {
	var edit bool
	var agentOverride string

	cmd := &cobra.Command{
		Use: "review",
		// Hidden from `entire help` while the feature is still maturing —
		// users who know about it can still run `entire review` / `entire
		// review --help` and the command works normally.
		Hidden: true,
		Short:  "Run configured review skills against the current branch",
		Long: `Run the review skills configured in .entire/settings.json against
the current branch. On first run, an interactive picker writes the config.

Labs entry: review is experimental. We are actively refining it based on user
feedback.

The review session is recorded as part of the next checkpoint, so the
review metadata is permanently attached to the commit it covers.

Flags:
  --edit         re-open the review config picker
  --agent NAME   select a specific configured agent when more than one is
                 configured (default: alphabetically first)

Subcommands:
  attach <id>    tag an existing session as a review (equivalent to
                 'entire attach --review <id>')`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			// Discover external agents so review configs that target them
			// resolve correctly — without this, GetAgentsWithHooksInstalled
			// and agent.Get can't see them.
			external.DiscoverAndRegister(ctx)

			if edit {
				_, err := RunReviewConfigPicker(ctx, cmd.OutOrStdout(), deps.GetAgentsWithHooksInstalled)
				return err
			}
			innerDeps := runReviewDeps{
				promptForAgentFn: deps.PromptForAgentFn,
				multiPickerFn:    deps.MultiPickerFn,
			}
			return runReview(ctx, cmd, agentOverride, deps, innerDeps)
		},
	}
	cmd.Flags().BoolVar(&edit, "edit", false, "re-open the review config picker")
	cmd.Flags().StringVar(&agentOverride, "agent", "", "select a specific configured agent (default: alphabetically first)")
	if deps.AttachCmd != nil {
		cmd.AddCommand(deps.AttachCmd)
	}
	return cmd
}

// runReview executes the main review flow.
func runReview(ctx context.Context, cmd *cobra.Command, agentOverride string, deps Deps, innerDeps runReviewDeps) error {
	out := cmd.OutOrStdout()
	silentErr := deps.NewSilentError

	// 1. Pre-flight: must be in a git repo.
	if _, err := paths.WorktreeRoot(ctx); err != nil {
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), "Not a git repository. Run `entire enable` first.")
		return silentErr(errors.New("not a git repository"))
	}

	// 2. Load config. A load error means the settings file exists but is
	// malformed (Load returns a default-filled object when the file is
	// missing). Surface the error instead of silently opening the picker,
	// which would cause SaveReviewConfig to write over the user's other
	// settings with an empty EntireSettings{}.
	s, err := settings.Load(ctx)
	if err != nil {
		cmd.SilenceUsage = true
		fmt.Fprintf(cmd.ErrOrStderr(), "Failed to load settings: %v\n", err)
		fmt.Fprintln(cmd.ErrOrStderr(), "Fix `.entire/settings.json` and re-run `entire review`.")
		return silentErr(err)
	}
	if s == nil || len(s.Review) == 0 {
		if !ConfirmFirstRunSetup(ctx, out) {
			return nil
		}
		picked, pickErr := RunReviewConfigPicker(ctx, out, deps.GetAgentsWithHooksInstalled)
		if pickErr != nil {
			return pickErr
		}
		if s == nil {
			s = &settings.EntireSettings{}
		}
		s.Review = picked
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Setup complete — running review now.")
	}

	// 3. Resolve installed agents and determine the dispatch path.
	//
	// Three paths:
	//   - Multi-agent: 2+ launchable eligible agents AND no --agent override →
	//     show multi-select picker then RunMulti. Steps 3.5, 3.6, and the
	//     single-agent skill-verify guard are skipped; each reviewer pulls
	//     its own skills from settings at spawn time via RunConfig.
	//   - Single-agent (default): 1 or fewer launchable eligible agents, OR
	//     --agent override set. Falls through to the full agent-selection and
	//     validation path below (steps 3–3.6).
	installed := deps.GetAgentsWithHooksInstalled(ctx)
	if agentOverride == "" {
		launchableEligible := computeLaunchableEligible(s, installed, deps.ReviewerFor)
		if len(launchableEligible) >= 2 {
			return runMultiAgentPath(ctx, cmd, launchableEligible, s, innerDeps, deps, out)
		}
	}

	// Single-agent path: pick agent, verify hooks + skills, scope, run.

	// 3a. Base selection on the eligible set (configured AND installed):
	//   - 0 eligible: fall through; SelectReviewAgent below errors with the
	//     full configured map (clearer "no installed agent" diagnostic than
	//     a silent fail).
	//   - 1 eligible: use it directly. This matters when the alphabetically-
	//     first configured agent isn't installed but exactly one other is —
	//     without this, SelectReviewAgent would default to the alphabetical
	//     first and the verify-hooks check below would error needlessly.
	//   - 2+ eligible: prompt with single-select (non-launchable agents reach
	//     this branch since computeLaunchableEligible filtered them out above).
	if agentOverride == "" {
		eligible := ComputeEligibleConfigured(s, installed)
		switch {
		case len(eligible) == 1:
			agentOverride = eligible[0].Name
		case len(eligible) > 1:
			fn := innerDeps.promptForAgentFn
			if fn == nil {
				fn = PromptForAgent
			}
			picked, pickErr := fn(ctx, eligible)
			if pickErr != nil {
				cmd.SilenceUsage = true
				fmt.Fprintln(cmd.ErrOrStderr(), pickErr.Error())
				return silentErr(pickErr)
			}
			if picked == "" {
				// Defensive: empty picker return must not fall through to
				// alphabetical-first default.
				cmd.SilenceUsage = true
				emptyErr := errors.New("agent picker returned empty agent name")
				fmt.Fprintln(cmd.ErrOrStderr(), emptyErr.Error())
				return silentErr(emptyErr)
			}
			agentOverride = picked
		}
	}

	agentName, cfg, err := SelectReviewAgent(s.Review, agentOverride)
	if err != nil {
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), err.Error())
		return silentErr(err)
	}

	return runSingleAgentPath(ctx, cmd, agentName, cfg, installed, deps, out)
}

// runSingleAgentPath completes a single-agent review: verifies hooks + skills,
// guards against re-review, resolves scope, then dispatches via Run or
// RunMarkerFallback.
func runSingleAgentPath(
	ctx context.Context,
	cmd *cobra.Command,
	agentName string,
	cfg settings.ReviewConfig,
	installed []types.AgentName,
	deps Deps,
	out io.Writer,
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
		return fmt.Errorf("resolve worktree root: %w", err)
	}

	// 6. Resolve HEAD SHA and detect scope.
	headSHA, shaErr := currentHeadSHA(ctx, worktreeRoot)
	if shaErr != nil {
		return fmt.Errorf("resolve HEAD: %w", shaErr)
	}
	scopeBaseRef := detectScope(ctx, worktreeRoot, out)

	runCfg := reviewtypes.RunConfig{
		PromptOverride: cfg.Prompt,
		Skills:         cfg.Skills,
		ScopeBaseRef:   scopeBaseRef,
		StartingSHA:    headSHA,
	}

	// 7. Branch on launchability.
	reviewer := deps.ReviewerFor(agentName)
	if reviewer == nil {
		// Non-launchable: write marker (with scope-aware prompt) and print guidance.
		return RunMarkerFallback(ctx, agentName, runCfg, worktreeRoot, out)
	}

	_, waitErr := Run(ctx, reviewer, runCfg, []reviewtypes.Sink{DumpSink{W: out}})
	if waitErr != nil && ctx.Err() == nil {
		// Non-cancellation error: surface to caller.
		return fmt.Errorf("review run: %w", waitErr)
	}
	return nil
}

// detectScope computes the scope base ref for the current repo and prints a
// scope banner to out on success. Best-effort: on any failure, returns an
// empty string and prints no banner so the run proceeds in degraded mode.
func detectScope(ctx context.Context, worktreeRoot string, out io.Writer) (scopeBaseRef string) {
	if repo, openErr := git.PlainOpen(worktreeRoot); openErr == nil {
		if stats, statsErr := ComputeScopeStats(ctx, repo); statsErr == nil {
			fmt.Fprintln(out, formatScopeBanner(stats))
			return stats.BaseRef
		} else { //nolint:revive // else-after-return is clearer here for the error-path log
			logging.Debug(ctx, "review scope detection failed", slog.String("error", statsErr.Error()))
		}
	} else {
		logging.Debug(ctx, "review repo open failed", slog.String("error", openErr.Error()))
	}
	return ""
}

// runMultiAgentPath handles the multi-agent review flow: shows the multi-select
// picker, collects an optional per-run prompt, builds per-agent RunConfigs,
// then runs all selected agents concurrently via RunMulti.
//
// This path skips the single-agent validation steps (3.5 hooks, 3.6 skills,
// re-run guard) for brevity — computeLaunchableEligible has already ensured
// each eligible agent has hooks installed and a Reviewer available.
func runMultiAgentPath(
	ctx context.Context,
	cmd *cobra.Command,
	launchableEligible []AgentChoice,
	s *settings.EntireSettings,
	innerDeps runReviewDeps,
	deps Deps,
	out io.Writer,
) error {
	// Note: skill verification is intentionally skipped here. The
	// computeLaunchableEligible filter in the dispatch fork already
	// guarantees every agent in launchableEligible has hooks installed
	// AND a non-nil ReviewerFor mapping, so a per-agent verify pass would
	// be redundant.
	silentErr := deps.NewSilentError

	// Show multi-select picker (or use injected stub in tests).
	pickerFn := innerDeps.multiPickerFn
	if pickerFn == nil {
		pickerFn = PickAgents
	}
	picked, pickErr := pickerFn(ctx, launchableEligible)
	if pickErr != nil {
		return handlePickerError(cmd, silentErr, pickErr)
	}

	// Resolve worktree root and HEAD SHA for scope detection.
	worktreeRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return fmt.Errorf("resolve worktree root: %w", err)
	}
	headSHA, shaErr := currentHeadSHA(ctx, worktreeRoot)
	if shaErr != nil {
		return fmt.Errorf("resolve HEAD: %w", shaErr)
	}

	scopeBaseRef := detectScope(ctx, worktreeRoot, out)

	// Build per-agent reviewers with individual RunConfigs (each agent has
	// its own skills + always-prompt from s.Review[name]).
	reviewers := make([]reviewtypes.AgentReviewer, 0, len(picked.Names))
	for _, name := range picked.Names {
		agentCfg := s.Review[name] // zero value is safe (empty skills/prompt)
		reviewer := deps.ReviewerFor(name)
		if reviewer == nil {
			// Shouldn't happen given launchableEligible was filtered for
			// ReviewerFor != nil, but be defensive.
			cmd.SilenceUsage = true
			return silentErr(fmt.Errorf("agent %q is not launchable but appeared in eligible list", name))
		}
		// Wrap the reviewer so it sees the per-agent RunConfig at Start time.
		// We cannot pass a different RunConfig per reviewer in RunMulti's
		// current API (all reviewers share one RunConfig). Instead, build a
		// configuredReviewer adapter that injects per-agent skills into
		// RunConfig before forwarding to the underlying reviewer.
		reviewers = append(reviewers, &perAgentConfiguredReviewer{
			inner: reviewer,
			cfg: reviewtypes.RunConfig{
				PromptOverride: agentCfg.Prompt,
				Skills:         agentCfg.Skills,
				PerRunPrompt:   picked.PerRun,
				ScopeBaseRef:   scopeBaseRef,
				StartingSHA:    headSHA,
			},
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
		perRunPrompt:      picked.PerRun,
	})
	if tuiSink, ok := findTUISink(sinks); ok {
		tuiSink.Start()
		defer tuiSink.Wait()
	}

	_, waitErr := RunMulti(runCtx, reviewers, reviewtypes.RunConfig{}, sinks)
	if waitErr != nil && runCtx.Err() == nil && ctx.Err() == nil {
		return fmt.Errorf("review run: %w", waitErr)
	}
	return nil
}

// handlePickerError maps multi-picker error sentinels to the appropriate
// command-layer response.
//   - ErrPickerCancelled → return nil (user cancelled; no error shown)
//   - ErrNoAgentsSelected → surface error to user
//   - other errors → surface to user
func handlePickerError(cmd *cobra.Command, silentErr func(error) error, pickErr error) error {
	if errors.Is(pickErr, ErrPickerCancelled) {
		return nil
	}
	cmd.SilenceUsage = true
	fmt.Fprintln(cmd.ErrOrStderr(), pickErr.Error())
	return silentErr(pickErr)
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
		NewTUISink(in.agentNames, in.cancelRun, in.out),
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
		})
	}
	return sinks
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
	out, err := runGit(ctx, repoRoot, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	return strings.TrimSpace(out), nil
}
