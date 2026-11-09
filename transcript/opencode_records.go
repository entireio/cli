package transcript

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
)

// OpenCodeAdapter turns snapshots of an OpenCode session's messages into
// transcript records as the session changes. OpenCode keeps a session in its
// database and rewrites a message's rows in place while the message grows,
// so there is no append-only transcript to follow: a live producer exports
// the session again whenever it changes and feeds every message's latest
// snapshot.
//
// A snapshot line is one message as OpenCodeSnapshotLines writes it: the
// message of a normalized export (NormalizeOpenCodeExport, so OpenCode 1
// shape for either version), {"info":{...},"parts":[...]}, on one line.
// Each message is keyed by its info.id, which is also its SourceID on
// assistant lines, and sent whole (OpMessage). Feeding a message again sends
// it again only when its record changed (text grew, a tool's state moved on,
// its tokens were counted), and the new record replaces the old one in the
// fold; an unchanged snapshot sends nothing. Drop hides a message the
// session no longer has (a revert, or a compaction OpenCode applied), and
// feeding it again restores it where it was first created.
//
// Folding every record from a sequence of snapshots reproduces Convert of
// the export made of each message's latest snapshot (with Options.StartLine
// 0), dropped messages left out, provided messages are first fed in the
// session's order: a message keeps the position at which it was first fed,
// so a message first fed after a later one stays after it. Feeding every
// export whole (FeedExport) satisfies that. An assistant message with no
// text or tool parts is still sent, with content [], which the fold does not
// show; this keeps its position for when its content arrives.
//
// Lines that are not a JSON object with a non-empty info.id, and messages
// whose role is neither user nor assistant, are consumed and ignored. Lines
// have no size limit: a long line is held whole until its newline arrives.
// Records carry no redaction: redact the snapshots before feeding them.
type OpenCodeAdapter struct {
	lines lineSplitter
	line  int // snapshot lines consumed so far
	// sent is the digest of the record last sent for each message, for
	// those sent and not dropped since. order lists every key ever sent, in
	// first-sent order, so FeedExport drops in a stable order.
	sent  map[string][sha256.Size]byte
	order []string
	known map[string]bool // keys in order
}

// NewOpenCodeAdapter returns an adapter for one OpenCode session's
// snapshots.
func NewOpenCodeAdapter() *OpenCodeAdapter {
	return &OpenCodeAdapter{sent: map[string][sha256.Size]byte{}, known: map[string]bool{}}
}

// Lines returns the number of snapshot lines consumed so far: every
// newline-terminated line fed, including blank and ignored ones, plus a
// partial line once Flush has consumed it, plus every message FeedExport
// read. A held partial line is not counted.
func (a *OpenCodeAdapter) Lines() int {
	return a.line
}

// Feed consumes the next chunk of snapshot lines and returns the records
// for every line it completed. A trailing partial line is held until a later
// chunk completes it, or until Flush.
func (a *OpenCodeAdapter) Feed(chunk []byte) []Record {
	var out []Record
	a.lines.feed(chunk, func(line []byte) bool {
		out = a.consume(line, out)
		return true
	})
	return out
}

// Flush treats a held partial line as complete.
func (a *OpenCodeAdapter) Flush() []Record {
	line := a.lines.flush()
	if line == nil {
		return nil
	}
	return a.consume(line, nil)
}

// Drop hides message id: it returns an OpDrop when the message was sent and
// not dropped since, and nothing otherwise. The next snapshot of the message
// sends it again, restored at its first position.
func (a *OpenCodeAdapter) Drop(id string) []Record {
	if _, ok := a.sent[id]; !ok {
		return nil
	}
	delete(a.sent, id)
	return []Record{{Op: OpDrop, ID: id}}
}

// FeedExport reads a whole export of the session, either version: it feeds
// every message's snapshot in order, then drops every message sent before
// that the export no longer has, in the order they were first sent. Do not
// call it while a partial line from Feed is held. An export that cannot be
// read returns an error and changes nothing.
func (a *OpenCodeAdapter) FeedExport(export []byte) ([]Record, error) {
	lines, err := OpenCodeSnapshotLines(export)
	if err != nil {
		return nil, err
	}
	present := make(map[string]bool, len(lines))
	var out []Record
	for _, line := range lines {
		if msg, ok := decodeOpenCodeSnapshot(line); ok {
			present[msg.Info.ID] = true
		}
		out = a.consume(line, out)
	}
	for _, id := range a.order {
		if !present[id] {
			out = append(out, a.Drop(id)...)
		}
	}
	return out, nil
}

// consume reads one snapshot line. It keeps no reference to line.
func (a *OpenCodeAdapter) consume(line []byte, out []Record) []Record {
	a.line++
	msg, ok := decodeOpenCodeSnapshot(line)
	if !ok {
		return out
	}
	id := msg.Info.ID
	r, ok := openCodeRecord(id, msg)
	if !ok {
		return out
	}
	enc, err := json.Marshal(r)
	if err != nil {
		return out
	}
	digest := sha256.Sum256(enc)
	if last, wasSent := a.sent[id]; wasSent && last == digest {
		return out
	}
	a.sent[id] = digest
	if !a.known[id] {
		a.known[id] = true
		a.order = append(a.order, id)
	}
	return append(out, r)
}

// decodeOpenCodeSnapshot reads a snapshot line; false when it is not a
// message with an ID.
func decodeOpenCodeSnapshot(line []byte) (openCodeMessage, bool) {
	var msg openCodeMessage
	if json.Unmarshal(line, &msg) != nil || msg.Info.ID == "" {
		return openCodeMessage{}, false
	}
	return msg, true
}

// OpenCodeSnapshotLines splits an OpenCode export, either version, into the
// snapshot lines OpenCodeAdapter reads: the messages of the normalized
// export (NormalizeOpenCodeExport), in order, each compacted onto one line
// with no trailing newline. Feed them each followed by "\n".
//
// A pending revert (the session's info.revert, set by OpenCode's undo) hides
// the messages from its message on, as OpenCode shows the session: the
// reverted message and every later one, or, when the revert names a part of
// that message, its parts from that one on. OpenCode deletes them with the
// next prompt, or brings them back with redo; Convert keeps them, as the
// database still holds them, so for such an export the fold of these lines
// is Convert of the export without them.
func OpenCodeSnapshotLines(export []byte) ([][]byte, error) {
	normalized, err := NormalizeOpenCodeExport(bytes.TrimSpace(export))
	if err != nil {
		return nil, err
	}
	var doc struct {
		Info struct {
			Revert *openCodeRevert `json:"revert"`
		} `json:"info"`
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(normalized, &doc); err != nil {
		return nil, fmt.Errorf("parsing opencode export: %w", err)
	}
	messages := doc.Messages
	if r := doc.Info.Revert; r != nil && r.MessageID != "" {
		if messages, err = openCodeBeforeRevert(messages, *r); err != nil {
			return nil, err
		}
	}
	lines := make([][]byte, 0, len(messages))
	for _, raw := range messages {
		var buf bytes.Buffer
		if err := json.Compact(&buf, raw); err != nil {
			return nil, fmt.Errorf("compacting opencode message: %w", err)
		}
		lines = append(lines, buf.Bytes())
	}
	return lines, nil
}

// openCodeRevert is a session's pending revert (info.revert).
type openCodeRevert struct {
	MessageID string `json:"messageID"`
	PartID    string `json:"partID"`
}

// openCodeBeforeRevert is messages without what revert r hides: from the
// message r names (by its ID, or, for OpenCode 2, an ID among the source
// messages it carries: one after its own, an idle marker, hides only the
// later messages) on, or, when r names one of that message's parts after
// its first, that message's parts from that one on and every later
// message. A revert naming no message of the export hides nothing.
func openCodeBeforeRevert(messages []json.RawMessage, r openCodeRevert) ([]json.RawMessage, error) {
	for i, raw := range messages {
		msg, err := decodeOpenCodeRevertTarget(raw)
		if err != nil {
			return nil, err
		}
		hit, after := msg.reverts(r.MessageID)
		switch {
		case !hit:
			continue
		case after:
			return messages[: i+1 : i+1], nil
		}
		kept := messages[:i:i]
		k := msg.partIndex(r.PartID)
		if k <= 0 {
			return kept, nil
		}
		cut, err := openCodeFirstParts(raw, k)
		if err != nil {
			return nil, err
		}
		return append(kept, cut), nil
	}
	return messages, nil
}

// openCodeRevertTarget is what a revert reads of a snapshot line: its ID,
// its OpenCode 2 source messages' IDs and its parts' IDs.
type openCodeRevertTarget struct {
	Info struct {
		ID     string `json:"id"`
		Native []struct {
			ID string `json:"id"`
		} `json:"opencode2"`
	} `json:"info"`
	Parts []struct {
		ID string `json:"id"`
	} `json:"parts"`
}

func decodeOpenCodeRevertTarget(raw json.RawMessage) (openCodeRevertTarget, error) {
	var msg openCodeRevertTarget
	if err := json.Unmarshal(raw, &msg); err != nil {
		return msg, fmt.Errorf("parsing opencode message: %w", err)
	}
	return msg, nil
}

// reverts reports whether a revert of message id hits this message, and
// whether it hits one of the OpenCode 2 source messages after its own (an
// idle marker), which keeps this message. Its sources are the ones before
// it that convert to nothing, itself, then those after it.
func (m openCodeRevertTarget) reverts(id string) (hit, after bool) {
	if m.Info.ID == id {
		return true, false
	}
	own := false
	for _, n := range m.Info.Native {
		own = own || n.ID == m.Info.ID
		if n.ID == id {
			return true, own
		}
	}
	return false, false
}

// partIndex is the index of part id, -1 when id is empty or not a part.
func (m openCodeRevertTarget) partIndex(id string) int {
	if id == "" {
		return -1
	}
	for k, part := range m.Parts {
		if part.ID == id {
			return k
		}
	}
	return -1
}

// openCodeFirstParts is message raw with only its first k parts.
func openCodeFirstParts(raw json.RawMessage, k int) (json.RawMessage, error) {
	var whole map[string]json.RawMessage
	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &whole); err != nil {
		return nil, fmt.Errorf("parsing opencode message: %w", err)
	}
	if err := json.Unmarshal(whole["parts"], &parts); err != nil {
		return nil, fmt.Errorf("parsing opencode message parts: %w", err)
	}
	trimmed, err := json.Marshal(parts[:k])
	if err != nil {
		return nil, fmt.Errorf("encoding opencode message parts: %w", err)
	}
	whole["parts"] = trimmed
	cut, err := json.Marshal(whole)
	if err != nil {
		return nil, fmt.Errorf("encoding opencode message: %w", err)
	}
	return cut, nil
}
