package cli

import (
	"context"
	"log/slog"
	"os"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/pricing"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/telemetry"
	"github.com/spf13/cobra"
)

// refreshPricingCmdName is the hidden worker subcommand the detached spawner
// invokes to run the daily remote-pricing refresh out of the foreground path.
const refreshPricingCmdName = "__refresh_pricing"

// detachedSpawn is the detached spawner used for the pricing-refresh worker,
// held as a package var so tests can capture the working directory the worker is
// asked to run in without spawning a real process.
var detachedSpawn = telemetry.SpawnDetached

// spawnDetachedRefresh starts the hidden pricing-refresh worker as a detached
// background process rooted at the current working directory — the project the
// hook ran in — so the worker's own IsRemoteEnabled check resolves the project's
// .entire/settings.json. Running at "/" (the analytics default) would hide a
// project-only pricing.remote opt-in and silently no-op the refresh forever.
// It is a package var so tests can replace it to assert the turn-end / post-run
// triggers only ever *spawn* — they never fetch inline.
var spawnDetachedRefresh = func() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	// Root the worker at the process working directory so its own settings load
	// resolves the project's .entire/settings.json. os.Getwd (not paths.RepoRoot)
	// is deliberate: this captures the literal cwd to hand a detached child, not a
	// git-relative path — the worker re-resolves the repo via git from there. An
	// empty dir (Getwd failure) falls back to the platform default in SpawnDetached.
	dir, err := os.Getwd() //nolint:forbidigo // hand cwd to a detached child; not a git-relative path
	if err != nil {
		dir = ""
	}
	//nolint:errcheck // Best effort background refresh - a failed spawn is non-fatal
	_ = detachedSpawn(dir, exe, refreshPricingCmdName)
}

// maybeSpawnPricingRefresh spawns the detached remote-pricing refresh worker
// when remote pricing merging is enabled (opt-in) and the local cache is stale
// (>24h) or absent. It never blocks and never performs the network fetch inline
// — the fetch happens in the detached __refresh_pricing worker. Safe to call
// from any command's post-run and from the turn-end hook; the cheap
// ShouldRefresh stat means the common (fresh or disabled) case does no work.
func maybeSpawnPricingRefresh(ctx context.Context) {
	if !settings.IsRemoteEnabled(ctx) {
		return
	}
	if !pricing.ShouldRefresh() {
		return
	}
	spawnDetachedRefresh()
}

// newRefreshPricingCmd builds the hidden worker the detached spawner invokes. It
// runs the actual network refresh, gated on the pricing.remote opt-in. Being
// Hidden, the PersistentPostRun parent-chain guard skips it, so it can never
// itself trigger another refresh spawn (no fork bomb) and never runs telemetry
// or the version check.
func newRefreshPricingCmd() *cobra.Command {
	return &cobra.Command{
		Use:    refreshPricingCmdName,
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if !settings.IsRemoteEnabled(ctx) {
				return nil
			}
			if err := pricing.RefreshRemoteCache(ctx); err != nil {
				logging.Debug(ctx, "pricing: remote refresh failed",
					slog.String("error", err.Error()))
			}
			return nil
		},
	}
}
