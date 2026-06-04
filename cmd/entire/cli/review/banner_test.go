package review

import "testing"

// TestFormatContextBanner pins the itemised scope banner: an empty state, the
// itemised checkpoints+sessions layout, and the count-only fallback used when
// items aren't populated.
func TestFormatContextBanner(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   ContextResult
		want string
	}{
		{
			name: "neither",
			in:   ContextResult{},
			want: "No prior session or checkpoint context for this branch yet.",
		},
		{
			name: "itemised checkpoints and sessions",
			in: ContextResult{
				Checkpoints: 2, Sessions: 1,
				CheckpointItems: []CheckpointScopeItem{
					{ID: "a3b2c4d5", Summary: "feat(review): emit honest live tokens"},
					{ID: "b4c3d5e6", Summary: "feat(review): flag-driven roles"},
				},
				SessionItems: []SessionScopeItem{
					{ID: "ac3d5c6e", Agent: "Claude Code"},
				},
			},
			want: "Checkpoints in scope (2):\n" +
				"  • a3b2c4d5  feat(review): emit honest live tokens\n" +
				"  • b4c3d5e6  feat(review): flag-driven roles\n" +
				"In-progress sessions (1):\n" +
				"  • ac3d5c6e  Claude Code",
		},
		{
			name: "sessions listed by short id and agent",
			in: ContextResult{
				Sessions: 2,
				SessionItems: []SessionScopeItem{
					{ID: "ac3d5c6e", Agent: "Claude Code"},
					{ID: "3d4c9f88", Agent: "Codex"},
				},
			},
			want: "In-progress sessions (2):\n" +
				"  • ac3d5c6e  Claude Code\n" +
				"  • 3d4c9f88  Codex",
		},
		{
			name: "count-only fallback when items absent",
			in:   ContextResult{Checkpoints: 3, Sessions: 1},
			want: "3 checkpoints in scope.\n1 session in progress.",
		},
		{
			name: "empty summary renders placeholder",
			in: ContextResult{
				Checkpoints:     1,
				CheckpointItems: []CheckpointScopeItem{{ID: "a3b2c4d5"}},
			},
			want: "Checkpoints in scope (1):\n  • a3b2c4d5  (no summary)",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := formatContextBanner(tc.in); got != tc.want {
				t.Errorf("formatContextBanner(%+v) =\n%q\nwant\n%q", tc.in, got, tc.want)
			}
		})
	}
}

func TestPluralizeContextNoun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		n    int
		want string
	}{
		{n: 1, want: "1 checkpoint"},
		{n: 2, want: "2 checkpoints"},
		{n: 0, want: "0 checkpoints"},
	}
	for _, tc := range tests {
		if got := pluralizeContextNoun(tc.n, "checkpoint", "checkpoints"); got != tc.want {
			t.Errorf("pluralizeContextNoun(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}
