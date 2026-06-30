package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/coreapi"
)

// parseOrgRole maps the --role flag for `entire grant org add` to the
// generated enum, rejecting unknown values at the CLI boundary so the
// user gets a clear message instead of a server 422. Mirrors
// parseProjectOwnerType. The empty string means "use the server default
// (member)" and is the caller's signal to omit the field entirely; it is
// not handled here.
func parseOrgRole(s string) (coreapi.AddOrgMemberInputBodyRole, error) {
	switch s {
	case "owner":
		return coreapi.AddOrgMemberInputBodyRoleOwner, nil
	case "admin":
		return coreapi.AddOrgMemberInputBodyRoleAdmin, nil
	case "member":
		return coreapi.AddOrgMemberInputBodyRoleMember, nil
	default:
		return "", fmt.Errorf("invalid --role %q: must be \"owner\", \"admin\", or \"member\"", s)
	}
}

// validateGrantRole rejects unknown project/repo grant roles at the CLI
// boundary (reader, writer, admin) so the user gets a clear message instead of
// a server 422. The GrantProjectAccess and GrantRepoAccess input bodies use
// distinct enum types that share these values, so callers cast the validated
// string to whichever type they need.
func validateGrantRole(role string) error {
	switch role {
	case "reader", "writer", "admin":
		return nil
	default:
		return fmt.Errorf("invalid --role %q: must be \"reader\", \"writer\", or \"admin\"", role)
	}
}

// newGrantCmd is the hidden `entire grant` command group: manage access
// grants and org membership on the Entire control plane. Org, project, and
// repo each support add / list / remove. Surfaced via `entire labs`.
//
// Grantees are addressed by a provider-qualified handle (e.g. github:alice),
// which the CLI resolves to the provider account behind the scenes. `remove`
// also accepts an account ULID to revoke a grant by id. Targets (org, project,
// repo) are addressed by name or ULID.
func newGrantCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "grant",
		Short:  "Manage Entire access grants and org membership",
		Hidden: true,
	}
	addControlPlaneFlags(cmd)
	cmd.AddCommand(newGrantOrgCmd())
	cmd.AddCommand(newGrantProjectCmd())
	cmd.AddCommand(newGrantRepoCmd())
	return cmd
}

// orgMemberColumns / grantColumns are the human table views of the
// membership/grant listings. Grant listings now include inherited and owner
// grants, so GRANTEE shows a friendly name (handle/org name) with SOURCE
// saying where the grant comes from; ID keeps the ULID for revoke.
var (
	orgMemberColumns = []string{"ACCOUNT", "ROLE", "STATUS"}
	grantColumns     = []string{"GRANTEE-TYPE", "GRANTEE", "ID", "ROLE", "SOURCE"}
)

func orgMemberRow(m coreapi.Membership) []string {
	return []string{m.AccountId, m.Role, m.Status}
}

func projectGrantRow(g coreapi.ProjectGrant) []string {
	return []string{g.GranteeType, granteeName(g.GranteeName, g.GranteeId), g.GranteeId, g.Role, g.Source}
}

// repoGrantRow mirrors projectGrantRow; RepoGrant and ProjectGrant share the
// grantee/role/source shape, so both reuse grantColumns.
func repoGrantRow(g coreapi.RepoGrant) []string {
	return []string{g.GranteeType, granteeName(g.GranteeName, g.GranteeId), g.GranteeId, g.Role, g.Source}
}

// granteeName returns the friendly name when the server resolved one, falling
// back to the ULID for grantees it couldn't label (e.g. teams).
func granteeName(name coreapi.OptString, granteeID string) string {
	if n := name.Or(""); n != "" {
		return n
	}
	return granteeID
}

// --- org membership -------------------------------------------------------

func newGrantOrgCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "org",
		Short: "Manage org membership",
	}
	cmd.AddCommand(newGrantOrgAddCmd())
	cmd.AddCommand(newGrantOrgListCmd())
	cmd.AddCommand(newGrantOrgRemoveCmd())
	return cmd
}

func newGrantOrgAddCmd() *cobra.Command {
	var role string
	cmd := &cobra.Command{
		Use:     "add <org> <grantee>",
		Short:   "Add a member to an org",
		Long:    "Add a member (addressed as provider:handle, e.g. github:alice) to an org (name or ULID).",
		Example: "  entire grant org add acme github:alice --role admin",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCoreJSON(cmd, func(ctx context.Context, c *coreapi.Client) (any, error) {
				orgID, err := resolveOrgRef(ctx, c, args[0])
				if err != nil {
					return nil, err
				}
				provider, providerUserID, err := resolveGranteeProvider(ctx, c, args[1])
				if err != nil {
					return nil, err
				}
				body := &coreapi.AddOrgMemberInputBody{
					Provider:       provider,
					ProviderUserId: providerUserID,
				}
				if role != "" {
					r, err := parseOrgRole(role)
					if err != nil {
						return nil, err
					}
					body.Role = coreapi.NewOptAddOrgMemberInputBodyRole(r)
				}
				return c.AddOrgMember(ctx, body, coreapi.AddOrgMemberParams{OrgId: orgID})
			})
		},
	}
	cmd.Flags().StringVar(&role, "role", "", "org role: owner, admin, or member (default member)")
	return cmd
}

func newGrantOrgListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list <org>",
		Short: "List org members",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCoreList(cmd, orgMemberColumns, orgMemberRow, func(ctx context.Context, c *coreapi.Client) ([]coreapi.Membership, error) {
				orgID, err := resolveOrgRef(ctx, c, args[0])
				if err != nil {
					return nil, err
				}
				return fetchAllPages(ctx, func(ctx context.Context, cursor string) ([]coreapi.Membership, string, error) {
					params := coreapi.ListOrgMembersParams{OrgId: orgID}
					if cursor != "" {
						params.PageToken = coreapi.NewOptString(cursor)
					}
					out, err := c.ListOrgMembers(ctx, params)
					if err != nil {
						return nil, "", err
					}
					return out.Members, out.NextPageToken.Or(""), nil
				})
			})
		},
	}
}

func newGrantOrgRemoveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "remove <org> <grantee>",
		Short:   "Remove a member from an org",
		Long:    "Remove a member (addressed as provider:handle, e.g. github:alice) from an org (name or ULID).",
		Example: "  entire grant org remove acme github:alice",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
				orgID, err := resolveOrgRef(ctx, c, args[0])
				if err != nil {
					return err
				}
				provider, providerUserID, err := resolveGranteeProvider(ctx, c, args[1])
				if err != nil {
					return err
				}
				return revokeGrant(cmd, "Removed", fmt.Sprintf("%s from org %s", args[1], args[0]), func() error {
					return c.RemoveOrgMember(ctx, coreapi.RemoveOrgMemberParams{
						OrgId:          orgID,
						Provider:       provider,
						ProviderUserId: providerUserID,
					})
				})
			})
		},
	}
	return cmd
}

// --- project grants -------------------------------------------------------

func newGrantProjectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "project",
		Short: "Manage project access",
	}
	cmd.AddCommand(newGrantProjectAddCmd())
	cmd.AddCommand(newGrantProjectListCmd())
	cmd.AddCommand(newGrantProjectRemoveCmd())
	return cmd
}

func newGrantProjectAddCmd() *cobra.Command {
	var role string
	cmd := &cobra.Command{
		Use:     "add <project> <grantee>",
		Short:   "Grant a user access to a project",
		Long:    "Grant a user (addressed as provider:handle, e.g. github:alice) access to a project (name or ULID).",
		Example: "  entire grant project add widgets github:alice --role writer",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateGrantRole(role); err != nil {
				cmd.SilenceUsage = true
				return err
			}
			return runCoreJSON(cmd, func(ctx context.Context, c *coreapi.Client) (any, error) {
				projID, err := resolveProjectRef(ctx, c, args[0])
				if err != nil {
					return nil, err
				}
				provider, providerUserID, err := resolveGranteeProvider(ctx, c, args[1])
				if err != nil {
					return nil, err
				}
				body := &coreapi.GrantProjectAccessInputBody{
					Provider:       provider,
					ProviderUserId: providerUserID,
					Role:           coreapi.GrantProjectAccessInputBodyRole(role),
				}
				return c.GrantProjectAccess(ctx, body, coreapi.GrantProjectAccessParams{ProjectId: projID})
			})
		},
	}
	cmd.Flags().StringVar(&role, "role", "", "project role: reader, writer, or admin (required)")
	markRequired(cmd, "role")
	return cmd
}

func newGrantProjectListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list <project>",
		Short: "List project members",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCoreList(cmd, grantColumns, projectGrantRow, func(ctx context.Context, c *coreapi.Client) ([]coreapi.ProjectGrant, error) {
				projID, err := resolveProjectRef(ctx, c, args[0])
				if err != nil {
					return nil, err
				}
				return fetchAllPages(ctx, func(ctx context.Context, cursor string) ([]coreapi.ProjectGrant, string, error) {
					params := coreapi.ListProjectMembersParams{ProjectId: projID}
					if cursor != "" {
						params.PageToken = coreapi.NewOptString(cursor)
					}
					out, err := c.ListProjectMembers(ctx, params)
					if err != nil {
						return nil, "", err
					}
					return out.Members, out.NextPageToken.Or(""), nil
				})
			})
		},
	}
}

func newGrantProjectRemoveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove <project> <grantee>",
		Short: "Revoke project access from a grantee",
		Long: "Revoke a grantee's access to a project (addressed by name or ULID). " +
			"The grantee is a provider-qualified handle (e.g. github:alice) or an " +
			"account ULID.",
		Example: "  entire grant project remove widgets github:alice",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
				projID, err := resolveProjectRef(ctx, c, args[0])
				if err != nil {
					return err
				}
				return revokeProjectGrantee(ctx, cmd, c, projID, args[0], args[1])
			})
		},
	}
	return cmd
}

// revokeProjectGrantee revokes a grantee (provider:handle or account ULID) from
// a resolved project. projectRef is the user's original (pre-resolution) project
// ref, used only for the success message.
func revokeProjectGrantee(ctx context.Context, cmd *cobra.Command, c *coreapi.Client, projID, projectRef, grantee string) error {
	return revokeGrantee(ctx, cmd, c, "project", projectRef, grantee,
		func() error {
			return c.RevokeProjectAccess(ctx, coreapi.RevokeProjectAccessParams{
				ProjectId:   projID,
				GranteeType: "account",
				GranteeId:   grantee,
			})
		},
		func(provider, providerUserID string) error {
			return c.RevokeProjectAccessByProvider(ctx, coreapi.RevokeProjectAccessByProviderParams{
				ProjectId:      projID,
				Provider:       provider,
				ProviderUserId: providerUserID,
			})
		})
}

// revokeGrantee performs the shared grantee-revocation routing for projects and
// repos: a ULID grantee takes the typed-id route (revokeByID); a provider:handle
// is resolved to its provider account first and takes the by-provider route
// (revokeByProvider). target ("project"/"repo") and ref name the grant in the
// success message.
func revokeGrantee(
	ctx context.Context,
	cmd *cobra.Command,
	c *coreapi.Client,
	target, ref, grantee string,
	revokeByID func() error,
	revokeByProvider func(provider, providerUserID string) error,
) error {
	if looksLikeULID(grantee) {
		return revokeGrant(cmd, "Revoked", fmt.Sprintf("account %s from %s %s", grantee, target, ref), revokeByID)
	}
	provider, providerUserID, err := resolveGranteeProvider(ctx, c, grantee)
	if err != nil {
		return err
	}
	return revokeGrant(cmd, "Revoked", fmt.Sprintf("%s from %s %s", grantee, target, ref), func() error {
		return revokeByProvider(provider, providerUserID)
	})
}

// --- repo grants ----------------------------------------------------------

func newGrantRepoCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "repo",
		Short: "Manage repo access",
	}
	cmd.AddCommand(newGrantRepoAddCmd())
	cmd.AddCommand(newGrantRepoListCmd())
	cmd.AddCommand(newGrantRepoRemoveCmd())
	return cmd
}

func newGrantRepoAddCmd() *cobra.Command {
	var role, project string
	cmd := &cobra.Command{
		Use:     "add <repo> <grantee>",
		Short:   "Grant a user access to a repo",
		Long:    "Grant a user (addressed as provider:handle, e.g. github:alice) access to a repo (name or ULID).",
		Example: "  entire grant repo add web github:alice --project acme --role writer",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateGrantRole(role); err != nil {
				cmd.SilenceUsage = true
				return err
			}
			return runCoreJSON(cmd, func(ctx context.Context, c *coreapi.Client) (any, error) {
				repoID, err := resolveRepoRef(ctx, c, args[0], project)
				if err != nil {
					return nil, err
				}
				provider, providerUserID, err := resolveGranteeProvider(ctx, c, args[1])
				if err != nil {
					return nil, err
				}
				body := &coreapi.GrantRepoAccessInputBody{
					Provider:       provider,
					ProviderUserId: providerUserID,
					Role:           coreapi.GrantRepoAccessInputBodyRole(role),
				}
				return c.GrantRepoAccess(ctx, body, coreapi.GrantRepoAccessParams{RepoId: repoID})
			})
		},
	}
	cmd.Flags().StringVar(&role, "role", "", "repo role: reader, writer, or admin (required)")
	bindRepoProjectFlag(cmd, &project)
	markRequired(cmd, "role")
	return cmd
}

func newGrantRepoListCmd() *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "list <repo>",
		Short: "List repo grants",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCoreList(cmd, grantColumns, repoGrantRow, func(ctx context.Context, c *coreapi.Client) ([]coreapi.RepoGrant, error) {
				repoID, err := resolveRepoRef(ctx, c, args[0], project)
				if err != nil {
					return nil, err
				}
				return fetchAllPages(ctx, func(ctx context.Context, cursor string) ([]coreapi.RepoGrant, string, error) {
					params := coreapi.ListRepoGrantsParams{RepoId: repoID}
					if cursor != "" {
						params.PageToken = coreapi.NewOptString(cursor)
					}
					out, err := c.ListRepoGrants(ctx, params)
					if err != nil {
						return nil, "", err
					}
					return out.Grants, out.NextPageToken.Or(""), nil
				})
			})
		},
	}
	bindRepoProjectFlag(cmd, &project)
	return cmd
}

func newGrantRepoRemoveCmd() *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "remove <repo> <grantee>",
		Short: "Revoke repo access from a grantee",
		Long: "Revoke a grantee's access to a repo (addressed by name or ULID). " +
			"The grantee is a provider-qualified handle (e.g. github:alice) or an " +
			"account ULID.",
		Example: "  entire grant repo remove web github:alice --project acme",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
				repoID, err := resolveRepoRef(ctx, c, args[0], project)
				if err != nil {
					return err
				}
				return revokeRepoGrantee(ctx, cmd, c, repoID, args[0], args[1])
			})
		},
	}
	bindRepoProjectFlag(cmd, &project)
	return cmd
}

// revokeRepoGrantee mirrors revokeProjectGrantee for repos. repoRef is the
// user's original repo ref, for messaging.
func revokeRepoGrantee(ctx context.Context, cmd *cobra.Command, c *coreapi.Client, repoID, repoRef, grantee string) error {
	return revokeGrantee(ctx, cmd, c, "repo", repoRef, grantee,
		func() error {
			return c.RevokeRepoAccess(ctx, coreapi.RevokeRepoAccessParams{
				RepoId:      repoID,
				GranteeType: "account",
				GranteeId:   grantee,
			})
		},
		func(provider, providerUserID string) error {
			return c.RevokeRepoAccessByProvider(ctx, coreapi.RevokeRepoAccessByProviderParams{
				RepoId:         repoID,
				Provider:       provider,
				ProviderUserId: providerUserID,
			})
		})
}

// revokeGrant runs a grant-removal API call idempotently. A 404 means the
// grantee already has no such grant — the desired end state — so it's reported
// as a no-op rather than surfaced as a raw error, matching runControlPlaneDelete.
// verb is the success word ("Revoked"/"Removed"); subject describes the grant,
// e.g. "github:alice from repo acme".
func revokeGrant(cmd *cobra.Command, verb, subject string, revoke func() error) error {
	if err := revoke(); err != nil {
		if isCoreNotFound(err) {
			cmd.Printf("%s: no such grant; nothing to revoke\n", subject)
			return nil
		}
		return err
	}
	cmd.Printf("%s %s\n", verb, subject)
	return nil
}
