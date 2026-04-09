// Package compact converts full.jsonl transcripts into a normalized,
// compact transcript.jsonl format. Only shared formatting is contained here.
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

// MetadataFields provides metadata fields written to every output line.
type MetadataFields struct {
	Agent      string // e.g. "claude-code"
	CLIVersion string // e.g. "0.42.0"
	StartLine  int    // checkpoint_transcript_start (0 = no truncation)
}

// transcriptLine is the uniform output format for every line in transcript.jsonl.
// Field order is guaranteed by encoding/json (struct declaration order).
type transcriptLine struct {
	V            int             `json:"v"`
	Agent        string          `json:"agent"`
	CLIVersion   string          `json:"cli_version"`
	Type         string          `json:"type"`
	TS           json.RawMessage `json:"ts,omitempty"`
	ID           string          `json:"id,omitempty"`
	InputTokens  int             `json:"input_tokens,omitempty"`
	OutputTokens int             `json:"output_tokens,omitempty"`
	Content      json.RawMessage `json:"content"`
}

// newTranscriptLine returns a transcriptLine pre-filled with the shared
// metadata fields that are identical on every output line.
func newTranscriptLine(opts MetadataFields) transcriptLine {
	return transcriptLine{
		V:          1,
		Agent:      opts.Agent,
		CLIVersion: opts.CLIVersion,
	}
}

const toolResultStatusError = "error"

// toolResultJSON is the compact result object inlined into tool_use blocks.
type toolResultJSON struct {
	Output     string              `json:"output"`
	Status     string              `json:"status"`
	File       *toolResultFileJSON `json:"file,omitempty"`
	MatchCount int                 `json:"matchCount,omitempty"`
}

// toolResultFileJSON carries structured file metadata from Read/Edit tool results.
type toolResultFileJSON struct {
	FilePath string `json:"filePath"`
	NumLines int    `json:"numLines,omitempty"`
}

// userTextBlock is a text block within user message content.
type userTextBlock struct {
	ID   string `json:"id,omitempty"`
	Text string `json:"text"`
}

// Compact converts a full.jsonl transcript into the condensed transcript.jsonl format.
//
// The output format puts version, agent, and cli_version on every line,
// merges streaming assistant fragments with the same message ID, and inlines
// tool results into the preceding assistant's tool_use blocks:
//
//	{"v":1,"agent":"claude-code","cli_version":"0.42.0","type":"user","ts":"...","content":"..."}
//	{"v":1,"agent":"claude-code","cli_version":"0.42.0","type":"assistant","ts":"...","id":"msg_xxx","content":[{"type":"text","text":"..."},{"type":"tool_use","id":"...","name":"...","input":{...},"result":{"output":"...","status":"..."}}]}
func Compact(content []byte, opts MetadataFields) ([]byte, error) {
	// Formats that need detection on raw content before line truncation:
	// - Single-object formats (OpenCode, Gemini): SliceFromLine would cut
	//   a JSON object mid-value. They handle StartLine as a message-index offset.
	// - Codex: session_meta header is only on the first line. Codex handles
	//   StartLine as a response_item index offset.
	if isOpenCodeFormat(content) {
		return compactOpenCode(content, opts)
	}

	if isGeminiFormat(content) {
		return compactGemini(content, opts)
	}

	if isCodexFormat(content) {
		return compactCodex(content, opts)
	}

	truncated := transcript.SliceFromLine(content, opts.StartLine)
	if truncated == nil {
		truncated = []byte{}
	}

	if isCopilotFormat(truncated) {
		return compactCopilot(truncated, opts)
	}

	if isDroidFormat(truncated) {
		return compactDroid(truncated, opts)
	}

	return compactJSONL(truncated, opts)
}

// droppedTypes are JSONL entry types that carry no parser-relevant data.
var droppedTypes = map[string]bool{
	"progress":              true,
	"file-history-snapshot": true,
	"queue-operation":       true,
	"system":                true,
}

// userAliases maps JSONL type/role values to the canonical "user" kind.
// Covers Claude Code ("user", "human") and Cursor ("user" via "role" field).
var userAliases = map[string]bool{
	transcript.TypeUser: true,
	"human":             true,
}

// normalizeKind returns the canonical entry kind ("user" or "assistant") for a
// JSONL transcript line. It checks the "type" field, then falls back to "role".
// Returns "" for unrecognised or dropped entries.
func normalizeKind(raw map[string]json.RawMessage) string {
	kind := unquote(raw["type"])
	if kind == "" {
		kind = unquote(raw["role"])
	}

	if droppedTypes[kind] {
		return ""
	}
	if userAliases[kind] {
		return transcript.TypeUser
	}
	if kind == transcript.TypeAssistant {
		return transcript.TypeAssistant
	}
	return ""
}

// linePreprocessor transforms a parsed JSONL line before conversion.
type linePreprocessor func(map[string]json.RawMessage) map[string]json.RawMessage

// parsedEntry is an intermediate representation of a JSONL line used during
// the two-pass compact conversion.
type parsedEntry struct {
	kind         string // "user" or "assistant"
	ts           json.RawMessage
	id           string            // message ID (assistant only)
	userID       string            // prompt ID (user only, e.g. Claude's promptId)
	inputTokens  int               // API input tokens (assistant only)
	outputTokens int               // API output tokens (assistant only)
	content      json.RawMessage   // stripped assistant content array, or nil
	userText     string            // extracted user text
	userImages   []json.RawMessage // image blocks from user messages
	toolResults  []toolResultEntry // user tool_result entries
}

// compactJSONL converts JSONL transcripts (Claude Code, Cursor) into the
// transcript.jsonl format.
func compactJSONL(content []byte, opts MetadataFields) ([]byte, error) {
	return compactJSONLWith(content, opts, nil)
}

func compactJSONLWith(content []byte, opts MetadataFields, preprocess linePreprocessor) ([]byte, error) {
	base := newTranscriptLine(opts)

	// Pass 1: parse all lines into intermediate entries.
	entries, err := parseJSONLEntries(content, preprocess)
	if err != nil {
		return nil, err
	}

	// Pass 2: merge and emit.
	var result []byte
	for i := 0; i < len(entries); i++ {
		e := entries[i]

		switch e.kind {
		case transcript.TypeAssistant:
			// Merge consecutive assistant entries with the same message ID.
			merged := e
			for i+1 < len(entries) && entries[i+1].kind == transcript.TypeAssistant && entries[i+1].id == e.id {
				i++
				merged = mergeAssistantEntries(merged, entries[i])
			}

			// Look ahead for user tool_result entries to inline.
			if i+1 < len(entries) && entries[i+1].kind == transcript.TypeUser && hasToolResults(entries[i+1]) {
				userEntry := entries[i+1]
				merged = inlineToolResults(merged, userEntry)
				i++ // consume the user tool_result entry

				// If the consumed user entry also had text or image content, emit it
				// as a separate user line after the assistant.
				if userEntry.userText != "" || len(userEntry.userImages) > 0 {
					emitAssistant(&result, base, merged)
					emitUser(&result, base, userEntry)
					continue
				}
			}

			if isEmptyContentArray(merged.content) {
				continue
			}

			emitAssistant(&result, base, merged)

		case transcript.TypeUser:
			// User entries that are purely tool results were already consumed
			// by the assistant look-ahead above. If we reach one here it was
			// not preceded by an assistant with a matching tool_use, so emit
			// it only if it has text or image content.
			if hasToolResults(e) && e.userText == "" && len(e.userImages) == 0 {
				continue
			}
			emitUser(&result, base, e)
		}
	}

	return result, nil
}

func emitAssistant(result *[]byte, base transcriptLine, e parsedEntry) {
	line := base
	line.Type = transcript.TypeAssistant
	line.TS = e.ts
	line.ID = e.id
	line.InputTokens = e.inputTokens
	line.OutputTokens = e.outputTokens
	line.Content = e.content
	appendLine(result, line)
}

func emitUser(result *[]byte, base transcriptLine, e parsedEntry) {
	var blocks []json.RawMessage

	// Text block (with optional prompt ID).
	if e.userText != "" || len(e.userImages) == 0 {
		b, err := json.Marshal(userTextBlock{ID: e.userID, Text: e.userText})
		if err != nil {
			return
		}
		blocks = append(blocks, b)
	}

	// Image blocks passed through verbatim.
	blocks = append(blocks, e.userImages...)

	contentJSON, err := json.Marshal(blocks)
	if err != nil {
		return
	}

	line := base
	line.Type = transcript.TypeUser
	line.TS = e.ts
	line.Content = contentJSON
	appendLine(result, line)
}

// appendLine marshals a transcriptLine and appends it (with newline) to result.
func appendLine(result *[]byte, line transcriptLine) {
	b, err := json.Marshal(line)
	if err != nil {
		return
	}
	*result = append(*result, b...)
	*result = append(*result, '\n')
}

// parseJSONLEntries parses all JSONL lines into intermediate entries,
// filtering dropped types and malformed lines.
func parseJSONLEntries(content []byte, preprocess linePreprocessor) ([]parsedEntry, error) {
	reader := bufio.NewReader(bytes.NewReader(content))
	var entries []parsedEntry

	for {
		lineBytes, err := reader.ReadBytes('\n')
		if err != nil && err != io.EOF {
			return nil, fmt.Errorf("reading JSONL line: %w", err)
		}

		if len(bytes.TrimSpace(lineBytes)) > 0 {
			if e, ok := parseLine(lineBytes, preprocess); ok {
				entries = append(entries, e)
			}
		}

		if err == io.EOF {
			break
		}
	}

	return entries, nil
}

// parseLine converts a single JSONL line into a parsedEntry.
// Returns ok=false for dropped/malformed lines.
func parseLine(lineBytes []byte, preprocess linePreprocessor) (parsedEntry, bool) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(lineBytes, &raw); err != nil {
		return parsedEntry{}, false
	}

	if preprocess != nil {
		raw = preprocess(raw)
	}

	kind := normalizeKind(raw)
	if kind == "" {
		return parsedEntry{}, false
	}

	e := parsedEntry{
		kind: kind,
		ts:   raw["timestamp"],
	}

	msg := parseMessage(raw)

	switch kind {
	case transcript.TypeAssistant:
		if msg != nil {
			e.id = unquote(msg["id"])
			if contentRaw, ok := msg["content"]; ok {
				e.content = stripAssistantContent(contentRaw)
			}
			e.inputTokens, e.outputTokens = extractUsageTokens(msg)
		}

	case transcript.TypeUser:
		e.userID = unquote(raw["promptId"])
		if msg != nil {
			if contentRaw, ok := msg["content"]; ok {
				uc := extractUserContent(contentRaw)
				e.userText = uc.text
				e.userImages = uc.images
				e.toolResults = uc.toolResults
			}
		}
		// Enrich tool results with metadata from toolUseResult.
		if turRaw, ok := raw["toolUseResult"]; ok {
			var tur map[string]json.RawMessage
			if json.Unmarshal(turRaw, &tur) == nil {
				e.toolResults = enrichToolResults(e.toolResults, tur)
			}
		}
	}

	return e, true
}

// mergeAssistantEntries combines two assistant entries with the same message ID.
// Content arrays are concatenated; the later timestamp and token counts win
// (streaming fragments report cumulative usage, so the last fragment is final).
func mergeAssistantEntries(a, b parsedEntry) parsedEntry {
	merged := a
	merged.ts = b.ts
	if b.inputTokens > 0 {
		merged.inputTokens = b.inputTokens
	}
	if b.outputTokens > 0 {
		merged.outputTokens = b.outputTokens
	}

	var aBlocks, bBlocks []json.RawMessage
	_ = json.Unmarshal(a.content, &aBlocks) //nolint:errcheck // best-effort merge
	_ = json.Unmarshal(b.content, &bBlocks) //nolint:errcheck // best-effort merge
	all := append(aBlocks, bBlocks...)      //nolint:gocritic // intentional append to new slice
	if data, err := json.Marshal(all); err == nil {
		merged.content = data
	}

	return merged
}

// inlineToolResults adds "result" fields to matching tool_use blocks in the
// assistant entry's content, using outputs from user tool_result entries.
func inlineToolResults(assistant, user parsedEntry) parsedEntry {
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(assistant.content, &blocks) != nil || len(blocks) == 0 {
		return assistant
	}

	for _, tr := range user.toolResults {
		// Find the tool_use block matching this tool_use_id.
		idx := -1
		for i := len(blocks) - 1; i >= 0; i-- {
			if unquote(blocks[i]["type"]) == transcript.ContentTypeToolUse {
				if tr.toolUseID == "" || unquote(blocks[i]["id"]) == tr.toolUseID {
					idx = i
					break
				}
			}
		}
		// No matching tool_use block: do not attach a result to unrelated content.
		if idx == -1 {
			continue
		}

		blocks[idx]["result"] = buildToolResult(tr)
	}

	if data, err := json.Marshal(blocks); err == nil {
		assistant.content = data
	}

	return assistant
}

// buildToolResult constructs the compact result object for a tool_use block,
// including optional rich metadata (file, matchCount) when available.
func buildToolResult(tr toolResultEntry) json.RawMessage {
	r := toolResultJSON{
		Output:     tr.output,
		Status:     "success",
		MatchCount: tr.matchCount,
	}
	if tr.isError {
		r.Status = toolResultStatusError
	}
	if tr.file != nil {
		r.File = &toolResultFileJSON{
			FilePath: tr.file.filePath,
			NumLines: tr.file.numLines,
		}
	}
	b, err := json.Marshal(r)
	if err != nil {
		return nil
	}
	return b
}

// extractUsageTokens extracts input_tokens and output_tokens from a Claude
// message's "usage" object. Returns (0, 0) if usage is absent or malformed.
func extractUsageTokens(msg map[string]json.RawMessage) (inputTokens, outputTokens int) {
	usageRaw, ok := msg["usage"]
	if !ok {
		return 0, 0
	}
	var usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	}
	if json.Unmarshal(usageRaw, &usage) != nil {
		return 0, 0
	}
	return usage.InputTokens, usage.OutputTokens
}

// isEmptyContentArray returns true if raw is a JSON empty array (`[]`).
func isEmptyContentArray(raw json.RawMessage) bool {
	var arr []json.RawMessage
	return json.Unmarshal(raw, &arr) == nil && len(arr) == 0
}

func hasToolResults(e parsedEntry) bool {
	return len(e.toolResults) > 0
}

type toolResultEntry struct {
	toolUseID string
	output    string
	isError   bool
	// Rich metadata extracted from toolUseResult (optional).
	file       *toolResultFile // Read/Edit: file path and line count
	matchCount int             // Grep: number of matching files
}

// toolResultFile carries structured file metadata from Read/Edit tool results.
type toolResultFile struct {
	filePath string
	numLines int // 0 if not available (e.g. Edit results)
}

// enrichToolResults extracts structured metadata from a toolUseResult envelope
// and attaches it to the corresponding tool result entries.
//
// Claude Code's toolUseResult has different shapes per tool:
//   - Bash:  {stdout, stderr, interrupted, ...}
//   - Read:  {type:"text", file:{filePath, numLines, content, ...}}
//   - Grep:  {numFiles, numLines, filenames, content, mode}
//   - Edit:  {filePath, oldString, newString, structuredPatch, ...}
func enrichToolResults(results []toolResultEntry, tur map[string]json.RawMessage) []toolResultEntry {
	// Bash-style: stdout provides the output text.
	if stdout := unquote(tur["stdout"]); stdout != "" {
		switch len(results) {
		case 0:
			// Keep compatibility with transcripts that only include toolUseResult.
			results = append(results, toolResultEntry{output: stdout})
		case 1:
			results[0].output = stdout
		}
	}

	// Read-style: file object with filePath and numLines.
	if fileRaw, ok := tur["file"]; ok {
		var file struct {
			FilePath string `json:"filePath"`
			NumLines int    `json:"numLines"`
		}
		if json.Unmarshal(fileRaw, &file) == nil && file.FilePath != "" {
			applyToSingleResult(results, func(tr *toolResultEntry) {
				tr.file = &toolResultFile{filePath: file.FilePath, numLines: file.NumLines}
			})
		}
	}

	// Edit-style: top-level filePath.
	if filePath := unquote(tur["filePath"]); filePath != "" {
		applyToSingleResult(results, func(tr *toolResultEntry) {
			// Edit results don't have numLines.
			tr.file = &toolResultFile{filePath: filePath}
		})
	}

	// Grep-style: numFiles as match count.
	if numFilesRaw, ok := tur["numFiles"]; ok {
		var n int
		if json.Unmarshal(numFilesRaw, &n) == nil && n > 0 {
			applyToSingleResult(results, func(tr *toolResultEntry) {
				tr.matchCount = n
			})
		}
	}

	return results
}

// applyToSingleResult applies fn to the first (and expected only) tool result.
// toolUseResult is a single-tool envelope, so this is only meaningful when
// there's exactly one result entry.
func applyToSingleResult(results []toolResultEntry, fn func(*toolResultEntry)) {
	if len(results) == 1 {
		fn(&results[0])
	}
}

// parseMessage extracts and parses the "message" field from a JSONL transcript
// line. All JSONL agents nest content inside a "message" object.
func parseMessage(raw map[string]json.RawMessage) map[string]json.RawMessage {
	msgRaw, ok := raw["message"]
	if !ok {
		return nil
	}
	var msg map[string]json.RawMessage
	if json.Unmarshal(msgRaw, &msg) == nil {
		return msg
	}
	return nil
}

// userContent holds the extracted parts of a user message content array.
type userContent struct {
	text        string
	images      []json.RawMessage
	toolResults []toolResultEntry
}

// extractUserContent separates user message content into text, images, and tool_result entries.
// IDE context tags (e.g. <user_query>, <ide_opened_file>) are stripped from user text.
func extractUserContent(contentRaw json.RawMessage) userContent {
	var str string
	if json.Unmarshal(contentRaw, &str) == nil {
		return userContent{text: textutil.StripIDEContextTags(str)}
	}

	var blocks []json.RawMessage
	if json.Unmarshal(contentRaw, &blocks) != nil {
		return userContent{}
	}

	var uc userContent

	for _, blockRaw := range blocks {
		var block map[string]json.RawMessage
		if json.Unmarshal(blockRaw, &block) != nil {
			continue
		}
		blockType := unquote(block["type"])

		switch blockType {
		case "tool_result":
			var isErr bool
			if raw, ok := block["is_error"]; ok {
				_ = json.Unmarshal(raw, &isErr) //nolint:errcheck // best-effort
			}
			uc.toolResults = append(uc.toolResults, toolResultEntry{
				toolUseID: unquote(block["tool_use_id"]),
				output:    unquote(block["content"]),
				isError:   isErr,
			})

		case "image":
			uc.images = append(uc.images, blockRaw)

		case transcript.ContentTypeText:
			stripped := textutil.StripIDEContextTags(unquote(block[transcript.ContentTypeText]))
			if stripped != "" {
				uc.text += stripped + "\n\n"
			}
		}
	}

	uc.text = strings.TrimSpace(uc.text)
	return uc
}

func stripAssistantContent(contentRaw json.RawMessage) json.RawMessage {
	var str string
	if json.Unmarshal(contentRaw, &str) == nil {
		return contentRaw
	}

	var blocks []map[string]json.RawMessage
	if json.Unmarshal(contentRaw, &blocks) != nil {
		return contentRaw
	}

	result := make([]map[string]json.RawMessage, 0, len(blocks))
	for _, block := range blocks {
		blockType := unquote(block["type"])

		if blockType == "thinking" || blockType == "redacted_thinking" {
			continue
		}

		if blockType == transcript.ContentTypeToolUse {
			stripped := make(map[string]json.RawMessage)
			copyField(stripped, block, "type")
			copyField(stripped, block, "id")
			copyField(stripped, block, "name")
			copyField(stripped, block, "input")
			result = append(result, stripped)
			continue
		}

		result = append(result, block)
	}

	b, err := json.Marshal(result)
	if err != nil {
		return contentRaw
	}
	return b
}

// copyField copies a single key from src to dst if it exists.
func copyField(dst, src map[string]json.RawMessage, key string) {
	if v, ok := src[key]; ok {
		dst[key] = v
	}
}

// unquote JSON-decodes a raw message as a string. Returns "" on failure.
func unquote(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}
