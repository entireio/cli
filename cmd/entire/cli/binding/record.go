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
	return mutateRecord(ctx, sessionID, func(rec *SessionRecord) error {
		now := time.Now().UTC()
		fillMetaIfEmpty(rec, meta)
		for i := range rec.BoundRepos {
			if rec.BoundRepos[i].CommonDir == ev.Repo.CommonDir {
				rec.BoundRepos[i].LastEvidenceAt = now
				rec.BoundRepos[i].EvidenceCount++
				rec.BoundRepos[i].Enabled = ev.Enabled
				return nil
			}
		}
		rec.BoundRepos = append(rec.BoundRepos, BoundRepo{
			RepoIdentity:    ev.Repo,
			FirstEvidenceAt: now,
			LastEvidenceAt:  now,
			EvidenceCount:   1,
			Enabled:         ev.Enabled,
		})
		return nil
	})
}

// AdvanceTranscriptCursor records that the no-repo evidence path has scanned
// sessionID's transcript up to cursor (in the owning agent's extractor-native
// unit — see SessionRecord.LastScannedTranscriptCursor). It creates the record
// if absent ("scanned, nothing found yet"), so repeat scans stay cheap even
// for sessions that never touch a repo. When reset is false the cursor only
// moves forward — max(current, cursor) — so a racing hook reporting an older
// position can never regress it. reset=true (caller detected a truncated or
// rotated transcript) stores cursor directly, permitting regression: without
// it a shrunk transcript leaves the cursor stuck at its high watermark and
// every later turn full-rescans and re-records.
func AdvanceTranscriptCursor(ctx context.Context, sessionID string, meta SessionMeta, cursor int, reset bool) error {
	return mutateRecord(ctx, sessionID, func(rec *SessionRecord) error {
		fillMetaIfEmpty(rec, meta)
		if reset || cursor > rec.LastScannedTranscriptCursor {
			rec.LastScannedTranscriptCursor = cursor
		}
		return nil
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

// mutateRecord runs fn over the session record under an exclusive file lock:
// MkdirAll → flock → load-or-create → fn → atomic write. Two hook processes
// (agent + git hook) can race on the same session; the flock serializes them.
func mutateRecord(ctx context.Context, sessionID string, fn func(*SessionRecord) error) error {
	path, err := recordPath(sessionID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create sessions directory: %w", err)
	}
	release, err := flock.AcquireContext(ctx, path+".lock")
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
	if err := fn(rec); err != nil {
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
