package agentimport

import (
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// parseTimestamp parses an RFC3339 timestamp, returning the zero time when the
// string is empty or unparseable. Shared by the importers that read a per-turn
// timestamp off the transcript line.
func parseTimestamp(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// splitLineTurns is the shared per-turn scaffolding for line-based (JSONL)
// importers. It finds the user-prompt turn starts with isPrompt, then for each
// turn spanning raw lines [start, end) calls build to fill the agent-specific
// fields (prompt, uuid, model, timestamp, tokens). LineStart/LineEnd are set
// here, and `truncated` is the [0,end) buffer the agents' token helpers consume
// (truncating the end bounds the turn while keeping the file's beginning, which
// branch-aware agents like Pi need). build may return a nil Turn to skip a
// start defensively (e.g. a line that unexpectedly fails to parse).
//
// Gemini imports per-session and does not use this — its transcript is a single
// JSON document, not newline-delimited records.
func splitLineTurns(
	rawLines [][]byte,
	isPrompt func(raw []byte) bool,
	build func(rawLines [][]byte, start, end int, truncated []byte) (*Turn, error),
) ([]Turn, error) {
	var starts []int
	for i, raw := range rawLines {
		if isPrompt(raw) {
			starts = append(starts, i)
		}
	}

	turns := make([]Turn, 0, len(starts))
	for k, start := range starts {
		end := len(rawLines)
		if k+1 < len(starts) {
			end = starts[k+1]
		}
		turn, err := build(rawLines, start, end, joinLines(rawLines[:end]))
		if err != nil {
			return nil, err
		}
		if turn == nil {
			continue
		}
		turn.LineStart, turn.LineEnd = start, end
		turns = append(turns, *turn)
	}
	rescopeSubagentTokensToDeltas(turns)
	return turns, nil
}

// rescopeSubagentTokensToDeltas converts each turn's SubagentTokens from the
// cumulative-since-session-start snapshot the token helpers return into the
// per-turn increment (this turn's cumulative minus the previous turn's).
//
// The subagent-aware token helpers (claudecode/factoryaidroid
// CalculateTotalTokenUsage) discover spawned agent IDs from the full transcript
// prefix [0,end) — so a subagent spawned before the current turn is still found
// (#329) — and re-read each agent-<id>.jsonl from line 0. That makes a turn's
// SubagentTokens a cumulative snapshot that repeats every already-discovered
// subagent's full total on every later turn, unlike the main-agent fields
// (InputTokens/OutputTokens/...), which are scoped to the turn's own
// [start,end) slice and are genuine per-turn deltas.
//
// Both import consumers sum per-turn token usage: writeSessionState folds the
// turns together with AddTokenUsage for the session total, and every imported
// checkpoint stores its turn's TokenUsage (downstream consumers sum those).
// Summing the cumulative snapshot multiplies a subagent's tokens by the number
// of turns after it is first discovered (trail finding 019f5ea3). Rescoping to
// per-turn deltas fixes both without special-casing either: each checkpoint
// carries only the subagent usage attributable to its turn, and summing the
// deltas reconstructs the final cumulative total exactly once.
//
// This mirrors the live path, which keeps the latest cumulative snapshot in
// state.TokenUsage (accumulateTokenUsage replaces rather than adds
// SubagentTokens) and rescopes each checkpoint window to "cumulative minus a
// captured baseline" via types.SubtractTokenUsage and
// SessionState.SubagentTokensBaseline (see cmd/entire/cli/strategy). Here the
// baseline for turn k is turn k-1's cumulative snapshot. The cumulative is
// monotonic non-decreasing across turns (discovered-agent set only grows and
// each subagent file total is fixed), so the clamped subtraction is exact and
// the deltas sum back to the final snapshot.
//
// Turns without a discovered subagent have a nil SubagentTokens and are left
// untouched, so this is a no-op for the non-subagent-aware importers that route
// through splitLineTurns (cursor/pi/codex/copilot).
func rescopeSubagentTokensToDeltas(turns []Turn) {
	var prevCumulative *types.TokenUsage
	for i := range turns {
		if turns[i].Tokens == nil {
			continue
		}
		cumulative := turns[i].Tokens.SubagentTokens
		turns[i].Tokens.SubagentTokens = types.SubtractTokenUsage(cumulative, prevCumulative)
		// Only advance the baseline when this turn carried a snapshot. A turn
		// whose agent-<id>.jsonl transiently failed to read has a nil cumulative
		// (CalculateTotalTokenUsage continue-s past the error); resetting
		// prevCumulative to nil here would make the next non-nil snapshot subtract
		// nothing and re-report the full cumulative, reintroducing the
		// double-counting this rescoping removes. Mirrors the live path, where
		// accumulateTokenUsage only replaces SubagentTokens when the incoming
		// snapshot is non-nil.
		if cumulative != nil {
			prevCumulative = cumulative
		}
	}
}
