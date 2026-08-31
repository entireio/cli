package cli

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/search"
	"github.com/entireio/cli/cmd/entire/cli/telemetry"
)

// The selection ledgers (searchModel.semanticSelectionsReported /
// codeSelectionsReported) are the dedupe mechanism, so these tests assert on
// them directly. emitSearchSelection is gated on loaded settings and is a no-op
// under test, so calling reportSelection here exercises the dedupe without
// emitting anything.

// reportedCount is the total across both ledgers.
func (m searchModel) reportedCount() int {
	return len(m.semanticSelectionsReported) + len(m.codeSelectionsReported)
}

func TestReportSelection_DedupesRepeatedOpensOfSameRow(t *testing.T) {
	t.Parallel()
	var m searchModel

	m = m.reportSelection(telemetry.SearchModeCheckpoint, "commit", 2, 10)
	if m.reportedCount() != 1 {
		t.Fatalf("after first selection, ledger has %d entries, want 1", m.reportedCount())
	}

	// Opening the same row again (enter, esc, enter) is one act of selection.
	m = m.reportSelection(telemetry.SearchModeCheckpoint, "commit", 2, 10)
	if m.reportedCount() != 1 {
		t.Errorf("reopening the same row grew the ledger to %d entries, want 1", m.reportedCount())
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
	if m.reportedCount() != 4 {
		t.Errorf("ledger has %d entries after 4 distinct ranks, want 4", m.reportedCount())
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
	if m.reportedCount() != 2 {
		t.Errorf("ledger has %d entries, want 2 — tab must be part of the key", m.reportedCount())
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

	if m.reportedCount() != 2 {
		t.Errorf("ledger has %d entries, want 2 — rank 0 on Commits and rank 0 on Sessions are different results", m.reportedCount())
	}
}

func TestResetSemanticSelectionReporting_ClearsLedger(t *testing.T) {
	t.Parallel()
	m := searchModel{filterType: typeFilterCommits}

	m = m.reportSelection(telemetry.SearchModeCheckpoint, "commit", 0, 10)
	m = m.resetSemanticSelectionReporting()
	if m.reportedCount() != 0 {
		t.Fatalf("ledger has %d entries after reset, want 0", m.reportedCount())
	}

	// After a fresh result set, rank 0 is a different row and must report again.
	m = m.reportSelection(telemetry.SearchModeCheckpoint, "commit", 0, 10)
	if m.reportedCount() != 1 {
		t.Errorf("ledger has %d entries after re-selecting rank 0, want 1", m.reportedCount())
	}
}

// Regression: one query dispatches both a semantic and a code search, and
// either can land first. With a single shared ledger, the slower sibling's
// arrival cleared the keys of the set the user was actually looking at, so
// re-opening an unchanged row reported a second time. Each ledger must be
// reset only by its own set's refresh. Found by bugbot on this branch.
func TestSelectionLedgers_SiblingRefreshDoesNotClearTheOtherSet(t *testing.T) {
	t.Parallel()

	// User is on the Code tab and opens the top code result.
	m := searchModel{filterType: typeFilterCode}
	m = m.reportSelection(telemetry.SearchModeCode, telemetry.SearchSelectionTypeCode, 0, 5)
	if len(m.codeSelectionsReported) != 1 {
		t.Fatalf("code ledger has %d entries, want 1", len(m.codeSelectionsReported))
	}

	// The slower semantic response now lands. Code results are unchanged.
	m = m.resetSemanticSelectionReporting()
	if len(m.codeSelectionsReported) != 1 {
		t.Fatalf("semantic refresh cleared the code ledger (%d entries, want 1)", len(m.codeSelectionsReported))
	}

	// Re-opening that same unchanged code row must NOT report again.
	m = m.reportSelection(telemetry.SearchModeCode, telemetry.SearchSelectionTypeCode, 0, 5)
	if len(m.codeSelectionsReported) != 1 {
		t.Errorf("re-opening the same code row after a semantic refresh double-reported (%d entries, want 1)", len(m.codeSelectionsReported))
	}

	// And the mirror case: a new code search must not clear semantic keys.
	m2 := searchModel{filterType: typeFilterCommits}
	m2 = m2.reportSelection(telemetry.SearchModeCheckpoint, "commit", 0, 10)
	m2 = m2.resetCodeSelectionReporting()
	if len(m2.semanticSelectionsReported) != 1 {
		t.Errorf("code refresh cleared the semantic ledger (%d entries, want 1)", len(m2.semanticSelectionsReported))
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
