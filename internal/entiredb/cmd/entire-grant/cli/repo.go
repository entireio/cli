package cli

import (
	"errors"
	"fmt"
	"net/url"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/entiredb/core/api"
	"github.com/entireio/cli/internal/entiredb/internal/cliapi"
	"github.com/entireio/cli/internal/entiredb/internal/cliauth"
	"github.com/entireio/cli/internal/entiredb/internal/clicobra"
	"github.com/entireio/cli/internal/entiredb/internal/clidial"
)

type clientFn func() (*api.Client, error)

// validRepoRoles is the set the server accepts for repo grants.
var validRepoRoles = map[string]struct{}{"admin": {}, "writer": {}, "reader": {}}

func newRepoCmd(cfg *cliauth.Config, client clientFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "repo",
		Short: "Grant, revoke, and list repo access",
		Long:  "<repo> accepts either <cluster>/et/<org>/<repo> (path form, resolved via the data plane) or a repo ULID.",
	}
	cmd.AddCommand(newRepoAddCmd(cfg, client))
	cmd.AddCommand(newRepoRemoveCmd(cfg, client))
	cmd.AddCommand(newRepoListCmd(cfg, client))
	return cmd
}

func newRepoAddCmd(cfg *cliauth.Config, client clientFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add <repo> --handle <handle> --role <role>",
		Short: "Grant access to a repo",
		Long: "Grants an account access to a repo. Identity resolution: " +
			"--handle resolves through --provider (default `github`), or " +
			"pass --provider-user-id directly. --role is one of admin, " +
			"writer, reader.",
		Example: "  entire-grant repo add aws-us-east-2.entire.io/et/alice/widgets --handle bob --role writer\n" +
			"  entire-grant repo add 01J0... --provider-user-id 12345 --role reader",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			provider := clicobra.MustGetString(cmd, "provider")
			handle := clicobra.MustGetString(cmd, "handle")
			providerUserID := clicobra.MustGetString(cmd, "provider-user-id")
			role := clicobra.MustGetString(cmd, "role")
			if _, ok := validRepoRoles[role]; !ok {
				return fmt.Errorf("--role must be one of admin, writer, reader (got %q)", role)
			}
			if handle == "" && providerUserID == "" {
				return errors.New("either --handle or --provider-user-id is required")
			}
			repoID, err := clidial.ResolveRepoID(*cfg, args[0])
			if err != nil {
				return err
			}
			c, err := client()
			if err != nil {
				return err
			}
			target, err := cliapi.ResolveTargetIdentity(c, provider, handle, providerUserID)
			if err != nil {
				return err
			}
			data, err := c.PostJSON("/api/v1/repos/"+url.PathEscape(repoID)+"/grants", map[string]string{
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
	cmd.Flags().String("role", "", "repo role: admin, writer, or reader")
	return cmd
}

func newRepoRemoveCmd(cfg *cliauth.Config, client clientFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove <repo> --handle <handle>",
		Short: "Revoke an account's access to a repo",
		Long: "Revokes an account's grant on <repo>. Identity resolution " +
			"mirrors `add`. Note: the server only registers POST on " +
			"/repos/{repoId}/grants today; this verb is shipped for shape " +
			"consistency across the grant tree and returns 404 (or 405) " +
			"until the corresponding huma operation lands.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			provider := clicobra.MustGetString(cmd, "provider")
			handle := clicobra.MustGetString(cmd, "handle")
			providerUserID := clicobra.MustGetString(cmd, "provider-user-id")
			if handle == "" && providerUserID == "" {
				return errors.New("either --handle or --provider-user-id is required")
			}
			repoID, err := clidial.ResolveRepoID(*cfg, args[0])
			if err != nil {
				return err
			}
			c, err := client()
			if err != nil {
				return err
			}
			target, err := cliapi.ResolveTargetIdentity(c, provider, handle, providerUserID)
			if err != nil {
				return err
			}
			// The DELETE endpoint isn't registered on /api/v1 yet
			// (core/coreapi/repos.go only declares POST). Calls 404 or
			// 405 until the matching huma operation lands; the
			// problem+json detail surfaces via core/api.Client's
			// errFromHTTPResponse.
			data, err := c.Delete("/api/v1/repos/" + url.PathEscape(repoID) +
				"/grants/account/" + url.PathEscape(target.ProviderUserID))
			if err != nil {
				return err
			}
			return clicobra.PrintJSON(data)
		},
	}
	cmd.Flags().String("provider", "github", "identity provider")
	cmd.Flags().String("handle", "", "handle to resolve to provider_user_id")
	cmd.Flags().String("provider-user-id", "", "stable provider user id (skips handle resolve)")
	return cmd
}

func newRepoListCmd(cfg *cliauth.Config, client clientFn) *cobra.Command {
	return &cobra.Command{
		Use:   "list <repo>",
		Short: "List grants on a repo",
		Long:  "Lists current grants on <repo>. Note: the GET endpoint isn't registered on /api/v1 yet (only POST is); the verb is shipped for shape consistency across the grant tree and returns 404 (or 405) until the matching huma operation lands.",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			repoID, err := clidial.ResolveRepoID(*cfg, args[0])
			if err != nil {
				return err
			}
			c, err := client()
			if err != nil {
				return err
			}
			// The GET endpoint isn't registered on /api/v1 yet. Calls
			// 404 or 405 until the matching huma operation lands; the
			// problem+json detail surfaces via core/api.Client's
			// errFromHTTPResponse.
			data, err := c.GetJSON("/api/v1/repos/" + url.PathEscape(repoID) + "/grants")
			if err != nil {
				return err
			}
			return clicobra.PrintJSON(data)
		},
	}
}
