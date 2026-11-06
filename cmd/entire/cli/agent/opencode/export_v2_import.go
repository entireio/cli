package opencode

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/entireio/cli/redact"
)

// errNotOpenCode2Capture reports a stored transcript that was not captured
// from OpenCode 2, so it holds no OpenCode 2 messages to import.
var errNotOpenCode2Capture = errors.New("transcript was not captured from OpenCode 2")

// openCodeBase64 is OpenCode 2's Prompt.Base64 pattern. Its import rejects
// the whole session when one attachment's data does not match, which a
// redacted or externalized attachment may no longer do.
var openCodeBase64 = regexp.MustCompile(`^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$`)

// openCodeImportInfoKeys are the session info fields a resume gives back to
// OpenCode 2. The rest are deliberately not restored from a checkpoint:
// permissions, agent and metadata would let whoever can push checkpoint data
// change how tools are approved in the resuming user's OpenCode, and
// parentID, fork and revert point at state the resuming store may not have.
var openCodeImportInfoKeys = []string{"id", "projectID", "model", "cost", "tokens", "outcome", "time", "title", "location", "subpath"}

// openCodeV2ImportPayload rebuilds the OpenCode 2 export a stored transcript
// was converted from (see normalizeOpenCodeExport), for `opencode session
// import`. The transcript must be sessionID's: the import keeps the ID it
// carries. directory becomes the session's location. Attachments whose data
// redaction or image externalization changed are dropped, since OpenCode
// validates them and rejects the whole import otherwise.
func openCodeV2ImportPayload(data []byte, sessionID, directory string) ([]byte, error) {
	var doc struct {
		Info     map[string]any `json:"info"`
		Messages []struct {
			Info map[string]json.RawMessage `json:"info"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse stored OpenCode transcript: %w", err)
	}
	if doc.Info == nil {
		return nil, errNotOpenCode2Capture
	}
	if id := v2String(doc.Info, "id"); id != sessionID {
		return nil, fmt.Errorf("stored OpenCode transcript is for session %q, not %q", id, sessionID)
	}
	messages := make([]any, 0, len(doc.Messages))
	for _, msg := range doc.Messages {
		raw, ok := msg.Info[openCodeNativeKey]
		if !ok {
			return nil, errNotOpenCode2Capture
		}
		var native []map[string]any
		if err := json.Unmarshal(raw, &native); err != nil {
			return nil, fmt.Errorf("parse stored OpenCode 2 message: %w", err)
		}
		for _, m := range native {
			messages = append(messages, dropInvalidAttachments(m))
		}
	}

	info := make(map[string]any, len(openCodeImportInfoKeys))
	for _, k := range openCodeImportInfoKeys {
		if v, ok := doc.Info[k]; ok {
			info[k] = v
		}
	}
	loc, ok := info["location"].(map[string]any)
	if !ok {
		loc = map[string]any{}
	}
	loc = copyMap(loc)
	loc["directory"] = directory
	info["location"] = loc

	out, err := json.Marshal(map[string]any{"info": info, "messages": messages})
	if err != nil {
		return nil, fmt.Errorf("encode OpenCode 2 import: %w", err)
	}
	return out, nil
}

// dropInvalidAttachments removes a user message's file attachments, and
// replaces an assistant tool result's non-text content, whose base64 data
// is no longer valid.
func dropInvalidAttachments(msg map[string]any) map[string]any {
	if files, ok := msg["files"].([]any); ok {
		kept := make([]any, 0, len(files))
		for _, f := range files {
			if !hasInvalidData(f) {
				kept = append(kept, f)
			}
		}
		msg["files"] = kept
	}
	content, ok := msg["content"].([]any)
	if !ok {
		return msg
	}
	for _, c := range content {
		part, ok := c.(map[string]any)
		if !ok {
			continue
		}
		state, ok := part["state"].(map[string]any)
		if !ok {
			continue
		}
		results, ok := state["content"].([]any)
		if !ok {
			continue
		}
		for i, r := range results {
			entry, ok := r.(map[string]any)
			if ok && entry["type"] != partTypeText && hasInvalidData(entry) {
				results[i] = map[string]any{"type": partTypeText, "text": "[attachment not restored]"}
			}
		}
	}
	return msg
}

// hasInvalidData reports whether v holds a "data" string, at any depth,
// that is not valid base64 or that redaction replaced. A redacted value is
// usually "REDACTED", itself valid base64, which would import as a corrupt
// attachment that the provider then rejects on every turn.
func hasInvalidData(v any) bool {
	switch val := v.(type) {
	case map[string]any:
		for k, inner := range val {
			if s, ok := inner.(string); ok && k == "data" && (!openCodeBase64.MatchString(s) || strings.Contains(s, redact.RedactedPlaceholder)) {
				return true
			}
			if hasInvalidData(inner) {
				return true
			}
		}
	case []any:
		for _, inner := range val {
			if hasInvalidData(inner) {
				return true
			}
		}
	}
	return false
}

func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
