package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/coreapi"
)

const (
	cmdServiceAccount = "service-account"
	saIDPrefix        = "svc_"

	// Core's published problem types; never renamed.
	saProblemNameInUse = "https://entire.com/errors/service-account/name-in-use"
	saProblemQuota     = "https://entire.com/errors/service-account/quota-reached"
)

// newServiceAccountCmd is the `entire service-account` group.
func newServiceAccountCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     cmdServiceAccount,
		Aliases: []string{"sa"},
		Short:   "Manage project service accounts",
		Long: "A service account is a non-human account owned by a project. " +
			"Grant it access to the project or its repos, then bind a GitHub " +
			"repository so its Actions workflows act as the account without a stored secret.\n\n" +
			"Commands address a service account by its svc_ ID or as <project>/<name>.",
	}
	addControlPlaneFlags(cmd)
	cmd.AddCommand(
		newSACreateCmd(), newSAListCmd(), newSAGetCmd(), newSADeleteCmd(),
		newSAGrantCmd(), newSARevokeCmd(), newSAGrantsCmd(),
		newSABindCmd(), newSABindingsCmd(), newSAUnbindCmd(),
	)
	return requireSubcommand(cmd)
}

var (
	saColumns     = []string{"ID", colHeaderName, colHeaderProject, colHeaderStatus}
	saListColumns = []string{"ID", colHeaderName, colHeaderStatus}
)

func saRow(sa coreapi.ServiceAccount) []string {
	return []string{sa.AccountId, sa.Name, orDash(sa.ProjectID.Or("")), sa.Status}
}

func saListRow(sa coreapi.ServiceAccountWithGrants) []string {
	return []string{sa.AccountId, sa.Name, sa.Status}
}

// saRef is a service account a command acts on.
type saRef struct {
	ID string
	// Name is <project>/<name>, empty for a typed ID.
	Name string
	// ProjectID is empty for a typed ID.
	ProjectID string
}

func (r saRef) label() string {
	return resolvedRefLabel(r.ID, resolvedRef{ID: r.ID, Name: r.Name})
}

// resolveSARef reads svc_<id> or <project>/<name>.
func resolveSARef(ctx context.Context, c *coreapi.Client, ref string) (saRef, error) {
	ref = strings.TrimSpace(ref)
	if id, ok := strings.CutPrefix(ref, saIDPrefix); ok && looksLikeULID(id) {
		return saRef{ID: ref}, nil
	}
	project, name, err := parseSAPath(ref)
	if err != nil {
		return saRef{}, err
	}
	proj, err := resolveProjectRefResolved(ctx, c, project)
	if err != nil {
		return saRef{}, err
	}
	accounts, err := listProjectSAs(ctx, c, proj.ID)
	if err != nil {
		return saRef{}, err
	}
	if proj.Name != "" {
		project = proj.Name
	}
	// Names are unique per project, case-sensitive.
	for _, sa := range accounts {
		if sa.Name == name {
			return saRef{ID: sa.AccountId, Name: project + "/" + sa.Name, ProjectID: proj.ID}, nil
		}
	}
	return saRef{}, fmt.Errorf("no service account %q in project %s", name, project)
}

// parseSAPath splits <project>/<name>.
func parseSAPath(ref string) (project, name string, err error) {
	project, name, ok := strings.Cut(ref, "/")
	if !ok || project == "" || name == "" {
		return "", "", fmt.Errorf("service account %q: use svc_<id> or <project>/<name>", ref)
	}
	return project, name, nil
}

func listProjectSAs(ctx context.Context, c *coreapi.Client, projectID string) ([]coreapi.ServiceAccountWithGrants, error) {
	return fetchAllPages(ctx, func(ctx context.Context, cursor string) ([]coreapi.ServiceAccountWithGrants, string, error) {
		params := coreapi.ListServiceAccountsParams{ProjectID: coreapi.NewOptString(projectID)}
		if cursor != "" {
			params.PageToken = coreapi.NewOptString(cursor)
		}
		out, err := c.ListServiceAccounts(ctx, params)
		if err != nil {
			return nil, "", err
		}
		return out.ServiceAccounts, out.NextPageToken.Or(""), nil
	})
}

func newSACreateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create <project> <name>",
		Short: "Create a service account in a project",
		Long: "Creates a service account owned by the project. You need manage " +
			"access to the project. Names are unique within a project, and each " +
			"project owner has a service account limit.",
		Example: "  entire service-account create acme ci",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			project, name := args[0], strings.TrimSpace(args[1])
			if name == "" {
				cmd.SilenceUsage = true
				return errors.New("service account name is empty")
			}
			return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
				proj, err := resolveProjectRefResolved(ctx, c, project)
				if err != nil {
					return err
				}
				if proj.Name != "" {
					project = proj.Name
				}
				sa, err := c.CreateServiceAccount(ctx, &coreapi.CreateServiceAccountInputBody{ProjectID: proj.ID, Name: name})
				if err != nil {
					return saCreateErr(err, project, name)
				}
				if jsonRequested(cmd) {
					return printJSON(cmd.OutOrStdout(), sa)
				}
				out := cmd.OutOrStdout()
				fmt.Fprintf(out, "✓ Created service account %s/%s (%s)\n", project, sa.Name, sa.AccountId)
				fmt.Fprintf(out, "Next: entire service-account grant %s --repo <repo> --role writer\n", sa.AccountId)
				return nil
			})
		},
	}
	addJSONFlag(cmd)
	return cmd
}

// saCreateErr names the project in core's refusals.
func saCreateErr(err error, project, name string) error {
	switch coreapi.APIErrorType(err) {
	case saProblemNameInUse:
		return fmt.Errorf("service account %s/%s already exists", project, name)
	case saProblemQuota:
		return fmt.Errorf("owner of project %s reached its service account limit", project)
	}
	// Core also answers 403 for a missing project.
	if coreapi.APIErrorStatus(err) == http.StatusForbidden {
		return fmt.Errorf("you need manage access to project %s", project)
	}
	return err
}

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
				return c.GetServiceAccount(ctx, coreapi.GetServiceAccountParams{AccountId: r.ID})
			})
		},
	}
	addJSONFlag(cmd)
	return cmd
}

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
