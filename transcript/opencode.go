package transcript

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/entireio/cli/internal/transcript/opencodev2"
)

// --- OpenCode format support ---
//
// OpenCode transcripts are a single JSON object (not JSONL):
//
//	{"info":{...},"messages":[{"info":{"role":"user","time":{...}},"parts":[...]}, ...]}
//
// Parts use "type" values: "text", "tool", "step-start", "step-finish".
// Tool parts store the tool name in "tool" (string) and call details in "state".

// isOpenCodeFormat checks whether content is a single JSON object with the
// OpenCode session shape (top-level "info" and "messages" keys).
func isOpenCodeFormat(content []byte) bool {
	trimmed := bytes.TrimSpace(content)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	// Quick structural check: unmarshal just enough to detect the keys.
	var probe struct {
		Info     *json.RawMessage `json:"info"`
		Messages *json.RawMessage `json:"messages"`
	}
	if json.Unmarshal(trimmed, &probe) != nil {
		return false
	}
	return probe.Info != nil && probe.Messages != nil
}

// openCodeMessage mirrors the OpenCode message structure for unmarshaling.
type openCodeMessage struct {
	Info  openCodeMessageInfo          `json:"info"`
	Parts []map[string]json.RawMessage `json:"parts"`
}

type openCodeMessageInfo struct {
	ID     string            `json:"id"`
	Role   string            `json:"role"`
	Time   openCodeMsgTime   `json:"time"`
	Tokens *openCodeMsgToken `json:"tokens"`
}

type openCodeMsgTime struct {
	Created   int64 `json:"created"`
	Completed int64 `json:"completed"`
}

type openCodeMsgToken struct {
	Input  int `json:"input"`
	Output int `json:"output"`
}

// OpenCode part types this package reads beyond text.
const (
	openCodePartTool       = "tool"
	openCodePartCompaction = "compaction"
)

// openCodeToolTask is OpenCode 1's subagent tool (OpenCode 2's "subagent").
const openCodeToolTask = "task"

// compactOpenCode converts a full OpenCode session JSON, either version
// (opencodev2.Normalize), into transcript lines. opts.StartLine is treated
// as a message-index offset (not a newline offset) because the OpenCode
// transcript is a single JSON object. It folds the records openCodeRecord
// gives each message, as OpenCodeAdapter streams them.
func compactOpenCode(content []byte, opts Options) ([]byte, error) {
	normalized, err := opencodev2.Normalize(bytes.TrimSpace(content))
	if err != nil {
		return nil, fmt.Errorf("converting opencode 2 export: %w", err)
	}
	var session struct {
		Messages []openCodeMessage `json:"messages"`
	}
	if err := json.Unmarshal(normalized, &session); err != nil {
		return nil, fmt.Errorf("parsing opencode session: %w", err)
	}

	messages := session.Messages
	if opts.StartLine > 0 {
		if opts.StartLine >= len(messages) {
			return []byte{}, nil
		}
		messages = messages[opts.StartLine:]
	}

	records := make([]Record, 0, len(messages))
	seen := make(map[string]bool, len(messages))
	for i, msg := range messages {
		// The fold key is the message ID. Convert also keeps a message
		// without one, or one repeating an earlier ID, as its own line.
		key := msg.Info.ID
		if key == "" || seen[key] {
			key = "\x00" + strconv.Itoa(i)
		}
		seen[key] = true
		if r, ok := openCodeRecord(key, msg); ok {
			records = append(records, r)
		}
	}
	return Fold(records, opts)
}

// openCodeRecord is msg's OpMessage under fold key key, false for a role
// other than user or assistant. An assistant message with no text or tool
// parts gets content [] (no line) when it has no tokens, and no content
// (a "content":null line) when it has some, so its usage still has a line.
// Its usage is a snapshot, so a later record replaces it whole.
func openCodeRecord(key string, msg openCodeMessage) (Record, bool) {
	r := Record{Op: OpMessage, ID: key, Role: msg.Info.Role, TS: msToTimestamp(msg.Info.Time.Created)}
	switch msg.Info.Role {
	case TypeUser:
		content, err := json.Marshal(openCodeUserBlocks(msg))
		if err != nil {
			return Record{}, false
		}
		r.Content = content
	case TypeAssistant:
		r.SourceID = msg.Info.ID
		r.Usage = &CallUsage{}
		if t := msg.Info.Tokens; t != nil {
			r.Usage.InputTokens = int64(t.Input)
			r.Usage.OutputTokens = int64(t.Output)
		}
		blocks := openCodeAssistantBlocks(msg)
		if len(blocks) == 0 && (r.Usage.InputTokens != 0 || r.Usage.OutputTokens != 0) {
			return r, true
		}
		content, err := json.Marshal(blocks)
		if err != nil {
			return Record{}, false
		}
		r.Content = content
	default:
		return Record{}, false
	}
	return r, true
}

// openCodeUserBlocks is a user message's text blocks, IDE context tags
// stripped; nil when it has none.
func openCodeUserBlocks(msg openCodeMessage) []json.RawMessage {
	var blocks []json.RawMessage
	for _, part := range msg.Parts {
		if unquote(part["type"]) != ContentTypeText {
			continue
		}
		text := StripIDEContextTags(unquote(part[ContentTypeText]))
		if text == "" {
			continue
		}
		tb := userTextBlock{Text: text}
		if id := part["id"]; id != nil {
			_ = json.Unmarshal(id, &tb.ID) //nolint:errcheck // best-effort
		}
		b, err := json.Marshal(tb)
		if err != nil {
			continue
		}
		blocks = append(blocks, b)
	}
	return blocks
}

// openCodeAssistantBlocks is an assistant message's text and tool_use
// blocks, the results of finished tools inlined.
func openCodeAssistantBlocks(msg openCodeMessage) []map[string]json.RawMessage {
	content := make([]map[string]json.RawMessage, 0, len(msg.Parts))

	for _, part := range msg.Parts {
		switch unquote(part["type"]) {
		case ContentTypeText:
			b, err := json.Marshal(ContentTypeText)
			if err != nil {
				continue
			}
			content = append(content, map[string]json.RawMessage{
				"type": b,
				"text": part[ContentTypeText],
			})
		case openCodePartTool:
			if block, ok := openCodeToolBlock(part); ok {
				content = append(content, block)
			}
		}
	}
	return content
}

// openCodeToolBlock is a tool part's tool_use block: its call ID, name and
// input, and its result once the tool finished.
func openCodeToolBlock(part map[string]json.RawMessage) (map[string]json.RawMessage, bool) {
	b, err := json.Marshal(ContentTypeToolUse)
	if err != nil {
		return nil, false
	}
	toolBlock := map[string]json.RawMessage{"type": b}
	if callID := part["callID"]; callID != nil {
		toolBlock["id"] = callID
	}
	if toolName := part["tool"]; toolName != nil {
		toolBlock["name"] = toolName
	}
	var state map[string]json.RawMessage
	if stateRaw := part["state"]; stateRaw == nil || json.Unmarshal(stateRaw, &state) != nil {
		return toolBlock, true
	}
	if inp := state["input"]; inp != nil {
		toolBlock["input"] = inp
	}
	if openCodeToolDone(state) {
		toolBlock["result"] = openCodeToolResult(unquote(part["tool"]), state)
	}
	return toolBlock, true
}

// openCodeToolDone reports whether a tool part's state has a result: a
// pending or running tool has none yet (a live stream shows it running).
func openCodeToolDone(state map[string]json.RawMessage) bool {
	switch unquote(state["status"]) {
	case "pending", "running":
		return false
	}
	return true
}

// openCodeToolResult builds the compact {"output":"...","status":"success"|"error"}
// object from an OpenCode tool state map.
func openCodeToolResult(tool string, state map[string]json.RawMessage) json.RawMessage {
	r := ToolResult{
		Output: unquote(state["output"]),
		Status: ToolStatusSuccess,
	}
	if s := unquote(state["status"]); s != "" && s != "completed" {
		r.Status = ToolStatusError
		// A failed tool's message is its state's error, not output (1.x
		// ToolStateError, and the 2.x conversion), as Codex's MCP calls do.
		if r.Output == "" {
			r.Output = unquote(state["error"])
		}
	}
	// The task tool records the subagent's session once it starts. Other
	// tools' metadata is not theirs to interpret.
	var metadata struct {
		SessionID string `json:"sessionId"`
	}
	if tool == openCodeToolTask && json.Unmarshal(state["metadata"], &metadata) == nil && metadata.SessionID != "" {
		r.Agent = &ToolResultAgent{ID: metadata.SessionID}
	}
	b, err := json.Marshal(r)
	if err != nil {
		return nil
	}
	return b
}

// msToTimestamp converts a Unix millisecond timestamp to an RFC3339 JSON string.
func msToTimestamp(ms int64) json.RawMessage {
	if ms == 0 {
		return nil
	}
	t := time.UnixMilli(ms).UTC()
	b, err := json.Marshal(t.Format(time.RFC3339Nano))
	if err != nil {
		return nil
	}
	return b
}
