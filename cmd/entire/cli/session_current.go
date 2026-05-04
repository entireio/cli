package cli

import (
	"errors"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/spf13/cobra"
)

func newSessionCurrentCmd() *cobra.Command {
	var jsonFlag bool

	cmd := &cobra.Command{
		Use:   "current",
		Short: "Show the active session for the current worktree",
		Long: `Show the most recently active session for the current worktree.

Resolves the session that is writing checkpoints in this directory right now,
preferring sessions from the current worktree and falling back to the most
recent session if no state matches this worktree. Equivalent to running
'sessions info' on the session ID returned by FindMostRecentSession.

Examples:
  entire session current
  entire session current --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if _, err := paths.WorktreeRoot(ctx); err != nil {
				cmd.SilenceUsage = true
				return errors.New("not a git repository")
			}

			sessionID := strategy.FindMostRecentSession(ctx)
			if sessionID == "" {
				fmt.Fprintln(cmd.OutOrStdout(), "No active session found in this worktree.")
				return nil
			}

			return runSessionInfo(ctx, cmd, sessionID, jsonFlag)
		},
	}

	cmd.Flags().BoolVar(&jsonFlag, "json", false, "Output as JSON")
	return cmd
}
