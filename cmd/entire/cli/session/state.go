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

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/validation"
)

const (
	// SessionStateDirName is the directory name for session state files within git common dir.
	SessionStateDirName = "entire-sessions"
)

// State represents the state of an active session.
// This is stored in .git/entire-sessions/<session-id>.json
type State struct {
	// SessionID is the unique session identifier
	SessionID string `json:"session_id"`

	// CLIVersion is the version of the CLI that created this session
	CLIVersion string `json:"cli_version,omitempty"`

	// BaseCommit tracks the current shadow branch base. Initially set to HEAD when the
	// session starts, but updated on migration (pull/rebase) and after condensation.
	// Used for shadow branch naming and checkpoint storage — NOT for attribution.
	BaseCommit string `json:"base_commit"`

	// AttributionBaseCommit is the commit used as the reference point for attribution calculations.
	// Unlike BaseCommit (which tracks the shadow branch and moves with migration), this field
	// preserves the original base commit so deferred condensation can correctly calculate
	// agent vs human line attribution. Updated only after successful condensation.
	AttributionBaseCommit string `json:"attribution_base_commit,omitempty"`

	// WorktreePath is the absolute path to the worktree root
	WorktreePath string `json:"worktree_path,omitempty"`

	// WorktreeID is the internal git worktree identifier (empty for main worktree)
	// Derived from .git/worktrees/<name>/, stable across git worktree move
	WorktreeID string `json:"worktree_id,omitempty"`

	// StartedAt is when the session was started
	StartedAt time.Time `json:"started_at"`

	// EndedAt is when the session was explicitly closed by the user.
	// nil means the session is still active or was not cleanly closed.
	EndedAt *time.Time `json:"ended_at,omitempty"`

	// Phase is the lifecycle stage of this session (see phase.go).
	// Empty means idle (backward compat with pre-state-machine files).
	Phase Phase `json:"phase,omitempty"`

	// PendingCheckpointID is the checkpoint ID for the current commit cycle.
	// Generated once when first needed, reused across all commits in the session.
	PendingCheckpointID string `json:"pending_checkpoint_id,omitempty"`

	// LastInteractionTime is updated on every hook invocation.
	// Used for stale session detection in "entire doctor".
	LastInteractionTime *time.Time `json:"last_interaction_time,omitempty"`

	// StepCount is the number of checkpoints/steps created in this session.
	// JSON tag kept as "checkpoint_count" for backward compatibility with existing state files.
	StepCount int `json:"checkpoint_count"`

	// CheckpointTranscriptStart is the transcript line offset where the current
	// checkpoint cycle began. Set to 0 at session start, updated to current
	// transcript length after each condensation. Used to scope the transcript
	// for checkpoint condensation: "everything since last checkpoint".
	CheckpointTranscriptStart int `json:"checkpoint_transcript_start,omitempty"`

	// Deprecated: CondensedTranscriptLines is replaced by CheckpointTranscriptStart.
	// Kept for backward compatibility with existing state files.
	// Use NormalizeAfterLoad() to migrate.
	CondensedTranscriptLines int `json:"condensed_transcript_lines,omitempty"`

	// UntrackedFilesAtStart tracks files that existed at session start (to preserve during rewind)
	UntrackedFilesAtStart []string `json:"untracked_files_at_start,omitempty"`

	// FilesTouched tracks files modified/created/deleted during this session
	FilesTouched []string `json:"files_touched,omitempty"`

	// LastCheckpointID is the checkpoint ID from last condensation, reused for subsequent commits without new content
	LastCheckpointID id.CheckpointID `json:"last_checkpoint_id,omitempty"`

	// AgentType identifies the agent that created this session (e.g., "Claude Code", "Gemini CLI", "Cursor")
	AgentType agent.AgentType `json:"agent_type,omitempty"`

	// Token usage tracking (accumulated across all checkpoints in this session)
	TokenUsage *agent.TokenUsage `json:"token_usage,omitempty"`

	// Deprecated: TranscriptLinesAtStart is replaced by CheckpointTranscriptStart.
	// Kept for backward compatibility with existing state files.
	TranscriptLinesAtStart int `json:"transcript_lines_at_start,omitempty"`

	// TranscriptIdentifierAtStart is the last transcript identifier when the session started.
	// Used for identifier-based transcript scoping (UUID for Claude, message ID for Gemini).
	TranscriptIdentifierAtStart string `json:"transcript_identifier_at_start,omitempty"`

	// TranscriptPath is the path to the live transcript file (for mid-session commit detection)
	TranscriptPath string `json:"transcript_path,omitempty"`

	// TranscriptLeafID is the active transcript leaf identifier for tree-based agents (Pi).
	// Used during post-hoc condensation to reconstruct the active branch when the transcript
	// contains multiple branches in a single JSONL file.
	TranscriptLeafID string `json:"transcript_leaf_id,omitempty"`

	// FirstPrompt is the first user prompt that started this session (truncated for display)
	FirstPrompt string `json:"first_prompt,omitempty"`

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

// NormalizeAfterLoad applies backward-compatible migrations to state loaded from disk.
// Call this after deserializing a State from JSON.
func (s *State) NormalizeAfterLoad() {
	// Migrate transcript fields: CheckpointTranscriptStart replaces both
	// CondensedTranscriptLines and TranscriptLinesAtStart from older state files.
	if s.CheckpointTranscriptStart == 0 {
		if s.CondensedTranscriptLines > 0 {
			s.CheckpointTranscriptStart = s.CondensedTranscriptLines
		} else if s.TranscriptLinesAtStart > 0 {
			s.CheckpointTranscriptStart = s.TranscriptLinesAtStart
		}
	}
	// Clear deprecated fields so they aren't re-persisted.
	// Note: this is a one-way migration. If the state is re-saved, older CLI versions
	// will see 0 for these fields and fall back to scoping from the transcript start.
	// This is acceptable since CLI upgrades are monotonic and the worst case is
	// redundant transcript content in a condensation, not data loss.
	s.CondensedTranscriptLines = 0
	s.TranscriptLinesAtStart = 0

	// Backfill AttributionBaseCommit for sessions created before this field existed.
	// Without this, a mid-turn commit would migrate BaseCommit and the fallback in
	// calculateSessionAttributions would use the migrated value, producing zero attribution.
	if s.AttributionBaseCommit == "" && s.BaseCommit != "" {
		s.AttributionBaseCommit = s.BaseCommit
	}
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
		stateDir: filepath.Join(commonDir, SessionStateDirName),
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
	state.NormalizeAfterLoad()
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
