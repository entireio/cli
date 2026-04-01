package compact

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/textutil"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
)

// --- Gemini CLI format support ---
//
// Gemini transcripts are a single JSON object (not JSONL):
//
//	{"sessionId":"...","messages":[{"id":"...","timestamp":"...","type":"user"|"gemini"|"info","content":"...","toolCalls":[...],"thoughts":[...]}]}
//
// Key differences from other formats:
//   - Assistant messages use type "gemini" (not "assistant")
//   - System messages use type "info" and should be dropped
//   - Tool calls are at the message level in a "toolCalls" array
//   - A message can have both content text and toolCalls
//   - Timestamps are ISO strings (not millisecond integers)
//   - Token usage is in "tokens" with "input" and "output" fields

// geminiDroppedTypes are Gemini message types that carry no transcript-relevant data.
var geminiDroppedTypes = map[string]bool{
	"info": true,
}

// isGeminiFormat checks whether content is a single JSON object with the
// Gemini session shape: top-level "sessionId" and "messages" keys, but NO "info"
// key (which distinguishes it from OpenCode format).
func isGeminiFormat(content []byte) bool {
	trimmed := bytes.TrimSpace(content)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	var probe struct {
		SessionID *json.RawMessage `json:"sessionId"`
		Messages  *json.RawMessage `json:"messages"`
		Info      *json.RawMessage `json:"info"`
	}
	if json.Unmarshal(trimmed, &probe) != nil {
		return false
	}
	return probe.SessionID != nil && probe.Messages != nil && probe.Info == nil
}

// geminiMessage mirrors the Gemini message structure for unmarshaling.
// Content is json.RawMessage because Gemini transcripts may encode it as
// either a plain string or an array of content parts.
type geminiMessage struct {
	ID        string           `json:"id"`
	Timestamp string           `json:"timestamp"`
	Type      string           `json:"type"`
	Content   json.RawMessage  `json:"content"`
	ToolCalls []geminiToolCall `json:"toolCalls"`
	Tokens    *geminiTokens    `json:"tokens"`
}

// geminiTokens holds token usage from a Gemini message.
type geminiTokens struct {
	Input  int `json:"input"`
	Output int `json:"output"`
}

// geminiToolCall represents a single tool invocation within a Gemini message.
type geminiToolCall struct {
	ID     string             `json:"id"`
	Name   string             `json:"name"`
	Args   json.RawMessage    `json:"args"`
	Result []geminiToolResult `json:"result"`
	Status string             `json:"status"`
}

// geminiToolResult represents a tool result entry from the Gemini format.
type geminiToolResult struct {
	FunctionResponse struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Response struct {
			Output string `json:"output"`
		} `json:"response"`
	} `json:"functionResponse"`
}

// compactGemini converts a full Gemini session JSON into transcript lines.
// opts.StartLine is treated as a message-index offset (not a newline offset)
// because the Gemini transcript is a single JSON object.
func compactGemini(content []byte, opts MetadataFields) ([]byte, error) {
	var session struct {
		Messages []geminiMessage `json:"messages"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(content), &session); err != nil {
		return nil, fmt.Errorf("parsing gemini session: %w", err)
	}

	messages := session.Messages
	if opts.StartLine > 0 {
		if opts.StartLine >= len(messages) {
			return []byte{}, nil
		}
		messages = messages[opts.StartLine:]
	}

	base := newTranscriptLine(opts)
	var result []byte

	for _, msg := range messages {
		if geminiDroppedTypes[msg.Type] {
			continue
		}

		ts, err := json.Marshal(msg.Timestamp)
		if err != nil {
			continue
		}

		switch msg.Type {
		case transcript.TypeUser:
			emitGeminiUser(&result, base, msg, ts)
		case "gemini":
			emitGeminiAssistant(&result, base, msg, ts)
		}
	}

	return result, nil
}

// emitGeminiUser produces a single user line. Gemini user messages may have
// content as a plain string or an array of content parts.
func emitGeminiUser(result *[]byte, base transcriptLine, msg geminiMessage, ts json.RawMessage) {
	text := textutil.StripIDEContextTags(geminiContentText(msg.Content))
	if text == "" {
		return
	}

	b, err := json.Marshal([]userTextBlock{{Text: text}})
	if err != nil {
		return
	}

	line := base
	line.Type = transcript.TypeUser
	line.TS = ts
	line.Content = b
	appendLine(result, line)
}

// emitGeminiAssistant produces a single assistant line. The content array
// contains text blocks (from content field) and tool_use blocks (from toolCalls).
func emitGeminiAssistant(result *[]byte, base transcriptLine, msg geminiMessage, ts json.RawMessage) {
	content := make([]map[string]json.RawMessage, 0, 1+len(msg.ToolCalls))

	if contentText := geminiContentText(msg.Content); contentText != "" {
		b, err := json.Marshal(transcript.ContentTypeText)
		if err == nil {
			text, err := json.Marshal(contentText)
			if err == nil {
				content = append(content, map[string]json.RawMessage{
					"type": b,
					"text": text,
				})
			}
		}
	}

	for _, tc := range msg.ToolCalls {
		b, err := json.Marshal(transcript.ContentTypeToolUse)
		if err != nil {
			continue
		}
		id, err := json.Marshal(tc.ID)
		if err != nil {
			continue
		}
		name, err := json.Marshal(tc.Name)
		if err != nil {
			continue
		}

		toolBlock := map[string]json.RawMessage{
			"type":   b,
			"id":     id,
			"name":   name,
			"result": geminiToolResultCompact(tc),
		}
		if tc.Args != nil {
			toolBlock["input"] = tc.Args
		}
		content = append(content, toolBlock)
	}

	if len(content) == 0 {
		return
	}

	contentJSON, err := json.Marshal(content)
	if err != nil {
		return
	}

	line := base
	line.Type = transcript.TypeAssistant
	line.TS = ts
	line.ID = msg.ID
	line.Content = contentJSON
	if msg.Tokens != nil {
		line.InputTokens = msg.Tokens.Input
		line.OutputTokens = msg.Tokens.Output
	}
	appendLine(result, line)
}

// geminiContentText extracts the text from a Gemini content field which may
// be either a plain JSON string or an array of content parts (each with a
// "text" field). Returns the concatenated text or "" if content is absent.
func geminiContentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Try plain string first.
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	// Try array of parts with "text" fields.
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var sb strings.Builder
		for _, p := range parts {
			sb.WriteString(p.Text)
		}
		return sb.String()
	}
	return ""
}

// geminiToolResultCompact builds the compact {"output":"...","status":"..."}
// object from a Gemini tool call.
func geminiToolResultCompact(tc geminiToolCall) json.RawMessage {
	output := ""
	if len(tc.Result) > 0 {
		output = tc.Result[0].FunctionResponse.Response.Output
	}

	r := toolResultJSON{
		Output: output,
		Status: "success",
	}
	if tc.Status != "" && tc.Status != "success" {
		r.Status = toolResultStatusError
	}
	b, err := json.Marshal(r)
	if err != nil {
		return nil
	}
	return b
}
