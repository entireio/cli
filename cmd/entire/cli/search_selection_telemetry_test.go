package cli

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/search"
	"github.com/entireio/cli/cmd/entire/cli/telemetry"
)

// The selection ledger (searchModel.selectionsReported) is the dedupe
// mechanism, so these tests assert on it directly. emitSearchSelection is gated
// on loaded settings and is a no-op under test, so calling reportSelection here
// exercises the dedupe without emitting anything.

func TestReportSelection_DedupesRepeatedOpensOfSameRow(t *testing.T) {
	t.Parallel()
	var m searchModel

	m = m.reportSelection(telemetry.SearchModeCheckpoint, "commit", 2, 10)
	if len(m.selectionsReported) != 1 {
		t.Fatalf("after first selection, ledger has %d entries, want 1", len(m.selectionsReported))
	}

	// Opening the same row again (enter, esc, enter) is one act of selection.
	m = m.reportSelection(telemetry.SearchModeCheckpoint, "commit", 2, 10)
	if len(m.selectionsReported) != 1 {
		t.Errorf("reopening the same row grew the ledger to %d entries, want 1", len(m.selectionsReported))
	}
}

func TestReportSelection_DistinctRanksEachReport(t *testing.T) {
	t.Parallel()
	var m searchModel

	// Digging through four results before finding the right one is itself the
	// relevance signal, so each distinct rank must report.
	for rank := range 4 {
		m = m.reportSelection(telemetry.SearchModeCheckpoint, "session", rank, 12)
	}
	if len(m.selectionsReported) != 4 {
		t.Errorf("ledger has %d entries after 4 distinct ranks, want 4", len(m.selectionsReported))
	}
}

// The ledger is keyed by tab as well as rank: rank 0 on the Code tab is a
// different result from rank 0 on the Commits tab.
func TestReportSelection_TabsDoNotCollide(t *testing.T) {
	t.Parallel()
	m := searchModel{filterType: typeFilterCommits}

	m = m.reportSelection(telemetry.SearchModeCheckpoint, "commit", 0, 10)
	m = m.switchTab(typeFilterCode)
	m = m.reportSelection(telemetry.SearchModeCode, telemetry.SearchSelectionTypeCode, 0, 3)
	if len(m.selectionsReported) != 2 {
		t.Errorf("ledger has %d entries, want 2 — tab must be part of the key", len(m.selectionsReported))
	}
}

// Regression: Commits and Sessions both report mode "checkpoint", and
// switchTab resets the cursor to 0 without clearing the ledger. Keying the
// ledger on the reported mode therefore swallowed rank 0 on whichever of the
// two tabs the user visited second — an ordinary navigation flow, silently
// undercounted. Found by trail review on this branch.
func TestReportSelection_SameModeDifferentTabsBothReport(t *testing.T) {
	t.Parallel()
	m := searchModel{filterType: typeFilterCommits}

	m = m.reportSelection(telemetry.SearchModeCheckpoint, "commit", 0, 10)
	m = m.switchTab(typeFilterSessions)
	if m.cursor != 0 {
		t.Fatalf("switchTab left cursor at %d, want 0 — the premise of this test", m.cursor)
	}
	m = m.reportSelection(telemetry.SearchModeCheckpoint, "session", 0, 10)

	if len(m.selectionsReported) != 2 {
		t.Errorf("ledger has %d entries, want 2 — rank 0 on Commits and rank 0 on Sessions are different results", len(m.selectionsReported))
	}
}

func TestResetSelectionReporting_ClearsLedger(t *testing.T) {
	t.Parallel()
	var m searchModel

	m = m.reportSelection(telemetry.SearchModeCheckpoint, "commit", 0, 10)
	m = m.resetSelectionReporting()
	if len(m.selectionsReported) != 0 {
		t.Fatalf("ledger has %d entries after reset, want 0", len(m.selectionsReported))
	}

	// After a fresh result set, rank 0 is a different row and must report again.
	m = m.reportSelection(telemetry.SearchModeCheckpoint, "commit", 0, 10)
	if len(m.selectionsReported) != 1 {
		t.Errorf("ledger has %d entries after re-selecting rank 0, want 1", len(m.selectionsReported))
	}
}

// searchSelectionResultType must pass through every type the search API
// models and clamp anything else, since Result.UnmarshalJSON assigns
// server-supplied types verbatim.
func TestSearchSelectionResultType(t *testing.T) {
	t.Parallel()
	for _, known := range []string{
		search.TypeCheckpoint, search.TypeCommit, search.TypeSession,
		search.TypeRepo, search.TypePR,
	} {
		if got := searchSelectionResultType(known); got != known {
			t.Errorf("searchSelectionResultType(%q) = %q, want passthrough", known, got)
		}
	}

	for _, unknown := range []string{
		"",
		"some-future-type",
		// The failure this guards against: a server-supplied string that is
		// content rather than a type discriminator.
		"fix the login bug in auth.go",
	} {
		if got := searchSelectionResultType(unknown); got != telemetry.SearchSelectionTypeOther {
			t.Errorf("searchSelectionResultType(%q) = %q, want %q", unknown, got, telemetry.SearchSelectionTypeOther)
		}
	}
}
