// Package summarize provides AI-powered summarization of development sessions.
package summarize

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"entire.io/cli/cmd/entire/cli/checkpoint"
	"entire.io/cli/cmd/entire/cli/transcript"
)

// GenerateFromTranscript generates a summary from raw transcript bytes.
// This is the shared implementation used by both explain --generate and auto-summarize.
//
// Parameters:
//   - ctx: context for cancellation
//   - transcriptBytes: raw transcript bytes (JSONL format)
//   - filesTouched: list of files modified during the session
//   - generator: summary generator to use (if nil, uses default ClaudeGenerator)
//
// Returns nil, error if transcript is empty or cannot be parsed.
func GenerateFromTranscript(ctx context.Context, transcriptBytes []byte, filesTouched []string, generator Generator) (*checkpoint.Summary, error) {
	if len(transcriptBytes) == 0 {
		return nil, errors.New("empty transcript")
	}

	// Build condensed transcript for summarization
	condensed, err := BuildCondensedTranscriptFromBytes(transcriptBytes)
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

	// Use default generator if none provided
	if generator == nil {
		generator = &ClaudeGenerator{}
	}

	summary, err := generator.Generate(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("failed to generate summary: %w", err)
	}

	return summary, nil
}

// Generator generates checkpoint summaries using an LLM.
type Generator interface {
	// Generate creates a summary from checkpoint data.
	// Returns the generated summary or an error if generation fails.
	Generate(ctx context.Context, input Input) (*checkpoint.Summary, error)
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

// BuildCondensedTranscriptFromBytes parses transcript bytes and extracts a condensed view.
// This is a convenience function that combines parsing and condensing.
func BuildCondensedTranscriptFromBytes(content []byte) ([]Entry, error) {
	lines, err := transcript.ParseFromBytes(content)
	if err != nil {
		return nil, fmt.Errorf("failed to parse transcript: %w", err)
	}
	return BuildCondensedTranscript(lines), nil
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
