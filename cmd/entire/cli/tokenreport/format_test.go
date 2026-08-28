package tokenreport

import (
	"testing"
	"time"
)

func TestFormatTokenCount(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   int
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1000, "1k"},
		{1500, "1.5k"},
		{100000, "100k"},
		{999_949, "999.9k"},
		{1_000_000, "1M"},
		{3_700_000, "3.7M"},
		{4_063_500, "4.1M"},
		{1_850_365, "1.9M"},
		{999_950, "1M"},
		{999_999, "1M"},
		{999_999_999, "1000M"}, // top of the M tier; no B tier yet
	}
	for _, c := range cases {
		if got := FormatTokenCount(c.in); got != c.want {
			t.Errorf("FormatTokenCount(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFormatPercent(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   float64
		want string
	}{
		{0, "0%"},
		{0.004, "<1%"},
		{0.0049, "<1%"},
		{0.005, "1%"},
		{0.369, "37%"},
		{1, "100%"},
		{-0.3, "0%"},
	}
	for _, c := range cases {
		if got := FormatPercent(c.in); got != c.want {
			t.Errorf("FormatPercent(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   time.Duration
		want string
	}{
		{42 * time.Second, "42s"},
		{6 * time.Minute, "6m"},
		{6*time.Minute + 30*time.Second, "6m"},
		{time.Hour + 5*time.Minute, "1h 05m"},
		{9*time.Hour + 42*time.Minute, "9h 42m"},
		{51 * time.Hour, "2d 3h"},
		{0, "0s"},
		{-2 * time.Hour, "0s"},
		{24 * time.Hour, "1d 0h"},
		{500 * time.Millisecond, "0s"},
	}
	for _, c := range cases {
		if got := FormatDuration(c.in); got != c.want {
			t.Errorf("FormatDuration(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}
