package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/pricing"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/spf13/cobra"
)

// pricingRefreshReport is the --json shape of the pricing-refresh outcome.
type pricingRefreshReport struct {
	SourceURL     string    `json:"source_url"`
	Outcome       string    `json:"outcome"`
	Entries       int       `json:"entries"`
	FetchedAt     time.Time `json:"fetched_at"`
	StalenessSecs int       `json:"staleness_seconds"`
	RemoteEnabled bool      `json:"remote_enabled"`
}

func newTokensPricingRefreshCmd() *cobra.Command {
	var jsonFlag bool
	cmd := &cobra.Command{
		Use:   "pricing-refresh",
		Short: "Force-refresh the cached remote pricing table",
		Long: `Force-refresh the locally cached remote pricing table.

Fetches the remote pricing table now, bypassing the daily throttle, and reports
the outcome. This refreshes the cache regardless of settings so you can inspect
what would be merged; remote rates are only merged into cost estimates when the
"pricing.remote" setting is enabled.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runTokensPricingRefresh(cmd.Context(), cmd, jsonFlag)
		},
	}
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "Output as JSON")
	return cmd
}

func runTokensPricingRefresh(ctx context.Context, cmd *cobra.Command, jsonOutput bool) error {
	result, err := pricing.RefreshRemoteCacheForce(ctx)
	if err != nil {
		return fmt.Errorf("refresh remote pricing cache: %w", err)
	}
	enabled := settings.IsRemoteEnabled(ctx)

	if jsonOutput {
		return printJSON(cmd.OutOrStdout(), pricingRefreshReport{
			SourceURL:     result.SourceURL,
			Outcome:       result.Outcome,
			Entries:       result.Entries,
			FetchedAt:     result.FetchedAt,
			StalenessSecs: int(result.Staleness().Seconds()),
			RemoteEnabled: enabled,
		})
	}

	w := cmd.OutOrStdout()
	fmt.Fprintln(w, "Remote pricing refresh")
	fmt.Fprintf(w, "Source:       %s\n", result.SourceURL)
	fmt.Fprintf(w, "Outcome:      %s\n", result.Outcome)
	fmt.Fprintf(w, "Entries:      %d\n", result.Entries)
	if result.FetchedAt.IsZero() {
		fmt.Fprintln(w, "Fetched at:   never")
	} else {
		fmt.Fprintf(w, "Fetched at:   %s (%s ago)\n",
			result.FetchedAt.UTC().Format(time.RFC3339),
			result.Staleness().Round(time.Second))
	}
	if enabled {
		fmt.Fprintln(w, "Remote merge: enabled")
	} else {
		fmt.Fprintln(w, `Remote merge: disabled — set "pricing": {"remote": true} in .entire/settings.json to merge remote rates into cost estimates`)
	}
	return nil
}
