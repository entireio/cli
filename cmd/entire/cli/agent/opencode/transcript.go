package opencode

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// ParseTranscript parses raw JSONL content into transcript entries.
//
//nolint:unparam // error return kept for API consistency with other agent parsers
func ParseTranscript(data []byte) ([]TranscriptEntry, error) {
	var entries []TranscriptEntry
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry TranscriptEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			// Skip malformed lines
			continue
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// ExtractModifiedFiles extracts all files modified by tool calls in the transcript.
func ExtractModifiedFiles(data []byte) []string {
	entries, err := ParseTranscript(data)
	if err != nil {
		return nil
	}
	return ExtractModifiedFilesFromEntries(entries)
}

// ExtractModifiedFilesFromEntries extracts modified files from parsed entries.
func ExtractModifiedFilesFromEntries(entries []TranscriptEntry) []string {
	fileSet := make(map[string]bool)
	var files []string

	for i := range entries {
		for _, f := range extractFilesFromEntry(&entries[i]) {
			if !fileSet[f] {
				fileSet[f] = true
				files = append(files, f)
			}
		}
	}
	return files
}

// extractFilesFromEntry extracts modified file paths from a single transcript entry.
func extractFilesFromEntry(entry *TranscriptEntry) []string {
	if entry.Info.Role != MessageRoleAssistant {
		return nil
	}

	var files []string
	fileSet := make(map[string]bool)

	// Check summary diffs
	if entry.Info.Summary != nil {
		for _, diff := range entry.Info.Summary.Diffs {
			if diff.File != "" && !fileSet[diff.File] {
				fileSet[diff.File] = true
				files = append(files, diff.File)
			}
		}
	}

	// Check tool parts
	for _, part := range entry.Parts {
		// Check filePath on patch or tool parts (OpenCode sets filePath on both)
		if (part.Type == PartTypePatch || part.Type == PartTypeTool) && part.FilePath != "" && !fileSet[part.FilePath] {
			fileSet[part.FilePath] = true
			files = append(files, part.FilePath)
		}

		// Check tool parts for file modification tools (extract from state.input)
		if part.Type != PartTypeTool {
			continue
		}

		isModifyTool := false
		for _, name := range FileModificationTools {
			if part.Tool == name {
				isModifyTool = true
				break
			}
		}
		if !isModifyTool {
			continue
		}

		// Try to extract file path from tool state input
		file := extractFilePathFromToolState(part.State)
		if file != "" && !fileSet[file] {
			fileSet[file] = true
			files = append(files, file)
		}
	}

	return files
}

// extractFilePathFromToolState extracts the file path from a tool's state input.
func extractFilePathFromToolState(state *TranscriptToolState) string {
	if state == nil || len(state.Input) == 0 {
		return ""
	}

	var input map[string]interface{}
	if err := json.Unmarshal(state.Input, &input); err != nil {
		return ""
	}

	// Try common field names for file paths
	for _, key := range []string{"file_path", "path", "filePath", "filename"} {
		if fp, ok := input[key].(string); ok && fp != "" {
			return fp
		}
	}
	return ""
}

// CalculateTokenUsage calculates token usage from OpenCode transcript entries.
// OpenCode assistant messages carry per-message token counts in info.tokens,
// and step-finish parts carry per-step token counts.
// We use the message-level tokens (deduplicated by message ID) since they
// represent the authoritative totals from the provider API.
// startIndex is the entry index to start counting from (0-based).
func CalculateTokenUsage(entries []TranscriptEntry, startIndex int) *agent.TokenUsage {
	usage := &agent.TokenUsage{}

	// Deduplicate by message ID (in case of streaming duplicates)
	seen := make(map[string]bool)

	for i := startIndex; i < len(entries); i++ {
		entry := &entries[i]
		if entry.Info.Role != MessageRoleAssistant {
			continue
		}

		// Use message-level tokens if available (authoritative)
		if entry.Info.Tokens != nil && entry.Info.ID != "" && !seen[entry.Info.ID] {
			seen[entry.Info.ID] = true
			usage.InputTokens += entry.Info.Tokens.Input
			usage.OutputTokens += entry.Info.Tokens.Output
			usage.CacheReadTokens += entry.Info.Tokens.Cache.Read
			usage.CacheCreationTokens += entry.Info.Tokens.Cache.Write
			usage.APICallCount++
		}
	}

	return usage
}

// CalculateTokenUsageFromData calculates token usage from raw JSONL data.
func CalculateTokenUsageFromData(data []byte, startIndex int) *agent.TokenUsage {
	entries, err := ParseTranscript(data)
	if err != nil || len(entries) == 0 {
		return &agent.TokenUsage{}
	}
	return CalculateTokenUsage(entries, startIndex)
}

// CalculateTokenUsageFromFile calculates token usage from a transcript file.
func CalculateTokenUsageFromFile(path string, startIndex int) (*agent.TokenUsage, error) {
	if path == "" {
		return &agent.TokenUsage{}, nil
	}
	data, err := os.ReadFile(path) //nolint:gosec // Path comes from OpenCode transcript location
	if err != nil {
		return nil, fmt.Errorf("reading transcript file: %w", err)
	}
	return CalculateTokenUsageFromData(data, startIndex), nil
}

// ExtractLastUserPrompt extracts the last user message from transcript data.
func ExtractLastUserPrompt(data []byte) string {
	entries, err := ParseTranscript(data)
	if err != nil {
		return ""
	}
	return ExtractLastUserPromptFromEntries(entries)
}

// ExtractLastUserPromptFromEntries extracts the last user prompt from parsed entries.
func ExtractLastUserPromptFromEntries(entries []TranscriptEntry) string {
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Info.Role != MessageRoleUser {
			continue
		}
		// Collect text parts
		var texts []string
		for _, part := range entries[i].Parts {
			if part.Type == PartTypeText && part.Text != "" {
				texts = append(texts, part.Text)
			}
		}
		if len(texts) > 0 {
			return strings.Join(texts, "\n")
		}
	}
	return ""
}

// ExtractAllUserPrompts extracts all user messages from transcript data.
func ExtractAllUserPrompts(data []byte) ([]string, error) {
	entries, err := ParseTranscript(data)
	if err != nil {
		return nil, err
	}
	return ExtractAllUserPromptsFromEntries(entries), nil
}

// ExtractAllUserPromptsFromEntries extracts all user prompts from parsed entries.
func ExtractAllUserPromptsFromEntries(entries []TranscriptEntry) []string {
	var prompts []string
	for _, entry := range entries {
		if entry.Info.Role != MessageRoleUser {
			continue
		}
		var texts []string
		for _, part := range entry.Parts {
			if part.Type == PartTypeText && part.Text != "" {
				texts = append(texts, part.Text)
			}
		}
		if len(texts) > 0 {
			prompts = append(prompts, strings.Join(texts, "\n"))
		}
	}
	return prompts
}

// ExtractLastAssistantMessage extracts the last assistant message from transcript data.
func ExtractLastAssistantMessage(data []byte) (string, error) {
	entries, err := ParseTranscript(data)
	if err != nil {
		return "", err
	}
	return ExtractLastAssistantMessageFromEntries(entries), nil
}

// ExtractLastAssistantMessageFromEntries extracts the last assistant response from parsed entries.
func ExtractLastAssistantMessageFromEntries(entries []TranscriptEntry) string {
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Info.Role != MessageRoleAssistant {
			continue
		}
		var texts []string
		for _, part := range entries[i].Parts {
			if part.Type == PartTypeText && part.Text != "" {
				texts = append(texts, part.Text)
			}
		}
		if len(texts) > 0 {
			return strings.Join(texts, "\n")
		}
	}
	return ""
}
