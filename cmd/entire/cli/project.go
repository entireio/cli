package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/coreapi"
)

// newProjectCmd is the hidden `entire project` command group: create, list,
// get, and delete projects on the Entire control plane. Surfaced via `entire
// labs`.
func newProjectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "project",
		Short:  "Manage Entire projects",
		Hidden: true,
	}
	addControlPlaneFlags(cmd)
	cmd.AddCommand(newProjectCreateCmd())
	cmd.AddCommand(newProjectListCmd())
	cmd.AddCommand(newProjectGetCmd())
	cmd.AddCommand(newProjectDeleteCmd())
	return cmd
}

// projectColumns is the human table/field view of a project.
var projectColumns = []string{"ID", "NAME", "OWNER-TYPE", "OWNER", "REGION"}

func projectRow(p coreapi.Project) []string {
	return []string{p.ID, p.Name, string(p.OwnerType), p.OwnerId, p.Region}
}

func newProjectCreateCmd() *cobra.Command {
	var (
		ownerID   string
		ownerType string
		region    string
	)
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a project under an org or account",
		Long: "Creates a project owned by an org or an account. --owner is the " +
			"owning org (name or ULID) or account (github:handle or ULID), and " +
			"--owner-type selects which (org or account).",
		Example: "  # Project under an org (by name)\n" +
			"  entire project create widgets --owner acme --owner-type org\n\n" +
			"  # Project owned by an account (by handle)\n" +
			"  entire project create widgets --owner github:alice --owner-type account",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			ot, err := parseProjectOwnerType(ownerType)
			if err != nil {
				return err
			}
			return runCoreMutation(cmd, func(ctx context.Context, c *coreapi.Client) (string, any, error) {
				// Orgs are addressed by name, accounts by github:handle; both
				// also accept a raw ULID.
				var ownerRef string
				switch ot {
				case coreapi.CreateProjectInputBodyOwnerTypeOrg:
					ownerRef, err = resolveOrgRef(ctx, c, ownerID)
				case coreapi.CreateProjectInputBodyOwnerTypeAccount:
					ownerRef, err = resolveAccountRef(ctx, c, ownerID)
				}
				if err != nil {
					return "", nil, err
				}
				body := &coreapi.CreateProjectInputBody{
					Name:      args[0],
					OwnerId:   ownerRef,
					OwnerType: ot,
				}
				if region != "" {
					body.Region = coreapi.NewOptString(region)
				}
				project, err := c.CreateProject(ctx, body)
				if err != nil {
					return "", nil, err
				}
				return fmt.Sprintf("✓ Created project %s (%s)", project.Name, project.ID), project, nil
			})
		},
	}
	cmd.Flags().StringVar(&ownerID, "owner", "", "Owning org (name or ULID), or account (github:handle or ULID) (required)")
	cmd.Flags().StringVar(&ownerType, "owner-type", "org", "Owner kind: org or account")
	cmd.Flags().StringVar(&region, "region", "", "Jurisdiction slug (defaults to the server's home jurisdiction)")
	markRequired(cmd, "owner")
	return cmd
}

func newProjectListCmd() *cobra.Command {
	var name, org string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List projects you can see",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCoreList(cmd, "No projects found.", projectColumns, projectRow, func(ctx context.Context, c *coreapi.Client) ([]coreapi.Project, error) {
				// Both the global and org-scoped list endpoints filter by name
				// server-side (case-insensitive), returning the single match under
				// the response's `project` field (or 404 → empty result, not an
				// error). Without --name we page through the full list.
				if org != "" {
					orgID, err := resolveOrgRef(ctx, c, org)
					if err != nil {
						return nil, err
					}
					if name != "" {
						out, err := c.ListOrgProjects(ctx, coreapi.ListOrgProjectsParams{OrgId: orgID, Name: coreapi.NewOptString(name)})
						if err != nil {
							if isCoreNotFound(err) {
								return nil, nil
							}
							return nil, err
						}
						return toProjectList(out.Project), nil
					}
					return fetchAllPages(ctx, func(ctx context.Context, cursor string) ([]coreapi.Project, string, error) {
						params := coreapi.ListOrgProjectsParams{OrgId: orgID}
						if cursor != "" {
							params.PageToken = coreapi.NewOptString(cursor)
						}
						out, err := c.ListOrgProjects(ctx, params)
						if err != nil {
							return nil, "", err
						}
						return out.Projects, out.NextPageToken.Or(""), nil
					})
				}
				if name != "" {
					out, err := c.ListProjects(ctx, coreapi.ListProjectsParams{Name: coreapi.NewOptString(name)})
					if err != nil {
						if isCoreNotFound(err) {
							return nil, nil
						}
						return nil, err
					}
					return toProjectList(out.Project), nil
				}
				return fetchAllPages(ctx, func(ctx context.Context, cursor string) ([]coreapi.Project, string, error) {
					params := coreapi.ListProjectsParams{}
					if cursor != "" {
						params.PageToken = coreapi.NewOptString(cursor)
					}
					out, err := c.ListProjects(ctx, params)
					if err != nil {
						return nil, "", err
					}
					return out.Projects, out.NextPageToken.Or(""), nil
				})
			})
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Filter by exact project name")
	cmd.Flags().StringVar(&org, "org", "", "List projects owned by this org (name or ULID)")
	return cmd
}

func newProjectGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <project>",
		Short: "Show a project by name or ULID",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCoreObject(cmd, projectColumns, projectRow, func(ctx context.Context, c *coreapi.Client) (*coreapi.Project, error) {
				projID, err := resolveProjectRef(ctx, c, args[0])
				if err != nil {
					return nil, err
				}
				return c.GetProject(ctx, coreapi.GetProjectParams{ProjectId: projID})
			})
		},
	}
}

func newProjectDeleteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <project>",
		Short: "Delete a project by name or ULID",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runControlPlaneDelete(cmd, "project", args[0],
				func(ctx context.Context, c *coreapi.Client) (string, error) {
					return resolveProjectRef(ctx, c, args[0])
				},
				func(ctx context.Context, c *coreapi.Client, id string) error {
					return c.DeleteProject(ctx, coreapi.DeleteProjectParams{ProjectId: id})
				})
		},
	}
	addForceFlag(cmd)
	return cmd
}

// parseProjectOwnerType maps the --owner-type flag to the generated enum,
// rejecting anything but org/account at the CLI boundary so the user gets
// a clear message instead of a server 422.
func parseProjectOwnerType(s string) (coreapi.CreateProjectInputBodyOwnerType, error) {
	switch s {
	case "org":
		return coreapi.CreateProjectInputBodyOwnerTypeOrg, nil
	case "account":
		return coreapi.CreateProjectInputBodyOwnerTypeAccount, nil
	default:
		// Plain error: the create RunE sets SilenceUsage, and main.go
		// prints plain errors (a SilentError would be swallowed).
		return "", fmt.Errorf("invalid --owner-type %q: must be \"org\" or \"account\"", s)
	}
}
