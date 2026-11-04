package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/coreapi"
)

const (
	saResourceRepo    = "repo"
	saResourceProject = "project"
)

// saTarget is the repo or project a grant names.
type saTarget struct {
	kind  string
	id    string
	label string
}

// addSATargetFlags wires --repo and --project.
func addSATargetFlags(cmd *cobra.Command, repo, project *string) {
	cmd.Flags().StringVar(repo, "repo", "", "Repo name in the service account's project, or its /et/<project>/<repo> path")
	cmd.Flags().StringVar(project, "project", "", "The service account's own project (name or ULID)")
	cmd.MarkFlagsOneRequired("repo", "project")
	cmd.MarkFlagsMutuallyExclusive("repo", "project")
}

// resolveSATarget turns --repo or --project into an ID.
func resolveSATarget(ctx context.Context, c *coreapi.Client, sa saRef, repo, project string) (saTarget, error) {
	if project != "" {
		p, err := resolveProjectRefResolved(ctx, c, project)
		if err != nil {
			return saTarget{}, err
		}
		return saTarget{kind: saResourceProject, id: p.ID, label: "project " + resolvedRefLabel(project, p)}, nil
	}
	var r resolvedRef
	var err error
	if strings.Contains(repo, "/") {
		r, err = resolveRepoPathResolved(ctx, c, repo)
	} else {
		// A bare name lives in the account's project.
		var projectID string
		if projectID, err = saProjectID(ctx, c, sa); err == nil {
			r, err = resolveRepoInProject(ctx, c, repo, projectID)
		}
	}
	if err != nil {
		return saTarget{}, err
	}
	return saTarget{kind: saResourceRepo, id: r.ID, label: "repo " + resolvedRefLabel(repo, r)}, nil
}

// saProjectID returns the owning project's ID.
func saProjectID(ctx context.Context, c *coreapi.Client, r saRef) (string, error) {
	if r.ProjectID != "" {
		return r.ProjectID, nil
	}
	sa, err := c.GetServiceAccount(ctx, coreapi.GetServiceAccountParams{AccountId: r.ID})
	if err != nil {
		return "", err
	}
	id := sa.ProjectID.Or("")
	if id == "" {
		return "", fmt.Errorf("service account %s names no project", r.ID)
	}
	return id, nil
}

func newSAGrantCmd() *cobra.Command {
	var repo, project, role string
	cmd := &cobra.Command{
		Use:   "grant <service-account>",
		Short: "Grant a service account access to a repo or its project",
		Long: "Grants the service account a role on one repo or on its own project. " +
			"Core refuses a repo or project outside the service account's project. " +
			"Granting again changes the role.",
		Example: "  entire service-account grant acme/ci --repo web --role writer\n" +
			"  entire service-account grant acme/ci --project acme --role reader",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			if !slices.Contains(accessRoles, role) {
				return fmt.Errorf("--role must be one of %s", strings.Join(accessRoles, ", "))
			}
			return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
				sa, err := resolveSARef(ctx, c, args[0])
				if err != nil {
					return err
				}
				target, err := resolveSATarget(ctx, c, sa, repo, project)
				if err != nil {
					return err
				}
				body := &coreapi.GrantServiceAccountAccessInputBody{
					ResourceType: coreapi.GrantServiceAccountAccessInputBodyResourceType(target.kind),
					ResourceId:   target.id,
					Role:         coreapi.GrantServiceAccountAccessInputBodyRole(role),
				}
				if _, err := c.GrantServiceAccountAccess(ctx, body, coreapi.GrantServiceAccountAccessParams{AccountId: sa.ID}); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "✓ Granted %s on %s to %s\n", role, target.label, sa.label())
				return nil
			})
		},
	}
	addSATargetFlags(cmd, &repo, &project)
	cmd.Flags().StringVar(&role, "role", "", "Role: one of "+strings.Join(accessRoles, ", "))
	markRequired(cmd, "role")
	return cmd
}

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
