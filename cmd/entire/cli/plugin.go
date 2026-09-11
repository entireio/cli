package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/external"
	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/telemetry"
	"github.com/entireio/cli/cmd/entire/cli/versioncheck"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/spf13/cobra"
)

// External-command resolution, kubectl-style. When the user invokes
// `entire <name>` and <name> isn't a built-in subcommand, look for an
// `entire-<name>` binary on PATH and exec it with the remaining args.
// Stdio and exit codes pass through. Binaries matching the agent
// protocol prefix are reserved for the external agent registry and
// skipped here.
const (
	pluginBinaryPrefix      = "entire-"
	agentPluginBinaryPrefix = "entire-agent-"
)

// selfUpdatePluginName is the plugin that replaces the entire binary on
// disk (`entire upgrade` → entire-upgrade).
const selfUpdatePluginName = "upgrade"

// onDemandInstallPluginName is the one missing plugin the dispatcher offers to
// install rather than falling through to Cobra's unknown-command path. Kept as
// a named constant beside the other plugin names the dispatcher special-cases,
// so the set is readable in one place.
const onDemandInstallPluginName = "graph"

// ExitPluginSignalled reports that a plugin was terminated by a signal, or
// that a signal interrupted an on-demand install before the plugin ran. It is
// deliberately not a valid exit status — os.Exit(-1) truncates to 255 — so
// main.go re-raises the signal instead of exiting with it. -1 is already what
// exec.ExitError.ExitCode() reports for a signalled child.
//
// MaybeRunPlugin's killedBy return says WHICH signal, when it is knowable.
// The two are separate because they have different sources: the exit code
// comes from the child's wait status, while the signal may have reached only
// the child (`kill -TERM` at the plugin, SIGPIPE from a closed pipe) or only
// this process (a Ctrl-C during the on-demand install, where there is no
// child yet).
const ExitPluginSignalled = -1

// postPluginVersionCheck is a test seam for the version-check notice that
// fires after a successful plugin run.
var postPluginVersionCheck = versioncheck.CheckAndNotify

// MaybeRunPlugin returns (true, exitCode) when an external command was
// resolved and run. On launch failure (e.g. missing executable bit)
// returns (true, 1) after printing to stderr. On no-match returns
// (false, 0) so the caller can fall through to Cobra. exitCode is
// ExitPluginSignalled when the plugin was killed by a signal, or when a
// signal interrupted an on-demand install before it ran; the caller turns
// that into a re-raised signal rather than an exit status.
//
// killedBy is the signal the plugin was killed by, when the platform reports
// one. It is nil for an ordinary exit, on Windows, and for an install
// interrupted before any child existed — in that last case the signal is the
// one this process received, which the caller already has.
//
// Telemetry and the version-check notice mirror Cobra's PersistentPostRun
// behavior for built-ins: both fire only on a successful (exit-0) run.
func MaybeRunPlugin(ctx context.Context, rootCmd *cobra.Command, args []string) (handled bool, exitCode int, killedBy os.Signal) {
	binPath, pluginArgs, ok := resolvePlugin(rootCmd, args)
	if !ok {
		return false, 0, nil
	}
	pluginName := args[0]
	if binPath == "" {
		var err error
		binPath, err = installMissingPlugin(ctx, rootCmd, pluginName)
		if err != nil {
			var silent *SilentError
			if errors.As(silencePluginCancel(ctx, err), &silent) {
				// A signal interrupted the install. Report it as a signal
				// rather than a plain failure so main.go re-raises it: a
				// shell breaks an enclosing loop only on WIFSIGNALED. No
				// child ran, so there is no child signal to name — the
				// caller falls back to the one it received.
				return true, ExitPluginSignalled, nil
			}
			fmt.Fprintln(rootCmd.ErrOrStderr(), RenderUserFacingError(err))
			return true, 1, nil
		}
		if binPath == "" {
			// The command was not executed because installation was declined.
			return true, 1, nil
		}
		// Say what is happening now: the install may have taken a while, and
		// it is the reason the user is still waiting.
		//
		// The binary's name, never the arguments. They are the user's own
		// command line, already on their screen, so echoing them back adds
		// nothing — and it would put whatever they contain into stderr and
		// into anything capturing it: a token passed as a flag, a newline
		// that forges a second line of output, a terminal escape that
		// repositions the cursor or repaints what is above it. That last one
		// is the same hazard hasTerminalControlChars exists for.
		fmt.Fprintf(rootCmd.ErrOrStderr(), "Running %s%s\n", pluginBinaryPrefix, pluginName)
	}
	exitCode, killedBy = runPlugin(ctx, pluginName, binPath, pluginArgs)
	if exitCode == 0 {
		maybeTrackPluginInvocation(ctx, pluginName)
		// Stderr, matching the built-in PersistentPostRun: the plugin's own
		// stdout may be machine-readable and piped.
		//
		// Skipped after a self-update: this process still carries the
		// pre-upgrade compiled-in version, so the check would see itself as
		// outdated and prompt to redo the upgrade that just completed.
		if pluginName != selfUpdatePluginName {
			postPluginVersionCheck(ctx, os.Stderr, versioninfo.Version)
		}
	}
	return true, exitCode, killedBy
}

// maybeTrackPluginInvocation fires telemetry only for plugins on the
// official allowlist. Third-party plugin names are never sent — see
// IsOfficialPlugin for the rationale.
func maybeTrackPluginInvocation(ctx context.Context, pluginName string) {
	if !IsOfficialPlugin(pluginName) {
		return
	}
	s, err := LoadEntireSettings(ctx)
	if err != nil {
		return
	}
	if !s.IsTelemetryEnabled() {
		return
	}
	telemetry.TrackPluginDetached(pluginName, s.Enabled, versioninfo.Version)
}

// resolvePlugin returns an empty binary path for a missing
// onDemandInstallPluginName so the dispatcher can offer installation. Other
// missing names fall through.
func resolvePlugin(rootCmd *cobra.Command, args []string) (binPath string, pluginArgs []string, ok bool) {
	if len(args) == 0 {
		return "", nil, false
	}
	name := args[0]
	if !isPluginCandidate(name) {
		return "", nil, false
	}
	// Cobra adds `help` and `completion` to the command tree inside Execute,
	// not in the constructor / SetHelpCommand. Without priming them, Find
	// reports "unknown command" for those names and an entire-help (or
	// entire-completion) binary on PATH would shadow the built-in. Both
	// initializers are idempotent and Execute calls them again later.
	rootCmd.InitDefaultHelpCmd()
	rootCmd.InitDefaultCompletionCmd(args...)
	// Built-in commands always win.
	if cmd, _, err := rootCmd.Find(args); err == nil && cmd != rootCmd {
		return "", nil, false
	}
	binName := pluginBinaryPrefix + name
	binPath, err := exec.LookPath(binName)
	if err != nil {
		// LookPath conflates "not on PATH" with "found but not executable".
		// Distinguish: if a file with this name exists on PATH but isn't
		// executable, surface that as a launch error rather than falling
		// through to Cobra's generic unknown-command path.
		if p, found := findInaccessiblePlugin(binName); found {
			return p, args[1:], true
		}
		if name == onDemandInstallPluginName && errors.Is(err, exec.ErrNotFound) {
			return "", args[1:], true
		}
		return "", nil, false
	}
	if isAgentProtocolBinary(binPath) {
		return "", nil, false
	}
	return binPath, args[1:], true
}

// findInaccessiblePlugin scans PATH for a non-directory file with the
// given name. Only meaningful after exec.LookPath has already failed —
// indicates the file exists but lacks the executable bit (or the
// equivalent platform-specific accessibility).
//
// The scan goes through execx.PathScanDirs, not a bare split: the rule that
// only absolute entries are scanned is shared with the external-agent
// scanner, and two copies of it is how they drifted apart in the first place.
// The rule matters especially here, because this function runs only after
// LookPath has failed. LookPath's error does not distinguish exec.ErrDot (its
// refusal to resolve a match found through a relative PATH entry) from a
// genuine "exists but not executable", so a scan that re-walked relative
// entries would blindly re-find the exact binary LookPath just correctly
// refused. runPlugin then execs it via a path containing a separator, which
// bypasses exec.Command's own ErrDot re-check, since that only fires for a
// bare name with no separator.
func findInaccessiblePlugin(filename string) (string, bool) {
	for _, dir := range execx.PathScanDirs() {
		candidate := filepath.Join(dir, filename)
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() {
			continue
		}
		return candidate, true
	}
	return "", false
}

// isPluginCandidate reports whether name is a syntactically valid plugin
// name the dispatcher should attempt to resolve. It is a thin bool wrapper
// over validatePluginName so the dispatcher's gate and the managed store's
// install-time check can never drift.
func isPluginCandidate(name string) bool {
	return validatePluginName(name) == nil
}

// isAgentProtocolBinary returns true when the binary name is reserved for
// the external agent protocol. Strip Windows extensions first so
// `entire-agent-foo.exe` is also recognized.
func isAgentProtocolBinary(binPath string) bool {
	base := external.StripExeExt(filepath.Base(binPath))
	return strings.HasPrefix(base, agentPluginBinaryPrefix)
}

// runPlugin executes the resolved plugin binary, propagating its exit code.
// On context cancellation the child gets SIGINT (with a 5s grace before the
// runtime falls back to SIGKILL) so plugins can clean up. Terminal signals
// reach the child directly via the shared process group.
func runPlugin(ctx context.Context, pluginName, binPath string, args []string) (exitCode int, killedBy os.Signal) {
	cmd := exec.CommandContext(ctx, binPath, args...)
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 5 * time.Second
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	extras := []string{"ENTIRE_CLI_VERSION=" + versioninfo.Version}
	if repoRoot, err := paths.WorktreeRoot(ctx); err == nil {
		extras = append(extras, "ENTIRE_REPO_ROOT="+repoRoot)
	}
	// Per-plugin durable storage. Passed regardless of where the binary lives
	// so plugins installed via raw PATH and via `entire plugin install` get
	// the same contract. The dir is not pre-created — that's the plugin's
	// responsibility on first use.
	//
	// PluginDataDir can only fail in degenerate environments (no resolvable
	// home dir, or a relative ENTIRE_PLUGIN_DIR override). The plugin name
	// itself already passed isPluginCandidate in resolvePlugin, so the name
	// validator branch can't fire here. Proceed without the var rather than
	// refuse to launch: a misconfigured environment is the user's problem to
	// surface, not a reason to break plugins that don't read the var. The
	// failure is logged at debug rather than printed to stderr — printing
	// would noise every plugin invocation in a degenerate env.
	parentEnv := os.Environ()
	if dataDir, err := PluginDataDir(pluginName); err == nil {
		extras = append(extras, pluginEnvPluginData+"="+dataDir)
	} else {
		// Strip any inherited value so the plugin doesn't silently see a
		// value we never sanctioned. Without this strip, a user with
		// ENTIRE_PLUGIN_DATA_DIR pre-set in their shell would have that
		// value pass through (ENTIRE_* is in the pluginEnv allowlist
		// prefix), even though resolution here failed.
		parentEnv = removeEnvKey(parentEnv, pluginEnvPluginData)
		logging.Debug(ctx, "ENTIRE_PLUGIN_DATA_DIR unset for plugin",
			slog.String("plugin", pluginName),
			slog.String("error", err.Error()))
	}
	cmd.Env = pluginEnv(parentEnv, extras...)

	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// A signalled child reports -1, i.e. ExitPluginSignalled: it is
			// not an exit status, so the caller re-raises the signal rather
			// than letting os.Exit truncate it to 255. Which signal comes
			// from the wait status, because it need not be one this process
			// received — see pluginTerminatingSignal.
			return exitErr.ExitCode(), pluginTerminatingSignal(exitErr.ProcessState)
		}
		// Prefix with the plugin name so users can tell parent vs child
		// errors apart in mixed stderr.
		fmt.Fprintf(os.Stderr, "Failed to run plugin %s: %v\n", filepath.Base(binPath), err)
		return 1, nil
	}
	return 0, nil
}
