package transcript

import "bytes"

// CodexRolloutAdapter turns a Codex rollout that takes the item path into
// transcript records as it grows. It uses Convert's item-path line mapping, so
// folding every record from Feed and a final Flush reproduces Convert's
// output for the same bytes, usage and model included.
//
// Use it only for a rollout decided item true (CodexItemRolloutDecider, or
// CodexItemRolloutDecided on the whole file). A rollout decided false, or
// still undecided once final, converts through the response_item path,
// which it doesn't implement; while undecided, wait for the decision.
// Usage-only lines are keyed by their line number, counted
// from 1 over every line fed, empty ones included, so a consumer that
// resumes replays from line 1 with a fresh adapter: there is no StartLine and
// no head overlap. A final line without a newline is held until it completes,
// or until Flush.
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

// Lines returns the number of source lines consumed so far: every
// newline-terminated line fed, including blank and skipped ones, plus a
// partial line once Flush has consumed it. A held partial line is not
// counted. Line-keyed usage records use the same numbering.
func (a *CodexRolloutAdapter) Lines() int {
	return a.line
}

// Feed consumes the next chunk of the rollout and returns the records of
// every line it completed. A trailing partial line is held until a later
// chunk completes it, or until Flush.
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

// Flush treats a held partial line as complete. Call it only once the
// rollout is final; while Codex is still writing, a partial line is usually
// an unfinished write.
func (a *CodexRolloutAdapter) Flush() []Record {
	if len(a.pending) == 0 {
		return nil
	}
	line := a.pending
	a.pending = nil
	a.line++
	return a.conv.line(line, a.line)
}
