package session

import (
	"bytes"
	"encoding/json"

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
	// rewritten transcript counts (see Observe). Its fields can be negative.
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
// the old one, never double counts.
func (l *TokenLedger) Observe(obs agent.CumulativeObservation) {
	switch {
	case usageTotal(obs.Total) >= usageTotal(l.Main):
		l.Main = cloneUsage(obs.Total)
	case obs.Restarted:
		l.Base = types.AddTokenUsage(l.Base, signedDiff(l.Main, obs.Total))
		l.Main = cloneUsage(obs.Total)
	}
	for id, usage := range obs.Children {
		if usage == nil {
			continue
		}
		if prev, ok := l.Children[id]; ok && usageTotal(usage) < usageTotal(prev) {
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
	l.Checkpointed = agent.MaxTokenUsage(l.Checkpointed, l.mainTotal())
	l.CheckpointedChildren = agent.MaxTokenUsage(l.CheckpointedChildren, l.childrenTotal())
}

// MarkCheckpointedUpTo marks only what o, an earlier copy of this ledger,
// had read: readings made since stay pending.
func (l *TokenLedger) MarkCheckpointedUpTo(o *TokenLedger) {
	l.Checkpointed = agent.MaxTokenUsage(l.Checkpointed, o.mainTotal())
	l.CheckpointedChildren = agent.MaxTokenUsage(l.CheckpointedChildren, o.childrenTotal())
}

// CheckpointedBeyond reports whether a checkpoint marked tokens since o was
// copied.
func (l *TokenLedger) CheckpointedBeyond(o *TokenLedger) bool {
	return usageTotal(l.Checkpointed) > usageTotal(o.Checkpointed) ||
		usageTotal(l.CheckpointedChildren) > usageTotal(o.CheckpointedChildren)
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
	return clampUsage(types.AddTokenUsage(cloneUsage(l.Base), cloneUsage(l.Main)))
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
	out.SubagentTokens = nil
	out.SubagentTokensComplete = nil
	if children != nil {
		c := *children
		c.SubagentTokens = nil
		c.SubagentTokensComplete = nil
		out.SubagentTokens = &c
		complete := true
		out.SubagentTokensComplete = &complete
	}
	return out
}

func usageTotal(u *agent.TokenUsage) int {
	if u == nil {
		return 0
	}
	return u.InputTokens + u.CacheCreationTokens + u.CacheReadTokens + u.OutputTokens
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

// signedDiff returns a − b per field, which can be negative.
func signedDiff(a, b *agent.TokenUsage) *agent.TokenUsage {
	if a == nil {
		a = &agent.TokenUsage{}
	}
	if b == nil {
		b = &agent.TokenUsage{}
	}
	return &agent.TokenUsage{
		InputTokens:         a.InputTokens - b.InputTokens,
		CacheCreationTokens: a.CacheCreationTokens - b.CacheCreationTokens,
		CacheReadTokens:     a.CacheReadTokens - b.CacheReadTokens,
		OutputTokens:        a.OutputTokens - b.OutputTokens,
		APICallCount:        a.APICallCount - b.APICallCount,
	}
}

// clampUsage floors each field at zero; nil for nil.
func clampUsage(u *agent.TokenUsage) *agent.TokenUsage {
	if u == nil {
		return nil
	}
	return &agent.TokenUsage{
		InputTokens:         max(u.InputTokens, 0),
		CacheCreationTokens: max(u.CacheCreationTokens, 0),
		CacheReadTokens:     max(u.CacheReadTokens, 0),
		OutputTokens:        max(u.OutputTokens, 0),
		APICallCount:        max(u.APICallCount, 0),
	}
}
