package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/entireio/cli/internal/transcript/codexitem"
)

// codexEvent is a rollout line's event_msg payload type.
type codexEvent struct {
	Type string `json:"type"`
}

// codexEventOf is line's event_msg payload; ok is false for other lines.
func codexEventOf(line []byte) (codexEvent, bool) {
	var l codexLine
	if json.Unmarshal(line, &l) != nil || l.Type != codexTypeEventMsg {
		return codexEvent{}, false
	}
	var ev codexEvent
	if json.Unmarshal(l.Payload, &ev) != nil {
		return codexEvent{}, false
	}
	return ev, true
}

// hasCodexItems reports whether a Codex rollout takes the item path: whether
// CodexItemRolloutDecided decides it does. A rollout that never decides (no
// task_started, or a first turn still running without a user item) keeps the
// response_item path. Decided on the whole content, before any slicing, so
// Convert and ConvertWithBoundary always take the same path; a rollout begun
// on an older Codex keeps the old path.
func hasCodexItems(content []byte) bool {
	if !bytes.Contains(content, []byte(`"item_completed"`)) {
		return false
	}
	item, decided := CodexItemRolloutDecided(content)
	return item && decided
}

// CodexItemRolloutDecided reports whether a Codex rollout's first turn has a
// user message item_completed event: whether the rollout was written by a
// Codex version that records the user's prompts as items, and so converts
// through its items. It is a CodexItemRolloutDecider fed all of content and
// flushed; see that type for the rules. Convert takes the item path only for
// a decided true: content that leaves it undecided converts through the
// response_item path.
func CodexItemRolloutDecided(content []byte) (item, decided bool) {
	d := NewCodexItemRolloutDecider()
	d.Feed(content)
	return d.Flush()
}

// CodexItemRolloutDecider makes CodexItemRolloutDecided's decision as a
// rollout grows, reading each line once, so a live reader can feed it the
// bytes appended since its last read instead of rescanning the file.
//
// The first turn runs from the first task_started event to the first
// task_complete or turn_aborted event, or to the next task_started if none
// comes first. The decision is item true as soon as the first turn holds a
// user message item_completed event, and item false once the first turn ends
// without one. Before that (only session_meta, no task_started yet, or a
// first turn still running with no user item yet) it is undecided. Once
// decided it never changes as the rollout grows, so a decision from a prefix
// of the file holds for the whole file, and the decider ignores what it is
// fed after.
//
// A rollout that is complete but still undecided converts through the
// response_item path, as if decided false: Convert does that. A live reader
// that sees undecided keeps feeding new bytes; once the session has ended
// (the rollout is final) it calls Flush, and a result still undecided means
// false. Rollouts that stay undecided to the end come from Codex versions
// that write no task_started, or end inside a first turn that has no user
// item.
//
// Other items alone don't count. Older rollouts record a sub-agent's activity
// and reasoning as items too, and Codex can write a prompt only as a
// response_item while the turn's agent message is an item (guardian
// sub-sessions); the item path would drop those prompts. A thread_spawn
// sub-agent's rollout has no items in its first turn and gets its task prompt
// as a response_item, so it keeps the response_item path as well.
type CodexItemRolloutDecider struct {
	lines       lineSplitter // while undecided
	inFirstTurn bool
	item        bool
	decided     bool
}

// NewCodexItemRolloutDecider returns a decider to be fed a rollout from its
// first byte.
func NewCodexItemRolloutDecider() *CodexItemRolloutDecider {
	return &CodexItemRolloutDecider{}
}

// Feed consumes the next chunk of the rollout and returns the decision after
// every line it completed. A trailing partial line is held until a later
// chunk completes it, or until Flush.
func (d *CodexItemRolloutDecider) Feed(chunk []byte) (item, decided bool) {
	if !d.decided {
		d.lines.feed(chunk, func(line []byte) bool {
			d.line(line)
			return !d.decided
		})
	}
	return d.item, d.decided
}

// Flush treats a held partial line as complete and returns the decision.
// Call it only once the rollout is final; while Codex is still writing, a
// partial line is usually an unfinished write. Undecided after Flush means
// the rollout takes the response_item path.
func (d *CodexItemRolloutDecider) Flush() (item, decided bool) {
	if line := d.lines.flush(); !d.decided && line != nil {
		d.line(line)
	}
	return d.item, d.decided
}

// line reads one complete line. The substring checks only skip lines
// cheaply: an item's or a turn's text may contain any of these words, so the
// event's type decides.
func (d *CodexItemRolloutDecider) line(line []byte) {
	if !bytes.Contains(line, []byte(`"event_msg"`)) {
		return
	}
	if !bytes.Contains(line, []byte(`"task_started"`)) && (!d.inFirstTurn ||
		!bytes.Contains(line, []byte(`"item_completed"`)) &&
			!bytes.Contains(line, []byte(`"task_complete"`)) &&
			!bytes.Contains(line, []byte(`"turn_aborted"`))) {
		return
	}
	ev, ok := codexEventOf(line)
	switch {
	case !ok:
	case ev.Type == "task_started" && !d.inFirstTurn:
		d.inFirstTurn = true
	case !d.inFirstTurn:
	case ev.Type == "item_completed":
		if it, ok := codexitem.DecodeRollout(line); ok && it.Kind == codexitem.KindUser {
			d.item, d.decided = true, true
		}
	case ev.Type == "task_started", ev.Type == "task_complete", ev.Type == "turn_aborted":
		// The first turn ended without a user item.
		d.decided = true
	}
}

// compactCodexItems converts a rollout through its item_completed items:
// codexItemRecords of each, in file order, folded. Usage follows the
// response_item path's rules (see compactCodex): a model call is the items up
// to the token_count snapshot that closes it, its usage is the difference
// between consecutive total_token_usage snapshots (codexUsageTracker), and its
// model is the latest turn_context's. The usage goes on the call's last
// assistant line; a call with usage but no assistant line of its own
// (reasoning only) gets a usage-only line. With StartLine, the skipped lines
// seed the usage baseline, and the last line before the boundary is part of
// the slice when the slice changes it (a one-line head overlap).
func compactCodexItems(content []byte, opts Options) ([]byte, error) {
	full := content
	cut := 0
	if opts.StartLine > 0 {
		rest := SliceFromLine(full, opts.StartLine)
		if rest == nil {
			return []byte{}, nil
		}
		cut = len(full) - len(rest)
	}
	c := &codexItemsConverter{folder: NewFolder(opts), tracker: newCodexUsageTracker()}
	if err := c.consume(full[:cut], 1); err != nil {
		return nil, err
	}
	before := codexSplitLines(c.folder.Lines())
	if err := c.consume(full[cut:], opts.StartLine); err != nil {
		return nil, err
	}
	after := codexSplitLines(c.folder.Lines())
	start := len(before)
	if start > 0 && !bytes.Equal(before[start-1], after[start-1]) {
		start--
	}
	return bytes.Join(after[start:], nil), nil
}

// codexItemsConverter folds a rollout's items and attaches each model call's
// usage.
type codexItemsConverter struct {
	folder  *Folder
	tracker *codexUsageTracker
	model   string
	cwd     string // the latest turn_context's working directory
	// callAssistant is the last assistant message of the open call, and
	// callItem the last item of any kind in it; both empty when no call is
	// open.
	callAssistant string
	callItem      string
}

// consume applies the lines of content, the first of which is line number
// first of the rollout.
func (c *codexItemsConverter) consume(content []byte, first int) error {
	reader := bufio.NewReader(bytes.NewReader(content))
	for n := first; ; n++ {
		line, err := reader.ReadBytes('\n')
		if err != nil && err != io.EOF {
			return fmt.Errorf("reading codex line %d: %w", n, err)
		}
		if records := c.line(line, n); len(records) > 0 {
			if _, applyErr := c.folder.Apply(records); applyErr != nil {
				return fmt.Errorf("codex line %d: %w", n, applyErr)
			}
		}
		if err == io.EOF {
			return nil
		}
	}
}

// line returns the records of rollout line n, and tracks the open call.
func (c *codexItemsConverter) line(line []byte, n int) []Record {
	// The substring checks only skip lines cheaply: an item's text may
	// contain any of these words, so each case confirms the line's type.
	if bytes.Contains(line, []byte(`"turn_context"`)) && c.turnContext(line) {
		return nil
	}
	switch {
	case !bytes.Contains(line, []byte(`"event_msg"`)):
		// Not an item or a token count: response_item, session_meta, ...
	case bytes.Contains(line, []byte(`"item_completed"`)):
		// Items it can't read or doesn't show give no records.
		it, ok := codexitem.DecodeRollout(line)
		if !ok {
			return nil
		}
		records := codexItemRecords(it, c.cwd)
		if it.Kind == codexitem.KindUser {
			// A user message ends the open call, as a user line does on the
			// response_item path.
			c.callAssistant, c.callItem = "", ""
			return records
		}
		c.callItem = it.ID
		for _, r := range records {
			if r.Op == OpMessage && r.Role == TypeAssistant {
				c.callAssistant = r.ID
			}
		}
		return records
	case bytes.Contains(line, []byte(`"token_count"`)):
		return c.tokenCount(line, n)
	}
	return nil
}

// turnContext reports whether line is a turn_context line, and takes the
// model and working directory it names. A cwd that isn't a string is ignored,
// and the model is still read.
func (c *codexItemsConverter) turnContext(line []byte) bool {
	var cl codexLine
	if json.Unmarshal(line, &cl) != nil || cl.Type != codexTypeTurnContext {
		return false
	}
	var tc struct {
		Model string          `json:"model"`
		CWD   json.RawMessage `json:"cwd"`
	}
	if json.Unmarshal(cl.Payload, &tc) != nil {
		return true
	}
	if tc.Model != "" {
		c.model = tc.Model
	}
	var cwd string
	if json.Unmarshal(tc.CWD, &cwd) == nil && cwd != "" {
		c.cwd = codexitem.PathOfFileURI(cwd)
	}
	return true
}

// tokenCount closes the open call at a new total_token_usage snapshot and
// returns the records that give it its usage. Repeated snapshots change
// nothing; once a counter decrease has stopped usage, snapshots still close
// calls but carry none.
func (c *codexItemsConverter) tokenCount(line []byte, n int) []Record {
	var cl codexLine
	if json.Unmarshal(line, &cl) != nil || cl.Type != codexTypeEventMsg {
		return nil
	}
	total, ok := codexTokenCountTotal(cl.Payload)
	if !ok {
		return nil
	}
	d, ok := c.tracker.next(total)
	if !ok {
		if c.tracker.stopped() {
			c.callAssistant, c.callItem = "", ""
		}
		return nil
	}
	d.Model = c.model
	var records []Record
	id := c.callAssistant
	if id == "" {
		// No assistant line in this call: a usage-only line of its own,
		// keyed apart from item IDs, without a printed id.
		id = "codex-call:" + c.callItem
		if c.callItem == "" {
			id = fmt.Sprintf("codex-call:line-%d", n)
		}
		records = append(records, Record{Op: OpMessage, ID: id, Role: TypeAssistant, TS: codexTSJSON(cl.ts())})
	}
	c.callAssistant, c.callItem = "", ""
	return append(records, Record{Op: OpUsage, ID: id, Usage: &d})
}

// codexSplitLines splits folded output into its lines, newline included.
func codexSplitLines(b []byte) [][]byte {
	return bytes.SplitAfter(b, []byte("\n"))[:bytes.Count(b, []byte("\n"))]
}
