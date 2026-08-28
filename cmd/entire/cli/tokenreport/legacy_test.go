package tokenreport

import (
	"slices"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// mtok builds a *types.TokenUsage carrying m million tokens in
// CacheReadTokens, so tests can express usage sizes like mtok(2.0) for 2.0M.
func mtok(m float64) *types.TokenUsage {
	return &types.TokenUsage{CacheReadTokens: int(m * 1_000_000)}
}

// at returns a deterministic, strictly increasing time for ordinal n.
func at(n int) time.Time {
	return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(n) * time.Hour)
}

// total sums the four token classes across all rows' Usage (nil Usage
// contributes 0).
func total(rows []CheckpointRow) int {
	sum := 0
	for _, r := range rows {
		if r.Usage == nil {
			continue
		}
		sum += r.Usage.InputTokens + r.Usage.CacheCreationTokens + r.Usage.CacheReadTokens + r.Usage.OutputTokens
	}
	return sum
}

// ids returns the CheckpointIDs of rows, in order, for assertion messages.
func ids(rows []CheckpointRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.CheckpointID
	}
	return out
}

func TestClassifyScope(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		row  CheckpointRow
		want Scope
	}{
		{"v2 is delta", CheckpointRow{Version: 2}, ScopeDelta},
		{"legacy at offset 0 may be the session's running total", CheckpointRow{Version: 0, CheckpointTranscriptStart: 0}, ScopeLegacyFromStart},
		{"legacy with an offset is a delta", CheckpointRow{Version: 0, CheckpointTranscriptStart: 4120}, ScopeDelta},
		{"v2 at offset 0 (post carry-forward) is still a delta", CheckpointRow{Version: 2, CheckpointTranscriptStart: 0}, ScopeDelta},
		{"version 1 (never written) still takes the legacy rule", CheckpointRow{Version: 1, CheckpointTranscriptStart: 0}, ScopeLegacyFromStart},
		{"negative version takes the legacy rule", CheckpointRow{Version: -1, CheckpointTranscriptStart: 0}, ScopeLegacyFromStart},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := ClassifyScope(tt.row); got != tt.want {
				t.Errorf("ClassifyScope(%+v) = %v, want %v", tt.row, got, tt.want)
			}
		})
	}
}

func TestDedupeLegacyCheckpoints_GroundingExample(t *testing.T) {
	t.Parallel()
	// Real shape from history: A (first, 2.0M), B (cumulative running total, 5.5M), C (delta after B, 0.8M) → 6.3M, 1 collapsed
	rows := []CheckpointRow{
		{CheckpointID: "A", SessionID: "s", CheckpointTranscriptStart: 0, Usage: mtok(2.0), CreatedAt: at(1)},
		{CheckpointID: "B", SessionID: "s", CheckpointTranscriptStart: 0, Usage: mtok(5.5), CreatedAt: at(2)},
		{CheckpointID: "C", SessionID: "s", CheckpointTranscriptStart: 4120, Usage: mtok(0.8), CreatedAt: at(3)},
	}
	kept, collapsed := DedupeLegacyCheckpoints(rows)
	if collapsed != 1 {
		t.Errorf("collapsed %d, want 1", collapsed)
	}
	if got := total(kept); got != 6_300_000 {
		t.Errorf("total %d, want 6300000", got)
	}
	if len(kept) != 2 || kept[0].CheckpointID != "B" || kept[1].CheckpointID != "C" {
		t.Errorf("kept %+v", ids(kept))
	}
}

func TestDedupe_InputOrderDoesNotMatter(t *testing.T) {
	t.Parallel()
	// Same rows as the grounding example, fed in reverse (C, B, A) order.
	// Result must not depend on input order — this pins the internal sort.
	rows := []CheckpointRow{
		{CheckpointID: "C", SessionID: "s", CheckpointTranscriptStart: 4120, Usage: mtok(0.8), CreatedAt: at(3)},
		{CheckpointID: "B", SessionID: "s", CheckpointTranscriptStart: 0, Usage: mtok(5.5), CreatedAt: at(2)},
		{CheckpointID: "A", SessionID: "s", CheckpointTranscriptStart: 0, Usage: mtok(2.0), CreatedAt: at(1)},
	}
	kept, collapsed := DedupeLegacyCheckpoints(rows)
	if collapsed != 1 {
		t.Errorf("collapsed %d, want 1", collapsed)
	}
	if got := ids(kept); !slices.Equal(got, []string{"B", "C"}) {
		t.Errorf("kept %v, want [B C]", got)
	}
}

func TestDedupe_SingleLegacyFirstCheckpointIsKept(t *testing.T) {
	t.Parallel()
	rows := []CheckpointRow{
		{CheckpointID: "A", SessionID: "s", CheckpointTranscriptStart: 0, Usage: mtok(2.0), CreatedAt: at(1)},
	}
	kept, collapsed := DedupeLegacyCheckpoints(rows)
	if collapsed != 0 {
		t.Errorf("collapsed %d, want 0", collapsed)
	}
	if len(kept) != 1 || kept[0].CheckpointID != "A" {
		t.Errorf("kept %+v, want [A]", ids(kept))
	}
}

func TestDedupe_DeltaBeforeLatestCumulativeIsDropped(t *testing.T) {
	t.Parallel()
	// A offset-0 2.0M (t1), D delta 0.5M (t2), B offset-0 5.5M (t3, cumulative,
	// supersedes both A and D) → keep only B; collapsed 2.
	rows := []CheckpointRow{
		{CheckpointID: "A", SessionID: "s", CheckpointTranscriptStart: 0, Usage: mtok(2.0), CreatedAt: at(1)},
		{CheckpointID: "D", SessionID: "s", CheckpointTranscriptStart: 900, Usage: mtok(0.5), CreatedAt: at(2)},
		{CheckpointID: "B", SessionID: "s", CheckpointTranscriptStart: 0, Usage: mtok(5.5), CreatedAt: at(3)},
	}
	kept, collapsed := DedupeLegacyCheckpoints(rows)
	if collapsed != 2 {
		t.Errorf("collapsed %d, want 2", collapsed)
	}
	if len(kept) != 1 || kept[0].CheckpointID != "B" {
		t.Errorf("kept %+v, want [B]", ids(kept))
	}
}

func TestDedupe_V2RowsNeverCollapsed(t *testing.T) {
	t.Parallel()
	rows := []CheckpointRow{
		{CheckpointID: "X", SessionID: "s", Version: 2, CheckpointTranscriptStart: 0, Usage: mtok(1.0), CreatedAt: at(1)},
		{CheckpointID: "Y", SessionID: "s", Version: 2, CheckpointTranscriptStart: 0, Usage: mtok(1.0), CreatedAt: at(2)},
	}
	kept, collapsed := DedupeLegacyCheckpoints(rows)
	if collapsed != 0 {
		t.Errorf("collapsed %d, want 0", collapsed)
	}
	if len(kept) != 2 {
		t.Errorf("kept %+v, want both rows", ids(kept))
	}
}

func TestDedupe_SessionsAreIndependent(t *testing.T) {
	t.Parallel()
	rows := []CheckpointRow{
		{CheckpointID: "A1", SessionID: "s1", CheckpointTranscriptStart: 0, Usage: mtok(1.0), CreatedAt: at(1)},
		{CheckpointID: "B1", SessionID: "s1", CheckpointTranscriptStart: 0, Usage: mtok(2.0), CreatedAt: at(2)},
		{CheckpointID: "A2", SessionID: "s2", CheckpointTranscriptStart: 0, Usage: mtok(3.0), CreatedAt: at(1)},
		{CheckpointID: "B2", SessionID: "s2", CheckpointTranscriptStart: 0, Usage: mtok(4.0), CreatedAt: at(2)},
	}
	kept, collapsed := DedupeLegacyCheckpoints(rows)
	if collapsed != 2 {
		t.Errorf("collapsed %d, want 2", collapsed)
	}
	if len(kept) != 2 {
		t.Fatalf("kept %+v, want 2 rows", ids(kept))
	}
	gotIDs := map[string]bool{kept[0].CheckpointID: true, kept[1].CheckpointID: true}
	if !gotIDs["B1"] || !gotIDs["B2"] {
		t.Errorf("kept %+v, want [B1 B2]", ids(kept))
	}
}

func TestDedupe_DoesNotMutateInput(t *testing.T) {
	t.Parallel()
	rows := []CheckpointRow{
		{CheckpointID: "A", SessionID: "s", CheckpointTranscriptStart: 0, Usage: mtok(2.0), CreatedAt: at(1)},
		{CheckpointID: "B", SessionID: "s", CheckpointTranscriptStart: 0, Usage: mtok(5.5), CreatedAt: at(2)},
	}
	original := make([]CheckpointRow, len(rows))
	copy(original, rows)

	_, _ = DedupeLegacyCheckpoints(rows)

	if len(rows) != len(original) {
		t.Fatalf("input length changed: got %d, want %d", len(rows), len(original))
	}
	for i := range rows {
		if rows[i].CheckpointID != original[i].CheckpointID {
			t.Errorf("input mutated at index %d: got %q, want %q", i, rows[i].CheckpointID, original[i].CheckpointID)
		}
	}
}

func TestDedupe_DeterministicOutputOrder(t *testing.T) {
	t.Parallel()
	rows := []CheckpointRow{
		{CheckpointID: "B2", SessionID: "s2", CheckpointTranscriptStart: 0, Usage: mtok(4.0), CreatedAt: at(2)},
		{CheckpointID: "A1", SessionID: "s1", CheckpointTranscriptStart: 0, Usage: mtok(1.0), CreatedAt: at(1)},
		{CheckpointID: "B1", SessionID: "s1", CheckpointTranscriptStart: 0, Usage: mtok(2.0), CreatedAt: at(2)},
		{CheckpointID: "A2", SessionID: "s2", CheckpointTranscriptStart: 0, Usage: mtok(3.0), CreatedAt: at(1)},
	}

	kept1, _ := DedupeLegacyCheckpoints(rows)
	kept2, _ := DedupeLegacyCheckpoints(rows)

	// Order is SessionID ("s1" < "s2") then CreatedAt within each session.
	want := []string{"B1", "B2"}
	if got := ids(kept1); !slices.Equal(got, want) {
		t.Errorf("kept1 = %v, want %v", got, want)
	}
	if got := ids(kept2); !slices.Equal(got, want) {
		t.Errorf("kept2 = %v, want %v", got, want)
	}
}

func TestDedupe_EmptyInput(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		rows []CheckpointRow
	}{
		{"nil", nil},
		{"empty", []CheckpointRow{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			kept, collapsed := DedupeLegacyCheckpoints(tc.rows)
			if len(kept) != 0 {
				t.Errorf("kept %+v, want empty", kept)
			}
			if collapsed != 0 {
				t.Errorf("collapsed %d, want 0", collapsed)
			}
		})
	}
}

func TestDedupe_NilUsageRowsFollowSameRule(t *testing.T) {
	t.Parallel()
	rows := []CheckpointRow{
		{CheckpointID: "A", SessionID: "s", CheckpointTranscriptStart: 0, Usage: nil, CreatedAt: at(1)},
		{CheckpointID: "B", SessionID: "s", CheckpointTranscriptStart: 0, Usage: mtok(5.5), CreatedAt: at(2)},
		{CheckpointID: "C", SessionID: "s", CheckpointTranscriptStart: 4120, Usage: nil, CreatedAt: at(3)},
	}
	kept, collapsed := DedupeLegacyCheckpoints(rows)
	if collapsed != 1 {
		t.Errorf("collapsed %d, want 1", collapsed)
	}
	if len(kept) != 2 || kept[0].CheckpointID != "B" || kept[1].CheckpointID != "C" {
		t.Errorf("kept %+v, want [B C]", ids(kept))
	}
	if got := total(kept); got != 5_500_000 {
		t.Errorf("total %d, want 5500000", got)
	}
}
