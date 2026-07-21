package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/proclive"
	"github.com/entireio/cli/cmd/entire/cli/validation"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// LiveSessionEntry is a cross-repo pointer to an ACTIVE session so commit hooks
// in a different git common dir can discover a unique adopt candidate without
// scanning the filesystem.
type LiveSessionEntry struct {
	SessionID           string             `json:"session_id"`
	CommonDir           string             `json:"common_dir"`
	WorktreePath        string             `json:"worktree_path"`
	AgentType           types.AgentType    `json:"agent_type,omitempty"`
	Phase               Phase              `json:"phase"`
	LastInteractionTime *time.Time         `json:"last_interaction_time,omitempty"`
	Owner               *proclive.Identity `json:"owner,omitempty"`
	FilesTouched        []string           `json:"files_touched,omitempty"`
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
// before ListLiveSessions sweeps it. Keep aligned with adoptRecentWindow in
// the cli package (cross-common-dir auto-adopt eligibility).
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

	entry := LiveSessionEntry{
		SessionID:           state.SessionID,
		CommonDir:           filepath.Clean(commonDir),
		WorktreePath:        state.WorktreePath,
		AgentType:           state.AgentType,
		Phase:               state.Phase,
		LastInteractionTime: cloneTime(state.LastInteractionTime),
		Owner:               cloneOwner(state.Owner),
		FilesTouched:        append([]string(nil), state.FilesTouched...),
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

	out := make([]LiveSessionEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		sessionID := strings.TrimSuffix(entry.Name(), ".json")
		if err := validation.ValidateSessionID(sessionID); err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name())) //nolint:gosec // path under cache dir from ReadDir
		if err != nil {
			continue
		}
		var live LiveSessionEntry
		if err := json.Unmarshal(data, &live); err != nil {
			_ = osroot.Remove(root, entry.Name()) //nolint:errcheck // sweep corrupt pointer
			continue
		}
		if live.SessionID == "" {
			live.SessionID = sessionID
		}
		if liveSessionExpired(live) {
			_ = osroot.Remove(root, entry.Name()) //nolint:errcheck // TTL sweep
			continue
		}
		out = append(out, live)
	}
	return out, nil
}

func liveSessionExpired(entry LiveSessionEntry) bool {
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

func cloneOwner(owner *proclive.Identity) *proclive.Identity {
	if owner == nil {
		return nil
	}
	cloned := *owner
	return &cloned
}

// CommonDirFromStateDir returns the git common dir that owns a session state
// directory (.../entire-sessions).
func CommonDirFromStateDir(stateDir string) string {
	return filepath.Clean(filepath.Dir(stateDir))
}
