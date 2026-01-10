// hooks_opencode_transcript.go contains OpenCode transcript parsing logic.
package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// opencodeTranscriptLine represents a single line in an OpenCode JSONL transcript.
type opencodeTranscriptLine struct {
	Info struct {
		ID        string `json:"id"`
		SessionID string `json:"sessionID"`
		Role      string `json:"role"` // "user" or "assistant"
		Time      struct {
			Created   int64 `json:"created"`
			Completed int64 `json:"completed,omitempty"`
		} `json:"time"`
		Summary *struct {
			Title string `json:"title"`
			Diffs []struct {
				File      string `json:"file"`
				Before    string `json:"before"`
				After     string `json:"after"`
				Additions int    `json:"additions"`
				Deletions int    `json:"deletions"`
			} `json:"diffs"`
		} `json:"summary,omitempty"`
	} `json:"info"`
	Parts []struct {
		ID       string `json:"id"`
		Type     string `json:"type"` // "text", "tool", "step-start", "step-finish", "patch"
		Text     string `json:"text,omitempty"`
		Tool     string `json:"tool,omitempty"`
		CallID   string `json:"callID,omitempty"`
		Snapshot string `json:"snapshot,omitempty"`
		FilePath string `json:"filePath,omitempty"`
		State    *struct {
			Status string         `json:"status"`
			Input  map[string]any `json:"input,omitempty"`
			Output string         `json:"output,omitempty"`
			Title  string         `json:"title,omitempty"`
		} `json:"state,omitempty"`
	} `json:"parts"`
}

// parseOpencodeTranscript parses an OpenCode JSONL transcript file.
func parseOpencodeTranscript(transcriptPath string) ([]opencodeTranscriptLine, error) {
	file, err := os.Open(transcriptPath) //nolint:gosec // transcriptPath from trusted source
	if err != nil {
		return nil, fmt.Errorf("failed to open transcript: %w", err)
	}
	defer func() { _ = file.Close() }()

	var lines []opencodeTranscriptLine
	scanner := bufio.NewScanner(file)
	// Use large buffer for very long lines (transcript lines can be huge)
	scanner.Buffer(make([]byte, 0, ScannerBufferSize), ScannerBufferSize)

	lineNum := 0
	for scanner.Scan() {
		lineNum++
		data := scanner.Bytes()
		if len(data) == 0 {
			continue
		}

		var line opencodeTranscriptLine
		if err := json.Unmarshal(data, &line); err != nil {
			// Log parse error but continue (skip malformed lines)
			fmt.Fprintf(os.Stderr, "Warning: failed to parse transcript line %d: %v\n", lineNum, err)
			continue
		}

		lines = append(lines, line)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading transcript: %w", err)
	}

	return lines, nil
}

// extractOpencodeUserPrompts extracts all user prompts from the transcript.
func extractOpencodeUserPrompts(lines []opencodeTranscriptLine) []string {
	var prompts []string

	for _, line := range lines {
		if line.Info.Role == transcriptTypeUser {
			// Extract text from all text parts
			for _, part := range line.Parts {
				if part.Type == contentTypeText && part.Text != "" {
					prompts = append(prompts, strings.TrimSpace(part.Text))
				}
			}
		}
	}

	return prompts
}

// extractOpencodeModifiedFiles extracts modified files from the transcript.
// Returns lists of modified, new, and deleted files.
// Note: Paths may be absolute in OpenCode transcripts, caller should normalize them.
func extractOpencodeModifiedFiles(lines []opencodeTranscriptLine) (modified, added, deleted []string) {
	// Track unique files using maps
	modifiedMap := make(map[string]bool)
	addedMap := make(map[string]bool)
	deletedMap := make(map[string]bool)

	for _, line := range lines {
		// Check summary diffs (present in user messages summarizing the changes)
		if line.Info.Summary != nil && len(line.Info.Summary.Diffs) > 0 {
			for _, diff := range line.Info.Summary.Diffs {
				if diff.File == "" {
					continue
				}

				// Just use the file path as-is, caller will normalize
				file := diff.File

				switch {
				case diff.Before == "" && diff.After != "":
					// New file
					addedMap[file] = true
				case diff.Before != "" && diff.After == "":
					// Deleted file
					deletedMap[file] = true
				case diff.Before != "" && diff.After != "":
					// Modified file
					modifiedMap[file] = true
				}
			}
		}

		// Also check tool calls for file operations
		for _, part := range line.Parts {
			if part.Type == "tool" && part.State != nil && part.State.Input != nil {
				// Extract file path from tool input
				if filePath, ok := part.State.Input["filePath"].(string); ok && filePath != "" {
					// Normalize path
					file := strings.TrimPrefix(filePath, "/")

					// Determine operation based on tool name
					switch part.Tool {
					case "write":
						// Could be new or modified - check if exists in deleted (indicates modification)
						if deletedMap[file] {
							modifiedMap[file] = true
							delete(deletedMap, file)
						} else if !modifiedMap[file] {
							// Assume new file unless we know otherwise
							addedMap[file] = true
						}
					case "edit":
						modifiedMap[file] = true
						// Remove from new if it was there
						delete(addedMap, file)
					}
				}
			}
		}
	}

	// Convert maps to slices
	for file := range modifiedMap {
		modified = append(modified, file)
	}
	for file := range addedMap {
		// Only include if not also in modified
		if !modifiedMap[file] {
			added = append(added, file)
		}
	}
	for file := range deletedMap {
		deleted = append(deleted, file)
	}

	return modified, added, deleted
}

// extractOpencodeSessionTitle extracts the session title from the first user message.
// OpenCode generates a nice summary title from the user's prompt.
func extractOpencodeSessionTitle(lines []opencodeTranscriptLine) string {
	for _, line := range lines {
		if line.Info.Role == transcriptTypeUser && line.Info.Summary != nil {
			if title := line.Info.Summary.Title; title != "" {
				return title
			}
		}
	}
	return ""
}

// generateOpencodeContext generates a context summary from the transcript.
func generateOpencodeContext(lines []opencodeTranscriptLine) string {
	var sb strings.Builder

	sb.WriteString("# OpenCode Session Context\n\n")

	// Extract user prompts and assistant responses
	for i, line := range lines {
		switch line.Info.Role {
		case transcriptTypeUser:
			sb.WriteString(fmt.Sprintf("## Prompt %d\n\n", i+1))
			for _, part := range line.Parts {
				if part.Type == contentTypeText && part.Text != "" {
					sb.WriteString(part.Text)
					sb.WriteString("\n\n")
				}
			}

			// Show file changes if available
			if line.Info.Summary != nil && len(line.Info.Summary.Diffs) > 0 {
				sb.WriteString("**Changes:**\n")
				for _, diff := range line.Info.Summary.Diffs {
					if diff.Additions > 0 || diff.Deletions > 0 {
						sb.WriteString(fmt.Sprintf("- `%s`: +%d -%d lines\n", diff.File, diff.Additions, diff.Deletions))
					}
				}
				sb.WriteString("\n")
			}
		case transcriptTypeAssistant:
			// Extract assistant text responses
			for _, part := range line.Parts {
				if part.Type == contentTypeText && part.Text != "" && strings.TrimSpace(part.Text) != "" {
					sb.WriteString("**Assistant:** ")
					sb.WriteString(strings.TrimSpace(part.Text))
					sb.WriteString("\n\n")
				}
			}
		}
	}

	return sb.String()
}
