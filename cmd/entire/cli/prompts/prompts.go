package prompts

import (
	"github.com/spf13/cobra"
)

const truncatedNoteSuffix = " (truncated)"

func NewCommandGroup() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "prompts",
		Short: "Search and list prompts from your checkpoint history",
		Long: `Search and list prompts from your checkpoint history.

Search prompts by keywords to find decisions and reasoning behind code changes.

Examples:
  entire prompts search "cache decision"
  entire prompts list
  entire prompts show a3b2c4d5e6f7
  entire prompts index --status`,
	}

	cmd.AddCommand(newSearchCmd())
	cmd.AddCommand(newListCmd())
	cmd.AddCommand(newShowCmd())
	cmd.AddCommand(newIndexCmd())

	return cmd
}
