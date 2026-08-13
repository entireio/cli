package binding

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/validation"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// CurrentRecordVersion is the record schema version this binary writes.
// mutateRecord refuses to rewrite a record with a newer Version: encoding/json
// drops unknown fields on the round-trip, so an older binary would silently
// erase whatever a newer schema added (e.g. slice 2's adopt marker).
//
// Version 2 added LastScannedTranscriptCursor. Consequence: v1-only binaries
// refuse to mutate v2 records and degrade to a Debug-logged skip.
const CurrentRecordVersion = 2

// SessionRecord is the machine-level record of a session: which repos its
// activity has touched. Lives under userdirs.Config()/sessions/, outside any
// repo, because a session is not owned by a repo — repos are bound to it.
//
// ANY schema addition must bump CurrentRecordVersion: the refuse-on-newer
// check in mutateRecord is what protects new fields from being dropped by
// older binaries — there is no unknown-field preservation.
type SessionRecord struct {
	Version        int         `json:"version"`
	SessionID      string      `json:"session_id"`
	AgentType      string      `json:"agent_type,omitempty"`
	TranscriptPath string      `json:"transcript_path,omitempty"`
	LaunchRoot     string      `json:"launch_root,omitempty"` // worktree root of the session's hook cwd
	CreatedAt      time.Time   `json:"created_at"`
	UpdatedAt      time.Time   `json:"updated_at"`
	BoundRepos     []BoundRepo `json:"bound_repos,omitempty"`

	// LastScannedTranscriptCursor is the position already scanned for evidence
	// in the session's OWN transcript by the no-repo hook path, in the OWNING
	// AGENT'S extractor-native unit: lines for Claude Code JSONL, message index
	// for Gemini CLI's single-JSON format, the extractor's returned position
	// generally. The in-repo tap does not use it; its offsets come from session
	// state. It advances monotonically; a shrinking position (truncated/rotated
	// transcript) resets the scan to 0.
	LastScannedTranscriptCursor int `json:"last_scanned_transcript_cursor,omitempty"`
}

// BoundRepo records evidence that a session touched a repo. Keyed by
// CommonDir (clone identity — linked worktrees share it).
type BoundRepo struct {
	RepoIdentity

	FirstEvidenceAt time.Time `json:"first_evidence_at"`
	LastEvidenceAt  time.Time `json:"last_evidence_at"`
	EvidenceCount   int       `json:"evidence_count"`
	Enabled         bool      `json:"enabled"` // repo had .entire setup when last observed
}

// SessionMeta carries session identity fields for the record. They are filled
// on first write only; later writes never overwrite non-empty values.
type SessionMeta struct {
	AgentType      string
	TranscriptPath string
	LaunchRoot     string
}

func recordPath(sessionID string) (string, error) {
	// Session IDs are already filename-safe per validation.ValidateSessionID,
	// but the record store is a new attack surface for path traversal —
	// validate before touching the filesystem.
	if err := validation.ValidateSessionID(sessionID); err != nil {
		return "", fmt.Errorf("session record: %w", err)
	}
	return filepath.Join(userdirs.Config(), "sessions", sessionID+".json"), nil
}

// Evidence is one observation that a session touched a repo. Enabled notes
// whether the repo had .entire setup when observed.
type Evidence struct {
	Repo    RepoIdentity
	Enabled bool
}

// RecordBinding upserts evidence that sessionID touched a repo: it creates
// the record if absent, appends or updates the BoundRepo keyed by CommonDir,
// and bumps counters and timestamps.
func RecordBinding(ctx context.Context, sessionID string, meta SessionMeta, ev Evidence) error {
	return mutateRecord(ctx, sessionID, func(now time.Time, rec *SessionRecord) error {
		fillMetaIfEmpty(rec, meta)
		upsertBoundRepo(rec, now, ev)
		return nil
	})
}

// RecordEvidenceAndAdvanceCursor persists one transcript scan's outcome —
// every evidence observation plus the new scan cursor — in a SINGLE locked
// mutation. The atomicity is the point: the cursor marks transcript content as
// scanned-and-recorded, so committing it in a separate write from the evidence
// would let a failure between the two (lock timeout, transient I/O, killed
// hook) advance the cursor past lines whose evidence was never persisted —
// silently lost, because an advanced cursor means those lines are never
// rescanned. A failure here leaves BOTH unwritten: the next turn-end rescans
// the same span, and because nothing was recorded a retry cannot double-count.
//
// The record is created if absent even with no evidence ("scanned, nothing
// found yet"), so repeat scans stay cheap for sessions that never touch a
// repo. Cursor semantics (in the owning agent's extractor-native unit — see
// SessionRecord.LastScannedTranscriptCursor): when reset is false the cursor
// only moves forward — max(current, cursor) — so a racing hook reporting an
// older position can never regress it. reset=true (caller detected a truncated
// or rotated transcript) stores cursor directly, permitting regression:
// without it a shrunk transcript leaves the cursor stuck at its high watermark
// and every later turn full-rescans and re-records.
func RecordEvidenceAndAdvanceCursor(ctx context.Context, sessionID string, meta SessionMeta, evs []Evidence, cursor int, reset bool) error {
	return mutateRecord(ctx, sessionID, func(now time.Time, rec *SessionRecord) error {
		fillMetaIfEmpty(rec, meta)
		for _, ev := range evs {
			upsertBoundRepo(rec, now, ev)
		}
		if reset || cursor > rec.LastScannedTranscriptCursor {
			rec.LastScannedTranscriptCursor = cursor
		}
		return nil
	})
}

// upsertBoundRepo applies one evidence observation: bump the BoundRepo keyed
// by the evidence's CommonDir, or append a new entry.
func upsertBoundRepo(rec *SessionRecord, now time.Time, ev Evidence) {
	for i := range rec.BoundRepos {
		if rec.BoundRepos[i].CommonDir == ev.Repo.CommonDir {
			// Enabled is computed at the observed worktree root, and linked
			// worktrees of one clone can differ (.entire/settings.local.json
			// is worktree-local) — update the identity WITH the flag so the
			// stored pair always reflects the same (latest) observation.
			rec.BoundRepos[i].RepoIdentity = ev.Repo
			rec.BoundRepos[i].LastEvidenceAt = now
			rec.BoundRepos[i].EvidenceCount++
			rec.BoundRepos[i].Enabled = ev.Enabled
			return
		}
	}
	rec.BoundRepos = append(rec.BoundRepos, BoundRepo{
		RepoIdentity:    ev.Repo,
		FirstEvidenceAt: now,
		LastEvidenceAt:  now,
		EvidenceCount:   1,
		Enabled:         ev.Enabled,
	})
}

// fillMetaIfEmpty applies the first-write-only rule for session identity
// fields: later writes never overwrite non-empty values.
func fillMetaIfEmpty(rec *SessionRecord, meta SessionMeta) {
	if rec.AgentType == "" {
		rec.AgentType = meta.AgentType
	}
	if rec.TranscriptPath == "" {
		rec.TranscriptPath = meta.TranscriptPath
	}
	if rec.LaunchRoot == "" {
		rec.LaunchRoot = meta.LaunchRoot
	}
}

// LoadRecord reads the session record for sessionID. Returns (nil, nil) when
// no record exists; a malformed record is an error.
func LoadRecord(_ context.Context, sessionID string) (*SessionRecord, error) {
	path, err := recordPath(sessionID)
	if err != nil {
		return nil, err
	}
	return loadRecordFromFile(path)
}

func loadRecordFromFile(path string) (*SessionRecord, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is userdirs.Config()-rooted with a validated session ID
	if os.IsNotExist(err) {
		return nil, nil //nolint:nilnil // absence is a normal outcome, distinct from a malformed record
	}
	if err != nil {
		return nil, fmt.Errorf("read session record: %w", err)
	}
	var rec SessionRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("parse session record %s: %w", path, err)
	}
	return &rec, nil
}

// recordLockTimeout bounds the wait for the session-record flock. Hook
// contexts carry no deadline, and without one AcquireContext blocks in the
// kernel indefinitely — a wedged lock holder would then stall the hook,
// breaking the tap's never-block-capture promise. This layer is best-effort
// by design, so timing out and dropping the evidence is the correct
// degradation.
const recordLockTimeout = 2 * time.Second

// mutateRecord runs fn over the session record under an exclusive file lock:
// MkdirAll → flock → load-or-create → fn → atomic write. Two hook processes
// (agent + git hook) can race on the same session; the flock serializes them.
// fn receives the single clock reading that also stamps CreatedAt/UpdatedAt,
// so every timestamp written by one mutation agrees.
func mutateRecord(ctx context.Context, sessionID string, fn func(now time.Time, rec *SessionRecord) error) error {
	path, err := recordPath(sessionID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create sessions directory: %w", err)
	}
	lockCtx, cancel := context.WithTimeout(ctx, recordLockTimeout)
	defer cancel()
	release, err := flock.AcquireContext(lockCtx, path+".lock")
	if err != nil {
		return fmt.Errorf("lock session record: %w", err)
	}
	defer release()

	rec, err := loadRecordFromFile(path)
	if err != nil {
		return err
	}
	if rec != nil && rec.Version > CurrentRecordVersion {
		return fmt.Errorf("session record %s has version %d, newer than this binary's %d: refusing to rewrite (unknown fields would be dropped)",
			path, rec.Version, CurrentRecordVersion)
	}
	now := time.Now().UTC()
	if rec == nil {
		rec = &SessionRecord{SessionID: sessionID, CreatedAt: now}
	}
	rec.Version = CurrentRecordVersion // normalize legacy/zero-version records upward
	if err := fn(now, rec); err != nil {
		return err
	}
	rec.UpdatedAt = now
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal session record: %w", err)
	}
	if err := jsonutil.WriteFileAtomic(path, data, 0o600); err != nil {
		return fmt.Errorf("write session record: %w", err)
	}
	return nil
}
