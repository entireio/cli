// Package summarise provides AI-powered summarisation of development sessions.
package summarise

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"entire.io/cli/cmd/entire/cli/checkpoint"
	"entire.io/cli/cmd/entire/cli/textutil"
)

// Generator generates checkpoint summaries using an LLM.
type Generator interface {
	// Generate creates a summary from checkpoint data.
	// Returns the generated summary or an error if generation fails.
	Generate(ctx context.Context, input Input) (*checkpoint.Summary, error)
}

// Input contains condensed checkpoint data for summarisation.
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

// TranscriptLine represents a single line in a Claude Code transcript.
// This mirrors the structure in the cli package's types.go.
type TranscriptLine struct {
	Type    string          `json:"type"`
	UUID    string          `json:"uuid"`
	Message json.RawMessage `json:"message"`
}

// Constants for transcript parsing
const (
	transcriptTypeUser      = "user"
	transcriptTypeAssistant = "assistant"
	contentTypeText         = "text"
	contentTypeToolUse      = "tool_use"
)

// userMessage represents a user message in the transcript
type userMessage struct {
	Content interface{} `json:"content"`
}

// assistantMessage represents an assistant message in the transcript
type assistantMessage struct {
	Content []contentBlock `json:"content"`
}

// contentBlock represents a block within an assistant message
type contentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

// toolInput represents the input to various tools
type toolInput struct {
	FilePath     string `json:"file_path,omitempty"`
	NotebookPath string `json:"notebook_path,omitempty"`
	Description  string `json:"description,omitempty"`
	Command      string `json:"command,omitempty"`
	Pattern      string `json:"pattern,omitempty"`
}

// BuildCondensedTranscriptFromBytes parses transcript bytes and extracts a condensed view.
// This is a convenience function that combines parsing and condensing.
func BuildCondensedTranscriptFromBytes(content []byte) ([]Entry, error) {
	lines, err := parseTranscriptFromBytes(content)
	if err != nil {
		return nil, err
	}
	return BuildCondensedTranscript(lines), nil
}

// parseTranscriptFromBytes parses transcript content from a byte slice.
// Uses bufio.Reader to handle arbitrarily long lines.
func parseTranscriptFromBytes(content []byte) ([]TranscriptLine, error) {
	var lines []TranscriptLine
	reader := bufio.NewReader(bytes.NewReader(content))

	for {
		lineBytes, err := reader.ReadBytes('\n')
		if err != nil && err != io.EOF {
			return nil, fmt.Errorf("failed to read transcript: %w", err)
		}

		// Handle empty line or EOF without content
		if len(lineBytes) == 0 {
			if err == io.EOF {
				break
			}
			continue
		}

		var line TranscriptLine
		if err := json.Unmarshal(lineBytes, &line); err == nil {
			lines = append(lines, line)
		}

		if err == io.EOF {
			break
		}
	}

	return lines, nil
}

// BuildCondensedTranscript extracts a condensed view of the transcript.
// It processes user prompts, assistant responses, and tool calls into
// a simplified format suitable for LLM summarisation.
func BuildCondensedTranscript(transcript []TranscriptLine) []Entry {
	var entries []Entry

	for _, line := range transcript {
		switch line.Type {
		case transcriptTypeUser:
			if entry := extractUserEntry(line); entry != nil {
				entries = append(entries, *entry)
			}
		case transcriptTypeAssistant:
			assistantEntries := extractAssistantEntries(line)
			entries = append(entries, assistantEntries...)
		}
	}

	return entries
}

// extractUserEntry extracts a user entry from a transcript line.
// Returns nil if the line doesn't contain a valid user prompt.
func extractUserEntry(line TranscriptLine) *Entry {
	content := extractUserContentFromMessage(line.Message)
	if content == "" {
		return nil
	}
	return &Entry{
		Type:    EntryTypeUser,
		Content: content,
	}
}

// extractUserContentFromMessage extracts user content from a raw message.
// Handles both string and array content formats.
// IDE-injected context tags (like <ide_opened_file>) are stripped from the result.
// Returns empty string if the message cannot be parsed or contains no text.
func extractUserContentFromMessage(message json.RawMessage) string {
	var msg userMessage
	if err := json.Unmarshal(message, &msg); err != nil {
		return ""
	}

	// Handle string content
	if str, ok := msg.Content.(string); ok {
		return textutil.StripIDEContextTags(str)
	}

	// Handle array content (only if it contains text blocks)
	if arr, ok := msg.Content.([]interface{}); ok {
		var texts []string
		for _, item := range arr {
			if m, ok := item.(map[string]interface{}); ok {
				if m["type"] == contentTypeText {
					if text, ok := m["text"].(string); ok {
						texts = append(texts, text)
					}
				}
			}
		}
		if len(texts) > 0 {
			return textutil.StripIDEContextTags(strings.Join(texts, "\n\n"))
		}
	}

	return ""
}

// extractAssistantEntries extracts assistant and tool entries from a transcript line.
func extractAssistantEntries(line TranscriptLine) []Entry {
	var msg assistantMessage
	if err := json.Unmarshal(line.Message, &msg); err != nil {
		return nil
	}

	var entries []Entry

	for _, block := range msg.Content {
		switch block.Type {
		case contentTypeText:
			if block.Text != "" {
				entries = append(entries, Entry{
					Type:    EntryTypeAssistant,
					Content: block.Text,
				})
			}
		case contentTypeToolUse:
			var input toolInput
			_ = json.Unmarshal(block.Input, &input) //nolint:errcheck // Best-effort parsing

			detail := input.Description
			if detail == "" {
				detail = input.Command
			}
			if detail == "" {
				detail = input.FilePath
			}
			if detail == "" {
				detail = input.NotebookPath
			}
			if detail == "" {
				detail = input.Pattern
			}

			entries = append(entries, Entry{
				Type:       EntryTypeTool,
				ToolName:   block.Name,
				ToolDetail: detail,
			})
		}
	}

	return entries
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
