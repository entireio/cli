package agentimport

import "time"

// Shared fixture constants used across the per-agent importer tests. They keep
// the common prompt/session literals in one place (and satisfy goconst).
const (
	fxFirst  = "first"
	fxSecond = "second"
	fxRecent = "recent"
)

// discoverCutoff is the lookback cutoff Run would compute for a given "now",
// which is what Importer.Discover takes. The per-agent Discover tests age
// their fixtures relative to the same now, so they keep asserting the same
// in-window / out-of-window split.
func discoverCutoff(now time.Time) time.Time {
	return now.AddDate(0, 0, -DefaultLookbackDays)
}
