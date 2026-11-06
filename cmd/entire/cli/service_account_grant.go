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

// saTarget is a grant's repo or project.
type saTarget struct {
	kind  string
	id    string
	label string
}

// addSATargetFlags wires --repo and --project.
func addSATargetFlags(cmd *cobra.Command, repo, project *string) {
	cmd.Flags().StringVar(repo, "repo", "", "Repo name in the service account's project, its /et/<project>/<repo> path, or its ULID")
	cmd.Flags().StringVar(project, "project", "", "The service account's own project (name or ULID)")
	cmd.MarkFlagsOneRequired("repo", "project")
	cmd.MarkFlagsMutuallyExclusive("repo", "project")
}

// resolveSATarget resolves --repo or --project.
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
	if looksLikeULID(repo) || strings.Contains(repo, "/") {
		// A ULID or /et/<project>/<repo> path names its own project.
		r, err = resolveRepoRefResolved(ctx, c, repo, "")
	} else {
		// Bare names live in the account's project.
		if sa.ProjectID == "" {
			return saTarget{}, fmt.Errorf("service account %s names no project", sa.ID)
		}
		r, err = resolveRepoInProject(ctx, c, repo, sa.ProjectID)
	}
	if err != nil {
		return saTarget{}, err
	}
	return saTarget{kind: saResourceRepo, id: r.ID, label: "repo " + resolvedRefLabel(repo, r)}, nil
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
