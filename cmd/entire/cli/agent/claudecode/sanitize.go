package claudecode

import (
	"bytes"
	"encoding/json"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
)

var _ agent.TranscriptSanitizer = (*ClaudeCodeAgent)(nil)

// The records in which Claude Code writes the text it attaches to its model
// requests: the request log (each request message and the system prompt with
// its tool definitions), the system prompt snapshot, and the instruction files
// it loaded (CLAUDE.md, AGENTS.md, memory notes).
const (
	recordTypeAPIRequestBlob  = "api-request-blob"
	recordTypeAPIRequestShape = "api-request-shape"
	recordTypeAttachment      = "attachment"
	attachmentPromptSnapshot  = "prompt_snapshot"
	attachmentInstructions    = "instructions"
)

// injectedRecordMarkers gate the per-line parse: a line naming none of these
// record kinds cannot be one, so a byte search skips it. Over-broad on purpose
// (a prompt that mentions a kind is parsed and then left alone).
var injectedRecordMarkers = [][]byte{
	[]byte(`"` + recordTypeAPIRequestBlob + `"`),
	[]byte(`"` + recordTypeAPIRequestShape + `"`),
	[]byte(`"` + attachmentPromptSnapshot + `"`),
	[]byte(`"` + attachmentInstructions + `"`),
}

var (
	emptyJSONArray  = json.RawMessage(`[]`)
	emptyJSONString = json.RawMessage(`""`)
)

// SanitizeTranscriptForStorage implements agent.TranscriptSanitizer.
func (c *ClaudeCodeAgent) SanitizeTranscriptForStorage(data []byte) []byte {
	return SanitizeStoredTranscript(data)
}

// SanitizeStoredTranscript blanks the text Claude Code attaches to its model
// requests. That text comes from outside the conversation (the system prompt,
// instruction files, the developer's memory notes) and nothing Entire stores a
// transcript for reads it; Claude Code re-reads its instructions when it
// resumes a transcript without them.
//
// Each record stays, with its type, ids, hashes and file paths, and no line is
// added or removed: checkpoint offsets count lines in this file. Lines it does
// not change keep their exact bytes.
func SanitizeStoredTranscript(data []byte) []byte {
	if !containsAny(data, injectedRecordMarkers) {
		return data
	}

	lines := bytes.Split(data, []byte("\n"))
	changed := false
	for i, line := range lines {
		if !containsAny(line, injectedRecordMarkers) {
			continue
		}
		if blanked, ok := blankInjectedRecord(line); ok {
			lines[i] = blanked
			changed = true
		}
	}
	if !changed {
		return data
	}
	return bytes.Join(lines, []byte("\n"))
}

// blankInjectedRecord returns line with its injected text blanked. ok is false
// when line is not one of the injected records, cannot be parsed, or holds
// nothing left to blank; the caller then keeps the original bytes.
func blankInjectedRecord(line []byte) ([]byte, bool) {
	var record map[string]json.RawMessage
	if err := json.Unmarshal(line, &record); err != nil {
		return nil, false
	}

	var changed bool
	switch jsonString(record["type"]) {
	case recordTypeAPIRequestBlob:
		changed = blankFields(record, "message", "content")
	case recordTypeAPIRequestShape:
		changed = blankFields(record, "shape", "system", "tools")
	case recordTypeAttachment:
		changed = blankAttachment(record)
	}
	if !changed {
		return nil, false
	}

	encoded, err := jsonutil.MarshalWithNoHTMLEscape(record)
	if err != nil {
		return nil, false
	}
	return encoded, true
}

func blankAttachment(record map[string]json.RawMessage) bool {
	var attachment map[string]json.RawMessage
	if err := json.Unmarshal(record["attachment"], &attachment); err != nil {
		return false
	}
	switch jsonString(attachment["type"]) {
	case attachmentPromptSnapshot:
		return blankFields(record, "attachment", "systemPrompt", "tools")
	case attachmentInstructions:
		// The file bodies, and the same text as it was rendered into the request.
		files := blankContentOfEach(attachment, "files")
		if files {
			encoded, err := jsonutil.MarshalWithNoHTMLEscape(attachment)
			if err != nil {
				return false
			}
			record["attachment"] = encoded
		}
		rendered := blankContentOfEach(record, "rendered")
		return files || rendered
	}
	return false
}

// blankFields empties the named fields of the object stored under key, and
// reports whether any of them held something.
func blankFields(record map[string]json.RawMessage, key string, fields ...string) bool {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(record[key], &object); err != nil || object == nil {
		return false
	}
	changed := false
	for _, field := range fields {
		if blank, ok := blankValue(object[field]); ok {
			object[field] = blank
			changed = true
		}
	}
	if !changed {
		return false
	}
	encoded, err := jsonutil.MarshalWithNoHTMLEscape(object)
	if err != nil {
		return false
	}
	record[key] = encoded
	return true
}

// blankContentOfEach empties the "content" of every object in the array stored
// under key, and reports whether any of them held something.
func blankContentOfEach(record map[string]json.RawMessage, key string) bool {
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(record[key], &items); err != nil {
		return false
	}
	changed := false
	for _, item := range items {
		if blank, ok := blankValue(item["content"]); ok {
			item["content"] = blank
			changed = true
		}
	}
	if !changed {
		return false
	}
	encoded, err := jsonutil.MarshalWithNoHTMLEscape(items)
	if err != nil {
		return false
	}
	record[key] = encoded
	return true
}

// blankValue returns the empty value of raw's own JSON type, so a blanked field
// still has the type its reader expects. ok is false when raw is neither an
// array nor a string, or is already empty.
func blankValue(raw json.RawMessage) (json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, false
	}
	var blank json.RawMessage
	switch trimmed[0] {
	case '[':
		blank = emptyJSONArray
	case '"':
		blank = emptyJSONString
	default:
		return nil, false
	}
	if bytes.Equal(trimmed, blank) {
		return nil, false
	}
	return blank, true
}

func jsonString(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}
