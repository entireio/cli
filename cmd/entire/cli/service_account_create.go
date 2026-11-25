package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/coreapi"
)

func newSACreateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create <project> <name>",
		Short: "Create a service account in a project",
		Long: "Creates a service account owned by the project. You need manage " +
			"access to the project. Names are unique within a project, and each " +
			"project owner has a service account limit.",
		Example: "  entire service-account create acme ci",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			project, name := args[0], strings.TrimSpace(args[1])
			if name == "" {
				cmd.SilenceUsage = true
				return errors.New("service account name is empty")
			}
			return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
				proj, err := resolveProjectRefResolved(ctx, c, project)
				if err != nil {
					return err
				}
				if proj.Name != "" {
					project = proj.Name
				}
				sa, err := c.CreateServiceAccount(ctx, &coreapi.CreateServiceAccountInputBody{ProjectID: proj.ID, Name: name})
				if err != nil {
					return saCreateErr(err, project, name)
				}
				if jsonRequested(cmd) {
					return printJSON(cmd.OutOrStdout(), sa)
				}
				out := cmd.OutOrStdout()
				fmt.Fprintf(out, "✓ Created service account %s/%s (%s)\n", project, sa.Name, sa.AccountId)
				fmt.Fprintf(out, "Next: entire service-account grant %s --repo <repo> --role writer\n", sa.AccountId)
				return nil
			})
		},
	}
	addJSONFlag(cmd)
	return cmd
}

// saCreateErr names the project in core's refusals.
func saCreateErr(err error, project, name string) error {
	switch coreapi.APIErrorType(err) {
	case saProblemNameInUse:
		return fmt.Errorf("service account %s/%s already exists", project, name)
	case saProblemQuota:
		return fmt.Errorf("owner of project %s reached its service account limit", project)
	}
	// Core answers 403 for missing projects.
	if coreapi.APIErrorStatus(err) == http.StatusForbidden {
		return fmt.Errorf("you need manage access to project %s", project)
	}
	return err
}
