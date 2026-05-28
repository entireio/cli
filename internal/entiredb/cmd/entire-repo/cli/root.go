package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/entiredb/core/api"
	"github.com/entireio/cli/internal/entiredb/internal/cliapi"
	"github.com/entireio/cli/internal/entiredb/internal/cliauth"
	"github.com/entireio/cli/internal/entiredb/internal/clidocs"
	"github.com/entireio/cli/internal/entiredb/slogutil"
)

// NewRootCmd builds the entire-repo cobra command tree.
//
// entire-repo consolidates all user-facing repo verbs in one binary:
// lifecycle (create/delete/list/grant), content (clone/log/merge/...),
// and mirror placement.
//
// Data-plane verbs take the target cluster as part of the positional
// (<cluster>/et/<org>/<repo>); there is no global --cluster flag. Lifecycle
// verbs talk to entire-core and accept --context / --core-url.
func NewRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "entire-repo",
		Short:         "Manage Entire repositories",
		Long:          `entire-repo is the consolidated CLI for every repo verb: create, delete, grant, clone, log, merge, rebase, mirror.`,
		SilenceErrors: true,
		SilenceUsage:  true,
		PersistentPreRun: func(_ *cobra.Command, _ []string) {
			slogutil.InstallStdoutDefault("entire-repo", slogutil.CLILogLevel())
		},
	}
	cfg, err := cliauth.LoadConfig()
	cobra.CheckErr(err)

	var contextOverride, coreURLOverride string
	cmd.PersistentFlags().StringVar(&contextOverride, "context", "",
		"override the active login context for this invocation")
	cmd.PersistentFlags().StringVar(&coreURLOverride, "core-url", "",
		"override the entire-core endpoint (used by lifecycle verbs: create, delete, list, grant)")

	client := func() (*api.Client, error) {
		return cliapi.Client(cfg, coreURLOverride, contextOverride)
	}

	AttachContentCmds(cmd, &cfg)
	AttachLifecycleCmds(cmd, &cfg, client)
	cmd.AddCommand(NewMirrorCmd(&cfg))
	cmd.AddCommand(clidocs.NewCmd())
	return cmd
}

// usageArgs wraps a cobra positional-args validator so that validation errors
// include the command's usage line. SilenceUsage on the root otherwise drops
// the usage hint on arg-count errors.
func usageArgs(v cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := v(cmd, args); err != nil {
			return fmt.Errorf("%w\nUsage: %s", err, cmd.UseLine())
		}
		return nil
	}
}
