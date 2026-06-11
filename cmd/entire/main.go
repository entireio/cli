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

	"github.com/entireio/cli/cmd/entire/cli"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
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
		<-sigChan
		cancel()
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

	executed, err := rootCmd.ExecuteContextC(ctx)
	if err != nil {
		var silent *cli.SilentError

		switch {
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
	cancel() // Cleanup on successful exit
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
