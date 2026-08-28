package tokenreport

import (
	"sort"
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
	// Version is the checkpoint's root token_usage_version. Checkpoints
	// written by newer CLIs stamp 2; legacy checkpoints have no version,
	// which decodes as 0.
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
	// ScopeLegacyFromStart means this is a legacy row (no token_usage_version)
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
// legacy row (Version 0) at transcript offset 0 is classified as
// ScopeLegacyFromStart rather than resolved to a definite delta or total:
// it may be either. A legacy row at a nonzero offset, or any row stamped
// with Version >= 2, is always a delta.
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
// resolves the ScopeLegacyFromStart ambiguity: sorted by CreatedAt (stable),
// the latest ScopeLegacyFromStart row is kept along with every ScopeDelta row
// created after it, on the assumption that the latest legacy-from-start row
// is the session's running total and therefore already contains every row
// before it. Rows before that latest legacy-from-start row are dropped as
// redundant. ScopeDelta rows are never collapsed. A session with a single
// ScopeLegacyFromStart row and nothing else keeps that row.
//
// Returned rows are ordered deterministically by SessionID, then CreatedAt.
// The input slice is not mutated.
func DedupeLegacyCheckpoints(rows []CheckpointRow) (kept []CheckpointRow, collapsed int) {
	bySession := make(map[string][]CheckpointRow)
	var sessionOrder []string
	for _, r := range rows {
		if _, ok := bySession[r.SessionID]; !ok {
			sessionOrder = append(sessionOrder, r.SessionID)
		}
		bySession[r.SessionID] = append(bySession[r.SessionID], r)
	}
	sort.Strings(sessionOrder)

	for _, sessionID := range sessionOrder {
		group := bySession[sessionID]
		sort.SliceStable(group, func(i, j int) bool {
			return group[i].CreatedAt.Before(group[j].CreatedAt)
		})

		latestLegacyFromStart := -1
		for i, r := range group {
			if ClassifyScope(r) == ScopeLegacyFromStart {
				latestLegacyFromStart = i
			}
		}

		for i, r := range group {
			if i < latestLegacyFromStart {
				collapsed++
				continue
			}
			kept = append(kept, r)
		}
	}

	return kept, collapsed
}
