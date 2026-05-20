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
			fmt.Fprintln(cmd.ErrOrStderr(), "Migration to checkpoints v2 has been halted for now.")
			return NewSilentError(errors.New("migration to checkpoints v2 has been halted"))
		},
	}
}
