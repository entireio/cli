package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

// formatExportJSON formats checkpoint data as JSON.
func formatExportJSON(checkpointID id.CheckpointID,
	content *checkpoint.SessionContent, summary *checkpoint.CheckpointSummary,
	transcript []byte, prompts, context string, filesTouched []string) ([]byte, error) {

	out := map[string]any{
		"checkpoint_id": checkpointID.String(),
		"session_id":    content.Metadata.SessionID,
		"metadata": map[string]any{
			"created_at":  content.Metadata.CreatedAt.Format(time.RFC3339),
			"strategy":    content.Metadata.Strategy,
			"agent":       content.Metadata.Agent,
			"token_usage": content.Metadata.TokenUsage,
		},
		"transcript":    string(transcript), // JSONL as string
		"prompts":       prompts,
		"context":       context,
		"files_touched": filesTouched,
		"exported_at":   time.Now().Format(time.RFC3339),
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

	return json.MarshalIndent(out, "", "  ")
}

// formatExportMarkdown formats checkpoint data as Markdown.
func formatExportMarkdown(checkpointID id.CheckpointID,
	content *checkpoint.SessionContent, summary *checkpoint.CheckpointSummary,
	transcript []byte, prompts, context string, filesTouched []string) ([]byte, error) {

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

	// Files section
	if len(filesTouched) > 0 {
		sb.WriteString("## Files Modified\n\n")
		for _, file := range filesTouched {
			fmt.Fprintf(&sb, "- `%s`\n", file)
		}
		sb.WriteString("\n")
	}

	// Transcript section - reuse existing formatting
	sb.WriteString("## Transcript\n\n")
	formattedTranscript := formatTranscriptBytes(transcript, "")
	sb.WriteString(formattedTranscript)

	return []byte(sb.String()), nil
}
