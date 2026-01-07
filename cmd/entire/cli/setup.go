package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"entire.io/cli/cmd/entire/cli/agent"
	"entire.io/cli/cmd/entire/cli/paths"
	"entire.io/cli/cmd/entire/cli/strategy"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"
)

// Strategy display names for user-friendly selection
const (
	strategyDisplayManualCommit = "manual-commit"
	strategyDisplayAutoCommit   = "auto-commit"
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

	cmd := &cobra.Command{
		Use:   "enable",
		Short: "Enable Entire",
		Long:  "Enable Entire with interactive setup for session tracking mode",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateSetupFlags(useLocalSettings, useProjectSettings); err != nil {
				return err
			}
			// Non-interactive mode if --agent flag is provided
			if agentName != "" {
				return setupAgentHooksNonInteractive(agentName, strategyFlag, localDev, forceHooks)
			}
			// If strategy is specified via flag, skip interactive selection
			if strategyFlag != "" {
				return runEnableWithStrategy(cmd.OutOrStdout(), strategyFlag, localDev, ignoreUntracked, useLocalSettings, useProjectSettings, forceHooks)
			}
			return runEnableInteractive(cmd.OutOrStdout(), localDev, ignoreUntracked, useLocalSettings, useProjectSettings, forceHooks)
		},
	}

	cmd.Flags().BoolVar(&localDev, "local-dev", false, "Use go run instead of entire binary for hooks")
	cmd.Flags().MarkHidden("local-dev") //nolint:errcheck,gosec // flag is defined above
	cmd.Flags().BoolVar(&ignoreUntracked, "ignore-untracked", false, "Commit all new files without tracking pre-existing untracked files")
	cmd.Flags().MarkHidden("ignore-untracked") //nolint:errcheck,gosec // flag is defined above
	cmd.Flags().BoolVar(&useLocalSettings, "local", false, "Write settings to settings.local.json instead of settings.json")
	cmd.Flags().BoolVar(&useProjectSettings, "project", false, "Write settings to settings.json even if it already exists")
	cmd.Flags().StringVar(&agentName, "agent", "", "Agent to setup hooks for (e.g., claude-code). Enables non-interactive mode.")
	cmd.Flags().StringVar(&strategyFlag, "strategy", "", "Strategy to use (manual-commit or auto-commit)")
	cmd.Flags().BoolVarP(&forceHooks, "force", "f", false, "Force reinstall hooks (removes existing Entire hooks first)")
	//nolint:errcheck,gosec // completion is optional, flag is defined above
	cmd.RegisterFlagCompletionFunc("strategy", func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
		return []string{strategyDisplayManualCommit, strategyDisplayAutoCommit}, cobra.ShellCompDirectiveNoFileComp
	})

	// Add subcommands for automation/testing
	cmd.AddCommand(newSetupGitHookCmd())
	cmd.AddCommand(newSetupAgentHooksCmd())

	return cmd
}

// newSetupAgentHooksCmd creates a command to setup agent hooks non-interactively.
// This is primarily used for testing and automation.
func newSetupAgentHooksCmd() *cobra.Command {
	var localDev bool
	var agentName string
	var strategyFlag string
	var forceHooks bool

	cmd := &cobra.Command{
		Use:     "agent-hooks",
		Aliases: []string{"claude-hooks"}, // Backwards compatibility
		Short:   "Setup agent hooks (non-interactive)",
		Hidden:  true, // Hidden as it's mainly for testing
		RunE: func(_ *cobra.Command, _ []string) error {
			// Default to claude-code if no agent specified (backwards compat)
			if agentName == "" {
				agentName = agent.AgentNameClaudeCode
			}
			return setupAgentHooksNonInteractive(agentName, strategyFlag, localDev, forceHooks)
		},
	}

	cmd.Flags().BoolVar(&localDev, "local-dev", false, "Use go run instead of entire binary for hooks")
	_ = cmd.Flags().MarkHidden("local-dev") //nolint:errcheck // hidden flag for internal use
	cmd.Flags().StringVar(&agentName, "agent", "", "Agent to setup hooks for (default: claude-code)")
	cmd.Flags().StringVar(&strategyFlag, "strategy", "", "Strategy to use (manual-commit or auto-commit)")
	cmd.Flags().BoolVarP(&forceHooks, "force", "f", false, "Force reinstall hooks (removes existing Entire hooks first)")

	return cmd
}

func newDisableCmd() *cobra.Command {
	var useProjectSettings bool

	cmd := &cobra.Command{
		Use:   "disable",
		Short: "Disable Entire",
		Long:  "Disable Entire temporarily. Hooks will exit silently and commands will show a disabled message.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDisable(cmd.OutOrStdout(), useProjectSettings)
		},
	}

	cmd.Flags().BoolVar(&useProjectSettings, "project", false, "Update settings.json instead of settings.local.json")

	return cmd
}

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show Entire status",
		Long:  "Show whether Entire is currently enabled or disabled",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runStatus(cmd.OutOrStdout())
		},
	}
}

// runEnableWithStrategy enables Entire with a specified strategy (non-interactive).
// The selectedStrategy can be either a display name (manual-commit, auto-commit)
// or an internal name (manual-commit, auto-commit).
func runEnableWithStrategy(w io.Writer, selectedStrategy string, localDev, _, useLocalSettings, useProjectSettings, forceHooks bool) error {
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

	// Setup Claude Code hooks
	hooksInstalled, err := setupClaudeCodeHook(localDev, forceHooks)
	if err != nil {
		return fmt.Errorf("failed to setup Claude Code hooks: %w", err)
	}
	if hooksInstalled > 0 {
		fmt.Fprintln(w, "✓ Claude Code hooks installed")
	} else {
		fmt.Fprintln(w, "✓ Claude Code hooks verified")
	}

	// Setup .entire directory
	dirCreated, err := setupEntireDirectory()
	if err != nil {
		return fmt.Errorf("failed to setup .entire directory: %w", err)
	}
	if dirCreated {
		fmt.Fprintln(w, "✓ .entire directory created")
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

	if shouldUseLocal {
		if err := SaveEntireSettingsLocal(settings); err != nil {
			return fmt.Errorf("failed to save local settings: %w", err)
		}
		fmt.Fprintln(w, "✓ Local settings saved (.entire/settings.local.json)")
	} else {
		if err := SaveEntireSettings(settings); err != nil {
			return fmt.Errorf("failed to save settings: %w", err)
		}
		fmt.Fprintln(w, "✓ Project settings saved (.entire/settings.json)")
	}

	// Install git hooks (always reinstall to ensure they're up-to-date)
	gitHooksInstalled, err := strategy.InstallGitHook(true)
	if err != nil {
		return fmt.Errorf("failed to install git hooks: %w", err)
	}
	if gitHooksInstalled > 0 {
		fmt.Fprintln(w, "✓ Git hooks installed")
	} else {
		fmt.Fprintln(w, "✓ Git hooks verified")
	}

	// Let the strategy handle its own setup requirements
	if err := strat.EnsureSetup(); err != nil {
		return fmt.Errorf("failed to setup strategy: %w", err)
	}

	// Show success message with display name
	displayName := selectedStrategy
	if dn, ok := strategyInternalToDisplay[internalStrategy]; ok {
		displayName = dn
	}
	fmt.Fprintf(w, "\n✓ %s strategy enabled\n", displayName)

	return nil
}

// runEnableInteractive runs the interactive enable flow with strategy selection.
func runEnableInteractive(w io.Writer, localDev, _, useLocalSettings, useProjectSettings, forceHooks bool) error {
	// Build strategy options with user-friendly names
	var selectedStrategy string
	options := []huh.Option[string]{
		huh.NewOption(strategyDisplayManualCommit+"  Sessions are only captured when you commit", strategyDisplayManualCommit),
		huh.NewOption(strategyDisplayAutoCommit+"  Automatically capture sessions after agent response completion", strategyDisplayAutoCommit),
	}

	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Options(options...).
				Value(&selectedStrategy),
		),
	)

	if err := form.Run(); err != nil {
		return fmt.Errorf("selection cancelled: %w", err)
	}

	// Map display name to internal strategy name
	internalStrategy, ok := strategyDisplayToInternal[selectedStrategy]
	if !ok {
		return fmt.Errorf("unknown strategy: %s", selectedStrategy)
	}

	// Setup Claude Code hooks
	hooksInstalled, err := setupClaudeCodeHook(localDev, forceHooks)
	if err != nil {
		return fmt.Errorf("failed to setup Claude Code hooks: %w", err)
	}
	if hooksInstalled > 0 {
		fmt.Fprintln(w, "✓ Claude Code hooks installed")
	} else {
		fmt.Fprintln(w, "✓ Claude Code hooks verified")
	}

	// Setup .entire directory
	dirCreated, err := setupEntireDirectory()
	if err != nil {
		return fmt.Errorf("failed to setup .entire directory: %w", err)
	}
	if dirCreated {
		fmt.Fprintln(w, "✓ .entire directory created")
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

	// Determine which settings file to write to (interactive prompt if settings.json exists)
	entireDirAbs, err := paths.AbsPath(paths.EntireDir)
	if err != nil {
		entireDirAbs = paths.EntireDir // Fallback to relative
	}
	shouldUseLocal, err := promptSettingsTarget(entireDirAbs, useLocalSettings, useProjectSettings)
	if err != nil {
		return err
	}

	if shouldUseLocal {
		if err := SaveEntireSettingsLocal(settings); err != nil {
			return fmt.Errorf("failed to save local settings: %w", err)
		}
		fmt.Fprintln(w, "✓ Local settings saved (.entire/settings.local.json)")
	} else {
		if err := SaveEntireSettings(settings); err != nil {
			return fmt.Errorf("failed to save settings: %w", err)
		}
		fmt.Fprintln(w, "✓ Project settings saved (.entire/settings.json)")
	}

	// Install git hooks (always reinstall to ensure they're up-to-date)
	gitHooksInstalled, err := strategy.InstallGitHook(true)
	if err != nil {
		return fmt.Errorf("failed to install git hooks: %w", err)
	}
	if gitHooksInstalled > 0 {
		fmt.Fprintln(w, "✓ Git hooks installed")
	} else {
		fmt.Fprintln(w, "✓ Git hooks verified")
	}

	// Let the strategy handle its own setup requirements
	strat, err := strategy.Get(internalStrategy)
	if err != nil {
		return fmt.Errorf("failed to get strategy: %w", err)
	}
	if err := strat.EnsureSetup(); err != nil {
		return fmt.Errorf("failed to setup strategy: %w", err)
	}

	// Show success message with display name
	fmt.Fprintf(w, "\n✓ %s strategy enabled\n", selectedStrategy)

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
		// Check if local settings file exists - if so, write there
		localSettingsAbs, pathErr := paths.AbsPath(EntireSettingsLocalFile)
		if pathErr != nil {
			localSettingsAbs = EntireSettingsLocalFile
		}
		if _, statErr := os.Stat(localSettingsAbs); statErr == nil {
			// Local settings exists, write there
			if err := SaveEntireSettingsLocal(settings); err != nil {
				return fmt.Errorf("failed to save local settings: %w", err)
			}
		} else {
			// No local settings, write to project settings
			if err := SaveEntireSettings(settings); err != nil {
				return fmt.Errorf("failed to save settings: %w", err)
			}
		}
	}

	fmt.Fprintln(w, "Entire is now disabled.")
	return nil
}

func runStatus(w io.Writer) error {
	// Check if we're in a git repository
	if _, repoErr := paths.RepoRoot(); repoErr != nil {
		fmt.Fprintln(w, "✕ not a git repository")
		return nil //nolint:nilerr // Not being in a git repo is a valid status, not an error
	}

	// Check if Entire is set up in this repository by checking for settings file
	settingsPath, err := paths.AbsPath(EntireSettingsFile)
	if err != nil {
		// Can't determine path, fall back to relative
		settingsPath = EntireSettingsFile
	}

	if _, err := os.Stat(settingsPath); os.IsNotExist(err) {
		// Also check for local settings file
		localSettingsPath, pathErr := paths.AbsPath(EntireSettingsLocalFile)
		if pathErr != nil {
			localSettingsPath = EntireSettingsLocalFile
		}
		if _, localErr := os.Stat(localSettingsPath); os.IsNotExist(localErr) {
			fmt.Fprintln(w, "○ not set up (run `entire enable` to get started)")
			return nil
		}
	}

	settings, err := LoadEntireSettings()
	if err != nil {
		return fmt.Errorf("failed to check status: %w", err)
	}

	if settings.Enabled {
		// Use user-friendly display name if available
		displayName := settings.Strategy
		if dn, ok := strategyInternalToDisplay[settings.Strategy]; ok {
			displayName = dn
		}
		fmt.Fprintf(w, "● enabled (%s)\n", displayName)
	} else {
		fmt.Fprintln(w, "○ disabled")
	}
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

// setupClaudeCodeHook sets up Claude Code hooks.
// This is a convenience wrapper that uses the agent package.
// Returns the number of hooks installed (0 if already installed).
func setupClaudeCodeHook(localDev, forceHooks bool) (int, error) {
	ag, err := agent.Get(agent.AgentNameClaudeCode)
	if err != nil {
		return 0, fmt.Errorf("failed to get claude-code agent: %w", err)
	}

	hookAgent, ok := ag.(agent.HookSupport)
	if !ok {
		return 0, errors.New("claude-code agent does not support hooks")
	}

	count, err := hookAgent.InstallHooks(localDev, forceHooks)
	if err != nil {
		return 0, fmt.Errorf("failed to install claude-code hooks: %w", err)
	}

	return count, nil
}

// setupAgentHooksNonInteractive sets up hooks for a specific agent non-interactively.
// If strategyName is provided, it sets the strategy; otherwise uses default.
func setupAgentHooksNonInteractive(agentName, strategyName string, localDev, forceHooks bool) error {
	ag, err := agent.Get(agentName)
	if err != nil {
		return fmt.Errorf("unknown agent: %s", agentName)
	}

	// Check if agent supports hooks
	hookAgent, ok := ag.(agent.HookSupport)
	if !ok {
		return fmt.Errorf("agent %s does not support hooks", agentName)
	}

	// Install hooks
	count, err := hookAgent.InstallHooks(localDev, forceHooks)
	if err != nil {
		return fmt.Errorf("failed to install hooks for %s: %w", agentName, err)
	}

	if count == 0 {
		fmt.Printf("Hooks for %s already installed\n", ag.Description())
	} else {
		fmt.Printf("Installed %d hooks for %s\n", count, ag.Description())
	}

	// Update settings to store the agent choice and strategy
	settings, _ := LoadEntireSettings() //nolint:errcheck // settings defaults are fine
	settings.Agent = agentName
	settings.Enabled = true
	if localDev {
		settings.LocalDev = localDev
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

	if err := SaveEntireSettings(settings); err != nil {
		return fmt.Errorf("failed to save settings: %w", err)
	}

	// Install git hooks (always reinstall to ensure they're up-to-date)
	if _, err := strategy.InstallGitHook(true); err != nil {
		return fmt.Errorf("failed to install git hooks: %w", err)
	}

	// Let the strategy handle its own setup requirements (creates entire/sessions branch, etc.)
	strat, err := strategy.Get(settings.Strategy)
	if err != nil {
		return fmt.Errorf("failed to get strategy: %w", err)
	}
	if err := strat.EnsureSetup(); err != nil {
		return fmt.Errorf("failed to setup strategy: %w", err)
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

// Settings target options for interactive prompt
const (
	settingsTargetProject = "project"
	settingsTargetLocal   = "local"
)

// promptSettingsTarget interactively asks the user where to save settings
// when settings.json already exists and no flags were provided.
// Returns (useLocal, error).
func promptSettingsTarget(entireDir string, useLocal, useProject bool) (bool, error) {
	// Explicit --local flag always uses local settings
	if useLocal {
		return true, nil
	}

	// Explicit --project flag always uses project settings
	if useProject {
		return false, nil
	}

	// Check if settings file exists
	settingsPath := filepath.Join(entireDir, paths.SettingsFileName)
	if _, err := os.Stat(settingsPath); os.IsNotExist(err) {
		// Settings file doesn't exist - create it (no prompt needed)
		return false, nil
	}

	// Settings file exists - prompt user
	var selected string
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Project settings already exist. Where should settings be saved?").
				Options(
					huh.NewOption("Update project settings (settings.json)", settingsTargetProject),
					huh.NewOption("Use local settings (settings.local.json, gitignored)", settingsTargetLocal),
				).
				Value(&selected),
		),
	)

	if err := form.Run(); err != nil {
		return false, fmt.Errorf("selection cancelled: %w", err)
	}

	return selected == settingsTargetLocal, nil
}

// setupEntireDirectory creates the .entire directory and gitignore.
// Returns true if the directory was created, false if it already existed.
func setupEntireDirectory() (bool, error) {
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
