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
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// globalWarnMarkerName marks that the current enabled generation has been
// announced. Generations are observational (hand-edits bypass every writer):
// observed-off deletes the marker; the next observed-enabled command warns.
const globalWarnMarkerName = "global_warn_ack"

func globalWarnMarkerPath() (string, error) {
	configDir, err := userdirs.ConfigDirChecked()
	if err != nil {
		return "", fmt.Errorf("resolve global warning directory: %w", err)
	}
	return filepath.Join(configDir, globalWarnMarkerName), nil
}

// globalPostRun is the root PersistentPostRun hook for the global tier: the
// one-time detection warning plus user-hook reconciliation, sharing a single
// read of the user settings file. Unreadable settings stay silent — doctor is
// that failure's surface, and a warn here would fire on every command forever.
// The installer (curl-bash-post-install) calls it explicitly because hidden
// commands skip the root post-run; `entire trust` is experimental and so
// hidden in stable builds — it does not trigger the post-run there, which is
// harmless (the next visible command does).
func globalPostRun(ctx context.Context, errW io.Writer) {
	us, err := settings.LoadUserSettings(ctx)
	if err != nil {
		return
	}
	maybeWarnGlobalTracking(ctx, us, errW)
	reconcileUserHooks(ctx, us, errW)
}

// maybeWarnGlobalTracking is the foreground detection warn. The marker holds
// the announced generation — "enabled" or "enabled+trust_all" — so flipping
// trust_all on while already enabled re-warns with the wider "captured AND
// synced" copy instead of staying silent.
func maybeWarnGlobalTracking(ctx context.Context, us *settings.UserSettings, errW io.Writer) {
	markerPath, err := globalWarnMarkerPath()
	if err != nil {
		logging.Debug(ctx, "global warn marker path unavailable", slog.String("error", err.Error()))
		return
	}
	acked, statErr := os.ReadFile(markerPath) //nolint:gosec // ConfigDirChecked validates the user-global root
	markerPresent := statErr == nil
	if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
		// Treated as marker-absent: can only over-warn, never suppress.
		logging.Debug(ctx, "global warn marker unreadable; treating as absent", slog.String("error", statErr.Error()))
	}
	generation := globalWarnGeneration(us)
	switch {
	case us.GlobalEnabled() && (!markerPresent || strings.TrimSpace(string(acked)) != generation):
		fmt.Fprintln(errW, globalTrackingWarnText(us))
		ackGlobalWarnMarker(ctx, generation)
	case !us.GlobalEnabled() && markerPresent:
		// Off-detection: a hand-edited disable still owes the held-data note.
		if err := os.Remove(markerPath); err != nil {
			return // marker survived; retry (and print) on a later command
		}
		fmt.Fprintln(errW, "Global tracking is off; locally captured checkpoints in untrusted repos will not sync.")
	}
}

// globalWarnGeneration names the enabled state the warning describes.
func globalWarnGeneration(us *settings.UserSettings) string {
	if us.Global != nil && us.Global.TrustAll {
		return "enabled+trust_all"
	}
	return "enabled"
}

// ackGlobalWarnMarker records which enabled generation the detection warning
// announced. Best-effort: a failed write only re-warns.
func ackGlobalWarnMarker(ctx context.Context, generation string) {
	markerPath, err := globalWarnMarkerPath()
	if err != nil {
		logging.Debug(ctx, "global warn marker not written", slog.String("error", err.Error()))
		return
	}
	if err := userdirs.EnsurePrivateDir(filepath.Dir(markerPath)); err != nil {
		logging.Debug(ctx, "global warn marker not written", slog.String("error", err.Error()))
		return
	}
	if err := os.WriteFile(markerPath, []byte(generation+"\n"), 0o600); err != nil {
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
