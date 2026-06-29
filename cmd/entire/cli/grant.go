package cli

import (
	"context"
	"errors"
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

// validateGrantGranteeType rejects grantee kinds the control plane no longer
// accepts when granting. A grant resolves to an account (from the provider
// identity), so "account" is the only valid kind ("" means the default,
// account). org/team granting was dropped server-side (COR-561) and the
// generated client enum is account-only, so catch it here with a clear message
// instead of an opaque enum-encoding error.
func validateGrantGranteeType(granteeType string) error {
	switch granteeType {
	case "", "account":
		return nil
	default:
		return fmt.Errorf("invalid --grantee-type %q: only \"account\" is supported", granteeType)
	}
}

// newGrantCmd is the hidden `entire grant` command group: manage access
// grants and org membership on the Entire control plane. Org, project, and
// repo each support add / list / remove. Surfaced via `entire labs`.
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

// repoGrantRow mirrors projectGrantRow; RepoGrant and ProjectGrant share the
// grantee-type/grantee/role shape, so both reuse projectGrantColumns.
func repoGrantRow(g coreapi.RepoGrant) []string {
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
				orgID, err := resolveOrgRef(ctx, c, args[0])
				if err != nil {
					return nil, err
				}
				return c.AddOrgMember(ctx, body, coreapi.AddOrgMemberParams{OrgId: orgID})
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
	var provider, providerUserID string
	cmd := &cobra.Command{
		Use:   "remove <org>",
		Short: "Remove a member from an org",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
				orgID, err := resolveOrgRef(ctx, c, args[0])
				if err != nil {
					return err
				}
				return revokeGrant(cmd, "Removed", fmt.Sprintf("%s/%s from org %s", provider, providerUserID, args[0]), func() error {
					return c.RemoveOrgMember(ctx, coreapi.RemoveOrgMemberParams{
						OrgId:          orgID,
						Provider:       provider,
						ProviderUserId: providerUserID,
					})
				})
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
			if err := validateGrantRole(role); err != nil {
				cmd.SilenceUsage = true
				return err
			}
			if err := validateGrantGranteeType(granteeType); err != nil {
				cmd.SilenceUsage = true
				return err
			}
			return runCoreJSON(cmd, func(ctx context.Context, c *coreapi.Client) (any, error) {
				projID, err := resolveProjectRef(ctx, c, args[0])
				if err != nil {
					return nil, err
				}
				body := &coreapi.GrantProjectAccessInputBody{
					Provider:       provider,
					ProviderUserId: providerUserID,
					Role:           coreapi.GrantProjectAccessInputBodyRole(role),
				}
				if granteeType != "" {
					body.GranteeType = coreapi.NewOptGrantProjectAccessInputBodyGranteeType(coreapi.GrantProjectAccessInputBodyGranteeType(granteeType))
				}
				return c.GrantProjectAccess(ctx, body, coreapi.GrantProjectAccessParams{ProjectId: projID})
			})
		},
	}
	bindGranteeFlags(cmd, &provider, &providerUserID)
	cmd.Flags().StringVar(&role, "role", "", "project role (required)")
	cmd.Flags().StringVar(&granteeType, "grantee-type", "", "grantee kind: account (the only supported kind; default)")
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
	var granteeType, granteeID, provider, providerUserID string
	cmd := &cobra.Command{
		Use:   "remove <project>",
		Short: "Revoke project access from a grantee",
		Long: "Revoke a grantee's access to a project (addressed by name or ULID). " +
			"Identify the grantee either by --provider/--provider-user-id (an " +
			"account, e.g. github + user id) or by --grantee-type account " +
			"--grantee-id <ULID>.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			mode, err := parseGranteeMode(provider, providerUserID, granteeType, granteeID)
			if err != nil {
				cmd.SilenceUsage = true
				return err
			}
			return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
				projID, err := resolveProjectRef(ctx, c, args[0])
				if err != nil {
					return err
				}
				if mode == granteeModeProvider {
					return revokeGrant(cmd, "Revoked", fmt.Sprintf("%s/%s from project %s", provider, providerUserID, args[0]), func() error {
						return c.RevokeProjectAccessByProvider(ctx, coreapi.RevokeProjectAccessByProviderParams{
							ProjectId:      projID,
							Provider:       provider,
							ProviderUserId: providerUserID,
						})
					})
				}
				return revokeGrant(cmd, "Revoked", fmt.Sprintf("%s %s from project %s", granteeType, granteeID, args[0]), func() error {
					return c.RevokeProjectAccess(ctx, coreapi.RevokeProjectAccessParams{
						ProjectId:   projID,
						GranteeType: granteeType,
						GranteeId:   granteeID,
					})
				})
			})
		},
	}
	cmd.Flags().StringVar(&granteeType, "grantee-type", "", "grantee kind: account (with --grantee-id)")
	cmd.Flags().StringVar(&granteeID, "grantee-id", "", "grantee ULID (with --grantee-type)")
	cmd.Flags().StringVar(&provider, "provider", "", "identity provider, e.g. github (with --provider-user-id)")
	cmd.Flags().StringVar(&providerUserID, "provider-user-id", "", "provider-specific user id (with --provider)")
	return cmd
}

// granteeMode names the two ways `grant project remove` / `grant repo remove`
// can address a grantee.
type granteeMode int

const (
	granteeModeProvider granteeMode = iota // --provider + --provider-user-id
	granteeModeID                          // --grantee-type + --grantee-id
)

// parseGranteeMode validates that exactly one addressing mode was supplied
// and fully specified, returning which one. The two modes are mutually
// exclusive: a provider account (github + user id) hits the by-provider revoke
// route, while a ULID grantee hits the typed-id route that also covers org and
// team grantees.
func parseGranteeMode(provider, providerUserID, granteeType, granteeID string) (granteeMode, error) {
	byProvider := provider != "" || providerUserID != ""
	byID := granteeType != "" || granteeID != ""
	switch {
	case byProvider && byID:
		return 0, errors.New("specify either --provider/--provider-user-id or --grantee-type/--grantee-id, not both")
	case byProvider:
		if provider == "" || providerUserID == "" {
			return 0, errors.New("both --provider and --provider-user-id are required")
		}
		return granteeModeProvider, nil
	case byID:
		if granteeType == "" || granteeID == "" {
			return 0, errors.New("both --grantee-type and --grantee-id are required")
		}
		return granteeModeID, nil
	default:
		return 0, errors.New("identify the grantee with --provider/--provider-user-id or --grantee-type/--grantee-id")
	}
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
	var provider, providerUserID, role, granteeType, project string
	cmd := &cobra.Command{
		Use:   "add <repo>",
		Short: "Grant access to a repo",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateGrantRole(role); err != nil {
				cmd.SilenceUsage = true
				return err
			}
			if err := validateGrantGranteeType(granteeType); err != nil {
				cmd.SilenceUsage = true
				return err
			}
			return runCoreJSON(cmd, func(ctx context.Context, c *coreapi.Client) (any, error) {
				repoID, err := resolveRepoRef(ctx, c, args[0], project)
				if err != nil {
					return nil, err
				}
				body := &coreapi.GrantRepoAccessInputBody{
					Provider:       provider,
					ProviderUserId: providerUserID,
					Role:           coreapi.GrantRepoAccessInputBodyRole(role),
				}
				if granteeType != "" {
					body.GranteeType = coreapi.NewOptGrantRepoAccessInputBodyGranteeType(coreapi.GrantRepoAccessInputBodyGranteeType(granteeType))
				}
				return c.GrantRepoAccess(ctx, body, coreapi.GrantRepoAccessParams{RepoId: repoID})
			})
		},
	}
	bindGranteeFlags(cmd, &provider, &providerUserID)
	cmd.Flags().StringVar(&role, "role", "", "repo role (required)")
	cmd.Flags().StringVar(&granteeType, "grantee-type", "", "grantee kind: account (the only supported kind; default)")
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
			return runCoreList(cmd, projectGrantColumns, repoGrantRow, func(ctx context.Context, c *coreapi.Client) ([]coreapi.RepoGrant, error) {
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
	var granteeType, granteeID, provider, providerUserID, project string
	cmd := &cobra.Command{
		Use:   "remove <repo>",
		Short: "Revoke repo access from a grantee",
		Long: "Revoke a grantee's access to a repo. Identify the grantee either by " +
			"--provider/--provider-user-id (an account, e.g. github + user id) or by " +
			"--grantee-type account --grantee-id <ULID>.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			mode, err := parseGranteeMode(provider, providerUserID, granteeType, granteeID)
			if err != nil {
				cmd.SilenceUsage = true
				return err
			}
			return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
				repoID, err := resolveRepoRef(ctx, c, args[0], project)
				if err != nil {
					return err
				}
				if mode == granteeModeProvider {
					return revokeGrant(cmd, "Revoked", fmt.Sprintf("%s/%s from repo %s", provider, providerUserID, args[0]), func() error {
						return c.RevokeRepoAccessByProvider(ctx, coreapi.RevokeRepoAccessByProviderParams{
							RepoId:         repoID,
							Provider:       provider,
							ProviderUserId: providerUserID,
						})
					})
				}
				return revokeGrant(cmd, "Revoked", fmt.Sprintf("%s %s from repo %s", granteeType, granteeID, args[0]), func() error {
					return c.RevokeRepoAccess(ctx, coreapi.RevokeRepoAccessParams{
						RepoId:      repoID,
						GranteeType: granteeType,
						GranteeId:   granteeID,
					})
				})
			})
		},
	}
	cmd.Flags().StringVar(&granteeType, "grantee-type", "", "grantee kind: account (with --grantee-id)")
	cmd.Flags().StringVar(&granteeID, "grantee-id", "", "grantee ULID (with --grantee-type)")
	cmd.Flags().StringVar(&provider, "provider", "", "identity provider, e.g. github (with --provider-user-id)")
	cmd.Flags().StringVar(&providerUserID, "provider-user-id", "", "provider-specific user id (with --provider)")
	bindRepoProjectFlag(cmd, &project)
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

// revokeGrant runs a grant-removal API call idempotently. A 404 means the
// grantee already has no such grant — the desired end state — so it's reported
// as a no-op rather than surfaced as a raw error, matching runControlPlaneDelete.
// verb is the success word ("Revoked"/"Removed"); subject describes the grant,
// e.g. "github/12345 from repo acme".
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
