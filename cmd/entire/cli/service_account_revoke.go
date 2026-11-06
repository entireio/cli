package cli

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/coreapi"
)

func newSARevokeCmd() *cobra.Command {
	var repo, project string
	cmd := &cobra.Command{
		Use:   "revoke <service-account>",
		Short: "Revoke a service account's access to a repo or project",
		Example: "  entire service-account revoke acme/ci --repo web\n" +
			"  entire service-account revoke acme/ci --project acme",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
				sa, err := resolveSARef(ctx, c, args[0])
				if err != nil {
					return err
				}
				target, err := resolveSATarget(ctx, c, sa, repo, project)
				if err != nil {
					return err
				}
				return revokeGrant(cmd, target.label+" from "+sa.label(), func() error {
					return c.RevokeServiceAccountAccess(ctx, coreapi.RevokeServiceAccountAccessParams{
						AccountId:    sa.ID,
						ResourceType: coreapi.RevokeServiceAccountAccessResourceType(target.kind),
						ResourceId:   target.id,
					})
				})
			})
		},
	}
	addSATargetFlags(cmd, &repo, &project)
	return cmd
}
