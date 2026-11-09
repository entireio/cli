package agent

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTurnEndBudgetWithin(t *testing.T) {
	t.Parallel()

	tests := []struct {
		hookTimeout time.Duration
		want        time.Duration
	}{
		{hookTimeout: 0, want: 0},
		{hookTimeout: time.Second, want: 0},
		{hookTimeout: 10 * time.Second, want: 8 * time.Second},
		{hookTimeout: 30 * time.Second, want: 24 * time.Second},
		{hookTimeout: 60 * time.Second, want: 50 * time.Second},
		{hookTimeout: 120 * time.Second, want: 110 * time.Second},
		{hookTimeout: 300 * time.Second, want: 290 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.hookTimeout.String(), func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, TurnEndBudgetWithin(tt.hookTimeout))
		})
	}
}
