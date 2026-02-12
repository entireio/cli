package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/redact"
)

// TaskCheckpoint contains the checkpoint information for a task
type TaskCheckpoint struct {
	SessionID      string `json:"session_id"`
	ToolUseID      string `json:"tool_use_id"`
	CheckpointUUID string `json:"checkpoint_uuid"`
	AgentID        string `json:"agent_id,omitempty"`
}

// TaskMetadataDir returns the path to a task's metadata directory
// within the session metadata directory.
func TaskMetadataDir(sessionMetadataDir, toolUseID string) string {
	return filepath.Join(sessionMetadataDir, "tasks", toolUseID)
}

// WriteTaskCheckpoint writes the checkpoint.json file to the task metadata directory.
// Creates the directory if it doesn't exist.
func WriteTaskCheckpoint(taskMetadataDir string, checkpoint *TaskCheckpoint) error {
	// Create directory if it doesn't exist
	if err := os.MkdirAll(taskMetadataDir, 0o750); err != nil {
		return fmt.Errorf("failed to create task metadata directory: %w", err)
	}

	data, err := jsonutil.MarshalIndentWithNewline(checkpoint, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal checkpoint: %w", err)
	}

	checkpointFile := filepath.Join(taskMetadataDir, paths.CheckpointFileName)
	if err := os.WriteFile(checkpointFile, data, 0o600); err != nil {
		return fmt.Errorf("failed to write checkpoint file: %w", err)
	}

	return nil
}

// ReadTaskCheckpoint reads the checkpoint file from the task metadata directory.
func ReadTaskCheckpoint(taskMetadataDir string) (*TaskCheckpoint, error) {
	checkpointFile := filepath.Join(taskMetadataDir, paths.CheckpointFileName)

	data, err := os.ReadFile(checkpointFile) //nolint:gosec // Reading from controlled git metadata path
	if err != nil {
		return nil, fmt.Errorf("failed to read checkpoint file: %w", err)
	}

	var checkpoint TaskCheckpoint
	if err := json.Unmarshal(data, &checkpoint); err != nil {
		return nil, fmt.Errorf("failed to unmarshal checkpoint: %w", err)
	}

	return &checkpoint, nil
}

// WriteTaskPrompt writes the task prompt to the task metadata directory.
// Redacts secrets before writing to prevent leakage in session metadata.
func WriteTaskPrompt(taskMetadataDir, prompt string) error {
	promptFile := filepath.Join(taskMetadataDir, paths.PromptFileName)
	if err := os.WriteFile(promptFile, []byte(redact.String(prompt)), 0o600); err != nil {
		return fmt.Errorf("failed to write prompt file: %w", err)
	}
	return nil
}

// CopyAgentTranscript copies a subagent's transcript to the task metadata directory.
// Redacts secrets in the JSONL content before writing.
// If the source transcript doesn't exist, this is a no-op (not an error).
func CopyAgentTranscript(srcTranscript, taskMetadataDir, agentID string) error {
	// Check if source exists
	if _, err := os.Stat(srcTranscript); os.IsNotExist(err) {
		// Source doesn't exist, nothing to copy
		return nil
	}

	dstTranscript := filepath.Join(taskMetadataDir, fmt.Sprintf("agent-%s.jsonl", agentID))
	return copyFileRedacted(srcTranscript, dstTranscript)
}
