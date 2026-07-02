package ticket

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// newRevokeTokenCmd builds `entire ticket revoke-token`.
func newRevokeTokenCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke-token",
		Short: "Remove the stored ticket-platform credential from this machine",
		Long: `Remove the API credential for the configured ticket platform from this
machine's credential store. The platform/team configuration is left in place;
run ` + "`entire ticket setup`" + ` to store a new credential (e.g. after rotating a key).

This only deletes the local copy. To fully invalidate a Linear personal key,
also revoke it in Linear (Settings → Security & access → API keys).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRevokeToken(cmd.Context(), cmd.OutOrStdout())
		},
	}
}

func runRevokeToken(ctx context.Context, out io.Writer) error {
	s, err := settings.Load(ctx)
	if err != nil {
		return fmt.Errorf("load settings: %w", err)
	}
	tc := s.TicketConfig()
	if tc.IsZero() {
		fmt.Fprintln(out, "No ticket platform configured.")
		return nil
	}
	platform, err := ParsePlatform(tc.Platform)
	if err != nil {
		return err
	}
	if err := DeleteToken(platform); err != nil {
		return err
	}
	fmt.Fprintf(out, "✓ Removed the stored %s credential from this machine.\n", platform.DisplayName())
	return nil
}
