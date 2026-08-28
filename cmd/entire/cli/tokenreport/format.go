package tokenreport

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// FormatTokenCount formats a token count for display, abbreviating with a
// "k" tier at 1,000 and an "M" tier at 1,000,000. Values below 1,000 render
// as-is; values at or above 1,000 render with one decimal place, trimming a
// trailing ".0" for clean display (e.g., 1000 → "1k" not "1.0k",
// 1_000_000 → "1M" not "1.0M").
func FormatTokenCount(n int) string {
	switch {
	case n < 1000:
		return strconv.Itoa(n)
	case n < 1_000_000:
		return trimTrailingZero(float64(n)/1000.0) + "k"
	default:
		return trimTrailingZero(float64(n)/1_000_000.0) + "M"
	}
}

// trimTrailingZero formats f with one decimal place, stripping a trailing
// ".0" (e.g., 1.0 → "1", 3.7 → "3.7").
func trimTrailingZero(f float64) string {
	s := fmt.Sprintf("%.1f", f)
	return strings.TrimSuffix(s, ".0")
}

// FormatPercent formats a share (0..1) as an integer percentage for display.
// A zero share renders as "0%"; a nonzero share below 0.5% renders as "<1%"
// rather than rounding down to "0%"; all other values round to the nearest
// whole percent.
func FormatPercent(share float64) string {
	switch {
	case share == 0:
		return "0%"
	case share < 0.005:
		return "<1%"
	default:
		return fmt.Sprintf("%d%%", int(math.Round(share*100)))
	}
}

// FormatDuration formats a duration for display at a granularity that grows
// coarser as the duration grows: seconds under a minute ("42s"), whole
// minutes under an hour ("6m"), hours and zero-padded minutes under a day
// ("1h 05m"), and days and hours beyond that ("2d 3h").
func FormatDuration(d time.Duration) string {
	totalSeconds := int(d.Seconds())

	if totalSeconds < 60 {
		return fmt.Sprintf("%ds", totalSeconds)
	}

	minutes := totalSeconds / 60
	if minutes < 60 {
		return fmt.Sprintf("%dm", minutes)
	}

	hours := totalSeconds / 3600
	if hours < 24 {
		remMinutes := (totalSeconds % 3600) / 60
		return fmt.Sprintf("%dh %02dm", hours, remMinutes)
	}

	days := hours / 24
	remHours := hours % 24
	return fmt.Sprintf("%dd %dh", days, remHours)
}
