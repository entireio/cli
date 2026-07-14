package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/entireio/cli/cmd/entire/cli"
	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/entireio/cli/internal/procsignal"
	"github.com/spf13/cobra"
)

func main() {
	// Resolve version/commit from build info before anything reads them.
	versioninfo.Load()

	// Create context that cancels on interrupt
	ctx, cancel := context.WithCancel(context.Background())

	// Handle interrupt signals
	sigChan := make(chan os.Signal, 1)
	signals := []os.Signal{os.Interrupt}
	if runtime.GOOS != "windows" {
		signals = append(signals, syscall.SIGTERM)
	}
	signal.Notify(sigChan, signals...)
	go func() {
		// First signal: cancel the context so in-flight work unwinds
		// cleanly. signal.Notify has disabled Go's default "signal
		// terminates" behavior, so without the second read below a user
		// who Ctrl-C's again during a slow/stuck shutdown (e.g. a keyring
		// read blocked in a subprocess we can't cancel) would find every
		// further Ctrl-C swallowed. The second read restores an escape
		// hatch: signal again to force-exit.
		//
		// We remember which signal fired so the eventual termination
		// re-raises that same signal — a SIGTERM (from a supervisor /
		// container stop) must exit 143, not masquerade as a SIGINT 130.
		sig := <-sigChan
		procsignal.Store(sig)
		if sig == os.Interrupt {
			fmt.Fprintln(os.Stderr, "\nInterrupting… press Ctrl-C again to force quit.")
		} else {
			fmt.Fprintln(os.Stderr, "\nReceived termination signal, shutting down… signal again to force quit.")
		}
		cancel()
		<-sigChan
		dieFromSignal(sig)
	}()

	// Create and execute root command
	rootCmd := cli.NewRootCmd()

	// Make managed-installed plugins discoverable by the kubectl-style
	// dispatcher: prepend the managed bin dir to PATH before resolution.
	// Idempotent and silent on failure (managed installs simply won't be
	// found this run; PATH-installed plugins still work). The closure
	// restores PATH so built-in commands and their subprocesses don't
	// inherit the prepended dir. When a plugin runs, we skip the restore
	// — the os.Exit ends the process, and the plugin child intentionally
	// inherits the prepended PATH so it can spawn sibling managed plugins.
	restorePATH := cli.PrependPluginBinDirToPATH(ctx)

	if handled, code := cli.MaybeRunPlugin(ctx, rootCmd, os.Args[1:]); handled {
		cancel()
		os.Exit(code)
	}
	restorePATH()

	// Retired env vars fail every built-in command up front; a removed knob
	// must never be silently ignored. Plugins (above) are exempt — what they
	// read is their business.
	if err := api.RejectRemovedAuthEnv(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		cancel()
		os.Exit(1)
	}

	executed, err := rootCmd.ExecuteContextC(ctx)
	if err != nil {
		var silent *cli.SilentError

		switch {
		case errors.Is(err, context.Canceled) && procsignal.Load() != nil:
			// A signal cancelled the root context (our handler fired) or a
			// keyring read was aborted by Ctrl-C. Don't dump the raw
			// transport/keyring cancellation string ("...: context canceled",
			// "read access token: signal: interrupt") as if it were a failure
			// — die quietly by re-raising the signal that triggered it (see
			// dieFromSignal) so an enclosing `while ...; do entire; done` loop
			// actually breaks on a single Ctrl-C, and a SIGTERM shutdown still
			// exits 143.
			//
			// We gate on a signal having been recorded rather than on the
			// error type alone: a context.Canceled that arose without a signal
			// (e.g. an internally-cancelled sub-context) is a genuine error
			// and must fall through to normal reporting, not masquerade as a
			// user abort (which would also wrongly break an enclosing loop).
			// procsignal is the shared source of truth written both by the
			// handler above and by the keyring interrupt path; the latter
			// records the signal on this same goroutine before returning, so
			// this Load never races that write.
			cancel()
			dieFromSignal(terminatingSignal())
		case errors.As(err, &silent):
			// Command already printed the error
		case strings.Contains(err.Error(), "unknown command") || strings.Contains(err.Error(), "unknown flag"):
			showSuggestion(rootCmd, err)
		case isPositionalArgError(err):
			// Arg-count errors come from cobra's own validators (e.g.
			// cobra.ExactArgs) and surface as "accepts N arg(s), received M".
			// Show the failing subcommand's usage so the user sees what it
			// actually expects — rootCmd's usage isn't useful for a
			// subcommand-level arg mismatch. ExecuteContextC returns the
			// deepest matched command, which is the one that failed.
			showSuggestion(executed, err)
		default:
			fmt.Fprintln(rootCmd.OutOrStderr(), err)
		}

		cancel()
		os.Exit(1)
	}
	if cli.ShouldCheckCheckpointPolicyWarning(executed) {
		cli.WarnCheckpointPolicyIfNeeded(ctx, rootCmd.ErrOrStderr(), versioninfo.Version)
	}
	cancel() // Cleanup on successful exit
}

// terminatingSignal returns the signal that cancelled the root context,
// defaulting to SIGINT when the cancellation came from something other than a
// recorded terminating signal (so a stray context.Canceled still exits 130).
// The recorded signal lives in the shared procsignal package, written by the
// handler goroutine (SIGINT/SIGTERM) and by the keyring interrupt path (SIGINT).
func terminatingSignal() os.Signal {
	if s := procsignal.Load(); s != nil {
		return s
	}
	return os.Interrupt
}

// dieFromSignal terminates the process as if it had been killed by sig, rather
// than exiting normally. The distinction matters to an interactive shell: it
// only aborts a `while true; do entire ...; done` loop when the child is
// *killed by* SIGINT (WIFSIGNALED). A plain os.Exit(130) is an ordinary exit,
// so the loop keeps respawning entire and Ctrl-C never escapes it. Re-raising
// the actual signal also keeps a SIGTERM shutdown reporting the conventional
// 143 (not 130). We reset sig to its default disposition, re-raise it to
// ourselves, and briefly wait for delivery; if the re-raise can't be delivered
// (e.g. Windows, where signal-to-self is unsupported) we fall back to a
// conventional 128+signal exit so we never hang.
func dieFromSignal(sig os.Signal) {
	signal.Reset(sig)
	if p, err := os.FindProcess(os.Getpid()); err == nil {
		if err := p.Signal(sig); err == nil {
			time.Sleep(500 * time.Millisecond) // signal delivery ends the process well before this elapses
		}
	}
	os.Exit(exitCodeForSignal(sig))
}

// exitCodeForSignal maps a signal to the conventional 128+signum exit code
// (130 for SIGINT, 143 for SIGTERM), falling back to 130 for a signal that
// doesn't carry a numeric value on this platform.
func exitCodeForSignal(sig os.Signal) int {
	if s, ok := sig.(syscall.Signal); ok {
		return 128 + int(s)
	}
	return 130
}

// isPositionalArgError reports whether err looks like a cobra positional-
// arg validator failure. cobra's stock validators (ExactArgs, NoArgs,
// MinimumNArgs, MaximumNArgs, RangeArgs) all surface error strings
// containing "arg(s)", and that substring doesn't appear in cobra's other
// error paths or in the cli's own errors — so it's a stable, cheap
// discriminator without reaching into cobra internals.
func isPositionalArgError(err error) bool {
	return strings.Contains(err.Error(), "arg(s)")
}

func showSuggestion(cmd *cobra.Command, err error) {
	// Print usage first (brew style)
	fmt.Fprint(cmd.OutOrStderr(), cmd.UsageString())
	fmt.Fprintf(cmd.OutOrStderr(), "\nError: Invalid usage: %v\n", err)
}
