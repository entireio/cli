package recap

import "time"

// RangeKey names the static recap windows supported by `entire recap`.
type RangeKey string

const (
	RangeDay   RangeKey = "day"
	RangeWeek  RangeKey = "week"
	RangeMonth RangeKey = "month"
	Range90d   RangeKey = "90d"
)

// Title returns the panel title for a range.
func (r RangeKey) Title() string {
	switch r {
	case RangeDay:
		return "Today"
	case RangeWeek:
		return "Last 7 days"
	case RangeMonth:
		return "This month"
	case Range90d:
		return "Last 90 days"
	default:
		return "Today"
	}
}

// Bounds returns a half-open [start, end) window in the user's local time.
func (r RangeKey) Bounds(now time.Time) (time.Time, time.Time) {
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	dayEnd := dayStart.AddDate(0, 0, 1)
	switch r {
	case RangeDay:
		return dayStart, dayEnd
	case RangeWeek:
		return dayEnd.AddDate(0, 0, -7), dayEnd
	case RangeMonth:
		monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
		return monthStart, monthStart.AddDate(0, 1, 0)
	case Range90d:
		return dayEnd.AddDate(0, 0, -90), dayEnd
	default:
		return dayStart, dayEnd
	}
}

// ViewMode selects which columns render in the static agents panel.
type ViewMode string

const (
	ViewYou  ViewMode = "you"
	ViewTeam ViewMode = "team"
	ViewBoth ViewMode = "both"
)

// Valid reports whether the mode is one of the supported static modes.
func (m ViewMode) Valid() bool {
	return m == ViewYou || m == ViewTeam || m == ViewBoth
}
