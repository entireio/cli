package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/coreapi"
)

// Org invitations and resource invitations have different wire types.
type inviteScope[Inv any] struct {
	noun       string // "org" | "project" | "repo": in Use, help and messages
	prose      string // the noun in a sentence: "organization", "project", "repository"
	article    string // "a" or "an", for prose
	refUsage   string // how the scope is addressed, for Long: reads after "addressed by"
	exampleRef string
	resolve    func(ctx context.Context, c *coreapi.Client, ref string) (string, error)
	list       func(ctx context.Context, c *coreapi.Client, id, status, cursor string) ([]Inv, string, error)
	revoke     func(ctx context.Context, c *coreapi.Client, id, invitationID string) error
	// view reads the fields the verbs use, and scrub drops the response
	// properties the schema does not declare before --json prints the entry.
	view  func(Inv) invitationView
	scrub func(*Inv)
	// webURLRefs reads a pasted entire.com URL as the path it spells before
	// resolve sees it, as grantTarget.webURLRefs does. Set for repo alone.
	webURLRefs bool
}

// typedRef is the scope ref every leaf works from: resolve reads canon, and the
// messages that echo the ref use typed. Without webURLRefs the argument is used
// exactly as given.
func (s inviteScope[Inv]) typedRef(arg string) typedRef {
	if !s.webURLRefs {
		return typedRef{typed: arg, canon: arg}
	}
	return newTypedRef(arg)
}

// invitationView is what the verbs need of one invitation, whichever wire type
// carried it.
type invitationView struct {
	id, role, status string
	email            string // "" for an account-targeted invitation
	accountID        string // the invited account; "" for an email invitation
	identity         coreapi.OptAccountIdentity
	expiresAt        time.Time
}

func (v invitationView) invitee() string {
	if v.email != "" {
		return v.email
	}
	if id, ok := v.identity.Get(); ok {
		if h, ok := primaryHandle(id.Handles); ok {
			return displayQualifiedHandle(h.Provider, h.Handle)
		}
	}
	return orDash(v.accountID)
}

func primaryHandle(handles []coreapi.AccountProfileHandle) (coreapi.AccountProfileHandle, bool) {
	for _, h := range handles {
		if h.IsPrimary {
			return h, true
		}
	}
	if len(handles) == 0 {
		return coreapi.AccountProfileHandle{}, false
	}
	return handles[0], true
}

var invitationColumns = []string{"ID", "INVITEE", colHeaderRole, colHeaderStatus, "EXPIRES"}

func (v invitationView) row() []string {
	return []string{v.id, v.invitee(), v.role, v.status, v.expiresAt.Format("2006-01-02")}
}

// invitationStatuses are the --status filter values. "all" is the API's own
// name for "every state", not a client-side wildcard.
var invitationStatuses = []string{"open", "accepted", "revoked", "expired", "all"}

func newInviteListCmd[Inv any](s inviteScope[Inv]) *cobra.Command {
	var status string
	cmd := &cobra.Command{
		Use:   fmt.Sprintf("list <%s>", s.noun),
		Short: fmt.Sprintf("List %s %s's invitations", s.article, s.prose),
		Long: fmt.Sprintf("List %s %s's invitations, open ones by default. The %s is addressed by %s. "+
			"An invitation to an existing account shows the account's handle, or its account ID when the handle cannot be resolved, instead of an email address.",
			s.article, s.prose, s.noun, s.refUsage),
		Example: fmt.Sprintf("  entire %s invite list %s --status all", s.noun, s.exampleRef),
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateChoice("status", status, invitationStatuses); err != nil {
				cmd.SilenceUsage = true
				return err
			}
			tr := s.typedRef(args[0])
			return runCoreList(cmd, "No invitations found.", invitationColumns, func(i Inv) []string { return s.view(i).row() },
				func(ctx context.Context, c *coreapi.Client) ([]Inv, error) {
					id, err := s.resolve(ctx, c, tr.canon)
					if err != nil {
						return nil, tr.wrapErr(err)
					}
					return s.listAll(ctx, c, id, status)
				})
		},
	}
	cmd.Flags().StringVar(&status, "status", "open", "Lifecycle state to list: one of "+strings.Join(invitationStatuses, ", "))
	addJSONFlag(cmd)
	return cmd
}

// Drop undeclared fields so --json cannot expose an acceptance token.
func (s inviteScope[Inv]) listAll(ctx context.Context, c *coreapi.Client, id, status string) ([]Inv, error) {
	invitations, err := fetchAllPages(ctx, func(ctx context.Context, cursor string) ([]Inv, string, error) {
		return s.list(ctx, c, id, status, cursor)
	})
	if err != nil {
		return nil, err
	}
	for i := range invitations {
		s.scrub(&invitations[i])
	}
	return invitations, nil
}

func newInviteRevokeCmd[Inv any](s inviteScope[Inv]) *cobra.Command {
	var email, invitationID string
	cmd := &cobra.Command{
		Use:   fmt.Sprintf("revoke <%s> (--id <invitation> | --email <email>)", s.noun),
		Short: fmt.Sprintf("Revoke %s %s invitation", s.article, s.prose),
		Long: fmt.Sprintf("Revoke an open invitation so it can no longer be accepted and its link stops working. The %s is addressed by %s. "+
			"Name the invitation by --id, as `entire %s invite list` prints it and `grant add` reports it, or by --email for an invitation sent to an address.",
			s.noun, s.refUsage, s.noun),
		Example: fmt.Sprintf("  entire %s invite revoke %s --id 01M7BDT64K8BTHRT2996R6XNG2\n  entire %s invite revoke %s --email dev@example.com",
			s.noun, s.exampleRef, s.noun, s.exampleRef),
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			tr := s.typedRef(args[0])
			if invitationID != "" && !looksLikeULID(invitationID) {
				cmd.SilenceUsage = true
				return fmt.Errorf("invalid --id %q: an invitation ID is a ULID, as `entire %s invite list %s` prints it", invitationID, s.noun, tr.typed)
			}
			return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
				id, err := s.resolve(ctx, c, tr.canon)
				if err != nil {
					return tr.wrapErr(err)
				}
				target := s.noun + " " + redactRefForError(tr.canon)
				if invitationID != "" {
					return revokeResource(cmd, "the invitation "+invitationID+" to "+target, "invitation", func() error {
						return s.revoke(ctx, c, id, invitationID)
					})
				}
				subject := "the invitation for " + email + " to " + target
				found, err := s.openInvitationIDForEmail(ctx, c, id, email)
				if err != nil {
					return err
				}
				if found == "" {
					// Same end state as revokeGrant's 404: nothing is open for
					// that address, so there is nothing to revoke.
					fmt.Fprintf(cmd.OutOrStdout(), "%s: no open invitation; nothing to revoke\n", subject)
					return nil
				}
				return revokeResource(cmd, subject, "invitation", func() error {
					return s.revoke(ctx, c, id, found)
				})
			})
		},
	}
	cmd.Flags().StringVar(&invitationID, "id", "", "Invitation `ID` to revoke, as invite list prints it")
	cmd.Flags().StringVar(&email, "email", "", "Email address whose open invitation to revoke")
	cmd.MarkFlagsMutuallyExclusive("id", "email")
	cmd.MarkFlagsOneRequired("id", "email")
	return cmd
}

// Core revokes by ID, so resolve --email against open invitations.
func (s inviteScope[Inv]) openInvitationIDForEmail(ctx context.Context, c *coreapi.Client, id, email string) (string, error) {
	invitations, err := s.listAll(ctx, c, id, "open")
	if err != nil {
		return "", err
	}
	for _, inv := range invitations {
		if v := s.view(inv); v.email != "" && strings.EqualFold(v.email, email) {
			return v.id, nil
		}
	}
	return "", nil
}

func inviteHint(noun, ref string) string {
	return fmt.Sprintf("Find the open invitation with `entire %s invite list %s`, then revoke it with `entire %s invite revoke %s --id <id>`.", noun, ref, noun, ref)
}

func newScopeInviteCmd[Inv any](s inviteScope[Inv], extra ...*cobra.Command) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "invite",
		Short: fmt.Sprintf("Manage %s invitations", s.prose),
	}
	cmd.AddCommand(extra...)
	cmd.AddCommand(newInviteListCmd(s), newInviteRevokeCmd(s))
	return requireSubcommand(cmd)
}

func newProjectInviteCmd() *cobra.Command { return newScopeInviteCmd(projectInviteScope) }
func newRepoInviteCmd() *cobra.Command    { return newScopeInviteCmd(repoInviteScope) }

const orgInviteRefUsage = "name"

var orgInviteScope = inviteScope[coreapi.Invitation]{
	noun:       cmdOrg,
	prose:      "organization",
	article:    "an",
	refUsage:   orgInviteRefUsage,
	exampleRef: "acme",
	resolve:    resolveInviteOrg,
	list: func(ctx context.Context, c *coreapi.Client, id, status, cursor string) ([]coreapi.Invitation, string, error) {
		params := coreapi.ListOrgInvitationsParams{OrgId: id, Status: coreapi.NewOptListOrgInvitationsStatus(coreapi.ListOrgInvitationsStatus(status))}
		if cursor != "" {
			params.PageToken = coreapi.NewOptString(cursor)
		}
		out, err := c.ListOrgInvitations(ctx, params)
		if err != nil {
			return nil, "", err
		}
		return out.Invitations, out.NextPageToken.Or(""), nil
	},
	revoke: func(ctx context.Context, c *coreapi.Client, id, invitationID string) error {
		return c.RevokeOrgInvitation(ctx, coreapi.RevokeOrgInvitationParams{OrgId: id, ID: invitationID})
	},
	view: func(i coreapi.Invitation) invitationView {
		return invitationView{id: i.ID, role: i.Role, status: i.Status, email: i.Email.Or(""), accountID: i.InviteeAccountId.Or(""), identity: i.InviteeIdentity, expiresAt: i.ExpiresAt}
	},
	scrub: func(i *coreapi.Invitation) { i.AdditionalProps = nil },
}

var projectInviteScope = inviteScope[coreapi.ResourceInvitation]{
	noun:       projectGrantTarget.noun,
	prose:      projectGrantTarget.noun,
	article:    "a",
	refUsage:   projectGrantTarget.refUsage,
	exampleRef: projectGrantTarget.exampleRef,
	resolve:    projectGrantTarget.resolve,
	list: func(ctx context.Context, c *coreapi.Client, id, status, cursor string) ([]coreapi.ResourceInvitation, string, error) {
		params := coreapi.ListProjectInvitationsParams{ProjectId: id, Status: coreapi.NewOptListProjectInvitationsStatus(coreapi.ListProjectInvitationsStatus(status))}
		if cursor != "" {
			params.PageToken = coreapi.NewOptString(cursor)
		}
		out, err := c.ListProjectInvitations(ctx, params)
		if err != nil {
			return nil, "", err
		}
		return out.Invitations, out.NextPageToken.Or(""), nil
	},
	revoke: func(ctx context.Context, c *coreapi.Client, id, invitationID string) error {
		return c.RevokeProjectInvitation(ctx, coreapi.RevokeProjectInvitationParams{ProjectId: id, ID: invitationID})
	},
	view:  resourceInvitationView,
	scrub: scrubResourceInvitation,
}

var repoInviteScope = inviteScope[coreapi.ResourceInvitation]{
	noun:       cmdRepo,
	prose:      "repository",
	article:    "a",
	refUsage:   repoGrantTarget.refUsage,
	exampleRef: repoGrantTarget.exampleRef,
	resolve:    repoGrantTarget.resolve,
	// Read through repoGrantTarget so the two repo subtrees agree on what an
	// entire.com URL means.
	webURLRefs: repoGrantTarget.webURLRefs,
	list: func(ctx context.Context, c *coreapi.Client, id, status, cursor string) ([]coreapi.ResourceInvitation, string, error) {
		params := coreapi.ListRepoInvitationsParams{RepoId: id, Status: coreapi.NewOptListRepoInvitationsStatus(coreapi.ListRepoInvitationsStatus(status))}
		if cursor != "" {
			params.PageToken = coreapi.NewOptString(cursor)
		}
		out, err := c.ListRepoInvitations(ctx, params)
		if err != nil {
			return nil, "", err
		}
		return out.Invitations, out.NextPageToken.Or(""), nil
	},
	revoke: func(ctx context.Context, c *coreapi.Client, id, invitationID string) error {
		return c.RevokeRepoInvitation(ctx, coreapi.RevokeRepoInvitationParams{RepoId: id, ID: invitationID})
	},
	view:  resourceInvitationView,
	scrub: scrubResourceInvitation,
}

func resourceInvitationView(i coreapi.ResourceInvitation) invitationView {
	return invitationView{id: i.ID, role: i.Role, status: i.Status, email: i.Email.Or(""), accountID: i.InviteeAccountId.Or(""), identity: i.InviteeIdentity, expiresAt: i.ExpiresAt}
}

func scrubResourceInvitation(i *coreapi.ResourceInvitation) { i.AdditionalProps = nil }
