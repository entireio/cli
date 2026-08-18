package cli

import (
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"

	"github.com/spf13/cobra"
)

// newEntityDeltasCmd creates the hidden command that computes a session's
// entity deltas and backfills them onto an already-written checkpoint. It is
// invoked only by the detached child that condensation forks (see
// strategy/entity_deltas.go) and should not be called directly.
//
// The single argument is the path to the job file the scheduler wrote; the
// runner consumes and deletes it.
func newEntityDeltasCmd() *cobra.Command {
	return &cobra.Command{
		Use:    strategy.EntityDeltasCommandName,
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			ctx := cmd.Context()
			// Detached child with discarded stdout/stderr: initialize file
			// logging so a failing backfill (missing producer, wedged run, a
			// store write that errors) is diagnosable in .entire/logs/entire.log
			// rather than vanishing. Guard on WorktreeRoot first — matching
			// __refresh_trail_enablement — so a child whose worktree was removed
			// between spawn and exec doesn't create a stray .entire/logs/ in an
			// arbitrary directory.
			if _, err := paths.WorktreeRoot(ctx); err == nil {
				logging.SetLogLevelGetter(GetLogLevel)
				if err := logging.Init(ctx, ""); err == nil {
					defer logging.Close()
				}
			}
			strategy.RunEntityDeltasBackfill(ctx, args[0])
		},
	}
}
