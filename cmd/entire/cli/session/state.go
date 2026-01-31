package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"entire.io/cli/cmd/entire/cli/agent"
	"entire.io/cli/cmd/entire/cli/checkpoint/id"
	"entire.io/cli/cmd/entire/cli/jsonutil"
	"entire.io/cli/cmd/entire/cli/validation"
)

const (
	// sessionStateDirName is the directory name for session state files within git common dir.
	sessionStateDirName = "entire-sessions"
)

// State represents the state of an active session.
// This is stored in .git/entire-sessions/<session-id>.json
type State struct {
	// SessionID is the unique session identifier
	SessionID string `json:"session_id"`

	// BaseCommit is the HEAD commit when the session started
	BaseCommit string `json:"base_commit"`

	// WorktreePath is the absolute path to the worktree root
	WorktreePath string `json:"worktree_path,omitempty"`

	// StartedAt is when the session was started
	StartedAt time.Time `json:"started_at"`

	// CheckpointCount is the number of checkpoints created in this session
	CheckpointCount int `json:"checkpoint_count"`

	// CondensedTranscriptLines tracks lines already included in previous condensation
	CondensedTranscriptLines int `json:"condensed_transcript_lines,omitempty"`

	// UntrackedFilesAtStart tracks files that existed at session start (to preserve during rewind)
	UntrackedFilesAtStart []string `json:"untracked_files_at_start,omitempty"`

	// FilesTouched tracks files modified/created/deleted during this session
	FilesTouched []string `json:"files_touched,omitempty"`

	// ConcurrentWarningShown is true if user was warned about concurrent sessions
	ConcurrentWarningShown bool `json:"concurrent_warning_shown,omitempty"`

	// ShouldResetShadowBranch is set by OnPromptStart when overlap check determines
	// the shadow branch should be reset on next checkpoint (manual-commit strategy only)
	ShouldResetShadowBranch bool `json:"should_reset_shadow_branch,omitempty"`

	// LastCheckpointID is the checkpoint ID from last condensation, reused for subsequent commits without new content
	LastCheckpointID id.CheckpointID `json:"last_checkpoint_id,omitempty"`

	// AgentType identifies the agent that created this session (e.g., "Claude Code", "Gemini CLI", "Cursor")
	AgentType agent.AgentType `json:"agent_type,omitempty"`

	// Token usage tracking (accumulated across all checkpoints in this session)
	TokenUsage *agent.TokenUsage `json:"token_usage,omitempty"`

	// Transcript position when session started (for multi-session checkpoints)
	TranscriptLinesAtStart      int    `json:"transcript_lines_at_start,omitempty"`
	TranscriptIdentifierAtStart string `json:"transcript_identifier_at_start,omitempty"`

	// TranscriptPath is the path to the live transcript file (for mid-session commit detection)
	TranscriptPath string `json:"transcript_path,omitempty"`

	// PromptAttributions tracks user and agent line changes at each prompt start.
	// This enables accurate attribution by capturing user edits between checkpoints.
	PromptAttributions []PromptAttribution `json:"prompt_attributions,omitempty"`

	// PendingPromptAttribution holds attribution calculated at prompt start (before agent runs).
	// This is moved to PromptAttributions when SaveChanges is called.
	PendingPromptAttribution *PromptAttribution `json:"pending_prompt_attribution,omitempty"`
}

// PromptAttribution captures line-level attribution data at the start of each prompt.
// By recording what changed since the last checkpoint BEFORE the agent works,
// we can accurately separate user edits from agent contributions.
type PromptAttribution struct {
	// CheckpointNumber is which checkpoint this was recorded before (1-indexed)
	CheckpointNumber int `json:"checkpoint_number"`

	// UserLinesAdded is lines added by user since the last checkpoint
	UserLinesAdded int `json:"user_lines_added"`

	// UserLinesRemoved is lines removed by user since the last checkpoint
	UserLinesRemoved int `json:"user_lines_removed"`

	// AgentLinesAdded is total agent lines added so far (base → last checkpoint).
	// Always 0 for checkpoint 1 since there's no previous checkpoint to measure against.
	AgentLinesAdded int `json:"agent_lines_added"`

	// AgentLinesRemoved is total agent lines removed so far (base → last checkpoint).
	// Always 0 for checkpoint 1 since there's no previous checkpoint to measure against.
	AgentLinesRemoved int `json:"agent_lines_removed"`

	// UserAddedPerFile tracks per-file user additions for accurate modification tracking.
	// This enables distinguishing user self-modifications from agent modifications.
	// See docs/architecture/attribution.md for details.
	UserAddedPerFile map[string]int `json:"user_added_per_file,omitempty"`
}

// StateStore provides low-level operations for managing session state files.
//
// StateStore is a primitive for session state persistence. It is NOT the same as
// the Sessions interface - it only handles state files in .git/entire-sessions/,
// not the full session data which includes checkpoint content.
//
// Use StateStore directly in strategies for performance-critical state operations.
// Use the Sessions interface (when implemented) for high-level session management.
type StateStore struct {
	// stateDir is the directory where session state files are stored
	stateDir string
}

// NewStateStore creates a new state store.
// Uses the git common dir to store session state (shared across worktrees).
func NewStateStore() (*StateStore, error) {
	commonDir, err := getGitCommonDir()
	if err != nil {
		return nil, fmt.Errorf("failed to get git common dir: %w", err)
	}
	return &StateStore{
		stateDir: filepath.Join(commonDir, sessionStateDirName),
	}, nil
}

// NewStateStoreWithDir creates a new state store with a custom directory.
// This is useful for testing.
func NewStateStoreWithDir(stateDir string) *StateStore {
	return &StateStore{stateDir: stateDir}
}

// Load loads the session state for the given session ID.
// Returns (nil, nil) when session file doesn't exist (not an error condition).
func (s *StateStore) Load(ctx context.Context, sessionID string) (*State, error) {
	_ = ctx // Reserved for future use

	// Validate session ID to prevent path traversal
	if err := validation.ValidateSessionID(sessionID); err != nil {
		return nil, fmt.Errorf("invalid session ID: %w", err)
	}

	stateFile := s.stateFilePath(sessionID)

	data, err := os.ReadFile(stateFile) //nolint:gosec // stateFile is derived from sessionID
	if os.IsNotExist(err) {
		return nil, nil //nolint:nilnil // nil,nil indicates session not found (expected case)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read session state: %w", err)
	}

	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("failed to unmarshal session state: %w", err)
	}
	return &state, nil
}

// Save saves the session state atomically.
func (s *StateStore) Save(ctx context.Context, state *State) error {
	_ = ctx // Reserved for future use

	// Validate session ID to prevent path traversal
	if err := validation.ValidateSessionID(state.SessionID); err != nil {
		return fmt.Errorf("invalid session ID: %w", err)
	}

	if err := os.MkdirAll(s.stateDir, 0o750); err != nil {
		return fmt.Errorf("failed to create session state directory: %w", err)
	}

	data, err := jsonutil.MarshalIndentWithNewline(state, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal session state: %w", err)
	}

	stateFile := s.stateFilePath(state.SessionID)

	// Atomic write: write to temp file, then rename
	tmpFile := stateFile + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0o600); err != nil {
		return fmt.Errorf("failed to write session state: %w", err)
	}
	if err := os.Rename(tmpFile, stateFile); err != nil {
		return fmt.Errorf("failed to rename session state file: %w", err)
	}
	return nil
}

// Clear removes the session state file for the given session ID.
func (s *StateStore) Clear(ctx context.Context, sessionID string) error {
	_ = ctx // Reserved for future use

	// Validate session ID to prevent path traversal
	if err := validation.ValidateSessionID(sessionID); err != nil {
		return fmt.Errorf("invalid session ID: %w", err)
	}

	stateFile := s.stateFilePath(sessionID)

	if err := os.Remove(stateFile); err != nil {
		if os.IsNotExist(err) {
			return nil // Already gone, not an error
		}
		return fmt.Errorf("failed to remove session state file: %w", err)
	}
	return nil
}

// RemoveAll removes the entire session state directory.
// This is used during uninstall to completely remove all session state.
func (s *StateStore) RemoveAll() error {
	if err := os.RemoveAll(s.stateDir); err != nil {
		return fmt.Errorf("failed to remove session state directory: %w", err)
	}
	return nil
}

// List returns all session states.
func (s *StateStore) List(ctx context.Context) ([]*State, error) {
	_ = ctx // Reserved for future use

	entries, err := os.ReadDir(s.stateDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read session state directory: %w", err)
	}

	var states []*State
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if strings.HasSuffix(entry.Name(), ".tmp") {
			continue // Skip temp files
		}

		sessionID := strings.TrimSuffix(entry.Name(), ".json")
		state, err := s.Load(ctx, sessionID)
		if err != nil {
			continue // Skip corrupted state files
		}
		if state == nil {
			continue
		}

		states = append(states, state)
	}
	return states, nil
}

// FindByBaseCommit finds all sessions based on the given commit hash.
func (s *StateStore) FindByBaseCommit(ctx context.Context, baseCommit string) ([]*State, error) {
	allStates, err := s.List(ctx)
	if err != nil {
		return nil, err
	}

	var matching []*State
	for _, state := range allStates {
		if state.BaseCommit == baseCommit {
			matching = append(matching, state)
		}
	}
	return matching, nil
}

// FindByWorktree finds all sessions for the given worktree path.
func (s *StateStore) FindByWorktree(ctx context.Context, worktreePath string) ([]*State, error) {
	allStates, err := s.List(ctx)
	if err != nil {
		return nil, err
	}

	var matching []*State
	for _, state := range allStates {
		if state.WorktreePath == worktreePath {
			matching = append(matching, state)
		}
	}
	return matching, nil
}

// stateFilePath returns the path to a session state file.
func (s *StateStore) stateFilePath(sessionID string) string {
	return filepath.Join(s.stateDir, sessionID+".json")
}

// getGitCommonDir returns the path to the shared git directory.
// In a regular checkout, this is .git/
// In a worktree, this is the main repo's .git/ (not .git/worktrees/<name>/)
func getGitCommonDir() (string, error) {
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--git-common-dir")
	cmd.Dir = "."
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get git common dir: %w", err)
	}

	commonDir := strings.TrimSpace(string(output))

	// git rev-parse --git-common-dir returns relative paths from the working directory,
	// so we need to make it absolute if it isn't already
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(".", commonDir)
	}

	return filepath.Clean(commonDir), nil
}

// GetWorktreePath returns the absolute path to the current worktree root.
func GetWorktreePath() (string, error) {
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get worktree path: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

// FindLegacyEntireSessionID checks for existing session state files with a legacy date-prefixed format.
// Takes an agent session ID and returns the corresponding entire session ID if found
// (e.g., "2026-01-20-abc123" for agent ID "abc123"), or empty string if no legacy session exists.
//
// This provides backward compatibility when resuming sessions that were created before
// the session ID format change (when EntireSessionID added a date prefix).
func FindLegacyEntireSessionID(agentSessionID string) string {
	if agentSessionID == "" {
		return ""
	}

	// Validate ID format to prevent path traversal attacks
	if err := validation.ValidateAgentSessionID(agentSessionID); err != nil {
		return ""
	}

	commonDir, err := getGitCommonDir()
	if err != nil {
		return ""
	}

	stateDir := filepath.Join(commonDir, sessionStateDirName)
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return ""
	}

	// Look for state files with legacy date-prefixed format matching this agent ID
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if strings.HasSuffix(entry.Name(), ".tmp") {
			continue
		}

		existingSessionID := strings.TrimSuffix(entry.Name(), ".json")

		// Check if this is a legacy format (has date prefix) that matches our agent ID
		// Legacy format: YYYY-MM-DD-<agent-uuid> (11 char prefix)
		if len(existingSessionID) > 11 &&
			existingSessionID[4] == '-' &&
			existingSessionID[7] == '-' &&
			existingSessionID[10] == '-' {
			// Extract the agent ID portion and compare
			extractedAgentID := existingSessionID[11:]
			if extractedAgentID == agentSessionID {
				return existingSessionID
			}
		}
	}

	return ""
}
