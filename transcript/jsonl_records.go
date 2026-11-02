package transcript

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// JSONLAdapter turns a Claude Code, Cursor or Droid JSONL transcript into
// transcript records. It is incremental: Feed accepts the transcript in
// chunks split anywhere, and the records depend only on the bytes fed, not
// on how they were chunked. Convert uses the same constructors, so folding
// the records reproduces Convert's output for that agent.
//
// The constructor fixes the behavior. NewClaudeAdapter also emits per-call
// usage snapshots (model, fresh input, cache and output tokens) by the rules
// below; NewCursorAdapter and NewDroidAdapter keep the legacy per-message
// input and output token counts. NewDroidAdapter unwraps Droid's envelope
// before parsing each line.
//
// The compaction rules live here, as decisions about which message a record
// targets:
//   - Consecutive assistant entries with the same message ID are fragments of
//     one message: the first opens it, the rest append to it.
//   - A tool result in a user entry becomes a result record on the assistant
//     message holding the tool_use with its tool_use_id, wherever the result
//     falls. Claude Code writes the results of parallel tool calls one user
//     entry each, sometimes after the next assistant message began or after
//     text it injected (isMeta). A result without a tool_use_id targets the
//     last assistant message, until a user entry with text or images (whose
//     own results still count). Other results are dropped, as is a user entry
//     that held only such results.
//   - The text and images of a user entry become a separate user message.
//   - For Claude Code, the fragment with the strictly highest output
//     tokens supplies the message's usage snapshot (an earlier one wins ties)
//     and the first fragment naming a model supplies its model. User entries
//     holding only tool results between fragments of one message ID do not
//     split the API call: later fragments become continuation messages that
//     carry no usage, and any improved snapshot goes to the call's first
//     visible message in a usage record. A message whose content is empty has
//     no line, so the next fragment takes over the call's usage instead of
//     continuing it.
//
// Fold keys are the agent's message ID where it has one and it is unused,
// otherwise derived from the source line number (1-based, counting every line
// fed, including blank and dropped ones). A message ID the agent reuses for a
// later, non-adjacent message gets "<id>@L<line>" as its key and keeps the
// agent's ID as SourceID; messages without an ID, including all user
// messages, get "@L<line>" and no SourceID. Keys are stable only for a
// transcript fed from its first line, so a stream that resumes must replay
// from line 1 rather than start a fresh adapter mid-file.
//
// Folding every record from Feed and a final Flush reproduces Convert's
// output for the same bytes, with Options.StartLine 0 and the Options.Agent
// that selects this constructor ("claude-code" for NewClaudeAdapter). Records carry no
// redaction: redact the transcript before feeding it, as checkpoints do.
type JSONLAdapter struct {
	preprocess   linePreprocessor
	perCallUsage bool // emit per-call usage snapshots (Claude Code)

	pending []byte          // bytes after the last newline fed
	line    int             // source lines consumed so far
	keys    map[string]bool // fold keys handed out
	open    *openAssistant  // set while the last parsed entry is an assistant
	// results is the key of the last assistant message, which a result
	// without a tool_use_id attaches to, or "" after a user message.
	results string
	// toolUses maps each tool_use ID seen to the key of the last message
	// holding it.
	toolUses map[string]string
	prev     *openAssistant // last assistant, kept across tool-result-only user entries
}

// openAssistant is the assistant message that further fragments, or the next
// user entry's tool results, still attach to.
type openAssistant struct {
	key     string
	entry   parsedEntry // the message as last restated
	isArray bool        // whether the merged content is a JSON array
	visible bool        // whether the merged content gives the message a line

	// owner holds the usage state of the API call this fragment belongs to:
	// the message itself, or the earlier message of an interrupted call.
	owner *openAssistant
	usage *CallUsage // best usage row so far, without model
	model string
	sent  *CallUsage // snapshot last emitted
}

// NewClaudeAdapter returns an adapter for a Claude Code JSONL transcript.
func NewClaudeAdapter() *JSONLAdapter {
	a := newJSONLAdapter(nil)
	a.perCallUsage = true
	return a
}

// NewCursorAdapter returns an adapter for a Cursor JSONL transcript, which
// shares Claude Code's line format.
func NewCursorAdapter() *JSONLAdapter {
	return newJSONLAdapter(nil)
}

// NewDroidAdapter returns an adapter for a Factory AI Droid JSONL transcript,
// unwrapping each line's envelope first.
func NewDroidAdapter() *JSONLAdapter {
	return newJSONLAdapter(unwrapDroidEnvelope)
}

func newJSONLAdapter(preprocess linePreprocessor) *JSONLAdapter {
	return &JSONLAdapter{preprocess: preprocess, keys: make(map[string]bool)}
}

// Lines returns the number of source lines consumed so far: every
// newline-terminated line fed, including blank and dropped ones, plus a
// partial line once Flush has consumed it. A held partial line is not
// counted. Line-derived keys ("@L<n>") use the same numbering.
func (a *JSONLAdapter) Lines() int {
	return a.line
}

// Feed consumes the next chunk of the transcript and returns the records
// implied by every line it completed. A trailing partial line is held until a
// later chunk completes it, or until Flush.
func (a *JSONLAdapter) Feed(chunk []byte) []Record {
	var out []Record
	for len(chunk) > 0 {
		i := bytes.IndexByte(chunk, '\n')
		if i < 0 {
			a.pending = append(a.pending, chunk...)
			break
		}
		lineBytes := chunk[:i+1]
		if len(a.pending) > 0 {
			a.pending = append(a.pending, lineBytes...)
			lineBytes = a.pending
		}
		out = a.consumeLine(lineBytes, out)
		a.pending = a.pending[:0]
		chunk = chunk[i+1:]
	}
	return out
}

// Flush treats a held partial line as complete. Call it only once the
// transcript is final; while an agent is still writing, a partial line is
// usually an unfinished write.
func (a *JSONLAdapter) Flush() []Record {
	if len(a.pending) == 0 {
		return nil
	}
	lineBytes := a.pending
	a.pending = nil
	return a.consumeLine(lineBytes, nil)
}

func (a *JSONLAdapter) consumeLine(lineBytes []byte, out []Record) []Record {
	a.line++
	if len(bytes.TrimSpace(lineBytes)) == 0 {
		return out
	}
	e, ok := parseLine(lineBytes, a.preprocess)
	if !ok {
		return out
	}
	switch e.kind {
	case TypeAssistant:
		out = a.assistant(e, out)
		a.results = a.open.key
		for _, id := range e.toolUseIDs {
			if a.toolUses == nil {
				a.toolUses = make(map[string]string)
			}
			a.toolUses[id] = a.results
		}
		return out
	case TypeUser:
		return a.user(e, out)
	}
	return out
}

func (a *JSONLAdapter) assistant(e parsedEntry, out []Record) []Record {
	o := a.open
	if o == nil || o.entry.id != e.id {
		o = &openAssistant{key: a.newKey(e.id), entry: e, isArray: isJSONArray(e.content), visible: !isEmptyContentArray(e.content)}
		o.owner = o
		if a.perCallUsage && e.id != "" && a.prev != nil && a.prev.entry.id == e.id {
			if call := a.prev.owner; call.visible {
				o.owner = call
			} else {
				o.usage, o.model = call.usage, call.model
			}
		}
		a.open, a.prev = o, o
		usage, out := o.usageUpdate(e, a.perCallUsage, out)
		return append(out, o.message(usage))
	}

	o.entry.ts = e.ts
	if e.inputTokens > 0 {
		o.entry.inputTokens = e.inputTokens
	}
	if e.outputTokens > 0 {
		o.entry.outputTokens = e.outputTokens
	}
	usage, out := o.usageUpdate(e, a.perCallUsage, out)
	add := isJSONArray(e.content)
	if !o.isArray {
		// Compaction's merge keeps only array blocks, so merging into
		// non-array content leaves just this fragment's blocks, or null.
		// Either way the restatement is no larger than the fragment.
		o.entry.content = jsonNull
		if add {
			o.entry.content = e.content
		}
		o.isArray = add
		o.visible = !isEmptyContentArray(o.entry.content)
		return append(out, o.message(usage))
	}
	content := e.content
	o.visible = o.visible || (add && !isEmptyContentArray(content))
	if !add {
		// A non-array fragment adds no blocks to array content but still
		// carries the timestamp and usage. Restating the whole message
		// here would make the records grow with the square of its length.
		content = nil
	}
	rec := Record{Op: OpAppend, ID: o.key, TS: e.ts, Content: content, Usage: usage}
	if !o.continuation() {
		rec.InputTokens, rec.OutputTokens = e.inputTokens, e.outputTokens
	}
	return append(out, rec)
}

func (a *JSONLAdapter) user(e parsedEntry, out []Record) []Record {
	a.open = nil

	hasContent := e.userText != "" || len(e.userImages) > 0
	if !hasToolResults(e) || hasContent {
		a.prev = nil
	}
	for _, tr := range e.toolResults {
		target := a.results
		if tr.toolUseID != "" {
			target = a.toolUses[tr.toolUseID]
		}
		if target == "" {
			continue
		}
		out = append(out, Record{
			Op:        OpResult,
			ID:        target,
			ToolUseID: tr.toolUseID,
			Result:    toolResult(tr),
		})
	}
	if hasToolResults(e) && !hasContent {
		return out
	}
	a.results = ""

	content, err := userContentBlocks(e)
	if err != nil {
		return out
	}
	return append(out, Record{
		Op:      OpMessage,
		ID:      a.newKey(""),
		Role:    TypeUser,
		TS:      e.ts,
		Content: content,
	})
}

// newKey returns an unused fold key for a message starting on the current
// line, preferring the agent's own ID.
func (a *JSONLAdapter) newKey(sourceID string) string {
	key := sourceID
	if key == "" || a.keys[key] {
		key = fmt.Sprintf("%s@L%d", sourceID, a.line)
	}
	// Only an agent ID shaped like a derived key can collide here.
	for a.keys[key] {
		key += "'"
	}
	a.keys[key] = true
	return key
}

// jsonNull is the content compaction's merge yields when neither side is an
// array.
var jsonNull = json.RawMessage("null")

func (o *openAssistant) continuation() bool { return o.owner != o }

func (o *openAssistant) message(usage *CallUsage) Record {
	r := Record{
		Op:       OpMessage,
		ID:       o.key,
		SourceID: o.entry.id,
		Role:     TypeAssistant,
		TS:       o.entry.ts,
		Content:  o.entry.content,
		Usage:    usage,
	}
	if !o.continuation() {
		r.InputTokens, r.OutputTokens = o.entry.inputTokens, o.entry.outputTokens
	}
	return r
}

// usageUpdate takes e's usage into the call's state, then returns the snapshot
// to put on o's own record. A continuation message carries none; its call's
// improved snapshot goes to the owner in a usage record.
func (o *openAssistant) usageUpdate(e parsedEntry, enabled bool, out []Record) (*CallUsage, []Record) {
	owner := o.owner
	if enabled {
		if owner.model == "" && e.model != syntheticModel {
			owner.model = e.model
		}
		if e.usage != nil && (owner.usage == nil || e.usage.OutputTokens > owner.usage.OutputTokens) {
			owner.usage = e.usage
		}
	}
	u := owner.changedUsage()
	if !o.continuation() {
		return u, out
	}
	if u != nil {
		out = append(out, Record{Op: OpUsage, ID: owner.key, Usage: u})
	}
	return nil, out
}

// changedUsage returns the message's usage snapshot if it differs from the
// last one returned, and nil otherwise.
func (o *openAssistant) changedUsage() *CallUsage {
	if o.usage == nil {
		return nil
	}
	snapshot := cloneCallUsage(o.usage)
	snapshot.Model = o.model
	if sameUsage(snapshot, o.sent) {
		return nil
	}
	o.sent = snapshot
	// The record gets its own copy: a caller changing it must not change what
	// the adapter believes it last sent.
	return cloneCallUsage(snapshot)
}

func sameUsage(a, b *CallUsage) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Model == b.Model &&
		a.InputTokens == b.InputTokens &&
		a.OutputTokens == b.OutputTokens &&
		a.CacheReadTokens == b.CacheReadTokens &&
		a.CacheWriteTokens == b.CacheWriteTokens &&
		a.ReasoningTokens == b.ReasoningTokens &&
		sameCount(a.CacheWrite5mTokens, b.CacheWrite5mTokens) &&
		sameCount(a.CacheWrite1hTokens, b.CacheWrite1hTokens)
}

func sameCount(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// isJSONArray reports whether raw is a JSON array (not null).
func isJSONArray(raw json.RawMessage) bool {
	var arr []json.RawMessage
	return json.Unmarshal(raw, &arr) == nil && arr != nil
}
