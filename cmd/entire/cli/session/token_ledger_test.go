package session

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

func usage(in, out, calls int) *agent.TokenUsage {
	return &agent.TokenUsage{InputTokens: in, OutputTokens: out, APICallCount: calls}
}

func reading(total *agent.TokenUsage, children map[string]*agent.TokenUsage) agent.CumulativeObservation {
	return agent.CumulativeObservation{Total: total, Children: children}
}

func assertUsage(t *testing.T, name string, got, want *agent.TokenUsage) {
	t.Helper()
	if (got == nil) != (want == nil) {
		t.Fatalf("%s = %+v, want %+v", name, got, want)
	}
	if got == nil {
		return
	}
	g, w := *got, *want
	gs, ws := g.SubagentTokens, w.SubagentTokens
	g.SubagentTokens, w.SubagentTokens, g.SubagentTokensComplete, w.SubagentTokensComplete = nil, nil, nil, nil
	if g != w {
		t.Fatalf("%s = %+v, want %+v", name, g, w)
	}
	if (gs == nil) != (ws == nil) || (gs != nil && *gs != *ws) {
		t.Fatalf("%s subagents = %+v, want %+v", name, gs, ws)
	}
}

func withKids(main, kids *agent.TokenUsage) *agent.TokenUsage {
	out := *main
	out.SubagentTokens = kids
	return &out
}

// Readings, rewrites and checkpoint marks are covered through the hooks
// (integration_test/token_ledger_test.go) and attach's partial mark by
// TestSaveAttachSessionState_LedgerMarksOnlyWhatAttachRead.
func TestTokenLedger(t *testing.T) {
	t.Parallel()

	t.Run("a child keeps its highest total after its file is gone", func(t *testing.T) {
		t.Parallel()
		var l TokenLedger
		l.Observe(reading(usage(10, 1, 1), map[string]*agent.TokenUsage{"a": usage(50, 5, 2)}))
		l.Observe(reading(usage(20, 2, 2), nil))
		assertUsage(t, "total", l.Total(), withKids(usage(20, 2, 2), usage(50, 5, 2)))
		l.MarkCheckpointed()
		l.Observe(reading(usage(20, 2, 2), map[string]*agent.TokenUsage{"a": usage(50, 5, 2), "b": usage(7, 1, 1)}))
		assertUsage(t, "pending", l.Pending(), withKids(&agent.TokenUsage{}, usage(7, 1, 1)))
	})

	t.Run("nothing used is nil", func(t *testing.T) {
		t.Parallel()
		var l TokenLedger
		l.Observe(reading(nil, nil))
		if l.Total() != nil || l.Pending() != nil || l.HasPending() {
			t.Fatalf("total %+v pending %+v", l.Total(), l.Pending())
		}
		var none *TokenLedger
		if none.HasPending() {
			t.Fatal("nil ledger has pending")
		}
	})
}
