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

	t.Run("a rewrite keeps what the rewritten-away copy counted", func(t *testing.T) {
		t.Parallel()
		var l TokenLedger
		l.Observe(reading(usage(100, 50, 2), nil))
		// Restored to an older copy that has since grown: input passed the
		// old total, output did not.
		l.Observe(agent.CumulativeObservation{Total: usage(120, 30, 3), Restarted: true})
		assertUsage(t, "total", l.Total(), usage(120, 50, 3))
	})

	t.Run("a child no reading counted leaves the children incomplete", func(t *testing.T) {
		t.Parallel()
		complete := func(name string, u *agent.TokenUsage, want bool) {
			t.Helper()
			if u == nil || u.SubagentTokensComplete == nil || *u.SubagentTokensComplete != want {
				t.Fatalf("%s = %+v, want subagent_tokens_complete %v", name, u, want)
			}
		}
		var l TokenLedger
		l.Observe(reading(usage(10, 1, 1), map[string]*agent.TokenUsage{"a": usage(50, 5, 2), "b": nil}))
		assertUsage(t, "total", l.Total(), withKids(usage(10, 1, 1), usage(50, 5, 2)))
		complete("total", l.Total(), false)
		complete("pending", l.Pending(), false)
		complete("clone", l.Clone().Total(), false)

		l.Observe(reading(usage(10, 1, 1), map[string]*agent.TokenUsage{"a": usage(50, 5, 2), "b": usage(7, 1, 1)}))
		complete("total once counted", l.Total(), true)

		// Counted once, it keeps its total when its file can no longer be read.
		l.Observe(reading(usage(10, 1, 1), map[string]*agent.TokenUsage{"a": usage(50, 5, 2), "b": nil}))
		assertUsage(t, "total", l.Total(), withKids(usage(10, 1, 1), usage(57, 6, 3)))
		complete("total after its file is gone", l.Total(), true)
	})

	t.Run("an uncounted only child marks main usage incomplete", func(t *testing.T) {
		t.Parallel()
		var l TokenLedger
		l.Observe(reading(usage(10, 1, 1), map[string]*agent.TokenUsage{"b": nil}))
		total := l.Total()
		assertUsage(t, "total", total, usage(10, 1, 1))
		if total.SubagentTokensComplete == nil || *total.SubagentTokensComplete {
			t.Fatalf("total = %+v, want subagent_tokens_complete false", total)
		}
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
