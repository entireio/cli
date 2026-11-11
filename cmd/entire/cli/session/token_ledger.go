package session

import (
	"bytes"
	"encoding/json"
	"maps"
	"reflect"

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
	// UncountedChildren holds each subagent a reading knew of but could not
	// count, so the children's total is not exact: Children lacks it, or has
	// only a total it may have grown past. It stays here, even once no
	// reading lists it (its file deleted), until a reading counts it.
	UncountedChildren map[string]bool `json:"uncounted_children,omitempty"`
	// LastCounted holds the subagents the latest reading counted. Children
	// can hold a total for others, kept from an earlier reading.
	LastCounted map[string]bool `json:"last_counted,omitempty"`
	// Cursor lets the next reading continue where the last one stopped.
	Cursor json.RawMessage `json:"cursor,omitempty"`
	// Checkpointed is what the session's checkpoints already hold.
	Checkpointed         *agent.TokenUsage `json:"checkpointed,omitempty"`
	CheckpointedChildren *agent.TokenUsage `json:"checkpointed_children,omitempty"`
	// CheckpointedUncounted is UncountedChildren when the last checkpoint
	// was marked: what the checkpoints hold of those subagents is unknown,
	// so a delta counting them is not exact either.
	CheckpointedUncounted map[string]bool `json:"checkpointed_uncounted,omitempty"`
	// CheckpointedCounted holds the subagents the reading behind the last
	// mark counted, so attach can tell which of the ones its reading could
	// not count a checkpoint marked since holds.
	CheckpointedCounted map[string]bool `json:"checkpointed_counted,omitempty"`
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
	l.LastCounted = nil
	for id, usage := range obs.Children {
		if usage == nil {
			l.markUncounted(id)
			continue
		}
		delete(l.UncountedChildren, id)
		l.LastCounted = addID(l.LastCounted, id)
		if prev, ok := l.Children[id]; ok && agent.TokenTotal(usage) < agent.TokenTotal(prev) {
			continue
		}
		if l.Children == nil {
			l.Children = make(map[string]*agent.TokenUsage)
		}
		l.Children[id] = cloneUsage(usage)
	}
	if len(l.UncountedChildren) == 0 {
		l.UncountedChildren = nil
	}
	if obs.Cursor != nil {
		l.Cursor = obs.Cursor
	}
}

// markUncounted records that a reading could not count subagent id. A total
// an earlier reading counted stands, but may be short of what id used since.
func (l *TokenLedger) markUncounted(id string) {
	l.UncountedChildren = addID(l.UncountedChildren, id)
}

// Total returns the session's usage, subagents in SubagentTokens; nil when
// nothing was used.
func (l *TokenLedger) Total() *agent.TokenUsage {
	return withSubagents(l.mainTotal(), l.childrenTotal(), len(l.UncountedChildren) == 0)
}

// Pending returns what the session used since its last checkpoint.
func (l *TokenLedger) Pending() *agent.TokenUsage {
	main := types.SubtractTokenUsage(l.mainTotal(), l.Checkpointed)
	children := types.SubtractTokenUsage(l.childrenTotal(), l.CheckpointedChildren)
	return withSubagents(main, children, len(l.UncountedChildren) == 0 && len(l.CheckpointedUncounted) == 0)
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
	l.CheckpointedUncounted = maps.Clone(l.UncountedChildren)
	l.CheckpointedCounted = maps.Clone(l.LastCounted)
}

// MarkCheckpointedUpTo marks only what o, an earlier copy of this ledger,
// had read: readings made since stay pending. A checkpoint marked since o
// was read (a hook's, while attach waited) can hold more than o did; see
// childrenAtMark for which subagents stay unknown then.
func (l *TokenLedger) MarkCheckpointedUpTo(o *TokenLedger) {
	read := o.childrenTotal()
	uncounted, counted := maps.Clone(o.UncountedChildren), maps.Clone(o.LastCounted)
	if l.markedSince(o) {
		uncounted, counted = l.childrenAtMark(o, read)
	}
	l.Checkpointed = agent.MaxTokenUsage(l.Checkpointed, o.mainTotal())
	l.CheckpointedChildren = agent.MaxTokenUsage(l.CheckpointedChildren, read)
	l.CheckpointedUncounted = uncounted
	l.CheckpointedCounted = counted
}

// childrenAtMark returns the subagents unknown to, and counted by, the
// children's mark once o's reading (children total read) is marked over a
// newer checkpoint's. The mark keeps the higher total in each field. When one
// side holds at least the other's total in every field, its mark stands:
// what its reading could not count is unknown, and so is each subagent the
// other could not count that its reading did not count either (one it never
// listed, or held only a total from an earlier reading for). Otherwise the
// mark mixes both, and every subagent either could not count is unknown.
func (l *TokenLedger) childrenAtMark(o *TokenLedger, read *agent.TokenUsage) (uncounted, counted map[string]bool) {
	switch {
	case covers(l.CheckpointedChildren, read):
		return unknownAtMark(l.CheckpointedUncounted, l.CheckpointedCounted, o.UncountedChildren), maps.Clone(l.CheckpointedCounted)
	case covers(read, l.CheckpointedChildren):
		return unknownAtMark(o.UncountedChildren, o.LastCounted, l.CheckpointedUncounted), maps.Clone(o.LastCounted)
	}
	uncounted = maps.Clone(o.UncountedChildren)
	for id := range l.CheckpointedUncounted {
		uncounted = addID(uncounted, id)
	}
	for _, ids := range []map[string]bool{o.LastCounted, l.CheckpointedCounted} {
		for id := range ids {
			if !uncounted[id] {
				counted = addID(counted, id)
			}
		}
	}
	return uncounted, counted
}

// unknownAtMark keeps a subagent the losing side could not count unknown
// unless the winning reading counted it, because otherwise the winning total
// holds none of what it used, or only an earlier total, and the delta that
// next counts it includes usage from before the mark.
func unknownAtMark(uncounted, counted, other map[string]bool) map[string]bool {
	unknown := maps.Clone(uncounted)
	for id := range other {
		if !counted[id] {
			unknown = addID(unknown, id)
		}
	}
	return unknown
}

func addID(set map[string]bool, id string) map[string]bool {
	if set == nil {
		set = make(map[string]bool)
	}
	set[id] = true
	return set
}

// covers reports whether a is at least b in every field, so a mark of a
// holds everything a mark of b would. nil is no usage.
func covers(a, b *agent.TokenUsage) bool {
	if b == nil {
		return true
	}
	have := a
	if have == nil {
		have = &agent.TokenUsage{}
	}
	return have.InputTokens >= b.InputTokens &&
		have.CacheCreationTokens >= b.CacheCreationTokens &&
		have.CacheReadTokens >= b.CacheReadTokens &&
		have.OutputTokens >= b.OutputTokens &&
		have.APICallCount >= b.APICallCount
}

// markedSince reports whether a checkpoint was marked on l after o, an
// earlier copy of it, was taken.
func (l *TokenLedger) markedSince(o *TokenLedger) bool {
	return !reflect.DeepEqual(l.Checkpointed, o.Checkpointed) ||
		!reflect.DeepEqual(l.CheckpointedChildren, o.CheckpointedChildren) ||
		!maps.Equal(l.CheckpointedUncounted, o.CheckpointedUncounted) ||
		!maps.Equal(l.CheckpointedCounted, o.CheckpointedCounted)
}

// Clone returns a deep copy.
func (l *TokenLedger) Clone() *TokenLedger {
	if l == nil {
		return nil
	}
	c := &TokenLedger{
		Active:                l.Active,
		UncountedChildren:     maps.Clone(l.UncountedChildren),
		LastCounted:           maps.Clone(l.LastCounted),
		CheckpointedUncounted: maps.Clone(l.CheckpointedUncounted),
		CheckpointedCounted:   maps.Clone(l.CheckpointedCounted),
		Main:                  cloneUsage(l.Main),
		Base:                  cloneUsage(l.Base),
		Cursor:                bytes.Clone(l.Cursor),
		Checkpointed:          cloneUsage(l.Checkpointed),
		CheckpointedChildren:  cloneUsage(l.CheckpointedChildren),
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

// withSubagents attaches children to main as SubagentTokens, marked complete
// unless a subagent the ledger knows of was never counted (then marked
// incomplete even with no subagent total). nil when both are empty.
func withSubagents(main, children *agent.TokenUsage, complete bool) *agent.TokenUsage {
	children = agent.UsageOrNil(cloneUsage(children))
	if agent.UsageOrNil(cloneUsage(main)) == nil && children == nil {
		return nil
	}
	out := &agent.TokenUsage{}
	if main != nil {
		*out = *main
	}
	if children != nil || !complete {
		out.SubagentTokens = children
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
