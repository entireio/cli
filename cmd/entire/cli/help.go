package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// NewHelpCmd creates a custom help command that supports a hidden -t flag
// to display the entire command tree.
func NewHelpCmd(rootCmd *cobra.Command) *cobra.Command {
	var showTree bool

	helpCmd := &cobra.Command{
		Use:   "help [command]",
		Short: "Help about any command",
		Long: `Provides help for any Entire CLI subcommand.
Simply type '` + rootCmd.Name() + ` help [command]' for full details.`,
		Run: func(_ *cobra.Command, args []string) {
			if showTree {
				printCommandTree(rootCmd)
				return
			}

			// Default help behavior
			targetCmd, _, err := rootCmd.Find(args)
			if err != nil || targetCmd == nil {
				targetCmd = rootCmd
			}
			targetCmd.Help() //nolint:errcheck,gosec // Help() only fails on write errors to stdout
		},
	}

	helpCmd.Flags().BoolVarP(&showTree, "tree", "t", false, "Show full command tree")
	helpCmd.Flags().MarkHidden("tree") //nolint:errcheck,gosec // flag is defined above

	return helpCmd
}

func printCommandTree(cmd *cobra.Command) {
	fmt.Println(cmd.Name())
	printChildren(cmd, "")
}

func printChildren(cmd *cobra.Command, indent string) {
	visibleCmds := getVisibleCommands(cmd)

	for i, sub := range visibleCmds {
		isLast := i == len(visibleCmds)-1
		printNode(sub, indent, isLast)
	}
}

func printNode(cmd *cobra.Command, indent string, isLast bool) {
	var branch, childIndent string
	if isLast {
		branch = "└── "
		childIndent = indent + "    "
	} else {
		branch = "├── "
		childIndent = indent + "│   "
	}

	fmt.Printf("%s%s%s", indent, branch, cmd.Name())
	if cmd.Short != "" {
		fmt.Printf(" - %s", cmd.Short)
	}
	fmt.Println()

	printChildren(cmd, childIndent)
}

func getVisibleCommands(cmd *cobra.Command) []*cobra.Command {
	var visible []*cobra.Command
	for _, sub := range cmd.Commands() {
		if !sub.Hidden && sub.Name() != "help" {
			visible = append(visible, sub)
		}
	}
	return visible
}
