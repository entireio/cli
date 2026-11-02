package cli

import (
	"github.com/spf13/cobra"
)

// newClusterCmd is the `entire cluster` command group: the catalog of
// data-plane clusters attached to the control plane the active login belongs
// to. Read-only — clusters are provisioned by Entire, not by users — so the
// group carries `list` and nothing else.
func newClusterCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cluster",
		Short: "Show the Entire clusters you can place projects and repos on",
	}
	addControlPlaneFlags(cmd)
	cmd.AddCommand(newClusterListCmd())
	return cmd
}
