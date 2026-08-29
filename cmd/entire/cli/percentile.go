package cli

import "cmp"

// percentile returns the p-th percentile of a sorted slice using nearest-rank,
// so p50 of one sample is that sample rather than an interpolation. An empty
// slice yields the zero value.
//
// The rank is ceil(p/100 * n), converted to a 0-based index. Truncating instead
// of rounding up overshoots by one whole sample whenever p*n divides evenly,
// which is exactly the round-numbered case: p90 of 10 samples would report the
// maximum, making a P90 column a duplicate of MAX.
func percentile[T cmp.Ordered](sorted []T, p int) T {
	var zero T
	if len(sorted) == 0 {
		return zero
	}
	n := len(sorted)
	rank := (p*n + 99) / 100 // ceil(p*n/100)
	idx := max(rank-1, 0)
	if idx >= n {
		idx = n - 1
	}
	return sorted[idx]
}
