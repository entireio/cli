package transcript

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func snap(input, cached, output int64, reasoning ...int64) codexUsage {
	u := codexUsage{Input: input, Cached: cached, Output: output}
	if len(reasoning) > 0 {
		u.Reasoning = &reasoning[0]
	}
	return u
}

func TestCodexUsageTracker(t *testing.T) {
	t.Parallel()

	call := func(fresh, read, output, reasoning int64) *CallUsage {
		return &CallUsage{InputTokens: fresh, CacheReadTokens: read, OutputTokens: output, ReasoningTokens: reasoning}
	}

	tests := []struct {
		name        string
		totals      []codexUsage
		want        []*CallUsage // nil entry: snapshot is not a model call
		wantStopped bool
	}{
		{
			name: "repeated snapshots are not calls and keep the baseline",
			totals: []codexUsage{
				snap(0, 0, 0),
				snap(10, 2, 4, 2),
				snap(10, 2, 4, 2),
				snap(10, 2, 4),
				snap(20, 5, 8, 3),
			},
			want: []*CallUsage{nil, call(8, 2, 4, 2), nil, nil, call(7, 3, 4, 1)},
		},
		{
			name: "reasoning delta needs adjacent reported snapshots",
			totals: []codexUsage{
				snap(10, 2, 4, 2),
				snap(20, 5, 8),
				snap(30, 9, 12, 6),
				snap(40, 14, 16, 7),
			},
			want: []*CallUsage{call(8, 2, 4, 2), call(7, 3, 4, 0), call(6, 4, 4, 0), call(5, 5, 4, 1)},
		},
		{
			name:   "fresh input is clamped when cached exceeds input",
			totals: []codexUsage{snap(2, 3, 5)},
			want:   []*CallUsage{call(0, 3, 5, 0)},
		},
		{
			name:        "reasoning decrease with unchanged totals stops collection",
			totals:      []codexUsage{snap(10, 0, 4, 2), snap(10, 0, 4, 0)},
			want:        []*CallUsage{call(10, 0, 4, 2), nil},
			wantStopped: true,
		},
		{
			name:        "reasoning below last reported total stops collection",
			totals:      []codexUsage{snap(10, 0, 4, 2), snap(20, 0, 8, 0), snap(30, 0, 12, 4)},
			want:        []*CallUsage{call(10, 0, 4, 2), nil, nil},
			wantStopped: true,
		},
		{
			name:        "unreported reasoning does not hide a later reasoning reset",
			totals:      []codexUsage{snap(10, 0, 4, 2), snap(20, 0, 8), snap(30, 0, 12, 0), snap(40, 0, 16, 4)},
			want:        []*CallUsage{call(10, 0, 4, 2), call(10, 0, 4, 0), nil, nil},
			wantStopped: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tracker := newCodexUsageTracker()
			for i, total := range tc.totals {
				got, ok := tracker.next(total)
				if tc.want[i] == nil {
					assert.False(t, ok, "snapshot %d", i)
					continue
				}
				if assert.True(t, ok, "snapshot %d", i) {
					assert.Equal(t, *tc.want[i], got, "snapshot %d", i)
				}
			}
			assert.Equal(t, tc.wantStopped, tracker.stopped())
		})
	}
}
