package cli

import (
	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/entiredb/core/api"
	"github.com/entireio/cli/internal/entiredb/internal/clicobra"
)

type clientFn func() (*api.Client, error)

// AttachOrgCmds attaches the org lifecycle verbs to root.
func AttachOrgCmds(root *cobra.Command, client clientFn) {
	root.AddCommand(newCreateOrgCmd(client))
	root.AddCommand(newListOrgsCmd(client))
}

func newCreateOrgCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create an org",
		Long:  "Creates an org named <name>. --region defaults to the server's jurisdiction.",
		Example: "  # Org in the server's region\n" +
			"  entire-org create acme\n\n" +
			"  # Pin a region explicitly\n" +
			"  entire-org create acme-eu --region eu",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body := map[string]string{"name": args[0]}
			if region := clicobra.MustGetString(cmd, "region"); region != "" {
				body["region"] = region
			}
			c, err := client()
			if err != nil {
				return err
			}
			data, err := c.PostJSON("/api/v1/orgs", body)
			if err != nil {
				return err
			}
			return clicobra.PrintJSON(data)
		},
	}
	cmd.Flags().String("region", "", "org region (defaults to server jurisdiction)")
	return cmd
}

func newListOrgsCmd(client clientFn) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Short:   "List your orgs",
		Long:    "Lists orgs you belong to.",
		Example: "  entire-org list",
		Args:    cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			data, err := c.GetJSON("/api/v1/orgs")
			if err != nil {
				return err
			}
			return clicobra.PrintJSON(data)
		},
	}
}
