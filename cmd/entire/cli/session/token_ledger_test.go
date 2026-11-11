package session

import (
	"maps"
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
			assertSubagentsComplete(t, name, u, want)
		}
		var l TokenLedger
		l.Observe(reading(usage(10, 1, 1), map[string]*agent.TokenUsage{"a": usage(50, 5, 2), "b": nil}))
		assertUsage(t, "total", l.Total(), withKids(usage(10, 1, 1), usage(50, 5, 2)))
		complete("total", l.Total(), false)
		complete("pending", l.Pending(), false)
		complete("clone", l.Clone().Total(), false)

		l.Observe(reading(usage(10, 1, 1), map[string]*agent.TokenUsage{"a": usage(50, 5, 2), "b": usage(7, 1, 1)}))
		complete("total once counted", l.Total(), true)

		// Counted once, it keeps its total while its file can't be read, but
		// it may have grown since, so the children are incomplete until a
		// reading counts it again, even once no reading lists it (its file
		// deleted).
		l.Observe(reading(usage(10, 1, 1), map[string]*agent.TokenUsage{"a": usage(50, 5, 2), "b": nil}))
		assertUsage(t, "total", l.Total(), withKids(usage(10, 1, 1), usage(57, 6, 3)))
		complete("total while its file can't be read", l.Total(), false)
		l.Observe(reading(usage(10, 1, 1), map[string]*agent.TokenUsage{"a": usage(50, 5, 2)}))
		assertUsage(t, "total", l.Total(), withKids(usage(10, 1, 1), usage(57, 6, 3)))
		complete("total after its file is gone", l.Total(), false)
		l.Observe(reading(usage(10, 1, 1), map[string]*agent.TokenUsage{"a": usage(50, 5, 2), "b": usage(9, 1, 2)}))
		complete("total once counted again", l.Total(), true)
	})

	t.Run("attach's mark keeps a newer checkpoint's uncounted child", func(t *testing.T) {
		t.Parallel()
		var l TokenLedger
		l.Observe(reading(usage(10, 1, 1), map[string]*agent.TokenUsage{"a": usage(50, 5, 2), "b": usage(7, 1, 1)}))
		read := l.Clone() // attach's reading, every child counted
		// While attach waits, a hook can't read b and checkpoints.
		l.Observe(reading(usage(20, 2, 2), map[string]*agent.TokenUsage{"a": usage(50, 5, 2), "b": nil}))
		l.MarkCheckpointed()
		l.MarkCheckpointedUpTo(read)
		// b's total now includes what it used before the hook's checkpoint.
		l.Observe(reading(usage(20, 2, 2), map[string]*agent.TokenUsage{"a": usage(50, 5, 2), "b": usage(9, 1, 2)}))
		assertSubagentsComplete(t, "pending", l.Pending(), false)
	})

	t.Run("attach's mark takes a newer checkpoint that counted its uncounted child", func(t *testing.T) {
		t.Parallel()
		var l TokenLedger
		l.Observe(reading(usage(10, 1, 1), map[string]*agent.TokenUsage{"a": usage(50, 5, 2), "b": nil}))
		read := l.Clone() // attach's reading could not count b
		// While attach waits, a hook counts b and checkpoints.
		l.Observe(reading(usage(20, 2, 2), map[string]*agent.TokenUsage{"a": usage(50, 5, 2), "b": usage(7, 1, 1)}))
		l.MarkCheckpointed()
		l.MarkCheckpointedUpTo(read)
		l.Observe(reading(usage(30, 3, 3), map[string]*agent.TokenUsage{"a": usage(60, 6, 3), "b": usage(7, 1, 1)}))
		assertUsage(t, "pending", l.Pending(), withKids(usage(10, 1, 1), usage(10, 1, 1)))
		assertSubagentsComplete(t, "pending", l.Pending(), true)
	})

	t.Run("attach's mark keeps a child the newer checkpoint never counted", func(t *testing.T) {
		t.Parallel()
		var l TokenLedger
		l.Observe(reading(usage(10, 1, 1), map[string]*agent.TokenUsage{"a": usage(40, 4, 1)}))
		l.MarkCheckpointed()
		read := l.Clone()
		// Attach's reading counts a and lists b, which it cannot read.
		read.Observe(reading(usage(20, 2, 2), map[string]*agent.TokenUsage{"a": usage(50, 5, 2), "b": nil}))
		// b's file is deleted; while attach waits, a hook counts a and
		// checkpoints without listing b.
		l.Observe(reading(usage(30, 3, 3), map[string]*agent.TokenUsage{"a": usage(60, 6, 3)}))
		l.MarkCheckpointed()
		l.MarkCheckpointedUpTo(read)
		// b's total now includes what it used before the hook's checkpoint.
		l.Observe(reading(usage(30, 3, 3), map[string]*agent.TokenUsage{"a": usage(60, 6, 3), "b": usage(7, 1, 1)}))
		assertSubagentsComplete(t, "pending", l.Pending(), false)
		assertSubagentsComplete(t, "cloned pending", l.Clone().Pending(), false)
	})

	t.Run("attach's mark keeps a child the newer checkpoint held only from an earlier reading", func(t *testing.T) {
		t.Parallel()
		var l TokenLedger
		l.Observe(reading(usage(10, 1, 1), map[string]*agent.TokenUsage{"a": usage(40, 4, 1)}))
		l.MarkCheckpointed()
		read := l.Clone()
		// a has used more since, and attach's reading cannot read it.
		read.Observe(reading(usage(20, 2, 2), map[string]*agent.TokenUsage{"a": nil, "b": usage(10, 1, 1)}))
		// a's file is gone; while attach waits, a hook reads b and
		// checkpoints, holding only the total a had at the first reading.
		l.Observe(reading(usage(30, 3, 3), map[string]*agent.TokenUsage{"b": usage(20, 2, 2)}))
		l.MarkCheckpointed()
		l.MarkCheckpointedUpTo(read)
		// a's file is back: its total includes what it used before the
		// hook's checkpoint.
		l.Observe(reading(usage(30, 3, 3), map[string]*agent.TokenUsage{"a": usage(70, 7, 3), "b": usage(20, 2, 2)}))
		assertSubagentsComplete(t, "pending", l.Pending(), false)
		assertSubagentsComplete(t, "cloned pending", l.Clone().Pending(), false)
	})

	t.Run("attach's winning mark keeps a newer checkpoint's uncounted child it did not count", func(t *testing.T) {
		t.Parallel()
		var l TokenLedger
		l.Observe(reading(usage(10, 1, 1), map[string]*agent.TokenUsage{"a": usage(40, 4, 1)}))
		l.MarkCheckpointed()
		read := l.Clone()
		read.Observe(reading(usage(20, 2, 2), map[string]*agent.TokenUsage{"a": usage(60, 6, 2)}))
		// a's file is gone; while attach waits, a hook finds b, cannot read
		// it, and checkpoints below attach's reading.
		l.Observe(reading(usage(30, 3, 3), map[string]*agent.TokenUsage{"b": nil}))
		l.MarkCheckpointed()
		l.MarkCheckpointedUpTo(read)
		// b's total now includes what it used before the hook's checkpoint.
		l.Observe(reading(usage(30, 3, 3), map[string]*agent.TokenUsage{"a": usage(60, 6, 2), "b": usage(7, 1, 1)}))
		assertSubagentsComplete(t, "pending", l.Pending(), false)
		assertSubagentsComplete(t, "cloned pending", l.Clone().Pending(), false)
	})

	t.Run("attach's mark keeps both uncounted sets when neither mark holds the other", func(t *testing.T) {
		t.Parallel()
		read := &TokenLedger{
			Active:            true,
			Children:          map[string]*agent.TokenUsage{"a": usage(50, 1, 1)},
			UncountedChildren: map[string]bool{"b": true},
		}
		l := read.Clone()
		// A hook's checkpoint with fewer input but more output tokens.
		l.CheckpointedChildren = usage(40, 2, 1)
		l.CheckpointedUncounted = map[string]bool{"c": true}
		l.MarkCheckpointedUpTo(read)
		if want := map[string]bool{"b": true, "c": true}; !maps.Equal(l.CheckpointedUncounted, want) {
			t.Fatalf("checkpointed uncounted = %v, want %v", l.CheckpointedUncounted, want)
		}
	})

	t.Run("attach's mark takes its own reading's counted children", func(t *testing.T) {
		t.Parallel()
		var l TokenLedger
		l.Observe(reading(usage(10, 1, 1), map[string]*agent.TokenUsage{"a": usage(50, 5, 2), "b": nil}))
		l.MarkCheckpointed()
		l.Observe(reading(usage(20, 2, 2), map[string]*agent.TokenUsage{"a": usage(50, 5, 2), "b": usage(7, 1, 1)}))
		read := l.Clone() // attach's reading counted b; no checkpoint since
		l.Observe(reading(usage(30, 3, 3), map[string]*agent.TokenUsage{"a": usage(60, 6, 3), "b": usage(7, 1, 1)}))
		l.MarkCheckpointedUpTo(read)
		assertUsage(t, "pending", l.Pending(), withKids(usage(10, 1, 1), usage(10, 1, 1)))
		assertSubagentsComplete(t, "pending", l.Pending(), true)
	})

	t.Run("a checkpoint taken while a child was uncounted leaves the next delta incomplete", func(t *testing.T) {
		t.Parallel()
		var l TokenLedger
		l.Observe(reading(usage(10, 1, 1), map[string]*agent.TokenUsage{"a": usage(50, 5, 2), "b": nil}))
		l.MarkCheckpointed()
		// b's total now includes what it used before that checkpoint.
		l.Observe(reading(usage(20, 2, 2), map[string]*agent.TokenUsage{"a": usage(50, 5, 2), "b": usage(7, 1, 1)}))
		assertSubagentsComplete(t, "total", l.Total(), true)
		assertSubagentsComplete(t, "pending", l.Pending(), false)
		assertSubagentsComplete(t, "cloned pending", l.Clone().Pending(), false)

		l.MarkCheckpointed()
		l.Observe(reading(usage(30, 3, 3), map[string]*agent.TokenUsage{"a": usage(60, 6, 3), "b": usage(7, 1, 1)}))
		assertSubagentsComplete(t, "pending after a checkpoint that counted every child", l.Pending(), true)
	})

	t.Run("an uncounted only child marks main usage incomplete", func(t *testing.T) {
		t.Parallel()
		var l TokenLedger
		l.Observe(reading(usage(10, 1, 1), map[string]*agent.TokenUsage{"b": nil}))
		assertUsage(t, "total", l.Total(), usage(10, 1, 1))
		assertSubagentsComplete(t, "total", l.Total(), false)
	})

	t.Run("an uncounted child that stops being listed stays uncounted", func(t *testing.T) {
		t.Parallel()
		var l TokenLedger
		l.Observe(reading(usage(10, 1, 1), map[string]*agent.TokenUsage{"a": usage(50, 5, 2), "b": nil}))
		// b's file is gone: the reading no longer lists it.
		l.Observe(reading(usage(10, 1, 1), map[string]*agent.TokenUsage{"a": usage(50, 5, 2)}))
		assertSubagentsComplete(t, "total", l.Total(), false)
		assertSubagentsComplete(t, "pending", l.Pending(), false)
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

// assertSubagentsComplete fails unless u's subagent totals are marked
// complete as want says.
func assertSubagentsComplete(t *testing.T, name string, u *agent.TokenUsage, want bool) {
	t.Helper()
	if u == nil || u.SubagentTokensComplete == nil || *u.SubagentTokensComplete != want {
		t.Fatalf("%s = %+v, want subagent_tokens_complete %v", name, u, want)
	}
}
