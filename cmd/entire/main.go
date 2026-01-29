package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"entire.io/cli/cmd/entire/cli"
	"github.com/spf13/cobra"
)

func main() {
	// Create context that cancels on interrupt
	ctx, cancel := context.WithCancel(context.Background())

	// Handle interrupt signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		cancel()
	}()

	// Create and execute root command
	rootCmd := cli.NewRootCmd()
	err := rootCmd.ExecuteContext(ctx)

	if err != nil {
		var silent *cli.SilentError

		switch {
		case errors.As(err, &silent):
			// Command already printed the error
		case strings.Contains(err.Error(), "unknown command"):
			showSuggestion(rootCmd)
		default:
			fmt.Fprintln(rootCmd.OutOrStderr(), err)
		}

		cancel()
		os.Exit(1)
	}
	cancel() // Cleanup on successful exit
}

func showSuggestion(cmd *cobra.Command) {
	// Print usage first (brew style)
	fmt.Fprint(cmd.OutOrStderr(), cmd.UsageString())

	// Build error message with optional suggestion
	errMsg := fmt.Sprintf("Unknown command: %s %s", cmd.CommandPath(), os.Args[1])
	suggestions := cmd.SuggestionsFor(os.Args[1])
	if len(suggestions) > 0 {
		errMsg += fmt.Sprintf(". Did you mean \"%s\"?", suggestions[0])
	}

	// Print error at the end
	fmt.Fprintf(cmd.OutOrStderr(), "\nError: Invalid usage: %s\n", errMsg)
}
