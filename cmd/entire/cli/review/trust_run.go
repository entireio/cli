package review

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/entireio/cli/cmd/entire/cli/gitexec"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// registerTrustFlags adds the trust gate's flags to the review command.
func registerTrustFlags(cmd *cobra.Command, trustTarget *string, showConfig, showConfigJSON *bool) {
	cmd.Flags().StringVar(trustTarget, "trust-target", "", "approve reviewing code by someone else at this commit SHA (the review loads the checkout's hooks, MCP servers, and settings)")
	cmd.Flags().BoolVar(showConfig, "show-config", false, "list what the review would run (hooks, MCP servers, settings) and exit without running it")
	cmd.Flags().BoolVar(showConfigJSON, "json", false, "with --show-config: print JSON")
	// Validate before any mode runs, so no mode silently ignores these flags.
	cmd.PreRunE = func(*cobra.Command, []string) error {
		return reviewGateOptions{TrustTarget: *trustTarget, ShowConfig: *showConfig, ShowConfigJSON: *showConfigJSON}.validate()
	}
}

// validate rejects malformed gate flags.
func (o reviewGateOptions) validate() error {
	if err := validateTrustTarget(o.TrustTarget); err != nil {
		return err
	}
	if o.ShowConfigJSON && !o.ShowConfig {
		return errors.New("--json requires --show-config")
	}
	return nil
}

const plainReviewLabel = "HEAD"

// reviewGateOptions carries the flags the trust gate reads.
type reviewGateOptions struct {
	TrustTarget    string
	Command        string
	AgentOverride  string
	ShowConfig     bool
	ShowConfigJSON bool
}

// reviewSettingsContext reads review settings from the user's own checkout,
// not the target worktree's .entire/settings.json, which the branch controls.
func reviewSettingsContext(ctx context.Context) context.Context {
	if caller := strings.TrimSpace(os.Getenv(envReviewFindingsWorktree)); caller != "" {
		return settings.WithWorktreeRoot(ctx, caller)
	}
	return ctx
}

// knownReviewAgents are the agents a review can launch.
var knownReviewAgents = []string{"claude-code", "codex", "pi"}

// profileTrustAgents lists the reviewer agents a profile launches (or just
// --agent). The judge runs from a temp directory, so it is not included. An
// agent counts as isolated only if every worker on it has a profile config.
func profileTrustAgents(profile settings.ReviewProfileConfig, agentOverride string) []TrustAgent {
	workers := nonZeroAgentConfigs(profile.Agents)
	if agentOverride != "" {
		if worker, cfg, err := selectProfileWorker(profile, agentOverride); err == nil {
			workers = map[string]settings.ReviewConfig{worker: cfg}
		}
	}
	isolated := map[string]bool{}
	for worker, cfg := range workers {
		name := reviewAgentName(worker, cfg)
		prev, seen := isolated[name]
		isolated[name] = cfg.Config != nil && (!seen || prev)
	}
	agents := make([]TrustAgent, 0, len(isolated))
	for _, name := range sortedStringKeys(isolated) {
		agents = append(agents, TrustAgent{Name: name, Isolated: isolated[name]})
	}
	return agents
}

// showConfigAgents uses the named profile's agents, or all launchable ones.
func showConfigAgents(ctx context.Context, profileName, agentOverride string) []TrustAgent {
	if s, err := settings.Load(reviewSettingsContext(ctx)); err == nil && s != nil {
		applyLegacyReviewProfileFallback(s)
		if strings.TrimSpace(profileName) != "" {
			if _, profile, selErr := selectReviewProfile(s, profileName); selErr == nil {
				if agents := profileTrustAgents(profile, agentOverride); len(agents) > 0 {
					return agents
				}
			}
		}
	}
	agents := make([]TrustAgent, 0, len(knownReviewAgents))
	for _, name := range knownReviewAgents {
		agents = append(agents, TrustAgent{Name: name})
	}
	return agents
}

// gatePlainReview applies the trust gate to a review of the current checkout.
func gatePlainReview(ctx context.Context, cmd *cobra.Command, opts reviewGateOptions, agents []TrustAgent, deps Deps) error {
	worktreeRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return fmt.Errorf("resolve worktree root: %w", err)
	}
	head, err := gitexec.HeadSHA(ctx, worktreeRoot)
	if err != nil {
		return trustInspectionFailed(cmd, deps, err)
	}
	// The parent already gated a target re-run and forwards the pinned head;
	// requiring it to match means the env var alone can't skip the gate. An
	// agent caller still goes through the gate: both are plain environment it
	// could set itself, and the gate is what tells the user it approved.
	if os.Getenv(envReviewFindingsWorktree) != "" && trustTargetMatches(opts.TrustTarget, head) && detectAgentCaller() == "" {
		return nil
	}
	subject, inv, err := inspectReview(ctx, worktreeRoot, head, TrustSource{WorktreeRoot: worktreeRoot}, agents, false, deps)
	if err != nil {
		return trustInspectionFailed(cmd, deps, err)
	}
	subject.Label = plainReviewLabel
	subject.Branch = currentBranchLabel(ctx, worktreeRoot)
	return runTrustGate(ctx, cmd, opts, subject, inv, deps)
}

// inspectReview gathers authorship and, when needed, what source would run.
func inspectReview(ctx context.Context, repoRoot, head string, source TrustSource, agents []TrustAgent, alwaysInventory bool, deps Deps) (TrustSubject, TrustInventory, error) {
	subject, err := commitAuthorship(ctx, repoRoot, head)
	if err != nil {
		return TrustSubject{}, TrustInventory{}, err
	}
	if subject.Yours && !alwaysInventory {
		return subject, TrustInventory{}, nil
	}
	if deps.InspectTrust == nil {
		return TrustSubject{}, TrustInventory{}, errors.New("review configuration inspection is unavailable")
	}
	inv, err := deps.InspectTrust(ctx, source, agents)
	if err != nil {
		return TrustSubject{}, TrustInventory{}, err
	}
	return subject, inv, nil
}

func runTrustGate(ctx context.Context, cmd *cobra.Command, opts reviewGateOptions, subject TrustSubject, inv TrustInventory, deps Deps) error {
	gate := trustGate{
		Subject:     subject,
		Inventory:   inv,
		TrustTarget: opts.TrustTarget,
		Command:     opts.Command,
		Interactive: reviewCommandIsInteractive(cmd),
		AgentCaller: detectAgentCaller(),
		Confirm:     confirmTrustOnTerminal,
	}
	err := gate.run(ctx, cmd.ErrOrStderr())
	if errors.Is(err, errTrustRefused) {
		cmd.SilenceUsage = true
		return wrapReviewSilentError(deps.NewSilentError, err)
	}
	return err
}

func trustInspectionFailed(cmd *cobra.Command, deps Deps, err error) error {
	cmd.SilenceUsage = true
	fmt.Fprintf(cmd.ErrOrStderr(), "Not run: could not check what this branch would run (%s). Nothing was checked out or run.\n", sanitizeDisplay(err.Error()))
	return wrapReviewSilentError(deps.NewSilentError, err)
}

func currentBranchLabel(ctx context.Context, worktreeRoot string) string {
	out, err := gitexec.Run(ctx, worktreeRoot, "branch", "--show-current")
	if err != nil || strings.TrimSpace(out) == "" {
		return plainReviewLabel
	}
	return strings.TrimSpace(out)
}

// runReviewShowConfig lists what a review of the current checkout would run.
func runReviewShowConfig(ctx context.Context, cmd *cobra.Command, profileName string, opts reviewGateOptions, deps Deps) error {
	worktreeRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return fmt.Errorf("resolve worktree root: %w", err)
	}
	head, err := gitexec.HeadSHA(ctx, worktreeRoot)
	if err != nil {
		return fmt.Errorf("resolve HEAD: %w", err)
	}
	agents := showConfigAgents(ctx, profileName, opts.AgentOverride)
	subject, inv, err := inspectReview(ctx, worktreeRoot, head, TrustSource{WorktreeRoot: worktreeRoot}, agents, true, deps)
	if err != nil {
		return err
	}
	subject.Label = plainReviewLabel
	subject.Branch = currentBranchLabel(ctx, worktreeRoot)
	return printTrustConfig(cmd.OutOrStdout(), subject, inv, opts.ShowConfigJSON)
}

// reviewInvocationFlags are kept when echoing the command in hints; --prompt
// and the gate's own flags are left out.
var reviewInvocationFlags = []string{"target", "profile", "agent", reviewFlagModel, "base", "timeout"}

const reviewFlagModel = "model"

// reviewInvocation rebuilds the user's command for hints.
func reviewInvocation(cmd *cobra.Command, positional []string) string {
	parts := []string{"entire", cmd.Name()}
	for _, arg := range positional {
		parts = append(parts, shellQuoteArg(arg))
	}
	cmd.Flags().Visit(func(flag *pflag.Flag) {
		if !slices.Contains(reviewInvocationFlags, flag.Name) {
			return
		}
		parts = append(parts, "--"+flag.Name, shellQuoteArg(flag.Value.String()))
	})
	return strings.Join(parts, " ")
}

var shellSafeArg = regexp.MustCompile(`^[A-Za-z0-9_./:=@%+,-]+$`)

func shellQuoteArg(s string) string {
	if shellSafeArg.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(sanitizeDisplay(s), "'", `'\''`) + "'"
}
