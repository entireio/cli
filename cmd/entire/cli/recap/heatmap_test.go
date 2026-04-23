package recap

import (
	"testing"
	"time"
)

func TestBuildHeatmap(t *testing.T) {
	t.Parallel()
	tz, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	from := time.Date(2026, 4, 13, 0, 0, 0, 0, tz)
	to := time.Date(2026, 4, 15, 23, 59, 59, 0, tz)

	sessions := []RecapSession{
		{Checkpoints: []RecapCheckpoint{
			{CreatedAt: time.Date(2026, 4, 13, 10, 30, 0, 0, tz)},
			{CreatedAt: time.Date(2026, 4, 13, 11, 0, 0, 0, tz)},
		}},
		{Checkpoints: []RecapCheckpoint{
			{CreatedAt: time.Date(2026, 4, 14, 15, 0, 0, 0, tz)},
		}},
	}

	cells := BuildHeatmap(sessions, from, to, MetricCheckpoints, tz)
	if len(cells) != 3 { // Apr 13, 14, 15
		t.Fatalf("expected 3 cells, got %d", len(cells))
	}
	if cells[0].Value != 2 {
		t.Errorf("day 1 expected 2, got %d", cells[0].Value)
	}
	if cells[1].Value != 1 {
		t.Errorf("day 2 expected 1, got %d", cells[1].Value)
	}
	if cells[2].Value != 0 {
		t.Errorf("day 3 expected 0, got %d", cells[2].Value)
	}
}

func TestHeatmapIntensity(t *testing.T) {
	t.Parallel()
	cases := []struct {
		value, peak int
		want        int // 0..4
	}{
		{0, 10, 0},
		{1, 10, 1},
		{3, 10, 1},
		{4, 10, 2},
		{7, 10, 3},
		{10, 10, 4},
	}
	for _, tc := range cases {
		if got := HeatmapIntensity(tc.value, tc.peak); got != tc.want {
			t.Errorf("HeatmapIntensity(%d, %d) = %d, want %d", tc.value, tc.peak, got, tc.want)
		}
	}
}
