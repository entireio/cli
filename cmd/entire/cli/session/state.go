package session

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"entire.io/cli/cmd/entire/cli/agent"
	"entire.io/cli/cmd/entire/cli/checkpoint/id"
	"entire.io/cli/cmd/entire/cli/jsonutil"
	"entire.io/cli/cmd/entire/cli/logging"
	"entire.io/cli/cmd/entire/cli/sessionid"
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

	// LastCheckpointID is the checkpoint ID from last condensation, reused for subsequent commits without new content
	LastCheckpointID id.CheckpointID `json:"last_checkpoint_id,omitempty"`

	// AgentType identifies the agent that created this session (e.g., "Claude Code", "Gemini CLI", "Cursor")
	AgentType agent.AgentType `json:"agent_type,omitempty"`

	// Token usage tracking (accumulated across all checkpoints in this session)
	TokenUsage *agent.TokenUsage `json:"token_usage,omitempty"`

	// Transcript position when session started (for multi-session checkpoints)
	TranscriptLinesAtStart int    `json:"transcript_lines_at_start,omitempty"`
	TranscriptUUIDAtStart  string `json:"transcript_uuid_at_start,omitempty"`

	// TranscriptPath is the path to the live transcript file (for mid-session commit detection)
	TranscriptPath string `json:"transcript_path,omitempty"`
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

// GetOrCreateEntireSessionID returns a stable session ID for the given agent session ID.
// If a session state already exists with this ID, returns that session ID
// (preserving the original date prefix). Otherwise creates a new ID with today's date.
//
// When multiple state files exist for the same ID (due to the midnight-crossing bug),
// this function picks the most recent by date and cleans up older duplicates.
//
// This function never returns an error - it always falls back to generating a new ID
// if any issues occur (e.g., corrupt git repo, permission problems, invalid ID).
//
// Note: This function is not thread-safe. Concurrent calls with the same ID may
// race on file cleanup. In practice this is not an issue since agent hooks are
// called sequentially within a session.
func GetOrCreateEntireSessionID(agentSessionID string) string {
	// Validate ID format to prevent path traversal attacks
	if err := validation.ValidateAgentSessionID(agentSessionID); err != nil {
		logging.Warn(context.Background(), "invalid agent session ID",
			slog.String("input", agentSessionID),
			slog.Any("error", err))
		// Invalid ID - return it anyway (will fail downstream with clearer error)
		// Don't generate random ID as that breaks session continuity
		return sessionid.EntireSessionID(agentSessionID)
	}

	commonDir, err := getGitCommonDir()
	if err != nil {
		// Can't get common dir (corrupt git repo?) - fall back to new ID
		return sessionid.EntireSessionID(agentSessionID)
	}

	stateDir := filepath.Join(commonDir, sessionStateDirName)
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		// State dir doesn't exist or can't read it - fall back to new ID
		return sessionid.EntireSessionID(agentSessionID)
	}

	// Collect all matching session IDs
	var matches []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		existingSessionID := strings.TrimSuffix(entry.Name(), ".json")
		existingUUID := sessionid.ModelSessionID(existingSessionID)

		if existingUUID == agentSessionID {
			matches = append(matches, existingSessionID)
		}
	}

	if len(matches) == 0 {
		// No existing session found - create new ID with today's date
		return sessionid.EntireSessionID(agentSessionID)
	}

	// Pick most recent (YYYY-MM-DD sorts correctly lexicographically)
	sort.Strings(matches)
	mostRecent := matches[len(matches)-1]

	// Best-effort cleanup of old duplicates (ignore errors)
	for _, oldID := range matches[:len(matches)-1] {
		oldFile := filepath.Join(stateDir, oldID+".json")
		_ = os.Remove(oldFile)
	}

	return mostRecent
}
