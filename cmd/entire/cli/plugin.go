package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/external"
	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/telemetry"
	"github.com/entireio/cli/cmd/entire/cli/versioncheck"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/entireio/cli/internal/procsignal"
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

// onDemandInstallPluginNames are the missing plugins the dispatcher offers to
// install rather than falling through to Cobra's unknown-command path. Kept
// beside the other plugin names the dispatcher special-cases, so the set is
// readable in one place.
//
// Membership is deliberately narrow. The offer is a prompt that defaults to
// Yes and ends in a downloaded binary linked onto $PATH, so it belongs only to
// former built-ins and to commands Entire's docs or generated agent guides
// tell people to run as `entire <name>` — names a user types because we told
// them the command exists. `graph` is what Brain's agent guide sends agents to
// (`entire graph search`); `brain` is documented as `entire brain`;
// `investigate` was a built-in until it moved to the entire-investigate
// plugin, and without the offer `entire investigate` would answer an
// established command with "unknown command for entire"; `rsl` is documented
// as `entire rsl` by the entire-rsl plugin.
//
// A name here must be resolvable through the plugin index: the install path
// looks it up there and nowhere else, so an unlisted name turns the prompt
// into a failure that the fall-through would have reported more plainly.
//
//nolint:gochecknoglobals // package-level set; a slice because there is no const slice in Go.
var onDemandInstallPluginNames = []string{"graph", "brain", "investigate", "rsl"}

// offersOnDemandInstall reports whether a missing plugin by this name should
// be offered for installation.
func offersOnDemandInstall(name string) bool {
	return slices.Contains(onDemandInstallPluginNames, name)
}

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

// PluginExitError is returned by a built-in command that ran a plugin on the
// user's behalf (agent-help's delegation) when the plugin did not exit 0. It
// carries runPlugin's outcome so main.go can exit with it — the plugin's own
// code, or its re-raised signal — exactly as it does for a dispatched plugin,
// rather than the plain 1 every other returned error gets. The plugin's own
// stderr is the user-facing message, so main prints nothing for it.
type PluginExitError struct {
	Code     int       // runPlugin's exit code; ExitPluginSignalled when killed by a signal
	KilledBy os.Signal // runPlugin's killedBy; nil for an ordinary exit
	Err      error
}

func (e *PluginExitError) Error() string { return e.Err.Error() }

func (e *PluginExitError) Unwrap() error { return e.Err }

// AlreadyPrinted reports that the plugin's stderr already carried the message.
func (e *PluginExitError) AlreadyPrinted() bool { return true }

// postPluginVersionCheck is a test seam for the version-check notice that
// fires after a successful plugin run.
var postPluginVersionCheck = versioncheck.CheckAndNotify

// postPluginUpdateCheck is a test seam for the plugin's own update notice,
// same pattern as postPluginVersionCheck above.
var postPluginUpdateCheck = checkPluginUpdateAndNotify

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
	} else if isManagedBinEntry(binPath) {
		// LookPath accepts a 0-byte executable, so an empty managed entry
		// resolves and then fails in exec with an opaque "exec format error".
		// Diagnose it the way the on-demand path does, with the remedy.
		if err := managedEntryUnrunnable(pluginName, binPath); err != nil {
			fmt.Fprintln(rootCmd.ErrOrStderr(), RenderUserFacingError(err))
			return true, 1, nil
		}
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
		postPluginUpdateCheck(ctx, os.Stderr, pluginName)
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

// resolvePlugin returns an empty binary path for a missing plugin named by
// onDemandInstallPluginNames so the dispatcher can offer installation. Other
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
		if offersOnDemandInstall(name) && errors.Is(err, exec.ErrNotFound) {
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
// The plugin stays a plain child in our process group, so terminal signals
// reach it directly. On context cancellation it is sent the signal we received
// (configurePluginCancel) so it can clean up; on a force quit, ForceQuitPlugins
// ends it.
//
// Known limitation, on every platform: if this process is killed in a way it
// cannot catch (SIGKILL, the OOM killer, taskkill /F, a crash), the plugin
// keeps running. Closing that would take a cross-platform mechanism such as a
// lifeline pipe (an inherited fd whose EOF the plugin SDK treats as "parent
// gone"); the per-platform ones (Linux Pdeathsig, a Windows kill-on-close job)
// were judged not worth their cost and side effects.
func runPlugin(ctx context.Context, pluginName, binPath string, args []string) (exitCode int, killedBy os.Signal) {
	cmd := exec.CommandContext(ctx, binPath, args...)
	configurePluginCancel(cmd, pluginCancelGOOS)
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

	if err := runTrackedPlugin(cmd); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// A signalled child reports -1, i.e. ExitPluginSignalled: it is
			// not an exit status, so the caller re-raises the signal rather
			// than letting os.Exit truncate it to 255. Which signal comes
			// from the wait status, because it need not be one this process
			// received — see pluginTerminatingSignal.
			return exitErr.ExitCode(), pluginTerminatingSignal(exitErr.ProcessState)
		}
		// The plugin ran and exited successfully after we cancelled it (it
		// handled the Ctrl-C itself). exec then reports the cancellation
		// instead of the exit — context.Canceled, or the Cancel func's own
		// error — but the plugin's outcome is what the user gets: a TUI that
		// quits cleanly on Ctrl-C exits 0, not "Failed to run plugin".
		if cmd.ProcessState != nil && cmd.ProcessState.Success() && ctx.Err() != nil {
			return 0, nil
		}
		// Prefix with the plugin name so users can tell parent vs child
		// errors apart in mixed stderr.
		fmt.Fprintf(os.Stderr, "Failed to run plugin %s: %v\n", filepath.Base(binPath), err)
		return 1, nil
	}
	return 0, nil
}

// runningPlugins holds the plugin processes this CLI is waiting on, each with
// a channel closed once it has exited, for ForceQuitPlugins.
var runningPlugins = struct {
	sync.Mutex

	procs map[*os.Process]chan struct{}
}{procs: map[*os.Process]chan struct{}{}}

// runTrackedPlugin is cmd.Run with the process registered for
// ForceQuitPlugins while it runs.
func runTrackedPlugin(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err //nolint:wrapcheck // runPlugin reports exec's errors as they are
	}
	proc, exited := cmd.Process, make(chan struct{})
	runningPlugins.Lock()
	runningPlugins.procs[proc] = exited
	runningPlugins.Unlock()
	defer func() {
		runningPlugins.Lock()
		delete(runningPlugins.procs, proc)
		runningPlugins.Unlock()
		close(exited)
	}()
	return cmd.Wait() //nolint:wrapcheck // runPlugin reports exec's errors as they are
}

// pluginForceQuitGrace is how long a force quit by a second Ctrl-C waits for
// running plugins to finish their own force-quit before killing them. A var
// so tests can shorten it.
var pluginForceQuitGrace = pluginCancelGrace

// ForceQuitPlugins ends the plugins this CLI is still waiting on, ahead of a
// force quit (the second signal). Once this process exits nothing else will:
// exec's kill-after-grace timer dies with it, and a child outlives its parent.
//
// sig is the signal that forced the quit. For os.Interrupt — a Ctrl-C, which
// the terminal or console delivers to the plugin as well — the plugin may be
// running its own force-quit (entire-stream flushes its last records), so it
// gets up to pluginForceQuitGrace to exit before it is killed. That window
// never extends the deadline the first signal set: exec's kill after the
// cancel grace (configurePluginCancel) still fires on time, so on Unix a
// second Ctrl-C four seconds after the first leaves about one second. That is
// deliberate — a second Ctrl-C asks to quit sooner, never later — and the
// plugin has been winding down since the first. A further signal on more (a
// third Ctrl-C) ends that window and kills at once. Any other second signal
// (SIGTERM or SIGHUP) reached only this process, so the plugins are killed at
// once.
func ForceQuitPlugins(sig os.Signal, more <-chan os.Signal) {
	runningPlugins.Lock()
	pending := make(map[*os.Process]chan struct{}, len(runningPlugins.procs))
	for proc, exited := range runningPlugins.procs {
		pending[proc] = exited
	}
	runningPlugins.Unlock()
	if sig == os.Interrupt {
		awaitPluginExits(pending, pluginForceQuitGrace, more)
	}
	for proc, exited := range pending {
		select {
		case <-exited:
		default:
			_ = proc.Kill() //nolint:errcheck // best effort; it may have just exited
		}
	}
}

// awaitPluginExits waits until every plugin in pending has exited, grace has
// passed, or a signal arrives on more.
func awaitPluginExits(pending map[*os.Process]chan struct{}, grace time.Duration, more <-chan os.Signal) {
	deadline := time.NewTimer(grace)
	defer deadline.Stop()
	for _, exited := range pending {
		select {
		case <-exited:
		case <-deadline.C:
			return
		case <-more:
			return
		}
	}
}

// pluginCancelGOOS selects configurePluginCancel's platform behaviour; a seam
// so the Windows behaviour is exercised by real child processes everywhere.
var pluginCancelGOOS = runtime.GOOS

// pluginCancelGrace is how long a cancelled plugin may take to exit after
// SIGINT before exec kills it, on platforms where we can send SIGINT.
const pluginCancelGrace = 5 * time.Second

// pluginWindowsCancelGrace is how long a plugin may wind down on Windows after
// the Ctrl-C the console delivered to it, before exec kills it. Longer than
// pluginCancelGrace because the plugin's own wind-down is under way, not
// merely signalled (entire-stream allows itself 5s), and cutting it off loses
// its last records. A var so tests can shorten it.
var pluginWindowsCancelGrace = 30 * time.Second

// configurePluginCancel decides what cancelling the root context (our signal
// handler, on the first Ctrl-C, SIGTERM or SIGHUP) does to a running plugin.
//
// Unix: forward the signal this process received (pluginCancelSignal), then
// kill after pluginCancelGrace. Forwarding the real signal matters: a
// supervisor's SIGTERM must reach the plugin as SIGTERM, not as a Ctrl-C.
// Except a terminal's Ctrl-C (terminalSentInterrupt): that already reached the
// plugin, and a second copy would read as the user's second Ctrl-C.
//
// Windows: send nothing, then kill after pluginWindowsCancelGrace.
// os.Process.Signal supports only Kill there, so sending os.Interrupt fails
// ("not supported by windows") and exec reports that failure instead of the
// plugin's exit. It is also unnecessary: the context is only ever cancelled by
// os.Interrupt (Ctrl-C/Ctrl-Break), which the console has already delivered
// to the plugin, a member of the same console. The kill is still needed: a
// plugin that ignores Ctrl-C would otherwise run on after the CLI gave up on
// it. (A second Ctrl-C ends it sooner: see ForceQuitPlugins.)
func configurePluginCancel(cmd *exec.Cmd, goos string) {
	if goos == windowsGOOS {
		cmd.Cancel = func() error { return nil }
		cmd.WaitDelay = pluginWindowsCancelGrace
		return
	}
	cmd.Cancel = func() error {
		received := procsignal.Load()
		if received == os.Interrupt && pluginTerminalSentInterrupt(cmd.Process.Pid) {
			// The terminal delivered this Ctrl-C to the plugin already.
			return nil
		}
		return cmd.Process.Signal(pluginCancelSignal())
	}
	cmd.WaitDelay = pluginCancelGrace
}

// pluginTerminalSentInterrupt is terminalSentInterrupt, a seam so tests can
// stand in for a terminal.
var pluginTerminalSentInterrupt = terminalSentInterrupt

// pluginCancelSignal is the signal to forward to a plugin when the root
// context is cancelled: the one this process received, or SIGINT when the
// cancellation came from elsewhere.
func pluginCancelSignal() os.Signal {
	if sig := procsignal.Load(); sig != nil {
		return sig
	}
	return os.Interrupt
}
