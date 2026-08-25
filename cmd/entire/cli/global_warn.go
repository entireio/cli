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

// globalWarnMarkerName marks that the current enabled generation has been
// announced. Generations are observational (hand-edits bypass every writer):
// observed-off deletes the marker; the next observed-enabled command warns.
const globalWarnMarkerName = "global_warn_ack"

func globalWarnMarkerPath() string {
	return filepath.Join(userdirs.Config(), globalWarnMarkerName)
}

// maybeWarnGlobalTracking is the foreground detection warn, run from the root
// PersistentPostRun. Unreadable settings stay silent — doctor is that
// failure's surface, and a warn here would fire on every command forever.
func maybeWarnGlobalTracking(ctx context.Context, errW io.Writer) {
	us, err := settings.LoadUserSettings(ctx)
	if err != nil {
		return
	}
	_, statErr := os.Stat(globalWarnMarkerPath())
	markerPresent := statErr == nil
	if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
		// Treated as marker-absent: can only over-warn, never suppress.
		logging.Debug(ctx, "global warn marker unreadable; treating as absent", slog.String("error", statErr.Error()))
	}
	switch {
	case us.GlobalEnabled() && !markerPresent:
		fmt.Fprintln(errW, globalTrackingWarnText(us))
		ackGlobalWarnMarker(ctx)
	case !us.GlobalEnabled() && markerPresent:
		// Off-detection: a hand-edited disable still owes the held-data note.
		if err := os.Remove(globalWarnMarkerPath()); err != nil {
			return // marker survived; retry (and print) on a later command
		}
		fmt.Fprintln(errW, "Global tracking is off; locally captured checkpoints in untrusted repos will not sync.")
	}
}

// ackGlobalWarnMarker records that the current enabled generation has been
// announced by the detection warning. Best-effort: a failed write only re-warns.
func ackGlobalWarnMarker(ctx context.Context) {
	if err := os.MkdirAll(userdirs.Config(), 0o700); err != nil {
		logging.Debug(ctx, "global warn marker not written", slog.String("error", err.Error()))
		return
	}
	if err := os.WriteFile(globalWarnMarkerPath(), nil, 0o600); err != nil {
		logging.Debug(ctx, "global warn marker not written", slog.String("error", err.Error()))
	}
}

// globalTrackingWarnText picks the warn copy: under trust_all the per-repo
// "sync only after `entire trust`" sentence would lie, so warn capture+sync.
func globalTrackingWarnText(us *settings.UserSettings) string {
	file := settings.UserSettingsPath()
	if us.Global != nil && us.Global.TrustAll {
		return fmt.Sprintf("Warning: global tracking is enabled (%s) — agent sessions in every repo on this machine are captured AND synced (trust_all is enabled). See `entire status` for this repo.", file)
	}
	return fmt.Sprintf("Warning: global tracking is enabled (%s) — agent sessions in every repo on this machine are now captured locally. Checkpoints sync per repo only after `entire trust`. See `entire status` for this repo.", file)
}
