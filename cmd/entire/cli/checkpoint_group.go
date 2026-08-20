package cli

import (
	"errors"

	"github.com/entireio/cli/cmd/entire/cli/experimental"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/spf13/cobra"
)

// newCheckpointGroupCmd builds the `entire checkpoint` parent command and
// registers list/explain/tokens/search/resume as children.
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
	cmd.AddCommand(newCheckpointResumeCmd())
	cmd.AddCommand(newExplainCmd())
	cmd.AddCommand(newCheckpointTokensCmd())
	experimental.Register(cmd, newCheckpointPolicyCmd()) // 'checkpoint policy' (experimental)
	cmd.AddCommand(newCheckpointSearchCmd())

	return cmd
}

func newCheckpointSearchCmd() *cobra.Command {
	cmd := newSearchCmd()
	// newSearchCmd's examples use the canonical top-level `entire search`
	// prefix; under this `checkpoint search` alias they must match this path.
	cmd.Example = "  entire checkpoint search \"retry backoff\" --json\n  entire checkpoint search \"retry backoff\" --json --compact\n  entire checkpoint search \"auth timeout author:alice date:week\"\n  entire checkpoint search --code \"parseToken\""
	return cmd
}

// newCheckpointListCmd wraps the existing branch-default list view and adds
// machine-readable (--json) and pending-rewind-point (--pending) modes.
//
// Dataset/format matrix:
//
//	(default)            condensed checkpoints on the branch, human view (pager)
//	--json               condensed checkpoints as JSON (branchCheckpointJSON shape)
//	--pending            live shadow-branch rewind points, human list
//	--pending --json     live shadow-branch rewind points as JSON — the drop-in
//	                     replacement for the deprecated `rewind --list` bridge
//
// The condensed dataset (entire/checkpoints/v1 for the branch) and the pending
// dataset (strategy.GetRewindPoints; task checkpoints, logs-only points,
// condensation IDs) are deliberately distinct — see issue #1767.
func newCheckpointListCmd() *cobra.Command {
	var sessionFlag string
	var noPagerFlag bool
	var jsonFlag bool
	var pendingFlag bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List checkpoints on the current branch",
		Long: `List checkpoints on the current branch.

By default shows condensed checkpoints from the checkpoints branch for the
current branch. Use --pending to list the live session's shadow-branch rewind
points instead (task checkpoints, logs-only points, condensation IDs).

Output modes:
  --json             Machine-readable JSON instead of the human view.
  --pending          Select the live shadow-branch rewind-point dataset.
  --pending --json   Rewind points as JSON (replaces the deprecated rewind --list).

Optionally filter condensed checkpoints by session ID with --session
(not applicable with --pending).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if checkDisabledGuard(cmd.Context(), cmd.OutOrStdout()) {
				return nil
			}
			ctx := cmd.Context()
			w := cmd.OutOrStdout()
			errW := cmd.ErrOrStderr()

			// --session filters the condensed dataset only; the pending dataset
			// mirrors the historical rewind --list, which had no session filter.
			if pendingFlag && sessionFlag != "" {
				return errors.New("--session cannot be combined with --pending")
			}

			switch {
			case pendingFlag && jsonFlag:
				return runCheckpointPendingListJSON(ctx, w)
			case pendingFlag:
				return runCheckpointPendingListHuman(ctx, w)
			case jsonFlag:
				return runExplainListJSON(ctx, w, errW, sessionFlag, 0)
			default:
				return runExplainBranchWithFilter(ctx, w, errW, noPagerFlag, sessionFlag)
			}
		},
	}

	cmd.Flags().StringVar(&sessionFlag, "session", "", "Filter checkpoints by session ID (or prefix)")
	cmd.Flags().BoolVar(&noPagerFlag, "no-pager", false, "Disable pager output")
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "Output as JSON instead of the human view")
	cmd.Flags().BoolVar(&pendingFlag, "pending", false, "List the live session's shadow-branch rewind points instead of condensed checkpoints")
	return cmd
}
