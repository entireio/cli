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
// count every API call the session has made so far exactly once, and a later
// call over a longer transcript must never report less.
//
// total is the main agent only and never carries SubagentTokens. children holds
// each spawned subagent's own running total keyed by a stable child ID (Claude
// Code agent ID, Droid Worker session ID, Codex child agent ID); callers decide
// how to fold them in. Both are nil when the agent recorded no usage — nil
// means "no data", not zero. Field meanings follow types.TokenUsage:
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

func tokenTotal(u *TokenUsage) int {
	return u.InputTokens + u.CacheReadTokens + u.CacheCreationTokens + u.OutputTokens
}

// NonCacheInput returns the fresh-input share of a cache-inclusive input count,
// floored at zero, for agents that report input with cache reads and writes
// folded in.
func NonCacheInput(inclusiveInput, cacheRead, cacheWrite int) int {
	return max(inclusiveInput-cacheRead-cacheWrite, 0)
}
