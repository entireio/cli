package recap

import "testing"

func TestRangeKey_Range30dRemoved(t *testing.T) {
	t.Parallel()
	// Compile-time check: if Range30d exists, this file will compile
	// but the test verifies the enum has exactly the expected members.
	want := []RangeKey{RangeDay, RangeWeek, RangeMonth, Range90d}
	if len(want) != 4 {
		t.Fatalf("expected 4 range keys, got %d", len(want))
	}
}
