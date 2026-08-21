package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
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

const (
	maxLiveSessionEntryBytes  = 64 << 10
	defaultLiveSessionScanCap = 256
)

// RegisterLiveSession writes or updates the cross-repo live-session pointer.
// Best-effort: callers should ignore errors so hook paths stay resilient.
func RegisterLiveSession(state *State, commonDir string) error {
	return registerLiveSession(context.Background(), state, commonDir)
}

func registerLiveSession(ctx context.Context, state *State, commonDir string) error {
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

	return withLiveSessionEntryLock(ctx, state.SessionID, func() error {
		// Preserve any in-flight adoption claim. The registry lock makes this
		// read-modify-write atomic with ClaimLiveSession and other StateStore.Save
		// calls, so a stale Save cannot erase a just-written claim.
		if prev, found, err := readLiveSessionEntry(ctx, state.SessionID); err == nil && found {
			entry.AdoptClaim = prev.AdoptClaim
		}
		return writeLiveSessionEntry(entry)
	})
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
func readLiveSessionEntry(ctx context.Context, sessionID string) (LiveSessionEntry, bool, error) {
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

	if err := ctx.Err(); err != nil {
		return LiveSessionEntry{}, false, err
	}
	fileName := sessionID + ".json"
	info, err := root.Stat(fileName)
	if err != nil {
		if os.IsNotExist(err) {
			return LiveSessionEntry{}, false, nil
		}
		return LiveSessionEntry{}, false, fmt.Errorf("stat live session: %w", err)
	}
	if !info.Mode().IsRegular() {
		return LiveSessionEntry{}, false, fmt.Errorf("live session entry is not a regular file")
	}
	if info.Size() > maxLiveSessionEntryBytes {
		return LiveSessionEntry{}, false, fmt.Errorf("live session entry exceeds %d bytes", maxLiveSessionEntryBytes)
	}
	f, err := root.Open(fileName)
	if err != nil {
		return LiveSessionEntry{}, false, fmt.Errorf("open live session: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxLiveSessionEntryBytes+1))
	if err != nil {
		return LiveSessionEntry{}, false, fmt.Errorf("read live session: %w", err)
	}
	if len(data) > maxLiveSessionEntryBytes {
		return LiveSessionEntry{}, false, fmt.Errorf("live session entry exceeds %d bytes", maxLiveSessionEntryBytes)
	}
	if err := ctx.Err(); err != nil {
		return LiveSessionEntry{}, false, err
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
	return unregisterLiveSession(context.Background(), sessionID, commonDir, "")
}

// UnregisterLiveSessionForWorktree removes an entry only when both its common
// dir and worktree match. This matters for linked worktrees sharing a common dir.
func UnregisterLiveSessionForWorktree(ctx context.Context, sessionID, commonDir, worktreePath string) error {
	return unregisterLiveSession(ctx, sessionID, commonDir, worktreePath)
}

func unregisterLiveSession(ctx context.Context, sessionID, commonDir, worktreePath string) error {
	if err := validation.ValidateSessionID(sessionID); err != nil {
		return fmt.Errorf("invalid session ID: %w", err)
	}
	return withLiveSessionEntryLock(ctx, sessionID, func() error {
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
		existing, found, readErr := readLiveSessionEntry(ctx, sessionID)
		if readErr == nil && found && existing.CommonDir != "" && strings.TrimSpace(commonDir) != "" {
			// A live adoption claim is the cross-repository handoff token. A target
			// may become ended/condensed during strategy.PostCommit before the
			// deferred source retire runs; its StateStore.Save must not erase that
			// token out from under finalization.
			if existing.AdoptClaim != nil && !existing.AdoptClaim.At.IsZero() &&
				time.Since(existing.AdoptClaim.At) <= AdoptClaimMaxAge {
				return nil
			}
			if normalizeCommonDir(existing.CommonDir) != normalizeCommonDir(commonDir) {
				return nil
			}
			if worktreePath != "" && existing.WorktreePath != "" &&
				!sameRegistryPath(existing.WorktreePath, worktreePath) {
				return nil
			}
		}
		_ = osroot.Remove(root, fileName) //nolint:errcheck // best-effort
		return nil
	})
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

// ListLiveSessions returns live-session registry entries up to a conservative
// process-wide cap. Hook-path callers that must distinguish a complete scan
// from a truncated one use ListLiveSessionsContext directly.
// Entries older than LiveSessionMaxAge (or missing LastInteractionTime) are
// deleted during the scan so crashed sessions do not accumulate forever.
func ListLiveSessions() ([]LiveSessionEntry, error) {
	entries, _, err := ListLiveSessionsContext(context.Background(), defaultLiveSessionScanCap)
	return entries, err
}

// ListLiveSessionsContext incrementally scans at most maxEntries JSON entries.
// complete is false when the context expires or another eligible registry file
// exists beyond the cap; callers must fail closed when uniqueness matters.
func ListLiveSessionsContext(ctx context.Context, maxEntries int) ([]LiveSessionEntry, bool, error) {
	if maxEntries <= 0 {
		return nil, false, errors.New("live-session scan cap must be positive")
	}
	dir := liveSessionsDir()
	dirFile, err := os.Open(dir)
	if os.IsNotExist(err) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("open live-sessions dir: %w", err)
	}
	defer dirFile.Close()

	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, false, fmt.Errorf("open live-sessions dir: %w", err)
	}
	defer root.Close()

	logCtx := logging.WithComponent(context.Background(), "session")
	out := make([]LiveSessionEntry, 0, min(maxEntries, 32))
	seen := 0
	for {
		if err := ctx.Err(); err != nil {
			return out, false, nil
		}
		entries, readErr := dirFile.ReadDir(32)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return out, false, fmt.Errorf("read live-sessions dir: %w", readErr)
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return out, false, nil
			}
			if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			if seen >= maxEntries {
				return out, false, nil
			}
			seen++
			sessionID := strings.TrimSuffix(entry.Name(), ".json")
			if err := validation.ValidateSessionID(sessionID); err != nil {
				continue
			}
			// Read through the already-held os.Root so a symlink planted inside the
			// registry dir cannot redirect the read outside it (os.ReadFile follows
			// symlinks; osroot.ReadFile refuses to traverse them).
			live, found, err := readLiveSessionEntry(ctx, sessionID)
			if err != nil || !found {
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
		if errors.Is(readErr, io.EOF) {
			return out, true, nil
		}
	}
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
	return LiveSessionClaimContext(context.Background(), sessionID)
}

func LiveSessionClaimContext(ctx context.Context, sessionID string) (*AdoptClaim, error) {
	entry, ok, err := readLiveSessionEntry(ctx, sessionID)
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
// The registry's per-session cross-process lock makes this read-modify-write
// atomic with claims, releases, and StateStore.Save registry refreshes. Adopt
// additionally holds the source and target state locks while coordinating the
// state-file changes.
func ClaimLiveSession(sessionID, byCommonDir, byWorktreePath string, at time.Time) (bool, error) {
	return ClaimLiveSessionContext(context.Background(), sessionID, AdoptClaim{
		ByCommonDir: byCommonDir, ByWorktreePath: byWorktreePath, At: at,
	})
}

// ClaimLiveSessionContext atomically records claim. A fresh claim can only be
// renewed by the exact same worktree and adoption-attempt nonce.
func ClaimLiveSessionContext(ctx context.Context, sessionID string, claim AdoptClaim) (bool, error) {
	if err := validation.ValidateSessionID(sessionID); err != nil {
		return false, fmt.Errorf("invalid session ID: %w", err)
	}
	claim.ByCommonDir = normalizeCommonDir(claim.ByCommonDir)
	claim.ByWorktreePath = normalizeRegistryPath(claim.ByWorktreePath)
	if claim.ByCommonDir == "" {
		return false, errors.New("claiming common dir is required")
	}
	if claim.AttemptID == "" {
		// Legacy callers have no nonce. Keep their behavior compatible while new
		// hook callers use a real attempt ID and exact ownership.
		claim.AttemptID = "legacy"
	}
	claimed := false
	err := withLiveSessionEntryLock(ctx, sessionID, func() error {
		entry, found, err := readLiveSessionEntry(ctx, sessionID)
		if err != nil {
			return err
		}
		if !found {
			entry = LiveSessionEntry{SessionID: sessionID}
		}
		if adoptClaimHeldByOther(entry.AdoptClaim, claim) {
			return nil
		}
		entry.AdoptClaim = &claim
		if err := writeLiveSessionEntry(entry); err != nil {
			return err
		}
		claimed = true
		return nil
	})
	return claimed, err
}

// adoptClaimHeldByOther reports whether a fresh claim belongs to a different
// worktree/attempt. A stale claim is replaceable so aborted commits self-heal.
func adoptClaimHeldByOther(claim *AdoptClaim, proposed AdoptClaim) bool {
	if claim == nil || claim.At.IsZero() {
		return false
	}
	if time.Since(claim.At) > AdoptClaimMaxAge {
		return false
	}
	return !sameAdoptClaimOwner(claim, &proposed)
}

// ReleaseLiveSessionClaim clears sessionID's claim when it is held by
// byCommonDir. Used by the post-commit finalize so a completed adopt does not
// leave a claim blocking the next one for the rest of AdoptClaimMaxAge.
// Best-effort: a missing entry or a claim held by someone else is a no-op.
func ReleaseLiveSessionClaim(sessionID, byCommonDir string) error {
	_, err := ReleaseLiveSessionClaimIfOwned(context.Background(), sessionID, AdoptClaim{ByCommonDir: byCommonDir})
	return err
}

// ReleaseLiveSessionClaimIfOwned clears only an exact claim. Empty expected
// worktree/attempt fields provide backward-compatible common-dir-only matching.
func ReleaseLiveSessionClaimIfOwned(ctx context.Context, sessionID string, expected AdoptClaim) (bool, error) {
	released := false
	err := withLiveSessionEntryLock(ctx, sessionID, func() error {
		entry, found, err := readLiveSessionEntry(ctx, sessionID)
		if err != nil || !found || entry.AdoptClaim == nil {
			return err
		}
		if !claimMatchesExpected(entry.AdoptClaim, &expected) {
			return nil
		}
		entry.AdoptClaim = nil
		if err := writeLiveSessionEntry(entry); err != nil {
			return err
		}
		released = true
		return nil
	})
	return released, err
}

func withLiveSessionEntryLock(ctx context.Context, sessionID string, fn func() error) error {
	if err := validation.ValidateSessionID(sessionID); err != nil {
		return fmt.Errorf("invalid session ID: %w", err)
	}
	dir := liveSessionsDir()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create live-sessions dir: %w", err)
	}
	release, err := flock.AcquireContext(ctx, filepath.Join(dir, sessionID+".lock"))
	if err != nil {
		return fmt.Errorf("acquire live-session lock: %w", err)
	}
	defer release()
	return fn()
}

func sameAdoptClaimOwner(a, b *AdoptClaim) bool {
	if a == nil || b == nil {
		return false
	}
	return normalizeCommonDir(a.ByCommonDir) == normalizeCommonDir(b.ByCommonDir) &&
		normalizeRegistryPath(a.ByWorktreePath) == normalizeRegistryPath(b.ByWorktreePath) &&
		a.ByWorktreeID == b.ByWorktreeID && a.AttemptID == b.AttemptID
}

func claimMatchesExpected(actual, expected *AdoptClaim) bool {
	if actual == nil || expected == nil || normalizeCommonDir(actual.ByCommonDir) != normalizeCommonDir(expected.ByCommonDir) {
		return false
	}
	if expected.ByWorktreePath != "" && !sameRegistryPath(actual.ByWorktreePath, expected.ByWorktreePath) {
		return false
	}
	if expected.ByWorktreeID != "" && actual.ByWorktreeID != expected.ByWorktreeID {
		return false
	}
	return expected.AttemptID == "" || actual.AttemptID == expected.AttemptID
}

func normalizeRegistryPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	return filepath.Clean(path)
}

func sameRegistryPath(a, b string) bool {
	return normalizeRegistryPath(a) == normalizeRegistryPath(b)
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
