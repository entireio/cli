package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// globalWarnMarkerName marks that the current generation of "global tracking
// enabled" has already been announced. Generations are observational — the
// settings file carries no counter and hand-edits bypass every writer — so
// each foreground command that observes the tier disabled/unconfigured
// deletes the marker, and the next observed-enabled command warns again. An
// off→on flip with no intervening foreground command is indistinguishable
// from continuous-on and deliberately does not re-warn.
const globalWarnMarkerName = "global_warn_ack"

func globalWarnMarkerPath() string {
	return filepath.Join(userdirs.Config(), globalWarnMarkerName)
}

// maybeWarnGlobalTracking is the foreground detection warn: it runs from the
// root PersistentPostRun (whose hidden-parent-chain walk already excludes
// hooks and infrastructure commands) and writes to stderr, like the version
// check. Unreadable settings stay silent — doctor is that failure's surface,
// and a warn here would fire on every command forever.
func maybeWarnGlobalTracking(ctx context.Context, errW io.Writer) {
	us, err := settings.LoadUserSettings(ctx)
	if err != nil {
		return
	}
	_, statErr := os.Stat(globalWarnMarkerPath())
	markerPresent := statErr == nil
	if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
		// Unexpected (permissions, I/O): treated as marker-absent, which can
		// only over-warn, never suppress — but leave a trace for diagnosis.
		logging.Debug(ctx, "global warn marker unreadable; treating as absent", slog.String("error", statErr.Error()))
	}
	switch {
	case us.GlobalEnabled() && !markerPresent:
		fmt.Fprintln(errW, globalTrackingWarnText(us))
		ackGlobalWarnMarker(ctx)
	case !us.GlobalEnabled() && markerPresent:
		// Symmetric off-detection: a hand-edited disable bypasses
		// `entire disable --global`, so this is where its held-data
		// one-liner gets delivered.
		if err := os.Remove(globalWarnMarkerPath()); err != nil {
			return // marker survived; retry (and print) on a later command
		}
		fmt.Fprintln(errW, "Global tracking is off; locally captured checkpoints in untrusted repos will not sync.")
	}
}

// ackGlobalWarnMarker records that the current enabled generation has been
// announced. Called by the detection warn above AND by enable --global, whose
// own confirmation IS the announcement — without the ack, the very command
// that enabled the tier would get the detection warn stacked on top of its
// confirmation. Best-effort: a failed write only re-warns on a later command,
// and must not suppress the announcement that already printed.
func ackGlobalWarnMarker(ctx context.Context) {
	if err := os.MkdirAll(userdirs.Config(), 0o700); err != nil {
		logging.Debug(ctx, "global warn marker not written", slog.String("error", err.Error()))
		return
	}
	if err := os.WriteFile(globalWarnMarkerPath(), nil, 0o600); err != nil {
		logging.Debug(ctx, "global warn marker not written", slog.String("error", err.Error()))
	}
}

// retireGlobalWarnMarker ends the announced generation without printing.
// Called by disable --global, whose own held-data line replaces the off-note
// — leaving the marker behind would make the next foreground command print
// that note a second time. (The off-detection above removes the marker
// inline instead, because its note must be deferred when removal fails.)
// Best-effort: a missing marker is already the desired state.
func retireGlobalWarnMarker(ctx context.Context) {
	if err := os.Remove(globalWarnMarkerPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		logging.Debug(ctx, "global warn marker not removed", slog.String("error", err.Error()))
	}
}

// globalTrackingWarnText picks the warn copy. With trust_all set the per-repo
// "sync only after `entire trust`" sentence would lie — every enrolled repo is
// already syncing — so that generation warns about capture AND sync instead.
func globalTrackingWarnText(us *settings.UserSettings) string {
	file := settings.UserSettingsPath()
	if us.Global != nil && us.Global.TrustAll {
		return fmt.Sprintf("Warning: global tracking is enabled (%s) — agent sessions in every repo on this machine are captured AND synced (trust_all is enabled). See `entire status` for this repo.", file)
	}
	return fmt.Sprintf("Warning: global tracking is enabled (%s) — agent sessions in every repo on this machine are now captured locally. Checkpoints sync per repo only after `entire trust`. See `entire status` for this repo.", file)
}
