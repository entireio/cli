package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/coreapi"
)

// The `grant` subtrees under `entire org`, `entire project` and `entire repo`:
// one builder, three target descriptions. Every leaf gives a user access to a
// target — `add <target> <grantee> --role`, `list <target>`, `remove <target>
// <grantee>` — and only the target kind changes: how it is addressed, which
// roles it has, and which generated client calls it maps to. The builder owns
// the command shape and the shared plumbing; a grantTarget owns the typed calls.
//
// Grantees are addressed by a provider-qualified handle (e.g. github:alice),
// which the CLI resolves to the provider account behind the scenes. `remove`
// also accepts an account ULID where the API has a typed-id route (project and
// repo). A user account is the only grantee kind the API grants to today.

// grantTarget describes one resource kind the shared `<noun> grant` subtree
// manages. Row is the wire type of one listing entry.
type grantTarget[Row any] struct {
	noun        string   // "org" | "project" | "repo": in Use, help and messages
	refUsage    string   // how a target is addressed, for Long: "name or ULID"; reads after "addressed by"
	exampleRef  string   // a target ref for the Example lines
	roles       []string // accepted --role values, in help order
	defaultRole string   // "" means --role is required; else the server default applied when --role is omitted
	columns     []string
	row         func(Row) []string

	// resolve turns the user's target ref into its ULID. project and repo wrap
	// theirs because those resolvers take an interface, not *coreapi.Client.
	resolve func(ctx context.Context, c *coreapi.Client, ref string) (string, error)
	// grant gives the provider account the role on the resolved target and
	// returns the effective role: the server's, when role was left to default.
	grant            func(ctx context.Context, c *coreapi.Client, id, provider, providerUserID, role string) (granted string, wire any, err error)
	list             func(ctx context.Context, c *coreapi.Client, id string, pageToken coreapi.OptString) ([]Row, string, error)
	revokeByProvider func(ctx context.Context, c *coreapi.Client, id, provider, providerUserID string) error
	// revokeByID is the typed-id route for an account ULID grantee. nil when
	// the target has none (org), so a ULID grantee falls through to
	// resolveGranteeProvider and is refused with the handle form named.
	revokeByID func(ctx context.Context, c *coreapi.Client, id, granteeID string) error

	// unwritableRef reports a ref that names something this target's `add` and
	// `remove` cannot write — the repo target's GitHub mirrors, whose access is
	// the upstream repository's. Checked before required flags are validated or
	// anything is dialed, so the answer is about the repo the user named rather
	// than a missing --role they would then supply for nothing. nil where every
	// ref a target parses is writable (org, project).
	unwritableRef func(ref string) error

	// listBranch is the second reading `list` gives a target ref, for a target
	// whose refs do not all name the same thing: a repo is an Entire repository
	// or a GitHub mirror, and only the first has grants. nil for org and
	// project, which have one kind of target each.
	listBranch *grantListBranch
}

// grantListBranch is the second answer a `list` leaf can give: claims reports
// which refs it answers for, leaving every other ref to the target's own
// resolver, and list answers one of them. long and example become the leaf's
// help, because a leaf taking two kinds of ref is the only one with more to say
// than its Short.
type grantListBranch struct {
	long    string
	example string
	claims  func(ref string) bool
	list    func(cmd *cobra.Command, ref string) error
}

func newOrgGrantCmd() *cobra.Command     { return newGrantSubtreeCmd(orgGrantTarget) }
func newProjectGrantCmd() *cobra.Command { return newGrantSubtreeCmd(projectGrantTarget) }
func newRepoGrantCmd() *cobra.Command    { return newGrantSubtreeCmd(repoGrantTarget) }

func newGrantSubtreeCmd[Row any](t grantTarget[Row]) *cobra.Command {
	cmd := &cobra.Command{
		Use:   cmdGrant,
		Short: "Manage " + t.noun + " access",
	}
	cmd.AddCommand(newGrantAddCmd(t), newGrantListCmd(t), newGrantRemoveCmd(t))
	return requireSubcommand(cmd)
}

func newGrantAddCmd[Row any](t grantTarget[Row]) *cobra.Command {
	var role string
	required := t.defaultRole == ""
	example := fmt.Sprintf("  entire %s grant add %s github:alice", t.noun, t.exampleRef)
	roleHelp := "Role: one of " + strings.Join(t.roles, ", ")
	if required {
		example += " --role " + t.roles[0]
		roleHelp += " (required)"
	} else {
		roleHelp += " (default " + t.defaultRole + ")"
	}
	cmd := &cobra.Command{
		Use:     fmt.Sprintf("add <%s> <grantee>", t.noun),
		Short:   fmt.Sprintf("Grant a user %s access", t.noun),
		Long:    fmt.Sprintf("Grant a user (addressed as provider:handle, e.g. github:alice) %s access. The %s is addressed by %s.", t.noun, t.noun, t.refUsage),
		Example: example,
		Args:    cobra.ExactArgs(2),
		PreRunE: refuseUnwritableRef(t),
		RunE: func(cmd *cobra.Command, args []string) error {
			// A role the user typed is always checked, an explicit `--role=`
			// included: markRequired asks only whether the flag was given, and
			// on org an empty value is not the same as leaving the flag out.
			// Only an omitted --role means the server default, which exists
			// only where required is false.
			if cmd.Flags().Changed("role") {
				if err := validateRole(role, t.roles); err != nil {
					cmd.SilenceUsage = true
					return err
				}
			}
			return runCoreMutation(cmd, func(ctx context.Context, c *coreapi.Client) (string, any, error) {
				id, err := t.resolve(ctx, c, args[0])
				if err != nil {
					return "", nil, err
				}
				provider, providerUserID, err := resolveGranteeProvider(ctx, c, args[1])
				if err != nil {
					return "", nil, err
				}
				granted, wire, err := t.grant(ctx, c, id, provider, providerUserID, role)
				if err != nil {
					return "", nil, err
				}
				return fmt.Sprintf("✓ Granted %s %s access to %s %s", args[1], granted, t.noun, args[0]), wire, nil
			})
		},
	}
	cmd.Flags().StringVar(&role, "role", "", roleHelp)
	if required {
		markRequired(cmd, "role")
	}
	addJSONFlag(cmd)
	return cmd
}

func newGrantListCmd[Row any](t grantTarget[Row]) *cobra.Command {
	cmd := &cobra.Command{
		Use:   fmt.Sprintf("list <%s>", t.noun),
		Short: fmt.Sprintf("List who has %s access", t.noun),
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if t.listBranch != nil && t.listBranch.claims(args[0]) {
				return t.listBranch.list(cmd, args[0])
			}
			return runCoreList(cmd, "No grants found.", t.columns, t.row, func(ctx context.Context, c *coreapi.Client) ([]Row, error) {
				id, err := t.resolve(ctx, c, args[0])
				if err != nil {
					return nil, err
				}
				return fetchAllPages(ctx, func(ctx context.Context, cursor string) ([]Row, string, error) {
					var pageToken coreapi.OptString
					if cursor != "" {
						pageToken = coreapi.NewOptString(cursor)
					}
					return t.list(ctx, c, id, pageToken)
				})
			})
		},
	}
	addJSONFlag(cmd)
	if t.listBranch != nil {
		cmd.Long, cmd.Example = t.listBranch.long, t.listBranch.example
	}
	return cmd
}

func newGrantRemoveCmd[Row any](t grantTarget[Row]) *cobra.Command {
	grantee := "a provider-qualified handle (e.g. github:alice)"
	if t.revokeByID != nil {
		grantee += " or an account ULID"
	}
	return &cobra.Command{
		Use:     fmt.Sprintf("remove <%s> <grantee>", t.noun),
		Short:   fmt.Sprintf("Revoke a user's %s access", t.noun),
		Long:    fmt.Sprintf("Revoke a grantee's %s access. The %s is addressed by %s; the grantee is %s.", t.noun, t.noun, t.refUsage, grantee),
		Example: fmt.Sprintf("  entire %s grant remove %s github:alice", t.noun, t.exampleRef),
		Args:    cobra.ExactArgs(2),
		PreRunE: refuseUnwritableRef(t),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
				id, err := t.resolve(ctx, c, args[0])
				if err != nil {
					return err
				}
				target := t.noun + " " + args[0]
				if t.revokeByID != nil && looksLikeULID(args[1]) {
					return revokeGrant(cmd, "account "+args[1]+" from "+target, func() error {
						return t.revokeByID(ctx, c, id, args[1])
					})
				}
				provider, providerUserID, err := resolveGranteeProvider(ctx, c, args[1])
				if err != nil {
					return err
				}
				return revokeGrant(cmd, args[1]+" from "+target, func() error {
					return t.revokeByProvider(ctx, c, id, provider, providerUserID)
				})
			})
		},
	}
}

// refuseUnwritableRef is the write verbs' first question: does this ref name
// something the target can write at all? Cobra runs PreRunE before it validates
// required flags, which is the point — `repo grant add /gh/acme/widget alice`
// is answered with what is wrong (a mirror's access lives on GitHub) rather
// than sending the user to add a --role that changes nothing. nil for a target
// with no such ref, which leaves the hook off the command entirely.
func refuseUnwritableRef[Row any](t grantTarget[Row]) func(*cobra.Command, []string) error {
	if t.unwritableRef == nil {
		return nil
	}
	return func(cmd *cobra.Command, args []string) error {
		if err := t.unwritableRef(args[0]); err != nil {
			cmd.SilenceUsage = true
			return err
		}
		return nil
	}
}

// validateRole rejects a --role outside the target's set at the CLI boundary
// so the user gets a clear message instead of a server 422. The generated
// bodies use a distinct enum type per target that shares these values, so the
// targets cast the validated string to whichever type they need.
func validateRole(role string, allowed []string) error {
	if slices.Contains(allowed, role) {
		return nil
	}
	return fmt.Errorf("invalid --role %q: must be one of %s", role, strings.Join(allowed, ", "))
}

// revokeGrant runs a grant-removal API call idempotently. A 404 means the
// grantee already has no such grant — the desired end state — so it's reported
// as a no-op rather than surfaced as a raw error, matching runControlPlaneDelete.
// subject describes the grant, e.g. "github:alice from repo /et/acme/web".
func revokeGrant(cmd *cobra.Command, subject string, revoke func() error) error {
	if err := revoke(); err != nil {
		if isCoreNotFound(err) {
			fmt.Fprintf(cmd.OutOrStdout(), "%s: no such grant; nothing to revoke\n", subject)
			return nil
		}
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "✓ Revoked %s\n", subject)
	return nil
}

// orgMemberColumns / grantColumns are the human table views of the
// membership/grant listings. Both lead with GRANTEE, the friendly name the
// server resolved (a provider:handle, or an org name), falling back to the
// ULID. Org membership is flat, so it carries the membership STATUS instead
// of provenance; project and repo grants include owner and inherited rows,
// so they add SOURCE and TYPE. No table prints an internal id: the grantee
// ULID is in the --json output for anyone who needs it.
var (
	orgMemberColumns = []string{colHeaderGrantee, colHeaderRole, colHeaderStatus}
	grantColumns     = []string{colHeaderGrantee, colHeaderRole, "SOURCE", "TYPE"}
)

func orgMemberRow(m coreapi.Membership) []string {
	return []string{granteeName(m.Handle, m.AccountId), m.Role, m.Status}
}

func projectGrantRow(g coreapi.ProjectGrant) []string {
	return []string{granteeName(g.GranteeName, g.GranteeId), g.Role, g.Source, g.GranteeType}
}

// repoGrantRow mirrors projectGrantRow; RepoGrant and ProjectGrant share the
// grantee/role/source shape, so both reuse grantColumns.
func repoGrantRow(g coreapi.RepoGrant) []string {
	return []string{granteeName(g.GranteeName, g.GranteeId), g.Role, g.Source, g.GranteeType}
}

// granteeName returns the friendly name when the server resolved one, falling
// back to the ULID for grantees it couldn't label (e.g. teams).
func granteeName(name coreapi.OptString, granteeID string) string {
	if n := name.Or(""); n != "" {
		return n
	}
	return granteeID
}

// granteeTypeAccount is the only grantee kind the revoke-by-id calls take; the
// provider-qualified variant has its own endpoint.
const granteeTypeAccount = "account"

// accessRoles are the project and repo grant roles; the two targets share the
// set because the server's SpiceDB relations are the same for both.
var accessRoles = []string{"reader", "writer", "admin"}

// orgGrantTarget is org membership: roles owner/admin/member with member as
// the server default, a target addressed by name or ULID, and no typed-id
// revoke route — members are removed by their provider identity.
var orgGrantTarget = grantTarget[coreapi.Membership]{
	noun:        cmdOrg,
	refUsage:    "name or ULID",
	exampleRef:  "acme",
	roles:       []string{"owner", "admin", "member"},
	defaultRole: "member",
	columns:     orgMemberColumns,
	row:         orgMemberRow,
	resolve:     resolveOrgRef,
	grant: func(ctx context.Context, c *coreapi.Client, id, provider, providerUserID, role string) (string, any, error) {
		body := &coreapi.AddOrgMemberInputBody{Provider: provider, ProviderUserId: providerUserID}
		if role != "" {
			body.Role = coreapi.NewOptAddOrgMemberInputBodyRole(coreapi.AddOrgMemberInputBodyRole(role))
		}
		m, err := c.AddOrgMember(ctx, body, coreapi.AddOrgMemberParams{OrgId: id})
		if err != nil {
			return "", nil, err
		}
		return m.Role, m, nil
	},
	list: func(ctx context.Context, c *coreapi.Client, id string, pageToken coreapi.OptString) ([]coreapi.Membership, string, error) {
		out, err := c.ListOrgMembers(ctx, coreapi.ListOrgMembersParams{OrgId: id, PageToken: pageToken})
		if err != nil {
			return nil, "", err
		}
		return out.Members, out.NextPageToken.Or(""), nil
	},
	revokeByProvider: func(ctx context.Context, c *coreapi.Client, id, provider, providerUserID string) error {
		return c.RemoveOrgMember(ctx, coreapi.RemoveOrgMemberParams{OrgId: id, Provider: provider, ProviderUserId: providerUserID})
	},
}

// projectGrantTarget is project access: roles reader/writer/admin, required,
// on a project addressed by name or ULID.
var projectGrantTarget = grantTarget[coreapi.ProjectGrant]{
	noun:       "project",
	refUsage:   "name or ULID",
	exampleRef: "widgets",
	roles:      accessRoles,
	columns:    grantColumns,
	row:        projectGrantRow,
	resolve: func(ctx context.Context, c *coreapi.Client, ref string) (string, error) {
		return resolveProjectRef(ctx, c, ref)
	},
	grant: func(ctx context.Context, c *coreapi.Client, id, provider, providerUserID, role string) (string, any, error) {
		out, err := c.GrantProjectAccess(ctx, &coreapi.GrantProjectAccessInputBody{
			Provider:       provider,
			ProviderUserId: providerUserID,
			Role:           coreapi.GrantProjectAccessInputBodyRole(role),
		}, coreapi.GrantProjectAccessParams{ProjectId: id})
		if err != nil {
			return "", nil, err
		}
		return role, out, nil
	},
	list: func(ctx context.Context, c *coreapi.Client, id string, pageToken coreapi.OptString) ([]coreapi.ProjectGrant, string, error) {
		out, err := c.ListProjectMembers(ctx, coreapi.ListProjectMembersParams{ProjectId: id, PageToken: pageToken})
		if err != nil {
			return nil, "", err
		}
		return out.Members, out.NextPageToken.Or(""), nil
	},
	revokeByProvider: func(ctx context.Context, c *coreapi.Client, id, provider, providerUserID string) error {
		return c.RevokeProjectAccessByProvider(ctx, coreapi.RevokeProjectAccessByProviderParams{ProjectId: id, Provider: provider, ProviderUserId: providerUserID})
	},
	revokeByID: func(ctx context.Context, c *coreapi.Client, id, granteeID string) error {
		return c.RevokeProjectAccess(ctx, coreapi.RevokeProjectAccessParams{ProjectId: id, GranteeType: granteeTypeAccount, GranteeId: granteeID})
	},
}

// repoGrantTarget is repo access: roles reader/writer/admin, required, on a
// repo addressed by its /et/<project>/<repo> path and nothing else. `list`
// alone also answers a GitHub mirror ref, from the upstream collaborators the
// placement materializes — see mirrorGrantListing.
var repoGrantTarget = grantTarget[coreapi.RepoGrant]{
	noun:          cmdRepo,
	refUsage:      "its /" + nativeCloneForge + "/<project>/<repo> path",
	exampleRef:    "/" + nativeCloneForge + "/acme/web",
	roles:         accessRoles,
	columns:       grantColumns,
	row:           repoGrantRow,
	listBranch:    mirrorGrantListing,
	unwritableRef: mirrorGrantsAreUpstream,
	resolve: func(ctx context.Context, c *coreapi.Client, ref string) (string, error) {
		return resolveRepoPath(ctx, c, ref)
	},
	grant: func(ctx context.Context, c *coreapi.Client, id, provider, providerUserID, role string) (string, any, error) {
		out, err := c.GrantRepoAccess(ctx, &coreapi.GrantRepoAccessInputBody{
			Provider:       provider,
			ProviderUserId: providerUserID,
			Role:           coreapi.GrantRepoAccessInputBodyRole(role),
		}, coreapi.GrantRepoAccessParams{RepoId: id})
		if err != nil {
			return "", nil, err
		}
		return role, out, nil
	},
	list: func(ctx context.Context, c *coreapi.Client, id string, pageToken coreapi.OptString) ([]coreapi.RepoGrant, string, error) {
		out, err := c.ListRepoGrants(ctx, coreapi.ListRepoGrantsParams{RepoId: id, PageToken: pageToken})
		if err != nil {
			return nil, "", err
		}
		return out.Grants, out.NextPageToken.Or(""), nil
	},
	revokeByProvider: func(ctx context.Context, c *coreapi.Client, id, provider, providerUserID string) error {
		return c.RevokeRepoAccessByProvider(ctx, coreapi.RevokeRepoAccessByProviderParams{RepoId: id, Provider: provider, ProviderUserId: providerUserID})
	},
	revokeByID: func(ctx context.Context, c *coreapi.Client, id, granteeID string) error {
		return c.RevokeRepoAccess(ctx, coreapi.RevokeRepoAccessParams{RepoId: id, GranteeType: granteeTypeAccount, GranteeId: granteeID})
	},
}
