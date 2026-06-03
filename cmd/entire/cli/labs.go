package cli

import (
	"fmt"
	"strings"
	"unicode/utf8"

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
	{
		Name:       "investigate",
		Invocation: "entire investigate",
		Summary:    "Run a multi-agent investigation against a topic, issue, or seed doc",
	},
	{
		Name:       "replay",
		Invocation: "entire replay",
		Summary:    "Replay checkpoint tasks in isolated worktrees",
	},
	{
		Name:       "eval",
		Invocation: "entire eval",
		Summary:    "Run private agent benchmarks from Entire checkpoints",
	},
	{
		Name:       "org",
		Invocation: "entire org",
		Summary:    "Manage Entire organizations (create, list)",
	},
	{
		Name:       "project",
		Invocation: "entire project",
		Summary:    "Manage Entire projects (create, list)",
	},
	{
		Name:       "repo",
		Invocation: "entire repo",
		Summary:    "Manage Entire repositories (create, list, get, delete)",
	},
	{
		Name:       "grant",
		Invocation: "entire grant",
		Summary:    "Manage access grants and org membership (org, project, repo)",
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
			fmt.Fprintf(cmd.ErrOrStderr(), "%v\n\n%s\n", err, labsTopicHint(args[0]))
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
  entire investigate --help
  entire replay --help
  entire eval --help
  entire org --help
  entire project --help
  entire repo --help
  entire grant --help
`
}

func labsTopicHint(topic string) string {
	for _, info := range experimentalCommands {
		if topic == info.Name {
			return fmt.Sprintf("Run `entire labs` to see available experimental commands, or run `%s --help` for command-specific help.", info.Invocation)
		}
	}
	return "Run `entire labs` to see available experimental commands and their command-specific help."
}

func renderExperimentalCommands(commands []experimentalCommandInfo) string {
	width := 0
	for _, info := range commands {
		if w := utf8.RuneCountInString(info.Invocation); w > width {
			width = w
		}
	}

	var out strings.Builder
	for _, info := range commands {
		out.WriteString("  ")
		out.WriteString(padRight(info.Invocation, width))
		out.WriteByte(' ')
		out.WriteString(info.Summary)
		out.WriteByte('\n')
	}
	return out.String()
}

func padRight(value string, width int) string {
	n := utf8.RuneCountInString(value)
	if n >= width {
		return value
	}
	return value + strings.Repeat(" ", width-n)
}
