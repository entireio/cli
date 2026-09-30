package cli

import (
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// newOPFScanCmd creates the hidden command the pre-push hook spawns detached
// when it held checkpoints back because OPF had not scanned them yet (see
// strategy.RunOPFScan). It scans in the background and then delivers to the
// remote the spawning push named. Not for direct use: `entire push` is the
// interactive surface for delivery.
func newOPFScanCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "__opf_scan <remote>",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Detached child with discarded stdout/stderr: attach the file
			// logger so a scan or delivery that cannot finish is diagnosable
			// in .entire/logs/entire.log. The root PersistentPostRun closes it.
			ensureLogger(cmd)
			// RunOPFScan is best-effort and never returns a non-nil error;
			// nothing watches this child's exit code.
			return strategy.RunOPFScan(cmd.Context(), args[0])
		},
	}
}
