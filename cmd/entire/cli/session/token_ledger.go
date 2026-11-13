package session

import (
	"bytes"
	"encoding/json"
	"slices"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// TokenLedger keeps a session's token usage as the agent's own running total
// (agent.ReadCumulativeUsage). Every reading replaces the last, so a repeated
// Stop or a mid-turn commit cannot count anything twice; a checkpoint stores
// what the total grew by since the last checkpoint. Readings happen under the
// session lock (strategy.ObserveTokenLedger), so they are ordered.
type TokenLedger struct {
	// Active is set by the first reading that found usage; until then the
	// session is counted per turn.
	Active bool `json:"active,omitempty"`
	// Main is the main agent's total from the latest reading.
	Main *agent.TokenUsage `json:"main,omitempty"`
	// Base is what transcripts rewritten away had counted beyond what the
	// rewritten transcript counts (see Observe).
	Base *agent.TokenUsage `json:"base,omitempty"`
	// Children is each subagent's highest total, so one whose file is
	// deleted keeps its last total.
	Children map[string]*agent.TokenUsage `json:"children,omitempty"`
	// Cursor lets the next reading continue where the last one stopped.
	Cursor json.RawMessage `json:"cursor,omitempty"`
	// Checkpointed is what the session's checkpoints already hold.
	Checkpointed         *agent.TokenUsage `json:"checkpointed,omitempty"`
	CheckpointedChildren *agent.TokenUsage `json:"checkpointed_children,omitempty"`
}

// Observe folds one reading in. A lower main total after a restart is a
// rewritten transcript (a resume restoring an older copy): the difference
// moves to Base so the total carries on. A lower total without one is a stale
// copy (OpenCode's export) and is ignored; where no restart can be reported
// (an OpenCode undo, Droid), a real rewrite undercounts until the total passes
// the old one, never double counts. A rewrite is seen by the turn-start
// reading, before the next turn's calls; one that has already grown past the
// old total moves only the fields still below it.
func (l *TokenLedger) Observe(obs agent.CumulativeObservation) {
	switch {
	case obs.Restarted:
		l.Base = types.AddTokenUsage(l.Base, types.SubtractTokenUsage(cloneUsage(l.Main), cloneUsage(obs.Total)))
		l.Main = cloneUsage(obs.Total)
	case agent.TokenTotal(obs.Total) >= agent.TokenTotal(l.Main):
		l.Main = cloneUsage(obs.Total)
	}
	for id, usage := range obs.Children {
		if usage == nil {
			continue
		}
		if prev, ok := l.Children[id]; ok && agent.TokenTotal(usage) < agent.TokenTotal(prev) {
			continue
		}
		if l.Children == nil {
			l.Children = make(map[string]*agent.TokenUsage)
		}
		l.Children[id] = cloneUsage(usage)
	}
	if obs.Cursor != nil {
		l.Cursor = obs.Cursor
	}
}

// Total returns the session's usage, subagents in SubagentTokens; nil when
// nothing was used.
func (l *TokenLedger) Total() *agent.TokenUsage {
	return withSubagents(l.mainTotal(), l.childrenTotal())
}

// Pending returns what the session used since its last checkpoint.
func (l *TokenLedger) Pending() *agent.TokenUsage {
	main := types.SubtractTokenUsage(l.mainTotal(), l.Checkpointed)
	children := types.SubtractTokenUsage(l.childrenTotal(), l.CheckpointedChildren)
	return withSubagents(main, children)
}

// HasPending reports whether a checkpoint would store any tokens now.
func (l *TokenLedger) HasPending() bool {
	return l.IsActive() && l.Pending() != nil
}

// IsActive reports whether the ledger is the session's token source.
func (l *TokenLedger) IsActive() bool {
	return l != nil && l.Active
}

// Activate makes the ledger the session's token source; what the per-turn
// accounting already checkpointed counts as checkpointed.
func (l *TokenLedger) Activate(mainCheckpointed, childrenCheckpointed *agent.TokenUsage) {
	l.Active = true
	l.Checkpointed = types.AddTokenUsage(l.Checkpointed, cloneUsage(mainCheckpointed))
	l.CheckpointedChildren = types.AddTokenUsage(l.CheckpointedChildren, cloneUsage(childrenCheckpointed))
}

// MarkCheckpointed records that a checkpoint holds everything observed. The
// mark never moves back.
func (l *TokenLedger) MarkCheckpointed() {
	l.Checkpointed = advanceMark(l.Checkpointed, l.mainTotal())
	l.CheckpointedChildren = advanceMark(l.CheckpointedChildren, l.childrenTotal())
}

// MarkCheckpointedUpTo marks only what o, an earlier copy of this ledger,
// had read: readings made since stay pending.
func (l *TokenLedger) MarkCheckpointedUpTo(o *TokenLedger) {
	l.Checkpointed = advanceMark(l.Checkpointed, o.mainTotal())
	l.CheckpointedChildren = advanceMark(l.CheckpointedChildren, o.childrenTotal())
}

// advanceMark moves a checkpointed mark up to total, never back
// (agent.MaxTokenUsage). A total at or above the mark on every count is the
// new mark as it is, model labels included, so the next checkpoint's delta
// can be split by model (types.SubtractTokenUsage leaves a delta unlabelled
// when its baseline is not). A total below the mark on any count keeps the
// per-field maximum, which has no label. The counts are the same either way.
func advanceMark(mark, total *agent.TokenUsage) *agent.TokenUsage {
	if mark == nil || total == nil || !coversCounts(total, mark) {
		return agent.MaxTokenUsage(mark, total)
	}
	next := *total
	next.ByModel = slices.Clone(total.ByModel)
	next.SubagentTokens, next.SubagentTokensComplete = nil, nil
	return &next
}

// coversCounts reports whether a is at least b on every count.
func coversCounts(a, b *agent.TokenUsage) bool {
	return a.InputTokens >= b.InputTokens && a.CacheCreationTokens >= b.CacheCreationTokens &&
		a.CacheReadTokens >= b.CacheReadTokens && a.OutputTokens >= b.OutputTokens &&
		a.APICallCount >= b.APICallCount
}

// Clone returns a deep copy.
func (l *TokenLedger) Clone() *TokenLedger {
	if l == nil {
		return nil
	}
	c := &TokenLedger{
		Active:               l.Active,
		Main:                 cloneUsage(l.Main),
		Base:                 cloneUsage(l.Base),
		Cursor:               bytes.Clone(l.Cursor),
		Checkpointed:         cloneUsage(l.Checkpointed),
		CheckpointedChildren: cloneUsage(l.CheckpointedChildren),
	}
	if l.Children != nil {
		c.Children = make(map[string]*agent.TokenUsage, len(l.Children))
		for id, usage := range l.Children {
			c.Children[id] = cloneUsage(usage)
		}
	}
	return c
}

// mainTotal is Main carried past rewrites by Base.
func (l *TokenLedger) mainTotal() *agent.TokenUsage {
	return types.AddTokenUsage(cloneUsage(l.Base), cloneUsage(l.Main))
}

func (l *TokenLedger) childrenTotal() *agent.TokenUsage {
	var total *agent.TokenUsage
	for _, usage := range l.Children {
		total = types.AddTokenUsage(total, usage)
	}
	return total
}

// withSubagents attaches children to main as SubagentTokens, marked complete:
// the ledger reads every subagent. nil when both are empty.
func withSubagents(main, children *agent.TokenUsage) *agent.TokenUsage {
	children = agent.UsageOrNil(cloneUsage(children))
	if agent.UsageOrNil(cloneUsage(main)) == nil && children == nil {
		return nil
	}
	out := &agent.TokenUsage{}
	if main != nil {
		*out = *main
	}
	if children != nil {
		out.SubagentTokens = children
		complete := true
		out.SubagentTokensComplete = &complete
	}
	return out
}

func cloneUsage(u *agent.TokenUsage) *agent.TokenUsage {
	if u == nil {
		return nil
	}
	c := *u
	c.SubagentTokens = nil
	c.SubagentTokensComplete = nil
	return &c
}
