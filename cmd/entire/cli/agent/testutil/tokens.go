package testutil

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// AssertCumulative fails the test when a cumulative token read differs from
// the wanted main total or per-child totals. nil is distinct from zero.
func AssertCumulative(t *testing.T, total *types.TokenUsage, children map[string]*types.TokenUsage, wantTotal *types.TokenUsage, wantChildren map[string]*types.TokenUsage) {
	t.Helper()
	if !reflect.DeepEqual(total, wantTotal) {
		t.Errorf("total = %s, want %s", format(total), format(wantTotal))
	}
	if !reflect.DeepEqual(children, wantChildren) {
		t.Errorf("children = %v, want %v", children, wantChildren)
	}
}

func format(u *types.TokenUsage) string {
	if u == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%+v", *u)
}
