package tokenreport

import (
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// CheckpointRow is the per-(checkpoint, session) view a reader has after
// loading a checkpoint's metadata: enough to classify the token-usage scope
// of that checkpoint's session entry and to dedupe legacy checkpoints within
// a session.
type CheckpointRow struct {
	// CheckpointID identifies the checkpoint this row was read from.
	CheckpointID string
	// SessionID identifies the session this row's token usage belongs to.
	SessionID string
	// Version is the checkpoint's root token_usage_version. Any value below
	// 2 takes the legacy rule (absent decodes as 0; 1 was never written).
	// Checkpoints written by newer CLIs stamp 2.
	Version int
	// CheckpointTranscriptStart is the session metadata's
	// checkpoint_transcript_start field: the transcript line offset this
	// checkpoint's usage was measured from. 0 when absent.
	CheckpointTranscriptStart int
	// Usage is the session metadata's token_usage block. May be nil.
	Usage *types.TokenUsage
	// CreatedAt is the checkpoint's creation time, used to order rows
	// within a session for dedupe.
	CreatedAt time.Time
}

// Scope classifies what a CheckpointRow's Usage represents: a delta scoped
// to just that checkpoint, or (for legacy rows only) a possible running
// total.
type Scope int

const (
	// ScopeDelta means Usage covers only this checkpoint.
	ScopeDelta Scope = iota
	// ScopeLegacyFromStart means this is a legacy row (Version below 2)
	// whose CheckpointTranscriptStart is 0. Usage may be a delta, or it may be
	// the session's running total up to this checkpoint (a historical bug).
	// There is no per-row "first checkpoint" signal to disambiguate the two
	// cases directly, so callers must apply DedupeLegacyCheckpoints, which
	// resolves the ambiguity using checkpoint order within the session.
	ScopeLegacyFromStart
)

// ClassifyScope determines a CheckpointRow's token-usage Scope.
//
// Checkpoint metadata carries no per-row "first checkpoint" signal — the
// metadata's checkpoints_count is a prompt count, not a row ordinal — so a
// row with Version below 2 (absent decodes as 0; 1 was never written) at
// transcript offset 0 is classified as ScopeLegacyFromStart rather than
// resolved to a definite delta or total: it may be either. A legacy row at a
// nonzero offset, or any row stamped with Version >= 2, is always a delta.
func ClassifyScope(r CheckpointRow) Scope {
	switch {
	case r.Version >= 2:
		return ScopeDelta
	case r.CheckpointTranscriptStart == 0:
		return ScopeLegacyFromStart
	default:
		return ScopeDelta
	}
}

// DedupeLegacyCheckpoints groups rows by SessionID and, within each session,
// resolves the ScopeLegacyFromStart ambiguity: sorted by CreatedAt (ties
// broken by CheckpointID for determinism only, not as a time-ordering
// fallback), the latest ScopeLegacyFromStart row is treated as an anchor —
// on the assumption that it is the session's running total and therefore
// already contains every row before it. Every row before the anchor is
// dropped regardless of its own Scope — a delta written before the running
// total is already contained in it. Rows after the anchor are kept as-is. In
// practice a session's Version >= 2 rows always post-date its legacy rows,
// so they are never collapsed. A session with a single ScopeLegacyFromStart
// row and nothing else keeps that row. A session with no ScopeLegacyFromStart
// row at all (e.g. every row is Version >= 2) keeps every row.
//
// Returned rows are ordered deterministically by SessionID, then CreatedAt,
// then CheckpointID.
//
// Precondition: CreatedAt must be set on every row; a zero CreatedAt sorts
// first and is treated as the oldest row in its session (the metadata loader
// that populates CheckpointRow is responsible for guaranteeing this).
//
// The input slice is not mutated.
func DedupeLegacyCheckpoints(rows []CheckpointRow) (kept []CheckpointRow, collapsed int) {
	bySession := make(map[string][]CheckpointRow)
	for _, r := range rows {
		bySession[r.SessionID] = append(bySession[r.SessionID], r)
	}

	for _, sessionID := range slices.Sorted(maps.Keys(bySession)) {
		group := bySession[sessionID]
		slices.SortStableFunc(group, func(a, b CheckpointRow) int {
			if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
				return c
			}
			return strings.Compare(a.CheckpointID, b.CheckpointID)
		})

		latest := -1
		for i, r := range group {
			if ClassifyScope(r) == ScopeLegacyFromStart {
				latest = i
			}
		}

		anchor := max(latest, 0)
		collapsed += anchor
		kept = append(kept, group[anchor:]...)
	}

	return kept, collapsed
}
