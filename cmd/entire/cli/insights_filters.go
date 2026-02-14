package cli

import (
	"time"
)

// applyPeriodFilter converts a period string to start/end times.
// Supports "week", "month", "year".
// Returns zero times for empty/unknown periods.
func applyPeriodFilter(period string) (time.Time, time.Time) {
	now := time.Now()
	var start, end time.Time

	switch period {
	case "week":
		start = now.AddDate(0, 0, -7)
		end = now
	case "month":
		start = now.AddDate(0, -1, 0)
		end = now
	case "year":
		start = now.AddDate(-1, 0, 0)
		end = now
	case "":
		// No filter - use default week
		start = now.AddDate(0, 0, -7)
		end = now
	default:
		// Unknown period - no filter
		return time.Time{}, time.Time{}
	}

	return start, end
}
