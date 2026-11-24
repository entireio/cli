package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/entireio/cli/internal/transcript/pijsonl"
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
// Parsing primitives (Entry/Message/ContentItem types, NewScanner) are shared
// with the pi agent package via internal/transcript/pijsonl so a fix applied
// here also lands there. Compaction folds a PiAdapter's records
// (pi_records.go), which resolves the active branch as
// pijsonl.ResolveActiveBranch does, one line at a time.

// piContentTypeToolCall is the content-block type value of Pi tool calls.
const piContentTypeToolCall = "toolCall"

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

// compactPi converts a Pi JSONL transcript into the Entire compact format by
// folding a PiAdapter's records, so checkpoints and live streams share one
// path.
//
// opts.StartLine is a JSONL line offset: the result is the lines of the
// messages read from that line on. Active-branch resolution and tool-result
// splicing still run over the whole transcript, so abandoned-branch entries
// and orphaned tool results cannot leak into the slice.
func compactPi(content []byte, opts Options) ([]byte, error) {
	if err := piCheckLineLengths(content); err != nil {
		return nil, err
	}
	a := NewPiAdapter()
	records := a.Feed(content)
	records = append(records, a.Flush()...)
	f := NewFolder(opts)
	if _, err := f.Apply(records); err != nil {
		return nil, fmt.Errorf("folding pi transcript records: %w", err)
	}
	if opts.StartLine <= 0 {
		return f.Lines(), nil
	}
	var out []byte
	for _, id := range a.messages {
		if a.entries[id].line <= opts.StartLine {
			continue
		}
		if line, ok := f.Line(id); ok {
			out = append(out, line...)
			out = append(out, '\n')
		}
	}
	return out, nil
}

// piCheckLineLengths keeps Convert failing where it always has: Pi
// transcripts were compacted with pijsonl's scanner, which fails on a line of
// pijsonl.MaxScannerLine bytes or more (newline excluded). PiAdapter itself
// has no line limit.
func piCheckLineLengths(content []byte) error {
	for rest := content; len(rest) > 0; {
		n := bytes.IndexByte(rest, '\n')
		if n < 0 {
			n = len(rest)
		}
		if n >= pijsonl.MaxScannerLine {
			return fmt.Errorf("scan pi transcript: %w", bufio.ErrTooLong)
		}
		rest = rest[min(n+1, len(rest)):]
	}
	return nil
}

// piCallUsage returns the per-call usage of a Pi assistant message, or nil
// when it reports none. Pi's input already excludes cached tokens on every
// backend that reports them, so it is used as is; a backend that folds cache
// reads into input cannot be told apart per call. The cache-write split is set
// only when Pi reports the 1h part.
func piCallUsage(m pijsonl.Message) *CallUsage {
	pu := m.Usage
	if pu == nil {
		return nil
	}
	u := &CallUsage{
		Model:            m.Model,
		InputTokens:      int64(pu.Input),
		OutputTokens:     int64(pu.Output),
		CacheReadTokens:  int64(pu.CacheRead),
		CacheWriteTokens: int64(pu.CacheWrite),
	}
	if pu.Reasoning != nil {
		u.ReasoningTokens = int64(*pu.Reasoning)
	}
	if pu.CacheWrite1h != nil {
		oneHour := int64(*pu.CacheWrite1h)
		fiveMin := max(0, u.CacheWriteTokens-oneHour)
		u.CacheWrite1hTokens, u.CacheWrite5mTokens = &oneHour, &fiveMin
	}
	return u
}

func piUsageHasCounts(u *CallUsage) bool {
	return u != nil && (u.InputTokens != 0 || u.OutputTokens != 0 || u.CacheReadTokens != 0 ||
		u.CacheWriteTokens != 0 || u.ReasoningTokens != 0)
}

// piEmitUserContent decodes a Pi user message's content (string or block array)
// into compact user blocks. Image items pass through verbatim, in source order.
func piEmitUserContent(raw json.RawMessage) []any {
	if text := pijsonl.DecodeStringContent(raw); text != "" {
		return []any{piCompactUserBlock{Text: text}}
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}
	blocks := make([]any, 0, len(items))
	for _, rawItem := range items {
		var item pijsonl.ContentItem
		if err := json.Unmarshal(rawItem, &item); err != nil {
			continue
		}
		switch {
		case item.Type == pijsonl.ContentTypeText && item.Text != "":
			blocks = append(blocks, piCompactUserBlock{Text: item.Text})
		case item.Type == BlockImage:
			blocks = append(blocks, rawItem)
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
		case piContentTypeToolCall:
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
		return ToolStatusError
	}
	return ToolStatusSuccess
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
