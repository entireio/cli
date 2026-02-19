package pi

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/textutil"
)

const (
	entryTypeMessage = "message"
	roleUser         = "user"
	roleAssistant    = "assistant"
	roleToolResult   = "toolResult"
	toolNameWrite    = "write"
	toolNameEdit     = "edit"
)

// TranscriptEntry represents one JSONL row in a Pi transcript.
type TranscriptEntry struct {
	Type     string             `json:"type"`
	ID       string             `json:"id,omitempty"`
	UUID     string             `json:"uuid,omitempty"`
	ParentID *string            `json:"parentId,omitempty"`
	Message  *TranscriptMessage `json:"message,omitempty"`
	Summary  string             `json:"summary,omitempty"`
	Content  interface{}        `json:"content,omitempty"`
	Display  bool               `json:"display,omitempty"`
}

// EntryID returns the best available identifier for this entry.
func (e TranscriptEntry) EntryID() string {
	if e.ID != "" {
		return e.ID
	}
	return e.UUID
}

// TranscriptMessage is the message payload of a transcript entry.
type TranscriptMessage struct {
	Role       string                 `json:"role"`
	Content    interface{}            `json:"content,omitempty"`
	ToolName   string                 `json:"toolName,omitempty"`
	ToolCallID string                 `json:"toolCallId,omitempty"`
	Details    interface{}            `json:"details,omitempty"`
	Usage      map[string]interface{} `json:"usage,omitempty"`
	Tokens     map[string]interface{} `json:"tokens,omitempty"`
}

type parsedTranscriptEntry struct {
	Entry      TranscriptEntry
	LineNumber int
}

// ParseTranscript parses active-branch JSONL entries from transcript bytes.
//
// For tree-based transcripts, this reconstructs the active path using parentId pointers.
// Without an explicit leaf ID, it falls back to Pi's behavior of using the last entry.
func ParseTranscript(data []byte) ([]TranscriptEntry, error) {
	entries, _, err := parseTranscriptFromBytes(data, 0, "")
	if err != nil {
		return nil, err
	}
	return entries, nil
}

// ParseTranscriptWithLeaf parses active-branch JSONL entries using an explicit leaf ID.
func ParseTranscriptWithLeaf(data []byte, leafID string) ([]TranscriptEntry, error) {
	entries, _, err := parseTranscriptFromBytes(data, 0, leafID)
	if err != nil {
		return nil, err
	}
	return entries, nil
}

// ParseTranscriptFromLine parses active-branch transcript entries from a file starting at startLine (0-indexed).
// Returns parsed entries and the total number of physical lines in the file.
func ParseTranscriptFromLine(path string, startLine int) ([]TranscriptEntry, int, error) {
	return ParseTranscriptFromLineWithLeaf(path, startLine, "")
}

// ParseTranscriptFromLineWithLeaf parses active-branch transcript entries from a file,
// constrained to entries at or after startLine and resolved from the provided leaf ID.
func ParseTranscriptFromLineWithLeaf(path string, startLine int, leafID string) ([]TranscriptEntry, int, error) {
	if path == "" {
		return nil, 0, nil
	}

	file, err := os.Open(path) //nolint:gosec // path comes from session transcript location
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("failed to open transcript: %w", err)
	}
	defer file.Close()

	return parseTranscriptFromReader(file, startLine, leafID)
}

// ExtractAllUserPrompts returns all user prompts in chronological order.
func ExtractAllUserPrompts(data []byte) ([]string, error) {
	entries, err := ParseTranscript(data)
	if err != nil {
		return nil, err
	}
	return ExtractAllUserPromptsFromEntries(entries), nil
}

// ExtractAllUserPromptsFromEntries returns all user prompts from parsed entries.
func ExtractAllUserPromptsFromEntries(entries []TranscriptEntry) []string {
	var prompts []string
	for _, entry := range entries {
		if entry.Type != entryTypeMessage || entry.Message == nil || entry.Message.Role != roleUser {
			continue
		}

		text := joinTextContent(entry.Message.Content)
		if text == "" {
			continue
		}

		cleaned := textutil.StripIDEContextTags(text)
		if cleaned != "" {
			prompts = append(prompts, cleaned)
		}
	}
	return prompts
}

// ExtractLastUserPrompt returns the last user prompt from transcript bytes.
func ExtractLastUserPrompt(data []byte) (string, error) {
	entries, err := ParseTranscript(data)
	if err != nil {
		return "", err
	}
	return ExtractLastUserPromptFromEntries(entries), nil
}

// ExtractLastUserPromptFromEntries returns the last user prompt from parsed entries.
func ExtractLastUserPromptFromEntries(entries []TranscriptEntry) string {
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		if entry.Type != entryTypeMessage || entry.Message == nil || entry.Message.Role != roleUser {
			continue
		}
		text := textutil.StripIDEContextTags(joinTextContent(entry.Message.Content))
		if text != "" {
			return text
		}
	}
	return ""
}

// ExtractLastAssistantMessage returns the latest assistant text response.
func ExtractLastAssistantMessage(data []byte) (string, error) {
	entries, err := ParseTranscript(data)
	if err != nil {
		return "", err
	}
	return ExtractLastAssistantMessageFromEntries(entries), nil
}

// ExtractLastAssistantMessageFromEntries returns the latest assistant text response.
func ExtractLastAssistantMessageFromEntries(entries []TranscriptEntry) string {
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		if entry.Type != entryTypeMessage || entry.Message == nil || entry.Message.Role != roleAssistant {
			continue
		}

		texts := extractTextBlocks(entry.Message.Content)
		if len(texts) == 0 {
			continue
		}

		for j := len(texts) - 1; j >= 0; j-- {
			if strings.TrimSpace(texts[j]) != "" {
				return strings.TrimSpace(texts[j])
			}
		}
	}
	return ""
}

// ExtractModifiedFiles parses modified files from transcript bytes.
func ExtractModifiedFiles(data []byte) ([]string, error) {
	entries, err := ParseTranscript(data)
	if err != nil {
		return nil, err
	}
	return ExtractModifiedFilesFromEntries(entries), nil
}

// ExtractModifiedFilesSinceOffset parses modified files from a file starting at startOffset.
func ExtractModifiedFilesSinceOffset(path string, startOffset int) (files []string, currentPosition int, err error) {
	return ExtractModifiedFilesSinceOffsetWithLeaf(path, startOffset, "")
}

// ExtractModifiedFilesSinceOffsetWithLeaf parses modified files from a file starting at startOffset,
// resolving active branch from the provided leaf ID when available.
func ExtractModifiedFilesSinceOffsetWithLeaf(path string, startOffset int, leafID string) (files []string, currentPosition int, err error) {
	entries, totalLines, err := ParseTranscriptFromLineWithLeaf(path, startOffset, leafID)
	if err != nil {
		return nil, 0, err
	}
	return ExtractModifiedFilesFromEntries(entries), totalLines, nil
}

// ExtractModifiedFilesFromEntries parses modified files from parsed transcript entries.
func ExtractModifiedFilesFromEntries(entries []TranscriptEntry) []string {
	files := make(map[string]bool)

	for _, entry := range entries {
		if entry.Type != entryTypeMessage || entry.Message == nil {
			continue
		}

		msg := entry.Message
		switch msg.Role {
		case roleToolResult:
			if msg.ToolName != toolNameWrite && msg.ToolName != toolNameEdit {
				continue
			}
			if path := extractPathFromAny(msg.Details); path != "" {
				files[path] = true
			}
		case roleAssistant:
			for _, file := range extractModifiedFilesFromAssistantContent(msg.Content) {
				files[file] = true
			}
		}
	}

	result := make([]string, 0, len(files))
	for file := range files {
		result = append(result, file)
	}
	return result
}

// FindCheckpointEntryID finds the entry ID of the toolResult for the given tool call ID.
func FindCheckpointEntryID(data []byte, toolCallID string) (string, bool) {
	if toolCallID == "" {
		return "", false
	}

	entries, err := ParseTranscript(data)
	if err != nil {
		return "", false
	}

	for _, entry := range entries {
		if entry.Type != entryTypeMessage || entry.Message == nil || entry.Message.Role != roleToolResult {
			continue
		}
		if entry.Message.ToolCallID == toolCallID {
			id := entry.EntryID()
			if id != "" {
				return id, true
			}
		}
	}

	return "", false
}

// CalculateTokenUsageFromTranscript calculates token usage from transcript bytes, starting at startOffset.
func CalculateTokenUsageFromTranscript(data []byte, startOffset int) *agent.TokenUsage {
	return CalculateTokenUsageFromTranscriptWithLeaf(data, startOffset, "")
}

// CalculateTokenUsageFromTranscriptWithLeaf calculates token usage from transcript bytes,
// resolving active branch using an explicit leaf ID when available.
func CalculateTokenUsageFromTranscriptWithLeaf(data []byte, startOffset int, leafID string) *agent.TokenUsage {
	entries, _, err := parseTranscriptFromBytes(data, startOffset, leafID)
	if err != nil {
		return &agent.TokenUsage{}
	}
	return CalculateTokenUsageFromEntries(entries)
}

// CalculateTokenUsageFromFile calculates token usage from a transcript file.
func CalculateTokenUsageFromFile(path string, startOffset int) (*agent.TokenUsage, error) {
	return CalculateTokenUsageFromFileWithLeaf(path, startOffset, "")
}

// CalculateTokenUsageFromFileWithLeaf calculates token usage from a transcript file,
// resolving active branch using an explicit leaf ID when available.
func CalculateTokenUsageFromFileWithLeaf(path string, startOffset int, leafID string) (*agent.TokenUsage, error) {
	if path == "" {
		return &agent.TokenUsage{}, nil
	}

	entries, _, err := ParseTranscriptFromLineWithLeaf(path, startOffset, leafID)
	if err != nil {
		return nil, err
	}

	return CalculateTokenUsageFromEntries(entries), nil
}

// CalculateTokenUsageFromEntries calculates token usage from parsed transcript entries.
func CalculateTokenUsageFromEntries(entries []TranscriptEntry) *agent.TokenUsage {
	usage := &agent.TokenUsage{}

	for _, entry := range entries {
		if entry.Type != entryTypeMessage || entry.Message == nil || entry.Message.Role != roleAssistant {
			continue
		}

		usage.APICallCount++
		stats := tokenStatsFromMessage(entry.Message)
		usage.InputTokens += stats.input
		usage.OutputTokens += stats.output
		usage.CacheReadTokens += stats.cacheRead
		usage.CacheCreationTokens += stats.cacheCreation
	}

	return usage
}

func parseTranscriptFromBytes(data []byte, startLine int, leafID string) ([]TranscriptEntry, int, error) {
	return parseTranscriptFromReader(bytes.NewReader(data), startLine, leafID)
}

func parseTranscriptFromReader(reader io.Reader, startLine int, leafID string) ([]TranscriptEntry, int, error) {
	allEntries, totalLines, err := parseTranscriptEntries(reader)
	if err != nil {
		return nil, 0, err
	}

	entries := selectActiveEntries(allEntries, startLine, leafID)
	return entries, totalLines, nil
}

func parseTranscriptEntries(reader io.Reader) ([]parsedTranscriptEntry, int, error) {
	bufReader := bufio.NewReader(reader)
	entries := make([]parsedTranscriptEntry, 0)
	lineCount := 0

	for {
		lineBytes, err := bufReader.ReadBytes('\n')
		if err != nil && err != io.EOF {
			return nil, 0, fmt.Errorf("failed to scan transcript: %w", err)
		}

		if len(lineBytes) > 0 {
			line := strings.TrimSpace(string(lineBytes))
			if line != "" {
				var entry TranscriptEntry
				if unmarshalErr := json.Unmarshal([]byte(line), &entry); unmarshalErr == nil {
					entries = append(entries, parsedTranscriptEntry{Entry: entry, LineNumber: lineCount})
				}
			}
			lineCount++
		}

		if err == io.EOF {
			break
		}
	}

	return entries, lineCount, nil
}

func selectActiveEntries(allEntries []parsedTranscriptEntry, startLine int, leafID string) []TranscriptEntry {
	if len(allEntries) == 0 {
		return nil
	}

	if !hasTreeParentReferences(allEntries) {
		return filterEntriesByOffset(allEntries, startLine, nil)
	}

	activeIDs := buildActivePathIDSet(allEntries, leafID)
	return filterEntriesByOffset(allEntries, startLine, activeIDs)
}

func hasTreeParentReferences(entries []parsedTranscriptEntry) bool {
	for _, entry := range entries {
		if entry.Entry.ParentID == nil {
			continue
		}
		if strings.TrimSpace(*entry.Entry.ParentID) != "" {
			return true
		}
	}
	return false
}

func buildActivePathIDSet(entries []parsedTranscriptEntry, leafID string) map[string]struct{} {
	byID := make(map[string]TranscriptEntry, len(entries))
	lastID := ""

	for _, entry := range entries {
		id := entry.Entry.EntryID()
		if id == "" {
			continue
		}
		byID[id] = entry.Entry
		lastID = id
	}

	if len(byID) == 0 {
		return nil
	}

	resolvedLeafID := strings.TrimSpace(leafID)
	if resolvedLeafID == "" {
		resolvedLeafID = lastID
	}
	if _, ok := byID[resolvedLeafID]; !ok {
		resolvedLeafID = lastID
	}
	if resolvedLeafID == "" {
		return nil
	}

	active := make(map[string]struct{})
	current := resolvedLeafID

	for current != "" {
		if _, seen := active[current]; seen {
			break
		}

		entry, ok := byID[current]
		if !ok {
			break
		}

		active[current] = struct{}{}
		if entry.ParentID == nil {
			break
		}

		parentID := strings.TrimSpace(*entry.ParentID)
		if parentID == "" || parentID == current {
			break
		}
		current = parentID
	}

	if len(active) == 0 {
		return nil
	}

	return active
}

func filterEntriesByOffset(entries []parsedTranscriptEntry, startLine int, activeIDs map[string]struct{}) []TranscriptEntry {
	filtered := make([]TranscriptEntry, 0, len(entries))

	for _, entry := range entries {
		if entry.LineNumber < startLine {
			continue
		}

		if activeIDs != nil {
			id := entry.Entry.EntryID()
			if id == "" {
				continue
			}
			if _, ok := activeIDs[id]; !ok {
				continue
			}
		}

		filtered = append(filtered, entry.Entry)
	}

	return filtered
}

func extractTextBlocks(content interface{}) []string {
	switch typed := content.(type) {
	case string:
		if strings.TrimSpace(typed) == "" {
			return nil
		}
		return []string{typed}
	case []interface{}:
		var texts []string
		for _, item := range typed {
			switch block := item.(type) {
			case string:
				if strings.TrimSpace(block) != "" {
					texts = append(texts, block)
				}
			case map[string]interface{}:
				blockType, hasType := block["type"].(string)
				text, hasText := block["text"].(string)
				if !hasText || text == "" {
					continue
				}
				if !hasType || blockType == "" || blockType == "text" {
					texts = append(texts, text)
				}
			}
		}
		return texts
	case map[string]interface{}:
		text, hasText := typed["text"].(string)
		if !hasText || strings.TrimSpace(text) == "" {
			return nil
		}
		return []string{text}
	default:
		return nil
	}
}

func joinTextContent(content interface{}) string {
	texts := extractTextBlocks(content)
	if len(texts) == 0 {
		return ""
	}
	return strings.TrimSpace(strings.Join(texts, "\n\n"))
}

func extractModifiedFilesFromAssistantContent(content interface{}) []string {
	arr, ok := content.([]interface{})
	if !ok {
		return nil
	}

	var files []string
	for _, item := range arr {
		block, ok := item.(map[string]interface{})
		if !ok {
			continue
		}

		blockType, hasType := block["type"].(string)
		if !hasType || blockType != "toolCall" {
			continue
		}

		name, hasName := block["name"].(string)
		if !hasName || (name != toolNameWrite && name != toolNameEdit) {
			continue
		}

		path := extractPathFromAny(block["arguments"])
		if path != "" {
			files = append(files, path)
		}
	}

	return files
}

func extractPathFromAny(raw interface{}) string {
	asMap, ok := raw.(map[string]interface{})
	if !ok {
		return ""
	}

	for _, key := range []string{"path", "file_path", "filename"} {
		if value, ok := asMap[key].(string); ok && strings.TrimSpace(value) != "" {
			return value
		}
	}

	return ""
}

type tokenStats struct {
	input         int
	output        int
	cacheRead     int
	cacheCreation int
}

func tokenStatsFromMessage(msg *TranscriptMessage) tokenStats {
	maps := []map[string]interface{}{
		msg.Usage,
		msg.Tokens,
	}

	if details, ok := msg.Details.(map[string]interface{}); ok {
		if usageMap, ok := details["usage"].(map[string]interface{}); ok {
			maps = append(maps, usageMap)
		}
		if tokensMap, ok := details["tokens"].(map[string]interface{}); ok {
			maps = append(maps, tokensMap)
		}
	}

	stats := tokenStats{}
	for _, m := range maps {
		if len(m) == 0 {
			continue
		}

		stats.input += firstIntFromMap(m,
			"input_tokens",
			"inputTokens",
			"promptTokens",
			"input",
		)
		stats.output += firstIntFromMap(m,
			"output_tokens",
			"outputTokens",
			"completionTokens",
			"output",
		)
		stats.cacheRead += firstIntFromMap(m,
			"cache_read_input_tokens",
			"cacheReadInputTokens",
			"cacheReadTokens",
			"cached",
			"cacheRead",
		)
		stats.cacheCreation += firstIntFromMap(m,
			"cache_creation_input_tokens",
			"cacheCreationInputTokens",
			"cacheCreationTokens",
			"cacheWrite",
		)
	}

	return stats
}

func firstIntFromMap(data map[string]interface{}, keys ...string) int {
	for _, key := range keys {
		value, ok := data[key]
		if !ok {
			continue
		}
		if parsed, ok := parseInt(value); ok {
			return parsed
		}
	}
	return 0
}

func parseInt(value interface{}) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case int32:
		return int(v), true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	case json.Number:
		parsed, err := v.Int64()
		if err != nil {
			return 0, false
		}
		return int(parsed), true
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}
