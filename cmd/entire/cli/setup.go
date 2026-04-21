package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/external"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/vercelconfig"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Config path display strings
const (
	configDisplayProject = ".entire/settings.json"
	configDisplayLocal   = ".entire/settings.local.json"
)

// Flag names used across setup commands.
const (
	agentFlagName            = "agent"
	flagCheckpointRemote     = "checkpoint-remote"
	flagSkipPushSessions     = "skip-push-sessions"
	flagSummarizeModel       = "summarize-model"
	flagSummarizeAgent       = "summarize-provider"
	checkpointProviderGitHub = "github"
)

// EnableOptions holds the flags for `entire enable`.
type EnableOptions struct {
	LocalDev            bool
	UseLocalSettings    bool
	UseProjectSettings  bool
	ForceHooks          bool
	SkipPushSessions    bool
	CheckpointRemote    string
	Telemetry           bool
	AbsoluteGitHookPath bool
	// SuppressDoneMessage tells `runEnableInteractive` to skip its final
	// "Ready." line and the "commit the configuration files" hint. Set
	// when the caller is running the bootstrap flow, which takes over
	// presentation of the final state (commit, push, done).
	SuppressDoneMessage bool
	Yes                 bool
}

// applyStrategyOptions sets strategy_options on settings from CLI flags.
func (opts *EnableOptions) applyStrategyOptions(settings *EntireSettings) {
	if opts.SkipPushSessions {
		if settings.StrategyOptions == nil {
			settings.StrategyOptions = make(map[string]interface{})
		}
		settings.StrategyOptions["push_sessions"] = false
	}
	if opts.CheckpointRemote != "" {
		provider, repo, err := parseCheckpointRemoteFlag(opts.CheckpointRemote)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: invalid --checkpoint-remote format: %v\n", err)
		} else {
			if settings.StrategyOptions == nil {
				settings.StrategyOptions = make(map[string]interface{})
			}
			settings.StrategyOptions["checkpoint_remote"] = map[string]any{
				"provider": provider,
				"repo":     repo,
			}
		}
	}
}

func hasStrategyFlags(cmd *cobra.Command) bool {
	return cmd.Flags().Changed(flagCheckpointRemote) || cmd.Flags().Changed(flagSkipPushSessions)
}

func hasSummaryProviderFlags(cmd *cobra.Command) bool {
	return cmd.Flags().Changed(flagSummarizeAgent) || cmd.Flags().Changed(flagSummarizeModel)
}

// enableUsesSetupFlow reports whether `entire enable` should delegate to the
// setup/configure flow instead of the lightweight re-enable path.
// Bare `enable` and `enable --local/--project` remain state-toggle operations;
// any other setup-mutating flag should share configure's behavior.
func enableUsesSetupFlow(cmd *cobra.Command, agentName string) bool {
	if agentName != "" || hasStrategyFlags(cmd) {
		return true
	}

	return cmd.Flags().Changed("force") ||
		cmd.Flags().Changed("local-dev") ||
		cmd.Flags().Changed("absolute-git-hook-path") ||
		cmd.Flags().Changed("telemetry") ||
		cmd.Flags().Changed("yes")
}

func enableNeedsAgentManagement(cmd *cobra.Command) bool {
	return cmd.Flags().Changed("force") ||
		cmd.Flags().Changed("local-dev") ||
		cmd.Flags().Changed("absolute-git-hook-path") ||
		cmd.Flags().Changed("telemetry") ||
		cmd.Flags().Changed("yes")
}

// updateStrategyOptions applies strategy flags to settings without re-running agent setup.
// Loads and writes only the target file to avoid leaking settings between layers.
func updateStrategyOptions(ctx context.Context, w io.Writer, opts EnableOptions) error {
	// Validate before doing any I/O so we don't report "Settings updated" on bad input.
	if opts.CheckpointRemote != "" {
		if _, _, err := parseCheckpointRemoteFlag(opts.CheckpointRemote); err != nil {
			return fmt.Errorf("invalid --checkpoint-remote: %w", err)
		}
	}

	targetFile, configDisplay := settingsTargetFile(ctx, opts.UseLocalSettings, opts.UseProjectSettings)

	targetFileAbs, err := paths.AbsPath(ctx, targetFile)
	if err != nil {
		targetFileAbs = targetFile
	}

	s, err := settings.LoadFromFile(targetFileAbs)
	if err != nil {
		return fmt.Errorf("failed to load settings: %w", err)
	}

	opts.applyStrategyOptions(s)

	if targetFile == settings.EntireSettingsLocalFile {
		if err := SaveEntireSettingsLocal(ctx, s); err != nil {
			return fmt.Errorf("failed to save settings: %w", err)
		}
	} else {
		if err := SaveEntireSettings(ctx, s); err != nil {
			return fmt.Errorf("failed to save settings: %w", err)
		}
	}

	fmt.Fprintf(w, "✓ Settings updated (%s)\n", configDisplay)
	return nil
}

func updateSummaryGenerationSettings(ctx context.Context, w io.Writer, provider, model string, opts EnableOptions) error {
	if provider == "" && model == "" {
		return errors.New("at least one of --summarize-provider or --summarize-model must be set")
	}

	targetFile, configDisplay := settingsTargetFile(ctx, opts.UseLocalSettings, opts.UseProjectSettings)
	targetFileAbs, err := paths.AbsPath(ctx, targetFile)
	if err != nil {
		targetFileAbs = targetFile
	}

	s, err := settings.LoadFromFile(targetFileAbs)
	if err != nil {
		return fmt.Errorf("failed to load settings: %w", err)
	}
	if s.SummaryGeneration == nil {
		s.SummaryGeneration = &settings.SummaryGenerationSettings{}
	}

	if provider != "" {
		if err := validateSummaryProvider(provider); err != nil {
			return err
		}
	}
	if model != "" && provider == "" && s.SummaryGeneration.Provider == "" {
		// The target file alone has no provider, but the merged runtime
		// settings might (e.g. provider in project, model override in local).
		// Check the full merged view before rejecting.
		merged, mergeErr := settings.Load(ctx)
		if mergeErr != nil || merged.SummaryGeneration == nil || merged.SummaryGeneration.Provider == "" {
			return errors.New("--summarize-model requires an existing summary provider or --summarize-provider")
		}
	}

	s.SummaryGeneration.SetProvider(provider, model)

	if targetFile == settings.EntireSettingsLocalFile {
		if err := SaveEntireSettingsLocal(ctx, s); err != nil {
			return fmt.Errorf("failed to save settings: %w", err)
		}
	} else {
		if err := SaveEntireSettings(ctx, s); err != nil {
			return fmt.Errorf("failed to save settings: %w", err)
		}
	}

	fmt.Fprintf(w, "✓ Settings updated (%s)\n", configDisplay)
	return nil
}

// settingsTargetFile determines which settings file to write to based on flags
// and which files exist. Unlike determineSettingsTarget, this correctly handles
// local-only repos by checking for settings.local.json when settings.json is absent.
func settingsTargetFile(ctx context.Context, useLocal, useProject bool) (string, string) {
	if useLocal {
		return settings.EntireSettingsLocalFile, configDisplayLocal
	}
	if useProject {
		return settings.EntireSettingsFile, configDisplayProject
	}

	// No explicit flag — write to whichever file exists.
	// Check project file first, then local.
	projectAbs, err := paths.AbsPath(ctx, settings.EntireSettingsFile)
	if err == nil {
		if _, statErr := os.Stat(projectAbs); statErr == nil {
			return settings.EntireSettingsFile, configDisplayProject
		}
	}
	localAbs, err := paths.AbsPath(ctx, settings.EntireSettingsLocalFile)
	if err == nil {
		if _, statErr := os.Stat(localAbs); statErr == nil {
			return settings.EntireSettingsLocalFile, configDisplayLocal
		}
	}

	// Neither exists — default to project
	return settings.EntireSettingsFile, configDisplayProject
}

func saveSettingsToTarget(ctx context.Context, s *EntireSettings, targetFile string) error {
	switch targetFile {
	case settings.EntireSettingsLocalFile:
		return SaveEntireSettingsLocal(ctx, s)
	case settings.EntireSettingsFile:
		return SaveEntireSettings(ctx, s)
	default:
		return fmt.Errorf("unknown settings target %q", targetFile)
	}
}

// parseCheckpointRemoteFlag parses a "provider:owner/repo" string into its components.
// Supported providers: "github".
func parseCheckpointRemoteFlag(value string) (provider, repo string, err error) {
	parts := strings.SplitN(value, ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("expected format provider:owner/repo (e.g., github:org/checkpoints-repo), got %q", value)
	}

	provider = parts[0]
	repo = parts[1]

	switch provider {
	case checkpointProviderGitHub:
		// valid
	default:
		return "", "", fmt.Errorf("unsupported provider %q (supported: %s)", provider, checkpointProviderGitHub)
	}

	repoParts := strings.SplitN(repo, "/", 2)
	if len(repoParts) != 2 || repoParts[0] == "" || repoParts[1] == "" {
		return "", "", fmt.Errorf("repo must be in owner/name format, got %q", repo)
	}

	return provider, repo, nil
}

// runSetupFlow runs the first-time setup flow (agent selection + hooks + settings).
// Shared by root command (no args), `entire configure`, and `entire enable` on fresh repos.
func runSetupFlow(ctx context.Context, w io.Writer, opts EnableOptions) error {
	// Discover external agent plugins so they appear in agent selection.
	// Use DiscoverAndRegisterAlways to bypass the external_agents setting —
	// during setup the setting doesn't exist yet.
	external.DiscoverAndRegisterAlways(ctx)

	var selectFn func(available []string) ([]string, error)
	if opts.Yes {
		selectFn = selectAllAgents
	}

	agents, err := detectOrSelectAgent(ctx, w, selectFn)
	if err != nil {
		return fmt.Errorf("agent selection failed: %w", err)
	}

	return runEnableInteractive(ctx, w, agents, opts)
}

// selectAllAgents is a selectFn that selects all available agents.
// Used by --yes to skip the interactive agent selection prompt.
func selectAllAgents(available []string) ([]string, error) {
	if len(available) == 0 {
		return nil, errors.New("no agents available")
	}
	return available, nil
}

// runManageAgents shows which agents are currently enabled and lets the user
// add or remove agents. Deselecting an installed agent removes its hooks.
func runManageAgents(ctx context.Context, w io.Writer, opts EnableOptions, selectFn func(available []string) ([]string, error)) error {
	installedNames := GetAgentsWithHooksInstalled(ctx)

	// Show currently installed agents
	if len(installedNames) > 0 {
		displayNames := make([]string, 0, len(installedNames))
		for _, name := range installedNames {
			if ag, err := agent.Get(name); err == nil {
				displayNames = append(displayNames, string(ag.Type()))
			}
		}
		fmt.Fprintf(w, "Enabled agents: %s\n\n", strings.Join(displayNames, ", "))
	}

	// Build pre-selection set from installed agents
	installedSet := make(map[types.AgentName]struct{}, len(installedNames))
	for _, name := range installedNames {
		installedSet[name] = struct{}{}
	}

	// When no selectFn is provided, check if we can prompt interactively.
	// A selectFn (e.g. from --yes) bypasses the interactive prompt entirely.
	if selectFn == nil && !interactive.CanPromptInteractively() {
		fmt.Fprintln(w, "Cannot show agent selection in non-interactive mode.")
		fmt.Fprintln(w, "Use: entire configure --agent <name>")
		return nil
	}

	// Discover external agent plugins so they appear in agent selection.
	// Use DiscoverAndRegisterAlways to bypass the external_agents setting —
	// during setup the setting doesn't exist yet.
	external.DiscoverAndRegisterAlways(ctx)

	// Build options from registered agents
	agentNames := agent.List()
	options := make([]huh.Option[string], 0, len(agentNames))
	for _, name := range agentNames {
		ag, err := agent.Get(name)
		if err != nil {
			continue
		}
		if _, ok := agent.AsHookSupport(ag); !ok {
			continue
		}
		if to, ok := ag.(agent.TestOnly); ok && to.IsTestOnly() {
			continue
		}
		opt := huh.NewOption(string(ag.Type()), string(name))
		if _, installed := installedSet[name]; installed {
			opt = opt.Selected(true)
		}
		options = append(options, opt)
	}

	if len(options) == 0 {
		return errors.New("no agents with hook support available")
	}

	// Collect available agent names for the selector
	availableNames := make([]string, 0, len(options))
	for _, opt := range options {
		availableNames = append(availableNames, opt.Value)
	}

	var selectedAgentNames []string
	if selectFn != nil {
		var err error
		selectedAgentNames, err = selectFn(availableNames)
		if err != nil {
			return fmt.Errorf("agent selection cancelled: %w", err)
		}
	} else {
		form := NewAccessibleForm(
			huh.NewGroup(
				huh.NewMultiSelect[string]().
					Title("Manage agents").
					Description("Use space to select/deselect, enter to confirm.").
					Options(options...).
					Value(&selectedAgentNames),
			),
		)
		if err := form.Run(); err != nil {
			return fmt.Errorf("agent selection cancelled: %w", err)
		}
	}

	// Nothing selected and nothing installed — no-op.
	if len(selectedAgentNames) == 0 && len(installedNames) == 0 {
		targetFile, _ := settingsTargetFile(ctx, opts.UseLocalSettings, opts.UseProjectSettings)
		changed, err := maybePromptVercelDeploymentDisable(ctx, w, targetFile, nil)
		if err != nil {
			return err
		}
		if !changed {
			fmt.Fprintln(w, "No changes made.")
		}
		return nil
	}

	err := applyAgentChanges(ctx, w, selectedAgentNames, installedNames, opts)
	if err == nil && len(selectedAgentNames) == 0 {
		fmt.Fprintln(w, "To add agents again, run: entire configure --agent <name>")
	}
	return err
}

// applyAgentChanges computes added/removed agent sets from the selection and
// installs or uninstalls hooks accordingly.
func applyAgentChanges(ctx context.Context, w io.Writer, selectedAgentNames []string, installedNames []types.AgentName, opts EnableOptions) error {
	installedSet := make(map[types.AgentName]struct{}, len(installedNames))
	for _, name := range installedNames {
		installedSet[name] = struct{}{}
	}

	selectedSet := make(map[string]struct{}, len(selectedAgentNames))
	for _, name := range selectedAgentNames {
		selectedSet[name] = struct{}{}
	}

	// Collect errors so partial successes are visible to the user.
	var errs []error

	var addedAgents []agent.Agent
	var reinstalledAgents []agent.Agent
	for _, name := range selectedAgentNames {
		ag, err := agent.Get(types.AgentName(name))
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to get agent %s: %w", name, err))
			continue
		}
		if _, wasInstalled := installedSet[types.AgentName(name)]; wasInstalled {
			if opts.ForceHooks {
				reinstalledAgents = append(reinstalledAgents, ag)
			}
			continue
		}
		addedAgents = append(addedAgents, ag)
	}

	var removedAgents []agent.Agent
	for _, name := range installedNames {
		if _, stillSelected := selectedSet[string(name)]; stillSelected {
			continue
		}
		ag, err := agent.Get(name)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to load deselected agent %s: %w", name, err))
			continue
		}
		removedAgents = append(removedAgents, ag)
	}

	if len(addedAgents) == 0 && len(reinstalledAgents) == 0 && len(removedAgents) == 0 && len(errs) == 0 {
		targetFile, _ := settingsTargetFile(ctx, opts.UseLocalSettings, opts.UseProjectSettings)
		changed, err := maybePromptVercelDeploymentDisable(ctx, w, targetFile, nil)
		if err != nil {
			return err
		}
		if !changed {
			fmt.Fprintln(w, "No changes made.")
		}
		return nil
	}
	var successfullyAddedAgents []agent.Agent
	for _, ag := range addedAgents {
		if _, err := setupAgentHooks(ctx, w, ag, opts.LocalDev, opts.ForceHooks); err != nil {
			errs = append(errs, fmt.Errorf("failed to setup %s hooks: %w", ag.Type(), err))
		} else {
			successfullyAddedAgents = append(successfullyAddedAgents, ag)
		}
	}

	var successfullyReinstalledAgents []agent.Agent
	for _, ag := range reinstalledAgents {
		if _, err := setupAgentHooks(ctx, w, ag, opts.LocalDev, opts.ForceHooks); err != nil {
			errs = append(errs, fmt.Errorf("failed to setup %s hooks: %w", ag.Type(), err))
		} else {
			successfullyReinstalledAgents = append(successfullyReinstalledAgents, ag)
		}
	}

	var uninstalledAgents []agent.Agent
	for _, ag := range removedAgents {
		hookAgent, ok := agent.AsHookSupport(ag)
		if !ok {
			logging.Warn(ctx, "installed agent does not support hooks, skipping removal",
				"agent", string(ag.Name()))
			continue
		}
		if err := hookAgent.UninstallHooks(ctx); err != nil {
			errs = append(errs, fmt.Errorf("failed to remove %s hooks: %w", ag.Type(), err))
		} else {
			uninstalledAgents = append(uninstalledAgents, ag)
		}
	}

	// Auto-enable external_agents setting if any new agent is external.
	for _, ag := range append(successfullyAddedAgents, successfullyReinstalledAgents...) {
		if external.IsExternal(ag) {
			s, loadErr := LoadEntireSettings(ctx)
			if loadErr != nil {
				s = &EntireSettings{}
			}
			if !s.ExternalAgents {
				s.ExternalAgents = true
				var saveErr error
				if opts.UseLocalSettings {
					saveErr = SaveEntireSettingsLocal(ctx, s)
				} else {
					saveErr = SaveEntireSettings(ctx, s)
				}
				if saveErr != nil {
					errs = append(errs, fmt.Errorf("failed to save external_agents setting: %w", saveErr))
				}
			}
			break
		}
	}

	// Print summary of what succeeded
	if len(successfullyAddedAgents) > 0 {
		names := make([]string, 0, len(successfullyAddedAgents))
		for _, ag := range successfullyAddedAgents {
			names = append(names, string(ag.Type()))
		}
		fmt.Fprintf(w, "✓ Added agents: %s\n", strings.Join(names, ", "))
	}
	if len(successfullyReinstalledAgents) > 0 {
		names := make([]string, 0, len(successfullyReinstalledAgents))
		for _, ag := range successfullyReinstalledAgents {
			names = append(names, string(ag.Type()))
		}
		fmt.Fprintf(w, "✓ Reinstalled agents: %s\n", strings.Join(names, ", "))
	}
	if len(uninstalledAgents) > 0 {
		if len(successfullyAddedAgents) == 0 && len(successfullyReinstalledAgents) == 0 && len(addedAgents) == 0 && len(removedAgents) == len(installedNames) {
			fmt.Fprintln(w, "All agents have been removed.")
		} else {
			names := make([]string, 0, len(uninstalledAgents))
			for _, ag := range uninstalledAgents {
				names = append(names, string(ag.Type()))
			}
			fmt.Fprintf(w, "✓ Removed agents: %s\n", strings.Join(names, ", "))
		}
	}

	vercelSettingsTarget, _ := settingsTargetFile(ctx, opts.UseLocalSettings, opts.UseProjectSettings)
	if _, err := maybePromptVercelDeploymentDisable(ctx, w, vercelSettingsTarget, nil); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

func newSetupCmd() *cobra.Command {
	var opts EnableOptions
	var agentName string
	var removeAgentName string
	var summarizeProvider string
	var summarizeModel string

	cmd := &cobra.Command{
		Use:   "configure",
		Short: "Configure Entire in current repository",
		Long: `Configure Entire with session tracking for your AI agent workflows.

On first run, this configures Entire and installs agent hooks.
On subsequent runs, it lets you add or remove agents interactively.

Use --remove to remove a specific agent non-interactively:
  entire configure --remove claude-code`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if _, err := paths.WorktreeRoot(ctx); err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "Not a git repository. Please run 'entire configure' from within a git repository.")
				return NewSilentError(errors.New("not a git repository"))
			}

			// Discover external agent plugins early so they're available
			// for --agent, --remove, and interactive selection.
			// Use DiscoverAndRegisterAlways so that --agent works on fresh repos
			// where the external_agents setting hasn't been persisted yet.
			external.DiscoverAndRegisterAlways(ctx)

			// Remove agent mode
			if removeAgentName != "" {
				return runRemoveAgent(ctx, cmd.OutOrStdout(), removeAgentName)
			}

			// Non-interactive --agent mode
			if cmd.Flags().Changed(agentFlagName) && agentName == "" {
				printMissingAgentError(cmd.ErrOrStderr())
				return NewSilentError(errors.New("missing agent name"))
			}
			if agentName != "" {
				ag, err := agent.Get(types.AgentName(agentName))
				if err != nil {
					printWrongAgentError(cmd.ErrOrStderr(), agentName)
					return NewSilentError(errors.New("wrong agent name"))
				}
				return setupAgentHooksNonInteractive(ctx, cmd.OutOrStdout(), ag, opts)
			}

			// Settings-only mode: update strategy options / summary provider without agent selection
			if settings.IsSetUpAny(ctx) && (hasStrategyFlags(cmd) || hasSummaryProviderFlags(cmd)) {
				if hasStrategyFlags(cmd) {
					if err := updateStrategyOptions(ctx, cmd.OutOrStdout(), opts); err != nil {
						return err
					}
				}
				if hasSummaryProviderFlags(cmd) {
					if err := updateSummaryGenerationSettings(ctx, cmd.OutOrStdout(), summarizeProvider, summarizeModel, opts); err != nil {
						return err
					}
				}
				return nil
			}

			// If already set up, show agents and let user add more
			if settings.IsSetUpAny(ctx) {
				var selectFn func(available []string) ([]string, error)
				if opts.Yes {
					selectFn = selectAllAgents
				}
				return runManageAgents(ctx, cmd.OutOrStdout(), opts, selectFn)
			}

			// Fresh repo — run full setup flow
			return runSetupFlow(ctx, cmd.OutOrStdout(), opts)
		},
	}

	cmd.Flags().BoolVar(&opts.LocalDev, "local-dev", false, "Use go run instead of entire binary for hooks")
	cmd.Flags().MarkHidden("local-dev") //nolint:errcheck,gosec // flag is defined above
	cmd.Flags().BoolVar(&opts.UseLocalSettings, "local", false, "Write settings to .entire/settings.local.json instead of .entire/settings.json")
	cmd.Flags().BoolVar(&opts.UseProjectSettings, "project", false, "Write settings to .entire/settings.json even if it already exists")
	cmd.Flags().StringVar(&agentName, agentFlagName, "", "Enable a specific agent (e.g., "+strings.Join(agent.StringList(), ", ")+"; external agents on $PATH are also available)")
	cmd.Flags().StringVar(&removeAgentName, "remove", "", "Remove a specific agent's hooks (e.g., "+strings.Join(agent.StringList(), ", ")+")")
	cmd.Flags().BoolVarP(&opts.ForceHooks, "force", "f", false, "Force reinstall hooks (removes existing Entire hooks first)")
	cmd.Flags().BoolVar(&opts.SkipPushSessions, flagSkipPushSessions, false, "Disable automatic pushing of session logs on git push")
	cmd.Flags().StringVar(&opts.CheckpointRemote, flagCheckpointRemote, "", "Checkpoint remote in provider:owner/repo format (e.g., github:org/checkpoints-repo)")
	cmd.Flags().StringVar(&summarizeProvider, flagSummarizeAgent, "", "Set the provider used by explain --generate (e.g., claude-code, codex, gemini, cursor, copilot-cli)")
	cmd.Flags().StringVar(&summarizeModel, flagSummarizeModel, "", "Set the model hint used by explain --generate")
	cmd.Flags().BoolVar(&opts.Telemetry, "telemetry", true, "Enable anonymous usage analytics")
	cmd.Flags().BoolVar(&opts.AbsoluteGitHookPath, "absolute-git-hook-path", false, "Embed full binary path in git hooks (for GUI git clients that don't source shell profiles)")
	cmd.Flags().BoolVarP(&opts.Yes, "yes", "y", false, "Accept all defaults without prompting (in a non-repo directory: init git, create private GitHub repo, commit; then enable all agents and accept telemetry)")

	// Provide a helpful error when --agent is used without a value
	defaultFlagErr := cmd.FlagErrorFunc()
	cmd.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		var valErr *pflag.ValueRequiredError
		if errors.As(err, &valErr) && valErr.GetSpecifiedName() == agentFlagName {
			printMissingAgentError(c.ErrOrStderr())
			return NewSilentError(errors.New("missing agent name"))
		}
		return defaultFlagErr(c, err)
	})

	return cmd
}

func newEnableCmd() *cobra.Command {
	var opts EnableOptions
	var ignoreUntracked bool
	var agentName string
	var bootstrapOpts GitHubBootstrapOptions

	cmd := &cobra.Command{
		Use:   "enable",
		Short: "Enable Entire in current repository",
		Long: `Enable Entire with session tracking for your AI agent workflows.

If Entire is not yet configured, this runs the full configuration flow.
If Entire is already configured but disabled, this re-enables it.

If the current directory is not a git repository, Entire can initialize one
for you and (optionally) create a matching GitHub repository via the gh CLI.`,
		RunE: func(cmd *cobra.Command, _ []string) (runErr error) {
			ctx := cmd.Context()
			// Check if we're in a git repository first. If not, offer to
			// bootstrap one (git init + optional GitHub repo). If the user
			// declines, fall back to the legacy prerequisite error.
			//
			// The bootstrap runs in two phases: phase 1 (git init + identity
			// + gather GitHub choices) before agent setup, phase 2
			// (initial commit + gh repo create + push) after agent setup so
			// the initial commit captures the .entire/, .claude/, hooks, and
			// settings files that setup writes.
			var bootstrap *bootstrapState
			if _, err := paths.WorktreeRoot(ctx); err != nil {
				bootstrapOpts.Yes = opts.Yes
				state, bootstrapErr := runGitHubBootstrapInit(ctx, cmd.OutOrStdout(), cmd.ErrOrStderr(), bootstrapOpts)
				if errors.Is(bootstrapErr, errBootstrapDeclined) {
					fmt.Fprintln(cmd.ErrOrStderr(), "Not a git repository. Please run 'entire enable' from within a git repository, or pass --init-repo to initialize one here.")
					return NewSilentError(errors.New("not a git repository"))
				}
				if errors.Is(bootstrapErr, errBootstrapInterrupted) {
					fmt.Fprintln(cmd.ErrOrStderr(), "Bootstrap cancelled. A local git repository has been initialized but setup didn't complete. Run `entire enable` again to continue.")
					return NewSilentError(errors.New("bootstrap interrupted"))
				}
				if bootstrapErr != nil {
					return bootstrapErr
				}
				bootstrap = state
				// Let the enable flow know that we'll be handling the final
				// "done" summary from the bootstrap finalize step.
				opts.SuppressDoneMessage = true
				// Re-check after bootstrap.
				if _, err := paths.WorktreeRoot(ctx); err != nil {
					return fmt.Errorf("bootstrap finished but no git repository detected: %w", err)
				}
				// Visual separator between bootstrap init and agent setup.
				printBootstrapSection(cmd.OutOrStdout(), "Enabling Entire")
				// On the way out (if setup succeeded), create the initial
				// commit and push to the GitHub repo. If setup returned an
				// error, skip the finalize — the user can fix the issue and
				// re-run; any partial state is just untracked files.
				defer func() {
					if runErr != nil || bootstrap == nil {
						return
					}
					if err := runGitHubBootstrapFinalize(ctx, cmd.OutOrStdout(), bootstrap); err != nil {
						runErr = err
					}
				}()
			}

			if err := validateSetupFlags(opts.UseLocalSettings, opts.UseProjectSettings); err != nil {
				return err
			}

			// Discover external agent plugins early so --agent can find them.
			// Use DiscoverAndRegisterAlways so that --agent works on fresh repos
			// where the external_agents setting hasn't been persisted yet.
			external.DiscoverAndRegisterAlways(ctx)

			// Non-interactive mode if --agent flag is provided
			if cmd.Flags().Changed(agentFlagName) && agentName == "" {
				printMissingAgentError(cmd.ErrOrStderr())
				return NewSilentError(errors.New("missing agent name"))
			}

			if agentName != "" {
				ag, err := agent.Get(types.AgentName(agentName))
				if err != nil {
					printWrongAgentError(cmd.ErrOrStderr(), agentName)
					return NewSilentError(errors.New("wrong agent name"))
				}
				// --agent is a targeted operation: set up this specific agent without
				// affecting other agents. Unlike the interactive path, it does not
				// uninstall hooks for other previously-enabled agents.
				return setupAgentHooksNonInteractive(ctx, cmd.OutOrStdout(), ag, opts)
			}

			// Any setup-mutating flags should behave like `configure` on repos that
			// are already set up. Bare `enable` remains the lightweight re-enable path.
			if settings.IsSetUpAny(ctx) {
				usedSetupFlow := enableUsesSetupFlow(cmd, agentName)
				if usedSetupFlow {
					if hasStrategyFlags(cmd) {
						if err := updateStrategyOptions(ctx, cmd.OutOrStdout(), opts); err != nil {
							return err
						}
					}
					if enableNeedsAgentManagement(cmd) {
						var selectFn func(available []string) ([]string, error)
						if opts.Yes {
							selectFn = selectAllAgents
						}
						if err := runManageAgents(ctx, cmd.OutOrStdout(), opts, selectFn); err != nil {
							return err
						}
					}
				}

				enabled, err := IsEnabled(ctx)
				if err == nil && enabled {
					w := cmd.OutOrStdout()
					if !usedSetupFlow {
						fmt.Fprintln(w, "Entire is already enabled.")
					}
					printEnabledStatus(ctx, w)
					return nil
				}
				return runEnable(ctx, cmd.OutOrStdout(), opts.UseProjectSettings)
			}

			// Fresh repo — run full setup flow
			return runSetupFlow(ctx, cmd.OutOrStdout(), opts)
		},
	}

	cmd.Flags().BoolVar(&opts.LocalDev, "local-dev", false, "Use go run instead of entire binary for hooks")
	cmd.Flags().MarkHidden("local-dev") //nolint:errcheck,gosec // flag is defined above
	cmd.Flags().BoolVar(&ignoreUntracked, "ignore-untracked", false, "Commit all new files without tracking pre-existing untracked files")
	cmd.Flags().MarkHidden("ignore-untracked") //nolint:errcheck,gosec // flag is defined above
	cmd.Flags().BoolVar(&opts.UseLocalSettings, "local", false, "Write settings to .entire/settings.local.json instead of .entire/settings.json")
	cmd.Flags().BoolVar(&opts.UseProjectSettings, "project", false, "Write settings to .entire/settings.json even if it already exists")
	cmd.Flags().StringVar(&agentName, agentFlagName, "", "Agent to set up hooks for (e.g., "+strings.Join(agent.StringList(), ", ")+"; external agents on $PATH are also available). Enables non-interactive mode.")
	cmd.Flags().BoolVarP(&opts.ForceHooks, "force", "f", false, "Force reinstall hooks (removes existing Entire hooks first)")
	cmd.Flags().BoolVar(&opts.SkipPushSessions, flagSkipPushSessions, false, "Disable automatic pushing of session logs on git push")
	cmd.Flags().StringVar(&opts.CheckpointRemote, flagCheckpointRemote, "", "Checkpoint remote in provider:owner/repo format (e.g., github:org/checkpoints-repo)")
	cmd.Flags().BoolVar(&opts.Telemetry, "telemetry", true, "Enable anonymous usage analytics")
	cmd.Flags().BoolVar(&opts.AbsoluteGitHookPath, "absolute-git-hook-path", false, "Embed full binary path in git hooks (for GUI git clients that don't source shell profiles)")
	cmd.Flags().BoolVarP(&opts.Yes, "yes", "y", false, "Accept all defaults without prompting (in a non-repo directory: init git, create private GitHub repo, commit; then enable all agents and accept telemetry)")

	// Bootstrap flags for non-git-repo folders.
	cmd.Flags().BoolVar(&bootstrapOpts.InitRepo, "init-repo", false, "If not a git repo, initialize one non-interactively")
	cmd.Flags().BoolVar(&bootstrapOpts.NoInitRepo, "no-init-repo", false, "If not a git repo, exit instead of prompting to initialize one")
	cmd.Flags().StringVar(&bootstrapOpts.RepoName, "repo-name", "", "GitHub repository name for the new repo (used when bootstrapping)")
	cmd.Flags().StringVar(&bootstrapOpts.RepoOwner, "repo-owner", "", "GitHub user or organization login for the new repo")
	cmd.Flags().StringVar(&bootstrapOpts.RepoVisibility, "repo-visibility", "", "GitHub repository visibility: public, private, or internal")
	cmd.Flags().BoolVar(&bootstrapOpts.NoGitHub, "no-github", false, "Initialize local git repo only; skip creating a GitHub remote")
	cmd.Flags().StringVar(&bootstrapOpts.InitialCommitMessage, "initial-commit-message", "", "Commit message for the initial commit when bootstrapping a new repo")
	cmd.Flags().BoolVar(&bootstrapOpts.SkipInitialCommit, "skip-initial-commit", false, "Don't create the initial commit when bootstrapping a new repo")
	cmd.MarkFlagsMutuallyExclusive("init-repo", "no-init-repo")
	cmd.MarkFlagsMutuallyExclusive("initial-commit-message", "skip-initial-commit")

	// Provide a helpful error when --agent is used without a value
	defaultFlagErr := cmd.FlagErrorFunc()
	cmd.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		var valErr *pflag.ValueRequiredError
		if errors.As(err, &valErr) && valErr.GetSpecifiedName() == agentFlagName {
			printMissingAgentError(c.ErrOrStderr())
			return NewSilentError(errors.New("missing agent name"))
		}
		return defaultFlagErr(c, err)
	})

	// Add subcommands for automation/testing
	cmd.AddCommand(newSetupGitHookCmd())

	return cmd
}

func newDisableCmd() *cobra.Command {
	var useProjectSettings bool
	var uninstall bool
	var force bool

	cmd := &cobra.Command{
		Use:   "disable",
		Short: "Disable Entire in current repository",
		Long: `Disable Entire integrations in the current repository.

By default, this command will disable Entire. Hooks will exit silently and commands will
show a disabled message.

To completely remove Entire integrations from this repository, use --uninstall:
  - .entire/ directory (settings, logs, metadata)
  - Git hooks (prepare-commit-msg, commit-msg, post-commit, pre-push)
  - Session state files (.git/entire-sessions/)
  - Shadow branches (entire/<hash>)
  - Agent hooks`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if uninstall {
				return runUninstall(ctx, cmd.OutOrStdout(), cmd.ErrOrStderr(), force)
			}
			return runDisable(ctx, cmd.OutOrStdout(), useProjectSettings)
		},
	}

	cmd.Flags().BoolVar(&useProjectSettings, "project", false, "Update .entire/settings.json instead of .entire/settings.local.json")
	cmd.Flags().BoolVar(&uninstall, "uninstall", false, "Completely remove Entire from this repository")
	cmd.Flags().BoolVar(&force, "force", false, "Skip confirmation prompt (use with --uninstall)")

	return cmd
}

// runEnableInteractive runs the interactive enable flow.
// agents must be provided by the caller (via detectOrSelectAgent).
func runEnableInteractive(ctx context.Context, w io.Writer, agents []agent.Agent, opts EnableOptions) error {
	// Uninstall hooks for agents that were previously active but are no longer selected
	if err := uninstallDeselectedAgentHooks(ctx, w, agents); err != nil {
		return fmt.Errorf("failed to clean up deselected agents: %w", err)
	}

	// Setup agent hooks for all selected agents
	for _, ag := range agents {
		if _, err := setupAgentHooks(ctx, w, ag, opts.LocalDev, opts.ForceHooks); err != nil {
			return fmt.Errorf("failed to setup %s hooks: %w", ag.Type(), err)
		}
	}

	// Setup .entire directory
	if _, err := setupEntireDirectory(ctx); err != nil {
		return fmt.Errorf("failed to setup .entire directory: %w", err)
	}

	// Load existing settings to preserve other options (like strategy_options.push)
	settings, err := LoadEntireSettings(ctx)
	if err != nil {
		// If we can't load, start with defaults
		settings = &EntireSettings{}
	}
	// Update the specific fields
	settings.Enabled = true
	if opts.LocalDev {
		settings.LocalDev = true
	}
	if opts.AbsoluteGitHookPath {
		settings.AbsoluteGitHookPath = true
	}

	// Auto-enable external_agents if any selected agent is external.
	for _, ag := range agents {
		if external.IsExternal(ag) {
			settings.ExternalAgents = true
			break
		}
	}

	opts.applyStrategyOptions(settings)

	// Determine which settings file to write to
	// First run always creates settings.json (no prompt)
	entireDirAbs, err := paths.AbsPath(ctx, paths.EntireDir)
	if err != nil {
		entireDirAbs = paths.EntireDir // Fallback to relative
	}
	shouldUseLocal, showNotification := determineSettingsTarget(entireDirAbs, opts.UseLocalSettings, opts.UseProjectSettings)

	if showNotification {
		fmt.Fprintln(w, "Info: Project settings exist. Saving to settings.local.json instead.")
		fmt.Fprintln(w, "  Use --project to update the project settings file.")
	}

	// Save settings to the appropriate file.
	targetFile := EntireSettingsFile
	if shouldUseLocal {
		targetFile = EntireSettingsLocalFile
	}
	saveSettings := func() error {
		return saveSettingsToTarget(ctx, settings, targetFile)
	}
	if err := saveSettings(); err != nil {
		return fmt.Errorf("failed to save settings: %w", err)
	}

	// Use settings values (merged from existing config + flags) for hook installation
	// This ensures re-running `entire enable` without flags preserves existing settings
	if _, err := strategy.InstallGitHook(ctx, true, settings.LocalDev, settings.AbsoluteGitHookPath); err != nil {
		return fmt.Errorf("failed to install git hooks: %w", err)
	}
	strategy.CheckAndWarnHookManagers(ctx, w, settings.LocalDev, settings.AbsoluteGitHookPath)
	fmt.Fprintln(w, "  ✓ Installed hooks")

	configDisplay := configDisplayProject
	if shouldUseLocal {
		configDisplay = configDisplayLocal
	}
	fmt.Fprintln(w, "  ✓ Configured project")
	fmt.Fprintf(w, "    %s\n", configDisplay)

	var vercelPromptFn func() (bool, error)
	if opts.Yes {
		vercelPromptFn = func() (bool, error) { return true, nil }
	}
	if _, err := maybePromptVercelDeploymentDisable(ctx, w, targetFile, vercelPromptFn); err != nil {
		return err
	}

	// Ask about telemetry consent (only if not already asked).
	// --yes skips the interactive prompt but still respects --telemetry=false
	// and ENTIRE_TELEMETRY_OPTOUT — it only auto-answers the interactive question.
	if opts.Yes {
		if !opts.Telemetry || os.Getenv("ENTIRE_TELEMETRY_OPTOUT") != "" {
			f := false
			settings.Telemetry = &f
		} else if settings.Telemetry == nil {
			t := true
			settings.Telemetry = &t
		}
	} else if err := promptTelemetryConsent(settings, opts.Telemetry); err != nil {
		return fmt.Errorf("telemetry consent: %w", err)
	}
	// Save again to persist telemetry choice
	if err := saveSettings(); err != nil {
		return fmt.Errorf("failed to save settings: %w", err)
	}

	if err := strategy.EnsureSetup(ctx); err != nil {
		return fmt.Errorf("failed to setup strategy: %w", err)
	}

	if opts.SuppressDoneMessage {
		// Bootstrap finalize will print its own completion summary after
		// making the initial commit and pushing.
		return nil
	}

	fmt.Fprintln(w, "\nReady.")

	// Note about empty repos at the end, after setup is complete
	if repo, err := strategy.OpenRepository(ctx); err == nil && strategy.IsEmptyRepository(repo) {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Note: Session checkpoints require at least one commit. To get started,")
		fmt.Fprintln(w, "commit the configuration files (e.g. .entire/, .claude/).")
	}

	return nil
}

// printEnabledStatus prints agents and a hint about `entire configure`.
func printEnabledStatus(ctx context.Context, w io.Writer) {
	if displayNames := InstalledAgentDisplayNames(ctx); len(displayNames) > 0 {
		fmt.Fprintf(w, "Agents: %s\n", strings.Join(displayNames, ", "))
	}
	fmt.Fprintln(w, "\nTo add more agents, run `entire configure`.")
}

// runEnable sets the enabled flag in settings.
// Writes to the target file (local by default, project with --project),
// and also updates the other file if it exists, so they can't get out of sync.
func runEnable(ctx context.Context, w io.Writer, useProjectSettings bool) error {
	s, err := LoadEntireSettings(ctx)
	if err != nil {
		return fmt.Errorf("failed to load settings: %w", err)
	}

	s.Enabled = true

	if err := saveEnabledState(ctx, s, useProjectSettings); err != nil {
		return err
	}

	fmt.Fprintln(w, "Entire is now enabled.")
	printEnabledStatus(ctx, w)
	return nil
}

func runDisable(ctx context.Context, w io.Writer, useProjectSettings bool) error {
	s, err := LoadEntireSettings(ctx)
	if err != nil {
		return fmt.Errorf("failed to load settings: %w", err)
	}

	s.Enabled = false

	if err := saveEnabledState(ctx, s, useProjectSettings); err != nil {
		return err
	}

	fmt.Fprintln(w, "Entire is now disabled.")
	return nil
}

// saveEnabledState writes settings to the target file and also updates the
// other settings file if it exists, preventing local/project from getting
// out of sync on the enabled field.
func saveEnabledState(ctx context.Context, s *EntireSettings, useProjectSettings bool) error {
	if useProjectSettings {
		if err := SaveEntireSettings(ctx, s); err != nil {
			return fmt.Errorf("failed to save settings: %w", err)
		}
		// Also update local if it exists, so it doesn't override
		if localExists(ctx) {
			if err := SaveEntireSettingsLocal(ctx, s); err != nil {
				return fmt.Errorf("failed to save local settings: %w", err)
			}
		}
	} else {
		if err := SaveEntireSettingsLocal(ctx, s); err != nil {
			return fmt.Errorf("failed to save local settings: %w", err)
		}
	}
	return nil
}

// localExists checks if settings.local.json exists.
func localExists(ctx context.Context) bool {
	localFile := settings.EntireSettingsLocalFile
	if abs, err := paths.AbsPath(ctx, localFile); err == nil {
		localFile = abs
	}
	_, err := os.Stat(localFile)
	return err == nil
}

// runRemoveAgent removes hooks for a specific agent.
func runRemoveAgent(ctx context.Context, w io.Writer, name string) error {
	ag, err := agent.Get(types.AgentName(name))
	if err != nil {
		printWrongAgentError(w, name)
		return NewSilentError(errors.New("wrong agent name"))
	}

	hookAgent, ok := agent.AsHookSupport(ag)
	if !ok {
		return fmt.Errorf("agent %s does not support hooks", name)
	}

	if !hookAgent.AreHooksInstalled(ctx) {
		fmt.Fprintf(w, "%s hooks are not installed.\n", ag.Type())
		return nil
	}

	if err := hookAgent.UninstallHooks(ctx); err != nil {
		return fmt.Errorf("failed to remove %s hooks: %w", ag.Type(), err)
	}

	fmt.Fprintf(w, "Removed %s hooks.\n", ag.Type())
	return nil
}

// DisabledMessage is the message shown when Entire is disabled
const DisabledMessage = "Entire is disabled. Run `entire enable` to re-enable."

// checkDisabledGuard checks if Entire is disabled and prints a message if so.
// Returns true if the caller should exit (i.e., Entire is disabled).
// On error reading settings, defaults to enabled (returns false).
func checkDisabledGuard(ctx context.Context, w io.Writer) bool {
	enabled, err := IsEnabled(ctx)
	if err != nil {
		// Default to enabled on error
		return false
	}
	if !enabled {
		fmt.Fprintln(w, DisabledMessage)
		return true
	}
	return false
}

// uninstallDeselectedAgentHooks removes hooks for agents that were previously
// installed but are not in the selected list. This handles the case where a user
// re-runs `entire enable` and deselects an agent.
func uninstallDeselectedAgentHooks(ctx context.Context, w io.Writer, selectedAgents []agent.Agent) error {
	installedNames := GetAgentsWithHooksInstalled(ctx)
	if len(installedNames) == 0 {
		return nil
	}

	selectedSet := make(map[types.AgentName]struct{}, len(selectedAgents))
	for _, ag := range selectedAgents {
		selectedSet[ag.Name()] = struct{}{}
	}

	var errs []error
	for _, name := range installedNames {
		if _, selected := selectedSet[name]; selected {
			continue
		}
		ag, err := agent.Get(name)
		if err != nil {
			continue
		}
		hookAgent, ok := agent.AsHookSupport(ag)
		if !ok {
			continue
		}
		if err := hookAgent.UninstallHooks(ctx); err != nil {
			errs = append(errs, fmt.Errorf("failed to uninstall %s hooks: %w", ag.Type(), err))
		} else {
			fmt.Fprintf(w, "Removed %s hooks\n", ag.Type())
		}
	}
	return errors.Join(errs...)
}

// setupAgentHooks sets up hooks for a given agent.
// Returns the number of hooks installed (0 if already installed).
func setupAgentHooks(ctx context.Context, w io.Writer, ag agent.Agent, localDev, forceHooks bool) (int, error) {
	hookAgent, ok := agent.AsHookSupport(ag)
	if !ok {
		return 0, fmt.Errorf("agent %s does not support hooks", ag.Name())
	}

	count, err := hookAgent.InstallHooks(ctx, localDev, forceHooks)
	if err != nil {
		return 0, fmt.Errorf("failed to install %s hooks: %w", ag.Name(), err)
	}

	scaffoldResult, err := scaffoldSearchSubagent(ctx, ag)
	if err != nil {
		return 0, fmt.Errorf("failed to scaffold %s search subagent: %w", ag.Name(), err)
	}
	reportSearchSubagentScaffold(w, ag, scaffoldResult)

	return count, nil
}

// detectOrSelectAgent tries to auto-detect agents, or prompts the user to select.
// Returns the detected/selected agents and any error.
//
// On first run (no hooks installed):
//   - Single detected built-in agent: used automatically
//   - Single detected external agent: interactive multi-select prompt
//   - Multiple/no detected agents: interactive multi-select prompt
//
// On re-run (hooks already installed):
//   - Always shows the interactive multi-select
//   - Pre-selects only agents that have hooks installed (respects prior deselection)
//
// selectFn overrides the interactive prompt for testing. When nil, the real form is used.
// It receives available agent names and returns the selected names.
func detectOrSelectAgent(ctx context.Context, w io.Writer, selectFn func(available []string) ([]string, error)) ([]agent.Agent, error) {
	// Check for agents with hooks already installed (re-run detection)
	installedAgentNames := GetAgentsWithHooksInstalled(ctx)
	hasInstalledHooks := len(installedAgentNames) > 0

	// Try auto-detection
	detected := agent.DetectAll(ctx)

	// First run: use existing auto-detect shortcuts
	if !hasInstalledHooks {
		switch {
		case len(detected) == 1:
			if isBuiltInAgent(detected[0]) {
				// When a selectFn is provided (e.g. --yes), skip the single-agent
				// shortcut so the caller's selection logic runs instead.
				if selectFn == nil {
					fmt.Fprintf(w, "Detected agent: %s\n\n", detected[0].Type())
					return detected, nil
				}
			}

		case len(detected) > 1:
			agentTypes := make([]string, 0, len(detected))
			for _, ag := range detected {
				agentTypes = append(agentTypes, string(ag.Type()))
			}
			fmt.Fprintf(w, "Detected multiple agents: %s\n", strings.Join(agentTypes, ", "))
			fmt.Fprintln(w)
		}
	}

	// When no selectFn is provided, check if we can prompt interactively.
	// A selectFn (e.g. from --yes) bypasses the interactive prompt entirely.
	if selectFn == nil && !interactive.CanPromptInteractively() {
		if hasInstalledHooks {
			// Re-run without TTY — keep currently installed agents
			agents := make([]agent.Agent, 0, len(installedAgentNames))
			for _, name := range installedAgentNames {
				ag, err := agent.Get(name)
				if err != nil {
					continue
				}
				agents = append(agents, ag)
			}
			return agents, nil
		}
		if len(detected) > 0 {
			return detected, nil
		}
		defaultAgent := agent.Default()
		if defaultAgent == nil {
			return nil, errors.New("no default agent available")
		}
		fmt.Fprintf(w, "Agent: %s (use --agent to change)\n\n", defaultAgent.Type())
		return []agent.Agent{defaultAgent}, nil
	}

	// Build pre-selection set.
	// On re-run: only pre-select agents with hooks installed (respect prior deselection).
	// On first run: pre-select detected built-in agents only.
	preSelectedSet := make(map[types.AgentName]struct{})
	if hasInstalledHooks {
		for _, name := range installedAgentNames {
			preSelectedSet[name] = struct{}{}
		}
	} else {
		for _, ag := range detected {
			if isBuiltInAgent(ag) {
				preSelectedSet[ag.Name()] = struct{}{}
			}
		}
	}

	// Build options from registered agents
	agentNames := agent.List()
	options := make([]huh.Option[string], 0, len(agentNames))
	for _, name := range agentNames {
		ag, err := agent.Get(name)
		if err != nil {
			continue
		}
		// Only show agents that support hooks
		if _, ok := agent.AsHookSupport(ag); !ok {
			continue
		}
		// Skip test-only agents (e.g., Vogon canary)
		if to, ok := ag.(agent.TestOnly); ok && to.IsTestOnly() {
			continue
		}
		opt := huh.NewOption(string(ag.Type()), string(name))
		if _, isPreSelected := preSelectedSet[name]; isPreSelected {
			opt = opt.Selected(true)
		}
		options = append(options, opt)
	}

	if len(options) == 0 {
		return nil, errors.New("no agents with hook support available")
	}

	// Collect available agent names for the selector
	availableNames := make([]string, 0, len(options))
	for _, opt := range options {
		availableNames = append(availableNames, opt.Value)
	}

	var selectedAgentNames []string
	if selectFn != nil {
		var err error
		selectedAgentNames, err = selectFn(availableNames)
		if err != nil {
			return nil, err
		}
		if len(selectedAgentNames) == 0 {
			return nil, errors.New("no agents selected")
		}
	} else {
		form := NewAccessibleForm(
			huh.NewGroup(
				huh.NewMultiSelect[string]().
					Title("Select the agents you want to use").
					Description("Use space to select, enter to confirm.").
					Options(options...).
					Validate(func(selected []string) error {
						if len(selected) == 0 {
							return errors.New("please select at least one agent")
						}
						return nil
					}).
					Value(&selectedAgentNames),
			),
		)
		if err := form.Run(); err != nil {
			return nil, fmt.Errorf("agent selection cancelled: %w", err)
		}
	}

	selectedAgents := make([]agent.Agent, 0, len(selectedAgentNames))
	for _, name := range selectedAgentNames {
		selectedAgent, err := agent.Get(types.AgentName(name))
		if err != nil {
			return nil, fmt.Errorf("failed to get selected agent %s: %w", name, err)
		}
		selectedAgents = append(selectedAgents, selectedAgent)
	}

	agentTypes := make([]string, 0, len(selectedAgents))
	for _, ag := range selectedAgents {
		agentTypes = append(agentTypes, string(ag.Type()))
	}
	fmt.Fprintf(w, "  Selected agents: %s\n", strings.Join(agentTypes, ", "))
	return selectedAgents, nil
}

func isBuiltInAgent(ag agent.Agent) bool {
	return !external.IsExternal(ag)
}

// printAgentError writes an error message followed by available agents and usage.
func printAgentError(w io.Writer, message string) {
	agents := agent.List()
	fmt.Fprintf(w, "%s Available agents:\n", message)
	fmt.Fprintln(w)
	for _, a := range agents {
		suffix := ""
		if a == agent.DefaultAgentName {
			suffix = "    (default)"
		}
		fmt.Fprintf(w, "  %s%s\n", a, suffix)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage: entire enable --agent <agent-name>")
}

// printMissingAgentError writes a helpful error listing available agents.
func printMissingAgentError(w io.Writer) {
	printAgentError(w, "Missing agent name.")
}

// printWrongAgentError writes a helpful error when an unknown agent name is provided.
func printWrongAgentError(w io.Writer, name string) {
	printAgentError(w, fmt.Sprintf("Unknown agent %q.", name))
}

// setupAgentHooksNonInteractive sets up hooks for a specific agent non-interactively.
// If strategyName is provided, it sets the strategy; otherwise uses default.
func setupAgentHooksNonInteractive(ctx context.Context, w io.Writer, ag agent.Agent, opts EnableOptions) error {
	agentName := ag.Name()
	// Check if agent supports hooks
	if _, ok := agent.AsHookSupport(ag); !ok {
		return fmt.Errorf("agent %s does not support hooks", agentName)
	}

	fmt.Fprintf(w, "  Agent: %s\n", ag.Type())

	// Install agent hooks (agent hooks don't depend on settings)
	installedHooks, err := setupAgentHooks(ctx, w, ag, opts.LocalDev, opts.ForceHooks)
	if err != nil {
		return fmt.Errorf("failed to setup %s hooks: %w", agentName, err)
	}

	// Setup .entire directory
	if _, err := setupEntireDirectory(ctx); err != nil {
		return fmt.Errorf("failed to setup .entire directory: %w", err)
	}

	// Load existing settings to preserve other options (like strategy_options.push)
	settings, err := LoadEntireSettings(ctx)
	if err != nil {
		// If we can't load, start with defaults
		settings = &EntireSettings{}
	}
	settings.Enabled = true
	if opts.LocalDev {
		settings.LocalDev = true
	}
	if opts.AbsoluteGitHookPath {
		settings.AbsoluteGitHookPath = true
	}

	// Auto-enable external_agents setting if the agent is external.
	if external.IsExternal(ag) {
		settings.ExternalAgents = true
	}

	opts.applyStrategyOptions(settings)

	// Handle telemetry for non-interactive mode
	// Note: if telemetry is nil (not configured), it defaults to disabled
	if !opts.Telemetry || os.Getenv("ENTIRE_TELEMETRY_OPTOUT") != "" {
		f := false
		settings.Telemetry = &f
	}

	targetFile, configDisplay := settingsTargetFile(ctx, opts.UseLocalSettings, opts.UseProjectSettings)
	if err := saveSettingsToTarget(ctx, settings, targetFile); err != nil {
		return fmt.Errorf("failed to save settings: %w", err)
	}

	// Use settings values (merged from existing config + flags) for hook installation
	// This ensures re-running `entire enable --agent X` without flags preserves existing settings
	if _, err := strategy.InstallGitHook(ctx, true, settings.LocalDev, settings.AbsoluteGitHookPath); err != nil {
		return fmt.Errorf("failed to install git hooks: %w", err)
	}
	strategy.CheckAndWarnHookManagers(ctx, w, settings.LocalDev, settings.AbsoluteGitHookPath)

	if installedHooks == 0 {
		msg := fmt.Sprintf("Hooks for %s already installed", ag.Description())
		if ag.IsPreview() {
			msg += " (Preview)"
		}
		fmt.Fprintf(w, "  %s\n", msg)
	} else {
		msg := fmt.Sprintf("Installed %d hooks for %s", installedHooks, ag.Description())
		if ag.IsPreview() {
			msg += " (Preview)"
		}
		fmt.Fprintf(w, "  %s\n", msg)
	}

	fmt.Fprintln(w, "  ✓ Configured project")
	fmt.Fprintf(w, "    %s\n", configDisplay)

	if _, err := maybePromptVercelDeploymentDisable(ctx, w, targetFile, nil); err != nil {
		return err
	}

	if err := strategy.EnsureSetup(ctx); err != nil {
		return fmt.Errorf("failed to setup strategy: %w", err)
	}

	if opts.SuppressDoneMessage {
		// Bootstrap finalize will print its own completion summary.
		return nil
	}

	fmt.Fprintln(w, "\nReady.")

	if repo, err := strategy.OpenRepository(ctx); err == nil && strategy.IsEmptyRepository(repo) {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Note: Session checkpoints require at least one commit. To get started,")
		fmt.Fprintln(w, "commit the configuration files (e.g. .entire/, .claude/).")
	}

	return nil
}

// validateSetupFlags checks that --local and --project flags are not both specified.
func validateSetupFlags(useLocal, useProject bool) error {
	if useLocal && useProject {
		return errors.New("cannot specify both --project and --local")
	}
	return nil
}

// determineSettingsTarget decides whether to write to settings.local.json based on:
// - Whether settings.json already exists
// - The --local and --project flags
// Returns (useLocal, showNotification).
func determineSettingsTarget(entireDir string, useLocal, useProject bool) (bool, bool) {
	// Explicit --local flag always uses local settings
	if useLocal {
		return true, false
	}

	// Explicit --project flag always uses project settings
	if useProject {
		return false, false
	}

	// No flags specified - check if settings file exists
	settingsPath := filepath.Join(entireDir, paths.SettingsFileName)
	if _, err := os.Stat(settingsPath); err == nil {
		// Settings file exists - auto-redirect to local with notification
		return true, true
	}

	// Settings file doesn't exist - create it
	return false, false
}

// setupEntireDirectory creates the .entire directory and gitignore.
// Returns true if the directory was created, false if it already existed.
func setupEntireDirectory(ctx context.Context) (bool, error) { //nolint:unparam // already present in codebase
	// Get absolute path for the .entire directory
	entireDirAbs, err := paths.AbsPath(ctx, paths.EntireDir)
	if err != nil {
		entireDirAbs = paths.EntireDir // Fallback to relative
	}

	// Check if directory already exists
	created := false
	if _, err := os.Stat(entireDirAbs); os.IsNotExist(err) {
		created = true
	}

	// Create .entire directory
	//nolint:gosec // G301: Project directory needs standard permissions for git
	if err := os.MkdirAll(entireDirAbs, 0o755); err != nil {
		return false, fmt.Errorf("failed to create .entire directory: %w", err)
	}

	// Create/update .gitignore with all required entries
	if err := strategy.EnsureEntireGitignore(ctx); err != nil {
		return false, fmt.Errorf("failed to setup .gitignore: %w", err)
	}

	return created, nil
}

// setupGitHook installs the prepare-commit-msg hook for context trailers.
func setupGitHook(ctx context.Context) error {
	s, err := settings.Load(ctx)
	localDev := err == nil && s.LocalDev
	absoluteHookPath := err == nil && s.AbsoluteGitHookPath
	if _, err := strategy.InstallGitHook(ctx, false, localDev, absoluteHookPath); err != nil {
		return fmt.Errorf("failed to install git hook: %w", err)
	}
	strategy.CheckAndWarnHookManagers(ctx, os.Stderr, localDev, absoluteHookPath)
	return nil
}

// newSetupGitHookCmd creates the standalone git-hook setup command
func newSetupGitHookCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "git-hook",
		Short:  "Install git hook for session context trailers",
		Hidden: true, // Hidden as it's mainly for testing
		RunE: func(cmd *cobra.Command, _ []string) error {
			return setupGitHook(cmd.Context())
		},
	}

	return cmd
}

func newCurlBashPostInstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "curl-bash-post-install",
		Short:  "Post-install tasks for curl|bash installer",
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			w := cmd.OutOrStdout()
			if err := promptShellCompletion(w); err != nil {
				fmt.Fprintf(w, "Note: Shell completion setup skipped: %v\n", err)
			}
			return nil
		},
	}
}

// shellCompletionComment is the comment preceding the completion line
const shellCompletionComment = "# Entire CLI shell completion"

// errUnsupportedShell is returned when the user's shell is not supported for completion.
var errUnsupportedShell = errors.New("unsupported shell")

// shellCompletionTarget returns the rc file path and completion lines for the
// user's current shell.
func shellCompletionTarget() (shellName, rcFile, completionLine string, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", "", fmt.Errorf("cannot determine home directory: %w", err)
	}

	shell := os.Getenv("SHELL")
	switch {
	case strings.Contains(shell, "zsh"):
		return "Zsh",
			filepath.Join(home, ".zshrc"),
			"autoload -Uz compinit && compinit && source <(entire completion zsh)",
			nil
	case strings.Contains(shell, "bash"):
		bashRC := filepath.Join(home, ".bashrc")
		if _, err := os.Stat(filepath.Join(home, ".bash_profile")); err == nil {
			bashRC = filepath.Join(home, ".bash_profile")
		}
		return "Bash",
			bashRC,
			"source <(entire completion bash)",
			nil
	case strings.Contains(shell, "fish"):
		return "Fish",
			filepath.Join(home, ".config", "fish", "config.fish"),
			"entire completion fish | source",
			nil
	default:
		return "", "", "", errUnsupportedShell
	}
}

// promptShellCompletion offers to add shell completion to the user's rc file.
// Only prompts if completion is not already configured.
func promptShellCompletion(w io.Writer) error {
	shellName, rcFile, completionLine, err := shellCompletionTarget()
	if err != nil {
		if errors.Is(err, errUnsupportedShell) {
			fmt.Fprintf(w, "Note: Shell completion not available for your shell. Supported: zsh, bash, fish.\n")
			return nil
		}
		return fmt.Errorf("shell completion: %w", err)
	}

	if isCompletionConfigured(rcFile) {
		fmt.Fprintf(w, "✓ Shell completion already configured in %s\n", rcFile)
		return nil
	}

	var selected string
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title(fmt.Sprintf("Enable shell completion? (detected: %s)", shellName)).
				Options(
					huh.NewOption("Yes", "yes"),
					huh.NewOption("No", "no"),
				).
				Value(&selected),
		),
	)

	if err := form.Run(); err != nil {
		//nolint:nilerr // User cancelled - not a fatal error, just skip
		return nil
	}

	if selected != "yes" {
		return nil
	}

	if err := appendShellCompletion(rcFile, completionLine); err != nil {
		return fmt.Errorf("failed to update %s: %w", rcFile, err)
	}

	fmt.Fprintf(w, "✓ Shell completion added to %s\n", rcFile)
	fmt.Fprintln(w, "  Restart your shell to activate")

	return nil
}

// isCompletionConfigured checks if shell completion is already in the rc file.
func isCompletionConfigured(rcFile string) bool {
	//nolint:gosec // G304: rcFile is constructed from home dir + known filename, not user input
	content, err := os.ReadFile(rcFile)
	if err != nil {
		return false // File doesn't exist or can't read, treat as not configured
	}
	return strings.Contains(string(content), "entire completion")
}

// appendShellCompletion adds the completion line to the rc file.
func appendShellCompletion(rcFile, completionLine string) error {
	if err := os.MkdirAll(filepath.Dir(rcFile), 0o700); err != nil {
		return fmt.Errorf("creating directory: %w", err)
	}
	//nolint:gosec // G302: Shell rc files need 0644 for user readability
	f, err := os.OpenFile(rcFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("opening file: %w", err)
	}
	defer f.Close()

	_, err = f.WriteString("\n" + shellCompletionComment + "\n" + completionLine + "\n")
	if err != nil {
		return fmt.Errorf("writing completion: %w", err)
	}
	return nil
}

// promptTelemetryConsent asks the user if they want to enable telemetry.
// It modifies settings.Telemetry based on the user's choice or flags.
// The caller is responsible for saving settings.
func promptTelemetryConsent(settings *EntireSettings, telemetryFlag bool) error {
	// Handle --telemetry=false flag first (always overrides existing setting)
	if !telemetryFlag {
		f := false
		settings.Telemetry = &f
		return nil
	}

	// Skip if already asked
	if settings.Telemetry != nil {
		return nil
	}

	// Skip if env var disables telemetry (record as disabled)
	if os.Getenv("ENTIRE_TELEMETRY_OPTOUT") != "" {
		f := false
		settings.Telemetry = &f
		return nil
	}

	consent := true // Default to Yes
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title("Help improve Entire CLI?").
				Description("Share anonymous usage data. No code or personal info collected.").
				Affirmative("Yes").
				Negative("No").
				Value(&consent),
		),
	)

	if err := form.Run(); err != nil {
		return fmt.Errorf("telemetry prompt: %w", err)
	}

	settings.Telemetry = &consent
	return nil
}

func maybePromptVercelDeploymentDisable(ctx context.Context, w io.Writer, targetFile string, promptFn func() (bool, error)) (bool, error) {
	repoRoot, rootErr := paths.WorktreeRoot(ctx)
	if rootErr == nil {
		vercelJSONPath := filepath.Join(repoRoot, "vercel.json")
		hasVercelJSON := false
		if _, err := os.Stat(vercelJSONPath); err == nil {
			hasVercelJSON = true
		} else if !os.IsNotExist(err) {
			fmt.Fprintf(w, "Note: Skipping Vercel deployment update: could not check vercel.json: %v\n", err)
			return false, nil
		}

		hasVercelProject := hasVercelJSON
		if !hasVercelProject {
			for _, path := range []string{
				filepath.Join(repoRoot, ".vercel"),
				filepath.Join(repoRoot, "vercel.ts"),
			} {
				if _, err := os.Stat(path); err == nil {
					hasVercelProject = true
					break
				} else if !os.IsNotExist(err) {
					fmt.Fprintf(w, "Note: Skipping Vercel deployment update: could not check %s: %v\n", path, err)
					return false, nil
				}
			}
		}

		if !hasVercelProject {
			return false, nil
		}

		configDisplay := configDisplayProject
		if targetFile == settings.EntireSettingsLocalFile {
			configDisplay = configDisplayLocal
		}

		targetSettingsPath := filepath.Join(repoRoot, targetFile)
		targetSettings, err := settings.LoadFromFile(targetSettingsPath)
		if err != nil {
			return false, fmt.Errorf("load settings: %w", err)
		}
		if targetSettings.Vercel {
			return false, nil
		}

		if config, alreadyDisabled, loadErr := vercelconfig.Load(vercelJSONPath); loadErr == nil &&
			config != nil && alreadyDisabled {
			targetSettings.Vercel = true
			if err := saveSettingsToTarget(ctx, targetSettings, targetFile); err != nil {
				return false, fmt.Errorf("save settings: %w", err)
			}
			fmt.Fprintf(w, "✓ Updated %s to manage Vercel deployment blocking on `%s`\n", configDisplay, vercelconfig.BranchPattern)
			return true, nil
		}

		if promptFn == nil {
			if !interactive.CanPromptInteractively() {
				fmt.Fprintf(w, "Note: Vercel detected. Run `entire configure` interactively to disable deployments for `%s` branches.\n", vercelconfig.BranchPattern)
				return false, nil
			}
			promptFn = promptVercelDeploymentDisable
		}

		disableDeployments, err := promptFn()
		if err != nil {
			return false, fmt.Errorf("vercel prompt: %w", err)
		}
		if !disableDeployments {
			return false, nil
		}

		targetSettings.Vercel = true
		if err := saveSettingsToTarget(ctx, targetSettings, targetFile); err != nil {
			return false, fmt.Errorf("save settings: %w", err)
		}

		fmt.Fprintf(w, "✓ Updated %s to block Vercel deploys of Entire metadata branch\n", configDisplay)
		return true, nil
	}

	return false, nil
}

func promptVercelDeploymentDisable() (bool, error) {
	disableDeployments := true
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title("Disable Vercel deployments for Entire metadata branch?").
				Description("This automatically creates a vercel.json in the Entire metadata branch.").
				Affirmative("Yes").
				Negative("No").
				Value(&disableDeployments),
		),
	)

	if err := form.Run(); err != nil {
		return false, fmt.Errorf("run vercel deployment disable form: %w", err)
	}

	return disableDeployments, nil
}

// runUninstall completely removes Entire from the repository.
func runUninstall(ctx context.Context, w, errW io.Writer, force bool) error {
	// Check if we're in a git repository
	if _, err := paths.WorktreeRoot(ctx); err != nil {
		fmt.Fprintln(errW, "Not a git repository. Nothing to uninstall.")
		return NewSilentError(errors.New("not a git repository"))
	}

	// Gather counts for display
	sessionStateCount := countSessionStates(ctx)
	shadowBranchCount := countShadowBranches(ctx)
	gitHooksInstalled := strategy.IsGitHookInstalled(ctx)
	agentsWithInstalledHooks := GetAgentsWithHooksInstalled(ctx)
	entireDirExists := checkEntireDirExists(ctx)

	// Check if there's anything to uninstall
	if !entireDirExists && !gitHooksInstalled && sessionStateCount == 0 &&
		shadowBranchCount == 0 && len(agentsWithInstalledHooks) == 0 {
		fmt.Fprintln(w, "Entire is not installed in this repository.")
		return nil
	}

	// Show confirmation prompt unless --force
	if !force {
		fmt.Fprintln(w, "\nThis will completely remove Entire from this repository:")
		if entireDirExists {
			fmt.Fprintln(w, "  - .entire/ directory")
		}
		if gitHooksInstalled {
			fmt.Fprintln(w, "  - Git hooks (prepare-commit-msg, commit-msg, post-commit, pre-push)")
		}
		if sessionStateCount > 0 {
			fmt.Fprintf(w, "  - Session state files (%d)\n", sessionStateCount)
		}
		if shadowBranchCount > 0 {
			fmt.Fprintf(w, "  - Shadow branches (%d)\n", shadowBranchCount)
		}
		if len(agentsWithInstalledHooks) > 0 {
			displayNames := make([]string, 0, len(agentsWithInstalledHooks))
			for _, name := range agentsWithInstalledHooks {
				if ag, err := agent.Get(name); err == nil {
					displayNames = append(displayNames, string(ag.Type()))
				}
			}
			fmt.Fprintf(w, "  - Agent hooks (%s)\n", strings.Join(displayNames, ", "))
		}
		fmt.Fprintln(w)

		var confirmed bool
		form := NewAccessibleForm(
			huh.NewGroup(
				huh.NewConfirm().
					Title("Are you sure you want to uninstall Entire?").
					Affirmative("Yes, uninstall").
					Negative("Cancel").
					Value(&confirmed),
			),
		)

		if err := form.Run(); err != nil {
			return fmt.Errorf("confirmation cancelled: %w", err)
		}

		if !confirmed {
			fmt.Fprintln(w, "Uninstall cancelled.")
			return nil
		}
	}

	fmt.Fprintln(w, "\nUninstalling Entire CLI...")

	// 1. Remove agent hooks (lowest risk)
	if err := removeAgentHooks(ctx, w); err != nil {
		fmt.Fprintf(errW, "Warning: failed to remove agent hooks: %v\n", err)
	}

	// 2. Remove git hooks
	removed, err := strategy.RemoveGitHook(ctx)
	if err != nil {
		fmt.Fprintf(errW, "Warning: failed to remove git hooks: %v\n", err)
	} else if removed > 0 {
		fmt.Fprintf(w, "  Removed git hooks (%d)\n", removed)
	}

	// 3. Remove session state files
	statesRemoved, err := removeAllSessionStates(ctx)
	if err != nil {
		fmt.Fprintf(errW, "Warning: failed to remove session states: %v\n", err)
	} else if statesRemoved > 0 {
		fmt.Fprintf(w, "  Removed session states (%d)\n", statesRemoved)
	}

	// 4. Remove .entire/ directory
	if err := removeEntireDirectory(ctx); err != nil {
		fmt.Fprintf(errW, "Warning: failed to remove .entire directory: %v\n", err)
	} else if entireDirExists {
		fmt.Fprintln(w, "  Removed .entire directory")
	}

	// 5. Remove shadow branches
	branchesRemoved, err := removeAllShadowBranches(ctx)
	if err != nil {
		fmt.Fprintf(errW, "Warning: failed to remove shadow branches: %v\n", err)
	} else if branchesRemoved > 0 {
		fmt.Fprintf(w, "  Removed %d shadow branches\n", branchesRemoved)
	}

	fmt.Fprintln(w, "\nEntire CLI uninstalled successfully.")
	return nil
}

// countSessionStates returns the number of active session state files.
func countSessionStates(ctx context.Context) int {
	store, err := session.NewStateStore(ctx)
	if err != nil {
		return 0
	}
	states, err := store.List(ctx)
	if err != nil {
		return 0
	}
	return len(states)
}

// countShadowBranches returns the number of shadow branches.
func countShadowBranches(ctx context.Context) int {
	branches, err := strategy.ListShadowBranches(ctx)
	if err != nil {
		return 0
	}
	return len(branches)
}

// checkEntireDirExists checks if the .entire directory exists.
func checkEntireDirExists(ctx context.Context) bool {
	entireDirAbs, err := paths.AbsPath(ctx, paths.EntireDir)
	if err != nil {
		entireDirAbs = paths.EntireDir
	}
	_, err = os.Stat(entireDirAbs)
	return err == nil
}

// removeAgentHooks removes hooks from all agents that support hooks.
func removeAgentHooks(ctx context.Context, w io.Writer) error {
	var errs []error
	for _, name := range agent.List() {
		ag, err := agent.Get(name)
		if err != nil {
			continue
		}
		hs, ok := agent.AsHookSupport(ag)
		if !ok {
			continue
		}
		wasInstalled := hs.AreHooksInstalled(ctx)
		if err := hs.UninstallHooks(ctx); err != nil {
			errs = append(errs, err)
		} else if wasInstalled {
			fmt.Fprintf(w, "  Removed %s hooks\n", ag.Type())
		}
	}
	return errors.Join(errs...)
}

// removeAllSessionStates removes all session state files and the directory.
func removeAllSessionStates(ctx context.Context) (int, error) {
	store, err := session.NewStateStore(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to create state store: %w", err)
	}

	// Count states before removing
	states, err := store.List(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to list session states: %w", err)
	}
	count := len(states)

	// Remove the entire directory
	if err := store.RemoveAll(); err != nil {
		return 0, fmt.Errorf("failed to remove session states: %w", err)
	}

	return count, nil
}

// removeEntireDirectory removes the .entire directory.
func removeEntireDirectory(ctx context.Context) error {
	entireDirAbs, err := paths.AbsPath(ctx, paths.EntireDir)
	if err != nil {
		entireDirAbs = paths.EntireDir
	}
	if err := os.RemoveAll(entireDirAbs); err != nil {
		return fmt.Errorf("failed to remove .entire directory: %w", err)
	}
	return nil
}

// removeAllShadowBranches removes all shadow branches.
func removeAllShadowBranches(ctx context.Context) (int, error) {
	branches, err := strategy.ListShadowBranches(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to list shadow branches: %w", err)
	}
	if len(branches) == 0 {
		return 0, nil
	}
	deleted, _, err := strategy.DeleteShadowBranches(ctx, branches)
	return len(deleted), err
}
