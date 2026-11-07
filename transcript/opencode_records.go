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
func OpenCodeSnapshotLines(export []byte) ([][]byte, error) {
	normalized, err := NormalizeOpenCodeExport(bytes.TrimSpace(export))
	if err != nil {
		return nil, err
	}
	var doc struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(normalized, &doc); err != nil {
		return nil, fmt.Errorf("parsing opencode export: %w", err)
	}
	lines := make([][]byte, 0, len(doc.Messages))
	for _, raw := range doc.Messages {
		var buf bytes.Buffer
		if err := json.Compact(&buf, raw); err != nil {
			return nil, fmt.Errorf("compacting opencode message: %w", err)
		}
		lines = append(lines, buf.Bytes())
	}
	return lines, nil
}
