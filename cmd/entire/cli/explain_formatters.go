package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

// formatExportJSON formats checkpoint data as JSON.
func formatExportJSON(checkpointID id.CheckpointID,
	content *checkpoint.SessionContent, _ *checkpoint.CheckpointSummary,
	transcript []byte, prompts, context string, filesTouched []string, opts exportOptions) ([]byte, error) {
	out := map[string]any{
		"checkpoint_id": checkpointID.String(),
		"session_id":    content.Metadata.SessionID,
		"metadata": map[string]any{
			"created_at":  content.Metadata.CreatedAt.Format(time.RFC3339),
			"strategy":    content.Metadata.Strategy,
			"agent":       content.Metadata.Agent,
			"token_usage": content.Metadata.TokenUsage,
		},
		"files_touched": filesTouched,
		"exported_at":   time.Now().Format(time.RFC3339),
	}

	// Conditionally include content based on options
	if !opts.NoTranscript {
		out["transcript"] = string(transcript) // JSONL as string
	}
	if !opts.NoPrompts {
		out["prompts"] = prompts
	}
	if !opts.NoContext {
		out["context"] = context
	}

	// Extract and include tool calls if requested
	if opts.IncludeToolCalls && len(transcript) > 0 {
		toolCalls := extractToolCalls(transcript)
		if len(toolCalls) > 0 {
			out["tool_calls"] = toolCalls
		}
	}

	// Include file diffs if requested
	if opts.IncludeFileDiffs {
		// TODO: Implement file diff extraction
		out["file_diffs"] = []string{} // Placeholder
	}

	// Include summary if available
	if content.Metadata.Summary != nil {
		out["summary"] = map[string]any{
			"intent":     content.Metadata.Summary.Intent,
			"outcome":    content.Metadata.Summary.Outcome,
			"learnings":  content.Metadata.Summary.Learnings,
			"friction":   content.Metadata.Summary.Friction,
			"open_items": content.Metadata.Summary.OpenItems,
		}
	}

	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("failed to marshal JSON: %w", err)
	}
	return data, nil
}

// formatExportMarkdown formats checkpoint data as Markdown.
//
//nolint:unparam // error return kept for consistency with formatExportJSON
func formatExportMarkdown(checkpointID id.CheckpointID,
	content *checkpoint.SessionContent, _ *checkpoint.CheckpointSummary,
	transcript []byte, prompts, context string, filesTouched []string, opts exportOptions) ([]byte, error) {
	var sb strings.Builder

	// Header
	fmt.Fprintf(&sb, "# Session: %s\n\n", content.Metadata.SessionID)
	fmt.Fprintf(&sb, "**Checkpoint:** `%s`\n\n", checkpointID.String())
	fmt.Fprintf(&sb, "**Created:** %s\n\n", content.Metadata.CreatedAt.Format("2006-01-02 15:04:05"))

	// Summary section (if available)
	if content.Metadata.Summary != nil {
		sb.WriteString("## Summary\n\n")
		fmt.Fprintf(&sb, "**Intent:** %s\n\n", content.Metadata.Summary.Intent)
		fmt.Fprintf(&sb, "**Outcome:** %s\n\n", content.Metadata.Summary.Outcome)

		if len(content.Metadata.Summary.Learnings.Code) > 0 {
			sb.WriteString("**Key Learnings:**\n\n")
			for _, l := range content.Metadata.Summary.Learnings.Code {
				fmt.Fprintf(&sb, "- %s: %s\n", l.Path, l.Finding)
			}
			sb.WriteString("\n")
		}
	}

	// Prompts section (if not excluded)
	if !opts.NoPrompts && prompts != "" {
		sb.WriteString("## Prompts\n\n")
		sb.WriteString(prompts)
		sb.WriteString("\n\n")
	}

	// Context section (if not excluded)
	if !opts.NoContext && context != "" {
		sb.WriteString("## Context\n\n")
		sb.WriteString(context)
		sb.WriteString("\n\n")
	}

	// Files section
	if len(filesTouched) > 0 {
		sb.WriteString("## Files Modified\n\n")
		for _, file := range filesTouched {
			fmt.Fprintf(&sb, "- `%s`\n", file)
		}
		sb.WriteString("\n")
	}

	// Transcript section (if not excluded)
	if !opts.NoTranscript && len(transcript) > 0 {
		sb.WriteString("## Transcript\n\n")
		formattedTranscript := formatTranscriptBytes(transcript, "")
		sb.WriteString(formattedTranscript)
	}

	// Tool calls section (if requested)
	if opts.IncludeToolCalls && len(transcript) > 0 {
		toolCalls := extractToolCalls(transcript)
		if len(toolCalls) > 0 {
			sb.WriteString("\n## Tool Calls\n\n")
			for i, tc := range toolCalls {
				fmt.Fprintf(&sb, "### Call %d: %s\n\n", i+1, tc.Name)
				if tc.Input != "" {
					fmt.Fprintf(&sb, "**Input:**\n```json\n%s\n```\n\n", tc.Input)
				}
			}
		}
	}

	return []byte(sb.String()), nil
}

// formatExportMultipleJSON formats multiple checkpoints as a JSON array.
func formatExportMultipleJSON(checkpoints []exportedCheckpointData, opts exportOptions) ([]byte, error) {
	var exportArray []map[string]any

	for _, cp := range checkpoints {
		out := map[string]any{
			"checkpoint_id": cp.CheckpointID.String(),
			"session_id":    cp.Content.Metadata.SessionID,
			"metadata": map[string]any{
				"created_at":  cp.Content.Metadata.CreatedAt.Format(time.RFC3339),
				"strategy":    cp.Content.Metadata.Strategy,
				"agent":       cp.Content.Metadata.Agent,
				"token_usage": cp.Content.Metadata.TokenUsage,
			},
			"files_touched": cp.FilesTouched,
		}

		// Conditionally include content based on options
		if !opts.NoTranscript {
			out["transcript"] = string(cp.Transcript)
		}
		if !opts.NoPrompts {
			out["prompts"] = cp.Prompts
		}
		if !opts.NoContext {
			out["context"] = cp.Context
		}

		// Extract and include tool calls if requested
		if opts.IncludeToolCalls && len(cp.Transcript) > 0 {
			toolCalls := extractToolCalls(cp.Transcript)
			if len(toolCalls) > 0 {
				out["tool_calls"] = toolCalls
			}
		}

		// Include file diffs if requested
		if opts.IncludeFileDiffs {
			out["file_diffs"] = []string{} // Placeholder
		}

		// Include summary if available
		if cp.Content.Metadata.Summary != nil {
			out["summary"] = map[string]any{
				"intent":     cp.Content.Metadata.Summary.Intent,
				"outcome":    cp.Content.Metadata.Summary.Outcome,
				"learnings":  cp.Content.Metadata.Summary.Learnings,
				"friction":   cp.Content.Metadata.Summary.Friction,
				"open_items": cp.Content.Metadata.Summary.OpenItems,
			}
		}

		exportArray = append(exportArray, out)
	}

	// Wrap in an object with metadata
	output := map[string]any{
		"checkpoints": exportArray,
		"count":       len(checkpoints),
		"exported_at": time.Now().Format(time.RFC3339),
	}

	data, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("failed to marshal JSON: %w", err)
	}
	return data, nil
}

// formatExportMultipleMarkdown formats multiple checkpoints as Markdown with sections.
//
//nolint:unparam // error return kept for consistency with formatExportMultipleJSON
func formatExportMultipleMarkdown(checkpoints []exportedCheckpointData, opts exportOptions) ([]byte, error) {
	var sb strings.Builder

	// Header
	fmt.Fprintf(&sb, "# Entire Sessions Export\n\n")
	fmt.Fprintf(&sb, "**Total Checkpoints:** %d\n\n", len(checkpoints))
	fmt.Fprintf(&sb, "**Exported:** %s\n\n", time.Now().Format("2006-01-02 15:04:05"))
	sb.WriteString("---\n\n")

	// Export each checkpoint as a section
	for i, cp := range checkpoints {
		fmt.Fprintf(&sb, "## Checkpoint %d: %s\n\n", i+1, cp.CheckpointID.String())
		fmt.Fprintf(&sb, "**Session:** %s\n\n", cp.Content.Metadata.SessionID)
		fmt.Fprintf(&sb, "**Created:** %s\n\n", cp.Content.Metadata.CreatedAt.Format("2006-01-02 15:04:05"))

		// Summary section (if available)
		if cp.Content.Metadata.Summary != nil {
			sb.WriteString("### Summary\n\n")
			fmt.Fprintf(&sb, "**Intent:** %s\n\n", cp.Content.Metadata.Summary.Intent)
			fmt.Fprintf(&sb, "**Outcome:** %s\n\n", cp.Content.Metadata.Summary.Outcome)

			if len(cp.Content.Metadata.Summary.Learnings.Code) > 0 {
				sb.WriteString("**Key Learnings:**\n\n")
				for _, l := range cp.Content.Metadata.Summary.Learnings.Code {
					fmt.Fprintf(&sb, "- %s: %s\n", l.Path, l.Finding)
				}
				sb.WriteString("\n")
			}
		}

		// Prompts section (if not excluded)
		if !opts.NoPrompts && cp.Prompts != "" {
			sb.WriteString("### Prompts\n\n")
			sb.WriteString(cp.Prompts)
			sb.WriteString("\n\n")
		}

		// Context section (if not excluded)
		if !opts.NoContext && cp.Context != "" {
			sb.WriteString("### Context\n\n")
			sb.WriteString(cp.Context)
			sb.WriteString("\n\n")
		}

		// Files section
		if len(cp.FilesTouched) > 0 {
			sb.WriteString("### Files Modified\n\n")
			for _, file := range cp.FilesTouched {
				fmt.Fprintf(&sb, "- `%s`\n", file)
			}
			sb.WriteString("\n")
		}

		// Transcript section (if not excluded)
		if !opts.NoTranscript && len(cp.Transcript) > 0 {
			sb.WriteString("### Transcript\n\n")
			formattedTranscript := formatTranscriptBytes(cp.Transcript, "")
			sb.WriteString(formattedTranscript)
		}

		// Tool calls section (if requested)
		if opts.IncludeToolCalls && len(cp.Transcript) > 0 {
			toolCalls := extractToolCalls(cp.Transcript)
			if len(toolCalls) > 0 {
				sb.WriteString("\n### Tool Calls\n\n")
				for j, tc := range toolCalls {
					fmt.Fprintf(&sb, "#### Call %d: %s\n\n", j+1, tc.Name)
					if tc.Input != "" {
						fmt.Fprintf(&sb, "**Input:**\n```json\n%s\n```\n\n", tc.Input)
					}
				}
			}
		}

		// Separator between checkpoints (except for the last one)
		if i < len(checkpoints)-1 {
			sb.WriteString("\n---\n\n")
		}
	}

	return []byte(sb.String()), nil
}

// toolCall represents a single tool call extracted from a transcript.
type toolCall struct {
	Name  string `json:"name"`
	Input string `json:"input"`
}

// extractToolCalls extracts tool calls from a JSONL transcript.
func extractToolCalls(transcript []byte) []toolCall {
	var toolCalls []toolCall

	// Parse JSONL line by line
	scanner := bufio.NewScanner(bytes.NewReader(transcript))
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		// Parse JSON
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}

		// Check if this is a tool use entry
		typeVal, hasType := entry["type"]
		if !hasType || typeVal != "tool_use" {
			continue
		}

		// Extract tool name and input
		name, _ := entry["name"].(string)
		input, _ := entry["input"].(map[string]any)

		// Serialize input to JSON
		var inputJSON string
		if input != nil {
			inputBytes, err := json.MarshalIndent(input, "", "  ")
			if err == nil {
				inputJSON = string(inputBytes)
			}
		}

		toolCalls = append(toolCalls, toolCall{
			Name:  name,
			Input: inputJSON,
		})
	}

	return toolCalls
}
