package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	agentpkg "github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
)

// Transcript message type constants - aliases to transcript package for local use.
const (
	transcriptTypeUser = transcript.TypeUser
)

// resolveTranscriptPath determines the correct file path for an agent's session transcript.
// Computes the path dynamically from the current repo location for cross-machine portability.
func resolveTranscriptPath(ctx context.Context, sessionID string, agent agentpkg.Agent) (string, error) {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get worktree root: %w", err)
	}

	sessionDir, err := agent.GetSessionDir(repoRoot)
	if err != nil {
		return "", fmt.Errorf("failed to get agent session directory: %w", err)
	}
	return agent.ResolveSessionFile(sessionDir, sessionID), nil
}

// AgentTranscriptPath returns the path to a subagent's transcript file.
// Subagent transcripts are stored as agent-{agentId}.jsonl in the same directory
// as the main transcript.
func AgentTranscriptPath(transcriptDir, agentID string) string {
	return filepath.Join(transcriptDir, fmt.Sprintf("agent-%s.jsonl", agentID))
}

// toolResultBlock represents a tool_result in a user message
type toolResultBlock struct {
	Type      string `json:"type"`
	ToolUseID string `json:"tool_use_id"`
}

// userMessageWithToolResults represents a user message that may contain tool results
type userMessageWithToolResults struct {
	Content []toolResultBlock `json:"content"`
}

// FindCheckpointUUID finds the UUID of the message containing the tool_result
// for the given tool_use_id. This is used to find the checkpoint point for
// transcript truncation when rewinding to a task.
// Returns the UUID and true if found, empty string and false otherwise.
func FindCheckpointUUID(transcript []transcriptLine, toolUseID string) (string, bool) {
	for _, line := range transcript {
		if line.Type != transcriptTypeUser {
			continue
		}

		var msg userMessageWithToolResults
		if err := json.Unmarshal(line.Message, &msg); err != nil {
			continue
		}

		for _, block := range msg.Content {
			if block.Type == "tool_result" && block.ToolUseID == toolUseID {
				return line.UUID, true
			}
		}
	}
	return "", false
}

// TruncateTranscriptAtUUID returns transcript lines up to and including the
// line with the given UUID. If the UUID is not found or is empty, returns
// the entire transcript.
//
//nolint:revive // Exported for testing purposes
func TruncateTranscriptAtUUID(transcript []transcriptLine, uuid string) []transcriptLine {
	if uuid == "" {
		return transcript
	}

	for i, line := range transcript {
		if line.UUID == uuid {
			return transcript[:i+1]
		}
	}

	// UUID not found, return full transcript
	return transcript
}

// writeTranscript writes transcript lines to a file in JSONL format.
func writeTranscript(path string, transcript []transcriptLine) error {
	file, err := os.Create(path) //nolint:gosec // Writing to controlled git metadata path
	if err != nil {
		return fmt.Errorf("failed to create file: %w", err)
	}
	defer func() { _ = file.Close() }()

	for _, line := range transcript {
		data, err := json.Marshal(line)
		if err != nil {
			return fmt.Errorf("failed to marshal line: %w", err)
		}
		if _, err := file.Write(data); err != nil {
			return fmt.Errorf("failed to write line: %w", err)
		}
		if _, err := file.WriteString("\n"); err != nil {
			return fmt.Errorf("failed to write newline: %w", err)
		}
	}

	return nil
}

// TranscriptPosition contains the position information for a transcript file.
type TranscriptPosition struct {
	LastUUID  string // Last non-empty UUID (from user/assistant messages)
	LineCount int    // Total number of lines
}

// GetTranscriptPosition reads a transcript file and returns the last UUID and line count.
// Returns empty position if file doesn't exist or is empty.
// Only considers UUIDs from actual messages (user/assistant), not summary rows which use leafUuid.
func GetTranscriptPosition(path string) (TranscriptPosition, error) {
	if path == "" {
		return TranscriptPosition{}, nil
	}

	file, err := os.Open(path) //nolint:gosec // Reading from controlled transcript path
	if err != nil {
		if os.IsNotExist(err) {
			return TranscriptPosition{}, nil
		}
		return TranscriptPosition{}, fmt.Errorf("failed to open transcript: %w", err)
	}
	defer func() { _ = file.Close() }()

	var pos TranscriptPosition
	reader := bufio.NewReader(file)

	for {
		lineBytes, err := reader.ReadBytes('\n')
		if err != nil && err != io.EOF {
			return TranscriptPosition{}, fmt.Errorf("failed to read transcript: %w", err)
		}

		if len(lineBytes) == 0 {
			if err == io.EOF {
				break
			}
			continue
		}

		pos.LineCount++

		// Parse line to extract UUID (only from user/assistant messages, not summaries)
		var line transcriptLine
		if err := json.Unmarshal(lineBytes, &line); err == nil {
			if line.UUID != "" {
				pos.LastUUID = line.UUID
			}
		}

		if err == io.EOF {
			break
		}
	}

	return pos, nil
}
