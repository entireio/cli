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
// its tool definitions) and attachments (the system prompt snapshot, the
// instruction files it loaded, session context, skill and tool listings, hook
// output, and more).
const (
	recordTypeAPIRequestBlob  = "api-request-blob"
	recordTypeAPIRequestShape = "api-request-shape"
	recordTypeAttachment      = "attachment"
	attachmentPromptSnapshot  = "prompt_snapshot"
	attachmentInstructions    = "instructions"

	// attachmentBlankedPromptSnapshot replaces a blanked snapshot's kind. Claude
	// Code reuses a recorded prompt_snapshot verbatim on resume, even an empty
	// one, so a stored transcript resumed with the original kind would run with
	// no system prompt; under a kind it does not know, it renders a fresh one.
	attachmentBlankedPromptSnapshot = "entire_blanked_prompt_snapshot"
)

// keptAttachmentKinds hold what the developer shared in the conversation:
// prompts queued while the agent worked, @-mentioned files, IDE selections, and
// edits Claude Code reports back. They are conversation, and Claude Code does
// not re-read them on resume, so they stay as written. Every other kind is
// blanked, including kinds Claude Code adds later.
var keptAttachmentKinds = map[string]bool{
	"queued_command":         true,
	"file":                   true,
	"already_read_file":      true,
	"at_mention_reference":   true,
	"pdf_reference":          true,
	"compact_file_reference": true,
	"selected_lines_in_ide":  true,
	"opened_file_in_ide":     true,
	"edited_text_file":       true,
	"edited_image_file":      true,
}

// structuralKeys survive blanking at any depth: a discriminator a reader
// dispatches on, and file paths, which the record keeps like every other
// blanked record does.
var structuralKeys = map[string]bool{
	"type":        true,
	"path":        true,
	"filePath":    true,
	"filename":    true,
	"displayPath": true,
}

// injectedRecordMarkers gate the per-line parse: a line naming none of these
// record kinds cannot be one, so a byte search skips it. Over-broad on purpose
// (a prompt that mentions a kind is parsed and then left alone).
var injectedRecordMarkers = [][]byte{
	[]byte(`"` + recordTypeAPIRequestBlob + `"`),
	[]byte(`"` + recordTypeAPIRequestShape + `"`),
	[]byte(`"` + recordTypeAttachment + `"`),
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
// instruction files, the developer's memory notes, session context) and nothing
// Entire stores a transcript for reads it; Claude Code re-reads its
// instruction files when it resumes a transcript without them, and renders a
// fresh system prompt because the blanked snapshot no longer is one (see
// attachmentBlankedPromptSnapshot). Attachments are
// blanked unless kept on purpose, because Claude Code adds new kinds often.
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
	kind := jsonString(attachment["type"])
	if keptAttachmentKinds[kind] {
		return false
	}
	switch kind {
	case attachmentPromptSnapshot:
		for _, field := range []string{"systemPrompt", "tools"} {
			if blank, ok := blankValue(attachment[field]); ok {
				attachment[field] = blank
			}
		}
		attachment["type"] = json.RawMessage(`"` + attachmentBlankedPromptSnapshot + `"`)
		encoded, err := jsonutil.MarshalWithNoHTMLEscape(attachment)
		if err != nil {
			return false
		}
		record["attachment"] = encoded
		return true
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
	default:
		blanked := blankAllText(attachment)
		if blanked {
			encoded, err := jsonutil.MarshalWithNoHTMLEscape(attachment)
			if err != nil {
				return false
			}
			record["attachment"] = encoded
		}
		rendered := blankContentOfEach(record, "rendered")
		return blanked || rendered
	}
}

// blankAllText empties every string and array in object, at any depth, except
// structuralKeys, and reports whether any of them held something. Numbers,
// booleans and nulls stay: they carry no text.
func blankAllText(object map[string]json.RawMessage) bool {
	changed := false
	for key, value := range object {
		if structuralKeys[key] {
			continue
		}
		if blank, ok := blankText(value); ok {
			object[key] = blank
			changed = true
		}
	}
	return changed
}

func blankText(raw json.RawMessage) (json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return blankValue(raw)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &object); err != nil || !blankAllText(object) {
		return nil, false
	}
	encoded, err := jsonutil.MarshalWithNoHTMLEscape(object)
	if err != nil {
		return nil, false
	}
	return encoded, true
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
