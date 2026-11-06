package cli

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/coreapi"
)

func newSAGetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <service-account>",
		Short: "Show a service account",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCoreObject(cmd, saColumns, saRow, func(ctx context.Context, c *coreapi.Client) (*coreapi.ServiceAccount, error) {
				r, err := resolveSARef(ctx, c, args[0])
				if err != nil {
					return nil, err
				}
				if r.record != nil {
					return r.record, nil
				}
				return c.GetServiceAccount(ctx, coreapi.GetServiceAccountParams{AccountId: r.ID})
			})
		},
	}
	addJSONFlag(cmd)
	return cmd
}
