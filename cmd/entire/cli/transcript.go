package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	agentpkg "github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
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

// searchTranscriptInProjectDirs searches for a session transcript across an agent's
// project directories that could plausibly belong to the current repository.
// Agents like Claude Code and Gemini CLI derive the project directory from the cwd,
// so the transcript may be stored under a different project directory if the session
// was started from a different working directory.
//
// The search is scoped to the agent's base directory (e.g., ~/.claude/projects) and only
// walks immediate subdirectories (plus one extra level for agents like Gemini that nest
// chats under <project>/chats/).
// Only agents implementing SessionBaseDirProvider support this fallback search.
func searchTranscriptInProjectDirs(sessionID string, ag agentpkg.Agent) (string, error) {
	provider, ok := agentpkg.AsSessionBaseDirProvider(ag)
	if !ok {
		return "", fmt.Errorf("fallback transcript search not supported for agent %q", ag.Name())
	}
	baseDir, err := provider.GetSessionBaseDir()
	if err != nil {
		return "", fmt.Errorf("failed to get base directory: %w", err)
	}

	// Walk subdirectories with a max depth of 3 (baseDir/project/subdir/file)
	// to avoid scanning unrelated project trees.
	const maxExtraDepth = 3

	var found string
	walkErr := filepath.WalkDir(baseDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // skip inaccessible dirs
		}
		if !d.IsDir() {
			return nil
		}
		// Limit walk depth using relative path from base
		rel, relErr := filepath.Rel(baseDir, path)
		if relErr != nil {
			return filepath.SkipDir
		}
		depth := strings.Count(rel, string(filepath.Separator))
		if depth > maxExtraDepth {
			return filepath.SkipDir
		}
		candidate := ag.ResolveSessionFile(path, sessionID)
		if _, statErr := os.Stat(candidate); statErr == nil {
			found = candidate
			return filepath.SkipAll
		}
		return nil
	})
	if walkErr != nil {
		return "", fmt.Errorf("failed to search project directories: %w", walkErr)
	}
	if found != "" {
		return found, nil
	}
	return "", errors.New("transcript not found in any project directory")
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
func FindCheckpointUUID(lines []transcriptLine, toolUseID string) (string, bool) {
	for _, line := range lines {
		if line.Type != transcript.TypeUser {
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
func TruncateTranscriptAtUUID(lines []transcriptLine, uuid string) []transcriptLine {
	if uuid == "" {
		return lines
	}

	for i, line := range lines {
		if line.UUID == uuid {
			return lines[:i+1]
		}
	}

	// UUID not found, return full transcript
	return lines
}

// writeTranscript writes transcript lines to a file in JSONL format.
func writeTranscript(path string, lines []transcriptLine) error {
	file, err := os.Create(path) //nolint:gosec // Writing to controlled git metadata path
	if err != nil {
		return fmt.Errorf("failed to create file: %w", err)
	}
	defer func() { _ = file.Close() }()

	for _, line := range lines {
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
