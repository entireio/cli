package cli

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/coreapi"
)

var saGrantColumns = []string{colHeaderType, colHeaderName, "ID", colHeaderRole}

func saGrantRow(g coreapi.ServiceAccountGrant) []string {
	return []string{g.ResourceType, orDash(g.ResourceName.Or("")), g.ResourceId, g.Role}
}

func newSAGrantsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "grants <service-account>",
		Short: "List a service account's grants",
		Long:  "Lists the repo and project grants the service account holds. Core shows only grants you can manage.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCoreList(cmd, "No grants found.", saGrantColumns, saGrantRow,
				func(ctx context.Context, c *coreapi.Client) ([]coreapi.ServiceAccountGrant, error) {
					sa, err := resolveSARef(ctx, c, args[0])
					if err != nil {
						return nil, err
					}
					return fetchAllPages(ctx, func(ctx context.Context, cursor string) ([]coreapi.ServiceAccountGrant, string, error) {
						params := coreapi.ListServiceAccountGrantsParams{AccountId: sa.ID}
						if cursor != "" {
							params.PageToken = coreapi.NewOptString(cursor)
						}
						out, err := c.ListServiceAccountGrants(ctx, params)
						if err != nil {
							return nil, "", err
						}
						return out.Grants, out.NextPageToken.Or(""), nil
					})
				})
		},
	}
	addJSONFlag(cmd)
	return cmd
}
