package dispatch

import (
	"testing"
	"time"
)

func TestParseSince_GoDuration(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 4, 16, 14, 32, 0, 0, time.UTC)
	got, err := ParseSinceAtNow("7d", now)
	if err != nil {
		t.Fatal(err)
	}
	want := now.Add(-7 * 24 * time.Hour)
	if !got.Equal(want) {
		t.Fatalf("want %v got %v", want, got)
	}
}

func TestParseSince_GitStyle(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 4, 16, 12, 0, 0, 0, time.UTC)
	got, err := ParseSinceAtNow("2 days ago", now)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(now.Add(-48 * time.Hour)) {
		t.Fatalf("got %v", got)
	}
}

func TestParseSince_GitStyleWithoutAgo(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 4, 16, 12, 0, 0, 0, time.UTC)
	got, err := ParseSinceAtNow("1 week", now)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(now.Add(-7 * 24 * time.Hour)) {
		t.Fatalf("got %v", got)
	}
}

func TestParseSince_LastWeekday(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 4, 16, 12, 0, 0, 0, time.UTC)
	got, err := ParseSinceAtNow("last monday", now)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 4, 13, 12, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("want %v got %v", want, got)
	}
}

func TestParseSince_ISO(t *testing.T) {
	t.Parallel()

	got, err := ParseSinceAtNow("2026-04-09", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got.Year() != 2026 || got.Month() != 4 || got.Day() != 9 {
		t.Fatalf("got %v", got)
	}
}

func TestParseUntil_EmptyDefaultsToNow(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 4, 16, 14, 32, 0, 0, time.UTC)
	got, err := ParseUntilAtNow("", now)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(now) {
		t.Fatalf("want %v got %v", now, got)
	}
}
