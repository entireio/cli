package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/coreapi"
)

// newProjectCmd is the hidden `entire project` command group: create and
// list projects on the Entire control plane. Surfaced via `entire labs`.
func newProjectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "project",
		Short:  "Manage Entire projects",
		Hidden: true,
	}
	addControlPlaneFlags(cmd)
	cmd.AddCommand(newProjectCreateCmd())
	cmd.AddCommand(newProjectListCmd())
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
			"owning org/account ULID and --owner-type selects which (org or account).",
		Example: "  # Project under an org\n" +
			"  entire project create widgets --owner 01J0... --owner-type org\n\n" +
			"  # Project owned by an account\n" +
			"  entire project create widgets --owner 01J0... --owner-type account",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			ot, err := parseProjectOwnerType(ownerType)
			if err != nil {
				return err
			}
			return runCoreJSON(cmd, func(ctx context.Context, c *coreapi.Client) (any, error) {
				body := &coreapi.CreateProjectInputBody{
					Name:      args[0],
					OwnerId:   ownerID,
					OwnerType: ot,
				}
				if region != "" {
					body.Region = coreapi.NewOptString(region)
				}
				return c.CreateProject(ctx, body)
			})
		},
	}
	cmd.Flags().StringVar(&ownerID, "owner", "", "owning org or account ULID (required)")
	cmd.Flags().StringVar(&ownerType, "owner-type", "org", "owner kind: org or account")
	cmd.Flags().StringVar(&region, "region", "", "jurisdiction slug (defaults to the server's home jurisdiction)")
	markRequired(cmd, "owner")
	return cmd
}

func newProjectListCmd() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List projects you can see",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCoreList(cmd, projectColumns, projectRow, func(ctx context.Context, c *coreapi.Client) ([]coreapi.Project, error) {
				var params coreapi.ListProjectsParams
				if name != "" {
					params.Name = coreapi.NewOptString(name)
				}
				out, err := c.ListProjects(ctx, params)
				if err != nil {
					return nil, err
				}
				return out.Projects, nil
			})
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "filter by exact project name")
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
