package cli

import (
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
			// Detached child with discarded stdout/stderr: ensureLogger attaches
			// file logging so a failing backfill is diagnosible in
			// .entire/logs/entire.log rather than vanishing.
			ensureLogger(cmd)
			strategy.RunEntityDeltasBackfill(cmd.Context(), args[0])
		},
	}
}
