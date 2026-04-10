package compact

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/textutil"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
)

const (
	transcriptTypeMessage         = "message"
	codexTypeResponseItem         = "response_item"
	codexTypeFunctionCall         = "function_call"
	codexTypeFunctionCallOutput   = "function_call_output"
	codexTypeCustomToolCall       = "custom_tool_call"
	codexTypeCustomToolCallOutput = "custom_tool_call_output"
)

// isCodexFormat checks whether JSONL content uses the Codex format.
func isCodexFormat(content []byte) bool {
	scanner := bufio.NewScanner(bytes.NewReader(content))
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var probe struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(line, &probe) != nil {
			continue
		}
		switch probe.Type {
		case "session_meta", codexTypeResponseItem, "event_msg", "turn_context":
			return true
		default:
			return false
		}
	}
	if scanner.Err() != nil {
		return false
	}
	return false
}

// codexLine is the raw parsed form of one Codex JSONL line.
type codexLine struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

// codexPayload captures the common fields across Codex payload types.
type codexPayload struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	Phase     string          `json:"phase"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Input     string          `json:"input"`
	CallID    string          `json:"call_id"`
	Output    string          `json:"output"`
}

// compactCodex converts a Codex JSONL transcript into the compact format.
func compactCodex(content []byte, opts MetadataFields) ([]byte, error) {
	if opts.StartLine > 0 {
		content = transcript.SliceFromLine(content, opts.StartLine)
		if content == nil {
			return []byte{}, nil
		}
	}

	lines, err := parseCodexLines(content)
	if err != nil {
		return nil, err
	}

	base := newTranscriptLine(opts)
	var result []byte
	var pendingInTok, pendingOutTok int

	for i := 0; i < len(lines); i++ {
		cl := lines[i]

		// Consume token_count lines at the top level (e.g. before any assistant).
		if isCodexTokenCountLine(cl) {
			pendingInTok, pendingOutTok = codexTokenCount(cl.Payload)
			continue
		}

		var p codexPayload
		if json.Unmarshal(cl.Payload, &p) != nil {
			continue
		}

		ts, err := json.Marshal(cl.Timestamp)
		if err != nil {
			continue
		}

		switch {
		case p.Type == transcriptTypeMessage && p.Role == "user":
			text := codexUserText(p.Content)
			if text == "" {
				continue
			}
			contentJSON, err := json.Marshal([]userTextBlock{{Text: text}})
			if err != nil {
				continue
			}
			line := base
			line.Type = transcript.TypeUser
			line.TS = ts
			line.Content = contentJSON
			appendLine(&result, line)

		case p.Type == transcriptTypeMessage && p.Role == "assistant":
			text := codexAssistantText(p.Content)
			if text == "" {
				continue
			}

			// Collect any tool calls that follow this assistant message.
			var toolBlocks []map[string]json.RawMessage
			inTok, outTok := pendingInTok, pendingOutTok
			pendingInTok, pendingOutTok = 0, 0
			for i+1 < len(lines) {
				next := lines[i+1]
				if isCodexTokenCountLine(next) {
					inTok, outTok = codexTokenCount(next.Payload)
					i++
					continue
				}
				var np codexPayload
				if json.Unmarshal(next.Payload, &np) != nil {
					break
				}
				if np.Type == codexTypeFunctionCall || np.Type == codexTypeCustomToolCall {
					i++ // consume the tool call line
					tb := codexConsumeToolCall(np, lines, &i, &inTok, &outTok)
					toolBlocks = append(toolBlocks, tb)
					continue
				}
				// Orphan output without a preceding call — skip.
				if np.Type == codexTypeFunctionCallOutput || np.Type == codexTypeCustomToolCallOutput {
					i++
					continue
				}
				break
			}

			contentArr := codexBuildContent(text, toolBlocks)
			line := base
			line.Type = transcript.TypeAssistant
			line.TS = ts
			line.InputTokens = inTok
			line.OutputTokens = outTok
			line.Content = contentArr
			appendLine(&result, line)

		case p.Type == codexTypeFunctionCall || p.Type == codexTypeCustomToolCall:
			// Standalone tool call not preceded by assistant text.
			inTok, outTok := pendingInTok, pendingOutTok
			pendingInTok, pendingOutTok = 0, 0
			tb := codexConsumeToolCall(p, lines, &i, &inTok, &outTok)
			// Also consume any trailing token_count.
			for i+1 < len(lines) && isCodexTokenCountLine(lines[i+1]) {
				inTok, outTok = codexTokenCount(lines[i+1].Payload)
				i++
			}
			contentArr, err := json.Marshal([]map[string]json.RawMessage{tb})
			if err != nil {
				continue
			}
			line := base
			line.Type = transcript.TypeAssistant
			line.TS = ts
			line.InputTokens = inTok
			line.OutputTokens = outTok
			line.Content = contentArr
			appendLine(&result, line)
		}
	}

	return result, nil
}

// parseCodexLines reads all JSONL lines, keeping response_item and
// token_count event_msg entries.
func parseCodexLines(content []byte) ([]codexLine, error) {
	reader := bufio.NewReader(bytes.NewReader(content))
	var lines []codexLine

	for {
		lineBytes, err := reader.ReadBytes('\n')
		if err != nil && err != io.EOF {
			return nil, fmt.Errorf("reading codex line: %w", err)
		}

		if len(bytes.TrimSpace(lineBytes)) > 0 {
			var cl codexLine
			if json.Unmarshal(lineBytes, &cl) == nil {
				if cl.Type == "response_item" || isCodexTokenCountLine(cl) {
					lines = append(lines, cl)
				}
			}
		}

		if err == io.EOF {
			break
		}
	}
	return lines, nil
}

// isCodexTokenCountLine checks if a codexLine is an event_msg with token_count payload.
func isCodexTokenCountLine(cl codexLine) bool {
	if cl.Type != "event_msg" {
		return false
	}
	var p struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(cl.Payload, &p) == nil && p.Type == "token_count"
}

// codexTokenCount extracts input/output tokens from a token_count payload.
func codexTokenCount(payload json.RawMessage) (input, output int) {
	var tc struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	}
	if json.Unmarshal(payload, &tc) == nil {
		return tc.InputTokens, tc.OutputTokens
	}
	return 0, 0
}

// codexUserText extracts the actual user prompt text from a Codex user message,
// dropping system-injected content (AGENTS.md, environment_context, permissions,
// turn_aborted, etc.).
func codexUserText(raw json.RawMessage) string {
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}

	var texts []string
	for _, b := range blocks {
		if b.Type != "input_text" {
			continue
		}
		// Skip system-injected content.
		if isCodexSystemContent(b.Text) {
			continue
		}
		stripped := textutil.StripIDEContextTags(b.Text)
		if stripped != "" {
			texts = append(texts, stripped)
		}
	}

	return strings.Join(texts, "\n\n")
}

// isCodexSystemContent returns true for content blocks that are system-injected
// rather than user-authored.
func isCodexSystemContent(text string) bool {
	prefixes := []string{
		"<permissions",
		"<collaboration_mode>",
		"<skills_instructions>",
		"<environment_context>",
		"<turn_aborted>",
		"# AGENTS.md",
	}
	for _, p := range prefixes {
		if len(text) >= len(p) && text[:len(p)] == p {
			return true
		}
	}
	return false
}

// codexAssistantText extracts text from a Codex assistant message content array.
func codexAssistantText(raw json.RawMessage) string {
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var texts []string
	for _, b := range blocks {
		if b.Type == "output_text" && b.Text != "" {
			texts = append(texts, b.Text)
		}
	}
	return strings.Join(texts, "\n\n")
}

// codexToolUseBlock builds a compact tool_use content block from a function_call.
func codexToolUseBlock(p codexPayload) map[string]json.RawMessage {
	block := map[string]json.RawMessage{
		"type": mustJSON(transcript.ContentTypeToolUse),
		"name": mustJSON(p.Name),
	}
	if p.CallID != "" {
		block["id"] = mustJSON(p.CallID)
	}

	// Parse the arguments JSON string into a raw object for the "input" field.
	var args json.RawMessage
	if json.Unmarshal([]byte(p.Arguments), &args) == nil {
		block["input"] = args
	}

	return block
}

// codexConsumeToolCall builds a tool_use block from a function_call or custom_tool_call,
// consuming any trailing token_count lines and the matching output line.
// The caller must advance i past the tool call line itself before calling this function,
// and must verify p.Type is a tool call type.
func codexConsumeToolCall(p codexPayload, lines []codexLine, i *int, inTok, outTok *int) map[string]json.RawMessage {
	var tb map[string]json.RawMessage
	var outputType string

	switch p.Type {
	case codexTypeFunctionCall:
		tb = codexToolUseBlock(p)
		outputType = codexTypeFunctionCallOutput
	case codexTypeCustomToolCall:
		tb = codexCustomToolUseBlock(p)
		outputType = codexTypeCustomToolCallOutput
	default:
		return nil
	}

	for *i+1 < len(lines) && isCodexTokenCountLine(lines[*i+1]) {
		*inTok, *outTok = codexTokenCount(lines[*i+1].Payload)
		*i++
	}

	if *i+1 < len(lines) {
		callID, typ := codexCallIDAndType(lines[*i+1].Payload)
		if typ == outputType && callID == p.CallID {
			output := codexToolOutputText(lines[*i+1].Payload, outputType)
			tb["result"] = buildToolResult(toolResultEntry{output: output})
			*i++
		}
	}

	return tb
}

// codexToolOutputText extracts the output text from a tool call output payload.
// For function_call_output, the output field is a plain string.
// For custom_tool_call_output, it is an object {"type":"text","text":"..."}.
func codexToolOutputText(payload json.RawMessage, outputType string) string {
	if outputType == codexTypeCustomToolCallOutput {
		return codexCustomOutputText(payload)
	}
	var p codexPayload
	if json.Unmarshal(payload, &p) == nil {
		return p.Output
	}
	return ""
}

// codexCallIDAndType extracts just the type and call_id from a payload without
// failing on fields with unexpected types (e.g., custom_tool_call_output has an
// object "output" field that can't unmarshal into codexPayload.Output string).
func codexCallIDAndType(payload json.RawMessage) (callID, typ string) {
	var p struct {
		Type   string `json:"type"`
		CallID string `json:"call_id"`
	}
	if json.Unmarshal(payload, &p) == nil {
		return p.CallID, p.Type
	}
	return "", ""
}

// codexCustomToolUseBlock builds a tool_use block from a custom_tool_call payload.
// Unlike function_call, the input is plain text (e.g., apply_patch content) rather
// than a JSON arguments string.
func codexCustomToolUseBlock(p codexPayload) map[string]json.RawMessage {
	block := map[string]json.RawMessage{
		"type": mustJSON(transcript.ContentTypeToolUse),
		"name": mustJSON(p.Name),
	}
	if p.CallID != "" {
		block["id"] = mustJSON(p.CallID)
	}
	if p.Input != "" {
		if inputJSON, err := json.Marshal(map[string]string{"input": p.Input}); err == nil {
			block["input"] = inputJSON
		}
	}
	return block
}

// codexCustomOutputText extracts the output text from a custom_tool_call_output payload.
// The output field is an object {"type":"text","text":"..."} rather than a plain string.
func codexCustomOutputText(payload json.RawMessage) string {
	var p struct {
		Output json.RawMessage `json:"output"`
	}
	if json.Unmarshal(payload, &p) != nil || p.Output == nil {
		return ""
	}
	// Try as plain string first (for forward compatibility).
	var s string
	if json.Unmarshal(p.Output, &s) == nil {
		return s
	}
	// Try as object with "text" field.
	var obj struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(p.Output, &obj) == nil {
		return obj.Text
	}
	return ""
}

// mustJSON marshals v to JSON, panicking on error (only used for simple types).
func mustJSON(v interface{}) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("compact: mustJSON: %v", err))
	}
	return b
}

// codexBuildContent builds the compact content array from assistant text and
// optional tool_use blocks.
func codexBuildContent(text string, toolBlocks []map[string]json.RawMessage) json.RawMessage {
	var content []map[string]json.RawMessage

	if text != "" {
		content = append(content, map[string]json.RawMessage{
			"type": mustJSON(transcript.ContentTypeText),
			"text": mustJSON(text),
		})
	}
	content = append(content, toolBlocks...)

	b, err := json.Marshal(content)
	if err != nil {
		return nil
	}
	return b
}
