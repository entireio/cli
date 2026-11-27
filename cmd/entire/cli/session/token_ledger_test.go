package session

import (
	"reflect"
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
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("%s = %+v, want %+v", name, g, w)
	}
	if (gs == nil) != (ws == nil) || (gs != nil && !reflect.DeepEqual(*gs, *ws)) {
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

func labelled(u *agent.TokenUsage, model string) *agent.TokenUsage {
	u.Model = model
	return u
}

// A checkpoint's mark keeps the subagents' models, so every later checkpoint
// can still split its subagent delta by model; the counts are what the
// per-field maximum gives.
func TestTokenLedger_SubagentModels(t *testing.T) {
	t.Parallel()
	const haiku, sonnet = "claude-haiku-4-5", "claude-sonnet-4-5"
	child := func(u *agent.TokenUsage) map[string]*agent.TokenUsage {
		return map[string]*agent.TokenUsage{"a": u}
	}

	t.Run("every checkpoint keeps the subagent's model", func(t *testing.T) {
		t.Parallel()
		var l TokenLedger
		for i, step := range []struct{ total, pending *agent.TokenUsage }{
			{usage(10, 1, 1), usage(10, 1, 1)},
			{usage(25, 3, 2), usage(15, 2, 1)},
			{usage(30, 4, 3), usage(5, 1, 1)},
		} {
			l.Observe(reading(usage(100*(i+1), 10*(i+1), i+1), child(labelled(step.total, haiku))))
			assertUsage(t, "pending", l.Pending(), withKids(usage(100, 10, 1), labelled(step.pending, haiku)))
			l.MarkCheckpointed()
		}
		for name, mark := range map[string]*agent.TokenUsage{"main": l.Checkpointed, "children": l.CheckpointedChildren} {
			if mark.SubagentTokens != nil || mark.SubagentTokensComplete != nil {
				t.Fatalf("%s mark holds subagent fields: %+v", name, mark)
			}
		}
	})

	t.Run("a total equal to an unlabelled mark gives it the label", func(t *testing.T) {
		t.Parallel()
		var l TokenLedger
		l.Activate(nil, usage(10, 1, 1))
		l.Observe(reading(nil, child(labelled(usage(10, 1, 1), haiku))))
		l.MarkCheckpointed()
		assertUsage(t, "mark", l.CheckpointedChildren, labelled(usage(10, 1, 1), haiku))
		l.Observe(reading(nil, child(labelled(usage(12, 2, 2), haiku))))
		assertUsage(t, "pending", l.Pending(), withKids(&agent.TokenUsage{}, labelled(usage(2, 1, 1), haiku)))
	})

	t.Run("a delta over an unlabelled mark is unlabelled once", func(t *testing.T) {
		t.Parallel()
		var l TokenLedger
		l.Activate(nil, usage(10, 1, 1)) // a mark written before models were recorded
		l.Observe(reading(nil, child(labelled(usage(15, 2, 2), haiku))))
		assertUsage(t, "first pending", l.Pending(), withKids(&agent.TokenUsage{}, usage(5, 1, 1)))
		l.MarkCheckpointed()
		l.Observe(reading(nil, child(labelled(usage(20, 3, 3), haiku))))
		assertUsage(t, "next pending", l.Pending(), withKids(&agent.TokenUsage{}, labelled(usage(5, 1, 1), haiku)))
	})

	t.Run("a total below the mark on any count keeps the per-field maximum", func(t *testing.T) {
		t.Parallel()
		var l TokenLedger
		l.Activate(nil, usage(10, 9, 1))
		l.Observe(reading(nil, child(labelled(usage(20, 5, 2), haiku))))
		l.MarkCheckpointed()
		assertUsage(t, "mark", l.CheckpointedChildren, usage(20, 9, 2))
	})

	t.Run("the mark does not share the total's split", func(t *testing.T) {
		t.Parallel()
		total := &agent.TokenUsage{InputTokens: 30, OutputTokens: 3, APICallCount: 2, ByModel: []agent.TokenUsage{
			*labelled(usage(10, 1, 1), haiku), *labelled(usage(20, 2, 1), sonnet),
		}}
		total.SubagentTokens = usage(1, 1, 1)
		mark := advanceMark(usage(5, 1, 1), total)
		total.ByModel[0].InputTokens = 999
		want := &agent.TokenUsage{InputTokens: 30, OutputTokens: 3, APICallCount: 2, ByModel: []agent.TokenUsage{
			*labelled(usage(10, 1, 1), haiku), *labelled(usage(20, 2, 1), sonnet),
		}}
		assertUsage(t, "mark", mark, want)
		if mark.SubagentTokens != nil {
			t.Fatalf("mark holds subagent tokens: %+v", mark.SubagentTokens)
		}
	})
}
