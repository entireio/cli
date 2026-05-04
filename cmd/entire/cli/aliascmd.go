package cli

import "github.com/spf13/cobra"

// hideAsAlias marks cmd as a hidden top-level shortcut that prints a one-line
// hint pointing at the canonical command. Cobra's Deprecated field renders the
// hint to stderr on every invocation while keeping the command functional.
func hideAsAlias(cmd *cobra.Command, canonical string) *cobra.Command {
	cmd.Hidden = true
	cmd.Deprecated = "use '" + canonical + "' instead"
	return cmd
}
