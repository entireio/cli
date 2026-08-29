package types

import "time"

// TimeSpan is the earliest and latest timestamp noted so far; both zero until
// the first Note. The attributors embed it to derive Attribution.Start/End
// from the rows they admit.
type TimeSpan struct {
	Start, End time.Time
}

// Note widens the span to include at: the first non-zero at sets both ends,
// a later one moves whichever end it lies beyond. A zero at is ignored.
func (s *TimeSpan) Note(at time.Time) {
	if at.IsZero() {
		return
	}
	if s.Start.IsZero() || at.Before(s.Start) {
		s.Start = at
	}
	if s.End.IsZero() || at.After(s.End) {
		s.End = at
	}
}

// ParseTimestamp parses the RFC 3339 timestamp Claude Code, Codex and Pi write
// on each row and Gemini CLI on each message — with or without fractional
// seconds, "Z" or a numeric offset; zero when s is empty or malformed.
func ParseTimestamp(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	at, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return at
}
