package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli"
	"github.com/spf13/cobra"
)

func main() {
	rootCmd := cli.NewRootCmd()
	err := rootCmd.Execute()

	if err != nil {
		var silent *cli.SilentError

		switch {
		case errors.As(err, &silent):
			// Command already printed the error
		case strings.Contains(err.Error(), "unknown command") || strings.Contains(err.Error(), "unknown flag"):
			showSuggestion(rootCmd, err)
		default:
			fmt.Fprintln(rootCmd.OutOrStderr(), err)
		}

		os.Exit(1)
	}
}

func showSuggestion(cmd *cobra.Command, err error) {
	// Print usage first (brew style)
	fmt.Fprint(cmd.OutOrStderr(), cmd.UsageString())
	fmt.Fprintf(cmd.OutOrStderr(), "\nError: Invalid usage: %v\n", err)
}
