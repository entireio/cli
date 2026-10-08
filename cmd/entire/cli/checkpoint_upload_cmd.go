package cli

import (
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// newCheckpointUploadCmd creates the hidden command the pre-push hook spawns
// detached to finish a git-refs checkpoint upload that did not fit its inline
// budget (see strategy.RunCheckpointUploadWorker). Not for direct use.
func newCheckpointUploadCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "__checkpoint_upload",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Detached child with its stdio on the null device: attach the file
			// logger so a failed background upload is diagnosable in
			// .entire/logs/entire.log. See newSweepSessionsCmd.
			ensureLogger(cmd)
			strategy.RunCheckpointUploadWorker(cmd.Context())
			return nil
		},
	}
}
