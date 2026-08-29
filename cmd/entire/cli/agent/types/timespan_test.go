package types

import (
	"testing"
	"time"
)

func TestTimeSpan_Note(t *testing.T) {
	t.Parallel()

	t1 := time.Date(2026, time.August, 28, 10, 0, 1, 0, time.UTC)
	t0 := t1.Add(-time.Second)
	t2 := t1.Add(time.Second)

	var s TimeSpan
	s.Note(time.Time{})
	if !s.Start.IsZero() || !s.End.IsZero() {
		t.Fatalf("after a zero Note: %+v, want both ends zero", s)
	}
	s.Note(t1)
	if !s.Start.Equal(t1) || !s.End.Equal(t1) {
		t.Fatalf("after the first Note: %+v, want both ends %v", s, t1)
	}
	s.Note(t1) // the same instant again changes nothing
	s.Note(t2)
	if !s.Start.Equal(t1) || !s.End.Equal(t2) {
		t.Fatalf("after a later Note: %+v, want %v..%v", s, t1, t2)
	}
	s.Note(t0)
	if !s.Start.Equal(t0) || !s.End.Equal(t2) {
		t.Fatalf("after an earlier Note: %+v, want %v..%v", s, t0, t2)
	}
	s.Note(t1) // inside the span: no change
	s.Note(time.Time{})
	if !s.Start.Equal(t0) || !s.End.Equal(t2) {
		t.Fatalf("after an inner and a zero Note: %+v, want %v..%v unchanged", s, t0, t2)
	}
}

func TestParseTimestamp(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want time.Time
	}{
		{name: "empty", in: "", want: time.Time{}},
		{name: "garbage", in: "yesterday", want: time.Time{}},
		{name: "epoch millis are not RFC 3339", in: "1773867525000", want: time.Time{}},
		{name: "millis and Z", in: "2026-08-28T10:00:01.250Z", want: time.Date(2026, time.August, 28, 10, 0, 1, 250_000_000, time.UTC)},
		{name: "no fraction", in: "2026-08-28T10:00:01Z", want: time.Date(2026, time.August, 28, 10, 0, 1, 0, time.UTC)},
		{name: "numeric offset", in: "2026-08-28T12:00:01+02:00", want: time.Date(2026, time.August, 28, 10, 0, 1, 0, time.UTC)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := ParseTimestamp(tt.in); !got.Equal(tt.want) {
				t.Errorf("ParseTimestamp(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}
