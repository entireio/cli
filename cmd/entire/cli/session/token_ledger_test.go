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

func TestTokenLedger(t *testing.T) {
	t.Parallel()

	t.Run("a repeated reading changes nothing", func(t *testing.T) {
		t.Parallel()
		var l TokenLedger
		l.Observe(reading(usage(100, 10, 1), nil))
		l.Observe(reading(usage(100, 10, 1), nil))
		assertUsage(t, "total", l.Total(), usage(100, 10, 1))
		assertUsage(t, "pending", l.Pending(), usage(100, 10, 1))
	})

	t.Run("a checkpoint stores only growth since the last one", func(t *testing.T) {
		t.Parallel()
		var l TokenLedger
		l.Observe(reading(usage(100, 10, 1), nil))
		l.MarkCheckpointed()
		if l.HasPending() {
			t.Fatalf("pending after checkpoint = %+v", l.Pending())
		}
		l.Observe(reading(usage(250, 30, 3), nil))
		assertUsage(t, "pending", l.Pending(), usage(150, 20, 2))
		assertUsage(t, "total", l.Total(), usage(250, 30, 3))
	})

	t.Run("a lower reading without a restart is a stale copy and is ignored", func(t *testing.T) {
		t.Parallel()
		var l TokenLedger
		l.Observe(reading(usage(250, 30, 3), nil))
		l.Observe(reading(usage(100, 10, 1), nil))
		assertUsage(t, "total", l.Total(), usage(250, 30, 3))
	})

	t.Run("a rewritten transcript carries the total on", func(t *testing.T) {
		t.Parallel()
		var l TokenLedger
		l.Observe(reading(usage(250, 30, 3), nil))
		l.MarkCheckpointed()
		// A resume restored an older copy holding one call.
		l.Observe(agent.CumulativeObservation{Total: usage(100, 10, 1), Restarted: true})
		assertUsage(t, "total after rewrite", l.Total(), usage(250, 30, 3))
		if l.HasPending() {
			t.Fatalf("pending after rewrite = %+v", l.Pending())
		}
		// One more call on the rewritten transcript.
		l.Observe(reading(usage(140, 15, 2), nil))
		assertUsage(t, "total", l.Total(), usage(290, 35, 4))
		assertUsage(t, "pending", l.Pending(), usage(40, 5, 1))
	})

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

	t.Run("a checkpoint from an older reading marks only what it read", func(t *testing.T) {
		t.Parallel()
		var onDisk TokenLedger
		onDisk.Observe(reading(usage(100, 10, 1), nil))
		attachRead := onDisk.Clone()
		// A Stop records more while attach waits to save.
		onDisk.Observe(reading(usage(150, 15, 2), nil))
		if onDisk.CheckpointedBeyond(attachRead) {
			t.Fatal("no other checkpoint moved the mark")
		}
		onDisk.MarkCheckpointedUpTo(attachRead)
		assertUsage(t, "pending", onDisk.Pending(), usage(50, 5, 1))
	})

	t.Run("the checkpointed mark never moves back", func(t *testing.T) {
		t.Parallel()
		var l TokenLedger
		l.Observe(reading(usage(100, 10, 1), nil))
		older := l.Clone()
		l.Observe(reading(usage(250, 30, 3), nil))
		l.MarkCheckpointed()
		l.MarkCheckpointedUpTo(older)
		assertUsage(t, "checkpointed", l.Checkpointed, usage(250, 30, 3))
		if !l.CheckpointedBeyond(older) {
			t.Fatal("a later checkpoint moved the mark past the older reading")
		}
	})

	t.Run("clone is deep", func(t *testing.T) {
		t.Parallel()
		var l TokenLedger
		l.Observe(reading(usage(10, 1, 1), map[string]*agent.TokenUsage{"a": usage(5, 1, 1)}))
		c := l.Clone()
		c.Children["a"].InputTokens = 999
		c.Main.InputTokens = 999
		assertUsage(t, "original", l.Total(), withKids(usage(10, 1, 1), usage(5, 1, 1)))
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
