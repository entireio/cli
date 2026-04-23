package recap

import (
	"testing"
	"time"
)

func TestBuildRecapView_FiltersByRange(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 21, 15, 0, 0, 0, time.UTC)
	sessions := []RecapSession{
		{
			SessionID:       "today",
			StartedAt:       now.Add(-2 * time.Hour),
			LastInteraction: now.Add(-30 * time.Minute),
			AgentsUsed:      []string{"claude-code"},
			IsActive:        true,
			Repo:            "entireio/cli",
		},
		{
			SessionID:       "yesterday",
			StartedAt:       now.AddDate(0, 0, -1).Add(-2 * time.Hour),
			LastInteraction: now.AddDate(0, 0, -1).Add(-1 * time.Hour),
			AgentsUsed:      []string{"codex"},
			Repo:            "entireio/cli",
		},
	}
	view := BuildView(sessions, BuildOpts{Range: RangeDay, Now: now})
	if view.Summary.SessionCount != 1 {
		t.Errorf("SessionCount = %d, want 1", view.Summary.SessionCount)
	}
	if len(view.Sessions) != 1 || view.Sessions[0].Agent != "claude-code" {
		t.Errorf("expected today's session only, got %+v", view.Sessions)
	}
}

func TestBuildRecapView_AgentFilterNarrows(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 21, 15, 0, 0, 0, time.UTC)
	sessions := []RecapSession{
		{SessionID: "a", StartedAt: now.Add(-1 * time.Hour), LastInteraction: now, AgentsUsed: []string{"claude-code"}},
		{SessionID: "b", StartedAt: now.Add(-1 * time.Hour), LastInteraction: now, AgentsUsed: []string{"codex"}},
	}
	view := BuildView(sessions, BuildOpts{Range: RangeDay, AgentFilter: "codex", Now: now})
	if view.Summary.SessionCount != 1 {
		t.Errorf("SessionCount = %d, want 1", view.Summary.SessionCount)
	}
	if view.Summary.AgentFilter != "codex" {
		t.Errorf("AgentFilter = %q, want codex", view.Summary.AgentFilter)
	}
}

func TestBuildRecapView_HidesReposWhenSingleRepo(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 21, 15, 0, 0, 0, time.UTC)
	sessions := []RecapSession{
		{SessionID: "a", StartedAt: now.Add(-1 * time.Hour), LastInteraction: now, Repo: "entireio/cli"},
		{SessionID: "b", StartedAt: now.Add(-2 * time.Hour), LastInteraction: now, Repo: "entireio/cli"},
	}
	view := BuildView(sessions, BuildOpts{Range: RangeDay, Now: now})
	if view.Repos != nil {
		t.Errorf("single-repo → Repos should be nil, got %v", view.Repos)
	}
}

func TestBuildRecapView_ShowsReposWhenMultiple(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 21, 15, 0, 0, 0, time.UTC)
	sessions := []RecapSession{
		{SessionID: "a", StartedAt: now.Add(-1 * time.Hour), LastInteraction: now, Repo: "entireio/cli"},
		{SessionID: "b", StartedAt: now.Add(-2 * time.Hour), LastInteraction: now, Repo: "entireio/ent.io"},
	}
	view := BuildView(sessions, BuildOpts{Range: RangeDay, Now: now})
	if len(view.Repos) != 2 {
		t.Errorf("multi-repo → len(Repos) = %d, want 2: %v", len(view.Repos), view.Repos)
	}
}

func TestBuildRecapView_ActivityBucketsForDay(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 21, 23, 0, 0, 0, time.UTC)
	// Checkpoint at 10am today.
	cp := RecapCheckpoint{
		CreatedAt: time.Date(2026, 4, 21, 10, 0, 0, 0, time.UTC),
		Labels:    []string{"bug_fix"},
	}
	sessions := []RecapSession{{
		StartedAt:       now.Add(-1 * time.Hour),
		LastInteraction: now,
		Checkpoints:     []RecapCheckpoint{cp},
	}}
	view := BuildView(sessions, BuildOpts{Range: RangeDay, Now: now})
	if len(view.Activity) != 24 {
		t.Fatalf("Day activity should have 24 hourly buckets, got %d", len(view.Activity))
	}
	if view.Activity[10] != 1 {
		t.Errorf("expected 1 event at hour 10, got %d", view.Activity[10])
	}
}

func TestRangeBounds_DayEqualsMidnightToMidnight(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 21, 15, 30, 0, 0, time.UTC)
	start, end := RangeDay.Bounds(now)
	wantStart := time.Date(2026, 4, 21, 0, 0, 0, 0, time.UTC)
	wantEnd := time.Date(2026, 4, 22, 0, 0, 0, 0, time.UTC)
	if !start.Equal(wantStart) || !end.Equal(wantEnd) {
		t.Errorf("Day bounds = [%v, %v), want [%v, %v)", start, end, wantStart, wantEnd)
	}
}

func TestRangeBounds_MonthIsCalendarAligned(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 21, 15, 0, 0, 0, time.UTC)
	start, end := RangeMonth.Bounds(now)
	wantStart := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	wantEnd := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	if !start.Equal(wantStart) || !end.Equal(wantEnd) {
		t.Errorf("Month bounds = [%v, %v), want [%v, %v)", start, end, wantStart, wantEnd)
	}
}
