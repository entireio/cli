package compact

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/textutil"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
)

// openCodeToolPart is the "type" of an OpenCode tool-call content part, and the
// key that names the tool in the v1 part map.
const openCodeToolPart = "tool"

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

// compactOpenCode converts a full OpenCode session JSON into transcript lines.
// opts.StartLine is treated as a message-index offset (not a newline offset)
// because the OpenCode transcript is a single JSON object.
func compactOpenCode(content []byte, opts MetadataFields) ([]byte, error) {
	var raw struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(content), &raw); err != nil {
		return nil, fmt.Errorf("parsing opencode session: %w", err)
	}

	messages := make([]openCodeMessage, 0, len(raw.Messages))
	for _, rawMessage := range raw.Messages {
		msg, err := normalizeOpenCodeMessage(rawMessage)
		if err != nil {
			return nil, err
		}
		messages = append(messages, msg)
	}

	if opts.StartLine > 0 {
		if opts.StartLine >= len(messages) {
			return []byte{}, nil
		}
		messages = messages[opts.StartLine:]
	}

	base := newTranscriptLine(opts)
	var result []byte

	for _, msg := range messages {
		ts := msToTimestamp(msg.Info.Time.Created)

		switch msg.Info.Role {
		case transcript.TypeUser:
			emitOpenCodeUser(&result, base, msg, ts)
		case transcript.TypeAssistant:
			emitOpenCodeAssistant(&result, base, msg, ts)
		}
	}

	return result, nil
}

func emitOpenCodeUser(result *[]byte, base transcriptLine, msg openCodeMessage, ts json.RawMessage) {
	var blocks []json.RawMessage

	for _, part := range msg.Parts {
		if unquote(part["type"]) != transcript.ContentTypeText {
			continue
		}
		text := textutil.StripIDEContextTags(unquote(part[transcript.ContentTypeText]))
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

	contentJSON, err := json.Marshal(blocks)
	if err != nil {
		return
	}

	line := base
	line.Type = transcript.TypeUser
	line.TS = ts
	line.Content = contentJSON
	appendLine(result, line)
}

func emitOpenCodeAssistant(result *[]byte, base transcriptLine, msg openCodeMessage, ts json.RawMessage) {
	content := make([]map[string]json.RawMessage, 0, len(msg.Parts))

	for _, part := range msg.Parts {
		partType := unquote(part["type"])

		switch partType {
		case transcript.ContentTypeText:
			b, err := json.Marshal(transcript.ContentTypeText)
			if err != nil {
				continue
			}
			content = append(content, map[string]json.RawMessage{
				"type": b,
				"text": part[transcript.ContentTypeText],
			})
		case openCodeToolPart:
			toolBlock := make(map[string]json.RawMessage)
			b, err := json.Marshal(transcript.ContentTypeToolUse)
			if err != nil {
				continue
			}
			toolBlock["type"] = b
			if callID := part["callID"]; callID != nil {
				toolBlock["id"] = callID
			}
			if toolName := part[openCodeToolPart]; toolName != nil {
				toolBlock["name"] = toolName
			}
			if stateRaw := part["state"]; stateRaw != nil {
				var state map[string]json.RawMessage
				if json.Unmarshal(stateRaw, &state) == nil {
					if inp := state["input"]; inp != nil {
						toolBlock["input"] = inp
					}
					toolBlock["result"] = openCodeToolResult(state)
				}
			}
			content = append(content, toolBlock)
		}
	}

	contentJSON, err := json.Marshal(content)
	if err != nil {
		return
	}

	line := base
	line.Type = transcript.TypeAssistant
	line.TS = ts
	line.ID = msg.Info.ID
	line.Content = contentJSON
	if msg.Info.Tokens != nil {
		line.InputTokens = msg.Info.Tokens.Input
		line.OutputTokens = msg.Info.Tokens.Output
	}
	appendLine(result, line)
}

// openCodeToolResult builds the compact {"output":"...","status":"success"|"error"}
// object from an OpenCode tool state map.
func openCodeToolResult(state map[string]json.RawMessage) json.RawMessage {
	r := toolResultJSON{
		Output: unquote(state["output"]),
		Status: toolResultStatusSuccess,
	}
	if s := unquote(state["status"]); s != "" && s != "completed" {
		r.Status = toolResultStatusError
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

// normalizeOpenCodeMessage converts either OpenCode export shape into the v1
// message the emitters below consume. OpenCode 1 nests metadata under "info" and
// content under "parts"; OpenCode 2 carries "type" plus a typed "content" array.
func normalizeOpenCodeMessage(raw json.RawMessage) (openCodeMessage, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return openCodeMessage{}, fmt.Errorf("parsing opencode message: %w", err)
	}
	if _, ok := probe["info"]; ok {
		var v1 openCodeMessage
		if err := json.Unmarshal(raw, &v1); err != nil {
			return openCodeMessage{}, fmt.Errorf("parsing opencode v1 message: %w", err)
		}
		return v1, nil
	}

	var v2 openCodeMessageV2
	if err := json.Unmarshal(raw, &v2); err != nil {
		return openCodeMessage{}, fmt.Errorf("parsing opencode v2 message: %w", err)
	}

	out := openCodeMessage{
		Info: openCodeMessageInfo{
			ID:     v2.ID,
			Role:   v2.Type,
			Time:   v2.Time,
			Tokens: v2.Tokens,
		},
	}

	for _, content := range v2.Content {
		switch content.Type {
		case transcript.ContentTypeText:
			out.Parts = append(out.Parts, map[string]json.RawMessage{
				"type": rawJSON(transcript.ContentTypeText),
				"text": rawJSON(content.Text),
			})
		case openCodeToolPart:
			var status, output string
			if content.State != nil {
				status = content.State.Status
				output = content.State.Output
				if output == "" {
					var texts []string
					for _, c := range content.State.Content {
						if c.Type == transcript.ContentTypeText && c.Text != "" {
							texts = append(texts, c.Text)
						}
					}
					output = strings.Join(texts, "\n")
				}
			}
			out.Parts = append(out.Parts, map[string]json.RawMessage{
				"type":           rawJSON(openCodeToolPart),
				openCodeToolPart: rawJSON(content.Name),
				"callID":         rawJSON(content.ID),
				"state":          marshalRaw(map[string]json.RawMessage{"status": rawJSON(status), "output": rawJSON(output)}),
			})
		}
	}

	// User and system messages carry their text at the message level.
	if v2.Text != "" {
		out.Parts = append(out.Parts, map[string]json.RawMessage{
			"type": rawJSON(transcript.ContentTypeText),
			"text": rawJSON(v2.Text),
		})
	}

	return out, nil
}

// openCodeMessageV2 mirrors the OpenCode 2 message shape.
type openCodeMessageV2 struct {
	ID      string              `json:"id"`
	Type    string              `json:"type"`
	Text    string              `json:"text"`
	Time    openCodeMsgTime     `json:"time"`
	Tokens  *openCodeMsgToken   `json:"tokens"`
	Content []openCodeContentV2 `json:"content"`
}

type openCodeContentV2 struct {
	Type  string               `json:"type"`
	Text  string               `json:"text"`
	ID    string               `json:"id"`
	Name  string               `json:"name"`
	State *openCodeToolStateV2 `json:"state"`
}

type openCodeToolStateV2 struct {
	Status  string `json:"status"`
	Output  string `json:"output"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

func rawJSON(s string) json.RawMessage {
	b, err := json.Marshal(s)
	if err != nil {
		return json.RawMessage(`""`)
	}
	return b
}

func marshalRaw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}
