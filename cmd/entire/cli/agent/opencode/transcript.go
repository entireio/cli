package opencode

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// Compile-time interface assertions
var (
	_ agent.TranscriptAnalyzer = (*OpenCodeAgent)(nil)
	_ agent.TranscriptPreparer = (*OpenCodeAgent)(nil)
	_ agent.PromptExtractor    = (*OpenCodeAgent)(nil)
	_ agent.TokenCalculator    = (*OpenCodeAgent)(nil)
)

// ParseExportSession parses export JSON content into an ExportSession structure.
func ParseExportSession(data []byte) (*ExportSession, error) {
	if len(data) == 0 {
		return nil, nil //nolint:nilnil // nil for empty data is expected
	}

	var session ExportSession
	if err := json.Unmarshal(data, &session); err != nil {
		return nil, fmt.Errorf("failed to parse export session: %w", err)
	}

	return &session, nil
}

// parseExportSessionFromFile reads a file and parses its contents as an ExportSession.
func parseExportSessionFromFile(path string) (*ExportSession, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path from agent hook/session state
	if err != nil {
		return nil, err //nolint:wrapcheck // caller adds context or checks os.IsNotExist
	}
	return ParseExportSession(data)
}

// SliceFromMessage returns an OpenCode export transcript scoped to messages starting from
// startMessageIndex. This is the OpenCode equivalent of transcript.SliceFromLine —
// for OpenCode's JSON format, scoping is done by message index rather than line offset.
// Returns the original data if startMessageIndex <= 0.
// Returns nil, nil if startMessageIndex exceeds the number of messages.
func SliceFromMessage(data []byte, startMessageIndex int) ([]byte, error) {
	if len(data) == 0 || startMessageIndex <= 0 {
		return data, nil
	}

	session, err := ParseExportSession(data)
	if err != nil {
		return nil, fmt.Errorf("failed to parse export session for slicing: %w", err)
	}
	if session == nil {
		return nil, nil
	}

	if startMessageIndex >= len(session.Messages) {
		return nil, nil
	}

	scoped := &ExportSession{
		Info:     session.Info,
		Messages: session.Messages[startMessageIndex:],
	}

	out, err := json.Marshal(scoped)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal scoped session: %w", err)
	}
	return out, nil
}

// GetTranscriptPosition returns the number of messages in the transcript.
func (a *OpenCodeAgent) GetTranscriptPosition(path string) (int, error) {
	session, err := parseExportSessionFromFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	if session == nil {
		return 0, nil
	}
	return len(session.Messages), nil
}

// ExtractModifiedFilesFromOffset extracts files modified by tool calls from the given message offset.
func (a *OpenCodeAgent) ExtractModifiedFilesFromOffset(path string, startOffset int) ([]string, int, error) {
	session, err := parseExportSessionFromFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	if session == nil {
		return nil, 0, nil
	}

	seen := make(map[string]bool)
	var files []string

	for i := startOffset; i < len(session.Messages); i++ {
		msg := session.Messages[i]
		if msg.Info.Role != roleAssistant {
			continue
		}
		for _, part := range msg.Parts {
			if part.Type != "tool" || part.State == nil {
				continue
			}
			if !slices.Contains(FileModificationTools, part.Tool) {
				continue
			}
			for _, filePath := range extractFilePaths(part.State) {
				if !seen[filePath] {
					seen[filePath] = true
					files = append(files, filePath)
				}
			}
		}
	}

	return files, len(session.Messages), nil
}

// ExtractModifiedFiles extracts modified file paths from raw export JSON transcript bytes.
// This is the bytes-based equivalent of ExtractModifiedFilesFromOffset, used by ReadSession.
func ExtractModifiedFiles(data []byte) ([]string, error) {
	session, err := ParseExportSession(data)
	if err != nil {
		return nil, err
	}
	if session == nil {
		return nil, nil
	}

	seen := make(map[string]bool)
	var files []string

	for _, msg := range session.Messages {
		if msg.Info.Role != roleAssistant {
			continue
		}
		for _, part := range msg.Parts {
			if part.Type != "tool" || part.State == nil {
				continue
			}
			if !slices.Contains(FileModificationTools, part.Tool) {
				continue
			}
			for _, filePath := range extractFilePaths(part.State) {
				if !seen[filePath] {
					seen[filePath] = true
					files = append(files, filePath)
				}
			}
		}
	}

	return files, nil
}

// extractFilePaths extracts file paths from an OpenCode tool's state.
// For standard tools (edit, write), the path is in input.filePath or input.path.
// For apply_patch (codex models), the paths are in state.metadata.files[].filePath.
func extractFilePaths(state *ToolState) []string {
	if state == nil {
		return nil
	}

	// Check metadata.files first (used by apply_patch / codex models).
	if state.Metadata != nil {
		var paths []string
		for _, f := range state.Metadata.Files {
			if f.FilePath != "" {
				paths = append(paths, f.FilePath)
			}
		}
		if len(paths) > 0 {
			return paths
		}
	}

	// Fall back to input keys (used by edit, write).
	for _, key := range []string{"filePath", "path"} {
		if v, ok := state.Input[key]; ok {
			if s, ok := v.(string); ok && s != "" {
				return []string{s}
			}
		}
	}
	return nil
}

// ExtractTextFromParts extracts text content from message parts.
func ExtractTextFromParts(parts []Part) string {
	var texts []string
	for _, part := range parts {
		if part.Type == "text" && part.Text != "" {
			texts = append(texts, part.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// Tags used by oh-my-opencode and similar orchestration tools to inject
// context into the conversation with role "user" — not actual user prompts.
const (
	systemReminderOpen  = "<system-reminder>"
	systemReminderClose = "</system-reminder>"
)

// isSystemReminderOnly reports whether content consists entirely of
// <system-reminder>...</system-reminder> blocks (after trimming whitespace).
// Delegates to stripSystemReminders to correctly handle multiple blocks
// (HasPrefix+HasSuffix would false-positive on "<sr>a</sr>real<sr>b</sr>").
func isSystemReminderOnly(content string) bool {
	if strings.TrimSpace(content) == "" {
		return false
	}
	return stripSystemReminders(content) == ""
}

// stripSystemReminders removes all <system-reminder>...</system-reminder>
// sections from content and returns the remaining text. If nothing remains
// after stripping (or the content was system-reminder-only), it returns "".
func stripSystemReminders(content string) string {
	result := content
	for {
		start := strings.Index(result, systemReminderOpen)
		if start == -1 {
			break
		}
		end := strings.Index(result[start:], systemReminderClose)
		if end == -1 {
			break
		}
		end += start + len(systemReminderClose)
		result = result[:start] + result[end:]
	}
	return strings.TrimSpace(result)
}

// ExtractAllUserPrompts extracts all user prompts from raw export JSON transcript bytes.
// This is a package-level function used by the condensation path.
// Messages that consist entirely of <system-reminder> tags (e.g. from oh-my-opencode)
// are excluded. Mixed messages have their system-reminder sections stripped.
func ExtractAllUserPrompts(data []byte) ([]string, error) {
	session, err := ParseExportSession(data)
	if err != nil {
		return nil, err
	}
	if session == nil {
		return nil, nil
	}

	var prompts []string
	for _, msg := range session.Messages {
		if msg.Info.Role != roleUser {
			continue
		}
		content := ExtractTextFromParts(msg.Parts)
		if content == "" {
			continue
		}
		if isSystemReminderOnly(content) {
			continue
		}
		content = stripSystemReminders(content)
		if content != "" {
			prompts = append(prompts, content)
		}
	}
	return prompts, nil
}

// ExtractPrompts extracts user prompts from an OpenCode export transcript starting
// at the given message offset.
func (a *OpenCodeAgent) ExtractPrompts(sessionRef string, fromOffset int) ([]string, error) {
	data, err := os.ReadFile(sessionRef) //nolint:gosec // path comes from validated agent session state
	if err != nil {
		return nil, fmt.Errorf("failed to read opencode transcript for prompt extraction: %w", err)
	}

	scoped, err := SliceFromMessage(data, fromOffset)
	if err != nil {
		return nil, fmt.Errorf("failed to scope opencode transcript for prompt extraction: %w", err)
	}

	prompts, err := ExtractAllUserPrompts(scoped)
	if err != nil {
		return nil, fmt.Errorf("failed to extract prompts from opencode transcript: %w", err)
	}
	return prompts, nil
}

// CalculateTokenUsage computes token usage from assistant messages starting at the given offset.
func (a *OpenCodeAgent) CalculateTokenUsage(transcriptData []byte, fromOffset int) (*agent.TokenUsage, error) {
	session, err := ParseExportSession(transcriptData)
	if err != nil {
		return nil, fmt.Errorf("failed to parse transcript for token usage: %w", err)
	}
	if session == nil {
		return nil, nil //nolint:nilnil // nil usage for empty data is expected
	}

	usage := &agent.TokenUsage{}
	for i := fromOffset; i < len(session.Messages); i++ {
		msg := session.Messages[i]
		if msg.Info.Role != roleAssistant || msg.Info.Tokens == nil {
			continue
		}
		usage.InputTokens += msg.Info.Tokens.Input
		usage.OutputTokens += msg.Info.Tokens.Output
		usage.CacheReadTokens += msg.Info.Tokens.Cache.Read
		usage.CacheCreationTokens += msg.Info.Tokens.Cache.Write
		usage.APICallCount++
	}

	return usage, nil
}
