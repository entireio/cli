package recap

import (
	"strings"
	"testing"
)

// countRunes counts how many of the given runes appear in s. Tests use
// NewStyles(false) so ANSI escapes are absent — plain rune count is accurate.
func countRunes(s string, r rune) int {
	n := 0
	for _, c := range s {
		if c == r {
			n++
		}
	}
	return n
}

func TestRenderComparisonBar_YouGreater(t *testing.T) {
	t.Parallel()
	// you=100, team=25, width=20 → amber fills 15 (20 - 5 team overlay), team overlays 5.
	got := renderComparisonBar(100, 25, 20, NewStyles(false))
	if n := countRunes(got, '█'); n != 15 {
		t.Errorf("expected 15 amber █ cells (20 - 5 team overlay); got %d in %q", n, got)
	}
	if n := countRunes(got, '▒'); n != 5 {
		t.Errorf("expected 5 magenta ▒ cells; got %d in %q", n, got)
	}
}

func TestRenderComparisonBar_TeamGreater(t *testing.T) {
	t.Parallel()
	// you=25, team=100, width=20 → team fills 20 (█), you fills 5 at start.
	got := renderComparisonBar(25, 100, 20, NewStyles(false))
	// With no color, both sides render as █ (no distinguishing glyph).
	if n := countRunes(got, '█'); n != 20 {
		t.Errorf("expected 20 fill cells total; got %d in %q", n, got)
	}
}

func TestRenderComparisonBar_Equal(t *testing.T) {
	t.Parallel()
	// you=50, team=50, width=12 (barMinWidth) → striped (magenta ▒ on even, amber █ on odd).
	got := renderComparisonBar(50, 50, 12, NewStyles(false))
	amberCount := countRunes(got, '█')
	dotCount := countRunes(got, '▒')
	if amberCount != 6 || dotCount != 6 {
		t.Errorf("expected 6 █ + 6 ▒ striped; got amber=%d dot=%d in %q", amberCount, dotCount, got)
	}
}

func TestRenderComparisonBar_YouOnly(t *testing.T) {
	t.Parallel()
	// you=100, team=0 → full amber, no overlay.
	got := renderComparisonBar(100, 0, 20, NewStyles(false))
	if n := countRunes(got, '█'); n != 20 {
		t.Errorf("expected 20 █ cells; got %d in %q", n, got)
	}
	if n := countRunes(got, '▒'); n != 0 {
		t.Errorf("expected 0 ▒ cells; got %d in %q", n, got)
	}
}

func TestRenderComparisonBar_TeamOnly(t *testing.T) {
	t.Parallel()
	// you=0, team=100 → full magenta, no overlay.
	got := renderComparisonBar(0, 100, 20, NewStyles(false))
	if n := countRunes(got, '█'); n != 20 {
		t.Errorf("expected 20 █ cells (team fill); got %d in %q", n, got)
	}
}

func TestRenderComparisonBar_BothZero(t *testing.T) {
	t.Parallel()
	got := renderComparisonBar(0, 0, 20, NewStyles(false))
	if got != "" {
		t.Errorf("expected empty bar when both zero; got %q", got)
	}
}

func TestRenderComparisonBar_TinyYouHugeTeam(t *testing.T) {
	t.Parallel()
	// you=1, team=1000, width=20 → you gets 1 cell (minimum), team fills 20.
	got := renderComparisonBar(1, 1000, 20, NewStyles(false))
	if countRunes(got, '█') < 1 {
		t.Errorf("expected at least 1 cell for tiny you value; got %q", got)
	}
}

func TestRenderComparisonBar_NarrowClampsToMin(t *testing.T) {
	t.Parallel()
	// width=8 requested, but min is 12. Bar should return "" so caller
	// falls back to readout-only mode.
	got := renderComparisonBar(100, 50, 8, NewStyles(false))
	if got != "" {
		t.Errorf("expected empty when width < 12; got %q", got)
	}
}

func TestRenderComparisonBar_WideClampsToMax(t *testing.T) {
	t.Parallel()
	// width=200 requested, but max is 40.
	got := renderComparisonBar(100, 100, 200, NewStyles(false))
	total := countRunes(got, '█') + countRunes(got, '▒')
	if total != 40 {
		t.Errorf("expected 40 total cells when clamped; got %d in %q", total, got)
	}
}

// keep strings import used by countRunes
var _ = strings.Contains
