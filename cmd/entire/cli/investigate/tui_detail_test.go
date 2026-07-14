package investigate

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

func TestDetailView_EmptyBuffer(t *testing.T) {
	t.Parallel()
	row := agentRow{name: "claude-code"}
	out := detailView(row, 0, 40, 5)
	require.Equal(t, 5, strings.Count(out, "\n")+1, "must be exactly termHeight lines")
	require.Contains(t, out, "claude-code")
}

func TestDetailView_SingleTurn(t *testing.T) {
	t.Parallel()
	row := agentRow{
		name: "codex",
		buffer: []timelineEntry{
			{turn: 1, kind: "finished", stance: stanceApprove, duration: 2 * time.Second, findings: "ok"},
		},
	}
	out := detailView(row, 0, 60, 5)
	require.Contains(t, out, "codex")
	require.Contains(t, out, "approve")
	require.Equal(t, 5, strings.Count(out, "\n")+1)
}

func TestDetailView_NarrowWidthTruncates(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 200)
	row := agentRow{
		name: "codex",
		buffer: []timelineEntry{
			{turn: 1, kind: "finished", stance: stanceApprove, findings: long},
		},
	}
	out := detailView(row, 0, 20, 5)
	for _, line := range strings.Split(out, "\n") {
		// Compare display cell width — header chrome ("───") is multi-byte UTF-8,
		// so len() would over-count. The renderer guarantees cell-width, not bytes.
		require.LessOrEqual(t, ansi.StringWidth(line), 20, "no line may exceed termWidth")
	}
}

func TestDetailView_ScrollClampedToBufferEnd(t *testing.T) {
	t.Parallel()
	row := agentRow{
		name: "codex",
		buffer: []timelineEntry{
			{turn: 1, kind: "started"},
			{turn: 1, kind: "finished", stance: stanceApprove},
		},
	}
	out := detailView(row, 9999, 40, 5)
	require.Equal(t, 5, strings.Count(out, "\n")+1)
}
