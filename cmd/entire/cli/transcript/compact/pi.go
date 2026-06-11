package compact

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent/pi/pijsonl"
)

// --- pi format support ---
//
// Pi sessions are JSONL with a tree-shaped entry layout. Each entry has a
// top-level type ("session", "message", "model_change", "thinking_level_change",
// "compaction", "branch_summary", "label", "custom", "custom_message",
// "session_info") and a parentId pointer.
//
// Branching: when the user forks/branches mid-conversation the JSONL
// accumulates entries from BOTH branches. Compaction must walk only the
// active branch (root → most-recent message) so abandoned tool calls don't
// pollute the compact transcript.
//
// Parsing primitives (Entry/Message/ContentItem types, ResolveActiveBranch,
// SkipLines, NewScanner) are shared with the pi agent package via
// cmd/entire/cli/agent/pi/pijsonl so a fix applied here also lands there.

const (
	piToolResultStatusOK  = "success"
	piToolResultStatusErr = "error"
)

// piToolNameMap normalises Pi's lowercase tool names to the title-cased names
// used elsewhere in Entire's compact format (matching Claude's "Read"/"Write"/"Edit").
var piToolNameMap = map[string]string{
	"edit":  "Edit",
	"read":  "Read",
	"write": "Write",
}

// isPiFormat reports whether content looks like a Pi session JSONL file.
// Anchored on the persisted session header that pi writes as the first line.
func isPiFormat(content []byte) bool {
	scanner := pijsonl.NewScanner(content)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var probe struct {
			Type    string `json:"type"`
			Version int    `json:"version"`
		}
		if json.Unmarshal(line, &probe) != nil {
			return false
		}
		// Pi auto-migrates v1/v2 to v3 on load; accept any positive version.
		return probe.Type == "session" && probe.Version > 0
	}
	return false
}

// --- output structures (compact format) ---

type piCompactUserBlock struct {
	Text string `json:"text"`
}

type piCompactAssistantTextBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type piCompactToolUseBlock struct {
	Type   string               `json:"type"`
	ID     string               `json:"id,omitempty"`
	Name   string               `json:"name"`
	Input  any                  `json:"input"`
	Result *piCompactToolResult `json:"result,omitempty"`
}

type piCompactToolResult struct {
	Output string `json:"output"`
	Status string `json:"status"`
}

// compactPi converts a Pi JSONL transcript into the Entire compact format.
//
// opts.StartLine is treated as a JSONL line offset.
//
// IMPORTANT: active-branch resolution and tool-result collection BOTH run on
// the original (untruncated) content. A truncated buffer breaks parentId
// chains and toolCallId references, which would let abandoned-branch entries
// and orphaned tool results leak into the compact output.
func compactPi(content []byte, opts MetadataFields) ([]byte, error) {
	active := pijsonl.ResolveActiveBranch(content)
	results, err := piCollectToolResults(content, active)
	if err != nil {
		return nil, err
	}

	emit := content
	if opts.StartLine > 0 {
		emit = pijsonl.SkipLines(content, opts.StartLine)
		if emit == nil {
			return []byte{}, nil
		}
	}

	base := newTranscriptLine(opts)
	var out []byte

	scanner := pijsonl.NewScanner(emit)
	for scanner.Scan() {
		var entry pijsonl.Entry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		if entry.Type != pijsonl.EntryTypeMessage {
			continue
		}
		if active != nil && !active[entry.ID] {
			continue
		}

		switch entry.Message.Role {
		case pijsonl.RoleUser:
			blocks := piEmitUserContent(entry.Message.Content)
			if len(blocks) == 0 {
				continue
			}
			contentJSON, err := json.Marshal(blocks)
			if err != nil {
				return nil, fmt.Errorf("marshal pi user content: %w", err)
			}
			line := base
			line.Type = pijsonl.RoleUser
			line.TS = piTimestampJSON(entry.Timestamp)
			line.Content = contentJSON
			appendLine(&out, line)

		case pijsonl.RoleAssistant:
			blocks := piEmitAssistantContent(entry.Message.Content, results)
			if len(blocks) == 0 {
				continue
			}
			contentJSON, err := json.Marshal(blocks)
			if err != nil {
				return nil, fmt.Errorf("marshal pi assistant content: %w", err)
			}
			line := base
			line.Type = pijsonl.RoleAssistant
			line.TS = piTimestampJSON(entry.Timestamp)
			line.ID = entry.ID
			line.Content = contentJSON
			if entry.Message.Usage != nil {
				line.InputTokens = entry.Message.Usage.Input
				line.OutputTokens = entry.Message.Usage.Output
			}
			appendLine(&out, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan pi transcript: %w", err)
	}
	return out, nil
}

// piEmitUserContent decodes a Pi user message's content (string or block array)
// into compact user blocks.
func piEmitUserContent(raw json.RawMessage) []piCompactUserBlock {
	if text := pijsonl.DecodeStringContent(raw); text != "" {
		return []piCompactUserBlock{{Text: text}}
	}
	var items []pijsonl.ContentItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}
	blocks := make([]piCompactUserBlock, 0, len(items))
	for _, item := range items {
		if item.Type == pijsonl.ContentTypeText && item.Text != "" {
			blocks = append(blocks, piCompactUserBlock{Text: item.Text})
		}
	}
	return blocks
}

// piEmitAssistantContent decodes a Pi assistant message's content into
// compact assistant blocks (text + tool_use). Tool results are spliced in
// from `results` keyed by toolCallID.
func piEmitAssistantContent(raw json.RawMessage, results map[string]piCompactToolResult) []any {
	if text := pijsonl.DecodeStringContent(raw); text != "" {
		return []any{piCompactAssistantTextBlock{Type: pijsonl.ContentTypeText, Text: text}}
	}
	var items []pijsonl.ContentItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}
	blocks := make([]any, 0, len(items))
	for _, item := range items {
		switch item.Type {
		case pijsonl.ContentTypeText:
			if item.Text != "" {
				blocks = append(blocks, piCompactAssistantTextBlock{
					Type: pijsonl.ContentTypeText,
					Text: item.Text,
				})
			}
		case "toolCall":
			block := piCompactToolUseBlock{
				Type:  "tool_use",
				ID:    item.ID,
				Name:  piNormalizeToolName(item.Name),
				Input: piDecodeArguments(item.Arguments),
			}
			if r, ok := results[item.ID]; ok {
				block.Result = &r
			}
			blocks = append(blocks, block)
		}
	}
	return blocks
}

// piCollectToolResults walks the transcript and returns a map of tool-call id
// to spliceable result. Branch-aware. Pass FULL transcript bytes — splicing
// requires resolving toolCallIds across the whole tree.
func piCollectToolResults(data []byte, active map[string]bool) (map[string]piCompactToolResult, error) {
	results := map[string]piCompactToolResult{}
	scanner := pijsonl.NewScanner(data)
	for scanner.Scan() {
		var entry pijsonl.Entry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		if entry.Type != pijsonl.EntryTypeMessage || entry.Message.Role != pijsonl.RoleToolResult {
			continue
		}
		if active != nil && !active[entry.ID] {
			continue
		}
		if entry.Message.ToolCallID == "" {
			continue
		}
		results[entry.Message.ToolCallID] = piCompactToolResult{
			Output: piDecodeResultOutput(entry.Message.Content),
			Status: piResultStatus(entry.Message.IsError),
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan pi tool results: %w", err)
	}
	return results, nil
}

func piNormalizeToolName(name string) string {
	if normalized, ok := piToolNameMap[name]; ok {
		return normalized
	}
	return name
}

func piDecodeArguments(raw json.RawMessage) any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return map[string]any{}
	}
	return decoded
}

func piDecodeResultOutput(raw json.RawMessage) string {
	if text := pijsonl.DecodeStringContent(raw); text != "" {
		return text
	}
	var items []pijsonl.ContentItem
	if err := json.Unmarshal(raw, &items); err == nil {
		texts := make([]string, 0, len(items))
		for _, item := range items {
			if item.Type == pijsonl.ContentTypeText && item.Text != "" {
				texts = append(texts, item.Text)
			}
		}
		if len(texts) > 0 {
			return strings.Join(texts, "\n")
		}
	}
	// Fall through: serialize unknown structure as JSON.
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err == nil {
		if encoded, err := json.Marshal(decoded); err == nil {
			return string(encoded)
		}
	}
	return string(raw)
}

func piResultStatus(isError bool) string {
	if isError {
		return piToolResultStatusErr
	}
	return piToolResultStatusOK
}

func piTimestampJSON(ts string) json.RawMessage {
	if ts == "" {
		return nil
	}
	b, err := json.Marshal(ts)
	if err != nil {
		return nil
	}
	return b
}
