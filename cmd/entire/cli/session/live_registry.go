package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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
	// AdoptClaim marks an in-flight cross-common-dir adoption of this session.
	//
	// It lives here rather than on the source session state because the registry is
	// keyed by session ID ALONE — one file per session, shared by every repo on the
	// machine — so it is the only place two repos racing to adopt the same session
	// can both see. Stamping the claim on the source's own state instead needed a
	// write to a second store, which in turn needed a rollback path when that write
	// failed; that is the machinery this replaces.
	AdoptClaim *AdoptClaim `json:"adopt_claim,omitempty"`
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
		return UnregisterLiveSession(state.SessionID, commonDir)
	}
	if err := validation.ValidateSessionID(state.SessionID); err != nil {
		return fmt.Errorf("invalid session ID: %w", err)
	}
	if strings.TrimSpace(commonDir) == "" {
		return errors.New("common dir is required")
	}
	// Persist an absolute common dir. Relative values like ".git" resolve against
	// the reader's CWD and falsely match unrelated repos in sameAdoptStore.
	commonDir = normalizeCommonDir(commonDir)

	entry := LiveSessionEntry{
		SessionID:           state.SessionID,
		CommonDir:           commonDir,
		WorktreePath:        state.WorktreePath,
		Phase:               state.Phase,
		LastInteractionTime: cloneTime(state.LastInteractionTime),
	}

	// Preserve any in-flight adoption claim: a re-register (the session keeps
	// working while a deferred adopt is pending) must not silently drop the claim
	// another repo is relying on.
	if prev, found, err := readLiveSessionEntry(state.SessionID); err == nil && found {
		entry.AdoptClaim = prev.AdoptClaim
	}
	return writeLiveSessionEntry(entry)
}

// writeLiveSessionEntry atomically replaces the entry file (temp + rename under
// an os.Root confined to the registry dir).
func writeLiveSessionEntry(entry LiveSessionEntry) error {
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
	fileName := entry.SessionID + ".json"
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

// readLiveSessionEntry loads one entry by session id. found=false when the file
// is absent; an unparseable file is reported as an error rather than silently
// treated as absent, so a claim is never lost to a corrupt read.
func readLiveSessionEntry(sessionID string) (LiveSessionEntry, bool, error) {
	if err := validation.ValidateSessionID(sessionID); err != nil {
		return LiveSessionEntry{}, false, fmt.Errorf("invalid session ID: %w", err)
	}
	dir := liveSessionsDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return LiveSessionEntry{}, false, nil
		}
		return LiveSessionEntry{}, false, fmt.Errorf("open live-sessions dir: %w", err)
	}
	defer root.Close()

	data, err := fs.ReadFile(root.FS(), sessionID+".json")
	if err != nil {
		if os.IsNotExist(err) {
			return LiveSessionEntry{}, false, nil
		}
		return LiveSessionEntry{}, false, fmt.Errorf("read live session: %w", err)
	}
	var entry LiveSessionEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return LiveSessionEntry{}, false, fmt.Errorf("parse live session %s: %w", sessionID, err)
	}
	return entry, true, nil
}

// UnregisterLiveSession removes the cross-repo live-session pointer for
// sessionID, but only when the on-disk entry belongs to commonDir.
//
// The registry is keyed by session ID alone, so a cross-repo adopt writes the
// TARGET's entry and then retires the SOURCE session in the same breath (Save
// of the tombstoned source state → this unregister). Without the common-dir
// scope the source retire would delete the entry the target just wrote, erasing
// the adopted session from the registry. An entry owned by a different common
// dir is therefore left untouched; a missing entry, or one we cannot read/parse
// (junk), is removed best-effort.
func UnregisterLiveSession(sessionID, commonDir string) error {
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

	fileName := sessionID + ".json"
	if data, readErr := osroot.ReadFile(root, fileName); readErr == nil {
		var existing LiveSessionEntry
		if json.Unmarshal(data, &existing) == nil &&
			existing.CommonDir != "" && strings.TrimSpace(commonDir) != "" &&
			normalizeCommonDir(existing.CommonDir) != normalizeCommonDir(commonDir) {
			return nil // entry belongs to a different repo; not ours to remove
		}
	}
	_ = osroot.Remove(root, fileName) //nolint:errcheck // best-effort
	return nil
}

// normalizeCommonDir makes a git common dir absolute and clean so entries and
// callers compare on the same canonical form regardless of the reader's CWD.
func normalizeCommonDir(commonDir string) string {
	commonDir = strings.TrimSpace(commonDir)
	if abs, err := filepath.Abs(commonDir); err == nil {
		commonDir = abs
	}
	return filepath.Clean(commonDir)
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
	// A fresh adoption claim pins the entry regardless of interaction time. A
	// claim-only entry (ClaimLiveSession upserting a source found by the sibling
	// scan, which was never registered live) has no LastInteractionTime at all, so
	// without this it would be swept on the very next list — dropping the claim it
	// was created to hold, mid-adopt.
	if entry.AdoptClaim != nil && !entry.AdoptClaim.At.IsZero() &&
		time.Since(entry.AdoptClaim.At) <= AdoptClaimMaxAge {
		return false
	}
	// A nil LastInteractionTime is treated as expired: it means either a crashed
	// half-written entry or a version-skew writer that stopped populating the
	// field. Either way we sweep it rather than keep an entry we cannot age out.
	if entry.LastInteractionTime == nil {
		return true
	}
	return time.Since(*entry.LastInteractionTime) > LiveSessionMaxAge
}

// AdoptClaimMaxAge bounds how long an in-flight adoption claim keeps blocking
// other targets. Deliberately NOT LiveSessionMaxAge: a claim only has to bridge
// prepare-commit-msg → post-commit within a single commit, not the lifetime of a
// live session, and it gates the manual adopt path too — so an over-long window
// turns an aborted commit into a lockout on `entire session adopt`. One hour
// covers a commit left open in an editor by a wide margin.
const AdoptClaimMaxAge = time.Hour

// LiveSessionClaim returns the in-flight adoption claim recorded for sessionID,
// or nil when there is none (or the entry is absent). Errors are returned so
// callers can distinguish "no claim" from "could not read".
func LiveSessionClaim(sessionID string) (*AdoptClaim, error) {
	entry, ok, err := readLiveSessionEntry(sessionID)
	if err != nil || !ok {
		return nil, err
	}
	return entry.AdoptClaim, nil
}

// ClaimLiveSession records byCommonDir's in-flight adoption of sessionID and
// reports whether the claim is now held by this caller.
//
// It UPSERTS: a source discovered by the sibling scan may have no registry entry
// at all, and refusing to claim it would leave exactly those adoptions unguarded.
// The synthesized entry carries the claim and nothing else; liveSessionExpired
// pins it for AdoptClaimMaxAge so the sweep cannot drop it mid-adopt.
//
// Callers MUST hold the source common dir's session-state lock (adopt does, via
// strategy.WithSessionStateLocks) — that is what makes this read-modify-write
// atomic against a concurrent adopter, and it is the same serialization the
// previous source-state claim relied on.
func ClaimLiveSession(sessionID, byCommonDir, byWorktreePath string, at time.Time) (bool, error) {
	if err := validation.ValidateSessionID(sessionID); err != nil {
		return false, fmt.Errorf("invalid session ID: %w", err)
	}
	byCommonDir = normalizeCommonDir(byCommonDir)
	if byCommonDir == "" {
		return false, errors.New("claiming common dir is required")
	}

	entry, found, err := readLiveSessionEntry(sessionID)
	if err != nil {
		return false, err
	}
	if !found {
		entry = LiveSessionEntry{SessionID: sessionID}
	}
	if adoptClaimHeldByOther(entry.AdoptClaim, byCommonDir) {
		return false, nil
	}
	entry.AdoptClaim = &AdoptClaim{ByCommonDir: byCommonDir, ByWorktreePath: byWorktreePath, At: at}
	if err := writeLiveSessionEntry(entry); err != nil {
		return false, err
	}
	return true, nil
}

// adoptClaimHeldByOther reports whether a FRESH claim by a DIFFERENT common dir
// is present. A stale claim (aborted commit) or a re-claim by the same target is
// not a block — re-adopting into the same repo is idempotent.
func adoptClaimHeldByOther(claim *AdoptClaim, byCommonDir string) bool {
	if claim == nil || claim.At.IsZero() {
		return false
	}
	if time.Since(claim.At) > AdoptClaimMaxAge {
		return false
	}
	return normalizeCommonDir(claim.ByCommonDir) != byCommonDir
}

// ReleaseLiveSessionClaim clears sessionID's claim when it is held by
// byCommonDir. Used by the post-commit finalize so a completed adopt does not
// leave a claim blocking the next one for the rest of AdoptClaimMaxAge.
// Best-effort: a missing entry or a claim held by someone else is a no-op.
func ReleaseLiveSessionClaim(sessionID, byCommonDir string) error {
	entry, found, err := readLiveSessionEntry(sessionID)
	if err != nil || !found || entry.AdoptClaim == nil {
		return err
	}
	if normalizeCommonDir(entry.AdoptClaim.ByCommonDir) != normalizeCommonDir(byCommonDir) {
		return nil
	}
	entry.AdoptClaim = nil
	return writeLiveSessionEntry(entry)
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
