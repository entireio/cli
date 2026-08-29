// Package pijsonl provides shared parsing primitives for Pi's session JSONL
// format. It is consumed both by the in-tree pi agent (cmd/entire/cli/agent/pi)
// and by the transcript compaction package (cmd/entire/cli/transcript/compact).
// Keeping these in one place ensures
// active-branch resolution, line counting, and offset slicing stay byte-
// compatible across both call sites.
package pijsonl

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
)

// EntryTypeMessage is the JSONL `type` value for conversational entries.
const EntryTypeMessage = "message"

// EntryTypeThinkingLevelChange is the JSONL `type` value for the entry Pi
// writes when the user changes the thinking level; the new level is on
// Entry.ThinkingLevel and applies to every later assistant message.
const EntryTypeThinkingLevelChange = "thinking_level_change"

// Role values present on Message.Role.
const (
	RoleUser       = "user"
	RoleAssistant  = "assistant"
	RoleToolResult = "toolResult"
)

// Content-block `type` values on ContentItem.Type.
const (
	ContentTypeText     = "text"
	ContentTypeToolCall = "toolCall"
)

// MaxScannerLine is the maximum size of a single JSONL line we will parse.
// Pi tool calls can embed full file contents in arguments — 10 MB matches
// what other in-tree transcript scanners use (codex, copilot, droid).
const MaxScannerLine = 10 * 1024 * 1024

// NewScanner returns a bufio.Scanner pre-configured with the Pi line-size limit.
func NewScanner(data []byte) *bufio.Scanner {
	s := bufio.NewScanner(bytes.NewReader(data))
	s.Buffer(make([]byte, 0, MaxScannerLine), MaxScannerLine)
	return s
}

// CountLines returns the number of newline-terminated (or final unterminated)
// lines in data, including blank lines. Matches the offset semantics used by
// SkipLines and the compact-format StartLine.
func CountLines(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	n := bytes.Count(data, []byte{'\n'})
	if data[len(data)-1] != '\n' {
		n++
	}
	return n
}

// SkipLines returns data with the first n newline-terminated lines removed.
// Returns nil if data has fewer than n lines.
func SkipLines(data []byte, n int) []byte {
	if n <= 0 {
		return data
	}
	off := 0
	for i := 0; i < n && off < len(data); i++ {
		idx := bytes.IndexByte(data[off:], '\n')
		if idx < 0 {
			return nil
		}
		off += idx + 1
	}
	return data[off:]
}

// Entry is the outer shell of one Pi JSONL line.
type Entry struct {
	Type      string  `json:"type"`
	ID        string  `json:"id"`
	Timestamp string  `json:"timestamp"`
	Message   Message `json:"message"`
	// ThinkingLevel is set on EntryTypeThinkingLevelChange entries only: the
	// level Pi runs at from here on ("off", "minimal", "low", "medium",
	// "high", "xhigh"), verbatim.
	ThinkingLevel string `json:"thinkingLevel,omitempty"`
}

// Message is the inner Pi `message` object on entries with type=="message".
type Message struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Usage      *Usage          `json:"usage,omitempty"`
	Model      string          `json:"model,omitempty"`
	Provider   string          `json:"provider,omitempty"`
	StopReason string          `json:"stopReason,omitempty"`
	ToolCallID string          `json:"toolCallId,omitempty"`
	ToolName   string          `json:"toolName,omitempty"`
	IsError    bool            `json:"isError,omitempty"`
}

// Usage mirrors pi-ai's Usage struct: the token counts plus the cost block
// Pi computes from its model registry (totalTokens is derived and not kept).
type Usage struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cacheRead"`
	CacheWrite int `json:"cacheWrite"`
	// CacheWrite1h is the 1-hour-TTL part of CacheWrite (Anthropic providers only).
	CacheWrite1h int       `json:"cacheWrite1h"`
	Cost         UsageCost `json:"cost"`
}

// UsageCost is pi-ai's Usage.cost block, reduced to the total dollar figure.
// The per-kind breakdown (input, output, cacheRead, cacheWrite) is not kept.
type UsageCost struct {
	Total float64 `json:"total"`
}

// ContentItem is one element of a Pi message's content array.
type ContentItem struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Name      string          `json:"name,omitempty"`
	ID        string          `json:"id,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// ForEachActiveMessage invokes fn for every message entry on the active
// conversation branch, starting at line fromOffset. It is ForEachActiveEntry
// filtered to entries with type=="message"; callers apply their own role
// filter inside fn.
func ForEachActiveMessage(data []byte, fromOffset int, fn func(Entry)) error {
	return ForEachActiveEntry(data, fromOffset, func(_ int, entry Entry) {
		if entry.Type == EntryTypeMessage {
			fn(entry)
		}
	})
}

// ForEachActiveEntry invokes fn for every entry of any type on the active
// conversation branch, starting at line fromOffset, with the entry's 0-based
// physical line index in data.
//
// It owns the parsing skeleton shared by all Pi transcript analysis: active-
// branch resolution runs on the FULL data (so parentId chains stay intact)
// while iteration starts at fromOffset; lines that fail to unmarshal or are
// off the active branch are skipped. When the transcript has a tree (see
// ResolveActiveBranch) the session header is off the branch — its id is never
// a parentId — and so is skipped too; in a flat transcript every entry passes.
//
// The line index counts lines exactly as SkipLines does: every '\n'
// terminates a line, blank and malformed lines count, and the empty tail after
// a trailing '\n' is not a line. Empty data is a no-op.
func ForEachActiveEntry(data []byte, fromOffset int, fn func(line int, entry Entry)) error {
	if len(data) == 0 {
		return nil
	}

	active := ResolveActiveBranch(data)
	content := SkipLines(data, fromOffset)

	scanner := NewScanner(content)
	for line := max(fromOffset, 0); scanner.Scan(); line++ {
		var entry Entry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		if active != nil && !active[entry.ID] {
			continue
		}
		fn(line, entry)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan transcript: %w", err)
	}
	return nil
}

// ResolveActiveBranch walks a Pi transcript tree and returns the set of entry
// IDs on the active conversation branch (root → most-recent message).
//
// Pi sessions form a tree: every entry has id and parentId. When the user
// /forks or /clones mid-conversation, the JSONL file accumulates entries from
// BOTH branches. Without filtering, downstream analysis double-counts tokens,
// files, and prompts.
//
// IMPORTANT: callers must pass FULL transcript bytes, not bytes already
// truncated by SkipLines. Resolving over a truncated buffer yields a
// disconnected tree where parentId pointers no longer reach the root, causing
// abandoned-branch entries to leak in.
//
// Returns nil when the transcript has no tree structure (every entry has no
// parent or all entries are linear) — callers should treat nil as "all entries
// are on the active branch".
func ResolveActiveBranch(data []byte) map[string]bool {
	type node struct {
		Type     string  `json:"type"`
		ID       string  `json:"id"`
		ParentID *string `json:"parentId"`
	}

	var lastMessageID string
	hasTree := false
	parentOf := make(map[string]string)

	scanner := NewScanner(data)
	for scanner.Scan() {
		var n node
		if err := json.Unmarshal(scanner.Bytes(), &n); err != nil || n.ID == "" {
			continue
		}
		if n.ParentID != nil {
			parentOf[n.ID] = *n.ParentID
			if *n.ParentID != "" {
				hasTree = true
			}
		}
		if n.Type == EntryTypeMessage {
			lastMessageID = n.ID
		}
	}

	if !hasTree || lastMessageID == "" {
		return nil
	}

	active := make(map[string]bool)
	for cur := lastMessageID; cur != ""; {
		if active[cur] {
			break // cycle protection
		}
		active[cur] = true
		parent, ok := parentOf[cur]
		if !ok {
			break
		}
		cur = parent
	}
	return active
}

// DecodeStringContent returns the raw string when content is a plain string,
// or "" when it's a JSON array (caller should decode as []ContentItem).
func DecodeStringContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return ""
}
