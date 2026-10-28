package transcript

import "bytes"

// CodexRolloutAdapter turns a Codex rollout that takes the item path into
// transcript records as it grows. It uses Convert's item-path line mapping, so
// folding every record it returns reproduces Convert's output for the same
// complete lines, usage and model included.
//
// Use it only for a rollout CodexItemRolloutDecided accepts (decided, item
// true); other rollouts convert through the response_item path, which it
// doesn't implement. Usage-only lines are keyed by their line number, counted
// from 1 over every line fed, empty ones included, so a consumer that
// resumes replays from line 1 with a fresh adapter: there is no StartLine and
// no head overlap. A final line without a newline is held until it completes.
type CodexRolloutAdapter struct {
	conv    codexItemsConverter
	pending []byte // bytes after the last newline fed
	line    int    // lines consumed so far
}

// NewCodexRolloutAdapter returns an adapter for a Codex item-path rollout,
// to be fed from its first line.
func NewCodexRolloutAdapter() *CodexRolloutAdapter {
	return &CodexRolloutAdapter{conv: codexItemsConverter{tracker: newCodexUsageTracker()}}
}

// Feed consumes the next chunk of the rollout and returns the records of
// every line it completed. A trailing partial line is held until a later
// chunk completes it.
func (a *CodexRolloutAdapter) Feed(chunk []byte) []Record {
	var out []Record
	for len(chunk) > 0 {
		i := bytes.IndexByte(chunk, '\n')
		if i < 0 {
			a.pending = append(a.pending, chunk...)
			break
		}
		line := chunk[:i+1]
		if len(a.pending) > 0 {
			a.pending = append(a.pending, line...)
			line = a.pending
		}
		a.line++
		out = append(out, a.conv.line(line, a.line)...)
		a.pending = a.pending[:0]
		chunk = chunk[i+1:]
	}
	return out
}
