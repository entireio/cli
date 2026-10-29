package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
)

const (
	transcriptTypeMessage         = "message"
	codexTypeResponseItem         = "response_item"
	codexTypeEventMsg             = "event_msg"
	codexTypeFunctionCall         = "function_call"
	codexTypeFunctionCallOutput   = "function_call_output"
	codexTypeCustomToolCall       = "custom_tool_call"
	codexTypeCustomToolCallOutput = "custom_tool_call_output"
	codexTypeInputText            = "input_text"
	codexTypeInputImage           = "input_image"
	codexTypeReasoning            = "reasoning"
	codexTypeTurnContext          = "turn_context"
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
		case "session_meta", codexTypeResponseItem, codexTypeEventMsg, codexTypeTurnContext:
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
	Timestamp json.RawMessage `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

// ts returns the timestamp string, or "" when absent or not a string.
func (cl codexLine) ts() string {
	var s string
	if json.Unmarshal(cl.Timestamp, &s) != nil {
		return ""
	}
	return s
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

// codexCall accumulates the items of one model call. Its usage arrives with
// the token_count event that closes it.
type codexCall struct {
	ts    string
	text  string
	tools []map[string]json.RawMessage
	usage *CallUsage
}

// codexEntry is one output line: a user line, or an assistant call rendered
// once the whole transcript has been read so late tool outputs can land.
type codexEntry struct {
	user transcriptLine
	call *codexCall
}

type codexToolSlot struct {
	call       *codexCall
	block      map[string]json.RawMessage
	outputType string
}

type codexConverter struct {
	base    transcriptLine
	tracker *codexUsageTracker
	// snapshots is true when the transcript has a usable total_token_usage
	// snapshot. Calls then end only at a snapshot or user line; otherwise a
	// tool output also ends its call.
	snapshots bool
	model     string
	entries   []codexEntry
	open      *codexCall
	tools     map[string]codexToolSlot
	// subagents maps spawn_agent call IDs to the launched subagent's thread.
	subagents map[string]string
}

// compactCodex converts a Codex JSONL transcript into the compact format.
// Each assistant line is one model call: the items since the previous call
// closed, with the usage of the token_count event that closed it.
func compactCodex(content []byte, opts Options) ([]byte, error) {
	full := content
	if opts.StartLine > 0 {
		content = SliceFromLine(full, opts.StartLine)
		if content == nil {
			return []byte{}, nil
		}
	}

	lines, err := parseCodexLines(content)
	if err != nil {
		return nil, err
	}
	var prefix []codexLine
	if opts.StartLine > 0 {
		if prefix, err = parseCodexLines(full[:len(full)-len(content)]); err != nil {
			return nil, err
		}
	}

	c := &codexConverter{
		base:      newTranscriptLine(opts),
		tracker:   newCodexUsageTracker(),
		snapshots: codexHasSnapshots(prefix) || codexHasSnapshots(lines),
		tools:     map[string]codexToolSlot{},
		subagents: codexSubagentThreads(full),
	}
	// Counters are cumulative for the whole session, so the skipped prefix
	// seeds the usage baseline. The last line before StartLine is part of the
	// slice when the slice adds its outputs or closing snapshot; reaching
	// further back would repeat earlier calls and their usage.
	for _, cl := range prefix {
		c.consume(cl)
	}
	before := c.renderEntries()
	start := len(before)
	for _, cl := range lines {
		c.consume(cl)
	}
	after := c.renderEntries()
	if start > 0 && !bytes.Equal(before[start-1], after[start-1]) {
		start--
	}
	return bytes.Join(after[start:], nil), nil
}

func codexHasSnapshots(lines []codexLine) bool {
	for _, cl := range lines {
		if !isCodexTokenCountLine(cl) {
			continue
		}
		if _, ok := codexTokenCountTotal(cl.Payload); ok {
			return true
		}
	}
	return false
}

func (c *codexConverter) consume(cl codexLine) {
	switch {
	case cl.Type == codexTypeTurnContext:
		c.noteModel(cl.Payload)
	case isCodexTokenCountLine(cl):
		c.tokenCount(cl)
	default:
		c.responseItem(cl)
	}
}

func (c *codexConverter) noteModel(payload json.RawMessage) {
	var tc struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(payload, &tc) == nil && tc.Model != "" {
		c.model = tc.Model
	}
}

// openCall returns the call being accumulated, starting one if needed, and
// moves its timestamp to ts unless ts is empty.
func (c *codexConverter) openCall(ts string) *codexCall {
	if c.open == nil {
		c.open = &codexCall{}
		c.entries = append(c.entries, codexEntry{call: c.open})
	}
	if ts != "" {
		c.open.ts = ts
	}
	return c.open
}

// tokenCount attaches the usage delta of a new snapshot to the open call, else
// to a new empty call, and closes it. Repeated snapshots change nothing; once a
// counter decrease has halted usage, every snapshot closes the call without usage.
func (c *codexConverter) tokenCount(cl codexLine) {
	total, ok := codexTokenCountTotal(cl.Payload)
	if !ok {
		return
	}
	d, ok := c.tracker.next(total)
	switch {
	case ok:
		d.Model = c.model
		c.openCall(cl.ts()).usage = &d
	case c.tracker.stopped():
		if c.open != nil {
			c.openCall(cl.ts())
		}
	default:
		return
	}
	c.open = nil
}

func (c *codexConverter) responseItem(cl codexLine) {
	callID, typ := codexCallIDAndType(cl.Payload)
	switch typ {
	case codexTypeFunctionCallOutput, codexTypeCustomToolCallOutput:
		if slot, ok := c.tools[callID]; ok && slot.outputType == typ {
			slot.block["result"] = buildToolResult(toolResultEntry{
				output:  codexToolOutputText(cl.Payload),
				images:  codexToolOutputImages(cl.Payload),
				agentID: c.subagents[callID],
			})
			if !c.snapshots && c.open == slot.call {
				c.open = nil
			}
		}
		return
	case codexTypeReasoning:
		c.openCall(cl.ts())
		return
	}

	var p codexPayload
	if json.Unmarshal(cl.Payload, &p) != nil {
		return
	}
	switch {
	case p.Type == transcriptTypeMessage && p.Role == TypeUser:
		text, images := codexUserContent(p.Content)
		c.user(cl.ts(), text, images)
	case p.Type == transcriptTypeMessage && p.Role == TypeAssistant:
		text := codexAssistantText(p.Content)
		if text == "" {
			return
		}
		call := c.openCall(cl.ts())
		if call.text != "" {
			call.text += "\n\n"
		}
		call.text += text
	case p.Type == codexTypeFunctionCall:
		c.toolCall(cl.ts(), p, codexToolUseBlock(p), codexTypeFunctionCallOutput)
	case p.Type == codexTypeCustomToolCall:
		c.toolCall(cl.ts(), p, codexCustomToolUseBlock(p), codexTypeCustomToolCallOutput)
	}
}

func codexTSJSON(ts string) json.RawMessage {
	if ts == "" {
		return nil
	}
	return mustJSON(ts)
}

func (c *codexConverter) toolCall(ts string, p codexPayload, block map[string]json.RawMessage, outputType string) {
	call := c.openCall(ts)
	call.tools = append(call.tools, block)
	if p.CallID != "" {
		c.tools[p.CallID] = codexToolSlot{call: call, block: block, outputType: outputType}
	}
}

func (c *codexConverter) user(ts, text string, images []json.RawMessage) {
	if text == "" && len(images) == 0 {
		return
	}
	contentJSON, err := userContentBlocks("", text, images)
	if err != nil {
		return
	}
	line := c.base
	line.Type = TypeUser
	line.TS = codexTSJSON(ts)
	line.Content = contentJSON
	c.open = nil
	c.entries = append(c.entries, codexEntry{user: line})
}

// renderEntries renders each entry to its output line, or to nothing for an
// empty call, so callers can compare entries before and after more input.
func (c *codexConverter) renderEntries() [][]byte {
	out := make([][]byte, len(c.entries))
	for i, e := range c.entries {
		if e.call == nil {
			appendLine(&out[i], e.user)
			continue
		}
		call := e.call
		if call.text == "" && len(call.tools) == 0 && call.usage == nil {
			continue
		}
		line := c.base
		line.Type = TypeAssistant
		line.TS = codexTSJSON(call.ts)
		line.Content = codexBuildContent(call.text, call.tools)
		if u := call.usage; u != nil {
			line.Model = u.Model
			line.InputTokens = u.InputTokens
			line.CacheReadTokens = u.CacheReadTokens
			line.OutputTokens = u.OutputTokens
			line.ReasoningTokens = u.ReasoningTokens
		}
		appendLine(&out[i], line)
	}
	return out
}

// parseCodexLines reads all JSONL lines, keeping response_item,
// turn_context and token_count event_msg entries.
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
				if cl.Type == codexTypeResponseItem || cl.Type == codexTypeTurnContext || isCodexTokenCountLine(cl) {
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

// codexSubagentThreads maps each spawn_agent call ID to the thread of the
// subagent it started, from item_completed SubAgentActivity events.
func codexSubagentThreads(content []byte) map[string]string {
	if !bytes.Contains(content, []byte(`"SubAgentActivity"`)) {
		return nil
	}
	threads := make(map[string]string)
	reader := bufio.NewReader(bytes.NewReader(content))
	for {
		lineBytes, err := reader.ReadBytes('\n')
		if bytes.Contains(lineBytes, []byte(`"SubAgentActivity"`)) {
			var line struct {
				Type    string `json:"type"`
				Payload struct {
					Type string `json:"type"`
					Item struct {
						Type          string `json:"type"`
						Kind          string `json:"kind"`
						ID            string `json:"id"`
						AgentThreadID string `json:"agent_thread_id"`
					} `json:"item"`
				} `json:"payload"`
			}
			if json.Unmarshal(lineBytes, &line) == nil && line.Type == codexTypeEventMsg &&
				line.Payload.Type == "item_completed" && line.Payload.Item.Type == "SubAgentActivity" &&
				line.Payload.Item.Kind == "started" && line.Payload.Item.ID != "" && line.Payload.Item.AgentThreadID != "" {
				threads[line.Payload.Item.ID] = line.Payload.Item.AgentThreadID
			}
		}
		if err != nil {
			break
		}
	}
	return threads
}

// isCodexTokenCountLine checks if a codexLine is an event_msg with token_count payload.
func isCodexTokenCountLine(cl codexLine) bool {
	if cl.Type != codexTypeEventMsg {
		return false
	}
	var p struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(cl.Payload, &p) == nil && p.Type == "token_count"
}

// codexImageTagRe matches the text items Codex wraps around a pasted image:
// `<image name=[Image #1]>`, optionally with a path attribute, and `</image>`.
var codexImageTagRe = regexp.MustCompile(`^(?:<image name=\[Image #\d+\](?: path="[^"\n]*")?>|</image>)$`)

// codexUserContent extracts the actual user prompt text from a Codex user
// message, dropping system-injected content (AGENTS.md, environment_context,
// permissions, turn_aborted, etc.) and the tags around pasted images. It also
// returns the message's input_image items, verbatim and in order.
func codexUserContent(raw json.RawMessage) (string, []json.RawMessage) {
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return "", nil
	}

	var texts []string
	var images []json.RawMessage
	for _, rawItem := range items {
		var b struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(rawItem, &b) != nil {
			continue
		}
		switch b.Type {
		case codexTypeInputImage:
			images = append(images, rawItem)
			continue
		case codexTypeInputText:
		default:
			continue
		}
		// Skip system-injected content and image tags.
		if isCodexSystemContent(b.Text) || codexImageTagRe.MatchString(b.Text) {
			continue
		}
		stripped := StripIDEContextTags(b.Text)
		if stripped != "" {
			texts = append(texts, stripped)
		}
	}

	return strings.Join(texts, "\n\n"), images
}

// isCodexSystemContent returns true for content blocks that are system-injected
// rather than user-authored. The prefix list is shared with the session-title and
// turn-counting paths (IsInjectedPrompt) so the two cannot drift.
func isCodexSystemContent(text string) bool {
	return IsInjectedPrompt(text)
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
		"type": mustJSON(ContentTypeToolUse),
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

// codexToolOutputText extracts the output text from a tool call output payload.
// Either output type holds a plain string, an object {"type":"text","text":"..."},
// or an array of content items whose input_text texts are joined, a newline
// separating two texts only where the first does not already end in one.
func codexToolOutputText(payload json.RawMessage) string {
	var p struct {
		Output json.RawMessage `json:"output"`
	}
	if json.Unmarshal(payload, &p) != nil || p.Output == nil {
		return ""
	}
	var s string
	if json.Unmarshal(p.Output, &s) == nil {
		return s
	}
	var items []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(p.Output, &items) == nil {
		var b strings.Builder
		for _, item := range items {
			if item.Type != codexTypeInputText || item.Text == "" {
				continue
			}
			if b.Len() > 0 && !strings.HasSuffix(b.String(), "\n") {
				b.WriteByte('\n')
			}
			b.WriteString(item.Text)
		}
		return b.String()
	}
	var obj struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(p.Output, &obj) == nil {
		return obj.Text
	}
	return ""
}

// codexToolOutputImages returns the input_image items of a tool call output
// whose "output" is a content-item array, verbatim and in order. Codex writes
// an image a tool returned (for example view_image) as
// {"type":"input_image","image_url":"data:image/…;base64,…"}.
func codexToolOutputImages(payload json.RawMessage) []json.RawMessage {
	var p struct {
		Output json.RawMessage `json:"output"`
	}
	if json.Unmarshal(payload, &p) != nil {
		return nil
	}
	return itemsOfType(p.Output, codexTypeInputImage)
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
		"type": mustJSON(ContentTypeToolUse),
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
	content := []map[string]json.RawMessage{}

	if text != "" {
		content = append(content, map[string]json.RawMessage{
			"type": mustJSON(ContentTypeText),
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
