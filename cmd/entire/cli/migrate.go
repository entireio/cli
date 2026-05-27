package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
)

func newMigrateCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "migrate",
		Short:              "Deprecated",
		Hidden:             true,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			fmt.Fprintln(cmd.ErrOrStderr(), "The migrate command is deprecated.")
			return NewSilentError(errors.New("migrate command is deprecated"))
		},
	}
}
