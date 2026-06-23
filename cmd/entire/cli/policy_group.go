package cli

import (
	"errors"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/spf13/cobra"
)

func newPolicyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "policy",
		Short:  "Manage repo-wide Entire policies",
		Hidden: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := paths.WorktreeRoot(cmd.Context()); err != nil {
				return errors.New("not a git repository")
			}
			return nil
		},
	}
	cmd.AddCommand(newPolicyCheckpointCmd())
	return cmd
}
