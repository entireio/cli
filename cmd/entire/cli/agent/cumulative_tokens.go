package agent

import (
	"context"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// CumulativeTokenInput identifies the session whose running token total is
// read. Agents use the fields their native format needs and ignore the rest.
type CumulativeTokenInput struct {
	SessionID string
	// TranscriptPath locates files beside the transcript (Claude Code
	// subagents, Droid settings.json and Workers).
	TranscriptPath string
	// TranscriptData is the whole live transcript, already read by the caller.
	TranscriptData []byte
	// Subagents and Home are the session's authoritative child inventory and
	// recorded agent home (see ExtractWithSubagentInventory), for agents whose
	// children are not discoverable from the transcript (Codex).
	Subagents []SubagentReference
	Home      string
}

// CumulativeTokenSource reports a session's token usage as a running total
// since the session began, read from the whole transcript on every call.
//
// Unlike TokenCalculator it takes no offset: a caller turns totals into
// per-checkpoint deltas by subtracting an earlier total, so each call must
// count every API call the session has made so far exactly once, and total
// must never decrease as the transcript grows.
//
// total is the main agent only and never carries SubagentTokens. children holds
// each spawned subagent's own running total keyed by a stable child ID (Claude
// Code agent ID, Droid Worker session ID, Codex child agent ID); callers decide
// how to fold them in. A child can vanish from a later call when its file is
// deleted, so callers must keep a per-child high-water mark rather than
// replace. Both are nil when the agent recorded no usage — nil means "no
// data", not zero. Field meanings follow types.TokenUsage:
// InputTokens excludes cache reads and writes, OutputTokens includes reasoning.
type CumulativeTokenSource interface {
	Agent

	CumulativeTokenUsage(ctx context.Context, in CumulativeTokenInput) (total *TokenUsage, children map[string]*TokenUsage, err error)
}

// SumTokenUsageByKey sums usage recorded once per key — an API call ID, a
// message ID, a turn's generation ID — so a call observed more than once is
// counted once. Returns nil for an empty map.
func SumTokenUsageByKey(byKey map[string]*TokenUsage) *TokenUsage {
	var total *TokenUsage
	for _, usage := range byKey {
		total = types.AddTokenUsage(total, usage)
	}
	return total
}

// SumCumulativeSegments totals a sequence of running-total snapshots, in file
// order, from a source that restarts its counter (a resumed process). A
// snapshot below its predecessor starts a new segment and the previous
// segment's last snapshot is banked; an equal or larger one continues the
// segment, so a replayed total is not counted twice. Returns nil when there
// are no snapshots.
//
// LIMITATION: a restart is visible only when the new segment's first snapshot
// is below the old segment's last. A process that restarts and reports more
// in its first snapshot than the previous process did in total (one call of
// 1,520 after a segment ending at 1,010) reads as one segment, undercounting
// by the earlier segment. Resume markers cannot settle it in general: Copilot's
// shutdown totals carry on across session.resume. Agents with per-call
// records should take MaxTokenUsage of the records sum and this.
func SumCumulativeSegments(snapshots []*TokenUsage) *TokenUsage {
	var banked, last *TokenUsage
	for _, snapshot := range snapshots {
		if snapshot == nil {
			continue
		}
		if last != nil && tokenTotal(snapshot) < tokenTotal(last) {
			banked = types.AddTokenUsage(banked, last)
		}
		last = snapshot
	}
	return types.AddTokenUsage(banked, last)
}

// MaxTokenUsage returns the per-field maximum of a and b, ignoring subagent
// usage. It combines two lower bounds on the same running total — a sum of
// per-call records and a segment sum of running totals — so usage that only
// one source saw (calls from before an agent began writing records) is kept
// and the result never decreases while both inputs grow. nil when both are.
func MaxTokenUsage(a, b *TokenUsage) *TokenUsage {
	if a == nil || b == nil {
		return types.AddTokenUsage(a, b)
	}
	return &TokenUsage{
		InputTokens:         max(a.InputTokens, b.InputTokens),
		CacheCreationTokens: max(a.CacheCreationTokens, b.CacheCreationTokens),
		CacheReadTokens:     max(a.CacheReadTokens, b.CacheReadTokens),
		OutputTokens:        max(a.OutputTokens, b.OutputTokens),
		APICallCount:        max(a.APICallCount, b.APICallCount),
	}
}

// UsageOrNil maps usage with no tokens and no calls to nil, the "no data"
// value CumulativeTokenUsage returns. Agents can write all-zero placeholders
// (Droid's settings.json before its first update, an idle Copilot shutdown).
func UsageOrNil(u *TokenUsage) *TokenUsage {
	if u == nil || (tokenTotal(u) == 0 && u.APICallCount == 0 && u.SubagentTokens == nil) {
		return nil
	}
	return u
}

func tokenTotal(u *TokenUsage) int {
	return u.InputTokens + u.CacheReadTokens + u.CacheCreationTokens + u.OutputTokens
}

// NonCacheInput returns the fresh-input share of a cache-inclusive input count,
// floored at zero, for agents that report input with cache reads and writes
// folded in.
func NonCacheInput(inclusiveInput, cacheRead, cacheWrite int) int {
	return max(inclusiveInput-cacheRead-cacheWrite, 0)
}
