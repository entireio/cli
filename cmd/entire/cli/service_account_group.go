package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/coreapi"
)

const (
	cmdServiceAccount = "service-account"
	saIDPrefix        = "svc_"
	saBindingIDPrefix = "eib_"

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
			"Grant it access to the project or its repos, then bind a CI provider, " +
			"such as GitHub Actions or Buildkite, so its jobs act as the account " +
			"without a stored secret. Anywhere else, create a secret and set it as " +
			"ENTIRE_TOKEN.\n\n" +
			"Commands address a service account by its svc_ ID or as <project>/<name>.",
	}
	addControlPlaneFlags(cmd)
	cmd.AddCommand(
		newSACreateCmd(), newSAListCmd(), newSAGetCmd(), newSADeleteCmd(),
		newSAGrantCmd(), newSARevokeCmd(), newSAGrantsCmd(),
		newSAProvidersCmd(), newSABindCmd(), newSABindingsCmd(), newSAUnbindCmd(),
		newSASecretCmd(),
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

// saRef names a resolved service account.
type saRef struct {
	ID string
	// Name is <project>/<name>; empty for IDs.
	Name      string
	ProjectID string
	// record is set for a typed ID.
	record *coreapi.ServiceAccount
}

func (r saRef) label() string {
	return resolvedRefLabel(r.ID, resolvedRef{ID: r.ID, Name: r.Name})
}

// resolveSARef finds svc_<id> or <project>/<name>.
// Both forms confirm the account exists.
func resolveSARef(ctx context.Context, c *coreapi.Client, ref string) (saRef, error) {
	ref = strings.TrimSpace(ref)
	if canonical, ok := canonicalPrefixedULID(saIDPrefix, ref); ok {
		ref = canonical
		sa, err := c.GetServiceAccount(ctx, coreapi.GetServiceAccountParams{AccountId: ref})
		if err != nil {
			if isCoreNotFound(err) {
				return saRef{}, fmt.Errorf("no service account %s", ref)
			}
			return saRef{}, err
		}
		return saRef{ID: sa.AccountId, ProjectID: sa.ProjectID.Or(""), record: sa}, nil
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

// canonicalPrefixedULID upper-cases the ULID in <prefix><ulid>. Core parses
// IDs case-insensitively but matches them upper-case, so a lower-case ID
// would miss: a 404 for an account, and a silent 204 for an unbind.
func canonicalPrefixedULID(prefix, s string) (string, bool) {
	id, ok := strings.CutPrefix(s, prefix)
	if !ok || !looksLikeULID(id) {
		return s, false
	}
	return prefix + strings.ToUpper(id), true
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
