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

func newOrgInviteCmd() *cobra.Command {
	return newScopeInviteCmd(orgInviteScope, newOrgInviteSendCmd())
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
					return fmt.Sprintf("✓ Invited %s to org %s as %s (%s)", inv.Email.Or(email), args[0], inv.Role, inv.ID), &inv, nil
				case *coreapi.CreateOrgInvitationOK:
					// The role here is the stored one, which an earlier invite
					// chose; saying so stops a --role that did not take effect
					// from reading as though it had.
					inv := coreapi.Invitation(*out)
					inv.AdditionalProps = nil
					return fmt.Sprintf("✓ Resent the open invitation for %s to org %s, which invites as %s (%s)", inv.Email.Or(email), args[0], inv.Role, inv.ID), &inv, nil
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
