package cli

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/coreapi"
)

func newSADeleteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <service-account>",
		Short: "Delete a service account",
		Long:  "Deletes the service account with its grants and bindings. Workflows bound to it stop working.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runControlPlaneDelete(cmd, "service account", args[0],
				func(ctx context.Context, c *coreapi.Client) (resolvedRef, error) {
					r, err := resolveSARef(ctx, c, args[0])
					return resolvedRef{ID: r.ID, Name: r.Name}, err
				},
				func(ctx context.Context, c *coreapi.Client, id string) error {
					return c.DeleteServiceAccount(ctx, coreapi.DeleteServiceAccountParams{AccountId: id})
				})
		},
	}
	addForceFlag(cmd)
	return cmd
}
