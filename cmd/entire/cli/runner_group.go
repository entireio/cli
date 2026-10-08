package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newRunnerCmd is the root of the `entire runner` group, which manages the
// trail runner configs under .entire/runners/. Hidden during maturation, like
// the related `trail` group.
func newRunnerCmd() *cobra.Command {
	var insecureHTTPAuth bool

	cmd := &cobra.Command{
		Use:    "runner",
		Short:  "Set up and tune trail runners for this repository",
		Hidden: true,
		Args:   cobra.NoArgs,
		Long: `Manage the trail runner configs in .entire/runners/.

Runners are the per-repo evaluators (risk, confidence, drift, security, review,
…) that score and review a branch's changes. Use ` + "`entire runner setup`" + ` to
create the default set in a repo that has none, and to tailor the runner
prompts to this repository.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	cmd.PersistentFlags().BoolVar(&insecureHTTPAuth, "insecure-http-auth", false,
		"Allow API calls over plain HTTP (insecure, for local development only)")
	if err := cmd.PersistentFlags().MarkHidden("insecure-http-auth"); err != nil {
		panic(fmt.Sprintf("hide insecure-http-auth flag: %v", err))
	}

	cmd.AddCommand(newRunnerSetupCmd())

	return cmd
}

// runnerInsecureHTTP reads the persistent --insecure-http-auth flag from the
// runner root command.
func runnerInsecureHTTP(cmd *cobra.Command) bool {
	v, _ := cmd.Flags().GetBool("insecure-http-auth") //nolint:errcheck // flag is always registered
	return v
}
