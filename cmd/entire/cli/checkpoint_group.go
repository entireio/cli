package cli

import (
	"errors"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/spf13/cobra"
)

// newCheckpointGroupCmd builds the `entire checkpoint` parent command and
// registers list/explain/tokens/search/resume as children.
func newCheckpointGroupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     cmdCheckpoint,
		Aliases: []string{"cp", cmdCheckpointsAlias},
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
	cmd.AddCommand(newCheckpointCreateCmd())
	cmd.AddCommand(newCheckpointResumeCmd())
	cmd.AddCommand(newExplainCmd())
	cmd.AddCommand(newCheckpointTokensCmd())
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
// machine-readable (--json) and pending-checkpoint (--pending) modes.
//
// Dataset/format matrix:
//
//	(default)            condensed checkpoints on the branch, human view (pager)
//	--json               condensed checkpoints as JSON (branchCheckpointJSON shape)
//	--pending            next-checkpoint preview + task records + logs-only
//	                     points, human list
//	--pending --json     the same as a JSON array — the drop-in replacement
//	                     for the deprecated `rewind --list` bridge
//
// The condensed dataset (entire/checkpoints/v1 for the branch) and the pending
// dataset (strategy.PreviewNextCheckpoint plus strategy.ListPendingCheckpoints;
// task records, logs-only points, condensation IDs) are deliberately distinct —
// see issue #1767.
func newCheckpointListCmd() *cobra.Command {
	var sessionFlag string
	var noPagerFlag bool
	var jsonFlag bool
	var pendingFlag bool

	cmd := &cobra.Command{
		Use:   cmdList,
		Short: "List checkpoints on the current branch",
		Long: `List checkpoints on the current branch.

By default shows condensed checkpoints from the checkpoints branch for the
current branch. Use --pending for work that is not a checkpoint yet:

  - A preview of the next checkpoint for each session in this worktree with
    pending work. Agent turns are recorded in session state, not in git, so
    this work has no checkpoint ID until you commit it. The preview shows the
    turns since the last checkpoint, the files touched, subagent task records,
    and the prompts since the last checkpoint.
  - Subagent task records of other sessions on HEAD (not yet condensed).
  - Logs-only resume points recovered from commits whose logs are already
    condensed.

Output modes:
  --json             Machine-readable JSON instead of the human view.
  --pending          Select the pending dataset described above.
  --pending --json   The pending dataset as a JSON array (replaces the
                     deprecated rewind --list).

Each --pending --json element has id, message, metadata_dir, date (RFC3339),
is_task_checkpoint, is_logs_only, and when set tool_use_id, condensation_id,
session_id and session_prompt. A next-checkpoint preview element has an empty
id, "is_next_checkpoint": true, and a "next_checkpoint" object:

  {"agent": "...", "turns": 2,
   "files_touched": ["..."],
   "task_records": [{"tool_use_id": "...", "subagent_type": "...",
                     "description": "...", "status": "running|completed"}],
   "prompts": ["..."]}

The arrays are always present, possibly empty.

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
	cmd.Flags().BoolVar(&pendingFlag, "pending", false, "List pending work — a preview of the next checkpoint, uncondensed subagent task records, and logs-only resume points")
	return cmd
}
