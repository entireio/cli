// Package summarize provides AI-powered summarization of development sessions.
package summarize

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/factoryaidroid"
	"github.com/entireio/cli/cmd/entire/cli/agent/geminicli"
	"github.com/entireio/cli/cmd/entire/cli/agent/opencode"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
	"github.com/entireio/cli/cmd/entire/cli/transcript/compact"
	"github.com/entireio/cli/redact"
)

// GenerateFromTranscript generates a summary from pre-redacted transcript bytes.
// This is the shared implementation used by both explain --generate and auto-summarize.
//
// Parameters:
//   - ctx: context for cancellation
//   - transcriptBytes: pre-redacted transcript (JSONL or JSON format depending on agent)
//   - filesTouched: list of files modified during the session
//   - agentType: the agent type to determine transcript format
//   - generator: summary generator to use (if nil, uses default ClaudeGenerator)
//
// Returns nil, error if transcript is empty or cannot be parsed.
func GenerateFromTranscript(ctx context.Context, transcriptBytes redact.RedactedBytes, filesTouched []string, agentType types.AgentType, generator Generator) (*checkpoint.Summary, error) {
	if transcriptBytes.Len() == 0 {
		return nil, errors.New("empty transcript")
	}

	// Build condensed transcript for summarization
	condensed, err := BuildCondensedTranscriptFromBytes(transcriptBytes, agentType)
	if err != nil {
		return nil, fmt.Errorf("failed to parse transcript: %w", err)
	}
	if len(condensed) == 0 {
		return nil, errors.New("transcript has no content to summarize")
	}

	input := Input{
		Transcript:   condensed,
		FilesTouched: filesTouched,
	}

	if generator == nil {
		generator = &ClaudeGenerator{}
	}

	summary, err := generator.Generate(ctx, input)
	if err != nil {
		return nil, err //nolint:wrapcheck // preserve *ClaudeError for errors.As at the explain layer
	}

	return summary, nil
}

// Generator generates checkpoint summaries using an LLM.
type Generator interface {
	// Generate creates a summary from checkpoint data.
	// Returns the generated summary or an error if generation fails.
	Generate(ctx context.Context, input Input) (*checkpoint.Summary, error)
}

// ResolveModel returns the effective model to use for summary generation.
// When the provider is Claude Code and no model is configured, it falls back
// to DefaultModel ("sonnet") — the summarize package's quality/cost choice,
// which differs from Claude Code's own invocation default ("haiku"). For
// other providers, an empty model means "use the provider CLI's own default",
// so we leave it unchanged.
func ResolveModel(name types.AgentName, model string) string {
	if name == agent.AgentNameClaudeCode && model == "" {
		return DefaultModel
	}
	return model
}

// Input contains condensed checkpoint data for summarization.
type Input struct {
	// Transcript is the condensed transcript entries
	Transcript []Entry

	// FilesTouched are the files modified during the session
	FilesTouched []string
}

// EntryType represents the type of a transcript entry.
type EntryType string

const (
	// EntryTypeUser indicates a user prompt entry.
	EntryTypeUser EntryType = "user"
	// EntryTypeAssistant indicates an assistant response entry.
	EntryTypeAssistant EntryType = "assistant"
	// EntryTypeTool indicates a tool call entry.
	EntryTypeTool EntryType = "tool"
)

// Entry represents one item in the condensed transcript.
type Entry struct {
	// Type is the entry type (user, assistant, tool)
	Type EntryType

	// Content is the text content for user/assistant entries
	Content string

	// ToolName is the name of the tool (for tool entries)
	ToolName string

	// ToolDetail is a description or file path (for tool entries)
	ToolDetail string
}

// minimalDetailTools lists tools that should show only essential details in summaries.
// These tools often have verbose outputs that don't add value to summarization.
// The detail shown is typically just a path, URL, or identifier rather than full content.
var minimalDetailTools = map[string]bool{
	"Skill":    true, // Show skill name only, not loaded content
	"Read":     true, // Show file path only, not file contents
	"WebFetch": true, // Show URL only, not fetched content
}

// BuildCondensedTranscriptFromBytes parses pre-redacted transcript bytes and extracts a condensed view.
// This is a convenience function that combines parsing and condensing.
// The agentType parameter determines which parser to use (Claude/OpenCode JSONL vs Gemini JSON).
func BuildCondensedTranscriptFromBytes(content redact.RedactedBytes, agentType types.AgentType) ([]Entry, error) {
	switch agentType {
	case agent.AgentTypeGemini:
		return buildCondensedTranscriptFromGemini(content)
	case agent.AgentTypeFactoryAIDroid:
		return buildCondensedTranscriptFromDroid(content)
	case agent.AgentTypeOpenCode:
		return buildCondensedTranscriptFromOpenCode(content)
	case agent.AgentTypeCodex:
		return buildCondensedTranscriptFromCodex(content)
	case agent.AgentTypeClaudeCode, agent.AgentTypeCursor, agent.AgentTypeUnknown:
		// Claude/cursor format - fall through to shared logic below
	}
	// Claude format (JSONL) - handles Claude Code, Unknown, and any future agent types
	lines, err := transcript.ParseFromBytes(content.Bytes())
	if err != nil {
		if compactEntries, compactErr := buildCondensedTranscriptFromCompact(content); compactErr == nil {
			return compactEntries, nil
		}
		return nil, fmt.Errorf("failed to parse transcript: %w", err)
	}
	entries := BuildCondensedTranscript(lines)
	if len(entries) > 0 {
		return entries, nil
	}
	if compactEntries, compactErr := buildCondensedTranscriptFromCompact(content); compactErr == nil {
		return compactEntries, nil
	}
	return entries, nil
}

func buildCondensedTranscriptFromCompact(redacted redact.RedactedBytes) ([]Entry, error) {
	compactEntries, err := compact.BuildCondensedEntries(redacted.Bytes())
	if err != nil {
		return nil, fmt.Errorf("failed to parse compact transcript: %w", err)
	}

	entries := make([]Entry, 0, len(compactEntries))
	for _, entry := range compactEntries {
		switch entry.Type {
		case "user":
			entries = append(entries, Entry{
				Type:    EntryTypeUser,
				Content: entry.Content,
			})
		case "assistant":
			entries = append(entries, Entry{
				Type:    EntryTypeAssistant,
				Content: entry.Content,
			})
		case "tool":
			entries = append(entries, Entry{
				Type:       EntryTypeTool,
				ToolName:   entry.ToolName,
				ToolDetail: entry.ToolDetail,
			})
		}
	}
	return entries, nil
}

// buildCondensedTranscriptFromGemini parses Gemini JSON transcript and extracts a condensed view.
func buildCondensedTranscriptFromGemini(redacted redact.RedactedBytes) ([]Entry, error) {
	geminiTranscript, err := geminicli.ParseTranscript(redacted.Bytes())
	if err != nil {
		return nil, fmt.Errorf("failed to parse Gemini transcript: %w", err)
	}

	var entries []Entry
	for _, msg := range geminiTranscript.Messages {
		switch msg.Type {
		case geminicli.MessageTypeUser:
			if msg.Content != "" {
				entries = append(entries, Entry{
					Type:    EntryTypeUser,
					Content: msg.Content,
				})
			}
		case geminicli.MessageTypeGemini:
			// Add assistant content
			if msg.Content != "" {
				entries = append(entries, Entry{
					Type:    EntryTypeAssistant,
					Content: msg.Content,
				})
			}
			// Add tool calls
			for _, tc := range msg.ToolCalls {
				entries = append(entries, Entry{
					Type:       EntryTypeTool,
					ToolName:   tc.Name,
					ToolDetail: extractGenericToolDetail(tc.Args),
				})
			}
		}
	}

	return entries, nil
}

// buildCondensedTranscriptFromOpenCode parses OpenCode export JSON transcript and extracts a condensed view.
func buildCondensedTranscriptFromOpenCode(redacted redact.RedactedBytes) ([]Entry, error) {
	session, err := opencode.ParseExportSession(redacted.Bytes())
	if err != nil {
		return nil, fmt.Errorf("failed to parse OpenCode transcript: %w", err)
	}
	if session == nil {
		return nil, nil
	}

	var entries []Entry
	for _, msg := range session.Messages {
		switch msg.Info.Role {
		case "user":
			text := opencode.ExtractTextFromParts(msg.Parts)
			if text != "" {
				entries = append(entries, Entry{
					Type:    EntryTypeUser,
					Content: text,
				})
			}
		case "assistant":
			text := opencode.ExtractTextFromParts(msg.Parts)
			if text != "" {
				entries = append(entries, Entry{
					Type:    EntryTypeAssistant,
					Content: text,
				})
			}
			for _, part := range msg.Parts {
				if part.Type == "tool" && part.State != nil {
					entries = append(entries, Entry{
						Type:       EntryTypeTool,
						ToolName:   part.Tool,
						ToolDetail: extractOpenCodeToolDetail(part.State.Input),
					})
				}
			}
		}
	}

	return entries, nil
}

// buildCondensedTranscriptFromCodex converts Codex rollout JSONL into the compact
// transcript format, then reuses the shared transcript condensation logic.
func buildCondensedTranscriptFromCodex(content redact.RedactedBytes) ([]Entry, error) {
	compacted, err := compact.Compact(content, compact.MetadataFields{
		Agent:      "codex",
		CLIVersion: "summarize",
	})
	if err != nil {
		return nil, fmt.Errorf("failed to compact Codex transcript: %w", err)
	}

	type compactUserTextBlock struct {
		Text string `json:"text"`
	}
	type compactAssistantBlock struct {
		Type  string          `json:"type"`
		Text  string          `json:"text,omitempty"`
		Name  string          `json:"name,omitempty"`
		Input json.RawMessage `json:"input,omitempty"`
	}
	type compactLine struct {
		Type    string          `json:"type"`
		Content json.RawMessage `json:"content"`
	}

	var entries []Entry
	for _, lineBytes := range splitCompactJSONL(compacted) {
		var line compactLine
		if err := json.Unmarshal(lineBytes, &line); err != nil {
			continue
		}

		switch line.Type {
		case transcript.TypeUser:
			var blocks []compactUserTextBlock
			if err := json.Unmarshal(line.Content, &blocks); err != nil {
				continue
			}
			for _, block := range blocks {
				if block.Text != "" {
					entries = append(entries, Entry{
						Type:    EntryTypeUser,
						Content: block.Text,
					})
				}
			}
		case transcript.TypeAssistant:
			var blocks []compactAssistantBlock
			if err := json.Unmarshal(line.Content, &blocks); err != nil {
				continue
			}
			for _, block := range blocks {
				switch block.Type {
				case transcript.ContentTypeText:
					if block.Text != "" {
						entries = append(entries, Entry{
							Type:    EntryTypeAssistant,
							Content: block.Text,
						})
					}
				case transcript.ContentTypeToolUse:
					var input map[string]interface{}
					if err := json.Unmarshal(block.Input, &input); err != nil {
						input = nil
					}
					entries = append(entries, Entry{
						Type:       EntryTypeTool,
						ToolName:   block.Name,
						ToolDetail: extractGenericToolDetail(input),
					})
				}
			}
		}
	}

	return entries, nil
}

// buildCondensedTranscriptFromDroid parses Droid transcript and extracts a condensed view.
func buildCondensedTranscriptFromDroid(redacted redact.RedactedBytes) ([]Entry, error) {
	droidLines, _, err := factoryaidroid.ParseDroidTranscriptFromBytes(redacted.Bytes(), 0)
	if err != nil {
		return nil, fmt.Errorf("failed to parse Droid transcript: %w", err)
	}
	return BuildCondensedTranscript(droidLines), nil
}

// extractOpenCodeToolDetail extracts a detail string from an OpenCode tool's input map.
// OpenCode uses camelCase keys (e.g., "filePath" instead of "file_path").
func extractOpenCodeToolDetail(input map[string]interface{}) string {
	for _, key := range []string{"description", "command", "filePath", "path", "pattern"} {
		if v, ok := input[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// extractGenericToolDetail extracts an appropriate detail string from a tool's input/args map.
// Checks common fields in order of preference.
func extractGenericToolDetail(input map[string]interface{}) string {
	if input == nil {
		return ""
	}
	for _, key := range []string{"description", "command", "cmd", "file_path", "path", "pattern"} {
		if v, ok := input[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func splitCompactJSONL(data []byte) [][]byte {
	var lines [][]byte
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) > 0 {
			lines = append(lines, line)
		}
	}
	return lines
}

// BuildCondensedTranscript extracts a condensed view of the transcript.
// It processes user prompts, assistant responses, and tool calls into
// a simplified format suitable for LLM summarization.
func BuildCondensedTranscript(lines []transcript.Line) []Entry {
	var entries []Entry

	for _, line := range lines {
		switch line.Type {
		case transcript.TypeUser:
			if entry := extractUserEntry(line); entry != nil {
				entries = append(entries, *entry)
			}
		case transcript.TypeAssistant:
			assistantEntries := extractAssistantEntries(line)
			entries = append(entries, assistantEntries...)
		}
	}

	return entries
}

// skillContentPrefix identifies user messages that are skill content injections.
// These are injected after a Skill tool call and contain the full skill instructions.
const skillContentPrefix = "Base directory for this skill:"

// extractUserEntry extracts a user entry from a transcript line.
// Returns nil if the line doesn't contain a valid user prompt or is skill content.
func extractUserEntry(line transcript.Line) *Entry {
	content := transcript.ExtractUserContent(line.Message)
	if content == "" {
		return nil
	}

	// Skip skill content injections - these are verbose skill instructions
	// injected as user messages after Skill tool invocations in Claude Code.
	// The prefix "Base directory for this skill:" is added by the superpowers
	// plugin when loading skill content. This filtering reduces transcript noise
	// since skill content is documentation, not user intent.
	if strings.HasPrefix(content, skillContentPrefix) {
		return nil
	}

	return &Entry{
		Type:    EntryTypeUser,
		Content: content,
	}
}

// extractAssistantEntries extracts assistant and tool entries from a transcript line.
func extractAssistantEntries(line transcript.Line) []Entry {
	var msg transcript.AssistantMessage
	if err := json.Unmarshal(line.Message, &msg); err != nil {
		return nil
	}

	var entries []Entry

	for _, block := range msg.Content {
		switch block.Type {
		case transcript.ContentTypeText:
			if block.Text != "" {
				entries = append(entries, Entry{
					Type:    EntryTypeAssistant,
					Content: block.Text,
				})
			}
		case transcript.ContentTypeToolUse:
			var input transcript.ToolInput
			_ = json.Unmarshal(block.Input, &input) //nolint:errcheck // Best-effort parsing

			detail := extractToolDetail(block.Name, input)

			entries = append(entries, Entry{
				Type:       EntryTypeTool,
				ToolName:   block.Name,
				ToolDetail: detail,
			})
		}
	}

	return entries
}

// extractToolDetail extracts an appropriate detail string for a tool call.
// For tools in minimalDetailTools, only essential identifiers are shown.
// For other tools, the full detail chain is used.
func extractToolDetail(toolName string, input transcript.ToolInput) string {
	// For minimal detail tools, extract only the essential identifier
	if minimalDetailTools[toolName] {
		switch toolName {
		case "Skill":
			return input.Skill
		case "Read":
			if input.FilePath != "" {
				return input.FilePath
			}
			return input.NotebookPath
		case "WebFetch":
			return input.URL
		}
	}

	// For other tools, use the full detail chain
	if input.Description != "" {
		return input.Description
	}
	if input.Command != "" {
		return input.Command
	}
	if input.FilePath != "" {
		return input.FilePath
	}
	if input.NotebookPath != "" {
		return input.NotebookPath
	}
	return input.Pattern
}

// FormatCondensedTranscript formats an Input into a human-readable string for LLM.
// The format is:
//
//	[User] user prompt here
//
//	[Assistant] assistant response here
//
//	[Tool] ToolName: description or file path
func FormatCondensedTranscript(input Input) string {
	var sb strings.Builder

	for i, entry := range input.Transcript {
		if i > 0 {
			sb.WriteString("\n")
		}

		switch entry.Type {
		case EntryTypeUser:
			sb.WriteString("[User] ")
			sb.WriteString(entry.Content)
			sb.WriteString("\n")
		case EntryTypeAssistant:
			sb.WriteString("[Assistant] ")
			sb.WriteString(entry.Content)
			sb.WriteString("\n")
		case EntryTypeTool:
			sb.WriteString("[Tool] ")
			sb.WriteString(entry.ToolName)
			if entry.ToolDetail != "" {
				sb.WriteString(": ")
				sb.WriteString(entry.ToolDetail)
			}
			sb.WriteString("\n")
		}
	}

	if len(input.FilesTouched) > 0 {
		sb.WriteString("\n[Files Modified]\n")
		for _, file := range input.FilesTouched {
			fmt.Fprintf(&sb, "- %s\n", file)
		}
	}

	return sb.String()
}
