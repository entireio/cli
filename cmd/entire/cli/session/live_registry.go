package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/validation"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// LiveSessionEntry is a cross-repo pointer to an ACTIVE session so commit hooks
// in a different git common dir can discover a unique adopt candidate without
// scanning the filesystem.
type LiveSessionEntry struct {
	SessionID           string     `json:"session_id"`
	CommonDir           string     `json:"common_dir"`
	WorktreePath        string     `json:"worktree_path"`
	Phase               Phase      `json:"phase"`
	LastInteractionTime *time.Time `json:"last_interaction_time,omitempty"`
}

func liveSessionsDir() string {
	return filepath.Join(userdirs.Cache(), "live-sessions")
}

// ShouldRegisterLive reports whether state should appear in the live-session
// registry. Tombstoned (adopted-away) and ended sessions must not.
func ShouldRegisterLive(state *State) bool {
	return state != nil &&
		state.Phase.IsActive() &&
		state.EndedAt == nil &&
		!state.FullyCondensed &&
		state.AdoptedIntoWorktreePath == ""
}

// LiveSessionMaxAge is how long a live-session registry entry remains on disk
// before ListLiveSessions sweeps it. It is the single source of truth for the
// cross-common-dir auto-adopt eligibility window: the cli package's
// adoptRecentWindow is defined as this constant.
const LiveSessionMaxAge = 12 * time.Hour

// RegisterLiveSession writes or updates the cross-repo live-session pointer.
// Best-effort: callers should ignore errors so hook paths stay resilient.
func RegisterLiveSession(state *State, commonDir string) error {
	if state == nil {
		return nil
	}
	if !ShouldRegisterLive(state) {
		return UnregisterLiveSession(state.SessionID)
	}
	if err := validation.ValidateSessionID(state.SessionID); err != nil {
		return fmt.Errorf("invalid session ID: %w", err)
	}
	commonDir = strings.TrimSpace(commonDir)
	if commonDir == "" {
		return errors.New("common dir is required")
	}
	// Persist an absolute common dir. Relative values like ".git" resolve against
	// the reader's CWD and falsely match unrelated repos in sameAdoptStore.
	if abs, err := filepath.Abs(commonDir); err == nil {
		commonDir = abs
	}
	commonDir = filepath.Clean(commonDir)

	entry := LiveSessionEntry{
		SessionID:           state.SessionID,
		CommonDir:           commonDir,
		WorktreePath:        state.WorktreePath,
		Phase:               state.Phase,
		LastInteractionTime: cloneTime(state.LastInteractionTime),
	}

	dir := liveSessionsDir()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create live-sessions dir: %w", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("open live-sessions dir: %w", err)
	}
	defer root.Close()

	data, err := jsonutil.MarshalIndentWithNewline(entry, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal live session: %w", err)
	}
	fileName := state.SessionID + ".json"
	tmp, err := os.CreateTemp(dir, fileName+".*.tmp")
	if err != nil {
		return fmt.Errorf("create live session temp: %w", err)
	}
	tmpName := tmp.Name()
	removeTmp := true
	defer func() {
		if removeTmp {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write live session temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close live session temp: %w", err)
	}
	if err := root.Rename(filepath.Base(tmpName), fileName); err != nil {
		return fmt.Errorf("rename live session: %w", err)
	}
	removeTmp = false
	return nil
}

// UnregisterLiveSession removes the cross-repo live-session pointer.
func UnregisterLiveSession(sessionID string) error {
	if err := validation.ValidateSessionID(sessionID); err != nil {
		return fmt.Errorf("invalid session ID: %w", err)
	}
	dir := liveSessionsDir()
	root, err := os.OpenRoot(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open live-sessions dir: %w", err)
	}
	defer root.Close()
	_ = osroot.Remove(root, sessionID+".json") //nolint:errcheck // best-effort
	return nil
}

// ListLiveSessions returns all live-session registry entries.
// Entries older than LiveSessionMaxAge (or missing LastInteractionTime) are
// deleted during the scan so crashed sessions do not accumulate forever.
func ListLiveSessions() ([]LiveSessionEntry, error) {
	dir := liveSessionsDir()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read live-sessions dir: %w", err)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open live-sessions dir: %w", err)
	}
	defer root.Close()

	logCtx := logging.WithComponent(context.Background(), "session")
	out := make([]LiveSessionEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		sessionID := strings.TrimSuffix(entry.Name(), ".json")
		if err := validation.ValidateSessionID(sessionID); err != nil {
			continue
		}
		// Read through the already-held os.Root so a symlink planted inside the
		// registry dir cannot redirect the read outside it (os.ReadFile follows
		// symlinks; osroot.ReadFile refuses to traverse them).
		data, err := osroot.ReadFile(root, entry.Name())
		if err != nil {
			continue
		}
		var live LiveSessionEntry
		if err := json.Unmarshal(data, &live); err != nil {
			logging.Debug(logCtx, "live-registry: removing corrupt entry",
				slog.String("entry", entry.Name()),
				slog.String("error", err.Error()),
			)
			_ = osroot.Remove(root, entry.Name()) //nolint:errcheck // sweep corrupt pointer
			continue
		}
		if live.SessionID == "" {
			live.SessionID = sessionID
		}
		if liveSessionExpired(live) {
			logging.Debug(logCtx, "live-registry: sweeping expired entry",
				slog.String("session_id", live.SessionID),
			)
			_ = osroot.Remove(root, entry.Name()) //nolint:errcheck // TTL sweep
			continue
		}
		out = append(out, live)
	}
	return out, nil
}

func liveSessionExpired(entry LiveSessionEntry) bool {
	// A nil LastInteractionTime is treated as expired: it means either a crashed
	// half-written entry or a version-skew writer that stopped populating the
	// field. Either way we sweep it rather than keep an entry we cannot age out.
	if entry.LastInteractionTime == nil {
		return true
	}
	return time.Since(*entry.LastInteractionTime) > LiveSessionMaxAge
}

func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	cloned := *t
	return &cloned
}

// CommonDirFromStateDir returns the git common dir that owns a session state
// directory (.../entire-sessions).
func CommonDirFromStateDir(stateDir string) string {
	return filepath.Clean(filepath.Dir(stateDir))
}
