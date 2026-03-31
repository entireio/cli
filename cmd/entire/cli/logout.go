package cli

import (
	"fmt"
	"io"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/spf13/cobra"
)

// tokenDeleter abstracts token removal so runLogout can be unit-tested
// without hitting the real OS keyring.
type tokenDeleter interface {
	DeleteToken(baseURL string) error
}

func newLogoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Log out of Entire",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLogout(cmd.OutOrStdout(), auth.NewStore(), api.BaseURL())
		},
	}
}

func runLogout(outW io.Writer, store tokenDeleter, baseURL string) error {
	if err := store.DeleteToken(baseURL); err != nil {
		return fmt.Errorf("remove auth token: %w", err)
	}

	fmt.Fprintln(outW, "Logged out.")
	return nil
}
