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

// hasCodexItems reports whether a Codex rollout's first turn (from its first
// task_started event to the next) has a user message item_completed event:
// rollouts written by Codex versions that record the user's prompts as items.
// Other items alone don't count. Older rollouts record a sub-agent's activity
// and reasoning as items too, and Codex can write a prompt only as a
// response_item while the turn's agent message is an item (guardian
// sub-sessions); the item path would drop those prompts. A thread_spawn
// sub-agent's rollout has no items in its first turn and gets its task prompt
// as a response_item, so it keeps the response_item path as well. Decided on the whole content, before any slicing, so Convert and
// ConvertWithBoundary always take the same path; a rollout begun on an older
// Codex keeps the old path.
func hasCodexItems(content []byte) bool {
	if !bytes.Contains(content, []byte(`"item_completed"`)) {
		return false
	}
	inFirstTurn := false
	reader := bufio.NewReader(bytes.NewReader(content))
	for {
		line, err := reader.ReadBytes('\n')
		switch {
		case !bytes.Contains(line, []byte(`"event_msg"`)):
		case inFirstTurn && bytes.Contains(line, []byte(`"item_completed"`)):
			if it, ok := codexitem.DecodeRollout(line); ok && it.Kind == codexitem.KindUser {
				return true
			}
		case bytes.Contains(line, []byte(`"task_started"`)):
			if ev, ok := codexEventOf(line); ok && ev.Type == "task_started" {
				if inFirstTurn {
					return false
				}
				inFirstTurn = true
			}
		}
		if err != nil {
			return false
		}
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
		records := codexItemRecords(it)
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
// model it names.
func (c *codexItemsConverter) turnContext(line []byte) bool {
	var cl codexLine
	if json.Unmarshal(line, &cl) != nil || cl.Type != codexTypeTurnContext {
		return false
	}
	var tc struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(cl.Payload, &tc) == nil && tc.Model != "" {
		c.model = tc.Model
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
