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
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Strategy display names for user-friendly selection
const (
	strategyDisplayManualCommit = "manual-commit"
	strategyDisplayAutoCommit   = "auto-commit"
)

// Config path display strings
const (
	configDisplayProject = ".entire/settings.json"
	configDisplayLocal   = ".entire/settings.local.json"
)

// strategyDisplayToInternal maps user-friendly names to internal strategy names
var strategyDisplayToInternal = map[string]string{
	strategyDisplayManualCommit: strategy.StrategyNameManualCommit,
	strategyDisplayAutoCommit:   strategy.StrategyNameAutoCommit,
}

// strategyInternalToDisplay maps internal strategy names to user-friendly names
var strategyInternalToDisplay = map[string]string{
	strategy.StrategyNameManualCommit: strategyDisplayManualCommit,
	strategy.StrategyNameAutoCommit:   strategyDisplayAutoCommit,
}

func newEnableCmd() *cobra.Command {
	var localDev bool
	var ignoreUntracked bool
	var useLocalSettings bool
	var useProjectSettings bool
	var agentName string
	var strategyFlag string
	var forceHooks bool
	var skipPushSessions bool
	var telemetry bool

	cmd := &cobra.Command{
		Use:   "enable",
		Short: "Enable Entire in current project",
		Long: `Enable Entire with session tracking for your AI agent workflows.

Uses the manual-commit strategy by default. To use a different strategy:

  entire enable --strategy auto-commit

Strategies: manual-commit (default), auto-commit`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Check if we're in a git repository first - this is a prerequisite error,
			// not a usage error, so we silence Cobra's output and use SilentError
			// to prevent duplicate error output in main.go
			if _, err := paths.RepoRoot(); err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "Not a git repository. Please run 'entire enable' from within a git repository.")
				return NewSilentError(errors.New("not a git repository"))
			}

			if err := validateSetupFlags(useLocalSettings, useProjectSettings); err != nil {
				return err
			}

			// Warn if repo has no commits yet
			if repo, err := strategy.OpenRepository(); err == nil && strategy.IsEmptyRepository(repo) {
				fmt.Fprintln(cmd.OutOrStdout(), "Note: This repository has no commits yet. Entire will be configured, but")
				fmt.Fprintln(cmd.OutOrStdout(), "session checkpoints won't work until you create your first commit.")
				fmt.Fprintln(cmd.OutOrStdout())
			}

			// Non-interactive mode if --agent flag is provided
			if cmd.Flags().Changed("agent") && agentName == "" {
				printMissingAgentError(cmd.ErrOrStderr())
				return NewSilentError(errors.New("missing agent name"))
			}

			if agentName != "" {
				ag, err := agent.Get(agent.AgentName(agentName))
				if err != nil {
					printWrongAgentError(cmd.ErrOrStderr(), agentName)
					return NewSilentError(errors.New("wrong agent name"))
				}
				// --agent is a targeted operation: set up this specific agent without
				// affecting other agents. Unlike the interactive path, it does not
				// uninstall hooks for other previously-enabled agents.
				return setupAgentHooksNonInteractive(cmd.OutOrStdout(), ag, strategyFlag, localDev, forceHooks, skipPushSessions, telemetry)
			}
			// Detect or prompt for agents
			agents, err := detectOrSelectAgent(cmd.OutOrStdout(), nil)
			if err != nil {
				return fmt.Errorf("agent selection failed: %w", err)
			}

			if strategyFlag != "" {
				return runEnableWithStrategy(cmd.OutOrStdout(), agents, strategyFlag, localDev, useLocalSettings, useProjectSettings, forceHooks, skipPushSessions, telemetry)
			}
			return runEnableInteractive(cmd.OutOrStdout(), agents, localDev, useLocalSettings, useProjectSettings, forceHooks, skipPushSessions, telemetry)
		},
	}

	cmd.Flags().BoolVar(&localDev, "local-dev", false, "Use go run instead of entire binary for hooks")
	cmd.Flags().MarkHidden("local-dev") //nolint:errcheck,gosec // flag is defined above
	cmd.Flags().BoolVar(&ignoreUntracked, "ignore-untracked", false, "Commit all new files without tracking pre-existing untracked files")
	cmd.Flags().MarkHidden("ignore-untracked") //nolint:errcheck,gosec // flag is defined above
	cmd.Flags().BoolVar(&useLocalSettings, "local", false, "Write settings to .entire/settings.local.json instead of .entire/settings.json")
	cmd.Flags().BoolVar(&useProjectSettings, "project", false, "Write settings to .entire/settings.json even if it already exists")
	cmd.Flags().StringVar(&agentName, "agent", "", "Agent to setup hooks for (e.g., claude-code). Enables non-interactive mode.")
	cmd.Flags().StringVar(&strategyFlag, "strategy", "", "Strategy to use (manual-commit or auto-commit)")
	cmd.Flags().BoolVarP(&forceHooks, "force", "f", false, "Force reinstall hooks (removes existing Entire hooks first)")
	cmd.Flags().BoolVar(&skipPushSessions, "skip-push-sessions", false, "Disable automatic pushing of session logs on git push")
	cmd.Flags().BoolVar(&telemetry, "telemetry", true, "Enable anonymous usage analytics")
	//nolint:errcheck,gosec // completion is optional, flag is defined above
	cmd.RegisterFlagCompletionFunc("strategy", func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
		return []string{strategyDisplayManualCommit, strategyDisplayAutoCommit}, cobra.ShellCompDirectiveNoFileComp
	})

	// Provide a helpful error when --agent is used without a value
	defaultFlagErr := cmd.FlagErrorFunc()
	cmd.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		var valErr *pflag.ValueRequiredError
		if errors.As(err, &valErr) && valErr.GetSpecifiedName() == "agent" {
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
		Short: "Disable Entire in current project",
		Long: `Disable Entire integrations in the current project.

By default, this command will disable Entire. Hooks will exit silently and commands will
show a disabled message.

To completely remove Entire integrations from this repository, use --uninstall:
  - .entire/ directory (settings, logs, metadata)
  - Git hooks (prepare-commit-msg, commit-msg, post-commit, pre-push)
  - Session state files (.git/entire-sessions/)
  - Shadow branches (entire/<hash>)
  - Agent hooks`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if uninstall {
				return runUninstall(cmd.OutOrStdout(), cmd.ErrOrStderr(), force)
			}
			return runDisable(cmd.OutOrStdout(), useProjectSettings)
		},
	}

	cmd.Flags().BoolVar(&useProjectSettings, "project", false, "Update .entire/settings.json instead of .entire/settings.local.json")
	cmd.Flags().BoolVar(&uninstall, "uninstall", false, "Completely remove Entire from this repository")
	cmd.Flags().BoolVar(&force, "force", false, "Skip confirmation prompt (use with --uninstall)")

	return cmd
}

// runEnableWithStrategy enables Entire with a specified strategy (non-interactive).
// The selectedStrategy can be either a display name (manual-commit, auto-commit)
// or an internal name (manual-commit, auto-commit).
// agents must be provided by the caller (via detectOrSelectAgent).
func runEnableWithStrategy(w io.Writer, agents []agent.Agent, selectedStrategy string, localDev, useLocalSettings, useProjectSettings, forceHooks, skipPushSessions, telemetry bool) error {
	// Map the strategy to internal name if it's a display name
	internalStrategy := selectedStrategy
	if mapped, ok := strategyDisplayToInternal[selectedStrategy]; ok {
		internalStrategy = mapped
	}

	// Validate the strategy exists
	strat, err := strategy.Get(internalStrategy)
	if err != nil {
		return fmt.Errorf("unknown strategy: %s (use manual-commit or auto-commit)", selectedStrategy)
	}

	// Uninstall hooks for agents that were previously active but are no longer selected
	if err := uninstallDeselectedAgentHooks(w, agents); err != nil {
		return fmt.Errorf("failed to clean up deselected agents: %w", err)
	}

	// Setup agent hooks for all selected agents
	for _, ag := range agents {
		if _, err := setupAgentHooks(ag, localDev, forceHooks); err != nil {
			return fmt.Errorf("failed to setup %s hooks: %w", ag.Type(), err)
		}
	}

	// Setup .entire directory
	if _, err := setupEntireDirectory(); err != nil {
		return fmt.Errorf("failed to setup .entire directory: %w", err)
	}

	// Load existing settings to preserve other options (like strategy_options.push)
	settings, err := LoadEntireSettings()
	if err != nil {
		// If we can't load, start with defaults
		settings = &EntireSettings{}
	}
	// Update the specific fields
	settings.Strategy = internalStrategy
	settings.LocalDev = localDev
	settings.Enabled = true

	// Set push_sessions option if --skip-push-sessions flag was provided
	if skipPushSessions {
		if settings.StrategyOptions == nil {
			settings.StrategyOptions = make(map[string]interface{})
		}
		settings.StrategyOptions["push_sessions"] = false
	}

	// Handle telemetry for non-interactive mode
	// Note: if telemetry is nil (not configured), it defaults to disabled
	if !telemetry || os.Getenv("ENTIRE_TELEMETRY_OPTOUT") != "" {
		f := false
		settings.Telemetry = &f
	}

	// Determine which settings file to write to
	entireDirAbs, err := paths.AbsPath(paths.EntireDir)
	if err != nil {
		entireDirAbs = paths.EntireDir // Fallback to relative
	}
	shouldUseLocal, showNotification := determineSettingsTarget(entireDirAbs, useLocalSettings, useProjectSettings)

	if showNotification {
		fmt.Fprintln(w, "Info: Project settings exist. Saving to settings.local.json instead.")
		fmt.Fprintln(w, "  Use --project to update the project settings file.")
	}

	configDisplay := configDisplayProject
	if shouldUseLocal {
		if err := SaveEntireSettingsLocal(settings); err != nil {
			return fmt.Errorf("failed to save local settings: %w", err)
		}
		configDisplay = configDisplayLocal
	} else {
		if err := SaveEntireSettings(settings); err != nil {
			return fmt.Errorf("failed to save settings: %w", err)
		}
	}

	// Install git hooks AFTER saving settings (InstallGitHook reads local_dev from settings)
	if _, err := strategy.InstallGitHook(true); err != nil {
		return fmt.Errorf("failed to install git hooks: %w", err)
	}
	strategy.CheckAndWarnHookManagers(w)
	fmt.Fprintln(w, "✓ Hooks installed")
	fmt.Fprintf(w, "✓ Project configured (%s)\n", configDisplay)

	// Let the strategy handle its own setup requirements
	if err := strat.EnsureSetup(); err != nil {
		return fmt.Errorf("failed to setup strategy: %w", err)
	}

	fmt.Fprintln(w, "\nReady.")

	return nil
}

// runEnableInteractive runs the interactive enable flow.
// agents must be provided by the caller (via detectOrSelectAgent).
func runEnableInteractive(w io.Writer, agents []agent.Agent, localDev, useLocalSettings, useProjectSettings, forceHooks, skipPushSessions, telemetry bool) error {
	// Uninstall hooks for agents that were previously active but are no longer selected
	if err := uninstallDeselectedAgentHooks(w, agents); err != nil {
		return fmt.Errorf("failed to clean up deselected agents: %w", err)
	}

	// Setup agent hooks for all selected agents
	for _, ag := range agents {
		if _, err := setupAgentHooks(ag, localDev, forceHooks); err != nil {
			return fmt.Errorf("failed to setup %s hooks: %w", ag.Type(), err)
		}
	}

	// Setup .entire directory
	if _, err := setupEntireDirectory(); err != nil {
		return fmt.Errorf("failed to setup .entire directory: %w", err)
	}

	// Use the default strategy (manual-commit)
	internalStrategy := strategy.DefaultStrategyName

	// Load existing settings to preserve other options (like strategy_options.push)
	settings, err := LoadEntireSettings()
	if err != nil {
		// If we can't load, start with defaults
		settings = &EntireSettings{}
	}
	// Update the specific fields
	settings.Strategy = internalStrategy
	settings.LocalDev = localDev
	settings.Enabled = true

	// Set push_sessions option if --skip-push-sessions flag was provided
	if skipPushSessions {
		if settings.StrategyOptions == nil {
			settings.StrategyOptions = make(map[string]interface{})
		}
		settings.StrategyOptions["push_sessions"] = false
	}

	// Determine which settings file to write to
	// First run always creates settings.json (no prompt)
	entireDirAbs, err := paths.AbsPath(paths.EntireDir)
	if err != nil {
		entireDirAbs = paths.EntireDir // Fallback to relative
	}
	shouldUseLocal, showNotification := determineSettingsTarget(entireDirAbs, useLocalSettings, useProjectSettings)

	if showNotification {
		fmt.Fprintln(w, "Info: Project settings exist. Saving to settings.local.json instead.")
		fmt.Fprintln(w, "  Use --project to update the project settings file.")
	}

	// Helper to save settings to the appropriate file
	saveSettings := func() error {
		if shouldUseLocal {
			return SaveEntireSettingsLocal(settings)
		}
		return SaveEntireSettings(settings)
	}

	// Save settings before telemetry prompt so config is persisted even if the user cancels
	if err := saveSettings(); err != nil {
		return fmt.Errorf("failed to save settings: %w", err)
	}

	// Install git hooks AFTER saving settings (InstallGitHook reads local_dev from settings)
	if _, err := strategy.InstallGitHook(true); err != nil {
		return fmt.Errorf("failed to install git hooks: %w", err)
	}
	strategy.CheckAndWarnHookManagers(w)
	fmt.Fprintln(w, "✓ Hooks installed")

	configDisplay := configDisplayProject
	if shouldUseLocal {
		configDisplay = configDisplayLocal
	}
	fmt.Fprintf(w, "✓ Project configured (%s)\n", configDisplay)

	// Ask about telemetry consent (only if not already asked)
	fmt.Fprintln(w)
	if err := promptTelemetryConsent(settings, telemetry); err != nil {
		return fmt.Errorf("telemetry consent: %w", err)
	}
	// Save again to persist telemetry choice
	if err := saveSettings(); err != nil {
		return fmt.Errorf("failed to save settings: %w", err)
	}

	// Let the strategy handle its own setup requirements
	strat, err := strategy.Get(internalStrategy)
	if err != nil {
		return fmt.Errorf("failed to get strategy: %w", err)
	}
	if err := strat.EnsureSetup(); err != nil {
		return fmt.Errorf("failed to setup strategy: %w", err)
	}

	fmt.Fprintln(w, "\nReady.")

	return nil
}

// runEnable is a simple enable that just sets the enabled flag (for programmatic use).
func runEnable(w io.Writer) error {
	settings, err := LoadEntireSettings()
	if err != nil {
		return fmt.Errorf("failed to load settings: %w", err)
	}

	settings.Enabled = true
	if err := SaveEntireSettings(settings); err != nil {
		return fmt.Errorf("failed to save settings: %w", err)
	}

	fmt.Fprintln(w, "Entire is now enabled.")
	return nil
}

func runDisable(w io.Writer, useProjectSettings bool) error {
	settings, err := LoadEntireSettings()
	if err != nil {
		return fmt.Errorf("failed to load settings: %w", err)
	}

	settings.Enabled = false

	// If --project flag is specified, always write to project settings
	if useProjectSettings {
		if err := SaveEntireSettings(settings); err != nil {
			return fmt.Errorf("failed to save settings: %w", err)
		}
	} else {
		// Always write to local settings file (create if doesn't exist)
		if err := SaveEntireSettingsLocal(settings); err != nil {
			return fmt.Errorf("failed to save local settings: %w", err)
		}
	}

	fmt.Fprintln(w, "Entire is now disabled.")
	return nil
}

// DisabledMessage is the message shown when Entire is disabled
const DisabledMessage = "Entire is disabled. Run `entire enable` to re-enable."

// checkDisabledGuard checks if Entire is disabled and prints a message if so.
// Returns true if the caller should exit (i.e., Entire is disabled).
// On error reading settings, defaults to enabled (returns false).
func checkDisabledGuard(w io.Writer) bool {
	enabled, err := IsEnabled()
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
func uninstallDeselectedAgentHooks(w io.Writer, selectedAgents []agent.Agent) error {
	installedNames := GetAgentsWithHooksInstalled()
	if len(installedNames) == 0 {
		return nil
	}

	selectedSet := make(map[agent.AgentName]struct{}, len(selectedAgents))
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
		hookAgent, ok := ag.(agent.HookSupport)
		if !ok {
			continue
		}
		if err := hookAgent.UninstallHooks(); err != nil {
			errs = append(errs, fmt.Errorf("failed to uninstall %s hooks: %w", ag.Type(), err))
		} else {
			fmt.Fprintf(w, "Removed %s hooks\n", ag.Type())
		}
	}
	return errors.Join(errs...)
}

// setupAgentHooks sets up hooks for a given agent.
// Returns the number of hooks installed (0 if already installed).
func setupAgentHooks(ag agent.Agent, localDev, forceHooks bool) (int, error) { //nolint:unparam // return value used by setupAgentHooksNonInteractive
	hookAgent, ok := ag.(agent.HookSupport)
	if !ok {
		return 0, fmt.Errorf("agent %s does not support hooks", ag.Name())
	}

	count, err := hookAgent.InstallHooks(localDev, forceHooks)
	if err != nil {
		return 0, fmt.Errorf("failed to install %s hooks: %w", ag.Name(), err)
	}

	return count, nil
}

// detectOrSelectAgent tries to auto-detect agents, or prompts the user to select.
// Returns the detected/selected agents and any error.
//
// On first run (no hooks installed):
//   - Single detected agent: used automatically
//   - Multiple/no detected agents: interactive multi-select prompt
//
// On re-run (hooks already installed):
//   - Always shows the interactive multi-select
//   - Pre-selects only agents that have hooks installed (respects prior deselection)
//
// selectFn overrides the interactive prompt for testing. When nil, the real form is used.
// It receives available agent names and returns the selected names.
func detectOrSelectAgent(w io.Writer, selectFn func(available []string) ([]string, error)) ([]agent.Agent, error) {
	// Check for agents with hooks already installed (re-run detection)
	installedAgentNames := GetAgentsWithHooksInstalled()
	hasInstalledHooks := len(installedAgentNames) > 0

	// Try auto-detection
	detected := agent.DetectAll()

	// First run: use existing auto-detect shortcuts
	if !hasInstalledHooks {
		switch {
		case len(detected) == 1:
			fmt.Fprintf(w, "Detected agent: %s\n\n", detected[0].Type())
			return detected, nil

		case len(detected) > 1:
			agentTypes := make([]string, 0, len(detected))
			for _, ag := range detected {
				agentTypes = append(agentTypes, string(ag.Type()))
			}
			fmt.Fprintf(w, "Detected multiple agents: %s\n", strings.Join(agentTypes, ", "))
			fmt.Fprintln(w)
		}
	}

	// Check if we can prompt interactively
	if !canPromptInteractively() {
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

	if !hasInstalledHooks && len(detected) == 0 {
		fmt.Fprintln(w, "No agent configuration detected (e.g., .claude or .gemini directory).")
		fmt.Fprintln(w, "This is normal - some agents don't require a config directory.")
		fmt.Fprintln(w)
	}

	// Build pre-selection set.
	// On re-run: only pre-select agents with hooks installed (respect prior deselection).
	// On first run: pre-select all detected agents.
	preSelectedSet := make(map[agent.AgentName]struct{})
	if hasInstalledHooks {
		for _, name := range installedAgentNames {
			preSelectedSet[name] = struct{}{}
		}
	} else {
		for _, ag := range detected {
			preSelectedSet[ag.Name()] = struct{}{}
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
		if _, ok := ag.(agent.HookSupport); !ok {
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
					Title("Which agents are you using?").
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
		selectedAgent, err := agent.Get(agent.AgentName(name))
		if err != nil {
			return nil, fmt.Errorf("failed to get selected agent %s: %w", name, err)
		}
		selectedAgents = append(selectedAgents, selectedAgent)
	}

	agentTypes := make([]string, 0, len(selectedAgents))
	for _, ag := range selectedAgents {
		agentTypes = append(agentTypes, string(ag.Type()))
	}
	fmt.Fprintf(w, "\nSelected agents: %s\n\n", strings.Join(agentTypes, ", "))
	return selectedAgents, nil
}

// canPromptInteractively checks if we can show interactive prompts.
// Returns false when running in CI, tests, or other non-interactive environments.
func canPromptInteractively() bool {
	// Check for test environment
	if os.Getenv("ENTIRE_TEST_TTY") != "" {
		return os.Getenv("ENTIRE_TEST_TTY") == "1"
	}

	// Check if /dev/tty is available
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	_ = tty.Close()
	return true
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
func setupAgentHooksNonInteractive(w io.Writer, ag agent.Agent, strategyName string, localDev, forceHooks, skipPushSessions, telemetry bool) error {
	agentName := ag.Name()
	// Check if agent supports hooks
	hookAgent, ok := ag.(agent.HookSupport)
	if !ok {
		return fmt.Errorf("agent %s does not support hooks", agentName)
	}

	fmt.Fprintf(w, "Agent: %s\n\n", ag.Type())

	// Install agent hooks (agent hooks don't depend on settings)
	installedHooks, err := hookAgent.InstallHooks(localDev, forceHooks)
	if err != nil {
		return fmt.Errorf("failed to install hooks for %s: %w", agentName, err)
	}

	// Setup .entire directory
	if _, err := setupEntireDirectory(); err != nil {
		return fmt.Errorf("failed to setup .entire directory: %w", err)
	}

	// Load existing settings to preserve other options (like strategy_options.push)
	settings, err := LoadEntireSettings()
	if err != nil {
		// If we can't load, start with defaults
		settings = &EntireSettings{Strategy: strategy.DefaultStrategyName}
	}
	settings.Enabled = true
	if localDev {
		settings.LocalDev = localDev
	}

	// Set push_sessions option if --skip-push-sessions flag was provided
	if skipPushSessions {
		if settings.StrategyOptions == nil {
			settings.StrategyOptions = make(map[string]interface{})
		}
		settings.StrategyOptions["push_sessions"] = false
	}

	// Set strategy if provided
	if strategyName != "" {
		// Map display name to internal name if needed
		internalStrategy := strategyName
		if mapped, ok := strategyDisplayToInternal[strategyName]; ok {
			internalStrategy = mapped
		}
		// Validate the strategy exists
		if _, err := strategy.Get(internalStrategy); err != nil {
			return fmt.Errorf("unknown strategy: %s (use manual-commit or auto-commit)", strategyName)
		}
		settings.Strategy = internalStrategy
	}

	// Handle telemetry for non-interactive mode
	// Note: if telemetry is nil (not configured), it defaults to disabled
	if !telemetry || os.Getenv("ENTIRE_TELEMETRY_OPTOUT") != "" {
		f := false
		settings.Telemetry = &f
	}

	if err := SaveEntireSettings(settings); err != nil {
		return fmt.Errorf("failed to save settings: %w", err)
	}

	// Install git hooks AFTER saving settings (InstallGitHook reads local_dev from settings)
	if _, err := strategy.InstallGitHook(true); err != nil {
		return fmt.Errorf("failed to install git hooks: %w", err)
	}
	strategy.CheckAndWarnHookManagers(w)

	if installedHooks == 0 {
		msg := fmt.Sprintf("Hooks for %s already installed", ag.Description())
		if ag.IsPreview() {
			msg += " (Preview)"
		}
		fmt.Fprintf(w, "%s\n", msg)
	} else {
		msg := fmt.Sprintf("Installed %d hooks for %s", installedHooks, ag.Description())
		if ag.IsPreview() {
			msg += " (Preview)"
		}
		fmt.Fprintf(w, "%s\n", msg)
	}

	fmt.Fprintf(w, "✓ Project configured (%s)\n", configDisplayProject)

	// Let the strategy handle its own setup requirements (creates entire/checkpoints/v1 branch, etc.)
	strat, err := strategy.Get(settings.Strategy)
	if err != nil {
		return fmt.Errorf("failed to get strategy: %w", err)
	}
	if err := strat.EnsureSetup(); err != nil {
		return fmt.Errorf("failed to setup strategy: %w", err)
	}

	fmt.Fprintln(w, "\nReady.")

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
func setupEntireDirectory() (bool, error) { //nolint:unparam // already present in codebase
	// Get absolute path for the .entire directory
	entireDirAbs, err := paths.AbsPath(paths.EntireDir)
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
	if err := strategy.EnsureEntireGitignore(); err != nil {
		return false, fmt.Errorf("failed to setup .gitignore: %w", err)
	}

	return created, nil
}

// setupGitHook installs the prepare-commit-msg hook for context trailers.
func setupGitHook() error {
	// Use shared implementation from strategy package
	// The localDev setting is read from settings.json
	_, err := strategy.InstallGitHook(false) // not silent - show output during setup
	if err != nil {
		return fmt.Errorf("failed to install git hook: %w", err)
	}
	strategy.CheckAndWarnHookManagers(os.Stderr)
	return nil
}

// newSetupGitHookCmd creates the standalone git-hook setup command
func newSetupGitHookCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "git-hook",
		Short:  "Install git hook for session context trailers",
		Hidden: true, // Hidden as it's mainly for testing
		RunE: func(_ *cobra.Command, _ []string) error {
			return setupGitHook()
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

// runUninstall completely removes Entire from the repository.
func runUninstall(w, errW io.Writer, force bool) error {
	// Check if we're in a git repository
	if _, err := paths.RepoRoot(); err != nil {
		fmt.Fprintln(errW, "Not a git repository. Nothing to uninstall.")
		return NewSilentError(errors.New("not a git repository"))
	}

	// Gather counts for display
	sessionStateCount := countSessionStates()
	shadowBranchCount := countShadowBranches()
	gitHooksInstalled := strategy.IsGitHookInstalled()
	agentsWithInstalledHooks := GetAgentsWithHooksInstalled()
	entireDirExists := checkEntireDirExists()

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
	if err := removeAgentHooks(w); err != nil {
		fmt.Fprintf(errW, "Warning: failed to remove agent hooks: %v\n", err)
	}

	// 2. Remove git hooks
	removed, err := strategy.RemoveGitHook()
	if err != nil {
		fmt.Fprintf(errW, "Warning: failed to remove git hooks: %v\n", err)
	} else if removed > 0 {
		fmt.Fprintf(w, "  Removed git hooks (%d)\n", removed)
	}

	// 3. Remove session state files
	statesRemoved, err := removeAllSessionStates()
	if err != nil {
		fmt.Fprintf(errW, "Warning: failed to remove session states: %v\n", err)
	} else if statesRemoved > 0 {
		fmt.Fprintf(w, "  Removed session states (%d)\n", statesRemoved)
	}

	// 4. Remove .entire/ directory
	if err := removeEntireDirectory(); err != nil {
		fmt.Fprintf(errW, "Warning: failed to remove .entire directory: %v\n", err)
	} else if entireDirExists {
		fmt.Fprintln(w, "  Removed .entire directory")
	}

	// 5. Remove shadow branches
	branchesRemoved, err := removeAllShadowBranches()
	if err != nil {
		fmt.Fprintf(errW, "Warning: failed to remove shadow branches: %v\n", err)
	} else if branchesRemoved > 0 {
		fmt.Fprintf(w, "  Removed %d shadow branches\n", branchesRemoved)
	}

	fmt.Fprintln(w, "\nEntire CLI uninstalled successfully.")
	return nil
}

// countSessionStates returns the number of active session state files.
func countSessionStates() int {
	store, err := session.NewStateStore()
	if err != nil {
		return 0
	}
	states, err := store.List(context.Background())
	if err != nil {
		return 0
	}
	return len(states)
}

// countShadowBranches returns the number of shadow branches.
func countShadowBranches() int {
	branches, err := strategy.ListShadowBranches()
	if err != nil {
		return 0
	}
	return len(branches)
}

// checkEntireDirExists checks if the .entire directory exists.
func checkEntireDirExists() bool {
	entireDirAbs, err := paths.AbsPath(paths.EntireDir)
	if err != nil {
		entireDirAbs = paths.EntireDir
	}
	_, err = os.Stat(entireDirAbs)
	return err == nil
}

// removeAgentHooks removes hooks from all agents that support hooks.
func removeAgentHooks(w io.Writer) error {
	var errs []error
	for _, name := range agent.List() {
		ag, err := agent.Get(name)
		if err != nil {
			continue
		}
		hs, ok := ag.(agent.HookSupport)
		if !ok {
			continue
		}
		wasInstalled := hs.AreHooksInstalled()
		if err := hs.UninstallHooks(); err != nil {
			errs = append(errs, err)
		} else if wasInstalled {
			fmt.Fprintf(w, "  Removed %s hooks\n", ag.Type())
		}
	}
	return errors.Join(errs...)
}

// removeAllSessionStates removes all session state files and the directory.
func removeAllSessionStates() (int, error) {
	store, err := session.NewStateStore()
	if err != nil {
		return 0, fmt.Errorf("failed to create state store: %w", err)
	}

	// Count states before removing
	states, err := store.List(context.Background())
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
func removeEntireDirectory() error {
	entireDirAbs, err := paths.AbsPath(paths.EntireDir)
	if err != nil {
		entireDirAbs = paths.EntireDir
	}
	if err := os.RemoveAll(entireDirAbs); err != nil {
		return fmt.Errorf("failed to remove .entire directory: %w", err)
	}
	return nil
}

// removeAllShadowBranches removes all shadow branches.
func removeAllShadowBranches() (int, error) {
	branches, err := strategy.ListShadowBranches()
	if err != nil {
		return 0, fmt.Errorf("failed to list shadow branches: %w", err)
	}
	if len(branches) == 0 {
		return 0, nil
	}
	deleted, _, err := strategy.DeleteShadowBranches(branches)
	return len(deleted), err
}
