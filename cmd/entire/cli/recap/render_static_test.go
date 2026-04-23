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

	// Each of the three panel concepts should appear in the rendered output.
	// New summary shape: you/team rows + top line (no "Top agent" label row).
	for _, want := range []string{"Today", "you", "claude-code", "Activity", "Agents", "bug_fix"} {
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
		{SessionID: "a", StartedAt: now.Add(-1 * time.Hour), LastInteraction: now, AgentsUsed: []string{"claude-code"}, Repo: "entireio/cli",
			Checkpoints: []RecapCheckpoint{{CreatedAt: now.Add(-30 * time.Minute), LinkedCommit: "abc"}}},
		{SessionID: "b", StartedAt: now.Add(-2 * time.Hour), LastInteraction: now, AgentsUsed: []string{"claude-code"}, Repo: "entireio/ent.io",
			Checkpoints: []RecapCheckpoint{{CreatedAt: now.Add(-90 * time.Minute), LinkedCommit: "def"}}},
	}
	view := BuildView(sessions, BuildOpts{Range: RangeDay, Now: now})
	out := RenderStatic(view, NewStyles(false), 100)
	// Repos now live inside agent cards (your repos rows), not a bottom panel.
	if !strings.Contains(out, "entireio/cli") || !strings.Contains(out, "entireio/ent.io") {
		t.Errorf("multi-repo view missing repo names:\n%s", out)
	}
}

func TestRenderStatic_NoBottomPanel(t *testing.T) {
	t.Parallel()
	view := View{
		Range:      Range90d,
		Title:      "Last 90 days",
		Summary:    SummaryBand{RangeLabel: "Last 90 days"},
		Activity:   make([]int, 90),
		AgentCards: []AgentCard{{Agent: "Claude Code", MeSessions: 1}},
		Worktrees:  []WorktreeRollup{{WorktreeID: "main", SessionCount: 1}},
	}
	got := RenderStatic(view, NewStyles(false), 100)
	if strings.Contains(got, "Worktrees") {
		t.Error("Worktrees section should be dropped from static output")
	}
	if strings.Contains(got, "Labels") && !strings.Contains(got, "team labels") {
		t.Error("standalone Labels section should be dropped (team labels stays)")
	}
}
