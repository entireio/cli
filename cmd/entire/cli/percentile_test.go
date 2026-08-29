package cli

import "testing"

// TestPercentile_NearestRank guards the round-numbered cases, where truncating
// the rank overshoots by a full sample and makes P90 a duplicate of MAX.
func TestPercentile_NearestRank(t *testing.T) {
	t.Parallel()

	ten := []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	cases := []struct {
		name   string
		sorted []int64
		p      int
		want   int64
	}{
		{"p90 of 10 samples is the 9th, not the max", ten, 90, 9},
		{"p50 of 10 samples is the 5th", ten, 50, 5},
		{"p100 is the max", ten, 100, 10},
		{"a percentile below one rank still lands on the first sample", ten, 1, 1},
		{"p50 of one sample is that sample", []int64{7}, 50, 7},
		{"p90 of one sample is that sample", []int64{7}, 90, 7},
		{"p50 of an even count takes the lower median", []int64{1, 2}, 50, 1},
		{"empty input yields zero", nil, 50, 0},
	}
	for _, tc := range cases {
		if got := percentile(tc.sorted, tc.p); got != tc.want {
			t.Errorf("%s: percentile(%v, %d) = %d, want %d", tc.name, tc.sorted, tc.p, got, tc.want)
		}
	}

	if got := percentile([]int{3, 5, 9}, 50); got != 5 {
		t.Errorf("percentile over int = %d, want 5", got)
	}
	if got := percentile([]float64{0.1, 0.4, 0.9}, 90); got != 0.9 {
		t.Errorf("percentile over float64 = %v, want 0.9", got)
	}
}
