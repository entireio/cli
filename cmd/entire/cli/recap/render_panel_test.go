package recap

import (
	"strings"
	"testing"
)

func TestRenderPanel_IncludesTitleAndBody(t *testing.T) {
	t.Parallel()
	out := renderPanel("Today", "hello world", 40, NewStyles(false))
	if !strings.Contains(out, "Today") {
		t.Errorf("panel missing title: %q", out)
	}
	if !strings.Contains(out, "hello world") {
		t.Errorf("panel missing body: %q", out)
	}
}

func TestRenderPanel_NoTitleSkipsHeaderBlock(t *testing.T) {
	t.Parallel()
	out := renderPanel("", "just body", 40, NewStyles(false))
	if !strings.Contains(out, "just body") {
		t.Errorf("panel missing body: %q", out)
	}
}

func TestRenderPanel_RespectsMinWidth(t *testing.T) {
	t.Parallel()
	// Width of 5 is below the 10-cell floor; should render without panic.
	out := renderPanel("t", "b", 5, NewStyles(false))
	if out == "" {
		t.Error("panel returned empty for narrow width")
	}
}
