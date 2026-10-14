package agent

import "testing"

func TestSumCumulativeSegments(t *testing.T) {
	t.Parallel()

	in := func(n int) *TokenUsage { return &TokenUsage{InputTokens: n, APICallCount: 1} }
	tests := []struct {
		name      string
		snapshots []*TokenUsage
		want      *TokenUsage
	}{
		{"none", nil, nil},
		{"growing total keeps the last", []*TokenUsage{in(10), in(25)}, in(25)},
		{"lower total starts a segment", []*TokenUsage{in(10), in(25), in(5), in(8)}, &TokenUsage{InputTokens: 33, APICallCount: 2}},
		{"replayed equal total is not recounted", []*TokenUsage{in(10), in(25), in(25)}, in(25)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := SumCumulativeSegments(tt.snapshots)
			if (got == nil) != (tt.want == nil) || (got != nil && (got.InputTokens != tt.want.InputTokens || got.APICallCount != tt.want.APICallCount)) {
				t.Fatalf("SumCumulativeSegments = %+v, want %+v", got, tt.want)
			}
		})
	}
}
