package cli

import (
	"errors"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/spf13/cobra"
)

// newCheckpointGroupCmd builds the `entire checkpoint` parent command and
// registers list/explain/rewind/search as children.
func newCheckpointGroupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "checkpoint",
		Aliases: []string{"cp", "checkpoints"},
		Short:   "Inspect, rewind, and search checkpoints",
		Long: `Operations on checkpoints — the persistent records of agent work tied to commits.

Commands:
  list     List checkpoints on the current branch
  explain  Explain a checkpoint, commit, or session
  rewind   Browse and rewind to a checkpoint
  search   Search checkpoints (semantic + keyword)

Examples:
  entire checkpoint list
  entire checkpoint explain <id|sha>
  entire checkpoint rewind --to <id>
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
	cmd.AddCommand(newRewindCmd())
	cmd.AddCommand(newCheckpointSearchCmd())

	return cmd
}

func newCheckpointSearchCmd() *cobra.Command {
	cmd := newSearchCmd()
	cmd.Hidden = false
	return cmd
}

// newCheckpointListCmd wraps the branch list view, with optional revision-range scoping.
func newCheckpointListCmd() *cobra.Command {
	var sessionFlag string
	var noPagerFlag bool

	cmd := &cobra.Command{
		Use:   "list [<revrange>]",
		Short: "List checkpoints reachable from HEAD or a revision range",
		Long: `List checkpoints reachable from HEAD, mirroring git log semantics
(checkpoints on merged feature branches are included).

Optionally restrict to a git-style revision range:
  list main..HEAD     — checkpoints on this branch but not in main
  list main..         — same (HEAD is implicit)
  list <ref>          — full history of <ref>

Filter further by session ID with --session.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if checkDisabledGuard(cmd.Context(), cmd.OutOrStdout()) {
				return nil
			}
			var revRangeArg string
			if len(args) > 0 {
				revRangeArg = args[0]
			}
			return runExplainBranchWithFilter(cmd.Context(), cmd.OutOrStdout(), noPagerFlag, sessionFlag, revRangeArg)
		},
	}

	cmd.Flags().StringVar(&sessionFlag, "session", "", "Filter checkpoints by session ID (or prefix)")
	cmd.Flags().BoolVar(&noPagerFlag, "no-pager", false, "Disable pager output")
	return cmd
}
