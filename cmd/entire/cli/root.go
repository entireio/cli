package cli

import (
	"fmt"
	"runtime"

	"entire.io/cli/cmd/entire/cli/commands"
	"github.com/spf13/cobra"
)

const gettingStarted = `

Getting Started:
  To get started with Entire CLI, run 'entire enable' to configure
  your environment. For more information, visit:
  https://entire.io/docs/cli/getting-started

`

const accessibilityHelp = `
Environment Variables:
  ACCESSIBLE    Set to any value (e.g., ACCESSIBLE=1) to enable accessibility
                mode. This uses simpler text prompts instead of interactive
                TUI elements, which works better with screen readers.
`

// Version information (can be set at build time)
var (
	Version = "dev"
	Commit  = "unknown"
)

func NewRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "entire",
		Short: "Entire CLI",
		Long:  "A command-line interface for Entire" + gettingStarted + accessibilityHelp,
		// Let main.go handle error printing to avoid duplication
		SilenceErrors: true,
		// Hide completion command from help but keep it functional
		CompletionOptions: cobra.CompletionOptions{
			HiddenDefaultCmd: true,
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	// Add subcommands here
	cmd.AddCommand(newRewindCmd())
	cmd.AddCommand(newResumeCmd())
	cmd.AddCommand(newSessionCmd())
	cmd.AddCommand(newEnableCmd())
	cmd.AddCommand(newDisableCmd())
	cmd.AddCommand(newStatusCmd())
	cmd.AddCommand(newHooksCmd())
	cmd.AddCommand(newVersionCmd())
	cmd.AddCommand(newExplainCmd())

	// Replace default help command with custom one that supports -t flag
	cmd.SetHelpCommand(commands.NewHelpCmd(cmd))

	return cmd
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Show version information",
		Run: func(_ *cobra.Command, _ []string) {
			fmt.Printf("Entire CLI %s (%s)\n", Version, Commit)
			fmt.Printf("Go version: %s\n", runtime.Version())
			fmt.Printf("OS/Arch: %s/%s\n", runtime.GOOS, runtime.GOARCH)
		},
	}
}
