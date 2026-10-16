package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/coreapi"
)

// The invitation verbs. `entire org invite` groups `send`, which creates an
// invitation, with `list` and `revoke` managing the ones already sent. An
// invitation is how an org grants membership to someone the control plane
// cannot name yet: `grant add` needs an existing provider account, an
// invitation needs only an email address.
//
// Who may invite with which role is the server's decision: it answers 403 for
// a role the caller cannot delegate. The CLI checks only that --role spells one
// of the values the API declares, so there is one place where "an admin may not
// mint owners" is decided.

// invitationStatuses are the --status filter values. "all" is the API's own
// name for "every state", not a client-side wildcard.
var invitationStatuses = []string{"open", "accepted", "revoked", "expired", "all"}

// invitationColumns leads with the invitation ULID, the stable handle for an
// invitation, as the org columns lead with the org's.
var invitationColumns = []string{"ID", "EMAIL", colHeaderRole, colHeaderStatus, "EXPIRES"}

func invitationRow(i coreapi.Invitation) []string {
	return []string{i.ID, i.Email, i.Role, i.Status, i.ExpiresAt.Format("2006-01-02")}
}

func newOrgInviteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "invite",
		Short: "Manage organization invitations",
	}
	cmd.AddCommand(newOrgInviteSendCmd(), newOrgInviteListCmd(), newOrgInviteRevokeCmd())
	return requireSubcommand(cmd)
}

func newOrgInviteSendCmd() *cobra.Command {
	var email, role string
	cmd := &cobra.Command{
		Use:     "send <org> --email <email>",
		Short:   "Invite an email address to an organization",
		Long:    "Invite an email address to an organization. The org is addressed by name. The invited address receives a link to accept. Inviting an address that already has an open invitation sends the mail again and keeps the role the invitation was created with.",
		Example: "  entire org invite send acme --email dev@example.com --role admin",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("role") {
				if err := validateChoice("role", role, orgRoles); err != nil {
					cmd.SilenceUsage = true
					return err
				}
			}
			return runCoreMutation(cmd, func(ctx context.Context, c *coreapi.Client) (string, any, error) {
				orgID, err := resolveInviteOrg(ctx, c, args[0])
				if err != nil {
					return "", nil, err
				}
				body := &coreapi.CreateOrgInvitationInputBody{
					Email: email,
					Role:  coreapi.CreateOrgInvitationInputBodyRole(role),
				}
				res, err := c.CreateOrgInvitation(ctx, body, coreapi.CreateOrgInvitationParams{OrgId: orgID})
				if err != nil {
					return "", nil, err
				}
				switch out := res.(type) {
				case *coreapi.CreateOrgInvitationCreated:
					// An invitation is the one object with an accept token. Its
					// modeled fields carry none today, but ogen
					// round-trips any response property this schema doesn't
					// declare, verbatim, into --json output, so blank the bag
					// rather than trust the endpoint's contract never grows one.
					inv := coreapi.Invitation(*out)
					inv.AdditionalProps = nil
					return fmt.Sprintf("✓ Invited %s to org %s as %s (%s)", inv.Email, args[0], inv.Role, inv.ID), &inv, nil
				case *coreapi.CreateOrgInvitationOK:
					// The role here is the stored one, which an earlier invite
					// chose; saying so stops a --role that did not take effect
					// from reading as though it had.
					inv := coreapi.Invitation(*out)
					inv.AdditionalProps = nil
					return fmt.Sprintf("✓ Resent the open invitation for %s to org %s, which invites as %s (%s)", inv.Email, args[0], inv.Role, inv.ID), &inv, nil
				default:
					return "", nil, fmt.Errorf("invite %s: unexpected response %T from the control plane", email, res)
				}
			})
		},
	}
	// The wire field is required, so an omitted flag still sends a role: the
	// same default the API documents.
	cmd.Flags().StringVar(&email, "email", "", "Email address to invite")
	cmd.Flags().StringVar(&role, "role", orgRoleMember, "Role the invitation grants: one of "+strings.Join(orgRoles, ", "))
	markRequired(cmd, "email")
	addJSONFlag(cmd)
	return cmd
}

func newOrgInviteListCmd() *cobra.Command {
	var status string
	cmd := &cobra.Command{
		Use:     "list <org>",
		Short:   "List an organization's invitations",
		Long:    "List an organization's invitations, open ones by default. The org is addressed by name.",
		Example: "  entire org invite list acme --status all",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateChoice("status", status, invitationStatuses); err != nil {
				cmd.SilenceUsage = true
				return err
			}
			return runCoreList(cmd, "No invitations found.", invitationColumns, invitationRow, func(ctx context.Context, c *coreapi.Client) ([]coreapi.Invitation, error) {
				orgID, err := resolveInviteOrg(ctx, c, args[0])
				if err != nil {
					return nil, err
				}
				return listOrgInvitations(ctx, c, orgID, status)
			})
		},
	}
	cmd.Flags().StringVar(&status, "status", "open", "Lifecycle state to list: one of "+strings.Join(invitationStatuses, ", "))
	addJSONFlag(cmd)
	return cmd
}

func listOrgInvitations(ctx context.Context, c *coreapi.Client, orgID, status string) ([]coreapi.Invitation, error) {
	invitations, err := fetchAllPages(ctx, func(ctx context.Context, cursor string) ([]coreapi.Invitation, string, error) {
		params := coreapi.ListOrgInvitationsParams{
			OrgId:  orgID,
			Status: coreapi.NewOptListOrgInvitationsStatus(coreapi.ListOrgInvitationsStatus(status)),
		}
		if cursor != "" {
			params.PageToken = coreapi.NewOptString(cursor)
		}
		out, err := c.ListOrgInvitations(ctx, params)
		if err != nil {
			return nil, "", err
		}
		return out.Invitations, out.NextPageToken.Or(""), nil
	})
	if err != nil {
		return nil, err
	}
	// Same defense as the create path above, applied to every invitation the
	// listing returns.
	for i := range invitations {
		invitations[i].AdditionalProps = nil
	}
	return invitations, nil
}

func newOrgInviteRevokeCmd() *cobra.Command {
	var email string
	cmd := &cobra.Command{
		Use:     "revoke <org> --email <email>",
		Short:   "Revoke an organization invitation",
		Long:    "Revoke the open invitation for an email address so its link stops working. The org is addressed by name.",
		Example: "  entire org invite revoke acme --email dev@example.com",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
				orgID, err := resolveInviteOrg(ctx, c, args[0])
				if err != nil {
					return err
				}
				subject := "the invitation for " + email + " to org " + args[0]
				invitationID, err := openInvitationIDForEmail(ctx, c, orgID, email)
				if err != nil {
					return err
				}
				if invitationID == "" {
					// Same end state as revokeGrant's 404: nothing is open for
					// that address, so there is nothing to revoke.
					fmt.Fprintf(cmd.OutOrStdout(), "%s: no open invitation; nothing to revoke\n", subject)
					return nil
				}
				return revokeGrant(cmd, subject, func() error {
					return c.RevokeOrgInvitation(ctx, coreapi.RevokeOrgInvitationParams{OrgId: orgID, ID: invitationID})
				})
			})
		},
	}
	cmd.Flags().StringVar(&email, "email", "", "Email address whose open invitation to revoke")
	markRequired(cmd, "email")
	return cmd
}

// resolveInviteOrg resolves the <org> the invite commands take. Their help
// documents the org by name only, so a miss names only that way out rather
// than the shared "or pass a ULID" hint.
func resolveInviteOrg(ctx context.Context, c *coreapi.Client, name string) (string, error) {
	id, err := resolveOrgRef(ctx, c, name)
	if notFound := (*orgNotFoundError)(nil); errors.As(err, &notFound) {
		return "", fmt.Errorf("no org named %q (run `entire org list` to see org names)", name)
	}
	return id, err
}

// openInvitationIDForEmail finds the open invitation for an address, or ""
// when the org has none. The revoke route takes a ULID, so an email is resolved
// through the listing. Only open invitations are searched: re-revoking an
// accepted or revoked one is not what the user asked for. The server stores the
// address lowercased, so the match folds case.
func openInvitationIDForEmail(ctx context.Context, c *coreapi.Client, orgID, email string) (string, error) {
	invitations, err := listOrgInvitations(ctx, c, orgID, "open")
	if err != nil {
		return "", err
	}
	for _, inv := range invitations {
		if strings.EqualFold(inv.Email, email) {
			return inv.ID, nil
		}
	}
	return "", nil
}
