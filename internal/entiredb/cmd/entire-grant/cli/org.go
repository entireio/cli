package cli

import (
	"errors"
	"fmt"
	"net/url"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/entiredb/internal/cliapi"
	"github.com/entireio/cli/internal/entiredb/internal/clicobra"
)

// validOrgRoles is the set the server accepts for org membership. Check
// client-side so typos error locally instead of round-tripping to SpiceDB.
var validOrgRoles = map[string]struct{}{"member": {}, "admin": {}, "owner": {}}

func newOrgCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "org",
		Short: "Grant, revoke, and list org membership",
	}
	cmd.AddCommand(newOrgAddCmd(client))
	cmd.AddCommand(newOrgRemoveCmd(client))
	cmd.AddCommand(newOrgListCmd(client))
	return cmd
}

func newOrgAddCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add <org> --handle <handle> --role <role>",
		Short: "Add a member to an org",
		Long: "Adds a member to <org> (name or ULID). Identity resolution: " +
			"--handle resolves through --provider (default `github`), or " +
			"pass --provider-user-id directly. --role is one of member, " +
			"admin, owner.",
		Example: "  entire-grant org add acme --handle alice --role member\n" +
			"  entire-grant org add 01J0... --provider-user-id 12345 --role admin",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			provider := clicobra.MustGetString(cmd, "provider")
			handle := clicobra.MustGetString(cmd, "handle")
			providerUserID := clicobra.MustGetString(cmd, "provider-user-id")
			role := clicobra.MustGetString(cmd, "role")
			if _, ok := validOrgRoles[role]; !ok {
				return fmt.Errorf("--role must be one of member, admin, owner (got %q)", role)
			}
			if handle == "" && providerUserID == "" {
				return errors.New("either --handle or --provider-user-id is required")
			}
			c, err := client()
			if err != nil {
				return err
			}
			orgID, err := cliapi.ResolveOrgID(c, args[0])
			if err != nil {
				return err
			}
			target, err := cliapi.ResolveTargetIdentity(c, provider, handle, providerUserID)
			if err != nil {
				return err
			}
			data, err := c.PostJSON("/api/v1/orgs/"+url.PathEscape(orgID)+"/members", map[string]string{
				"provider":       target.Provider,
				"providerUserId": target.ProviderUserID,
				"role":           role,
			})
			if err != nil {
				return err
			}
			return clicobra.PrintJSON(data)
		},
	}
	cmd.Flags().String("provider", "github", "identity provider")
	cmd.Flags().String("handle", "", "handle to resolve to provider_user_id")
	cmd.Flags().String("provider-user-id", "", "stable provider user id (skips handle resolve)")
	cmd.Flags().String("role", "", "org role: member, admin, or owner")
	return cmd
}

func newOrgRemoveCmd(client clientFn) *cobra.Command {
	return newProviderIdentityRemoveCmd(client, providerIdentityRemoveSpec{
		Use:               "remove <org> --handle <handle>",
		Short:             "Remove a member from an org",
		Long:              "Removes a member from <org> (name or ULID). Identity resolution mirrors `add` (--handle or --provider-user-id).",
		Example:           "  entire-grant org remove acme --handle alice",
		ResolveResourceID: cliapi.ResolveOrgID,
		DeletePath: func(orgID string, target *cliapi.TargetIdentity) string {
			return "/api/v1/orgs/" + url.PathEscape(orgID) +
				"/members/" + url.PathEscape(target.Provider) +
				"/" + url.PathEscape(target.ProviderUserID)
		},
	})
}

func newOrgListCmd(client clientFn) *cobra.Command {
	return &cobra.Command{
		Use:     "list <org>",
		Short:   "List members of an org",
		Long:    "Lists members of <org> (name or ULID).",
		Example: "  entire-grant org list acme",
		Args:    cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			orgID, err := cliapi.ResolveOrgID(c, args[0])
			if err != nil {
				return err
			}
			data, err := c.GetJSON("/api/v1/orgs/" + url.PathEscape(orgID) + "/members")
			if err != nil {
				return err
			}
			return clicobra.PrintJSON(data)
		},
	}
}
