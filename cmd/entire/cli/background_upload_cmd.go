package cli

import (
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/bgupload"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// newBackgroundUploadCmd creates the hidden command a hook spawns detached to
// run one kind of background upload (see bgupload). Not for direct use.
func newBackgroundUploadCmd() *cobra.Command {
	return &cobra.Command{
		Use:    bgupload.WorkerCommand + " <kind>",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Detached, with stdio on the null device: log to .entire/logs.
			ensureLogger(cmd)
			if args[0] == checkpoint.UploadKind {
				strategy.RunCheckpointUploadWorker(cmd.Context())
			}
			return nil
		},
	}
}
