package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/coreapi"
)

// newOrgCmd is the hidden `entire org` command group: create, list, get, and
// delete organizations on the Entire control plane. Surfaced via `entire
// labs` while the control-plane surface matures.
func newOrgCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "org",
		Short:  "Manage Entire organizations",
		Hidden: true,
	}
	addControlPlaneFlags(cmd)
	cmd.AddCommand(newOrgCreateCmd())
	cmd.AddCommand(newOrgListCmd())
	cmd.AddCommand(newOrgGetCmd())
	cmd.AddCommand(newOrgDeleteCmd())
	return cmd
}

// orgColumns is the human table/field view of an org, shared by list and
// any future `org get`.
var orgColumns = []string{"ID", "NAME", "REGION", "CREATED"}

func orgRow(o coreapi.Org) []string {
	return []string{o.ID, o.Name, o.Region, o.CreatedAt.Format("2006-01-02")}
}

func newOrgCreateCmd() *cobra.Command {
	var region string
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create an organization",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCoreMutation(cmd, func(ctx context.Context, c *coreapi.Client) (string, any, error) {
				body := &coreapi.CreateOrgInputBody{Name: args[0]}
				if region != "" {
					body.Region = coreapi.NewOptString(region)
				}
				org, err := c.CreateOrg(ctx, body)
				if err != nil {
					return "", nil, err
				}
				return fmt.Sprintf("✓ Created org %s (%s)", org.Name, org.ID), org, nil
			})
		},
	}
	cmd.Flags().StringVar(&region, "region", "", "Jurisdiction slug (defaults to the server's home jurisdiction)")
	return cmd
}

func newOrgListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List organizations you can see",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCoreList(cmd, "No organizations found.", orgColumns, orgRow, func(ctx context.Context, c *coreapi.Client) ([]coreapi.Org, error) {
				return fetchAllPages(ctx, func(ctx context.Context, cursor string) ([]coreapi.Org, string, error) {
					params := coreapi.ListOrgsParams{}
					if cursor != "" {
						params.PageToken = coreapi.NewOptString(cursor)
					}
					out, err := c.ListOrgs(ctx, params)
					if err != nil {
						return nil, "", err
					}
					return out.Orgs, out.NextPageToken.Or(""), nil
				})
			})
		},
	}
}

func newOrgGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <org>",
		Short: "Show an organization by name or ULID",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCoreObject(cmd, orgColumns, orgRow, func(ctx context.Context, c *coreapi.Client) (*coreapi.Org, error) {
				orgID, err := resolveOrgRef(ctx, c, args[0])
				if err != nil {
					return nil, err
				}
				return c.GetOrg(ctx, coreapi.GetOrgParams{OrgId: orgID})
			})
		},
	}
}

func newOrgDeleteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <org>",
		Short: "Delete an organization by name or ULID",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runControlPlaneDelete(cmd, "org", args[0],
				func(ctx context.Context, c *coreapi.Client) (string, error) {
					return resolveOrgRef(ctx, c, args[0])
				},
				func(ctx context.Context, c *coreapi.Client, id string) error {
					return c.DeleteOrg(ctx, coreapi.DeleteOrgParams{OrgId: id})
				})
		},
	}
	addForceFlag(cmd)
	return cmd
}
