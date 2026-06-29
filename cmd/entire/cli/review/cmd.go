// Package review — see env.go for package-level rationale.
//
// cmd.go provides NewCommand(), the cobra entry point for `entire review`.
// It routes through the new AgentReviewer / Sink / Run architecture for
// agents with review-runner adapters (claude-code, codex, gemini) and falls
// back to RunMarkerFallback for agents that are not yet wired into that review
// runner contract.
package review

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

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
	"github.com/entireio/cli/cmd/entire/cli/session"
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

	// HeadHasReviewCheckpoint checks whether HEAD's checkpoint metadata
	// includes a review session. Returns (true, infoString) if HasReview is set.
	// Injected to avoid an import cycle: review → checkpoint → codex → review.
	HeadHasReviewCheckpoint func(ctx context.Context) (bool, string)

	// ReviewCheckpointContext returns best-effort checkpoint context for the
	// branch review scope. Injected from the cli package because checkpoint
	// readers cannot be imported here without cycling through agent reviewers.
	ReviewCheckpointContext func(ctx context.Context, worktreeRoot string, scopeBaseRef string) string

	// ReviewerFor maps an agent registry name to its AgentReviewer
	// implementation. Returns nil for agents that do not yet have a review-runner
	// adapter. Injected to break the import cycle:
	// per-agent reviewer packages import review (for ComposeReviewPrompt /
	// AppendReviewEnv), so review/cmd.go cannot import them back.
	ReviewerFor func(agentName string) reviewtypes.AgentReviewer

	// PostReviewToTrail posts the final review verdict to the current branch's
	// trail as a finding (the "trail" output destination). Injected from the cli
	// package because the data API + auth live there. It prints its own success
	// line to out. nil when trail delivery is unavailable (e.g. tests), in which
	// case the run falls back to local output with a notice.
	PostReviewToTrail func(ctx context.Context, out io.Writer, profileName, verdict string) error
}

// NewCommand returns the `entire review` cobra command wired with the
// provided deps. Callers in the cli package pass a fully-populated Deps;
// tests pass a Deps with stub fields.
func NewCommand(deps Deps) *cobra.Command {
	var configure bool
	var edit bool
	var agentOverride string
	var modelOverride string
	var baseOverride string
	var profileOverride string
	var perRunPrompt string
	var findings bool
	var listModels bool
	var listAgents bool
	var listProfiles bool
	var setAgents []string
	var setJudge string
	var setOutput string
	var setLocal bool
	var reviewTimeout time.Duration
	var setTask string
	var setModels []string
	var setSlots []string

	cmd := &cobra.Command{
		Use: "review",
		// Hidden from `entire help` while the feature is still maturing —
		// users who know about it can still run `entire review` / `entire
		// review --help` and the command works normally.
		Hidden: true,
		Short:  "Run a multi-agent review against the current branch",
		Long: `Run a multi-agent review against the current branch: several reviewer
agents review the change in parallel, then a single judge consolidates their
reports into the final verdict in a closing round. Reviews are saved as named
profiles in Entire settings and clone-local preferences. On first run, guided
setup writes a profile and asks before starting agents.

Flags:
  --configure    set up a review profile (shows available agents + profiles).
                 With --set-* flags it writes the profile non-interactively;
                 otherwise it opens the wizard (interactive) without starting agents.
  --set-agents   with --configure: comma-separated reviewer agents for the profile
  --set-judge    with --configure: the consolidating judge as agent[=model]
  --set-output   with --configure: where the verdict goes: local (default) or trail
  --local        with --configure: save to .entire/settings.local.json (just you)
                 instead of .entire/settings.json (shared). Interactive setup asks.
  --set-task     with --configure: the profile's canonical task text
  --set-model    with --configure: per-reviewer model as agent=model (repeatable)
  --set-slot     with --configure: a reviewer slot as agent[=model] (repeatable;
                 the same agent/model may repeat to run it multiple times)
  --edit         re-open the advanced profile skill picker
  --findings     browse local findings
  --agent NAME   run only one reviewer from the selected profile
  --list         list configured review profiles (their reviewers and judge)
  --agents       list the reviewer agents you can pass to --agent for the profile
  --model NAME   override the model for the --agent reviewer (requires --agent)
  --models       list the models each agent advertises (optionally --agent NAME)
  --profile NAME select a profile (also accepted as positional arg)
  --prompt TEXT  add one-off per-run instructions for this invocation
  --timeout DUR  max time each reviewer may run before it's cancelled and
                 marked failed (default 10m; 0 disables). Siblings and the
                 judge proceed.
  --base REF     scope against REF instead of mainline. Useful for stacked
                 PRs where the base is the parent feature branch, not main.
                 Default: first existing of origin/HEAD, origin/main,
                 origin/master, main, master.

To tag an already-finished session as a review, use
'entire attach --review <id>'.`,
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 1 {
				return fmt.Errorf("accepts at most one argument, received %d", len(args))
			}
			if len(args) == 1 && profileOverride != "" {
				return errors.New("pass profile either positionally or with --profile, not both")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			// Discover external agents so review configs that target them
			// resolve correctly — without this, GetAgentsWithHooksInstalled
			// and agent.Get can't see them.
			external.DiscoverAndRegister(ctx)

			if listModels {
				return runReviewListModels(ctx, cmd, agentOverride, deps)
			}
			if listAgents {
				listProfile := profileOverride
				if len(args) == 1 {
					listProfile = args[0]
				}
				return runReviewListAgents(ctx, cmd, listProfile, deps)
			}
			if listProfiles {
				return runReviewListProfiles(ctx, cmd, deps)
			}

			modes := 0
			for _, enabled := range []bool{configure, edit, findings} {
				if enabled {
					modes++
				}
			}
			if modes > 1 {
				return errors.New("--configure, --edit, and --findings are mutually exclusive")
			}
			if modelOverride != "" && agentOverride == "" {
				return errors.New("--model requires --agent (the model applies to a single reviewer)")
			}
			profileName := profileOverride
			if len(args) == 1 {
				profileName = args[0]
			}
			if configure {
				return runReviewConfigure(ctx, cmd, profileName, reviewConfigureOptions{
					Agents: setAgents,
					Judge:  setJudge,
					Output: setOutput,
					Local:  setLocal,
					Task:   setTask,
					Models: setModels,
					Slots:  setSlots,
				}, deps)
			}
			if edit {
				return RunReviewProfileConfigPicker(ctx, cmd.OutOrStdout(), deps.GetAgentsWithHooksInstalled, profileName)
			}
			if findings {
				return runReviewFindings(ctx, cmd, deps.NewSilentError)
			}
			// --timeout 0 disables the per-reviewer bound. The RunConfig zero
			// value means "use the default", so translate an explicit 0 to a
			// negative disable sentinel. (The flag defaults to 10m, so the value is
			// only 0 when the user passed --timeout 0.)
			timeoutArg := reviewTimeout
			if timeoutArg == 0 {
				timeoutArg = -1
			}
			return runReview(ctx, cmd, agentOverride, modelOverride, baseOverride, profileName, perRunPrompt, timeoutArg, deps)
		},
	}
	cmd.Flags().BoolVar(&configure, "configure", false, "set up a review profile; shows available agents and accepts --set-* flags for non-interactive config")
	cmd.Flags().StringSliceVar(&setAgents, "set-agents", nil, "with --configure: reviewer agents for the profile (comma-separated)")
	cmd.Flags().StringVar(&setJudge, "set-judge", "", "with --configure: the consolidating judge as agent[=model]")
	cmd.Flags().StringVar(&setOutput, "set-output", "", "with --configure: where the verdict is delivered (local or trail)")
	cmd.Flags().BoolVar(&setLocal, "local", false, "with --configure: save the profile to .entire/settings.local.json (per-developer) instead of .entire/settings.json")
	cmd.Flags().StringVar(&setTask, "set-task", "", "with --configure: the profile's canonical task text")
	cmd.Flags().StringArrayVar(&setModels, "set-model", nil, "with --configure: per-reviewer model as agent=model (repeatable)")
	cmd.Flags().StringArrayVar(&setSlots, "set-slot", nil, "with --configure: a reviewer slot as agent[=model] (repeatable; same agent/model may repeat)")
	cmd.Flags().BoolVar(&edit, "edit", false, "re-open the advanced review profile skill picker")
	cmd.Flags().BoolVar(&findings, "findings", false, "browse local review findings")
	cmd.Flags().BoolVar(&listAgents, "agents", false, "list the reviewer agents you can pass to --agent for the selected profile")
	cmd.Flags().BoolVar(&listModels, "models", false, "list the models each review agent advertises (optionally filtered by --agent)")
	cmd.Flags().BoolVar(&listProfiles, "list", false, "list configured review profiles (reviewers and judge)")
	cmd.Flags().StringVar(&agentOverride, "agent", "", "run one configured reviewer from the selected profile")
	cmd.Flags().StringVar(&modelOverride, "model", "", "override the model for the --agent reviewer (requires --agent)")
	cmd.Flags().StringVar(&profileOverride, "profile", "", "review profile to run (default: review_default_profile or general)")
	cmd.Flags().StringVar(&perRunPrompt, "prompt", "", "one-off instructions appended to this review run")
	cmd.Flags().StringVar(&baseOverride, "base", "", "git ref to scope the review against (default: origin/HEAD → origin/main → origin/master → main → master)")
	cmd.Flags().DurationVar(&reviewTimeout, "timeout", defaultReviewerTimeout, "max time each reviewer may run before it is cancelled and marked failed (0 disables)")
	// The listing modes and the action modes each select a distinct command
	// behavior; combining them silently runs one and drops the rest, so reject
	// the combination up front with a clear cobra error.
	cmd.MarkFlagsMutuallyExclusive("configure", "edit", "findings", "list", "agents", "models")
	return cmd
}

// reviewConfigureOptions carries the non-interactive `--configure` inputs.
type reviewConfigureOptions struct {
	Agents []string // reviewer agent names (--set-agents)
	Judge  string   // consolidating judge as "agent[=model]" (--set-judge)
	Output string   // output destination: local|trail (--set-output)
	Local  bool     // save to local settings file instead of project (--local)
	Task   string   // profile task text (--set-task)
	Models []string // per-reviewer "agent=model" entries (--set-model)
	Slots  []string // reviewer slots as "agent[=model]" entries (--set-slot)
}

func (o reviewConfigureOptions) scripted() bool {
	// Local selects the destination only; by itself it must not force the
	// non-interactive/scripted path. `entire review --configure --local` should
	// still run the guided picker and preselect the local settings file.
	return len(o.Agents) > 0 || o.Judge != "" || o.Output != "" || o.Task != "" || len(o.Models) > 0 || len(o.Slots) > 0
}

func runReviewConfigure(ctx context.Context, cmd *cobra.Command, profileOverride string, opts reviewConfigureOptions, deps Deps) error {
	out := cmd.OutOrStdout()
	silentErr := deps.NewSilentError
	if _, err := paths.WorktreeRoot(ctx); err != nil {
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), "Not a git repository. Run `entire enable` first.")
		return silentErr(errors.New("not a git repository"))
	}
	s, err := settings.Load(ctx)
	if err != nil {
		cmd.SilenceUsage = true
		fmt.Fprintf(cmd.ErrOrStderr(), "Failed to load settings: %v\n", err)
		return silentErr(err)
	}
	if s == nil {
		s = &settings.EntireSettings{}
	}
	applyLegacyReviewProfileFallback(s)
	profileName := strings.TrimSpace(profileOverride)
	if profileName == "" {
		profileName = strings.TrimSpace(s.ReviewDefaultProfile)
	}
	if profileName == "" {
		profileName = DefaultProfileName
	}
	installed := deps.GetAgentsWithHooksInstalled(ctx)

	// Scripted path: build + save the profile from --set-* flags, no TUI. The
	// destination is the --local flag (default: project settings).
	if opts.scripted() {
		profile, buildErr := buildConfiguredProfile(ctx, profileName, opts, s, deps)
		if buildErr != nil {
			cmd.SilenceUsage = true
			fmt.Fprintln(cmd.ErrOrStderr(), buildErr.Error())
			return silentErr(buildErr)
		}
		scope := reviewScopeProject
		if opts.Local {
			scope = reviewScopeLocal
		}
		if err := saveReviewProfile(ctx, profileName, profile, false, scope); err != nil {
			return err
		}
		fmt.Fprintf(out, "Review profile %q saved to %s with %s.\n", profileName, scope.file(), strings.Join(sortedMapKeys(profile.Agents), ", "))
		fmt.Fprintf(out, "Run `entire review %s` to start.\n", profileName)
		return nil
	}

	// Interactive path: the guided wizard already lists the agents, so don't
	// duplicate the catalog here. Pass the raw --profile value (empty when not
	// given) so the guided setup runs the "what kind of review?" type picker
	// instead of being silently defaulted to the general profile.
	if interactive.IsTerminalWriter(out) && interactive.CanPromptInteractively() {
		name, profile, setupErr := RunReviewGuidedSetup(ctx, out, installed, deps.ReviewerFor, strings.TrimSpace(profileOverride), false, s)
		if setupErr != nil {
			return handlePickerError(cmd, silentErr, setupErr)
		}
		scope, scopeErr := promptForSettingsScope(ctx, opts.Local)
		if scopeErr != nil {
			return handlePickerError(cmd, silentErr, scopeErr)
		}
		if err := saveReviewProfile(ctx, name, profile, false, scope); err != nil {
			return err
		}
		fmt.Fprintf(out, "Review profile %q saved to %s. Run `entire review`, or `entire review %s`, to start.\n", name, scope.file(), name)
		return nil
	}

	// Non-interactive with no --set-* flags: this is the discovery view — show
	// the available agents, current profiles, and how to configure.
	catalog := availableReviewAgents(installed, deps.ReviewerFor)
	printReviewConfigCatalog(out, profileName, catalog, s)
	return nil
}

// runReviewListModels prints the models each review-runner agent advertises
// (claude-code, codex, gemini, ...). It needs no git repo or profile: model
// lists are advisory metadata. With agentFilter set, only that agent is shown.
func runReviewListModels(ctx context.Context, cmd *cobra.Command, agentFilter string, deps Deps) error {
	out := cmd.OutOrStdout()
	installed := deps.GetAgentsWithHooksInstalled(ctx)
	catalog := availableReviewAgents(installed, deps.ReviewerFor)

	if agentFilter != "" {
		filtered := make([]reviewAgentCatalogEntry, 0, 1)
		for _, e := range catalog {
			if e.Name == agentFilter {
				filtered = append(filtered, e)
			}
		}
		if len(filtered) == 0 {
			cmd.SilenceUsage = true
			err := fmt.Errorf("agent %q has no review runner adapter; available: %s", agentFilter, strings.Join(reviewAgentNames(deps), ", "))
			fmt.Fprintln(cmd.ErrOrStderr(), err.Error())
			return deps.NewSilentError(err)
		}
		catalog = filtered
	}

	for _, e := range catalog {
		fmt.Fprintf(out, "%s:\n", e.Name)
		ag, getErr := agent.Get(types.AgentName(e.Name))
		if getErr != nil {
			fmt.Fprintln(out, "  (agent unavailable)")
			continue
		}
		lister, ok := agent.AsModelLister(ag)
		if !ok {
			fmt.Fprintln(out, "  (no advertised models; pass any value your CLI accepts via --model)")
			continue
		}
		models, listErr := lister.ListModels(ctx)
		if listErr != nil || len(models) == 0 {
			fmt.Fprintln(out, "  (model list unavailable)")
			continue
		}
		for _, m := range models {
			if m.Note != "" {
				fmt.Fprintf(out, "  %-18s %s\n", m.ID, m.Note)
			} else {
				fmt.Fprintf(out, "  %s\n", m.ID)
			}
		}
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, "These are common models/aliases, not an exhaustive list. Use one with:")
	fmt.Fprintln(out, "  entire review --agent <name> --model <model>")
	return nil
}

// runReviewListProfiles prints the configured review profiles with their
// reviewers and judges, marking the default. Needs settings but no review run.
func runReviewListProfiles(ctx context.Context, cmd *cobra.Command, deps Deps) error {
	out := cmd.OutOrStdout()
	s, err := settings.Load(ctx)
	if err != nil {
		cmd.SilenceUsage = true
		fmt.Fprintf(cmd.ErrOrStderr(), "Failed to load settings: %v\n", err)
		return deps.NewSilentError(err)
	}
	if s == nil {
		s = &settings.EntireSettings{}
	}
	applyLegacyReviewProfileFallback(s)
	profiles := nonZeroProfiles(s.ReviewProfiles)
	if len(profiles) == 0 {
		fmt.Fprintln(out, "No review profiles configured. Create one with `entire review --configure`.")
		return nil
	}
	defaultName := strings.TrimSpace(s.ReviewDefaultProfile)
	fmt.Fprintln(out, "Profiles:")
	for _, name := range sortedMapKeys(profiles) {
		p := profiles[name]
		p.Agents = nonZeroAgentConfigs(p.Agents)
		marker := ""
		if name == defaultName {
			marker = "  (default)"
		}
		fmt.Fprintf(out, "  %s%s\n", name, marker)

		reviewers := make([]string, 0, len(p.Agents))
		for _, w := range sortedMapKeys(p.Agents) {
			cfg := p.Agents[w]
			model := strings.TrimSpace(cfg.Model)
			if model == "" {
				model = "default"
			}
			reviewers = append(reviewers, reviewAgentName(w, cfg)+" · "+model)
		}
		fmt.Fprintf(out, "    reviewers: %s\n", strings.Join(reviewers, ", "))

		if j, ok := profileJudge(p); ok {
			fmt.Fprintf(out, "    judge:      %s\n", judgeLabel(j))
		}
		fmt.Fprintf(out, "    output:     %s\n", profileOutput(p))
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Run one with `entire review <name>`.")
	return nil
}

const reviewHooksInstalledStatus = "hooks installed"

// runReviewListAgents lists the reviewer agents valid for `--agent` in the
// resolved profile (with hook-install status). With no
// usable profile it falls back to the available review-agent catalog.
func runReviewListAgents(ctx context.Context, cmd *cobra.Command, profileOverride string, deps Deps) error {
	out := cmd.OutOrStdout()
	installed := deps.GetAgentsWithHooksInstalled(ctx)
	installedSet := make(map[string]struct{}, len(installed))
	for _, n := range installed {
		installedSet[string(n)] = struct{}{}
	}

	s, err := settings.Load(ctx)
	if err == nil && s != nil {
		if name, profile, selErr := selectReviewProfile(s, profileOverride); selErr == nil {
			profile.Agents = nonZeroAgentConfigs(profile.Agents)
			fmt.Fprintf(out, "Reviewers in profile %q (pass one to --agent):\n", name)
			for _, worker := range sortedMapKeys(profile.Agents) {
				cfg := profile.Agents[worker]
				status := reviewHooksInstalledStatus
				if _, ok := installedSet[reviewAgentName(worker, cfg)]; !ok {
					status = "hooks NOT installed; run `entire configure --agent " + reviewAgentName(worker, cfg) + "`"
				}
				fmt.Fprintf(out, "  %s: %s\n", reviewWorkerLabel(worker, cfg), status)
			}
			fmt.Fprintln(out)
			fmt.Fprintln(out, "See all available agents and profiles with `entire review --configure`.")
			return nil
		}
	}

	// No usable profile: show the catalog of available review agents instead.
	catalog := availableReviewAgents(installed, deps.ReviewerFor)
	fmt.Fprintln(out, "No review profile configured yet. Available review agents:")
	for _, e := range catalog {
		status := "not installed; run `entire configure --agent " + e.Name + "`"
		if e.Installed {
			status = reviewHooksInstalledStatus
		}
		fmt.Fprintf(out, "  %-14s %s\n", e.Name, status)
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Configure a profile with `entire review --configure`.")
	return nil
}

// reviewAgentCatalogEntry is one row in the `--configure` discovery listing.
type reviewAgentCatalogEntry struct {
	Name      string
	Installed bool
}

// availableReviewAgents lists every registered agent that has a review-runner
// adapter (claude-code, codex, gemini, pi, ...), marking which have hooks
// installed in this repo. Derived from the registry + deps.ReviewerFor so it
// never drifts from the set of agents `entire review` can actually launch.
func availableReviewAgents(installed []types.AgentName, reviewerFor func(string) reviewtypes.AgentReviewer) []reviewAgentCatalogEntry {
	installedSet := make(map[string]struct{}, len(installed))
	for _, n := range installed {
		installedSet[string(n)] = struct{}{}
	}
	var out []reviewAgentCatalogEntry
	for _, name := range agent.List() {
		ns := string(name)
		if reviewerFor(ns) == nil {
			continue
		}
		_, ok := installedSet[ns]
		out = append(out, reviewAgentCatalogEntry{Name: ns, Installed: ok})
	}
	return out
}

func printReviewConfigCatalog(out io.Writer, profileName string, catalog []reviewAgentCatalogEntry, s *settings.EntireSettings) {
	fmt.Fprintln(out, "Available review agents:")
	if len(catalog) == 0 {
		fmt.Fprintln(out, "  (none; install one with `entire configure --agent claude-code`)")
	}
	for _, e := range catalog {
		status := "not installed; run `entire configure --agent " + e.Name + "`"
		if e.Installed {
			status = reviewHooksInstalledStatus
		}
		fmt.Fprintf(out, "  %-14s %s\n", e.Name, status)
	}

	fmt.Fprintln(out)
	profiles := nonZeroProfiles(s.ReviewProfiles)
	if len(profiles) == 0 {
		fmt.Fprintln(out, "Configured profiles: (none yet)")
	} else {
		fmt.Fprintln(out, "Configured profiles:")
		for _, name := range sortedMapKeys(profiles) {
			p := profiles[name]
			marker := ""
			if name == strings.TrimSpace(s.ReviewDefaultProfile) {
				marker = " (default)"
			}
			line := fmt.Sprintf("  %s%s: %s", name, marker, strings.Join(sortedMapKeys(p.Agents), ", "))
			if j, ok := profileJudge(p); ok {
				line += "  judge=" + j.agent
			}
			if profileOutput(p) == ReviewOutputTrail {
				line += "  output=trail"
			}
			fmt.Fprintln(out, line)
		}
	}

	fmt.Fprintln(out)
	fmt.Fprintf(out, "Configure %q non-interactively, e.g.:\n", profileName)
	fmt.Fprintf(out, "  entire review --configure --profile %s --set-agents %s --set-judge <agent>\n",
		profileName, exampleAgentList(catalog))
}

func exampleAgentList(catalog []reviewAgentCatalogEntry) string {
	names := make([]string, 0, len(catalog))
	for _, e := range catalog {
		if e.Installed {
			names = append(names, e.Name)
		}
	}
	if len(names) == 0 {
		return "claude-code,codex"
	}
	if len(names) > 2 {
		names = names[:2]
	}
	return strings.Join(names, ",")
}

// buildConfiguredProfile produces a ReviewProfileConfig from --set-* flags,
// merging onto any existing profile so unspecified profile-level fields
// (task, master_model) are preserved.
func buildConfiguredProfile(ctx context.Context, profileName string, opts reviewConfigureOptions, s *settings.EntireSettings, deps Deps) (settings.ReviewProfileConfig, error) {
	profile := s.ReviewProfiles[profileName]

	if len(opts.Agents) > 0 || len(opts.Slots) > 0 {
		agents := make(map[string]settings.ReviewConfig, len(opts.Agents)+len(opts.Slots))
		for _, raw := range opts.Agents {
			name := strings.TrimSpace(raw)
			if name == "" {
				continue
			}
			if deps.ReviewerFor(name) == nil {
				return settings.ReviewProfileConfig{}, fmt.Errorf("agent %q has no review runner adapter; available: %s", name, strings.Join(reviewAgentNames(deps), ", "))
			}
			// Keyed by the bare agent name so `--set-model agent=model` can target
			// it; one default-model slot per agent.
			agents[name] = defaultReviewAgentConfig(profileName, name)
		}
		for _, raw := range opts.Slots {
			rawName, model, _ := strings.Cut(raw, "=")
			name := strings.TrimSpace(rawName)
			model = strings.TrimSpace(model)
			if name == "" {
				continue
			}
			if deps.ReviewerFor(name) == nil {
				return settings.ReviewProfileConfig{}, fmt.Errorf("agent %q has no review runner adapter; available: %s", name, strings.Join(reviewAgentNames(deps), ", "))
			}
			// Each slot is its own worker; workerIDForAgentModel disambiguates
			// duplicates (claude-code, claude-code-2, claude-code:opus, …).
			cfg := defaultReviewAgentConfig(profileName, name)
			cfg.Agent = name
			cfg.Model = model
			agents[workerIDForAgentModel(name, model, agents)] = cfg
		}
		if len(agents) == 0 {
			return settings.ReviewProfileConfig{}, errors.New("--set-agents/--set-slot listed no usable agents")
		}
		profile.Agents = agents
	}
	if len(nonZeroAgentConfigs(profile.Agents)) == 0 {
		return settings.ReviewProfileConfig{}, errors.New("profile has no agents; pass --set-agents or --set-slot")
	}

	for _, raw := range opts.Models {
		key, model, ok := strings.Cut(raw, "=")
		key = strings.TrimSpace(key)
		model = strings.TrimSpace(model)
		if !ok || key == "" || model == "" {
			return settings.ReviewProfileConfig{}, fmt.Errorf("invalid --set-model %q; expected agent=model", raw)
		}
		workerName, _, selErr := selectProfileWorker(profile, key)
		if selErr != nil {
			return settings.ReviewProfileConfig{}, fmt.Errorf("--set-model %q: %w", raw, selErr)
		}
		cfg := profile.Agents[workerName]
		cfg.Model = model
		profile.Agents[workerName] = cfg
	}

	if opts.Task != "" {
		profile.Task = opts.Task
	}
	if strings.TrimSpace(profile.Task) == "" {
		profile.Task = profileTask(profileName, settings.ReviewProfileConfig{})
	}

	// Judge: explicit --set-judge wins; otherwise a multi-reviewer profile gets
	// an auto-selected judge, and a single-reviewer profile needs none.
	reviewerCount := len(nonZeroAgentConfigs(profile.Agents))
	switch {
	case strings.TrimSpace(opts.Judge) != "":
		rawName, model, _ := strings.Cut(opts.Judge, "=")
		name := strings.TrimSpace(rawName)
		if name == "" {
			return settings.ReviewProfileConfig{}, errors.New("--set-judge needs an agent name")
		}
		// A judge consolidates the reviewers' reports via text generation, so it
		// must be a known agent that can write a verdict. Validate up front rather
		// than failing at synthesis time.
		if !agentSupportsTextGeneration(ctx, name) {
			if _, getErr := agent.Get(types.AgentName(name)); getErr != nil {
				return settings.ReviewProfileConfig{}, fmt.Errorf("--set-judge %q is not a known agent", name)
			}
			return settings.ReviewProfileConfig{}, fmt.Errorf("--set-judge %q cannot write a verdict (the agent has no text generation); choose an agent that supports text generation", name)
		}
		profile.Judge = &settings.ReviewConfig{Agent: name, Model: strings.TrimSpace(model)}
	case reviewerCount > 1 && (profile.Judge == nil || profile.Judge.IsZero()):
		if j, ok := defaultJudge(ctx, profile.Agents); ok {
			profile.Judge = &settings.ReviewConfig{Agent: j.agent, Model: j.model}
		}
	case reviewerCount <= 1:
		profile.Judge = nil
	}

	if opts.Output != "" {
		out, outErr := normalizeReviewOutput(opts.Output)
		if outErr != nil {
			return settings.ReviewProfileConfig{}, outErr
		}
		// Store only the non-default destination so local profiles stay clean.
		if out == ReviewOutputTrail {
			profile.Output = ReviewOutputTrail
		} else {
			profile.Output = ""
		}
	}
	return profile, nil
}

func reviewAgentNames(deps Deps) []string {
	var names []string
	for _, name := range agent.List() {
		if deps.ReviewerFor(string(name)) != nil {
			names = append(names, string(name))
		}
	}
	return names
}

// runReview executes the main review flow.
func runReview(ctx context.Context, cmd *cobra.Command, agentOverride, modelOverride, baseOverride, profileOverride, perRunPrompt string, timeout time.Duration, deps Deps) error {
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
	// which would cause the config writer to write over the user's other
	// settings with an empty EntireSettings{}.
	s, err := settings.Load(ctx)
	if err != nil {
		cmd.SilenceUsage = true
		fmt.Fprintf(cmd.ErrOrStderr(), "Failed to load settings: %v\n", err)
		fmt.Fprintln(cmd.ErrOrStderr(),
			"Fix your Entire settings or clone-local review preferences and re-run `entire review`.")
		return silentErr(err)
	}
	installed := deps.GetAgentsWithHooksInstalled(ctx)
	if s == nil {
		s = &settings.EntireSettings{}
	}
	applyLegacyReviewProfileFallback(s)

	profileOverride = strings.TrimSpace(profileOverride)
	interactiveTTY := interactive.IsTerminalWriter(out) && interactive.CanPromptInteractively()

	// Bare `entire review` never auto-runs a profile. Without a TTY we cannot
	// prompt, so list the profiles (or point at setup) and require an explicit
	// selection rather than silently spawning a default crew.
	if profileOverride == "" && !interactiveTTY {
		cmd.SilenceUsage = true
		eo := cmd.ErrOrStderr()
		if profs := nonZeroProfiles(s.ReviewProfiles); len(profs) > 0 {
			ns := sortedMapKeys(profs)
			fmt.Fprintf(eo, "Specify a profile to review, e.g. `entire review %s`.\n", ns[0])
			fmt.Fprintf(eo, "Configured profiles: %s\n", strings.Join(ns, ", "))
		} else {
			fmt.Fprintln(eo, "No review profiles configured. Run `entire review --configure` in a terminal first.")
		}
		return silentErr(errors.New("no profile specified"))
	}

	// Trigger first-run setup when no usable profile exists. Counting only
	// non-zero profiles means a placeholder/empty entry (e.g. an empty
	// `general` profile in a hand-edited preferences file) still routes through
	// guided setup / the non-interactive default instead of dead-ending later in
	// selectReviewProfile with "every profile is empty".
	if len(nonZeroProfiles(s.ReviewProfiles)) == 0 {
		profileForSetup := profileOverride
		var profile settings.ReviewProfileConfig
		// Non-interactive first run writes the shared project settings; interactive
		// setup asks the user where to save below.
		saveScope := reviewScopeProject
		guidedSetup := interactive.IsTerminalWriter(out) && interactive.CanPromptInteractively()
		if guidedSetup {
			var setupErr error
			profileForSetup, profile, setupErr = RunReviewGuidedSetup(ctx, out, installed, deps.ReviewerFor, profileForSetup, true, s)
			if setupErr != nil {
				return handlePickerError(cmd, silentErr, setupErr)
			}
			scope, scopeErr := promptForSettingsScope(ctx, false)
			if scopeErr != nil {
				return handlePickerError(cmd, silentErr, scopeErr)
			}
			saveScope = scope
		} else {
			if profileForSetup == "" {
				profileForSetup = DefaultProfileName
			}
			defaultProfile, defaultErr := defaultReviewProfileForInstalledAgents(ctx, profileForSetup, installed, deps.ReviewerFor)
			if defaultErr != nil {
				cmd.SilenceUsage = true
				fmt.Fprintln(cmd.ErrOrStderr(), defaultErr.Error())
				return silentErr(defaultErr)
			}
			profile = defaultProfile
			fmt.Fprintf(out, "No review profiles found; using default %q profile with %s.\n", profileForSetup, strings.Join(sortedMapKeys(profile.Agents), ", "))
			fmt.Fprintln(out, "Configure later with `entire review --configure`.")
			fmt.Fprintln(out)
		}
		if saveErr := saveReviewProfile(ctx, profileForSetup, profile, false, saveScope); saveErr != nil {
			return saveErr
		}
		s.ReviewProfiles = map[string]settings.ReviewProfileConfig{profileForSetup: profile}
		s.ReviewDefaultProfile = profileForSetup
		// The user just chose this profile in setup; treat it as the selection so
		// the chooser below doesn't prompt again.
		profileOverride = profileForSetup
		if guidedSetup {
			runNow, confirmErr := ConfirmRunReviewNow(ctx, out)
			if confirmErr != nil {
				return handlePickerError(cmd, silentErr, confirmErr)
			}
			if !runNow {
				return nil
			}
			fmt.Fprintln(out)
		}
	}

	// Interactive bare `entire review` with existing profiles: require a choice
	// instead of defaulting silently.
	if profileOverride == "" {
		picked, pickErr := promptForProfileToRun(ctx, s)
		if pickErr != nil {
			return handlePickerError(cmd, silentErr, pickErr)
		}
		profileOverride = picked
	}

	profileName, profile, err := selectReviewProfile(s, profileOverride)
	if err != nil {
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), err.Error())
		return silentErr(err)
	}
	profile.Task = profileTask(profileName, profile)
	profile.Agents = nonZeroAgentConfigs(profile.Agents)
	outputMode := profileOutput(profile)

	if agentOverride != "" {
		workerName, cfg, selectErr := selectProfileWorker(profile, agentOverride)
		if selectErr != nil {
			cmd.SilenceUsage = true
			err := fmt.Errorf("%w in review profile %q", selectErr, profileName)
			fmt.Fprintln(cmd.ErrOrStderr(), err.Error())
			return silentErr(err)
		}
		if modelOverride != "" {
			cfg.Model = modelOverride
		}
		return runSingleAgentPath(ctx, cmd, profileName, workerName, baseOverride, perRunPrompt, profile.Task, outputMode, timeout, cfg, installed, deps, out)
	}

	if missing := missingInstalledProfileAgents(profile.Agents, installed); len(missing) > 0 {
		cmd.SilenceUsage = true
		err := fmt.Errorf("hooks are not installed for review profile %q agent(s): %s; run `entire configure --agent <name>` first, or edit the profile", profileName, strings.Join(missing, ", "))
		fmt.Fprintln(cmd.ErrOrStderr(), err.Error())
		return silentErr(err)
	}

	eligible := ComputeEligibleConfiguredForProfile(profile, installed)
	switch len(eligible) {
	case 0:
		cmd.SilenceUsage = true
		err := fmt.Errorf("review profile %q has no eligible agents", profileName)
		fmt.Fprintln(cmd.ErrOrStderr(), err.Error())
		return silentErr(err)
	case 1:
		cfg := profile.Agents[eligible[0].Name]
		return runSingleAgentPath(ctx, cmd, profileName, eligible[0].Name, baseOverride, perRunPrompt, profile.Task, outputMode, timeout, cfg, installed, deps, out)
	default:
		launchableEligible := computeLaunchableEligibleForProfile(profile, installed, deps.ReviewerFor)
		if len(launchableEligible) != len(eligible) {
			nonLaunchable := nonLaunchableEligibleNames(profile, eligible, deps.ReviewerFor)
			cmd.SilenceUsage = true
			err := fmt.Errorf("review profile %q includes agent(s) without review runner adapters in a fan-out run: %s. Use --agent for a single manual fallback, or remove them from the profile", profileName, strings.Join(nonLaunchable, ", "))
			fmt.Fprintln(cmd.ErrOrStderr(), err.Error())
			return silentErr(err)
		}
		// Require a consolidating judge (explicit or auto-selected). A judge that
		// can't actually write a verdict (no text generation) is tolerated here and
		// handled at synthesis time, where it fails gracefully ("final report
		// unavailable").
		judge, ok := resolveJudge(ctx, profile)
		if !ok {
			cmd.SilenceUsage = true
			err := fmt.Errorf("review profile %q has multiple reviewers but no judge that can write a verdict; set review_profiles.%s.judge", profileName, profileName)
			fmt.Fprintln(cmd.ErrOrStderr(), err.Error())
			return silentErr(err)
		}
		return runMultiAgentPath(ctx, cmd, profileName, profile, launchableEligible, judge, outputMode, timeout, baseOverride, perRunPrompt, deps, out)
	}
}

func missingInstalledProfileAgents(configured map[string]settings.ReviewConfig, installed []types.AgentName) []string {
	installedSet := make(map[string]struct{}, len(installed))
	for _, name := range installed {
		installedSet[string(name)] = struct{}{}
	}
	var missing []string
	for name, cfg := range configured {
		if cfg.IsZero() {
			continue
		}
		agentName := reviewAgentName(name, cfg)
		if _, ok := installedSet[agentName]; !ok {
			missing = append(missing, reviewWorkerLabel(name, cfg))
		}
	}
	sort.Strings(missing)
	return missing
}

func nonLaunchableEligibleNames(profile settings.ReviewProfileConfig, eligible []AgentChoice, reviewerFor func(string) reviewtypes.AgentReviewer) []string {
	var out []string
	for _, c := range eligible {
		cfg := profile.Agents[c.Name]
		if reviewerFor(reviewAgentName(c.Name, cfg)) == nil {
			out = append(out, reviewWorkerLabel(c.Name, cfg))
		}
	}
	sort.Strings(out)
	return out
}

// confirmReReviewOrProceed implements the "HEAD already reviewed" guard.
// It returns (proceed, err). When the checkpoint has no prior review it returns
// (true, nil). In a non-interactive context it cannot prompt, so it proceeds
// (the user explicitly invoked `entire review`) after printing a note rather
// than blocking on a confirm form that would error out.
func confirmReReviewOrProceed(ctx context.Context, out io.Writer, deps Deps) (bool, error) {
	reviewed, meta := deps.HeadHasReviewCheckpoint(ctx)
	if !reviewed {
		return true, nil
	}
	if !interactive.CanPromptInteractively() {
		fmt.Fprintf(out, "Note: HEAD was already reviewed (%s); re-running.\n", meta)
		return true, nil
	}
	var proceed bool
	form := newAccessibleForm(huh.NewGroup(
		huh.NewConfirm().
			Title(fmt.Sprintf("Already reviewed: %s. Proceed anyway?", meta)).
			Value(&proceed),
	))
	if err := form.RunWithContext(ctx); err != nil {
		return false, err //nolint:wrapcheck // propagate huh cancellation
	}
	return proceed, nil
}

// runSingleAgentPath completes a single-agent review: verifies hooks + skills,
// guards against re-review, resolves scope, then dispatches via Run or
// RunMarkerFallback.
func runSingleAgentPath(
	ctx context.Context,
	cmd *cobra.Command,
	profileName, workerName, baseOverride, perRunPrompt, task, outputMode string,
	timeout time.Duration,
	cfg settings.ReviewConfig,
	installed []types.AgentName,
	deps Deps,
	out io.Writer,
) error {
	silentErr := deps.NewSilentError
	agentName := reviewAgentName(workerName, cfg)
	displayName := reviewWorkerLabel(workerName, cfg)

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
			agentName, agentName, displayName)
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
	if proceed, guardErr := confirmReReviewOrProceed(ctx, out, deps); guardErr != nil {
		fmt.Fprintln(out, "prompt cancelled")
		return silentErr(guardErr)
	} else if !proceed {
		fmt.Fprintln(out, "Review cancelled.")
		return nil
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
	scopeBaseRef, scopeErr := detectScope(ctx, worktreeRoot, baseOverride, out)
	if scopeErr != nil {
		cmd.SilenceUsage = true
		return scopeErr
	}
	checkpointContext := ""
	if deps.ReviewCheckpointContext != nil {
		checkpointContext = deps.ReviewCheckpointContext(ctx, worktreeRoot, scopeBaseRef)
	}

	runCfg := reviewtypes.RunConfig{
		ProfileName:       profileName,
		Task:              task,
		PerRunPrompt:      perRunPrompt,
		ScopeBaseRef:      scopeBaseRef,
		CheckpointContext: checkpointContext,
		StartingSHA:       headSHA,
		ReviewerTimeout:   timeout,
	}
	applyReviewConfig(&runCfg, cfg)

	// 7. Branch on launchability.
	reviewer := deps.ReviewerFor(agentName)
	if reviewer == nil {
		// No review runner adapter yet: write marker (with scope-aware prompt) and print guidance.
		return RunMarkerFallback(ctx, agentName, runCfg, worktreeRoot, out)
	}
	reviewer = &perAgentConfiguredReviewer{name: displayName, inner: reviewer, cfg: runCfg}

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	runCfg.EnrichSummary = reviewSummaryTokenEnricher(worktreeRoot, headSHA)
	canPrompt := interactive.CanPromptInteractively()
	sinks := composeSingleAgentSinks(singleAgentSinkInputs{
		out:       out,
		isTTY:     interactive.IsTerminalWriter(out) && canPrompt,
		canPrompt: canPrompt,
		agentName: displayName,
		cancelRun: cancelRun,
	})
	if tuiSink, ok := findTUISink(sinks); ok {
		tuiSink.Start()
		defer tuiSink.Wait()
	}

	summary, waitErr := Run(runCtx, reviewer, runCfg, sinks)
	writePostReviewManifest(ctx, out, worktreeRoot, headSHA, summary, "")
	maybePostReviewToTrail(ctx, out, deps, outputMode, profileName, summary, "")
	if waitErr != nil && runCtx.Err() == nil && ctx.Err() == nil {
		// Non-cancellation error: surface to caller.
		return fmt.Errorf("review run: %w", waitErr)
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
func detectScope(ctx context.Context, worktreeRoot, baseOverride string, out io.Writer) (string, error) {
	repo, openErr := gitrepo.OpenPath(worktreeRoot)
	if openErr != nil {
		logging.Debug(ctx, "review repo open failed", slog.String("error", openErr.Error()))
		// Fail-loud when the user explicitly asked for a base. Without this
		// branch an explicit --base flag would be silently dropped on
		// PlainOpen failure, inconsistent with the ComputeScopeStats error
		// path below that aborts on bad overrides.
		if baseOverride != "" {
			return "", fmt.Errorf("--base %q given but cannot open repository at %q: %w", baseOverride, worktreeRoot, openErr)
		}
		return "", nil
	}
	defer repo.Close()
	stats, statsErr := ComputeScopeStats(ctx, repo, baseOverride)
	if statsErr != nil {
		// With an override, the user explicitly asked for a specific base.
		// A bad ref must abort before agents spawn so the user learns about
		// the typo immediately, not after a long review run.
		if baseOverride != "" {
			return "", statsErr
		}
		logging.Debug(ctx, "review scope detection failed", slog.String("error", statsErr.Error()))
		return "", nil
	}
	fmt.Fprintln(out, formatScopeBanner(stats))
	return stats.BaseRef, nil
}

// runMultiAgentPath handles the profile-native fan-out flow. Every configured
// reviewer in the selected profile runs concurrently against the same
// canonical task, then the single judge consolidates their reports into the
// final verdict.
func runMultiAgentPath(
	ctx context.Context,
	cmd *cobra.Command,
	profileName string,
	profile settings.ReviewProfileConfig,
	launchableEligible []AgentChoice,
	judge judgeSpec,
	outputMode string,
	timeout time.Duration,
	baseOverride string,
	perRunPrompt string,
	deps Deps,
	out io.Writer,
) error {
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

	if proceed, guardErr := confirmReReviewOrProceed(ctx, out, deps); guardErr != nil {
		fmt.Fprintln(out, "prompt cancelled")
		return deps.NewSilentError(guardErr)
	} else if !proceed {
		fmt.Fprintln(out, "Review cancelled.")
		return nil
	}

	scopeBaseRef, scopeErr := detectScope(ctx, worktreeRoot, baseOverride, out)
	if scopeErr != nil {
		cmd.SilenceUsage = true
		return scopeErr
	}
	checkpointContext := ""
	if deps.ReviewCheckpointContext != nil {
		checkpointContext = deps.ReviewCheckpointContext(ctx, worktreeRoot, scopeBaseRef)
	}
	reviewers := make([]reviewtypes.AgentReviewer, 0, len(launchableEligible))
	for _, choice := range launchableEligible {
		workerName := choice.Name
		agentCfg := profile.Agents[workerName]
		agentName := reviewAgentName(workerName, agentCfg)
		if len(agentCfg.Skills) > 0 {
			ag, agErr := agent.Get(types.AgentName(agentName))
			if agErr != nil {
				return fmt.Errorf("resolve agent %s: %w", agentName, agErr)
			}
			if err := VerifyConfiguredSkillsInstalled(ctx, ag, agentCfg); err != nil {
				cmd.SilenceUsage = true
				fmt.Fprintln(cmd.ErrOrStderr(), err.Error())
				return deps.NewSilentError(err)
			}
		}
		reviewer := deps.ReviewerFor(agentName)
		if reviewer == nil {
			cmd.SilenceUsage = true
			return deps.NewSilentError(fmt.Errorf("agent %q has no review runner adapter but appeared in eligible list", agentName))
		}
		reviewers = append(reviewers, &perAgentConfiguredReviewer{
			name:  reviewWorkerLabel(workerName, agentCfg),
			inner: reviewer,
			cfg: runConfigWithReviewConfig(reviewtypes.RunConfig{
				ProfileName:       profileName,
				Task:              profile.Task,
				PerRunPrompt:      perRunPrompt,
				ScopeBaseRef:      scopeBaseRef,
				CheckpointContext: checkpointContext,
				StartingSHA:       headSHA,
			}, agentCfg),
		})
	}

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	agentNames := make([]string, len(reviewers))
	for i, r := range reviewers {
		agentNames[i] = r.Name()
	}
	aggregateOutput := ""

	// The single consolidating judge (resolved and validated by the caller)
	// turns the reviewers' reports into the final verdict.
	var synthProvider SynthesisProvider = AgentSynthesisProvider{AgentName: judge.agent, Model: judge.model}
	masterLabel := judgeLabel(judge)
	sinks := composeMultiAgentSinks(multiAgentSinkInputs{
		out:               out,
		isTTY:             interactive.IsTerminalWriter(out) && interactive.CanPromptInteractively(),
		agentNames:        agentNames,
		cancelRun:         cancelRun,
		runContext:        runCtx,
		synthesisProvider: synthProvider,
		perRunPrompt:      perRunPrompt,
		profileName:       profileName,
		task:              profile.Task,
		masterName:        masterLabel,
		onSynthesisResult: func(result string) {
			aggregateOutput = result
		},
	})
	if tuiSink, ok := findTUISink(sinks); ok {
		tuiSink.Start()
		defer tuiSink.Wait()
	}

	runMultiCfg := reviewtypes.RunConfig{ReviewerTimeout: timeout}
	runMultiCfg.EnrichAgentRun = reviewAgentRunTokenEnricherForRuns(worktreeRoot, headSHA, plannedAgentRunsForReviewers(reviewers, runMultiCfg))
	runMultiCfg.EnrichSummary = reviewSummaryTokenEnricher(worktreeRoot, headSHA)
	summary, waitErr := RunMulti(runCtx, reviewers, runMultiCfg, sinks)
	if shouldAbortMultiReview(summary, waitErr) && runCtx.Err() == nil && ctx.Err() == nil {
		return multiReviewFailureError(waitErr)
	}
	writePostReviewManifest(ctx, out, worktreeRoot, headSHA, summary, aggregateOutput)
	maybePostReviewToTrail(ctx, out, deps, outputMode, profileName, summary, aggregateOutput)
	return nil
}

// handlePickerError maps picker error sentinels to the appropriate
// command-layer response.
//   - ErrPickerCancelled → return nil (user cancelled; no error shown)
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
// It exists so tests can drive the helper with an explicit isTTY value
// instead of monkey-patching interactive helpers at run time.
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
	agentNames        []string
	cancelRun         context.CancelFunc
	runContext        context.Context
	synthesisProvider SynthesisProvider
	perRunPrompt      string
	profileName       string
	task              string
	masterName        string
	onSynthesisResult func(result string)
}

type singleAgentSinkInputs struct {
	out       io.Writer
	isTTY     bool
	canPrompt bool
	agentName string
	cancelRun context.CancelFunc
}

// composeMultiAgentSinks builds the sink slice for a multi-agent run. The
// master adjudication phase (SynthesisSink) runs unconditionally when a
// provider is configured — it needs no stdin, so it is available in TTY,
// redirected, and CI output alike.
//
//   - Non-TTY: [DumpSink, SynthesisSink?] — narrative dump plus the final report.
//   - TTY: [TUISink, buffered DumpSink, buffered SynthesisSink, buffer flusher].
//     The TUI stays up during the judge phase and post-run stdout is flushed
//     after the alt-screen exits.
//   - TTY without a provider: [TUISink, TUI finalizer, DumpSink].
func composeMultiAgentSinks(in multiAgentSinkInputs) []reviewtypes.Sink {
	sinks := []reviewtypes.Sink{}
	if in.isTTY {
		tui := NewTUISink(in.agentNames, in.cancelRun, in.out, os.Stdin)
		sinks = append(sinks, tui)
		if in.synthesisProvider != nil {
			postRunOut := &bytes.Buffer{}
			sinks = append(sinks, DumpSink{W: postRunOut})
			sinks = append(sinks, SynthesisSink{
				Provider:     in.synthesisProvider,
				Writer:       postRunOut,
				RenderWriter: in.out,
				PerRunPrompt: in.perRunPrompt,
				ProfileName:  in.profileName,
				Task:         in.task,
				MasterName:   in.masterName,
				RunContext:   in.runContext,
				OnResult:     in.onSynthesisResult,
				OnStart: func() {
					tui.FinalPhaseStarted(finalJudgeDisplayName(in.masterName))
				},
				OnComplete: func(err error) {
					tui.FinalPhaseFinished(err)
				},
			})
			sinks = append(sinks, tuiPostRunCompleteSink{tui: tui, buf: postRunOut, out: in.out})
			return sinks
		}
		sinks = append(sinks, tuiPostRunCompleteSink{tui: tui})
		sinks = append(sinks, DumpSink{W: in.out})
		return sinks
	}

	sinks = append(sinks, DumpSink{W: in.out})
	if in.synthesisProvider != nil {
		sinks = append(sinks, SynthesisSink{
			Provider:     in.synthesisProvider,
			Writer:       in.out,
			PerRunPrompt: in.perRunPrompt,
			ProfileName:  in.profileName,
			Task:         in.task,
			MasterName:   in.masterName,
			RunContext:   in.runContext,
			OnResult:     in.onSynthesisResult,
		})
	}
	return sinks
}

func finalJudgeDisplayName(masterName string) string {
	masterName = strings.TrimSpace(masterName)
	if masterName == "" {
		return "final judge"
	}
	return "judge: " + masterName
}

// shouldAbortMultiReview reports whether the profile-native fan-out produced no
// successful reviewer at all. Individual reviewer infrastructure failures (for
// example quota/auth/tool failures) should not fail the entire review when at
// least one sibling produced a usable review; the failed reviewer remains
// visible in terminal output only. With zero successful reviewers, there is no
// review result to manifest or post, so the command fails loudly.
func shouldAbortMultiReview(summary reviewtypes.RunSummary, waitErr error) bool {
	if len(summary.AgentRuns) == 0 {
		return waitErr != nil
	}
	for _, run := range summary.AgentRuns {
		if run.Status == reviewtypes.AgentStatusSucceeded {
			return false
		}
	}
	if waitErr != nil {
		return true
	}
	for _, run := range summary.AgentRuns {
		if run.Status == reviewtypes.AgentStatusFailed {
			return true
		}
	}
	return false
}

func multiReviewFailureError(waitErr error) error {
	if waitErr != nil {
		return fmt.Errorf("review run: %w", waitErr)
	}
	return errors.New("review run: all reviewers failed")
}

// maybePostReviewToTrail delivers the final review output to the branch's trail
// when the profile selects the "trail" destination. It never fails the run:
// the review already happened, so a posting error (or a missing hook) is
// surfaced as a notice and the local output stands.
func maybePostReviewToTrail(
	ctx context.Context,
	out io.Writer,
	deps Deps,
	outputMode, profileName string,
	summary reviewtypes.RunSummary,
	aggregateOutput string,
) {
	if outputMode != ReviewOutputTrail || summary.Cancelled {
		return
	}
	verdict := strings.TrimSpace(aggregateOutput)
	if verdict == "" {
		verdict = combinedReviewNarratives(summary)
	}
	if verdict == "" {
		fmt.Fprintln(out, "Nothing to report, so nothing was posted to the trail.")
		return
	}
	if deps.PostReviewToTrail == nil {
		fmt.Fprintln(out, "Trail output is not available here; the review was kept local.")
		return
	}
	// On success the hook prints its own confirmation and trail link.
	if err := deps.PostReviewToTrail(ctx, out, profileName, verdict); err != nil {
		if !userMessageAlreadyPrinted(err) {
			fmt.Fprintf(out, "Could not post the review to the trail: %v\n", err)
		}
	}
}

type alreadyPrintedError interface {
	AlreadyPrinted() bool
}

func userMessageAlreadyPrinted(err error) bool {
	var printed alreadyPrintedError
	return errors.As(err, &printed) && printed.AlreadyPrinted()
}

// combinedReviewNarratives joins the reviewers' narratives into one document,
// used as the trail-posting body for single-reviewer runs (which have no
// synthesized verdict) and as a fallback when synthesis produced nothing.
func combinedReviewNarratives(summary reviewtypes.RunSummary) string {
	var b strings.Builder
	for _, run := range usableAgentRuns(summary) {
		narrative := joinAssistantText(run.Buffer)
		if narrative == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "## %s\n\n%s", run.Name, narrative)
	}
	return strings.TrimSpace(b.String())
}

func writePostReviewManifest(
	ctx context.Context,
	out io.Writer,
	worktreeRoot string,
	headSHA string,
	summary reviewtypes.RunSummary,
	aggregateOutput string,
) {
	if summary.Cancelled || len(summary.AgentRuns) == 0 {
		return
	}

	// Detach from the run context: a Ctrl+C during a slow finalize cancels ctx
	// after the workers finished, and must not discard their findings.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	manifest, states, err := localReviewManifestFromCurrentState(ctx, worktreeRoot, headSHA, summary, aggregateOutput)
	if err != nil {
		logging.Debug(ctx, "review manifest not written", slog.String("error", err.Error()))
		warnManifestNotWritten(out, "could not load session state: "+err.Error())
		return
	}
	if len(manifest.Sources) == 0 {
		reason, sentinel := explainEmptyManifest(worktreeRoot, headSHA, summary, states)
		if sentinel {
			// Matcher and explainer have drifted — the matcher rejected
			// every tagged session for a reason none of the explainer's
			// filters cover. Surface at Warn so this gets noticed without
			// requiring debug logging.
			logging.Warn(ctx, "review manifest matcher/explainer drift detected",
				slog.String("reason", reason),
				slog.Int("tagged_state_count", len(states)),
				slog.Int("agent_run_count", len(summary.AgentRuns)))
		} else {
			logging.Debug(ctx, "review manifest not written: no matching review sessions",
				slog.String("reason", reason))
		}
		warnManifestNotWritten(out, reason)
		return
	}
	if err := writeLocalReviewManifest(ctx, manifest); err != nil {
		logging.Debug(ctx, "review manifest write failed", slog.String("error", err.Error()))
		warnManifestNotWritten(out, "write to disk failed: "+err.Error())
		return
	}
	writeReviewCompletionFooter(out, manifest)
}

// warnManifestNotWritten prints a user-visible note explaining that the
// review skills ran but findings were not persisted, so `entire review
// --findings` will not see this run. The reason string is appended verbatim
// and should describe the underlying cause in terms the user can act on (or at
// least diagnose with debug logs).
func warnManifestNotWritten(out io.Writer, reason string) {
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Note: review skills ran but findings were not persisted.")
	fmt.Fprintf(out, "  Reason: %s\n", reason)
	fmt.Fprintln(out, "  `entire review --findings` will not see this run.")
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
	return reviewAgentRunTokenEnricherForRuns(worktreeRoot, headSHA, nil)
}

func reviewAgentRunTokenEnricherForRuns(worktreeRoot, headSHA string, planned []reviewtypes.AgentRun) func(context.Context, reviewtypes.AgentRun) reviewtypes.AgentRun {
	var mu sync.Mutex
	usedSessions := map[string]bool{}
	claimedPlan := make([]bool, len(planned))
	planned = slices.Clone(planned)
	runStartedAt := time.Now()
	return func(ctx context.Context, run reviewtypes.AgentRun) reviewtypes.AgentRun {
		mu.Lock()
		defer mu.Unlock()

		store, err := session.NewStateStore(ctx)
		if err != nil {
			logging.Debug(ctx, "review agent token hydration skipped", slog.String("error", fmt.Errorf("create session state store: %w", err).Error()))
			return run
		}
		states, err := store.List(ctx)
		if err != nil {
			logging.Debug(ctx, "review agent token hydration skipped", slog.String("error", fmt.Errorf("list session states: %w", err).Error()))
			return run
		}

		if enriched, ok, sessionID := hydrateReviewAgentRunTokensFromStatesWithPlan(ctx, worktreeRoot, headSHA, run, states, agent.GetByAgentType, planned, runStartedAt, claimedPlan); ok {
			if sessionID != "" {
				usedSessions[sessionID] = true
			}
			return enriched
		}
		return hydrateReviewAgentRunTokensFromStatesWithUsed(ctx, worktreeRoot, headSHA, run, states, agent.GetByAgentType, usedSessions)
	}
}

func composeSingleAgentSinks(in singleAgentSinkInputs) []reviewtypes.Sink {
	if !in.isTTY || !in.canPrompt {
		fmt.Fprintf(in.out, "Running review with %s...\n", in.agentName)
		return []reviewtypes.Sink{DumpSink{W: in.out}}
	}
	tui := NewTUISink([]string{in.agentName}, in.cancelRun, in.out, os.Stdin)
	postRunOut := &bytes.Buffer{}
	return []reviewtypes.Sink{
		tui,
		DumpSink{W: postRunOut},
		tuiPostRunCompleteSink{tui: tui, buf: postRunOut, out: in.out},
	}
}

func runConfigWithReviewConfig(base reviewtypes.RunConfig, cfg settings.ReviewConfig) reviewtypes.RunConfig {
	applyReviewConfig(&base, cfg)
	return base
}

func applyReviewConfig(runCfg *reviewtypes.RunConfig, cfg settings.ReviewConfig) {
	runCfg.Model = strings.TrimSpace(cfg.Model)
	runCfg.Skills = cfg.Skills
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
	name  string
	inner reviewtypes.AgentReviewer
	cfg   reviewtypes.RunConfig
}

func (r *perAgentConfiguredReviewer) Name() string {
	if strings.TrimSpace(r.name) != "" {
		return strings.TrimSpace(r.name)
	}
	return r.inner.Name()
}
func (r *perAgentConfiguredReviewer) ActualAgentName() string { return r.inner.Name() }
func (r *perAgentConfiguredReviewer) ModelName() string       { return strings.TrimSpace(r.cfg.Model) }
func (r *perAgentConfiguredReviewer) Start(ctx context.Context, _ reviewtypes.RunConfig) (reviewtypes.Process, error) {
	return r.inner.Start(ctx, r.cfg) //nolint:wrapcheck // transparent adapter; callers see inner's error type directly
}

// Compile-time interface check.
var _ reviewtypes.AgentReviewer = (*perAgentConfiguredReviewer)(nil)

// currentHeadSHA returns the current HEAD commit hash as a 40-char hex string.
func currentHeadSHA(ctx context.Context, repoRoot string) (string, error) {
	return gitexec.HeadSHA(ctx, repoRoot) //nolint:wrapcheck // gitexec already wraps
}
