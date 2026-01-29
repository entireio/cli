package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"entire.io/cli/cmd/entire/cli/jsonutil"
	"entire.io/cli/cmd/entire/cli/paths"
	"entire.io/cli/cmd/entire/cli/strategy"

	"github.com/go-git/go-git/v5"
)

// PrePromptState stores the state captured before a user prompt
type PrePromptState struct {
	SessionID      string   `json:"session_id"`
	Timestamp      string   `json:"timestamp"`
	UntrackedFiles []string `json:"untracked_files"`

	// StartMessageIndex is the message count in the transcript when this state
	// was captured. Used for calculating token usage since the prompt started.
	// Only set for Gemini sessions. Zero means not set or session just started.
	StartMessageIndex int `json:"start_message_index,omitempty"`

	// Transcript position at prompt start - tracks what was added during this checkpoint
	// Used for Claude Code sessions.
	LastTranscriptIdentifier string `json:"last_transcript_identifier,omitempty"` // Last identifier when prompt started (UUID for Claude, message ID for Gemini)
	LastTranscriptLineCount  int    `json:"last_transcript_line_count,omitempty"` // Line count when prompt started
}

// CapturePrePromptState captures current untracked files and transcript position before a prompt
// and saves them to a state file.
// Works correctly from any subdirectory within the repository.
// The transcriptPath parameter is optional - if empty, transcript position won't be captured.
func CapturePrePromptState(sessionID, transcriptPath string) error {
	if sessionID == "" {
		sessionID = unknownSessionID
	}

	// Get absolute path for tmp directory
	tmpDirAbs, err := paths.AbsPath(paths.EntireTmpDir)
	if err != nil {
		tmpDirAbs = paths.EntireTmpDir // Fallback to relative
	}

	// Create tmp directory if it doesn't exist
	if err := os.MkdirAll(tmpDirAbs, 0o750); err != nil {
		return fmt.Errorf("failed to create tmp directory: %w", err)
	}

	// Get list of untracked files (excluding .entire directory itself)
	untrackedFiles, err := getUntrackedFilesForState()
	if err != nil {
		return fmt.Errorf("failed to get untracked files: %w", err)
	}

	// Get transcript position (last UUID and line count)
	var transcriptPos TranscriptPosition
	if transcriptPath != "" {
		transcriptPos, err = GetTranscriptPosition(transcriptPath)
		if err != nil {
			// Log warning but don't fail - transcript position is optional
			fmt.Fprintf(os.Stderr, "Warning: failed to get transcript position: %v\n", err)
		}
	}

	// Create state file
	stateFile := prePromptStateFile(sessionID)
	state := PrePromptState{
		SessionID:                sessionID,
		Timestamp:                time.Now().UTC().Format(time.RFC3339),
		UntrackedFiles:           untrackedFiles,
		LastTranscriptIdentifier: transcriptPos.LastUUID,
		LastTranscriptLineCount:  transcriptPos.LineCount,
	}

	data, err := jsonutil.MarshalIndentWithNewline(state, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal state: %w", err)
	}

	if err := os.WriteFile(stateFile, data, 0o600); err != nil {
		return fmt.Errorf("failed to write state file: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Captured state before prompt: %d untracked files, transcript at line %d (uuid: %s)\n",
		len(untrackedFiles), transcriptPos.LineCount, transcriptPos.LastUUID)
	return nil
}

// CaptureGeminiPrePromptState captures current untracked files and transcript position
// before a prompt for Gemini sessions. This is called by the BeforeAgent hook.
// The transcriptPath is the path to the Gemini session transcript (JSON format).
func CaptureGeminiPrePromptState(sessionID, transcriptPath string) error {
	if sessionID == "" {
		sessionID = unknownSessionID
	}

	// Get absolute path for tmp directory
	tmpDirAbs, err := paths.AbsPath(paths.EntireTmpDir)
	if err != nil {
		tmpDirAbs = paths.EntireTmpDir // Fallback to relative
	}

	// Create tmp directory if it doesn't exist
	if err := os.MkdirAll(tmpDirAbs, 0o750); err != nil {
		return fmt.Errorf("failed to create tmp directory: %w", err)
	}

	// Get list of untracked files (excluding .entire directory itself)
	untrackedFiles, err := getUntrackedFilesForState()
	if err != nil {
		return fmt.Errorf("failed to get untracked files: %w", err)
	}

	// Get transcript position (message count and last message ID) for Gemini
	var startMessageIndex int
	var lastMessageID string
	if transcriptPath != "" {
		// Read transcript and extract both message count and last message ID
		if data, readErr := os.ReadFile(transcriptPath); readErr == nil && len(data) > 0 { //nolint:gosec // Reading from controlled transcript path
			var transcript struct {
				Messages []struct {
					ID string `json:"id"`
				} `json:"messages"`
			}
			if jsonErr := json.Unmarshal(data, &transcript); jsonErr == nil {
				startMessageIndex = len(transcript.Messages)
				if startMessageIndex > 0 {
					lastMessageID = transcript.Messages[startMessageIndex-1].ID
				}
			} else {
				fmt.Fprintf(os.Stderr, "Warning: failed to parse transcript for message tracking: %v\n", jsonErr)
			}
		}
	}

	// Create state file
	stateFile := prePromptStateFile(sessionID)
	state := PrePromptState{
		SessionID:                sessionID,
		Timestamp:                time.Now().UTC().Format(time.RFC3339),
		UntrackedFiles:           untrackedFiles,
		StartMessageIndex:        startMessageIndex,
		LastTranscriptIdentifier: lastMessageID,
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal state: %w", err)
	}

	if err := os.WriteFile(stateFile, data, 0o600); err != nil {
		return fmt.Errorf("failed to write state file: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Captured Gemini state before prompt: %d untracked files, transcript position: %d (last msg id: %s)\n", len(untrackedFiles), startMessageIndex, lastMessageID)
	return nil
}

// LoadPrePromptState loads previously captured state.
// Returns nil if no state file exists.
func LoadPrePromptState(sessionID string) (*PrePromptState, error) {
	stateFile := prePromptStateFile(sessionID)

	if !fileExists(stateFile) {
		return nil, nil
	}

	data, err := os.ReadFile(stateFile) //nolint:gosec // Reading from controlled git metadata path
	if err != nil {
		return nil, fmt.Errorf("failed to read state file: %w", err)
	}

	var state PrePromptState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("failed to unmarshal state: %w", err)
	}

	return &state, nil
}

// CleanupPrePromptState removes the state file after use
func CleanupPrePromptState(sessionID string) error {
	stateFile := prePromptStateFile(sessionID)
	if fileExists(stateFile) {
		return os.Remove(stateFile)
	}
	return nil
}

// ComputeNewFiles compares current untracked files with pre-prompt state
// to find files that were created during the session.
func ComputeNewFiles(preState *PrePromptState) ([]string, error) {
	if preState == nil {
		return nil, nil
	}

	currentUntracked, err := getUntrackedFilesForState()
	if err != nil {
		return nil, err
	}

	return findNewUntrackedFiles(currentUntracked, preState.UntrackedFiles), nil
}

// ComputeDeletedFiles returns files that were deleted during the session
// (tracked files that no longer exist).
func ComputeDeletedFiles() ([]string, error) {
	repo, err := openRepository()
	if err != nil {
		return nil, err
	}

	worktree, err := repo.Worktree()
	if err != nil {
		return nil, err
	}

	status, err := worktree.Status()
	if err != nil {
		return nil, err
	}

	var deletedFiles []string
	for file, st := range status {
		// Skip .entire directory
		if paths.IsInfrastructurePath(file) {
			continue
		}

		// Find deleted files that haven't been staged yet
		if st.Worktree == git.Deleted && st.Staging != git.Deleted {
			deletedFiles = append(deletedFiles, file)
		}
	}

	return deletedFiles, nil
}

// FilterAndNormalizePaths converts absolute paths to relative and filters out
// infrastructure paths and paths outside the repo.
func FilterAndNormalizePaths(files []string, cwd string) []string {
	var result []string
	for _, file := range files {
		relPath := paths.ToRelativePath(file, cwd)
		if relPath == "" {
			continue // outside repo
		}
		if paths.IsInfrastructurePath(relPath) {
			continue // skip .entire directory
		}
		result = append(result, relPath)
	}
	return result
}

// prePromptStateFile returns the absolute path to the pre-prompt state file for a session.
// Works correctly from any subdirectory within the repository.
func prePromptStateFile(sessionID string) string {
	tmpDirAbs, err := paths.AbsPath(paths.EntireTmpDir)
	if err != nil {
		tmpDirAbs = paths.EntireTmpDir // Fallback to relative
	}
	return filepath.Join(tmpDirAbs, fmt.Sprintf("pre-prompt-%s.json", sessionID))
}

// getUntrackedFilesForState returns a list of untracked files using go-git
// Excludes .entire directory
func getUntrackedFilesForState() ([]string, error) {
	repo, err := openRepository()
	if err != nil {
		return nil, err
	}

	worktree, err := repo.Worktree()
	if err != nil {
		return nil, err
	}

	status, err := worktree.Status()
	if err != nil {
		return nil, err
	}

	var untrackedFiles []string
	for file, st := range status {
		if st.Worktree == git.Untracked {
			// Exclude .entire directory
			if !strings.HasPrefix(file, paths.EntireDir+"/") && file != paths.EntireDir {
				untrackedFiles = append(untrackedFiles, file)
			}
		}
	}

	return untrackedFiles, nil
}

// PreTaskState stores the state captured before a task execution
type PreTaskState struct {
	ToolUseID      string   `json:"tool_use_id"`
	Timestamp      string   `json:"timestamp"`
	UntrackedFiles []string `json:"untracked_files"`
}

// CapturePreTaskState captures current untracked files before a Task execution
// and saves them to a state file.
// Works correctly from any subdirectory within the repository.
func CapturePreTaskState(toolUseID string) error {
	if toolUseID == "" {
		return errors.New("tool_use_id is required")
	}

	// Get absolute path for tmp directory
	tmpDirAbs, err := paths.AbsPath(paths.EntireTmpDir)
	if err != nil {
		tmpDirAbs = paths.EntireTmpDir // Fallback to relative
	}

	// Create tmp directory if it doesn't exist
	if err := os.MkdirAll(tmpDirAbs, 0o750); err != nil {
		return fmt.Errorf("failed to create tmp directory: %w", err)
	}

	// Get list of untracked files (excluding .entire directory itself)
	untrackedFiles, err := getUntrackedFilesForState()
	if err != nil {
		return fmt.Errorf("failed to get untracked files: %w", err)
	}

	// Create state file
	stateFile := preTaskStateFile(toolUseID)
	state := PreTaskState{
		ToolUseID:      toolUseID,
		Timestamp:      time.Now().UTC().Format(time.RFC3339),
		UntrackedFiles: untrackedFiles,
	}

	data, err := jsonutil.MarshalIndentWithNewline(state, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal state: %w", err)
	}

	if err := os.WriteFile(stateFile, data, 0o600); err != nil {
		return fmt.Errorf("failed to write state file: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Captured state before task: %d untracked files\n", len(untrackedFiles))
	return nil
}

// LoadPreTaskState loads previously captured task state.
// Returns nil if no state file exists.
func LoadPreTaskState(toolUseID string) (*PreTaskState, error) {
	stateFile := preTaskStateFile(toolUseID)

	if !fileExists(stateFile) {
		return nil, nil
	}

	data, err := os.ReadFile(stateFile) //nolint:gosec // Reading from controlled git metadata path
	if err != nil {
		return nil, fmt.Errorf("failed to read state file: %w", err)
	}

	var state PreTaskState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("failed to unmarshal state: %w", err)
	}

	return &state, nil
}

// CleanupPreTaskState removes the task state file after use
func CleanupPreTaskState(toolUseID string) error {
	stateFile := preTaskStateFile(toolUseID)
	if fileExists(stateFile) {
		return os.Remove(stateFile)
	}
	return nil
}

// ComputeNewFilesFromTask compares current untracked files with pre-task state
// to find files that were created during the task.
func ComputeNewFilesFromTask(preState *PreTaskState) ([]string, error) {
	if preState == nil {
		return nil, nil
	}

	currentUntracked, err := getUntrackedFilesForState()
	if err != nil {
		return nil, err
	}

	return findNewUntrackedFiles(currentUntracked, preState.UntrackedFiles), nil
}

// computeNewFilesFromTaskState is a helper that doesn't need to query git
// (used for testing)
func computeNewFilesFromTaskState(preState *PreTaskState, currentFiles []string) []string {
	if preState == nil {
		return nil
	}
	return findNewUntrackedFiles(currentFiles, preState.UntrackedFiles)
}

// preTaskStateFile returns the absolute path to the pre-task state file for a tool use.
// Works correctly from any subdirectory within the repository.
func preTaskStateFile(toolUseID string) string {
	tmpDirAbs, err := paths.AbsPath(paths.EntireTmpDir)
	if err != nil {
		tmpDirAbs = paths.EntireTmpDir // Fallback to relative
	}
	return filepath.Join(tmpDirAbs, fmt.Sprintf("pre-task-%s.json", toolUseID))
}

// preTaskFilePrefix is the prefix for pre-task state files
const preTaskFilePrefix = "pre-task-"

// FindActivePreTaskFile finds an active pre-task file in .entire/tmp/ and returns
// the parent Task's tool_use_id. Returns ("", false) if no pre-task file exists.
// When multiple pre-task files exist (nested subagents), returns the most recently
// modified one.
// Works correctly from any subdirectory within the repository.
func FindActivePreTaskFile() (taskToolUseID string, found bool) {
	tmpDirAbs, err := paths.AbsPath(paths.EntireTmpDir)
	if err != nil {
		tmpDirAbs = paths.EntireTmpDir // Fallback to relative
	}
	entries, err := os.ReadDir(tmpDirAbs)
	if err != nil {
		return "", false
	}

	var latestFile string
	var latestTime time.Time

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, preTaskFilePrefix) || !strings.HasSuffix(name, ".json") {
			continue
		}

		// Check modification time for nested subagent handling
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if latestFile == "" || info.ModTime().After(latestTime) {
			latestFile = name
			latestTime = info.ModTime()
		}
	}

	if latestFile == "" {
		return "", false
	}

	// Extract tool_use_id from filename: pre-task-<tool_use_id>.json
	toolUseID := strings.TrimPrefix(latestFile, preTaskFilePrefix)
	toolUseID = strings.TrimSuffix(toolUseID, ".json")
	return toolUseID, true
}

// DetectChangedFiles detects files that have been modified, added, or deleted
// since the last commit (or compared to the index for incremental checkpoints).
// Returns three slices: modified files, new (untracked) files, and deleted files.
// Excludes .entire/ directory from all results.
func DetectChangedFiles() (modified, newFiles, deleted []string, err error) {
	repo, err := openRepository()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to open repository: %w", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to get worktree: %w", err)
	}

	status, err := worktree.Status()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to get status: %w", err)
	}

	for file, st := range status {
		// Skip .entire directory
		if paths.IsInfrastructurePath(file) {
			continue
		}

		// Handle different status codes
		// Worktree status indicates changes not yet staged
		// Staging status indicates changes staged for commit
		switch {
		case st.Worktree == git.Untracked:
			// New untracked file
			newFiles = append(newFiles, file)
		case st.Worktree == git.Deleted || st.Staging == git.Deleted:
			// Deleted file
			deleted = append(deleted, file)
		case st.Worktree == git.Modified || st.Staging == git.Modified ||
			st.Worktree == git.Added || st.Staging == git.Added:
			// Modified or staged file
			modified = append(modified, file)
		}
	}

	return modified, newFiles, deleted, nil
}

// GetNextCheckpointSequence returns the next sequence number for incremental checkpoints.
// It counts existing checkpoint files in the task metadata checkpoints directory.
// Returns 1 if no checkpoints exist yet.
func GetNextCheckpointSequence(sessionID, taskToolUseID string) int {
	// ctx.SessionID is already an Entire session ID (date-prefixed), so use SessionMetadataDirFromEntireID
	sessionMetadataDir := paths.SessionMetadataDirFromEntireID(sessionID)
	taskMetadataDir := strategy.TaskMetadataDir(sessionMetadataDir, taskToolUseID)
	checkpointsDir := filepath.Join(taskMetadataDir, "checkpoints")

	entries, err := os.ReadDir(checkpointsDir)
	if err != nil {
		// Directory doesn't exist or can't be read - start at 1
		return 1
	}

	// Count JSON files (checkpoints)
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			count++
		}
	}

	return count + 1
}
