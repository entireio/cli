package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/proclive"
	"github.com/entireio/cli/cmd/entire/cli/validation"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// homeIndexDirName is the per-user directory, under userdirs.CacheRoot, that
// maps a session ID to the worktree it runs in. A commit hook in another
// repository reads it to find the session committing there. It is only a
// pointer: readers load the session's state from that worktree's repository.
const homeIndexDirName = "session-homes"

// homePruneAge is how old a record may get before ListSessionHomes deletes
// it. Turn start rewrites a live session's record, so an older one is left
// over from a session that is gone.
const homePruneAge = 7 * 24 * time.Hour

// SessionHome is one index record: the worktree session SessionID runs in, and
// the agent process that owns it.
//
//nolint:revive // SessionHome stutter is accepted: "Home" alone reads as the agent home (State.AgentHome).
type SessionHome struct {
	SessionID    string             `json:"session_id"`
	WorktreePath string             `json:"worktree_path"`
	Owner        *proclive.Identity `json:"owner,omitempty"`
	UpdatedAt    time.Time          `json:"updated_at"`
}

// homeIndexRoot returns the shared root over the index directory, creating it
// when create is set. The returned root belongs to the osroot registry: never
// Close it.
func homeIndexRoot(create bool) (*os.Root, error) {
	cacheDir, err := userdirs.CacheDirChecked()
	if err != nil {
		return nil, fmt.Errorf("resolve cache dir: %w", err)
	}
	parent, err := userdirs.CacheRoot()
	if err != nil {
		return nil, fmt.Errorf("open cache dir: %w", err)
	}
	if create {
		if err := osroot.MkdirAllNoSymlink(parent, homeIndexDirName, 0o700); err != nil {
			return nil, fmt.Errorf("create %s: %w", homeIndexDirName, err)
		}
	}
	abs, err := filepath.Abs(cacheDir)
	if err != nil {
		return nil, fmt.Errorf("resolve cache dir: %w", err)
	}
	return osroot.SharedChild(parent, filepath.Join(abs, homeIndexDirName), homeIndexDirName) //nolint:wrapcheck // preserves missing-path classification
}

func validateSessionHome(h SessionHome) error {
	if err := validation.ValidateSessionID(h.SessionID); err != nil {
		return fmt.Errorf("invalid session ID: %w", err)
	}
	if !filepath.IsAbs(h.WorktreePath) || filepath.Clean(h.WorktreePath) != h.WorktreePath {
		return fmt.Errorf("session home worktree %q must be an absolute, clean path", h.WorktreePath)
	}
	return nil
}

// RecordSessionHome writes h to the index, stamped now.
func RecordSessionHome(h SessionHome) error {
	if err := validateSessionHome(h); err != nil {
		return err
	}
	h.UpdatedAt = time.Now().UTC()
	root, err := homeIndexRoot(true)
	if err != nil {
		return fmt.Errorf("open session home index: %w", err)
	}
	data, err := jsonutil.MarshalIndentWithNewline(h, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal session home: %w", err)
	}
	if err := jsonutil.WriteFileAtomicIn(root, h.SessionID+".json", data, 0o600); err != nil {
		return fmt.Errorf("write session home: %w", err)
	}
	return nil
}

// ListSessionHomes returns the records updated within maxAge. Unreadable
// records are skipped; invalid ones and those older than homePruneAge are
// removed.
func ListSessionHomes(maxAge time.Duration) ([]SessionHome, error) {
	root, err := homeIndexRoot(false)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open session home index: %w", err)
	}
	entries, err := osroot.ReadDir(root, ".")
	if err != nil {
		return nil, fmt.Errorf("read session home index: %w", err)
	}
	now := time.Now()
	var homes []SessionHome
	for _, entry := range entries {
		name := entry.Name()
		if !entry.Type().IsRegular() || !strings.HasSuffix(name, ".json") || jsonutil.IsTempName(name) {
			continue
		}
		data, err := osroot.ReadFileNoFollow(root, name)
		if err != nil {
			continue
		}
		var h SessionHome
		if json.Unmarshal(data, &h) != nil || validateSessionHome(h) != nil ||
			h.SessionID+".json" != name || now.Sub(h.UpdatedAt) > homePruneAge {
			_ = osroot.RemoveNoSymlinks(root, name) //nolint:errcheck // best-effort cleanup of an invalid or abandoned cache record
			continue
		}
		if now.Sub(h.UpdatedAt) <= maxAge {
			homes = append(homes, h)
		}
	}
	return homes, nil
}
