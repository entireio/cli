package agentimport

import "time"

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
	return turns, nil
}
