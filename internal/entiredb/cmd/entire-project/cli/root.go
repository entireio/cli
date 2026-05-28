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

// NewRootCmd builds the entire-project cobra command tree.
//
// Global flags:
//   - --context <name>     pick a stored login context
//   - --core-url <url>     override the entire-core endpoint
func NewRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "entire-project",
		Short:         "Manage Entire projects",
		Long:          `entire-project creates, lists, and inspects projects.`,
		SilenceErrors: true,
		SilenceUsage:  true,
		PersistentPreRun: func(_ *cobra.Command, _ []string) {
			slogutil.InstallStdoutDefault("entire-project", slog.LevelInfo)
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

	AttachProjectCmds(cmd, client)
	cmd.AddCommand(clidocs.NewCmd())
	return cmd
}
