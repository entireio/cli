package strategy

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/session"
)

const (
	// orphanedSessionWarningThreshold is the number of active sessions above
	// which a warning is displayed to the user.
	orphanedSessionWarningThreshold = 10

	// sessionWarningCooldown is the minimum interval between warnings.
	sessionWarningCooldown = 1 * time.Hour

	// sessionWarningFile is the name of the rate-limit timestamp file.
	sessionWarningFile = "last_session_warning"
)

// warnIfTooManySessions checks the number of active session state files and
// prints a warning to stderr when the count exceeds orphanedSessionWarningThreshold.
// The warning is rate-limited to once per sessionWarningCooldown using a
// timestamp file in the session state directory.
func warnIfTooManySessions(ctx context.Context) {
	store, err := session.NewStateStore(ctx)
	if err != nil {
		return
	}

	states, err := store.List(ctx)
	if err != nil || len(states) <= orphanedSessionWarningThreshold {
		return
	}

	stateDir, err := getSessionStateDir(ctx)
	if err != nil {
		return
	}

	warningPath := filepath.Join(stateDir, sessionWarningFile)
	if info, err := os.Stat(warningPath); err == nil {
		if time.Since(info.ModTime()) < sessionWarningCooldown {
			return // warned recently
		}
	}

	// Update the timestamp file (best-effort).
	_ = os.WriteFile(warningPath, nil, 0o600)

	count := len(states)
	logCtx := logging.WithComponent(ctx, "session")
	logging.Warn(logCtx, "high session count detected",
		slog.Int("count", count),
		slog.Int("threshold", orphanedSessionWarningThreshold),
	)

	fmt.Fprintf(os.Stderr, "[entire] Warning: %d active sessions detected. Run 'entire doctor' to check for stuck sessions or 'entire clean' to clean up.\n", count)
}
