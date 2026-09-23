package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/coreapi"
)

// `entire org join <token>` accepts an invitation on behalf of whoever is
// logged in. It hangs off `entire org` rather than the `grant` subtree because
// it acts on the caller's own account: the caller is the invitee, not a manager
// addressing someone else.
//
// The token is a bearer credential: whoever holds it can join the org as the
// invitee. Nothing this command writes may contain it. The accept response's
// modeled fields carry no token, but generated response types round-trip any
// property the schema doesn't declare, so the success and --json paths blank
// that bag rather than assume the server never sends one. Two other paths
// could reintroduce the token: a flag parse error, which echoes the offending
// argument, and a server problem detail that quotes what it rejected. The
// FlagErrorFunc and redactToken below close those.

func newOrgJoinCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "join <token>",
		Short:   "Accept an organization invitation",
		Long:    "Accept an organization invitation using the token from the invitation link. The token is a credential: treat it like a password, and prefer a shell that does not record the command. Pass it after `--` if it starts with a dash.",
		Example: "  entire org join 01JBQ7X0000000000000000000",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			token := args[0]
			return runCoreMutation(cmd, func(ctx context.Context, c *coreapi.Client) (string, any, error) {
				res, err := c.AcceptInvitation(ctx, &coreapi.AcceptInvitationInputBody{Token: token})
				if err != nil {
					return "", nil, redactToken(err, token)
				}
				switch out := res.(type) {
				case *coreapi.AcceptedInvitation:
					// ogen round-trips any response property this schema doesn't
					// declare, verbatim, into --json output. The endpoint's
					// contract carries no token, but blank the bag rather than
					// trust that a future server response never adds one.
					out.AdditionalProps = nil
					out.Membership.AdditionalProps = nil
					return fmt.Sprintf("✓ Joined org %s as %s", out.OrgName, out.Membership.Role), out, nil
				case *coreapi.AcceptInvitationOK:
					// The server reports an already-active membership as 200
					// rather than an error, so accepting twice is a no-op.
					out.AdditionalProps = nil
					out.Membership.AdditionalProps = nil
					return fmt.Sprintf("Already a member of org %s as %s", out.OrgName, out.Membership.Role), out, nil
				default:
					return "", nil, fmt.Errorf("join: unexpected response %T from the control plane", res)
				}
			})
		},
	}
	// A token that starts with a dash parses as an unknown flag, and cobra's
	// message quotes it. Replace that message: the user learns the argument was
	// read as a flag without the credential reaching stderr or a CI log.
	cmd.SetFlagErrorFunc(func(*cobra.Command, error) error {
		return errors.New("could not parse the arguments to `entire org join`; if the token starts with a dash, pass it after `--`")
	})
	addJSONFlag(cmd)
	return cmd
}

// redactToken keeps an invitation token out of an error shown to the user. The
// control plane should not quote a credential back, but this enforces the
// guarantee here rather than assume it of the server. It renders the error
// first, because the rendered message is the string the token can hide in.
//
// The token is deleted outright rather than replaced with a placeholder like
// "<redacted>". The token schema requires only a non-empty string, so a
// caller can supply that placeholder text as their actual token; replacing a
// token with text equal to itself is a no-op, which would leave the token in
// the message despite looking redacted.
func redactToken(err error, token string) error {
	rendered := renderCoreError(err)
	if rendered == nil || token == "" {
		return rendered
	}
	msg := rendered.Error()
	if !strings.Contains(msg, token) {
		return rendered
	}
	return errors.New(strings.ReplaceAll(msg, token, ""))
}
