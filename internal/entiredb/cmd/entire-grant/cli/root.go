package cli

import (
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/entiredb/core/api"
	"github.com/entireio/cli/internal/entiredb/internal/cliapi"
	"github.com/entireio/cli/internal/entiredb/internal/cliauth"
	"github.com/entireio/cli/internal/entiredb/internal/clidocs"
	"github.com/entireio/cli/internal/entiredb/slogutil"
)

// NewRootCmd builds the entire-grant cobra command tree.
//
// Global flags:
//   - --context <name>     pick a stored login context
//   - --core-url <url>     override the entire-core endpoint
//
// The `repo` subgroup resolves data-plane paths via the cluster embedded in
// the <cluster>/et/<org>/<repo> positional; there is no --cluster flag.
func NewRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "entire-grant",
		Short:         "Grant access to orgs, projects, and repos",
		Long:          `entire-grant manages org membership and project / repo grants under one binary.`,
		SilenceErrors: true,
		SilenceUsage:  true,
		PersistentPreRun: func(_ *cobra.Command, _ []string) {
			slogutil.InstallStdoutDefault("entire-grant", slog.LevelInfo)
		},
	}
	cfg, err := cliauth.LoadConfig()
	cobra.CheckErr(err)

	var contextOverride, coreURLOverride string
	cmd.PersistentFlags().StringVar(&contextOverride, "context", "",
		"override the active login context for this invocation")
	cmd.PersistentFlags().StringVar(&coreURLOverride, "core-url", "",
		"override the entire-core endpoint")

	client := func() (*api.Client, error) {
		return cliapi.Client(cfg, coreURLOverride, contextOverride)
	}

	cmd.AddCommand(newRepoCmd(&cfg, client))
	cmd.AddCommand(newProjectCmd(client))
	cmd.AddCommand(newOrgCmd(client))
	cmd.AddCommand(clidocs.NewCmd())
	return cmd
}
