package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/shellhook"
	"github.com/entireio/cli/cmd/entire/cli/uiform"
)

// shellhookComment is the marker comment for the block `shellhook install`
// manages in the user's rc file.
const shellhookComment = "# Entire CLI shell hook"

// errShellhookUnsupportedOS is returned by install/init on platforms where the
// POSIX shell integration does not apply.
var errShellhookUnsupportedOS = errors.New("the shell hook is not supported on Windows")

func newShellhookCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "shellhook",
		Short: "Warn (or auto-enable) when you cd into a repo without Entire",
		Long: `Manage an opt-in shell hook that notices repositories where Entire is not enabled.

Once installed, the hook runs on every directory change. In its steady state it
does the whole check with shell builtins and never forks a process; it only runs
` + "`entire shellhook check`" + ` after landing in a new git repository that has no
.entire/settings.json.

Default behavior is a single throttled warning on stderr. Opt into 'auto' mode to
be offered ` + "`entire enable`" + ` instead.

Escape hatches:
  ENTIRE_NO_SHELL_HOOK=1     silence the hook for a shell session
  entire shellhook dismiss   silence the hook for one repository
  entire shellhook uninstall remove the hook entirely`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(
		newShellhookInitCmd(),
		newShellhookInstallCmd(),
		newShellhookUninstallCmd(),
		newShellhookStatusCmd(),
		newShellhookDismissCmd(),
		newShellhookCheckCmd(),
	)
	return cmd
}

// ---------------------------------------------------------------- init

func newShellhookInitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init <bash|zsh|fish>",
		Short: "Print the shell hook script for a shell",
		Long: `Print the shell hook script to stdout, for eval in an rc file:

  zsh/bash:  eval "$(entire shellhook init zsh)"
  fish:      entire shellhook init fish | source

` + "`entire shellhook install`" + ` writes the matching line for you.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireShellhookOS(cmd); err != nil {
				return err
			}
			script, err := shellhookScript(args[0])
			if err != nil {
				cmd.SilenceUsage = true
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), script)
			return nil
		},
	}
}

// ---------------------------------------------------------------- install

type shellhookInstallOptions struct {
	shell  string
	mode   string
	agents []string
	yes    bool
}

func newShellhookInstallCmd() *cobra.Command {
	var opts shellhookInstallOptions

	cmd := &cobra.Command{
		Use:   "install",
		Short: "Add the shell hook to your shell's rc file",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireShellhookOS(cmd); err != nil {
				return err
			}
			return runShellhookInstall(cmd, opts)
		},
	}
	cmd.Flags().StringVar(&opts.shell, "shell", "", "Shell to install for: "+supportedShellNames+" (default: detect from $SHELL)")
	cmd.Flags().StringVar(&opts.mode, "mode", string(shellhook.ModeWarn), "What the hook does in an un-enabled repo: warn or auto")
	cmd.Flags().StringArrayVar(&opts.agents, "agent", nil, "Agent to enable in 'auto' mode (repeatable; defaults to whatever is detected in the repo)")
	cmd.Flags().BoolVarP(&opts.yes, "yes", "y", false, "Accept defaults without prompting")
	return cmd
}

func runShellhookInstall(cmd *cobra.Command, opts shellhookInstallOptions) error {
	out := cmd.OutOrStdout()

	kind, shellName, rcFile, err := shellRCTarget(opts.shell)
	if err != nil {
		cmd.SilenceUsage = true
		if errors.Is(err, errUnsupportedShell) {
			named := opts.shell
			if named == "" {
				named = os.Getenv("SHELL")
			}
			fmt.Fprintf(cmd.ErrOrStderr(),
				"Cannot install the shell hook for %q. Supported shells: %s.\nPass --shell to choose one explicitly.\n",
				named, supportedShellNames)
			return NewSilentError(errUnsupportedShell)
		}
		return err
	}

	prefs, err := resolveShellhookInstallPrefs(cmd, opts, shellName)
	if errors.Is(err, errShellhookInstallDeclined) {
		fmt.Fprintln(out, "Shell hook installation cancelled.")
		return nil
	}
	if err != nil {
		return err
	}

	if err := shellhook.SavePreferences(prefs); err != nil {
		cmd.SilenceUsage = true
		return fmt.Errorf("saving shell hook preferences: %w", err)
	}

	if isMarkerConfigured(rcFile, shellhookComment) {
		fmt.Fprintf(out, "✓ Shell hook already configured in %s (mode: %s)\n", rcFile, prefs.Mode)
		return nil
	}
	if err := appendMarkedBlock(rcFile, shellhookComment, shellhookRCLines[kind]); err != nil {
		cmd.SilenceUsage = true
		return fmt.Errorf("failed to update %s: %w", rcFile, err)
	}

	fmt.Fprintf(out, "✓ Shell hook added to %s (mode: %s)\n", rcFile, prefs.Mode)
	fmt.Fprintln(out, "  Restart your shell to activate")
	return nil
}

// errShellhookInstallDeclined signals that the user said no at the prompt —
// not a failure, but nothing to install either.
var errShellhookInstallDeclined = errors.New("shell hook installation declined")

// resolveShellhookInstallPrefs builds the preferences to store, prompting for
// anything the flags did not pin down. Returns errShellhookInstallDeclined
// when the user turns the install down.
func resolveShellhookInstallPrefs(cmd *cobra.Command, opts shellhookInstallOptions, shellName string) (*shellhook.Preferences, error) {
	mode := shellhook.Mode(opts.mode)
	if !mode.Valid() || mode == shellhook.ModeOff {
		cmd.SilenceUsage = true
		return nil, fmt.Errorf("invalid --mode %q: want %q or %q", opts.mode, shellhook.ModeWarn, shellhook.ModeAuto)
	}
	agents := opts.agents

	if !opts.yes && interactive.CanPromptInteractively() {
		if !confirmShellhookInstall(shellName) {
			return nil, errShellhookInstallDeclined
		}
		if !cmd.Flags().Changed("mode") {
			mode = selectShellhookMode()
		}
		if mode == shellhook.ModeAuto && !cmd.Flags().Changed("agent") {
			agents = selectShellhookAgents()
		}
	}

	return &shellhook.Preferences{
		Version:       shellhook.PreferencesVersion,
		Mode:          mode,
		DefaultAgents: agents,
	}, nil
}

// The three prompt helpers below never fail: a cancelled form is answered with
// the conservative default (no install, warn mode, no configured agents).

func confirmShellhookInstall(shellName string) bool {
	var choice string
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title(fmt.Sprintf("Install the Entire shell hook? (detected: %s)", shellName)).
				Description("Warns when you cd into a git repository where Entire is not enabled.").
				Options(huh.NewOption("Yes", promptOptionYes), huh.NewOption("No", "no")).
				Value(&choice),
		),
	)
	if err := form.Run(); err != nil {
		return false
	}
	return choice == promptOptionYes
}

func selectShellhookMode() shellhook.Mode {
	var choice string
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("What should the hook do in an un-enabled repository?").
				Options(
					huh.NewOption("Warn once per repository (recommended)", string(shellhook.ModeWarn)),
					huh.NewOption("Offer to run `entire enable`", string(shellhook.ModeAuto)),
				).
				Value(&choice),
		),
	)
	if err := form.Run(); err != nil {
		return shellhook.ModeWarn
	}
	if mode := shellhook.Mode(choice); mode.Valid() {
		return mode
	}
	return shellhook.ModeWarn
}

func selectShellhookAgents() []string {
	options := shellhookAgentOptions()
	if len(options) == 0 {
		return nil
	}
	var selected []string
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewMultiSelect[string]().
				Title("Which agents should `entire enable` set up by default?").
				Description("Agents detected in the repository are always included.").
				Options(options...).
				Value(&selected),
		),
	)
	if err := form.Run(); err != nil {
		return nil
	}
	return selected
}

// shellhookAgentOptions renders the registered agents as display-name options.
func shellhookAgentOptions() []huh.Option[string] {
	names := agent.StringList()
	options := make([]huh.Option[string], 0, len(names))
	for _, name := range names {
		options = append(options, huh.NewOption(agentDisplayName(name), name))
	}
	return options
}

// agentDisplayName returns an agent's human-readable type, falling back to its
// registry key for agents that cannot be instantiated.
func agentDisplayName(name string) string {
	ag, err := agent.Get(types.AgentName(name))
	if err != nil {
		return name
	}
	if display := string(ag.Type()); display != "" {
		return display
	}
	return name
}

// ---------------------------------------------------------------- uninstall

func newShellhookUninstallCmd() *cobra.Command {
	var shell string
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the shell hook from your shell's rc file",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()

			_, _, rcFile, err := shellRCTarget(shell)
			if err != nil && !errors.Is(err, errUnsupportedShell) {
				cmd.SilenceUsage = true
				return err
			}

			// Turn the hook off first: preferences are what `check` consults,
			// so an rc file the user edits back in stays inert.
			prefs, loadErr := shellhook.LoadPreferences()
			if loadErr != nil {
				prefs = &shellhook.Preferences{Version: shellhook.PreferencesVersion}
			}
			prefs.Mode = shellhook.ModeOff
			if err := shellhook.SavePreferences(prefs); err != nil {
				cmd.SilenceUsage = true
				return fmt.Errorf("saving shell hook preferences: %w", err)
			}

			if rcFile == "" {
				fmt.Fprintln(out, "✓ Shell hook disabled (no supported shell rc file detected)")
				return nil
			}
			removed, err := removeMarkedBlock(rcFile, shellhookComment)
			if err != nil {
				cmd.SilenceUsage = true
				return fmt.Errorf("failed to update %s: %w", rcFile, err)
			}
			if removed {
				fmt.Fprintf(out, "✓ Shell hook removed from %s\n", rcFile)
				fmt.Fprintln(out, "  Restart your shell to finish")
			} else {
				fmt.Fprintf(out, "✓ Shell hook disabled (nothing to remove from %s)\n", rcFile)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&shell, "shell", "", "Shell to uninstall from: "+supportedShellNames+" (default: detect from $SHELL)")
	return cmd
}

// ---------------------------------------------------------------- status

// shellhookStatusJSON is the output of `entire shellhook status --json`.
type shellhookStatusJSON struct {
	Installed      bool     `json:"installed"`
	Mode           string   `json:"mode"`
	Shell          string   `json:"shell,omitempty"`
	RCFile         string   `json:"rc_file,omitempty"`
	DefaultAgents  []string `json:"default_agents,omitempty"`
	DismissedRepos int      `json:"dismissed_repos"`
	Error          string   `json:"error,omitempty"`
}

func newShellhookStatusCmd() *cobra.Command {
	var jsonOut bool
	var shell string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show whether the shell hook is installed and what it does",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			status := collectShellhookStatus(shell)
			if jsonOut {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(status)
			}
			writeShellhookStatus(cmd.OutOrStdout(), status)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	cmd.Flags().StringVar(&shell, "shell", "", "Shell to report on: "+supportedShellNames+" (default: detect from $SHELL)")
	return cmd
}

func collectShellhookStatus(shell string) shellhookStatusJSON {
	status := shellhookStatusJSON{Mode: string(shellhook.ModeOff)}

	if _, shellName, rcFile, err := shellRCTarget(shell); err == nil {
		status.Shell = shellName
		status.RCFile = rcFile
		status.Installed = isMarkerConfigured(rcFile, shellhookComment)
	}

	prefs, err := shellhook.LoadPreferences()
	if err != nil {
		status.Error = err.Error()
		return status
	}
	status.Mode = string(prefs.Mode)
	status.DefaultAgents = prefs.DefaultAgents

	state, err := shellhook.LoadState()
	if err != nil {
		status.Error = err.Error()
		return status
	}
	status.DismissedRepos = state.DismissedCount()
	return status
}

func writeShellhookStatus(w io.Writer, status shellhookStatusJSON) {
	switch {
	case status.Installed:
		fmt.Fprintf(w, "Installed:        yes (%s)\n", status.RCFile)
	case status.RCFile != "":
		fmt.Fprintf(w, "Installed:        no (%s)\n", status.RCFile)
	default:
		fmt.Fprintln(w, "Installed:        no (no supported shell detected)")
	}
	fmt.Fprintf(w, "Mode:             %s\n", status.Mode)
	if len(status.DefaultAgents) > 0 {
		fmt.Fprintf(w, "Default agents:   %s\n", strings.Join(status.DefaultAgents, ", "))
	} else {
		fmt.Fprintln(w, "Default agents:   (detect per repository)")
	}
	fmt.Fprintf(w, "Dismissed repos:  %d\n", status.DismissedRepos)
	if status.Error != "" {
		fmt.Fprintf(w, "Error:            %s\n", status.Error)
	}
	if !status.Installed {
		fmt.Fprintln(w, "\nRun `entire shellhook install` to enable it.")
	}
}

// ---------------------------------------------------------------- dismiss

func newShellhookDismissCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "dismiss",
		Short: "Silence the shell hook for the current repository",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			root, err := paths.WorktreeRoot(ctx)
			if err != nil {
				cmd.SilenceUsage = true
				fmt.Fprintln(cmd.ErrOrStderr(), "Not a git repository. Run `entire shellhook dismiss` from inside the repository you want to silence.")
				return NewSilentError(errors.New("not a git repository"))
			}
			key, err := shellhookRepoKey(ctx, root)
			if err != nil {
				cmd.SilenceUsage = true
				return fmt.Errorf("resolving repository: %w", err)
			}

			state, err := shellhook.LoadState()
			if err != nil {
				cmd.SilenceUsage = true
				return fmt.Errorf("loading shell hook state: %w", err)
			}
			state.MarkDismissed(key, time.Now())
			if err := shellhook.SaveState(state); err != nil {
				cmd.SilenceUsage = true
				return fmt.Errorf("saving shell hook state: %w", err)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "✓ Shell hook silenced for %s\n", root)
			return nil
		},
	}
}

// ---------------------------------------------------------------- check

func newShellhookCheckCmd() *cobra.Command {
	var root string
	cmd := &cobra.Command{
		Use:    "check",
		Short:  "Report whether a repository is missing Entire (internal; used by the shell hook)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			runShellhookCheck(cmd.Context(), shellhookIO{
				stdin:  cmd.InOrStdin(),
				stdout: cmd.OutOrStdout(),
				stderr: cmd.ErrOrStderr(),
			}, root)
			// The hook runs inside the user's prompt: never a non-zero exit.
			return nil
		},
	}
	cmd.Flags().StringVar(&root, "root", "", "Absolute path of the repository root to check")
	return cmd
}

// shellhookIO carries the streams `check` may touch, so tests can drive it
// without a terminal.
type shellhookIO struct {
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
}

// runShellhookCheck is the hot path invoked from the user's shell prompt.
//
// It is deliberately total: every failure degrades to silence (logged at debug
// level only, never to stderr) and panics are recovered. A broken check must
// never break someone's prompt, and it must never write to stdout — the shell
// captures nothing, but a stray line would still land mid-prompt.
func runShellhookCheck(ctx context.Context, sio shellhookIO, root string) {
	defer func() {
		if r := recover(); r != nil {
			logging.Debug(ctx, "shellhook check panicked", "panic", fmt.Sprint(r))
		}
	}()

	if root == "" {
		return
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		logging.Debug(ctx, "shellhook check: cannot resolve root", "error", err.Error())
		return
	}

	prefs, err := shellhook.LoadPreferences()
	if err != nil {
		logging.Debug(ctx, "shellhook check: cannot load preferences", "error", err.Error())
		return
	}
	if prefs.Mode == shellhook.ModeOff {
		return
	}

	// The shell already checked for the settings files, but that result is a
	// race away from stale (another shell may have just enabled the repo) and
	// it cannot see a repo enabled through clone-local preferences.
	repoCtx := paths.WithWorktreeRoot(ctx, absRoot)
	if settings.IsSetUpAny(repoCtx) {
		return
	}

	key, err := shellhookRepoKey(ctx, absRoot)
	if err != nil {
		logging.Debug(ctx, "shellhook check: not a git repository", "error", err.Error())
		return
	}

	state, err := shellhook.LoadState()
	if err != nil {
		logging.Debug(ctx, "shellhook check: cannot load state", "error", err.Error())
		return
	}
	now := time.Now()
	if !state.ShouldWarn(key, now, prefs.WarnThrottle()) {
		return
	}

	detected := detectedAgentDisplayNames(repoCtx)

	if prefs.Mode == shellhook.ModeAuto && shellhookAutoEnableAllowed(absRoot) &&
		runShellhookAutoEnable(repoCtx, sio, prefs, absRoot, detected) {
		return
	}

	fmt.Fprintln(sio.stderr, shellhookWarning(detected))
	state.MarkWarned(key, now)
	if err := shellhook.SaveState(state); err != nil {
		logging.Debug(ctx, "shellhook check: cannot save state", "error", err.Error())
	}
}

// shellhookWarning renders the single stderr line the hook emits.
func shellhookWarning(detectedAgents []string) string {
	detected := ""
	if len(detectedAgents) > 0 {
		detected = fmt.Sprintf(" (detected: %s)", strings.Join(detectedAgents, ", "))
	}
	return fmt.Sprintf(
		"entire: checkpointing is not enabled in this repo%s. Run 'entire enable', or 'entire shellhook dismiss' to silence for this repo.",
		detected)
}

// detectedAgentDisplayNames lists the agents configured in the repository.
func detectedAgentDisplayNames(ctx context.Context) []string {
	agents := agent.DetectAll(ctx)
	names := make([]string, 0, len(agents))
	for _, ag := range agents {
		if display := string(ag.Type()); display != "" {
			names = append(names, display)
		}
	}
	return names
}

// shellhookAutoEnableAllowed gates the auto mode on the two conditions that
// make running `entire enable` unattended defensible: a real terminal to
// confirm at, and a repository the current user owns. Anything else (a shared
// checkout, another user's tree, a CI shell) degrades to a warning.
func shellhookAutoEnableAllowed(root string) bool {
	return interactive.CanPromptInteractively() && pathOwnedByCurrentUser(root)
}

// runShellhookAutoEnable offers to enable Entire and reports whether the
// repository ended up enabled. A decline, a cancel, or a failed enable returns
// false so the caller falls back to the warning.
func runShellhookAutoEnable(ctx context.Context, sio shellhookIO, prefs *shellhook.Preferences, root string, detected []string) bool {
	if !prefs.AutoEnableNoConfirm && !confirmShellhookAutoEnable(ctx, root) {
		return false
	}

	agents := shellhookAutoEnableAgents(prefs.DefaultAgents, detected)
	exe, err := os.Executable()
	if err != nil {
		logging.Debug(ctx, "shellhook auto-enable: cannot resolve executable", "error", err.Error())
		return false
	}

	// `entire enable --agent` is a targeted, single-agent operation, so run it
	// once per agent rather than passing a list it would not understand.
	for _, name := range agents {
		cmd := exec.CommandContext(ctx, exe, "enable", "--agent", name)
		cmd.Dir = root
		cmd.Stdin = sio.stdin
		cmd.Stdout = sio.stdout
		cmd.Stderr = sio.stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(sio.stderr, "entire: could not enable %s automatically (%v)\n", name, err)
			return false
		}
	}
	return len(agents) > 0
}

// confirmShellhookAutoEnable asks before running enable. uiform.PromptYN
// applies the shared theme and ACCESSIBLE handling and treats a cancel as
// "no", which is exactly the conservative default this path wants.
func confirmShellhookAutoEnable(ctx context.Context, root string) bool {
	confirmed, err := uiform.PromptYN(ctx, fmt.Sprintf("Enable Entire checkpointing in %s?", root), false)
	if err != nil {
		return false
	}
	return confirmed
}

// shellhookAutoEnableAgents is the union of what the repository shows and what
// the user configured, resolved back to registry names and de-duplicated.
func shellhookAutoEnableAgents(defaults, detectedDisplayNames []string) []string {
	seen := make(map[string]bool)
	var agents []string
	add := func(name string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		agents = append(agents, name)
	}

	displayToName := make(map[string]string)
	for _, name := range agent.StringList() {
		displayToName[agentDisplayName(name)] = name
	}
	for _, display := range detectedDisplayNames {
		add(displayToName[display])
	}
	for _, name := range defaults {
		add(name)
	}
	return agents
}

// shellhookRepoKey returns the state key for a repository: the absolute path
// of its git common directory, so every worktree of one clone shares a single
// throttle and a single dismissal.
func shellhookRepoKey(ctx context.Context, root string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "--git-common-dir")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("resolving git common dir for %s: %w", root, err)
	}

	commonDir := strings.TrimSpace(string(output))
	if commonDir == "" {
		return "", fmt.Errorf("empty git common dir for %s", root)
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(root, commonDir)
	}
	commonDir = filepath.Clean(commonDir)
	if resolved, err := filepath.EvalSymlinks(commonDir); err == nil {
		commonDir = resolved
	}
	return commonDir, nil
}

// requireShellhookOS rejects the POSIX-shell-only subcommands on Windows.
func requireShellhookOS(cmd *cobra.Command) error {
	if runtime.GOOS != windowsGOOS {
		return nil
	}
	cmd.SilenceUsage = true
	fmt.Fprintln(cmd.ErrOrStderr(),
		"The Entire shell hook supports bash, zsh, and fish only, so it is not available on Windows yet.")
	return NewSilentError(errShellhookUnsupportedOS)
}
