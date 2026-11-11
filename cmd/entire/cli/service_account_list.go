package cli

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/coreapi"
)

func newSAListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list <project>",
		Short: "List a project's service accounts",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCoreList(cmd, "No service accounts found.", saListColumns, saListRow,
				func(ctx context.Context, c *coreapi.Client) ([]coreapi.ServiceAccountWithGrants, error) {
					projID, err := resolveProjectRef(ctx, c, args[0])
					if err != nil {
						return nil, err
					}
					return listProjectSAs(ctx, c, projID)
				})
		},
	}
	addJSONFlag(cmd)
	return cmd
}
