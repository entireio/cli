package cli

import (
	"errors"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/spf13/cobra"
)

// newCheckpointGroupCmd builds the `entire checkpoint` parent command and
// registers list/explain/tokens/search as children, plus the deprecated rewind.
func newCheckpointGroupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "checkpoint",
		Aliases: []string{"cp", "checkpoints"},
		Short:   "Inspect and search checkpoints",
		Long: `Operations on checkpoints — the persistent records of agent work tied to commits.

Commands:
  list     List checkpoints on the current branch
  explain  Explain a checkpoint, commit, or session
  tokens   Show token usage and optimization recommendations
  search   Search checkpoints (semantic + keyword)

Examples:
  entire checkpoint list
  entire checkpoint explain <id|sha>
  entire checkpoint tokens <id>
  entire checkpoint search "fix login"`,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := paths.WorktreeRoot(cmd.Context()); err != nil {
				return errors.New("not a git repository")
			}
			return nil
		},
	}

	cmd.AddCommand(newCheckpointListCmd())
	cmd.AddCommand(newExplainCmd())
	cmd.AddCommand(newCheckpointTokensCmd())
	cmd.AddCommand(newRewindCmd())
	cmd.AddCommand(newCheckpointSearchCmd())

	return cmd
}

func newCheckpointSearchCmd() *cobra.Command {
	cmd := newSearchCmd()
	cmd.Hidden = false
	return cmd
}

// newCheckpointListCmd wraps the existing branch-default list view.
func newCheckpointListCmd() *cobra.Command {
	var sessionFlag string
	var noPagerFlag bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List checkpoints on the current branch",
		Long: `List checkpoints on the current branch.

Optionally filter by session ID with --session.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if checkDisabledGuard(cmd.Context(), cmd.OutOrStdout()) {
				return nil
			}
			return runExplainBranchWithFilter(cmd.Context(), cmd.OutOrStdout(), noPagerFlag, sessionFlag)
		},
	}

	cmd.Flags().StringVar(&sessionFlag, "session", "", "Filter checkpoints by session ID (or prefix)")
	cmd.Flags().BoolVar(&noPagerFlag, "no-pager", false, "Disable pager output")
	return cmd
}
