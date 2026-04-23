package recap

import (
	"testing"
	"time"
)

func TestRecapSession_SpanMinutes(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 4, 15, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		name     string
		cps      []RecapCheckpoint
		wantMins float64
	}{
		{
			name:     "empty",
			cps:      nil,
			wantMins: 0,
		},
		{
			name: "single checkpoint",
			cps: []RecapCheckpoint{
				{CreatedAt: base},
			},
			wantMins: 0,
		},
		{
			name: "two checkpoints 15 minutes apart",
			cps: []RecapCheckpoint{
				{CreatedAt: base},
				{CreatedAt: base.Add(15 * time.Minute)},
			},
			wantMins: 15,
		},
		{
			name: "three checkpoints spanning an hour",
			cps: []RecapCheckpoint{
				{CreatedAt: base},
				{CreatedAt: base.Add(20 * time.Minute)},
				{CreatedAt: base.Add(60 * time.Minute)},
			},
			wantMins: 60,
		},
		{
			name: "unsorted checkpoints — order-agnostic",
			cps: []RecapCheckpoint{
				{CreatedAt: base.Add(30 * time.Minute)},
				{CreatedAt: base},
				{CreatedAt: base.Add(10 * time.Minute)},
			},
			wantMins: 30,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := RecapSession{Checkpoints: tc.cps}
			if got := s.SpanMinutes(); got != tc.wantMins {
				t.Errorf("SpanMinutes() = %v, want %v", got, tc.wantMins)
			}
		})
	}
}

func TestDataSource_String(t *testing.T) {
	t.Parallel()
	cases := []struct {
		src  DataSource
		want string
	}{
		{SourceLocal, "local"},
		{SourceServer, "server"},
		{SourceMixed, "mixed"},
		{DataSource(99), "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()
			if got := tc.src.String(); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
