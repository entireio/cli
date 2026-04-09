package compact

import (
	"bufio"
	"bytes"
	"encoding/json"

	"github.com/entireio/cli/cmd/entire/cli/transcript"
)

// isDroidFormat checks whether JSONL content uses Factory AI Droid's envelope
// format. It scans lines looking for one with a recognizable "type" field - if
// the first such line has type "message" with a nested "message" object, it's
// Droid format. Unrecognized types (session_start, session_event) are skipped.
func isDroidFormat(content []byte) bool {
	scanner := bufio.NewScanner(bytes.NewReader(content))
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var probe struct {
			Type    string           `json:"type"`
			Message *json.RawMessage `json:"message"`
		}
		if json.Unmarshal(line, &probe) != nil {
			continue
		}
		if probe.Type == "message" && probe.Message != nil {
			return true
		}
		// If we hit a known Claude Code/Cursor type, it's not Droid.
		if userAliases[probe.Type] || probe.Type == transcript.TypeAssistant || droppedTypes[probe.Type] {
			return false
		}
	}
	if scanner.Err() != nil {
		return false
	}
	return false
}

// compactDroid converts Factory AI Droid JSONL transcripts into the compact
// format. Droid uses the same Anthropic Messages API structure as Claude Code
// and Cursor, but wraps each message in an envelope that must be unwrapped first.
func compactDroid(content []byte, opts MetadataFields) ([]byte, error) {
	return compactJSONLWith(content, opts, unwrapDroidEnvelope)
}

// unwrapDroidEnvelope flattens a Droid envelope line in place:
//
//	{"type":"message","id":"m1","timestamp":"t1","message":{"role":"user","content":...}}
//
// becomes a map with role promoted to type, outer timestamp/id carried over,
// and the outer id injected into the inner message so parseLine can extract it.
// Non-envelope lines are returned unchanged (and dropped by normalizeKind).
func unwrapDroidEnvelope(raw map[string]json.RawMessage) map[string]json.RawMessage {
	if unquote(raw["type"]) != "message" {
		return raw
	}

	msgRaw, ok := raw["message"]
	if !ok {
		return raw
	}

	var inner map[string]json.RawMessage
	if json.Unmarshal(msgRaw, &inner) != nil {
		return raw
	}

	role := unquote(inner["role"])
	if !userAliases[role] && role != transcript.TypeAssistant {
		return raw
	}

	// Build flat line: promote role to type, carry over outer timestamp/id.
	flat := make(map[string]json.RawMessage, 4)
	flat["type"] = inner["role"]
	if v, has := raw["timestamp"]; has {
		flat["timestamp"] = v
	}

	// Inject outer id into inner message so parseMessage can find it.
	if outerID, has := raw["id"]; has {
		if _, hasInner := inner["id"]; !hasInner {
			inner["id"] = outerID
		}
	}
	rebuilt, err := json.Marshal(inner)
	if err != nil {
		flat["message"] = msgRaw
	} else {
		flat["message"] = rebuilt
	}

	return flat
}
