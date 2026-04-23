package recap

import (
	"sort"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/session"
)

func TestComputeBadges(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 4, 15, 10, 0, 0, 0, time.UTC)
	ended := base.Add(30 * time.Minute)
	cases := []struct {
		name string
		s    RecapSession
		want []string
	}{
		{
			name: "active session (no EndedAt)",
			s: RecapSession{
				StartedAt: base,
				Phase:     session.PhaseActive,
			},
			want: []string{"active"},
		},
		{
			name: "linked commit",
			s: RecapSession{
				StartedAt:     base,
				EndedAt:       &ended,
				LinkedCommits: []string{"abc1234"},
			},
			want: []string{"linked"},
		},
		{
			name: "delegated",
			s: RecapSession{
				StartedAt: base,
				EndedAt:   &ended,
				Checkpoints: []RecapCheckpoint{
					{IsTask: true},
				},
			},
			want: []string{"delegated"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ComputeBadges(tc.s, nil)
			sort.Strings(got)
			sort.Strings(tc.want)
			if !stringSlicesEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestComputeBadges_Resumed(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 4, 15, 10, 0, 0, 0, time.UTC)
	priorEnd := base.Add(-23 * time.Hour)
	prior := []RecapSession{
		{
			StartedAt: base.Add(-24 * time.Hour),
			EndedAt:   &priorEnd,
			Branch:    "main",
		},
	}
	current := RecapSession{StartedAt: base, Branch: "main"}
	got := ComputeBadges(current, prior)
	if !contains(got, "resumed") {
		t.Errorf("expected 'resumed' badge, got %v", got)
	}
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
