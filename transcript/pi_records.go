package transcript

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/entireio/cli/internal/transcript/pijsonl"
)

// PiAdapter turns a Pi session transcript into transcript records as it is
// written. Feed accepts the transcript in chunks split anywhere, and the
// records depend only on the bytes fed, not on how they were chunked.
// Convert uses the same adapter, so after any complete line, folding every
// record so far reproduces Convert of the bytes so far (with
// Options.StartLine 0), wherever Convert succeeds: it still fails on a line
// of 10 MiB or more, which the adapter reads whole.
//
// Pi sessions are trees: every entry has an id and a parentId, and /tree and
// branching continue from an earlier entry. The conversation is the active
// branch, the path from the last message entry to the root
// (pijsonl.ResolveActiveBranch). While no entry has a non-empty parentId
// there is no tree, and every message is active.
//
// Each message is keyed by its entry ID, which is also its SourceID, and
// sent whole (OpMessage) with the results of its tool calls spliced in: per
// tool call ID, the last toolResult entry on the active branch. It is sent
// again when a result it shows changes (one arrives, or a branch switch
// leaves it behind), and dropped (OpDrop) when it leaves the active branch.
// An OpMessage on a dropped key restores it where it was first created, so
// returning to an earlier branch needs no new keys. An assistant message
// with no visible content but non-zero usage is sent with no content, which
// the fold keeps and prints as "content":null, so its usage has a line.
//
// A line that only grows the active branch (the usual case: a message whose
// parent is the tip, perhaps through entries that are not messages) is read
// without going over the whole tree, so reading a long session that only
// grew takes linear time. Any other line reconciles the whole tree, as does
// every line while there is no tree yet, which Pi (session version 3) has
// only for its first line or two.
//
// Lines have no size limit: a long line is held whole until its newline
// arrives, as JSONLAdapter holds one.
//
// Entry IDs are unique in transcripts Pi writes. A repeated ID is
// re-read as the entry it names, which keeps its first position.
// Records carry no redaction: redact the transcript before feeding it.
type PiAdapter struct {
	pending []byte // bytes after the last newline fed; see piPendingKeep
	line    int    // source lines consumed so far

	parent   map[string]string // every entry's parentId
	hasTree  bool              // some entry has a non-empty parentId
	tip      string            // the last message entry
	messages []string          // decoded message entries, in file order
	entries  map[string]*piMessage
	// sent is, for each message sent and not dropped since, the results it
	// was sent with (see piResultsKey). After any line, every active message
	// compaction emits a line for is in it, with the results it shows now:
	// grow relies on that to send only what the line changes.
	sent map[string]string

	// What the last reconcile worked out, kept up to date while the branch
	// only grows: the active branch (nil: no tree, so nothing grows it),
	// each tool call ID's supplier (see reconcile), and the messages with
	// each tool call ID, in file order.
	active   map[string]bool
	supplier map[string]string
	owners   map[string][]string
	// above holds some of the entries that are not messages and descend
	// from the tip through such entries only: those grow read since the tip
	// last moved. A message under one of them grows the branch too.
	// reconcile clears it without rebuilding it, which only costs a full
	// reconcile for a message under an entry it forgot.
	above map[string]bool
}

// piPendingKeep is the most capacity Feed keeps in pending once a line it
// held is read: a longer line's buffer is let go rather than kept for the
// adapter's lifetime.
const piPendingKeep = 1 << 20

// piMessage is one decoded message entry, with what reconcile needs of it
// worked out once.
type piMessage struct {
	entry  pijsonl.Entry
	line   int                  // the source line it was first read from
	calls  []string             // its tool call IDs, in order
	empty  bool                 // it has no line, whatever the results
	result *piCompactToolResult // a toolResult entry's result (its content is not kept)
}

// piNode is how pijsonl.ResolveActiveBranch reads an entry.
type piNode struct {
	Type     string  `json:"type"`
	ID       string  `json:"id"`
	ParentID *string `json:"parentId"`
}

// NewPiAdapter returns an adapter for one Pi transcript, to be fed from its
// first line.
func NewPiAdapter() *PiAdapter {
	return &PiAdapter{
		parent:   map[string]string{},
		entries:  map[string]*piMessage{},
		sent:     map[string]string{},
		supplier: map[string]string{},
		owners:   map[string][]string{},
		above:    map[string]bool{},
	}
}

// Lines returns the number of source lines consumed so far: every
// newline-terminated line fed, including blank and invalid ones,
// plus a partial line once Flush has consumed it. A held partial line is not
// counted.
func (a *PiAdapter) Lines() int {
	return a.line
}

// Feed consumes the next chunk of the transcript and returns the records
// implied by every line it completed. A trailing partial line is held until a
// later chunk completes it, or until Flush.
func (a *PiAdapter) Feed(chunk []byte) []Record {
	var out []Record
	for len(chunk) > 0 {
		i := bytes.IndexByte(chunk, '\n')
		if i < 0 {
			a.pending = append(a.pending, chunk...)
			break
		}
		line := chunk[:i]
		if len(a.pending) > 0 {
			a.pending = append(a.pending, line...)
			line = a.pending
		}
		out = a.consumeLine(line, out)
		if cap(a.pending) > piPendingKeep {
			a.pending = nil
		} else {
			a.pending = a.pending[:0]
		}
		chunk = chunk[i+1:]
	}
	return out
}

// Flush treats a held partial line as complete. Call it only once the
// transcript is final; while Pi is still writing, a partial line is usually
// an unfinished write.
func (a *PiAdapter) Flush() []Record {
	if len(a.pending) == 0 {
		return nil
	}
	line := a.pending
	a.pending = nil
	return a.consumeLine(line, nil)
}

// consumeLine reads one complete line. It keeps no reference to line.
func (a *PiAdapter) consumeLine(line []byte, out []Record) []Record {
	a.line++
	line = bytes.TrimSuffix(line, []byte("\r"))
	var n piNode
	if json.Unmarshal(line, &n) != nil || n.ID == "" {
		return out
	}
	if recs, ok := a.grow(line, n); ok {
		return append(out, recs...)
	}
	if n.ParentID != nil {
		a.parent[n.ID] = *n.ParentID
		if *n.ParentID != "" {
			a.hasTree = true
		}
	}
	if n.Type == pijsonl.EntryTypeMessage {
		a.tip = n.ID
		if e, ok := piDecodeEntry(line); ok {
			m := piDecodeMessage(e)
			m.line = a.line
			if prev, seen := a.entries[n.ID]; seen {
				m.line = prev.line
			} else {
				a.messages = append(a.messages, n.ID)
			}
			a.entries[n.ID] = m
		}
	}
	return append(out, a.reconcile()...)
}

// piDecodeEntry decodes a message entry; false when the line is not a valid
// entry.
func piDecodeEntry(line []byte) (pijsonl.Entry, bool) {
	var e pijsonl.Entry
	if json.Unmarshal(line, &e) != nil {
		return pijsonl.Entry{}, false
	}
	return e, true
}

// grow reads a line that only grows the active branch, if n's is one. A
// message n under the tip (directly, or through entries in above) becomes
// the tip: the active branch is as it was, plus n and the entries in above
// it descends through. Any other entry leaves the tip and the active branch
// as they were (it may join above). That needs a tree and a tip (so the
// active branch is the walk from the tip), and n's ID in neither entries nor
// parent (so no parentId changes; an ID seen only on an entry without a
// parentId passes, but that is a repeated ID), nor reached by the walk (an
// entry whose parent was unknown ends the walk; it is in the active branch).
// Then only n can need sending, or, for a toolResult, the active messages
// with its tool call ID: it supplies their result now, as the last on the
// branch. False leaves n unread, for consumeLine to reconcile; so does a
// message whose line is not a valid entry, which moves the tip but has no
// entry.
func (a *PiAdapter) grow(line []byte, n piNode) ([]Record, bool) {
	if a.active == nil || n.ParentID == nil || a.active[n.ID] || a.entries[n.ID] != nil {
		return nil, false
	}
	if _, seen := a.parent[n.ID]; seen {
		return nil, false
	}
	p := *n.ParentID
	under := p != "" && (p == a.tip || a.above[p])
	if n.Type != pijsonl.EntryTypeMessage {
		a.parent[n.ID] = p
		if under {
			a.above[n.ID] = true
		}
		return nil, true // the tip and the branch stay
	}
	if !under {
		return nil, false
	}
	e, ok := piDecodeEntry(line)
	if !ok {
		return nil, false
	}
	a.parent[n.ID], a.tip = p, n.ID
	for cur := p; a.above[cur]; cur = a.parent[cur] {
		a.active[cur] = true
	}
	clear(a.above)
	a.active[n.ID] = true
	m := a.add(n.ID, e)
	if m.result == nil {
		if r, ok := a.send(n.ID, m); ok {
			return []Record{r}, true
		}
		return nil, true
	}
	call := m.entry.Message.ToolCallID
	a.supplier[call] = n.ID
	var out []Record
	for _, id := range a.owners[call] {
		if a.active[id] {
			if r, ok := a.send(id, a.entries[id]); ok {
				out = append(out, r)
			}
		}
	}
	return out, true
}

// add decodes a message entry not seen before.
func (a *PiAdapter) add(id string, e pijsonl.Entry) *piMessage {
	m := piDecodeMessage(e)
	m.line = a.line
	a.messages = append(a.messages, id)
	a.entries[id] = m
	a.own(id, m)
	return m
}

// own lists m under each of its tool call IDs.
func (a *PiAdapter) own(id string, m *piMessage) {
	for _, call := range m.calls {
		if ids := a.owners[call]; len(ids) == 0 || ids[len(ids)-1] != id {
			a.owners[call] = append(ids, id)
		}
	}
}

// piDecodeMessage works out once what reconcile needs of a message entry.
func piDecodeMessage(e pijsonl.Entry) *piMessage {
	m := &piMessage{entry: e}
	switch e.Message.Role {
	case pijsonl.RoleToolResult:
		if e.Message.ToolCallID != "" {
			m.result = &piCompactToolResult{
				Output: piDecodeResultOutput(e.Message.Content),
				Status: piResultStatus(e.Message.IsError),
			}
		}
		m.entry.Message.Content = nil // only the call ID and result are needed
		m.empty = true
	case pijsonl.RoleAssistant:
		var visible bool
		m.calls, visible = piAssistantCalls(e.Message.Content)
		m.empty = !visible && !piUsageHasCounts(piCallUsage(e.Message))
	default:
		_, ok := piRecord(e, nil)
		m.empty = !ok
	}
	return m
}

// piAssistantCalls reads assistant content once for its tool call IDs, and
// whether piEmitAssistantContent gives it a block, without decoding the
// calls' arguments.
func piAssistantCalls(raw json.RawMessage) (calls []string, visible bool) {
	if pijsonl.DecodeStringContent(raw) != "" {
		return nil, true
	}
	var items []pijsonl.ContentItem
	if json.Unmarshal(raw, &items) != nil {
		return nil, false
	}
	for _, it := range items {
		switch it.Type {
		case pijsonl.ContentTypeText:
			visible = visible || it.Text != ""
		case piContentTypeToolCall:
			calls = append(calls, it.ID)
			visible = true
		}
	}
	return calls, visible
}

// reconcile sends what changed on the active branch: drops for messages
// that left it, and every message on it not sent, or sent with other
// results than it shows now. It works out again what grow keeps up to date.
func (a *PiAdapter) reconcile() []Record {
	a.active = a.activeBranch()
	on := func(id string) bool { return a.active == nil || a.active[id] }
	clear(a.supplier)
	clear(a.owners)
	clear(a.above)
	for _, id := range a.messages {
		m := a.entries[id]
		if m.result != nil && on(id) {
			a.supplier[m.entry.Message.ToolCallID] = id
		}
		a.own(id, m)
	}
	var out []Record
	for _, id := range a.messages {
		m := a.entries[id]
		// An empty message was sent only if a repeated ID replaced it.
		if m.empty || !on(id) {
			if _, wasSent := a.sent[id]; wasSent {
				out = append(out, Record{Op: OpDrop, ID: id})
				delete(a.sent, id)
			}
			continue
		}
		if r, ok := a.send(id, m); ok {
			out = append(out, r)
		}
	}
	return out
}

// send is the OpMessage for active message m, unless it was sent with the
// results it shows now (or it has no line).
func (a *PiAdapter) send(id string, m *piMessage) (Record, bool) {
	key := piResultsKey(m.calls, a.supplier)
	if last, wasSent := a.sent[id]; wasSent && last == key {
		return Record{}, false
	}
	results := map[string]piCompactToolResult{}
	for _, call := range m.calls {
		if s, ok := a.supplier[call]; ok {
			results[call] = *a.entries[s].result
		}
	}
	r, ok := piRecord(m.entry, results)
	if !ok {
		return Record{}, false
	}
	a.sent[id] = key
	return r, true
}

// piResultsKey names the results a message with these tool calls shows: the
// toolResult entry supplying each call's, if any.
func piResultsKey(calls []string, supplier map[string]string) string {
	if len(calls) == 0 {
		return ""
	}
	parts := make([]string, len(calls))
	for i, call := range calls {
		parts[i] = supplier[call]
	}
	return strings.Join(parts, "\x00")
}

// activeBranch is pijsonl.ResolveActiveBranch's set: the entries from the
// tip to the root, or nil (every message is active) while there is no tree.
func (a *PiAdapter) activeBranch() map[string]bool {
	if !a.hasTree || a.tip == "" {
		return nil
	}
	active := map[string]bool{}
	for cur := a.tip; cur != ""; {
		if active[cur] {
			break // a cycle
		}
		active[cur] = true
		p, ok := a.parent[cur]
		if !ok {
			break
		}
		cur = p
	}
	return active
}

// piRecord is entry's OpMessage, with results spliced into its tool calls;
// false when it has no line (another role, or no content and no usage). An
// assistant call with usage but no visible content gets no content: the fold
// hides content [] and null but keeps a message with none.
func piRecord(e pijsonl.Entry, results map[string]piCompactToolResult) (Record, bool) {
	r := Record{Op: OpMessage, ID: e.ID, Role: e.Message.Role, TS: piTimestampJSON(e.Timestamp)}
	var blocks []any
	switch e.Message.Role {
	case pijsonl.RoleUser:
		blocks = piEmitUserContent(e.Message.Content)
	case pijsonl.RoleAssistant:
		blocks = piEmitAssistantContent(e.Message.Content, results)
		r.SourceID = e.ID // Pi lines carry the ID on assistant lines only
		r.Usage = piCallUsage(e.Message)
		if len(blocks) == 0 && piUsageHasCounts(r.Usage) {
			return r, true
		}
	default:
		return Record{}, false
	}
	if len(blocks) == 0 {
		return Record{}, false
	}
	content, err := json.Marshal(blocks)
	if err != nil {
		return Record{}, false
	}
	r.Content = content
	return r, true
}
