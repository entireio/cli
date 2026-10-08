package cli

import (
	"context"
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
			"Run in an interactive terminal without --owner, --owner-type or " +
			"--region (optionally with a name), a wizard asks for the owner, name " +
			"and region; those flags always mean the flag form, so one given with " +
			"a missing name or --owner is an error.\n\n" +
			"Project names are 3-32 lowercase letters, digits or hyphens, starting " +
			"and ending with a letter or digit, can't look like an id, and are " +
			"unique across all owners. The wizard lowercases a typed name and " +
			"says so; the flag form refuses one with uppercase.",
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
			if in.complete() {
				return createProjectDirect(cmd, in, ot)
			}
			// Settled before any request: a prompt nobody can answer must not
			// cost a lookup. Any flag means the flag form, which the wizard
			// does not take starting values from, so it is refused too.
			if in.usesFlags(cmd) || !interactive.CanPromptInteractively() {
				return projectCreateMissingErr(in)
			}
			return runProjectCreateWizard(cmd, in.name)
		},
	}
	cmd.Flags().StringVar(&in.owner, "owner", "", "Owning org (name), or account (github:handle) (required; run in a terminal without --owner, --owner-type or --region to be asked instead)")
	cmd.Flags().StringVar(&in.ownerType, "owner-type", ownerTypeOrg, "Owner kind: org or account")
	cmd.Flags().StringVar(&in.region, "region", "", "Jurisdiction slug (defaults to the server's jurisdiction; the wizard suggests the owner's region)")
	addJSONFlag(cmd)
	return cmd
}

// projectCreateMissingErr names what the flag form is missing and both
// spellings of --owner: a handle needs --owner-type account, which defaults
// to org.
func projectCreateMissingErr(in projectCreateInput) error {
	var missing []string
	if in.name == "" {
		missing = append(missing, "a project name")
	}
	if in.owner == "" {
		missing = append(missing, "--owner")
	}
	verb := "are"
	if len(missing) == 1 {
		verb = "is"
	}
	return fmt.Errorf("%s %s required:\n"+
		"  entire project create <name> --owner <org>\n"+
		"  entire project create <name> --owner github:<handle> --owner-type account\n"+
		"or run 'entire project create' in a terminal, without --owner, --owner-type or --region, to be asked",
		strings.Join(missing, " and "), verb)
}

// createProjectDirect is the flag-complete path: no prompts, the same request
// the command has always sent. An omitted --region stays omitted so the server
// picks its home jurisdiction.
func createProjectDirect(cmd *cobra.Command, in projectCreateInput, ot coreapi.CreateProjectInputBodyOwnerType) error {
	// The server's name rule, checked before any request so scripts and
	// agents learn it from the command rather than from a server 400. Unlike
	// the wizard, the flag form does not lowercase: it creates exactly the
	// name it was given, or refuses.
	if err := checkProjectName(in.name); err != nil {
		return fmt.Errorf("invalid project name %q: %w", in.name, err)
	}
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
		return printProjectCreated(cmd, project)
	})
}

// printProjectCreated renders a created project the way runCoreMutation renders
// any mutation: the wire object under --json, else a ✓ line naming the project
// the way every command takes it (its name, unique across owners) and the
// region it landed in, which the flag form leaves to the server. Never by id.
func printProjectCreated(cmd *cobra.Command, project *coreapi.CreatedProject) error {
	if jsonRequested(cmd) {
		return printJSON(cmd.OutOrStdout(), project)
	}
	if project.Region != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "✓ Created project %s in %s\n", project.Name, project.Region)
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "✓ Created project %s\n", project.Name)
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
