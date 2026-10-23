package session

import (
	"bytes"
	"encoding/json"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// TokenLedger keeps a session's token usage as the agent's own running total,
// read from the agent's records (agent.ReadCumulativeUsage), instead of
// summing per-turn slices of the transcript. Every reading replaces the last,
// so a repeated or concurrent Stop, a commit in the middle of a turn, or a
// message whose lines straddle a checkpoint cannot count anything twice. A
// checkpoint stores what the total has grown by since the last checkpoint.
//
// Readings happen under the session lock (see strategy.ObserveTokenLedger),
// so they are ordered: a later reading is of a later transcript.
type TokenLedger struct {
	// Active is set by the first reading that found usage. Until then the
	// session is counted per turn, as before the ledger: an agent version that
	// records no usage in its transcript (older Antigravity) keeps that path.
	Active bool `json:"active,omitempty"`
	// Main is the main agent's total from the latest reading.
	Main *agent.TokenUsage `json:"main,omitempty"`
	// Base is what transcripts rewritten away had counted beyond what the
	// rewritten transcript counts (see Observe). Its fields can be negative.
	Base *agent.TokenUsage `json:"base,omitempty"`
	// Children is each subagent's highest total, keyed by the agent's child ID.
	// A child's file can be deleted, so a child that stops appearing keeps its
	// last total.
	Children map[string]*agent.TokenUsage `json:"children,omitempty"`
	// Cursor lets the next reading continue where the last one stopped.
	Cursor json.RawMessage `json:"cursor,omitempty"`
	// Checkpointed is the main and subagent usage the session's checkpoints
	// already hold, as running totals.
	Checkpointed         *agent.TokenUsage `json:"checkpointed,omitempty"`
	CheckpointedChildren *agent.TokenUsage `json:"checkpointed_children,omitempty"`
}

// Observe folds one reading in.
//
// The main total is taken from the reading unless it is lower than the last.
// A lower total after a restart is a rewritten transcript (a resume restoring
// an older copy): the difference moves to Base so the session total carries
// on from where it was, and calls made after the rewrite add on top. A lower
// total without a restart is a reading of a stale copy of the transcript
// (OpenCode's export) and is ignored. Only an appended transcript can report
// a restart, so for a rewritten file (an OpenCode undo) or a total that also
// rests on a file that does not shrink with the transcript (Droid's
// settings.json), a real rewrite to a lower total is ignored too: calls after
// it count only once the total passes the old one, an undercount rather than
// a double count.
func (l *TokenLedger) Observe(obs agent.CumulativeObservation) {
	switch {
	case usageTotal(obs.Total) >= usageTotal(l.Main):
		l.Main = cloneUsage(obs.Total)
	case obs.Restarted:
		l.Base = addSigned(l.Base, signedDiff(l.Main, obs.Total))
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

// Total returns the session's usage in the shape session state and checkpoint
// metadata use: the main agent's counters, with every subagent's in
// SubagentTokens. nil when nothing was used.
func (l *TokenLedger) Total() *agent.TokenUsage {
	return withSubagents(clampUsage(addSigned(l.Base, l.Main)), l.childrenTotal())
}

// Pending returns what the session used since its last checkpoint, in the same
// shape as Total. nil when nothing is pending.
func (l *TokenLedger) Pending() *agent.TokenUsage {
	main := types.SubtractTokenUsage(clampUsage(addSigned(l.Base, l.Main)), l.Checkpointed)
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

// Activate makes the ledger the session's token source once a reading found
// usage. mainCheckpointed and childrenCheckpointed are what the per-turn
// accounting already stored in checkpoints; they count as checkpointed so the
// ledger's first checkpoint stores only the rest.
func (l *TokenLedger) Activate(mainCheckpointed, childrenCheckpointed *agent.TokenUsage) {
	l.Active = true
	l.Checkpointed = addSigned(l.Checkpointed, mainCheckpointed)
	l.CheckpointedChildren = addSigned(l.CheckpointedChildren, cloneUsage(childrenCheckpointed))
}

// MarkCheckpointed records that a checkpoint now holds everything observed.
// It never moves back, so a checkpoint written from an older reading cannot
// make tokens pending again.
func (l *TokenLedger) MarkCheckpointed() {
	l.Checkpointed = maxUsage(l.Checkpointed, clampUsage(addSigned(l.Base, l.Main)))
	l.CheckpointedChildren = maxUsage(l.CheckpointedChildren, l.childrenTotal())
}

// MarkCheckpointedUpTo records that a checkpoint holds everything o, an
// earlier copy of this ledger, had read. Readings this ledger made since stay
// pending. Like MarkCheckpointed, it never moves back.
func (l *TokenLedger) MarkCheckpointedUpTo(o *TokenLedger) {
	l.Checkpointed = maxUsage(l.Checkpointed, clampUsage(addSigned(o.Base, o.Main)))
	l.CheckpointedChildren = maxUsage(l.CheckpointedChildren, o.childrenTotal())
}

// CheckpointedBeyond reports whether a checkpoint marked tokens since o, an
// earlier copy of this ledger, was taken.
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

func (l *TokenLedger) childrenTotal() *agent.TokenUsage {
	var total *agent.TokenUsage
	for _, usage := range l.Children {
		total = types.AddTokenUsage(total, usage)
	}
	return total
}

// withSubagents attaches children to main as SubagentTokens, marked complete:
// the ledger reads every subagent the agent reports. nil when both are empty.
func withSubagents(main, children *agent.TokenUsage) *agent.TokenUsage {
	if isEmptyUsage(children) {
		children = nil
	}
	if isEmptyUsage(main) && children == nil {
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

func isEmptyUsage(u *agent.TokenUsage) bool {
	return u == nil || (usageTotal(u) == 0 && u.APICallCount == 0)
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

// addSigned adds a and b per field without clamping; nil when both are.
func addSigned(a, b *agent.TokenUsage) *agent.TokenUsage {
	return types.AddTokenUsage(cloneUsage(a), cloneUsage(b))
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

// maxUsage returns the per-field maximum; nil when both are nil.
func maxUsage(a, b *agent.TokenUsage) *agent.TokenUsage {
	if a == nil || b == nil {
		return cloneUsage(types.AddTokenUsage(a, b))
	}
	return &agent.TokenUsage{
		InputTokens:         max(a.InputTokens, b.InputTokens),
		CacheCreationTokens: max(a.CacheCreationTokens, b.CacheCreationTokens),
		CacheReadTokens:     max(a.CacheReadTokens, b.CacheReadTokens),
		OutputTokens:        max(a.OutputTokens, b.OutputTokens),
		APICallCount:        max(a.APICallCount, b.APICallCount),
	}
}
