package dispatch

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	goDurationPattern  = regexp.MustCompile(`^(\d+)([smhdw])$`)
	relativePattern    = regexp.MustCompile(`^(\d+)\s*(second|minute|hour|day|week|month)s?(?:\s*ago)?$`)
	lastWeekdayPattern = regexp.MustCompile(`^last\s+(monday|tuesday|wednesday|thursday|friday|saturday|sunday)$`)
)

// ParseSinceAtNow parses durations like 7d, relative times like "2 days ago",
// RFC3339 timestamps, and ISO dates.
func ParseSinceAtNow(value string, now time.Time) (time.Time, error) {
	parsed, err := parseTimeAtNow(value, now)
	if err != nil {
		return time.Time{}, err
	}
	if parsed.IsZero() {
		return time.Time{}, fmt.Errorf("unparseable --since: %q", strings.TrimSpace(value))
	}
	return parsed, nil
}

func ParseUntilAtNow(value string, now time.Time) (time.Time, error) {
	parsed, err := parseTimeAtNow(value, now)
	if err != nil {
		return time.Time{}, err
	}
	if parsed.IsZero() {
		return now, nil
	}
	return parsed, nil
}

func parseTimeAtNow(value string, now time.Time) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	if strings.EqualFold(value, "now") {
		return now, nil
	}

	if matches := goDurationPattern.FindStringSubmatch(value); matches != nil {
		n, err := strconv.Atoi(matches[1])
		if err != nil {
			return time.Time{}, fmt.Errorf("parse duration count: %w", err)
		}
		switch matches[2] {
		case "d":
			return now.Add(-time.Duration(n) * 24 * time.Hour), nil
		case "w":
			return now.Add(-time.Duration(n) * 7 * 24 * time.Hour), nil
		default:
			d, err := time.ParseDuration(matches[1] + matches[2])
			if err != nil {
				return time.Time{}, fmt.Errorf("parse duration: %w", err)
			}
			return now.Add(-d), nil
		}
	}

	if matches := relativePattern.FindStringSubmatch(strings.ToLower(value)); matches != nil {
		n, err := strconv.Atoi(matches[1])
		if err != nil {
			return time.Time{}, fmt.Errorf("parse relative count: %w", err)
		}
		var unit time.Duration
		switch matches[2] {
		case "second":
			unit = time.Second
		case "minute":
			unit = time.Minute
		case "hour":
			unit = time.Hour
		case "day":
			unit = 24 * time.Hour
		case "week":
			unit = 7 * 24 * time.Hour
		case "month":
			unit = 30 * 24 * time.Hour
		}
		return now.Add(-time.Duration(n) * unit), nil
	}

	if matches := lastWeekdayPattern.FindStringSubmatch(strings.ToLower(value)); matches != nil {
		target := weekdayFromString(matches[1])
		if target >= 0 {
			diff := (int(now.Weekday()) - int(target) + 7) % 7
			if diff == 0 {
				diff = 7
			}
			return now.Add(-time.Duration(diff) * 24 * time.Hour), nil
		}
	}

	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed, nil
	}
	if parsed, err := time.Parse("2006-01-02", value); err == nil {
		return parsed, nil
	}

	return time.Time{}, fmt.Errorf("unparseable time: %q", value)
}

func weekdayFromString(value string) time.Weekday {
	switch value {
	case "sunday":
		return time.Sunday
	case "monday":
		return time.Monday
	case "tuesday":
		return time.Tuesday
	case "wednesday":
		return time.Wednesday
	case "thursday":
		return time.Thursday
	case "friday":
		return time.Friday
	case "saturday":
		return time.Saturday
	default:
		return -1
	}
}
