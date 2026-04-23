package recap

import (
	"strings"
	"testing"
)

func TestRenderActivityStrip_ZeroDaysRenderAsDarkCells(t *testing.T) {
	t.Parallel()
	view := View{
		Range:    Range90d,
		Activity: make([]int, 90), // all zero
	}
	got := renderActivityStrip(view, NewStyles(false))
	// Every cell must be a ░ or similar darkest glyph; none should be skipped.
	if n := countRunes(got, '░'); n < 80 {
		t.Errorf("expected ≥80 dark cells for all-zero 90-day range; got %d in\n%s", n, got)
	}
}

func TestRenderActivityStrip_PeakAnnotationRenders(t *testing.T) {
	t.Parallel()
	act := make([]int, 90)
	act[80] = 10 // peak 10 days ago
	view := View{
		Range:    Range90d,
		Activity: act,
	}
	got := renderActivityStrip(view, NewStyles(false))
	if !strings.Contains(got, "most active") {
		t.Errorf("expected 'most active' annotation; got\n%s", got)
	}
}

func TestRenderActivityStrip_NoPeakWhenAllZero(t *testing.T) {
	t.Parallel()
	view := View{
		Range:    Range90d,
		Activity: make([]int, 90),
	}
	got := renderActivityStrip(view, NewStyles(false))
	if strings.Contains(got, "most active") {
		t.Errorf("expected no 'most active' when all zero; got\n%s", got)
	}
}

func TestRenderActivityStrip_DayRangePeakShowsHour(t *testing.T) {
	t.Parallel()
	hours := [24]int{}
	hours[14] = 5
	view := View{
		Range:    RangeDay,
		Activity: hours[:],
	}
	got := renderActivityStrip(view, NewStyles(false))
	if !strings.Contains(got, "14:00") {
		t.Errorf("expected hourly peak '14:00'; got\n%s", got)
	}
}
