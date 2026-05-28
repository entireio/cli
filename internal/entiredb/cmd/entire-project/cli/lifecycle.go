package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/entiredb/core/api"
	"github.com/entireio/cli/internal/entiredb/internal/cliapi"
	"github.com/entireio/cli/internal/entiredb/internal/clicobra"
)

type clientFn func() (*api.Client, error)

// AttachProjectCmds attaches the project lifecycle verbs to root.
func AttachProjectCmds(root *cobra.Command, client clientFn) {
	root.AddCommand(newCreateProjectCmd(client))
	root.AddCommand(newListProjectsCmd(client))
}

func newCreateProjectCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a project",
		Long: "Creates a project named <name>. --owner picks the owning org " +
			"or account.\n\n" +
			"  - empty:                     project is owned by the logged-in account.\n" +
			"  - an org name:               --owner-type defaults to org; resolved to the org ULID.\n" +
			"  - an account handle:         pass --owner-type account; resolved via --owner-provider (default github).\n" +
			"  - a ULID:                    --owner-type {org|account} is REQUIRED to disambiguate.\n\n" +
			"--region defaults to the server's jurisdiction.",
		Example: "  # Personal project (owned by the logged-in account)\n" +
			"  entire-project create personal\n\n" +
			"  # Project under an org (by name)\n" +
			"  entire-project create widgets --owner acme\n\n" +
			"  # Project under another user's account (by GitHub handle)\n" +
			"  entire-project create widgets --owner alice --owner-type account\n\n" +
			"  # Project under an explicit ULID\n" +
			"  entire-project create widgets --owner 01J0... --owner-type org",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			owner := clicobra.MustGetString(cmd, "owner")
			ownerType := clicobra.MustGetString(cmd, "owner-type")
			ownerProvider := clicobra.MustGetString(cmd, "owner-provider")
			region := clicobra.MustGetString(cmd, "region")

			c, err := client()
			if err != nil {
				return err
			}
			ownerID, ownerType, err := resolveOwner(c, owner, ownerType, ownerProvider)
			if err != nil {
				return err
			}
			body := map[string]string{
				"name": args[0], "ownerType": ownerType, "ownerId": ownerID,
			}
			if region != "" {
				body["region"] = region
			}
			data, err := c.PostJSON("/api/v1/projects", body)
			if err != nil {
				return err
			}
			return clicobra.PrintJSON(data)
		},
	}
	cmd.Flags().String("owner", "", "org name, account handle, or ULID; empty owns the project as the logged-in account")
	cmd.Flags().String("owner-type", "", "org|account (required when --owner is a ULID; required when --owner is an account handle)")
	cmd.Flags().String("owner-provider", "github", "identity provider for account-handle resolution; ignored unless --owner-type=account with a non-ULID --owner")
	cmd.Flags().String("region", "", "project region (defaults to server jurisdiction)")
	return cmd
}

// resolveOwner turns --owner / --owner-type / --owner-provider into
// (owner_id, owner_type) for the API body. See the create command's
// Long doc for the precedence. provider is only consulted when --owner
// is an account handle.
func resolveOwner(c *api.Client, owner, ownerType, provider string) (id string, typ string, err error) {
	if owner == "" {
		accountID, err := cliapi.CurrentAccount(c)
		if err != nil {
			return "", "", fmt.Errorf("default --owner to current account: %w", err)
		}
		return accountID, "account", nil
	}
	if cliapi.LooksLikeULID(owner) {
		switch ownerType {
		case "org", "account":
			return owner, ownerType, nil
		case "":
			return "", "", errors.New("--owner is a ULID; pass --owner-type org or --owner-type account")
		default:
			return "", "", fmt.Errorf("--owner-type must be org or account, got %q", ownerType)
		}
	}
	// Name input — resolve based on --owner-type. Default to org for
	// backwards compatibility with the pre-handle-lookup CLI.
	switch ownerType {
	case "", "org":
		orgID, err := cliapi.ResolveOrgID(c, owner)
		if err != nil {
			return "", "", err
		}
		return orgID, "org", nil
	case "account":
		accountID, err := cliapi.ResolveAccountIDByHandle(c, provider, owner)
		if err != nil {
			return "", "", err
		}
		return accountID, "account", nil
	default:
		return "", "", fmt.Errorf("--owner-type must be org or account, got %q", ownerType)
	}
}

func newListProjectsCmd(client clientFn) *cobra.Command {
	return &cobra.Command{
		Use:   "list [<org>]",
		Short: "List projects (in an org, or all you can read)",
		Long: "Lists projects: with <org>, every project in that org " +
			"(by name or ULID); without an arg, every project you have " +
			"read on across orgs.",
		Example: "  # All projects you can read\n" +
			"  entire-project list\n\n" +
			"  # Projects in one org (by name)\n" +
			"  entire-project list acme",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			if len(args) == 1 {
				orgID, err := cliapi.ResolveOrgID(c, args[0])
				if err != nil {
					return err
				}
				data, err := c.GetJSON("/api/v1/orgs/" + orgID + "/projects")
				if err != nil {
					return err
				}
				return clicobra.PrintJSON(data)
			}
			// Cross-org path: SpiceDB returns ULIDs only, so chain a
			// /api/lookup call to enrich them with names + owner refs.
			return cliapi.ListAccessibleAndResolve(c, "project", "read")
		},
	}
}
