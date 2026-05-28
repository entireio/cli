package cli

import (
	"errors"
	"fmt"
	"net/url"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/entiredb/internal/cliapi"
	"github.com/entireio/cli/internal/entiredb/internal/clicobra"
)

// validProjectRoles is the set the server accepts for project grants.
var validProjectRoles = map[string]struct{}{"admin": {}, "writer": {}, "reader": {}}

func newProjectCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "project",
		Short: "Grant, revoke, and list project access",
		Long:  "<project> is a project name or ULID.",
	}
	cmd.AddCommand(newProjectAddCmd(client))
	cmd.AddCommand(newProjectRemoveCmd(client))
	cmd.AddCommand(newProjectListCmd(client))
	return cmd
}

func newProjectAddCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add <project> --handle <handle> --role <role>",
		Short: "Grant access to a project",
		Long: "Grants an account access to <project> (project name or ULID). Identity resolution: " +
			"--handle resolves through --provider (default `github`), or " +
			"pass --provider-user-id directly. --role is one of admin, " +
			"writer, reader.",
		Example: "  entire-grant project add widgets --handle alice --role writer",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			provider := clicobra.MustGetString(cmd, "provider")
			handle := clicobra.MustGetString(cmd, "handle")
			providerUserID := clicobra.MustGetString(cmd, "provider-user-id")
			role := clicobra.MustGetString(cmd, "role")
			if _, ok := validProjectRoles[role]; !ok {
				return fmt.Errorf("--role must be one of admin, writer, reader (got %q)", role)
			}
			if handle == "" && providerUserID == "" {
				return errors.New("either --handle or --provider-user-id is required")
			}
			c, err := client()
			if err != nil {
				return err
			}
			projectID, err := cliapi.ResolveProjectID(c, args[0])
			if err != nil {
				return err
			}
			target, err := cliapi.ResolveTargetIdentity(c, provider, handle, providerUserID)
			if err != nil {
				return err
			}
			data, err := c.PostJSON("/api/v1/projects/"+url.PathEscape(projectID)+"/grants", map[string]string{
				"provider":       target.Provider,
				"providerUserId": target.ProviderUserID,
				"granteeType":    "account",
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
	cmd.Flags().String("role", "", "project role: admin, writer, or reader")
	return cmd
}

func newProjectRemoveCmd(client clientFn) *cobra.Command {
	return newProviderIdentityRemoveCmd(client, providerIdentityRemoveSpec{
		Use:               "remove <project> --handle <handle>",
		Short:             "Revoke an account's access to a project",
		Long:              "Revokes an account's grant on <project> (project name or ULID). Identity resolution mirrors `add` (--handle or --provider-user-id).",
		Example:           "  entire-grant project remove widgets --handle alice",
		ResolveResourceID: cliapi.ResolveProjectID,
		DeletePath: func(projectID string, target *cliapi.TargetIdentity) string {
			return "/api/v1/projects/" + url.PathEscape(projectID) +
				"/grants/account/" + url.PathEscape(target.Provider) +
				"/" + url.PathEscape(target.ProviderUserID)
		},
	})
}

func newProjectListCmd(client clientFn) *cobra.Command {
	return &cobra.Command{
		Use:   "list <project>",
		Short: "List members of a project",
		Long:  "Lists current members of <project> (project name or ULID).",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			projectID, err := cliapi.ResolveProjectID(c, args[0])
			if err != nil {
				return err
			}
			data, err := c.GetJSON("/api/v1/projects/" + url.PathEscape(projectID) + "/members")
			if err != nil {
				return err
			}
			return clicobra.PrintJSON(data)
		},
	}
}
