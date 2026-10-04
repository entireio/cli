package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/internal/coreapi"
)

// newProjectCmd is the `entire project` command group: create, list,
// get, and delete projects on the Entire control plane, plus the `grant`
// subtree for project access (see grant.go).
func newProjectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "project",
		Short: "Manage Entire projects",
	}
	addControlPlaneFlags(cmd)
	cmd.AddCommand(newProjectCreateCmd())
	cmd.AddCommand(newProjectListCmd())
	cmd.AddCommand(newProjectGetCmd())
	cmd.AddCommand(newProjectDeleteCmd())
	cmd.AddCommand(newProjectGrantCmd())
	return cmd
}

// projectColumns is the human table/field view of a project.
var projectColumns = []string{"ID", colHeaderName, "OWNER-TYPE", "OWNER", colHeaderRegion}

func projectRow(p coreapi.Project) []string {
	return []string{p.ID, p.Name, string(p.OwnerType), p.OwnerId, p.Region}
}

func newProjectCreateCmd() *cobra.Command {
	var in projectCreateInput
	cmd := &cobra.Command{
		Use:   "create [<name>]",
		Short: "Create a project under an org or account",
		Long: "Creates a project owned by an org or an account. --owner is the " +
			"owning org (by name) or account (github:handle), and --owner-type " +
			"selects which (org or account).\n\n" +
			"With both a name and --owner the project is created directly. " +
			"Otherwise, in an interactive terminal, a wizard asks for the owner, " +
			"name and region, starting from whatever was given.",
		Example: "  # Project under an org (by name)\n" +
			"  entire project create widgets --owner acme --owner-type org\n\n" +
			"  # Project owned by an account (by handle)\n" +
			"  entire project create widgets --owner github:alice --owner-type account\n\n" +
			"  # Pick the owner and region interactively\n" +
			"  entire project create widgets",
		Args: cobra.RangeArgs(0, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			if len(args) == 1 {
				// Trimmed once for both paths, as the wizard trims what is
				// typed into it; a blank name counts as no name.
				in.name = strings.TrimSpace(args[0])
			}
			ot, err := parseProjectOwnerType(in.ownerType)
			if err != nil {
				return err
			}
			if cmd.Flags().Changed("owner-type") {
				in.ownerKind = ot
			}
			if in.complete() {
				return createProjectDirect(cmd, in, ot)
			}
			// Settled before any request: a prompt nobody can answer must not
			// cost a lookup.
			if !interactive.CanPromptInteractively() {
				return errors.New("a project name and --owner are required without an interactive terminal: " +
					"entire project create <name> --owner <org|github:handle>")
			}
			return runProjectCreateWizard(cmd, in)
		},
	}
	cmd.Flags().StringVar(&in.owner, "owner", "", "Owning org (name), or account (github:handle)")
	cmd.Flags().StringVar(&in.ownerType, "owner-type", ownerTypeOrg, "Owner kind: org or account")
	cmd.Flags().StringVar(&in.region, "region", "", "Jurisdiction slug (defaults to the server's home jurisdiction)")
	addJSONFlag(cmd)
	return cmd
}

// createProjectDirect is the flag-complete path: no prompts, the same request
// the command has always sent. An omitted --region stays omitted so the server
// picks its home jurisdiction.
func createProjectDirect(cmd *cobra.Command, in projectCreateInput, ot coreapi.CreateProjectInputBodyOwnerType) error {
	return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
		// Orgs are addressed by name, accounts by github:handle; both
		// also accept a raw ULID.
		var ownerRef string
		var err error
		switch ot {
		case coreapi.CreateProjectInputBodyOwnerTypeOrg:
			ownerRef, err = resolveOrgRef(ctx, c, in.owner)
		case coreapi.CreateProjectInputBodyOwnerTypeAccount:
			ownerRef, err = resolveAccountRef(ctx, c, in.owner)
		}
		if err != nil {
			return err
		}
		body := &coreapi.CreateProjectInputBody{
			Name:      in.name,
			OwnerId:   ownerRef,
			OwnerType: ot,
		}
		if in.region != "" {
			body.Region = coreapi.NewOptString(in.region)
		}
		created, err := c.CreateProject(ctx, body)
		if err != nil {
			return err
		}
		project := &created.Response
		// A ULID --owner is accepted but never echoed: name the owner the way
		// the server does, falling back to what was typed only when that is a
		// name, and otherwise to no owner at all.
		owner := project.OwnerName.Or("")
		if owner == "" && !looksLikeULID(in.owner) {
			owner = in.owner
		}
		return printProjectCreated(cmd, project, owner)
	})
}

// printProjectCreated renders a created project the way runCoreMutation renders
// any mutation: the wire object under --json, else a ✓ line naming the project
// by owner and name, never by id.
// An empty owner leaves the owner out rather than print a stand-in.
func printProjectCreated(cmd *cobra.Command, project *coreapi.CreatedProject, owner string) error {
	if jsonRequested(cmd) {
		return printJSON(cmd.OutOrStdout(), project)
	}
	ref := project.Name
	if owner != "" {
		ref = owner + "/" + project.Name
	}
	if project.Region != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "✓ Created project %s in %s\n", ref, project.Region)
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "✓ Created project %s\n", ref)
	return nil
}

func newProjectListCmd() *cobra.Command {
	var name, org string
	cmd := &cobra.Command{
		Use:   cmdList,
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
				return listAllProjects(ctx, c)
			})
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Filter by exact project name")
	cmd.Flags().StringVar(&org, "org", "", "List projects owned by this org (name or ULID)")
	addJSONFlag(cmd)
	return cmd
}

// listAllProjects pages through every project the caller can see.
func listAllProjects(ctx context.Context, c *coreapi.Client) ([]coreapi.Project, error) {
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
}

func newProjectGetCmd() *cobra.Command {
	cmd := &cobra.Command{
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
	addJSONFlag(cmd)
	return cmd
}

func newProjectDeleteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <project>",
		Short: "Delete a project by name or ULID",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runControlPlaneDelete(cmd, "project", args[0],
				func(ctx context.Context, c *coreapi.Client) (resolvedRef, error) {
					return resolveProjectRefResolved(ctx, c, args[0])
				},
				func(ctx context.Context, c *coreapi.Client, id string) error {
					_, err := c.DeleteProject(ctx, coreapi.DeleteProjectParams{ProjectId: id})
					return err
				})
		},
	}
	addForceFlag(cmd)
	return cmd
}

// The two owner kinds a project may have, as the --owner-type flag spells them.
const (
	ownerTypeOrg     = "org"
	ownerTypeAccount = "account"
)

// parseProjectOwnerType maps the --owner-type flag to the generated enum,
// rejecting anything but org/account at the CLI boundary so the user gets
// a clear message instead of a server 422.
func parseProjectOwnerType(s string) (coreapi.CreateProjectInputBodyOwnerType, error) {
	switch s {
	case ownerTypeOrg:
		return coreapi.CreateProjectInputBodyOwnerTypeOrg, nil
	case ownerTypeAccount:
		return coreapi.CreateProjectInputBodyOwnerTypeAccount, nil
	default:
		// Plain error: the create RunE sets SilenceUsage, and main.go
		// prints plain errors (a SilentError would be swallowed).
		return "", fmt.Errorf("invalid --owner-type %q: must be \"org\" or \"account\"", s)
	}
}
