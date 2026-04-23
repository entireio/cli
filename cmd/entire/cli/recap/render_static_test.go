package recap

import (
	"strings"
	"testing"
	"time"
)

func TestRenderStatic_ProducesAllFourPanels(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 21, 15, 0, 0, 0, time.UTC)
	sessions := []RecapSession{
		{
			SessionID:       "s1",
			StartedAt:       now.Add(-2 * time.Hour),
			LastInteraction: now,
			AgentsUsed:      []string{"claude-code"},
			ModelsUsed:      []string{"opus-4-7"},
			Repo:            "entireio/cli",
			WorktreeID:      "main",
			WorktreePath:    "/repo",
			IsActive:        true,
			Checkpoints: []RecapCheckpoint{
				{CreatedAt: now.Add(-1 * time.Hour), Labels: []string{"bug_fix"}, LinkedCommit: "abc"},
			},
		},
	}
	view := BuildView(sessions, BuildOpts{Range: RangeDay, Now: now})
	styles := NewStyles(false) // plain mode for predictable output

	out := RenderStatic(view, styles, 100)

	// Each of the four panel concepts should appear in the rendered output.
	for _, want := range []string{"Today", "Top agent", "claude-code", "Activity", "Sessions", "bug_fix"} {
		if !strings.Contains(out, want) {
			t.Errorf("static output missing %q:\n%s", want, out)
		}
	}
}

func TestRenderStatic_EmptyRangeShowsPlaceholders(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 21, 15, 0, 0, 0, time.UTC)
	view := BuildView(nil, BuildOpts{Range: RangeDay, Now: now})
	out := RenderStatic(view, NewStyles(false), 100)
	if !strings.Contains(out, "no sessions") {
		t.Errorf("empty view should surface 'no sessions' placeholder:\n%s", out)
	}
}

func TestRenderStatic_AccessibleModeHasNoANSI(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 21, 15, 0, 0, 0, time.UTC)
	sessions := []RecapSession{
		{
			SessionID: "s1", StartedAt: now.Add(-1 * time.Hour), LastInteraction: now,
			AgentsUsed: []string{"claude-code"},
			Repo:       "entireio/cli",
			Checkpoints: []RecapCheckpoint{
				{CreatedAt: now.Add(-30 * time.Minute), Labels: []string{"refactor"}, LinkedCommit: "abc"},
			},
		},
	}
	view := BuildView(sessions, BuildOpts{Range: RangeDay, Now: now})
	out := RenderStatic(view, NewStyles(false), 100)
	if strings.ContainsRune(out, '\x1b') {
		t.Errorf("accessible mode leaked ANSI escape in output:\n%q", out)
	}
}

func TestRenderStatic_IncludesRepoColumnWhenMultiRepo(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 21, 15, 0, 0, 0, time.UTC)
	sessions := []RecapSession{
		{SessionID: "a", StartedAt: now.Add(-1 * time.Hour), LastInteraction: now, Repo: "entireio/cli"},
		{SessionID: "b", StartedAt: now.Add(-2 * time.Hour), LastInteraction: now, Repo: "entireio/ent.io"},
	}
	view := BuildView(sessions, BuildOpts{Range: RangeDay, Now: now})
	out := RenderStatic(view, NewStyles(false), 100)
	if !strings.Contains(out, "Repos") {
		t.Errorf("multi-repo view should include Repos column:\n%s", out)
	}
	if !strings.Contains(out, "entireio/cli") || !strings.Contains(out, "entireio/ent.io") {
		t.Errorf("multi-repo view missing repo names:\n%s", out)
	}
}
