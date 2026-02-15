package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"

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

	// Transcript position at prompt start - tracks what was added during this step/turn.
	// Used for Claude Code sessions.
	LastTranscriptIdentifier string `json:"last_transcript_identifier,omitempty"` // Last identifier when prompt started (UUID for Claude, message ID for Gemini)
	StepTranscriptStart      int    `json:"step_transcript_start,omitempty"`      // Transcript line count when this step/turn started

	// Deprecated: LastTranscriptLineCount is the old name for StepTranscriptStart.
	// Kept for backward compatibility when reading state files written by older CLI versions.
	LastTranscriptLineCount int `json:"last_transcript_line_count,omitempty"`
}

// PreUntrackedFiles returns the untracked files list, or nil if the receiver is nil.
// This nil-vs-empty distinction lets DetectFileChanges know whether to skip new-file detection.
// When the receiver is non-nil but UntrackedFiles is nil (e.g., old state files deserialized with
// "untracked_files": null), returns an empty non-nil slice so that all current untracked files
// are correctly treated as new.
func (s *PrePromptState) PreUntrackedFiles() []string {
	if s == nil {
		return nil
	}
	if s.UntrackedFiles == nil {
		return []string{}
	}
	return s.UntrackedFiles
}

// normalizePrePromptState migrates deprecated fields after loading from JSON.
func (s *PrePromptState) normalizePrePromptState() {
	if s.StepTranscriptStart == 0 && s.LastTranscriptLineCount > 0 {
		s.StepTranscriptStart = s.LastTranscriptLineCount
	}
	s.LastTranscriptLineCount = 0
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
		StepTranscriptStart:      transcriptPos.LineCount,
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

// CaptureOpencodePrePromptState captures current untracked files and transcript position
// before a prompt for OpenCode sessions. OpenCode uses JSONL transcripts, so position
// is the line count and the last entry's info.id.
func CaptureOpencodePrePromptState(sessionID, transcriptPath string) error {
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

	// Get transcript position (line count and last entry ID) for OpenCode JSONL
	var lineCount int
	var lastEntryID string
	if transcriptPath != "" {
		if data, readErr := os.ReadFile(transcriptPath); readErr == nil && len(data) > 0 { //nolint:gosec // Reading from controlled transcript path
			lines := strings.Split(string(data), "\n")
			for _, line := range lines {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				lineCount++
				var entry struct {
					Info struct {
						ID string `json:"id"`
					} `json:"info"`
				}
				if jsonErr := json.Unmarshal([]byte(line), &entry); jsonErr == nil && entry.Info.ID != "" {
					lastEntryID = entry.Info.ID
				}
			}
		}
	}

	// Create state file
	stateFile := prePromptStateFile(sessionID)
	state := PrePromptState{
		SessionID:                sessionID,
		Timestamp:                time.Now().UTC().Format(time.RFC3339),
		UntrackedFiles:           untrackedFiles,
		StepTranscriptStart:      lineCount,
		LastTranscriptIdentifier: lastEntryID,
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal state: %w", err)
	}

	if err := os.WriteFile(stateFile, data, 0o600); err != nil {
		return fmt.Errorf("failed to write state file: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Captured OpenCode state before prompt: %d untracked files, transcript line: %d (last entry id: %s)\n", len(untrackedFiles), lineCount, lastEntryID)
	return nil
}

// LoadPrePromptState loads previously captured state.
// Returns nil if no state file exists.
func LoadPrePromptState(sessionID string) (*PrePromptState, error) {
	stateFile := prePromptStateFile(sessionID)

	if !fileExists(stateFile) {
		return nil, nil //nolint:nilnil // already present in codebase
	}

	data, err := os.ReadFile(stateFile) //nolint:gosec // Reading from controlled git metadata path
	if err != nil {
		return nil, fmt.Errorf("failed to read state file: %w", err)
	}

	var state PrePromptState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("failed to unmarshal state: %w", err)
	}

	state.normalizePrePromptState()

	return &state, nil
}

// CleanupPrePromptState removes the state file after use
func CleanupPrePromptState(sessionID string) error {
	stateFile := prePromptStateFile(sessionID)
	if fileExists(stateFile) {
		return os.Remove(stateFile) //nolint:wrapcheck // already present in codebase
	}
	return nil
}

// FileChanges holds categorized file changes from git status.
type FileChanges struct {
	Modified []string // Modified or staged files
	New      []string // Untracked files (filtered if previouslyUntracked provided)
	Deleted  []string // Deleted files (staged or unstaged)
}

// DetectFileChanges returns categorized file changes from the current git status.
//
// previouslyUntracked controls new-file detection:
//   - nil: all untracked files go into New
//   - non-nil: only untracked files NOT in the pre-existing set go into New
//
// Modified includes both worktree and staging modified/added files.
// Deleted includes both staged and unstaged deletions.
// All results exclude .entire/ directory.
func DetectFileChanges(previouslyUntracked []string) (*FileChanges, error) {
	repo, err := openRepository()
	if err != nil {
		return nil, fmt.Errorf("failed to open repository: %w", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		return nil, fmt.Errorf("failed to get worktree: %w", err)
	}

	status, err := worktree.Status()
	if err != nil {
		return nil, fmt.Errorf("failed to get status: %w", err)
	}

	// Build set of pre-existing untracked files for quick lookup
	var preExisting map[string]bool
	if previouslyUntracked != nil {
		preExisting = make(map[string]bool, len(previouslyUntracked))
		for _, f := range previouslyUntracked {
			preExisting[f] = true
		}
	}

	var changes FileChanges
	for file, st := range status {
		if paths.IsInfrastructurePath(file) {
			continue
		}

		switch {
		case st.Worktree == git.Untracked:
			if preExisting != nil {
				if !preExisting[file] {
					changes.New = append(changes.New, file)
				}
			} else {
				changes.New = append(changes.New, file)
			}
		case st.Worktree == git.Deleted || st.Staging == git.Deleted:
			changes.Deleted = append(changes.Deleted, file)
		case st.Worktree == git.Modified || st.Staging == git.Modified ||
			st.Worktree == git.Added || st.Staging == git.Added:
			changes.Modified = append(changes.Modified, file)
		}
	}

	return &changes, nil
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
		return nil, err //nolint:wrapcheck // already present in codebase
	}

	status, err := worktree.Status()
	if err != nil {
		return nil, err //nolint:wrapcheck // already present in codebase
	}

	untrackedFiles := []string{}
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

// PreUntrackedFiles returns the untracked files list, or nil if the receiver is nil.
// See PrePromptState.PreUntrackedFiles for nil-vs-empty semantics.
func (s *PreTaskState) PreUntrackedFiles() []string {
	if s == nil {
		return nil
	}
	if s.UntrackedFiles == nil {
		return []string{}
	}
	return s.UntrackedFiles
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
		return nil, nil //nolint:nilnil // already present in codebase
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
		return os.Remove(stateFile) //nolint:wrapcheck // already present in codebase
	}
	return nil
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

// GetNextCheckpointSequence returns the next sequence number for incremental checkpoints.
// It counts existing checkpoint files in the task metadata checkpoints directory.
// Returns 1 if no checkpoints exist yet.
func GetNextCheckpointSequence(sessionID, taskToolUseID string) int {
	// Use the session ID directly as the metadata directory name
	sessionMetadataDir := paths.SessionMetadataDirFromSessionID(sessionID)
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
