package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

type experimentalCommandInfo struct {
	Name       string
	Invocation string
	Summary    string
}

var experimentalCommands = []experimentalCommandInfo{
	{
		Name:       "review",
		Invocation: "entire review",
		Summary:    "Run configured review skills against the current branch",
	},
}

func newLabsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "labs",
		Short: "Explore experimental Entire workflows",
		Long:  labsOverview(),
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return nil
			}
			err := fmt.Errorf("unknown labs topic %q", args[0])
			fmt.Fprintf(cmd.ErrOrStderr(),
				"%v\n\nRun `entire labs` to see available experimental commands, or run `entire review --help` for command-specific help.\n",
				err)
			return NewSilentError(err)
		},
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprint(cmd.OutOrStdout(), labsOverview())
		},
	}
	return cmd
}

func labsOverview() string {
	if len(experimentalCommands) == 0 {
		return `Labs

No experimental commands are available in this build.
`
	}

	return `Labs

These are newer Entire workflows we are actively refining. They are available
to try now, but details may change based on feedback.

Available experimental commands:
` + renderExperimentalCommands(experimentalCommands) + `
Try:
  entire review --help
`
}

func renderExperimentalCommands(commands []experimentalCommandInfo) string {
	var out strings.Builder
	for _, info := range commands {
		out.WriteString("  ")
		out.WriteString(padRight(info.Invocation, 16))
		out.WriteByte(' ')
		out.WriteString(info.Summary)
		out.WriteByte('\n')
	}
	return out.String()
}

func padRight(value string, width int) string {
	if len(value) >= width {
		return value
	}
	return value + strings.Repeat(" ", width-len(value))
}
