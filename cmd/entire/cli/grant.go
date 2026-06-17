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

// newGrantCmd is the hidden `entire grant` command group: manage access
// grants and org membership on the Entire control plane. Surface follows
// what the Core API exposes per resource: org and project support
// add / list / remove, while repo supports only add (the API has no
// repo-grant list or revoke route yet). Surfaced via `entire labs`.
//
// Grantees are addressed by their identity provider + provider user id
// (e.g. --provider github --provider-user-id 12345), matching the control
// plane's grant model. Handle-based addressing is a follow-up.
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

// orgMemberColumns / projectGrantColumns are the human table views of the
// two membership/grant listings.
var (
	orgMemberColumns    = []string{"ACCOUNT", "ROLE", "STATUS"}
	projectGrantColumns = []string{"GRANTEE-TYPE", "GRANTEE", "ROLE"}
)

func orgMemberRow(m coreapi.Membership) []string {
	return []string{m.AccountId, m.Role, m.Status}
}

func projectGrantRow(g coreapi.ProjectGrant) []string {
	return []string{g.GranteeType, g.GranteeId, g.Role}
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
	var provider, providerUserID, role string
	cmd := &cobra.Command{
		Use:   "add <org>",
		Short: "Add a member to an org",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body := &coreapi.AddOrgMemberInputBody{
				Provider:       provider,
				ProviderUserId: providerUserID,
			}
			if role != "" {
				r, err := parseOrgRole(role)
				if err != nil {
					cmd.SilenceUsage = true
					return err
				}
				body.Role = coreapi.NewOptAddOrgMemberInputBodyRole(r)
			}
			return runCoreJSON(cmd, func(ctx context.Context, c *coreapi.Client) (any, error) {
				return c.AddOrgMember(ctx, body, coreapi.AddOrgMemberParams{OrgId: args[0]})
			})
		},
	}
	bindGranteeFlags(cmd, &provider, &providerUserID)
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
				out, err := c.ListOrgMembers(ctx, coreapi.ListOrgMembersParams{OrgId: args[0]})
				if err != nil {
					return nil, err
				}
				return out.Members, nil
			})
		},
	}
}

func newGrantOrgRemoveCmd() *cobra.Command {
	var provider, providerUserID string
	cmd := &cobra.Command{
		Use:   "remove <org>",
		Short: "Remove a member from an org",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
				if err := c.RemoveOrgMember(ctx, coreapi.RemoveOrgMemberParams{
					OrgId:          args[0],
					Provider:       provider,
					ProviderUserId: providerUserID,
				}); err != nil {
					return err
				}
				cmd.Printf("Removed %s/%s from org %s\n", provider, providerUserID, args[0])
				return nil
			})
		},
	}
	bindGranteeFlags(cmd, &provider, &providerUserID)
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
	var provider, providerUserID, role, granteeType string
	cmd := &cobra.Command{
		Use:   "add <project>",
		Short: "Grant access to a project",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCoreJSON(cmd, func(ctx context.Context, c *coreapi.Client) (any, error) {
				body := &coreapi.GrantProjectAccessInputBody{
					Provider:       provider,
					ProviderUserId: providerUserID,
					Role:           role,
				}
				if granteeType != "" {
					body.GranteeType = coreapi.NewOptGrantProjectAccessInputBodyGranteeType(coreapi.GrantProjectAccessInputBodyGranteeType(granteeType))
				}
				return c.GrantProjectAccess(ctx, body, coreapi.GrantProjectAccessParams{ProjectId: args[0]})
			})
		},
	}
	bindGranteeFlags(cmd, &provider, &providerUserID)
	cmd.Flags().StringVar(&role, "role", "", "project role (required)")
	cmd.Flags().StringVar(&granteeType, "grantee-type", "", "grantee kind: account, org, or team (default account)")
	markRequired(cmd, "role")
	return cmd
}

func newGrantProjectListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list <project>",
		Short: "List project members",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCoreList(cmd, projectGrantColumns, projectGrantRow, func(ctx context.Context, c *coreapi.Client) ([]coreapi.ProjectGrant, error) {
				out, err := c.ListProjectMembers(ctx, coreapi.ListProjectMembersParams{ProjectId: args[0]})
				if err != nil {
					return nil, err
				}
				return out.Members, nil
			})
		},
	}
}

func newGrantProjectRemoveCmd() *cobra.Command {
	var granteeType, granteeID string
	cmd := &cobra.Command{
		Use:   "remove <project>",
		Short: "Revoke project access from a grantee",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
				if err := c.RevokeProjectAccess(ctx, coreapi.RevokeProjectAccessParams{
					ProjectId:   args[0],
					GranteeType: granteeType,
					GranteeId:   granteeID,
				}); err != nil {
					return err
				}
				cmd.Printf("Revoked %s %s from project %s\n", granteeType, granteeID, args[0])
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&granteeType, "grantee-type", "", "grantee kind: account, org, or team (required)")
	cmd.Flags().StringVar(&granteeID, "grantee-id", "", "grantee ULID (required)")
	markRequired(cmd, "grantee-type", "grantee-id")
	return cmd
}

// --- repo grants ----------------------------------------------------------

func newGrantRepoCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "repo",
		Short: "Manage repo access",
	}
	cmd.AddCommand(newGrantRepoAddCmd())
	return cmd
}

func newGrantRepoAddCmd() *cobra.Command {
	var provider, providerUserID, role, granteeType string
	cmd := &cobra.Command{
		Use:   "add <repo>",
		Short: "Grant access to a repo",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCoreJSON(cmd, func(ctx context.Context, c *coreapi.Client) (any, error) {
				body := &coreapi.GrantRepoAccessInputBody{
					Provider:       provider,
					ProviderUserId: providerUserID,
					Role:           role,
				}
				if granteeType != "" {
					body.GranteeType = coreapi.NewOptGrantRepoAccessInputBodyGranteeType(coreapi.GrantRepoAccessInputBodyGranteeType(granteeType))
				}
				return c.GrantRepoAccess(ctx, body, coreapi.GrantRepoAccessParams{RepoId: args[0]})
			})
		},
	}
	bindGranteeFlags(cmd, &provider, &providerUserID)
	cmd.Flags().StringVar(&role, "role", "", "repo role (required)")
	cmd.Flags().StringVar(&granteeType, "grantee-type", "", "grantee kind: account, org, or team (default account)")
	markRequired(cmd, "role")
	return cmd
}

// bindGranteeFlags wires the shared --provider / --provider-user-id pair
// that identifies a grantee across the org/project/repo add+remove verbs,
// marking both required.
func bindGranteeFlags(cmd *cobra.Command, provider, providerUserID *string) {
	cmd.Flags().StringVar(provider, "provider", "", "identity provider (e.g. github) (required)")
	cmd.Flags().StringVar(providerUserID, "provider-user-id", "", "provider-specific user id (required)")
	markRequired(cmd, "provider", "provider-user-id")
}
