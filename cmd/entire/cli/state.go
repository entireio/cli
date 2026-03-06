package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"

	"github.com/go-git/go-git/v5"
)

// PrePromptState stores the state captured before a user prompt
type PrePromptState struct {
	SessionID      string   `json:"session_id"`
	Timestamp      string   `json:"timestamp"`
	UntrackedFiles []string `json:"untracked_files"`

	// TranscriptOffset is the unified transcript position when this state was captured.
	// For Claude Code (JSONL), this is the line count.
	// For Gemini CLI (JSON), this is the message count.
	// Zero means not set or session just started.
	TranscriptOffset int `json:"transcript_offset,omitempty"`

	// LastTranscriptIdentifier is the agent-specific identifier at the transcript position.
	// UUID for Claude Code, message ID for Gemini CLI. Optional metadata.
	LastTranscriptIdentifier string `json:"last_transcript_identifier,omitempty"`

	// Deprecated: StartMessageIndex is the old Gemini-specific field.
	// Migrated to TranscriptOffset on load.
	StartMessageIndex int `json:"start_message_index,omitempty"`

	// Deprecated: StepTranscriptStart is the old Claude-specific field.
	// Migrated to TranscriptOffset on load.
	StepTranscriptStart int `json:"step_transcript_start,omitempty"`

	// Deprecated: LastTranscriptLineCount is the oldest name for transcript position.
	// Migrated to TranscriptOffset on load.
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
	// Migrate the oldest field to the intermediate field first
	if s.StepTranscriptStart == 0 && s.LastTranscriptLineCount > 0 {
		s.StepTranscriptStart = s.LastTranscriptLineCount
	}
	s.LastTranscriptLineCount = 0

	// Migrate all deprecated fields to the unified TranscriptOffset
	if s.TranscriptOffset == 0 {
		if s.StepTranscriptStart > 0 {
			s.TranscriptOffset = s.StepTranscriptStart
		} else if s.StartMessageIndex > 0 {
			s.TranscriptOffset = s.StartMessageIndex
		}
	}
	s.StepTranscriptStart = 0
	s.StartMessageIndex = 0
}

// CapturePrePromptState captures current untracked files and transcript position before a prompt
// and saves them to a state file.
//
// The agent parameter is used to determine the transcript position via TranscriptAnalyzer.
// If the agent does not implement TranscriptAnalyzer, the transcript offset will be 0.
// The sessionRef parameter is optional — if empty, transcript position won't be captured.
//
// Works correctly from any subdirectory within the repository.
func CapturePrePromptState(ctx context.Context, ag agent.Agent, sessionID, sessionRef string) error {
	if sessionID == "" {
		sessionID = unknownSessionID
	}

	// Get absolute path for tmp directory
	tmpDirAbs, err := paths.AbsPath(ctx, paths.EntireTmpDir)
	if err != nil {
		tmpDirAbs = paths.EntireTmpDir // Fallback to relative
	}

	// Create tmp directory if it doesn't exist
	if err := os.MkdirAll(tmpDirAbs, 0o750); err != nil {
		return fmt.Errorf("failed to create tmp directory: %w", err)
	}

	// Get list of untracked files (excluding .entire directory itself)
	untrackedFiles, err := getUntrackedFilesForState(ctx)
	if err != nil {
		return fmt.Errorf("failed to get untracked files: %w", err)
	}

	// Get transcript position using TranscriptAnalyzer if available
	var transcriptOffset int
	if analyzer, ok := agent.AsTranscriptAnalyzer(ag); ok && sessionRef != "" {
		pos, posErr := analyzer.GetTranscriptPosition(sessionRef)
		if posErr != nil {
			logging.Warn(logging.WithComponent(ctx, "state"), "failed to get transcript position",
				slog.String("error", posErr.Error()))
		} else {
			transcriptOffset = pos
		}
	}

	// Create state file
	stateFile := prePromptStateFile(ctx, sessionID)
	state := PrePromptState{
		SessionID:        sessionID,
		Timestamp:        time.Now().UTC().Format(time.RFC3339),
		UntrackedFiles:   untrackedFiles,
		TranscriptOffset: transcriptOffset,
	}

	data, err := jsonutil.MarshalIndentWithNewline(state, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal state: %w", err)
	}

	if err := os.WriteFile(stateFile, data, 0o600); err != nil {
		return fmt.Errorf("failed to write state file: %w", err)
	}

	logging.Debug(logging.WithComponent(ctx, "state"), "captured state before prompt",
		slog.Int("untracked_files", len(untrackedFiles)),
		slog.Int("transcript_offset", transcriptOffset))
	return nil
}

// LoadPrePromptState loads previously captured state.
// Returns nil if no state file exists.
func LoadPrePromptState(ctx context.Context, sessionID string) (*PrePromptState, error) {
	stateFile := prePromptStateFile(ctx, sessionID)

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
func CleanupPrePromptState(ctx context.Context, sessionID string) error {
	stateFile := prePromptStateFile(ctx, sessionID)
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
func DetectFileChanges(ctx context.Context, previouslyUntracked []string) (*FileChanges, error) {
	repo, err := openRepository(ctx)
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

// filterToUncommittedFiles removes files from the list that are already committed to HEAD
// with matching content. This prevents re-adding files that an agent committed mid-turn
// (already condensed by PostCommit) back to FilesTouched via SaveStep. Files not in
// HEAD or with different content in the working tree are kept. Fails open: if any git
// operation errors, returns the original list unchanged.
func filterToUncommittedFiles(ctx context.Context, files []string, repoRoot string) []string {
	if len(files) == 0 {
		return files
	}

	repo, err := openRepository(ctx)
	if err != nil {
		return files // fail open
	}

	head, err := repo.Head()
	if err != nil {
		return files // fail open (empty repo, detached HEAD, etc.)
	}

	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		return files // fail open
	}

	headTree, err := commit.Tree()
	if err != nil {
		return files // fail open
	}

	var result []string
	for _, relPath := range files {
		headFile, err := headTree.File(relPath)
		if err != nil {
			// File not in HEAD — it's uncommitted
			result = append(result, relPath)
			continue
		}

		// File is in HEAD — compare content with working tree
		absPath := filepath.Join(repoRoot, relPath)
		workingContent, err := os.ReadFile(absPath) //nolint:gosec // path from controlled source
		if err != nil {
			// Can't read working tree file (deleted?) — keep it
			result = append(result, relPath)
			continue
		}

		headContent, err := headFile.Contents()
		if err != nil {
			result = append(result, relPath)
			continue
		}

		if string(workingContent) != headContent {
			// Working tree differs from HEAD — uncommitted changes
			result = append(result, relPath)
		}
		// else: content matches HEAD — already committed, skip
	}

	return result
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

// mergeUnique appends elements from extra into base, skipping duplicates already in base.
func mergeUnique(base, extra []string) []string {
	if len(extra) == 0 {
		return base
	}
	seen := make(map[string]bool, len(base))
	for _, s := range base {
		seen[s] = true
	}
	for _, s := range extra {
		if !seen[s] {
			seen[s] = true
			base = append(base, s)
		}
	}
	return base
}

// prePromptStateFile returns the absolute path to the pre-prompt state file for a session.
// Works correctly from any subdirectory within the repository.
func prePromptStateFile(ctx context.Context, sessionID string) string {
	tmpDirAbs, err := paths.AbsPath(ctx, paths.EntireTmpDir)
	if err != nil {
		tmpDirAbs = paths.EntireTmpDir // Fallback to relative
	}
	return filepath.Join(tmpDirAbs, fmt.Sprintf("pre-prompt-%s.json", sessionID))
}

// getUntrackedFilesForState returns a list of untracked files using go-git
// Excludes .entire directory
func getUntrackedFilesForState(ctx context.Context) ([]string, error) {
	repo, err := openRepository(ctx)
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
func CapturePreTaskState(ctx context.Context, toolUseID string) error {
	if toolUseID == "" {
		return errors.New("tool_use_id is required")
	}

	// Get absolute path for tmp directory
	tmpDirAbs, err := paths.AbsPath(ctx, paths.EntireTmpDir)
	if err != nil {
		tmpDirAbs = paths.EntireTmpDir // Fallback to relative
	}

	// Create tmp directory if it doesn't exist
	if err := os.MkdirAll(tmpDirAbs, 0o750); err != nil {
		return fmt.Errorf("failed to create tmp directory: %w", err)
	}

	// Get list of untracked files (excluding .entire directory itself)
	untrackedFiles, err := getUntrackedFilesForState(ctx)
	if err != nil {
		return fmt.Errorf("failed to get untracked files: %w", err)
	}

	// Create state file
	stateFile := preTaskStateFile(ctx, toolUseID)
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

	logging.Debug(logging.WithComponent(ctx, "state"), "captured state before task",
		slog.Int("untracked_files", len(untrackedFiles)))
	return nil
}

// LoadPreTaskState loads previously captured task state.
// Returns nil if no state file exists.
func LoadPreTaskState(ctx context.Context, toolUseID string) (*PreTaskState, error) {
	stateFile := preTaskStateFile(ctx, toolUseID)

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
func CleanupPreTaskState(ctx context.Context, toolUseID string) error {
	stateFile := preTaskStateFile(ctx, toolUseID)
	if fileExists(stateFile) {
		return os.Remove(stateFile) //nolint:wrapcheck // already present in codebase
	}
	return nil
}

// preTaskStateFile returns the absolute path to the pre-task state file for a tool use.
// Works correctly from any subdirectory within the repository.
func preTaskStateFile(ctx context.Context, toolUseID string) string {
	tmpDirAbs, err := paths.AbsPath(ctx, paths.EntireTmpDir)
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
func FindActivePreTaskFile(ctx context.Context) (taskToolUseID string, found bool) {
	tmpDirAbs, err := paths.AbsPath(ctx, paths.EntireTmpDir)
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
