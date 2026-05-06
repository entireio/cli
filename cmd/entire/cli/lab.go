package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

type labCommandInfo struct {
	Name       string
	Invocation string
	Summary    string
}

var labCommands = []labCommandInfo{
	{
		Name:       "review",
		Invocation: "entire review",
		Summary:    "Run configured review skills against the current branch",
	},
}

func newLabCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "lab",
		Short: "Explore experimental Entire workflows",
		Long:  labOverview(),
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return nil
			}
			err := fmt.Errorf("unknown lab topic %q", args[0])
			fmt.Fprintf(cmd.ErrOrStderr(),
				"%v\n\nRun `entire lab` to see available lab commands, or run `entire review --help` for command-specific help.\n",
				err)
			return NewSilentError(err)
		},
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprint(cmd.OutOrStdout(), labOverview())
		},
	}
	return cmd
}

func labOverview() string {
	if len(labCommands) == 0 {
		return `Lab commands

No lab commands are available in this build.
`
	}

	return `Lab commands

These are newer Entire workflows we are actively refining. They are available
to try now, but details may change based on feedback.

Available lab commands:
` + renderLabCommands(labCommands) + `
Try:
  entire review --help
`
}

func renderLabCommands(commands []labCommandInfo) string {
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
