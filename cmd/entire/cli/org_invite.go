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
	var email, account, role string
	cmd := &cobra.Command{
		Use:   "send <org> (--email <email> | --account <provider:handle>)",
		Short: "Invite an email address or an existing account to an organization",
		Long: "Invite someone to an organization. The org is addressed by name. " +
			"With --email, the address receives a link to accept; inviting an address that already has an open invitation sends the mail again and keeps the role the invitation was created with. " +
			"With --account (a provider-qualified handle, e.g. github:alice), the existing account is invited and has no access until it accepts; the command prints the invitation and when it expires. " +
			"Inviting an account that already has an open invitation with the same role reports that invitation without a second mail, and one with another role is refused until the open one is revoked. " +
			"An account that is already a member is refused. `entire org grant add` does the same as --account.",
		Example: "  entire org invite send acme --email dev@example.com --role admin\n" +
			"  entire org invite send acme --account github:alice",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateInviteSendFlags(cmd, email, account, role); err != nil {
				cmd.SilenceUsage = true
				return err
			}
			return runCoreMutation(cmd, func(ctx context.Context, c *coreapi.Client) (string, any, error) {
				orgID, err := resolveInviteOrg(ctx, c, args[0])
				if err != nil {
					return "", nil, err
				}
				if account != "" {
					return sendAccountInvitation(ctx, c, args[0], orgID, account, role)
				}
				return sendEmailInvitation(ctx, c, args[0], orgID, email, role)
			})
		},
	}
	// The wire field is required, so an omitted flag still sends a role: the
	// same default the API documents.
	cmd.Flags().StringVar(&email, "email", "", "Email address to invite")
	cmd.Flags().StringVar(&account, "account", "", "Existing account to invite, as a provider-qualified handle (e.g. github:alice)")
	cmd.Flags().StringVar(&role, "role", orgRoleMember, "Role the invitation grants: one of "+strings.Join(orgRoles, ", "))
	cmd.MarkFlagsMutuallyExclusive("email", "account")
	cmd.MarkFlagsOneRequired("email", "account")
	addJSONFlag(cmd)
	return cmd
}

// validateInviteSendFlags settles before any request what the command line
// alone decides: the role, and a value given to the wrong one of --email and
// --account. The two cannot be confused by the command, because each is its
// own flag, but a person can put a handle in --email or an address in
// --account; each is named back with the flag it belongs to.
func validateInviteSendFlags(cmd *cobra.Command, email, account, role string) error {
	if cmd.Flags().Changed("role") {
		if err := validateChoice("role", role, orgRoles); err != nil {
			return err
		}
	}
	if email != "" && !strings.Contains(email, "@") {
		if _, _, err := parseQualifiedHandle(email); err == nil {
			return fmt.Errorf("invalid --email %q: that is an account handle; invite it with --account %s", email, email)
		}
	}
	if account != "" {
		if strings.Contains(account, "@") && !strings.Contains(account, ":") {
			return fmt.Errorf("invalid --account %q: that is an email address; invite it with --email %s", account, account)
		}
		if err := ensureGranteeIsHandle(account); err != nil {
			return err
		}
	}
	return nil
}

// sendEmailInvitation creates, or resends, the invitation for an address.
func sendEmailInvitation(ctx context.Context, c *coreapi.Client, orgRef, orgID, email, role string) (string, any, error) {
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
		return fmt.Sprintf("✓ Invited %s to org %s as %s (%s)", inv.Email.Or(email), orgRef, inv.Role, inv.ID), &inv, nil
	case *coreapi.CreateOrgInvitationOK:
		// The role here is the stored one, which an earlier invite
		// chose; saying so stops a --role that did not take effect
		// from reading as though it had.
		inv := coreapi.Invitation(*out)
		inv.AdditionalProps = nil
		return fmt.Sprintf("✓ Resent the open invitation for %s to org %s, which invites as %s (%s)", inv.Email.Or(email), orgRef, inv.Role, inv.ID), &inv, nil
	default:
		return "", nil, fmt.Errorf("invite %s: unexpected response %T from the control plane", email, res)
	}
}

// sendAccountInvitation invites an existing account. It is the org add with
// the invitation allowed — the call `org grant add` makes — so the outcome
// and its wording are grant add's: Core answers 202 with the invitation, or,
// before it requires consent, 201 with the membership it wrote, which is
// reported as the grant it is.
func sendAccountInvitation(ctx context.Context, c *coreapi.Client, orgRef, orgID, account, role string) (string, any, error) {
	pt := grantPickerTarget{noun: cmdOrg, ref: orgRef}
	provider, providerUserID, err := resolveGranteeProvider(ctx, c, account)
	if err != nil {
		return "", nil, err
	}
	outcome, err := orgGrantTarget.grant(ctx, c, orgID, provider, providerUserID, role)
	if err != nil {
		return "", nil, withRoleMismatchHint(orgGrantTarget, pt, err)
	}
	return grantOutcomeLine(account, pt, outcome), outcome.wire, nil
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
