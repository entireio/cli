package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/coreapi"
)

// The invitation verbs. `entire org grant invite|invites|uninvite` sit beside
// add/list/remove because an invitation is how an org grants membership to
// someone the control plane cannot name yet: `grant add` needs an existing
// provider account, an invitation needs only an email address. `entire org
// join` is the invitee's half and hangs off `entire org` instead, because it
// acts on the caller's own account rather than on an org they manage.
//
// Who may invite with which role is the server's decision: it answers 403 for
// a role the caller cannot delegate. The CLI checks only that --role spells one
// of the values the API declares, so there is one place where "an admin may not
// mint owners" is decided.

// invitationStatuses are the --status filter values. "all" is the API's own
// name for "every state", not a client-side wildcard.
var invitationStatuses = []string{"open", "accepted", "revoked", "expired", "all"}

// invitationColumns omits the invitation ULID, which only --json carries;
// `uninvite` takes the email address instead.
var invitationColumns = []string{"EMAIL", colHeaderRole, colHeaderStatus, "EXPIRES"}

func invitationRow(i coreapi.Invitation) []string {
	return []string{i.Email, i.Role, i.Status, i.ExpiresAt.Format("2006-01-02")}
}

func newOrgInviteCmd() *cobra.Command {
	var role string
	cmd := &cobra.Command{
		Use:     "invite <org> <email>",
		Short:   "Invite an email address to an organization",
		Long:    "Invite an email address to an organization. The org is addressed by name or ULID. The invited address receives a link; the invitee runs `entire org join` to accept. Inviting an address that already has an open invitation sends the mail again and keeps the role the invitation was created with.",
		Example: "  entire org grant invite acme dev@example.com --role admin",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("role") {
				if err := validateChoice("role", role, orgRoles); err != nil {
					cmd.SilenceUsage = true
					return err
				}
			}
			return runCoreMutation(cmd, func(ctx context.Context, c *coreapi.Client) (string, any, error) {
				orgID, err := resolveOrgRef(ctx, c, args[0])
				if err != nil {
					return "", nil, err
				}
				body := &coreapi.CreateOrgInvitationInputBody{
					Email: args[1],
					Role:  coreapi.CreateOrgInvitationInputBodyRole(role),
				}
				res, err := c.CreateOrgInvitation(ctx, body, coreapi.CreateOrgInvitationParams{OrgId: orgID})
				if err != nil {
					return "", nil, err
				}
				switch out := res.(type) {
				case *coreapi.CreateOrgInvitationCreated:
					// An invitation is the one object with an accept token (see
					// org_join.go). Its modeled fields carry none today, but ogen
					// round-trips any response property this schema doesn't
					// declare, verbatim, into --json output, so blank the bag
					// rather than trust the endpoint's contract never grows one.
					inv := coreapi.Invitation(*out)
					inv.AdditionalProps = nil
					return fmt.Sprintf("✓ Invited %s to org %s as %s", inv.Email, args[0], inv.Role), &inv, nil
				case *coreapi.CreateOrgInvitationOK:
					// The role here is the stored one, which an earlier invite
					// chose; saying so stops a --role that did not take effect
					// from reading as though it had.
					inv := coreapi.Invitation(*out)
					inv.AdditionalProps = nil
					return fmt.Sprintf("✓ Resent the open invitation for %s to org %s, which invites as %s", inv.Email, args[0], inv.Role), &inv, nil
				default:
					return "", nil, fmt.Errorf("invite %s: unexpected response %T from the control plane", args[1], res)
				}
			})
		},
	}
	// The wire field is required, so an omitted flag still sends a role: the
	// same default the API documents.
	cmd.Flags().StringVar(&role, "role", roleMember, "Role the invitation grants: one of "+strings.Join(orgRoles, ", "))
	addJSONFlag(cmd)
	return cmd
}

func newOrgInvitesCmd() *cobra.Command {
	var status string
	cmd := &cobra.Command{
		Use:     "invites <org>",
		Short:   "List an organization's invitations",
		Long:    "List an organization's invitations, open ones by default. The org is addressed by name or ULID.",
		Example: "  entire org grant invites acme --status all",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateChoice("status", status, invitationStatuses); err != nil {
				cmd.SilenceUsage = true
				return err
			}
			return runCoreList(cmd, "No invitations found.", invitationColumns, invitationRow, func(ctx context.Context, c *coreapi.Client) ([]coreapi.Invitation, error) {
				orgID, err := resolveOrgRef(ctx, c, args[0])
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

func newOrgUninviteCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "uninvite <org> <email|id>",
		Short:   "Revoke an organization invitation",
		Long:    "Revoke an open invitation so its link stops working. The org is addressed by name or ULID; the invitation by the invited email address or its own ULID.",
		Example: "  entire org grant uninvite acme dev@example.com",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
				orgID, err := resolveOrgRef(ctx, c, args[0])
				if err != nil {
					return err
				}
				subject := "the invitation for " + args[1] + " to org " + args[0]
				invitationID := args[1]
				if !looksLikeULID(invitationID) {
					invitationID, err = openInvitationIDForEmail(ctx, c, orgID, args[1])
					if err != nil {
						return err
					}
					if invitationID == "" {
						// Same end state as revokeGrant's 404: nothing is open
						// for that address, so there is nothing to revoke.
						fmt.Fprintf(cmd.OutOrStdout(), "%s: no open invitation; nothing to revoke\n", subject)
						return nil
					}
				}
				return revokeGrant(cmd, subject, func() error {
					return c.RevokeOrgInvitation(ctx, coreapi.RevokeOrgInvitationParams{OrgId: orgID, ID: invitationID})
				})
			})
		},
	}
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
