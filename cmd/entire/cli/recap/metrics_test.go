package recap

import (
	"reflect"
	"testing"
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

func TestDominantLabel_ExactThresholds(t *testing.T) {
	t.Parallel()
	// Exactly 0.10 lead at 0.55 share — below 0.15 requirement → not qualified.
	counts := map[string]int{"a": 55, "b": 45}
	if _, ok := DominantLabel(counts); ok {
		t.Error("exactly-0.10 lead should not qualify")
	}

	// 0.55 share, 0.15 lead (vs runner-up 40) — at threshold, should qualify.
	counts = map[string]int{"a": 55, "b": 40, "c": 5}
	if _, ok := DominantLabel(counts); !ok {
		t.Error("exactly-0.15 lead at 0.55 share should qualify (>= comparison)")
	}
}
