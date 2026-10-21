package transcript

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// message is one folded message. Its content is either raw, exactly as the
// last message record gave it, or a decoded block list once an append or a
// tool result has touched it. Blocks are re-encoded only when a line is
// produced.
type message struct {
	id       string
	sourceID string
	role     string
	ts       json.RawMessage
	raw      json.RawMessage // content while decoded is false
	// blocks is the content once decoded. Before that, once split is set,
	// it holds raw split into blocks, so that tool results that match
	// nothing do not split raw again.
	blocks       []block
	split        bool           // whether blocks and the fields below describe the content
	decoded      bool           // whether blocks is the content and raw is unused
	nonObjects   int            // blocks that are neither an object nor null
	lastToolUse  int            // index of the last tool_use block, or -1
	toolUses     map[string]int // index of the last tool_use block with each id
	rekeyed      int            // blocks[:rekeyed] are written with sorted keys
	inputTokens  int
	outputTokens int
	dropped      bool
}

// block is one content block of a decoded message. Every block present when
// a tool result was attached is written with its keys sorted, as encoding a
// decoded object does, which is how compaction has always written such
// content; message.rekeyed counts them.
type block struct {
	raw    json.RawMessage
	typ    string // the "type" string, if any
	id     string // the "id" string, if any
	result *ToolResult
}

// Folder applies records incrementally. Messages keep the order in which
// their IDs were first created. The zero value is not usable; call NewFolder.
//
// A Folder keeps every message in memory and has no limits of its own.
// Applying a record costs time in the record's size, except that the first
// append or result after a message record splits that record's content into
// blocks, once. Until an append or a matching result decodes the message, the
// split blocks are kept beside the raw content. Producing a line costs time
// in that message's size, and Lines in the size of all messages. Callers
// folding records from an untrusted source must bound the number and size of
// records and messages.
type Folder struct {
	opts Options
	msgs []*message
	byID map[string]*message
}

// NewFolder returns an empty Folder whose lines carry opts.Agent and
// opts.CLIVersion; opts.StartLine is ignored.
func NewFolder(opts Options) *Folder {
	return &Folder{opts: opts, byID: make(map[string]*message)}
}

// Fold applies records to an empty Folder and returns its lines.
func Fold(records []Record, opts Options) ([]byte, error) {
	f := NewFolder(opts)
	if _, err := f.Apply(records); err != nil {
		return nil, err
	}
	return f.Lines(), nil
}

// Apply applies records in order and returns the IDs of the messages they
// touched, each once, in order of first touch. Apply validates every record
// before changing anything, so on error the Folder is unchanged.
func (f *Folder) Apply(records []Record) ([]string, error) {
	if err := f.validate(records); err != nil {
		return nil, err
	}

	var touched []string
	seen := make(map[string]bool)
	for _, r := range records {
		f.apply(r)
		if !seen[r.ID] {
			seen[r.ID] = true
			touched = append(touched, r.ID)
		}
	}
	return touched, nil
}

// Lines returns the compact transcript: one newline-terminated line per
// visible message, in creation order.
func (f *Folder) Lines() []byte {
	var out []byte
	for _, m := range f.msgs {
		if line, ok := f.line(m); ok {
			out = append(out, line...)
			out = append(out, '\n')
		}
	}
	return out
}

// Line returns the compact line (without a trailing newline) for message id.
// visible is false when the message is unknown, dropped, or an assistant
// message whose content is [] or null; such a message has no line in Lines.
// An assistant message with no content at all is visible and prints
// "content":null, as Convert always has.
func (f *Folder) Line(id string) (line []byte, visible bool) {
	m, ok := f.byID[id]
	if !ok {
		return nil, false
	}
	return f.line(m)
}

func (f *Folder) line(m *message) ([]byte, bool) {
	if m.dropped || (m.role == TypeAssistant && m.isEmpty()) {
		return nil, false
	}
	content, err := m.content()
	if err != nil {
		return nil, false
	}
	line := newTranscriptLine(f.opts)
	line.Type = m.role
	line.TS = m.ts
	line.ID = m.sourceID
	line.InputTokens = m.inputTokens
	line.OutputTokens = m.outputTokens
	line.Content = content
	b, err := json.Marshal(line)
	if err != nil {
		return nil, false
	}
	return b, true
}

func (f *Folder) validate(records []Record) error {
	created := make(map[string]bool)
	for i, r := range records {
		if err := validateRecord(r); err != nil {
			return fmt.Errorf("record %d: %w", i, err)
		}
		if r.Op == OpMessage {
			created[r.ID] = true
			continue
		}
		if _, ok := f.byID[r.ID]; !ok && !created[r.ID] {
			return fmt.Errorf("record %d: %s targets unknown message %q", i, r.Op, r.ID)
		}
	}
	return nil
}

func validateRecord(r Record) error {
	if r.ID == "" {
		return errors.New("missing id")
	}
	if len(r.TS) > 0 && !json.Valid(r.TS) {
		return errors.New("ts is not valid JSON")
	}
	if len(r.Content) > 0 && !json.Valid(r.Content) {
		return errors.New("content is not valid JSON")
	}
	switch r.Op {
	case OpMessage:
		if r.Role != TypeUser && r.Role != TypeAssistant {
			return fmt.Errorf("message role %q is not %q or %q", r.Role, TypeUser, TypeAssistant)
		}
	case OpResult:
		if r.Result == nil {
			return errors.New("result record without result")
		}
	case OpAppend, OpUsage, OpDrop:
	default:
		return fmt.Errorf("unknown op %q", r.Op)
	}
	return nil
}

// apply applies a validated record. It copies any record bytes it keeps:
// callers may reuse a Record's buffers, for example by decoding every record
// of a stream into the same variable.
func (f *Folder) apply(r Record) {
	m := f.byID[r.ID]
	switch r.Op {
	case OpMessage:
		if m == nil {
			m = &message{id: r.ID}
			f.byID[r.ID] = m
			f.msgs = append(f.msgs, m)
		}
		m.sourceID = r.SourceID
		m.role = r.Role
		m.ts = cloneRaw(r.TS)
		m.setContent(r.Content)
		m.dropped = false
		m.setTokens(r)
	case OpAppend:
		m.ts = cloneRaw(r.TS)
		m.appendBlocks(r.Content)
		m.setTokens(r)
	case OpResult:
		m.setToolResult(r.ToolUseID, r.Result)
	case OpUsage:
		m.setTokens(r)
	case OpDrop:
		m.dropped = true
	}
}

// cloneRaw copies raw. Empty and nil both mean absent, as they do when a
// Record is encoded, so empty becomes nil.
func cloneRaw(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return json.RawMessage(bytes.Clone(raw))
}

// setTokens applies the record's non-zero token counts. Agents report
// cumulative usage, so the latest non-zero value is the total.
func (m *message) setTokens(r Record) {
	if r.InputTokens > 0 {
		m.inputTokens = r.InputTokens
	}
	if r.OutputTokens > 0 {
		m.outputTokens = r.OutputTokens
	}
}

// setContent replaces the content with raw, undecoded.
func (m *message) setContent(raw json.RawMessage) {
	m.raw = cloneRaw(raw)
	m.blocks, m.split, m.decoded = nil, false, false
	m.nonObjects, m.lastToolUse, m.toolUses, m.rekeyed = 0, -1, nil, 0
}

// splitRaw splits raw into blocks, once. Content that is not an array (or
// is null) has no blocks.
func (m *message) splitRaw() {
	if m.split {
		return
	}
	m.addBlocks(m.raw)
	m.split = true
}

// decode switches m to its block list.
func (m *message) decode() {
	m.splitRaw()
	m.raw, m.decoded = nil, true
}

// addBlocks adds the blocks of content, a content array, and indexes its
// tool_use blocks.
func (m *message) addBlocks(content json.RawMessage) {
	more, nonObjects := decodeBlocks(content)
	for i, b := range more {
		if b.typ != ContentTypeToolUse {
			continue
		}
		m.lastToolUse = len(m.blocks) + i
		if b.id != "" {
			if m.toolUses == nil {
				m.toolUses = make(map[string]int)
			}
			m.toolUses[b.id] = m.lastToolUse
		}
	}
	m.blocks = append(m.blocks, more...)
	m.nonObjects += nonObjects
}

// appendBlocks appends the blocks of add, a content array; a value that is
// not an array contributes no blocks. Content that is not an array has no
// blocks, so appending to it discards it.
func (m *message) appendBlocks(add json.RawMessage) {
	m.decode()
	m.addBlocks(add)
}

// setToolResult sets result on the last tool_use block whose id is
// toolUseID, or on the last tool_use block when toolUseID is empty. Nothing
// changes when the content is not an array, when one of its blocks is not an
// object, or when no block matches.
func (m *message) setToolResult(toolUseID string, result *ToolResult) {
	m.splitRaw()
	if m.nonObjects > 0 {
		return
	}
	i := m.lastToolUse
	if toolUseID != "" {
		var ok bool
		if i, ok = m.toolUses[toolUseID]; !ok {
			return
		}
	}
	if i < 0 {
		return
	}
	m.decode()
	m.rekeyed = len(m.blocks)
	m.blocks[i].result = cloneToolResult(result)
}

// isEmpty reports whether the content is an empty array or null.
func (m *message) isEmpty() bool {
	if m.decoded {
		return len(m.blocks) == 0
	}
	return isEmptyContentArray(m.raw)
}

// content encodes the message's content for its line.
func (m *message) content() (json.RawMessage, error) {
	if !m.decoded {
		return m.raw, nil
	}
	out := make([]json.RawMessage, len(m.blocks))
	for i, b := range m.blocks {
		if i >= m.rekeyed {
			out[i] = b.raw
			continue
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(b.raw, &obj); err != nil {
			return nil, fmt.Errorf("decoding content block %d: %w", i, err)
		}
		if b.result != nil {
			r, err := json.Marshal(b.result)
			if err != nil {
				return nil, fmt.Errorf("encoding tool result: %w", err)
			}
			obj["result"] = r
		}
		enc, err := json.Marshal(obj)
		if err != nil {
			return nil, fmt.Errorf("encoding content block %d: %w", i, err)
		}
		out[i] = enc
	}
	data, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("encoding content: %w", err)
	}
	return data, nil
}

// decodeBlocks splits a content array into blocks, reading each block's type
// and id. It returns no blocks for anything but an array, and counts the
// blocks that are neither an object nor null.
func decodeBlocks(content json.RawMessage) ([]block, int) {
	var raws []json.RawMessage
	if json.Unmarshal(content, &raws) != nil {
		return nil, 0
	}
	blocks := make([]block, len(raws))
	nonObjects := 0
	for i, raw := range raws {
		blocks[i].raw = raw
		var obj map[string]json.RawMessage
		if json.Unmarshal(raw, &obj) != nil {
			nonObjects++
			continue
		}
		blocks[i].typ = unquote(obj["type"])
		blocks[i].id = unquote(obj["id"])
	}
	return blocks, nonObjects
}

func cloneToolResult(r *ToolResult) *ToolResult {
	c := *r
	if r.File != nil {
		file := *r.File
		c.File = &file
	}
	return &c
}
