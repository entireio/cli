package pi

import (
	"bytes"
	"encoding/json"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
)

var _ agent.TranscriptSanitizer = (*PiAgent)(nil)

// projectContextField is the system-message section in which Pi records the
// instruction files it loaded (AGENTS.md and the like).
const projectContextField = "project_context"

// projectContextMarker gates the per-line parse: a line that does not name the
// field cannot hold it.
var projectContextMarker = []byte(`"` + projectContextField + `"`)

// SanitizeTranscriptForStorage implements agent.TranscriptSanitizer.
func (a *PiAgent) SanitizeTranscriptForStorage(data []byte) []byte {
	return SanitizeStoredTranscript(data)
}

// SanitizeStoredTranscript blanks the instruction-file text Pi records in its
// system messages. That text comes from outside the conversation, and Pi
// re-reads its instruction files when it resumes a session without it.
//
// Each record stays, with its ids and its other sections, and no line is added
// or removed: checkpoint offsets count lines in this file. Lines it does not
// change keep their exact bytes.
func SanitizeStoredTranscript(data []byte) []byte {
	if !bytes.Contains(data, projectContextMarker) {
		return data
	}

	lines := bytes.Split(data, []byte("\n"))
	changed := false
	for i, line := range lines {
		if !bytes.Contains(line, projectContextMarker) {
			continue
		}
		if blanked, ok := blankProjectContext(line); ok {
			lines[i] = blanked
			changed = true
		}
	}
	if !changed {
		return data
	}
	return bytes.Join(lines, []byte("\n"))
}

// blankProjectContext returns line with its project context emptied. ok is
// false when line holds none, or cannot be parsed; the caller then keeps the
// original bytes.
func blankProjectContext(line []byte) ([]byte, bool) {
	var record map[string]json.RawMessage
	if err := json.Unmarshal(line, &record); err != nil {
		return nil, false
	}

	// A system message carries its sections under "message"; a compaction
	// record carries the system message it replaced under "systemMessage".
	message := blankSectionsOf(record, "message")
	compacted := blankSectionsOf(record, "systemMessage")
	if !message && !compacted {
		return nil, false
	}

	encoded, err := jsonutil.MarshalWithNoHTMLEscape(record)
	if err != nil {
		return nil, false
	}
	return encoded, true
}

// blankSectionsOf empties sections.project_context of the object stored under
// key, and reports whether it held text.
func blankSectionsOf(record map[string]json.RawMessage, key string) bool {
	var holder map[string]json.RawMessage
	if err := json.Unmarshal(record[key], &holder); err != nil || holder == nil {
		return false
	}
	var sections map[string]json.RawMessage
	if err := json.Unmarshal(holder["sections"], &sections); err != nil || sections == nil {
		return false
	}
	var text string
	if err := json.Unmarshal(sections[projectContextField], &text); err != nil || text == "" {
		return false
	}

	sections[projectContextField] = json.RawMessage(`""`)
	encodedSections, err := jsonutil.MarshalWithNoHTMLEscape(sections)
	if err != nil {
		return false
	}
	holder["sections"] = encodedSections
	encodedHolder, err := jsonutil.MarshalWithNoHTMLEscape(holder)
	if err != nil {
		return false
	}
	record[key] = encodedHolder
	return true
}
