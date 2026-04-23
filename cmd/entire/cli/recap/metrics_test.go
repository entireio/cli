package recap

import (
	"reflect"
	"testing"
	"time"
)

func TestLabelCounts(t *testing.T) {
	t.Parallel()
	sessions := []RecapSession{
		{Checkpoints: []RecapCheckpoint{
			{Labels: []string{"feature_build"}},
			{Labels: []string{"feature_build", "testing"}},
		}},
		{Checkpoints: []RecapCheckpoint{
			{Labels: []string{"bug_fix"}},
		}},
	}
	got := LabelCounts(sessions)
	want := map[string]int{"feature_build": 2, "testing": 1, "bug_fix": 1}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("LabelCounts = %v, want %v", got, want)
	}
}

func TestDominantLabel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		counts  map[string]int
		wantLbl string
		wantOK  bool
	}{
		{
			name:    "clear winner",
			counts:  map[string]int{"feature_build": 8, "bug_fix": 2, "testing": 1},
			wantLbl: "feature_build",
			wantOK:  true,
		},
		{
			name:    "below share threshold",
			counts:  map[string]int{"feature_build": 6, "bug_fix": 3, "testing": 2},
			wantLbl: "",
			wantOK:  false,
		},
		{
			name:    "share ok but lead too small",
			counts:  map[string]int{"feature_build": 11, "bug_fix": 9},
			wantLbl: "",
			wantOK:  false,
		},
		{
			name:    "empty",
			counts:  map[string]int{},
			wantLbl: "",
			wantOK:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			lbl, ok := DominantLabel(tc.counts)
			if lbl != tc.wantLbl || ok != tc.wantOK {
				t.Errorf("DominantLabel(%v) = (%q, %v), want (%q, %v)",
					tc.counts, lbl, ok, tc.wantLbl, tc.wantOK)
			}
		})
	}
}

func TestAggregateRange(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 4, 15, 10, 0, 0, 0, time.UTC)
	sessions := []RecapSession{
		{
			Repo:      "entireio/cli",
			StartedAt: base,
			Checkpoints: []RecapCheckpoint{
				{CreatedAt: base, LinkedCommit: "abc1234"},
				{CreatedAt: base.Add(15 * time.Minute)},
			},
			LinkedCommits: []string{"abc1234"},
		},
		{
			Repo:      "entireio/cli",
			StartedAt: base.Add(2 * time.Hour),
			Checkpoints: []RecapCheckpoint{
				{CreatedAt: base.Add(2*time.Hour + 5*time.Minute), IsTask: true},
			},
		},
	}
	from := base.Add(-time.Hour)
	to := base.Add(3 * time.Hour)
	got := AggregateRange(sessions, from, to)
	if got.Sessions != 2 {
		t.Errorf("Sessions = %d, want 2", got.Sessions)
	}
	if got.Checkpoints != 3 {
		t.Errorf("Checkpoints = %d, want 3", got.Checkpoints)
	}
	if got.TaskCheckpoints != 1 {
		t.Errorf("TaskCheckpoints = %d, want 1", got.TaskCheckpoints)
	}
	if got.LinkedCommits != 1 {
		t.Errorf("LinkedCommits = %d, want 1", got.LinkedCommits)
	}
	if len(got.ReposTouched) != 1 || got.ReposTouched[0] != "entireio/cli" {
		t.Errorf("ReposTouched = %v, want [entireio/cli]", got.ReposTouched)
	}
}

func TestAggregateRange_InclusiveBoundaries(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 4, 15, 10, 0, 0, 0, time.UTC)
	sessions := []RecapSession{
		{Checkpoints: []RecapCheckpoint{
			{CreatedAt: base},                              // at from
			{CreatedAt: base.Add(time.Hour)},               // at to
			{CreatedAt: base.Add(-time.Minute)},            // just before from
			{CreatedAt: base.Add(time.Hour + time.Minute)}, // just after to
		}},
	}
	got := AggregateRange(sessions, base, base.Add(time.Hour))
	if got.Checkpoints != 2 {
		t.Errorf("expected 2 checkpoints inside [from,to], got %d", got.Checkpoints)
	}
}

func TestDominantLabel_ExactThresholds(t *testing.T) {
	t.Parallel()
	// Exactly 0.55 share: 55 top, 45 other = 100 total. Top share = 0.55.
	// Lead = 0.55 - 0.45 = 0.10, below 0.15 → should NOT qualify.
	counts := map[string]int{"a": 55, "b": 45}
	if _, ok := DominantLabel(counts); ok {
		t.Error("exactly-0.10 lead should not qualify")
	}

	// 0.55 share, 0.15 lead: 55 top, 40 other, 5 third = 100 total.
	// Top share = 0.55, lead vs runner-up (40) = 0.15 — at threshold.
	counts = map[string]int{"a": 55, "b": 40, "c": 5}
	if _, ok := DominantLabel(counts); !ok {
		t.Error("exactly-0.15 lead at 0.55 share should qualify (>= comparison)")
	}
}

func TestAggregateByDay(t *testing.T) {
	t.Parallel()
	tz := time.UTC
	sessions := []RecapSession{
		{Checkpoints: []RecapCheckpoint{
			{CreatedAt: time.Date(2026, 4, 13, 12, 0, 0, 0, tz)},
			{CreatedAt: time.Date(2026, 4, 13, 13, 0, 0, 0, tz)},
		}},
		{Checkpoints: []RecapCheckpoint{
			{CreatedAt: time.Date(2026, 4, 15, 9, 0, 0, 0, tz)},
		}},
	}
	from := time.Date(2026, 4, 13, 0, 0, 0, 0, tz)
	to := time.Date(2026, 4, 15, 23, 0, 0, 0, tz)
	days := AggregateByDay(sessions, from, to, tz)
	if len(days) != 3 {
		t.Fatalf("expected 3 days, got %d", len(days))
	}
	if days[0].Checkpoints != 2 {
		t.Errorf("day[0].Checkpoints = %d, want 2", days[0].Checkpoints)
	}
	if days[1].Checkpoints != 0 {
		t.Errorf("day[1] (gap) = %d, want 0", days[1].Checkpoints)
	}
	if days[2].Checkpoints != 1 {
		t.Errorf("day[2].Checkpoints = %d, want 1", days[2].Checkpoints)
	}
}

func TestAggregateByAgent(t *testing.T) {
	t.Parallel()
	sessions := []RecapSession{
		{AgentsUsed: []string{"Claude Code"}, Checkpoints: []RecapCheckpoint{{}, {}, {LinkedCommit: "abc"}}},
		{AgentsUsed: []string{"Claude Code"}, Checkpoints: []RecapCheckpoint{{LinkedCommit: "def"}}},
		{AgentsUsed: []string{"Codex"}, Checkpoints: []RecapCheckpoint{{}}},
	}
	got := AggregateByAgent(sessions)
	if len(got) != 2 {
		t.Fatalf("expected 2 agents, got %d", len(got))
	}
	// Claude Code sorted first (more sessions).
	if got[0].Agent != "Claude Code" {
		t.Errorf("got[0].Agent = %q, want Claude Code", got[0].Agent)
	}
	if got[0].Sessions != 2 {
		t.Errorf("got[0].Sessions = %d, want 2", got[0].Sessions)
	}
	if got[0].Checkpoints != 4 {
		t.Errorf("got[0].Checkpoints = %d, want 4", got[0].Checkpoints)
	}
	// Linked rate: 2 linked out of 4 checkpoints = 0.5
	if got[0].LinkedRate != 0.5 {
		t.Errorf("got[0].LinkedRate = %v, want 0.5", got[0].LinkedRate)
	}
	// Density: 4 checkpoints / 2 sessions = 2.0
	if got[0].CheckpointDensity != 2.0 {
		t.Errorf("got[0].CheckpointDensity = %v, want 2.0", got[0].CheckpointDensity)
	}
}

func TestAggregateByRepo(t *testing.T) {
	t.Parallel()
	sessions := []RecapSession{
		{Repo: "entireio/cli", Checkpoints: []RecapCheckpoint{{LinkedCommit: "abc"}, {}}},
		{Repo: "entireio/cli", Checkpoints: []RecapCheckpoint{{}}},
		{Repo: "entireio/api", Checkpoints: []RecapCheckpoint{{LinkedCommit: "def"}}},
	}
	got := AggregateByRepo(sessions)
	if len(got) != 2 {
		t.Fatalf("expected 2 repos, got %d", len(got))
	}
	if got[0].Repo != "entireio/cli" {
		t.Errorf("got[0].Repo = %q, want entireio/cli", got[0].Repo)
	}
	if got[0].Sessions != 2 {
		t.Errorf("got[0].Sessions = %d, want 2", got[0].Sessions)
	}
	// Linked rate: 1 linked / 3 checkpoints = 0.333...
	if got[0].LinkedRate < 0.33 || got[0].LinkedRate > 0.34 {
		t.Errorf("got[0].LinkedRate = %v, want ~0.333", got[0].LinkedRate)
	}
}
