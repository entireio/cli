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
	var skipPushSessions bool
	var telemetry bool

	cmd := &cobra.Command{
		Use:   "enable",
		Short: "Enable Entire",
		Long: `Enable Entire with session tracking for your AI agent workflows.

Uses the manual-commit strategy by default. To use a different strategy:

  entire enable --strategy auto-commit

Strategies: manual-commit (default), auto-commit`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Check if we're in a git repository first - this is a prerequisite error,
			// not a usage error, so we silence Cobra's output and use SilentError
			// to prevent duplicate error output in main.go
			if _, err := paths.RepoRoot(); err != nil {
				cmd.SilenceUsage = true
				cmd.SilenceErrors = true
				fmt.Fprintln(cmd.ErrOrStderr(), "Not a git repository. Please run 'entire enable' from within a git repository.")
				return NewSilentError(errors.New("not a git repository"))
			}

			if err := validateSetupFlags(useLocalSettings, useProjectSettings); err != nil {
				return err
			}
			// Non-interactive mode if --agent flag is provided
			if agentName != "" {
				return setupAgentHooksNonInteractive(agent.AgentName(agentName), strategyFlag, localDev, forceHooks, skipPushSessions, telemetry)
			}
			// If strategy is specified via flag, skip interactive selection
			if strategyFlag != "" {
				return runEnableWithStrategy(cmd.OutOrStdout(), strategyFlag, localDev, ignoreUntracked, useLocalSettings, useProjectSettings, forceHooks, skipPushSessions, telemetry)
			}
			return runEnableInteractive(cmd.OutOrStdout(), localDev, ignoreUntracked, useLocalSettings, useProjectSettings, forceHooks, skipPushSessions, telemetry)
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
	cmd.Flags().BoolVar(&skipPushSessions, "skip-push-sessions", false, "Disable automatic pushing of session logs on git push")
	cmd.Flags().BoolVar(&telemetry, "telemetry", true, "Enable anonymous usage analytics")
	//nolint:errcheck,gosec // completion is optional, flag is defined above
	cmd.RegisterFlagCompletionFunc("strategy", func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
		return []string{strategyDisplayManualCommit, strategyDisplayAutoCommit}, cobra.ShellCompDirectiveNoFileComp
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
		Short: "Disable Entire",
		Long: `Disable Entire temporarily. Hooks will exit silently and commands will show a disabled message.

Use --uninstall to completely remove Entire from this repository, including:
  - .entire/ directory (settings, logs, metadata)
  - Git hooks (prepare-commit-msg, commit-msg, post-commit, pre-push)
  - Session state files (.git/entire-sessions/)
  - Shadow branches (entire/<hash>)
  - Agent hooks (Claude Code, Gemini CLI)`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if uninstall {
				return runUninstall(cmd.OutOrStdout(), cmd.ErrOrStderr(), force)
			}
			return runDisable(cmd.OutOrStdout(), useProjectSettings)
		},
	}

	cmd.Flags().BoolVar(&useProjectSettings, "project", false, "Update settings.json instead of settings.local.json")
	cmd.Flags().BoolVar(&uninstall, "uninstall", false, "Completely remove Entire from this repository")
	cmd.Flags().BoolVar(&force, "force", false, "Skip confirmation prompt (use with --uninstall)")

	return cmd
}

// runEnableWithStrategy enables Entire with a specified strategy (non-interactive).
// The selectedStrategy can be either a display name (manual-commit, auto-commit)
// or an internal name (manual-commit, auto-commit).
func runEnableWithStrategy(w io.Writer, selectedStrategy string, localDev, _, useLocalSettings, useProjectSettings, forceHooks, skipPushSessions, telemetry bool) error {
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

// runEnableInteractive runs the interactive enable flow.
func runEnableInteractive(w io.Writer, localDev, _, useLocalSettings, useProjectSettings, forceHooks, skipPushSessions, telemetry bool) error {
	// Use the default strategy (manual-commit)
	internalStrategy := strategy.DefaultStrategyName
	fmt.Fprintf(w, "Using %s strategy (use --strategy to change)\n\n", strategyInternalToDisplay[internalStrategy])

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

	// Set push_sessions option if --skip-push-sessions flag was provided
	if skipPushSessions {
		if settings.StrategyOptions == nil {
			settings.StrategyOptions = make(map[string]interface{})
		}
		settings.StrategyOptions["push_sessions"] = false
	}

	// Ask about telemetry consent (only if not already asked)
	if err := promptTelemetryConsent(settings, telemetry); err != nil {
		return fmt.Errorf("telemetry consent: %w", err)
	}

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
	fmt.Fprintf(w, "\n✓ %s strategy enabled\n", strategyInternalToDisplay[internalStrategy])

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
func setupAgentHooksNonInteractive(agentName agent.AgentName, strategyName string, localDev, forceHooks, skipPushSessions, telemetry bool) error {
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
		msg := fmt.Sprintf("Hooks for %s already installed", ag.Description())
		if agentName == agent.AgentNameGemini {
			msg += " - This is a work in progress"
		}
		fmt.Println(msg)
	} else {
		msg := fmt.Sprintf("Installed %d hooks for %s", count, ag.Description())
		if agentName == agent.AgentNameGemini {
			msg += " - This is a work in progress"
		}
		fmt.Println(msg)
	}

	// Update settings to store the strategy
	settings, _ := LoadEntireSettings() //nolint:errcheck // settings defaults are fine
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

// Shell completion stanza management.
// The stanza is a versioned, delimited block written to ~/.zshrc or ~/.bashrc:
//
//	# Entire CLI completions (v1) {{{
//	autoload -Uz compinit && compinit && source <(entire completion zsh)
//	# }}}
const (
	shellCompletionStanzaVersion = 1
	shellCompletionOpenFormat    = "# Entire CLI completions (v%d) {{{"
	shellCompletionClose         = "# }}}"
	shellCompletionLegacyComment = "# Entire CLI shell completion" // old format for migration
)

type stanzaStatus int

const (
	stanzaNotFound stanzaStatus = iota
	stanzaCurrent
	stanzaOutdated
	stanzaLegacy
)

var (
	errStanzaCorrupted = errors.New("shell completion block is corrupted (missing closing marker)")
	errMultipleStanzas = errors.New("found multiple shell completion blocks")
)

// findStanzaVersion scans content for the opening stanza marker and returns
// the version number. Returns 0 if no stanza is found.
func findStanzaVersion(content string) int {
	for line := range strings.SplitSeq(content, "\n") {
		trimmed := strings.TrimSpace(line)
		var version int
		if _, err := fmt.Sscanf(trimmed, shellCompletionOpenFormat, &version); err == nil {
			return version
		}
	}
	return 0
}

// checkStanzaStatus determines the status of the shell completion stanza in content.
func checkStanzaStatus(content string, currentVersion int) stanzaStatus {
	version := findStanzaVersion(content)
	if version > 0 {
		if version >= currentVersion {
			return stanzaCurrent
		}
		return stanzaOutdated
	}
	if strings.Contains(content, shellCompletionLegacyComment) {
		return stanzaLegacy
	}
	return stanzaNotFound
}

// buildStanza constructs the full stanza block for the given version and completion line.
func buildStanza(version int, completionLine string) string {
	return fmt.Sprintf(shellCompletionOpenFormat, version) + "\n" + completionLine + "\n" + shellCompletionClose
}

// countStanzaOpeners counts how many opening stanza markers exist in content.
func countStanzaOpeners(content string) int {
	count := 0
	for line := range strings.SplitSeq(content, "\n") {
		trimmed := strings.TrimSpace(line)
		var version int
		if _, err := fmt.Sscanf(trimmed, shellCompletionOpenFormat, &version); err == nil {
			count++
		}
	}
	return count
}

// replaceStanza finds the open+close markers in content and replaces the block
// with a new stanza. Returns errStanzaCorrupted if open found without close,
// errMultipleStanzas if count > 1. Returns unchanged content with nil error if
// no stanza found.
func replaceStanza(content string, version int, completionLine string) (string, error) {
	openerCount := countStanzaOpeners(content)
	if openerCount == 0 {
		return content, nil
	}
	if openerCount > 1 {
		return content, errMultipleStanzas
	}

	lines := strings.Split(content, "\n")
	openIdx := -1
	closeIdx := -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		var v int
		if _, err := fmt.Sscanf(trimmed, shellCompletionOpenFormat, &v); err == nil {
			openIdx = i
		}
		if openIdx >= 0 && trimmed == shellCompletionClose {
			closeIdx = i
			break
		}
	}

	if openIdx >= 0 && closeIdx < 0 {
		return content, errStanzaCorrupted
	}

	newStanza := buildStanza(version, completionLine)
	var result []string
	result = append(result, lines[:openIdx]...)
	result = append(result, newStanza)
	result = append(result, lines[closeIdx+1:]...)
	return strings.Join(result, "\n"), nil
}

// removeStanza finds and removes the stanza block from content.
// Returns errStanzaCorrupted if open found without close,
// errMultipleStanzas if count > 1. Returns unchanged content with nil error if
// no stanza found.
func removeStanza(content string) (string, error) {
	openerCount := countStanzaOpeners(content)
	if openerCount == 0 {
		return content, nil
	}
	if openerCount > 1 {
		return content, errMultipleStanzas
	}

	lines := strings.Split(content, "\n")
	openIdx := -1
	closeIdx := -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		var v int
		if _, err := fmt.Sscanf(trimmed, shellCompletionOpenFormat, &v); err == nil {
			openIdx = i
		}
		if openIdx >= 0 && trimmed == shellCompletionClose {
			closeIdx = i
			break
		}
	}

	if openIdx >= 0 && closeIdx < 0 {
		return content, errStanzaCorrupted
	}

	var result []string
	result = append(result, lines[:openIdx]...)
	// Skip blank line immediately after stanza if present
	tail := lines[closeIdx+1:]
	if len(tail) > 0 && strings.TrimSpace(tail[0]) == "" {
		tail = tail[1:]
	}
	result = append(result, tail...)
	return strings.Join(result, "\n"), nil
}

// removeLegacyCompletion removes the old-format comment and its following
// completion line from content. Returns the modified content and whether
// anything was removed.
func removeLegacyCompletion(content, completionLine string) (string, bool) {
	lines := strings.Split(content, "\n")
	var result []string
	removed := false
	for i := 0; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == shellCompletionLegacyComment {
			// Remove the comment line and the next line if it matches the completion line
			if i+1 < len(lines) && strings.TrimSpace(lines[i+1]) == completionLine {
				i++ // skip next line too
			}
			removed = true
			continue
		}
		result = append(result, lines[i])
	}
	if !removed {
		return content, false
	}
	return strings.Join(result, "\n"), true
}

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
			"source <(entire completion zsh)",
			nil
	case strings.Contains(shell, "bash"):
		return "Bash",
			filepath.Join(home, ".bashrc"),
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
// Handles versioned stanza detection, updates, and legacy migration.
func promptShellCompletion(w io.Writer) error {
	shellName, rcFile, completionLine, err := shellCompletionTarget()
	if err != nil {
		if errors.Is(err, errUnsupportedShell) {
			fmt.Fprintf(w, "Note: Shell completion not available for your shell. Supported: zsh, bash, fish.\n")
			return nil
		}
		return fmt.Errorf("shell completion: %w", err)
	}

	//nolint:gosec // G304: rcFile is constructed from home dir + known filename, not user input
	content, err := os.ReadFile(rcFile)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading %s: %w", rcFile, err)
	}

	status := checkStanzaStatus(string(content), shellCompletionStanzaVersion)
	switch status {
	case stanzaCurrent:
		fmt.Fprintf(w, "✓ Shell completion already configured in %s\n", rcFile)
		return nil

	case stanzaOutdated:
		if !promptYesNo("Shell completion is outdated. Update?") {
			return nil
		}
		// Re-read in case file changed
		//nolint:gosec // G304: rcFile is constructed from home dir + known filename
		content, err = os.ReadFile(rcFile)
		if err != nil {
			return fmt.Errorf("reading %s: %w", rcFile, err)
		}
		updated, replaceErr := replaceStanza(string(content), shellCompletionStanzaVersion, completionLine)
		if replaceErr != nil {
			fmt.Fprintf(w, "Warning: %v in %s. Please fix manually.\n", replaceErr, rcFile)
			return nil
		}
		//nolint:gosec // G306: Shell rc files need 0644 for user readability
		if err := os.WriteFile(rcFile, []byte(updated), 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", rcFile, err)
		}
		fmt.Fprintf(w, "✓ Shell completion updated in %s\n", rcFile)
		fmt.Fprintln(w, "  Run `source "+rcFile+"` or restart your shell to activate")
		return nil

	case stanzaLegacy:
		if !promptYesNo("Shell completion uses old format. Update?") {
			return nil
		}
		//nolint:gosec // G304: rcFile is constructed from home dir + known filename
		content, err = os.ReadFile(rcFile)
		if err != nil {
			return fmt.Errorf("reading %s: %w", rcFile, err)
		}
		updated, _ := removeLegacyCompletion(string(content), completionLine)
		updated = updated + "\n" + buildStanza(shellCompletionStanzaVersion, completionLine) + "\n"
		//nolint:gosec // G306: Shell rc files need 0644 for user readability
		if err := os.WriteFile(rcFile, []byte(updated), 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", rcFile, err)
		}
		fmt.Fprintf(w, "✓ Shell completion updated in %s\n", rcFile)
		fmt.Fprintln(w, "  Run `source "+rcFile+"` or restart your shell to activate")
		return nil

	case stanzaNotFound:
		if !promptYesNo(fmt.Sprintf("Enable shell completion? (detected: %s)", shellName)) {
			return nil
		}
		if err := appendShellCompletion(rcFile, completionLine); err != nil {
			return fmt.Errorf("failed to update %s: %w", rcFile, err)
		}
		fmt.Fprintf(w, "✓ Shell completion added to %s\n", rcFile)
		fmt.Fprintln(w, "  Run `source "+rcFile+"` or restart your shell to activate")
	}

	return nil
}

// promptYesNo shows a yes/no selection prompt and returns whether the user chose yes.
func promptYesNo(title string) bool {
	var selected string
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title(title).
				Options(
					huh.NewOption("Yes", "yes"),
					huh.NewOption("No", "no"),
				).
				Value(&selected),
		),
	)

	if err := form.Run(); err != nil {
		return false // User cancelled
	}
	return selected == "yes"
}

// appendShellCompletion adds the completion stanza to the rc file.
func appendShellCompletion(rcFile, completionLine string) error {
	if err := os.MkdirAll(filepath.Dir(rcFile), 0o755); err != nil {
		return fmt.Errorf("creating directory: %w", err)
	}
	//nolint:gosec // G302: Shell rc files need 0644 for user readability
	f, err := os.OpenFile(rcFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("opening file: %w", err)
	}
	defer f.Close()

	_, err = f.WriteString("\n" + buildStanza(shellCompletionStanzaVersion, completionLine) + "\n")
	if err != nil {
		return fmt.Errorf("writing completion: %w", err)
	}
	return nil
}

// removeShellCompletion removes the shell completion stanza (or legacy format)
// from the user's rc file. Used during uninstall.
func removeShellCompletion() error {
	_, rcFile, completionLine, err := shellCompletionTarget()
	if err != nil {
		return nil // unsupported shell or no home dir - nothing to remove
	}

	//nolint:gosec // G304: rcFile is constructed from home dir + known filename, not user input
	content, err := os.ReadFile(rcFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading %s: %w", rcFile, err)
	}

	text := string(content)
	updated, removeErr := removeStanza(text)
	if removeErr != nil {
		return fmt.Errorf("%w in %s", removeErr, rcFile)
	}

	if updated != text {
		//nolint:gosec // G306: Shell rc files need 0644 for user readability
		if err := os.WriteFile(rcFile, []byte(updated), 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", rcFile, err)
		}
		return nil
	}

	// Fall back to legacy format removal
	updated, removed := removeLegacyCompletion(text, completionLine)
	if removed {
		//nolint:gosec // G306: Shell rc files need 0644 for user readability
		if err := os.WriteFile(rcFile, []byte(updated), 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", rcFile, err)
		}
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
	claudeHooksInstalled := checkClaudeCodeHooksInstalled()
	geminiHooksInstalled := checkGeminiCLIHooksInstalled()
	entireDirExists := checkEntireDirExists()

	// Check if there's anything to uninstall
	if !entireDirExists && !gitHooksInstalled && sessionStateCount == 0 &&
		shadowBranchCount == 0 && !claudeHooksInstalled && !geminiHooksInstalled {
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
		switch {
		case claudeHooksInstalled && geminiHooksInstalled:
			fmt.Fprintln(w, "  - Agent hooks (Claude Code, Gemini CLI)")
		case claudeHooksInstalled:
			fmt.Fprintln(w, "  - Agent hooks (Claude Code)")
		case geminiHooksInstalled:
			fmt.Fprintln(w, "  - Agent hooks (Gemini CLI)")
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

	// 0. Remove shell completion from rc file
	if err := removeShellCompletion(); err != nil {
		fmt.Fprintf(errW, "Warning: failed to remove shell completion: %v\n", err)
	} else {
		fmt.Fprintln(w, "  Removed shell completion")
	}

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

// checkClaudeCodeHooksInstalled checks if Claude Code hooks are installed.
func checkClaudeCodeHooksInstalled() bool {
	ag, err := agent.Get(agent.AgentNameClaudeCode)
	if err != nil {
		return false
	}
	hookAgent, ok := ag.(agent.HookSupport)
	if !ok {
		return false
	}
	return hookAgent.AreHooksInstalled()
}

// checkGeminiCLIHooksInstalled checks if Gemini CLI hooks are installed.
func checkGeminiCLIHooksInstalled() bool {
	ag, err := agent.Get(agent.AgentNameGemini)
	if err != nil {
		return false
	}
	hookAgent, ok := ag.(agent.HookSupport)
	if !ok {
		return false
	}
	return hookAgent.AreHooksInstalled()
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

	// Remove Claude Code hooks
	claudeAgent, err := agent.Get(agent.AgentNameClaudeCode)
	if err == nil {
		if hookAgent, ok := claudeAgent.(agent.HookSupport); ok {
			wasInstalled := hookAgent.AreHooksInstalled()
			if err := hookAgent.UninstallHooks(); err != nil {
				errs = append(errs, err)
			} else if wasInstalled {
				fmt.Fprintln(w, "  Removed Claude Code hooks")
			}
		}
	}

	// Remove Gemini CLI hooks
	geminiAgent, err := agent.Get(agent.AgentNameGemini)
	if err == nil {
		if hookAgent, ok := geminiAgent.(agent.HookSupport); ok {
			wasInstalled := hookAgent.AreHooksInstalled()
			if err := hookAgent.UninstallHooks(); err != nil {
				errs = append(errs, err)
			} else if wasInstalled {
				fmt.Fprintln(w, "  Removed Gemini CLI hooks")
			}
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
