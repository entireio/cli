package session

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/validation"
)

const (
	// SessionStateDirName is the directory name for session state files within git common dir.
	SessionStateDirName = "entire-sessions"

	// StaleSessionThreshold is the duration after which an ended session is considered stale
	// and will be automatically deleted during load/list operations.
	StaleSessionThreshold = 7 * 24 * time.Hour

	// StuckActiveThreshold is the duration after which an ACTIVE session with no
	// interaction is considered stuck (used by "entire doctor" and "entire status").
	StuckActiveThreshold = 1 * time.Hour
)

// Kind identifies the purpose of a session. Empty means "normal" (legacy
// sessions + every session that isn't a review). Callers must not rely on
// Kind being set unless they specifically want to branch on it.
type Kind string

const (
	// KindReview tags a session created by `entire review`.
	KindReview Kind = "review"
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

	// Kind tags the session's purpose. Empty for normal agent sessions;
	// set to KindReview when the session was started by `entire review`.
	Kind Kind `json:"kind,omitempty"`

	// ReviewSkills is the snapshot of configured review skills at session start.
	// Preserved so checkpoint metadata records which skills were run.
	ReviewSkills []string `json:"review_skills,omitempty"`

	// TurnID is a unique identifier for the current agent turn.
	// Lifecycle:
	//   - Generated fresh in InitializeSession at each turn start
	//   - Shared across all checkpoints within the same turn
	//   - Used to correlate related checkpoints when a turn's work spans multiple commits
	//   - Persists until the next InitializeSession call generates a new one
	TurnID string `json:"turn_id,omitempty"`

	// TurnCheckpointIDs tracks all checkpoint IDs condensed during the current turn.
	// Lifecycle:
	//   - Set in PostCommit when a checkpoint is condensed for an ACTIVE session
	//   - Consumed in HandleTurnEnd to finalize all checkpoints with the full transcript
	//   - Cleared in HandleTurnEnd after finalization completes
	//   - Cleared in InitializeSession when a new prompt starts
	//   - Cleared when session is reset (ResetSession deletes the state file entirely)
	TurnCheckpointIDs []string `json:"turn_checkpoint_ids,omitempty"`

	// LastInteractionTime is updated on agent-interaction events (TurnStart,
	// TurnEnd, SessionStop, Compaction) but NOT on git commit hooks.
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

	// CheckpointTranscriptSize is the byte size of the transcript at last condensation.
	// Used for fast "has new content?" checks in PostCommit: compare the git blob size
	// against this value without reading the full transcript content.
	CheckpointTranscriptSize int64 `json:"checkpoint_transcript_size,omitempty"`

	// CompactTranscriptStart is the transcript.jsonl line offset where the current
	// checkpoint cycle began. It parallels CheckpointTranscriptStart (full.jsonl)
	// and is updated after each condensation.
	CompactTranscriptStart int `json:"compact_transcript_start,omitempty"`

	// Deprecated: CondensedTranscriptLines is replaced by CheckpointTranscriptStart.
	// Kept for backward compatibility with existing state files.
	// Use NormalizeAfterLoad() to migrate.
	CondensedTranscriptLines int `json:"condensed_transcript_lines,omitempty"`

	// UntrackedFilesAtStart tracks files that existed at session start (to preserve during rewind)
	UntrackedFilesAtStart []string `json:"untracked_files_at_start,omitempty"`

	// FilesTouched tracks files modified/created/deleted during this session
	FilesTouched []string `json:"files_touched,omitempty"`

	// LastCheckpointID is the checkpoint ID from the most recent condensation.
	// Used to restore the Entire-Checkpoint trailer on amend and to identify
	// sessions that have been condensed at least once. Cleared on new prompt.
	LastCheckpointID id.CheckpointID `json:"last_checkpoint_id,omitempty"`

	// LastCheckpointCommitHash is the exact commit SHA that carried
	// LastCheckpointID at condensation time. Used by the reconcile path to
	// distinguish "reset back to the condensed commit" (same SHA) from
	// "cherry-picked / rebased a commit that happens to preserve the trailer"
	// (different SHA). Without this guard, a cherry-picked checkpoint would
	// falsely fire reconcile and drop the pinned AttributionBaseCommit,
	// corrupting attribution math for uncondensed shadow-branch work.
	// Empty for legacy state files — reconcile falls back to trailer-only
	// matching for backward compatibility.
	LastCheckpointCommitHash string `json:"last_checkpoint_commit_hash,omitempty"`

	// FullyCondensed indicates this session has been condensed and has no remaining
	// carry-forward files. PostCommit skips fully-condensed sessions entirely.
	// Set after successful condensation when no files remain for carry-forward
	// and the session phase is ENDED. Cleared on session reactivation (ENDED →
	// ACTIVE via TurnStart, or ENDED → IDLE via SessionStart) by ActionClearEndedAt.
	FullyCondensed bool `json:"fully_condensed,omitempty"`

	// DivergenceNoticeShown indicates the prepare-commit-msg warning about
	// attribution divergence has been shown. Set when the warning fires,
	// cleared when AttributionBaseCommit realigns with BaseCommit (next
	// successful condensation). Prevents repeated warnings on every commit.
	DivergenceNoticeShown bool `json:"divergence_notice_shown,omitempty"`

	// AttachedManually indicates this session was imported via `entire attach` rather
	// than being captured by hooks during normal agent execution.
	AttachedManually bool `json:"attached_manually,omitempty"`

	// AgentType identifies the agent that created this session (e.g., "Claude Code", "Gemini CLI", "Cursor")
	AgentType types.AgentType `json:"agent_type,omitempty"`

	// ModelName is the LLM model used in this session (e.g., "claude-sonnet-4-20250514", "gpt-4o").
	// Set from hook data when the agent provides it.
	ModelName string `json:"model_name,omitempty"`

	// Token usage tracking (accumulated across all checkpoints in this session)
	TokenUsage *agent.TokenUsage `json:"token_usage,omitempty"`

	// Hook-provided session metrics (for agents like Cursor that report via hooks)
	SessionDurationMs int64 `json:"session_duration_ms,omitempty"`
	SessionTurnCount  int   `json:"session_turn_count,omitempty"`
	ContextTokens     int   `json:"context_tokens,omitempty"`
	ContextWindowSize int   `json:"context_window_size,omitempty"`

	// Deprecated: TranscriptLinesAtStart is replaced by CheckpointTranscriptStart.
	// Kept for backward compatibility with existing state files.
	TranscriptLinesAtStart int `json:"transcript_lines_at_start,omitempty"`

	// TranscriptIdentifierAtStart is the last transcript identifier when the session started.
	// Used for identifier-based transcript scoping (UUID for Claude, message ID for Gemini).
	TranscriptIdentifierAtStart string `json:"transcript_identifier_at_start,omitempty"`

	// TranscriptPath is the path to the live transcript file (for mid-session commit detection)
	TranscriptPath string `json:"transcript_path,omitempty"`

	// LastPrompt is the most recent user prompt for this session (truncated for display).
	// Updated on every turn start (UserPromptSubmit). JSON tag kept as "first_prompt"
	// for backward compatibility with existing state files.
	LastPrompt string `json:"last_prompt,omitempty"`

	// PromptAttributions tracks user and agent line changes at each prompt start.
	// This enables accurate attribution by capturing user edits between checkpoints.
	PromptAttributions []PromptAttribution `json:"prompt_attributions,omitempty"`

	// PendingPromptAttribution holds attribution calculated at prompt start (before agent runs).
	// This is moved to PromptAttributions when SaveStep is called.
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

	// UserRemovedPerFile tracks per-file user removals for accurate agent deletion attribution.
	// Without this, global user removals would be subtracted from agent-file-only removals,
	// incorrectly reducing agent deletion credit when users delete lines in non-agent files.
	UserRemovedPerFile map[string]int `json:"user_removed_per_file,omitempty"`
}

// NormalizeAfterLoad applies backward-compatible migrations to state loaded from disk.
// Call this after deserializing a State from JSON.
func (s *State) NormalizeAfterLoad(ctx context.Context) {
	// Normalize legacy phase values. "active_committed" was removed with the
	// 1:1 checkpoint model in favor of the state machine handling commits
	// during ACTIVE phase with immediate condensation.
	if s.Phase == "active_committed" {
		logCtx := logging.WithComponent(ctx, "session")
		logging.Info(logCtx, "migrating legacy active_committed phase to active",
			slog.String("session_id", s.SessionID),
		)
		s.Phase = PhaseActive
	}
	// Also normalize via PhaseFromString to handle any other legacy/unknown values.
	s.Phase = PhaseFromString(string(s.Phase))

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

	// DivergenceNoticeShown is only meaningful while attribution is actually
	// diverged. Self-heal any state file where the flag outlived the divergence
	// — otherwise a future legitimate divergence would be silently suppressed.
	if s.DivergenceNoticeShown && s.AttributionBaseCommit == s.BaseCommit {
		s.DivergenceNoticeShown = false
	}
}

// RealignAttributionBase sets AttributionBaseCommit to newBase and clears any
// bookkeeping whose meaning depends on attribution being diverged from the
// shadow-branch base. Call this every time a code path intentionally brings
// AttributionBaseCommit back in line with BaseCommit (condensation, reconcile,
// post-commit base advance) so a stale DivergenceNoticeShown cannot suppress
// the next legitimate divergence warning.
func (s *State) RealignAttributionBase(newBase string) {
	s.AttributionBaseCommit = newBase
	s.DivergenceNoticeShown = false
}

// IsStale returns true when a session hasn't seen interaction for longer than
// StaleSessionThreshold. Falls back to StartedAt when LastInteractionTime is
// nil (sessions created before interaction tracking was added).
// IsStuckActive returns true if the session is in ACTIVE phase but has not had
// any interaction for longer than StuckActiveThreshold. Falls back to StartedAt
// when LastInteractionTime is nil, so brand-new sessions are not falsely flagged.
func (s *State) IsStuckActive() bool {
	if !s.Phase.IsActive() {
		return false
	}
	ref := s.LastInteractionTime
	if ref == nil {
		ref = &s.StartedAt
	}
	return time.Since(*ref) > StuckActiveThreshold
}

func (s *State) IsStale() bool {
	var since time.Duration
	if s.LastInteractionTime != nil {
		since = time.Since(*s.LastInteractionTime)
	} else {
		since = time.Since(s.StartedAt)
	}
	return since > StaleSessionThreshold
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
func NewStateStore(ctx context.Context) (*StateStore, error) {
	commonDir, err := getGitCommonDir(ctx)
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
// Returns (nil, nil) when session file doesn't exist or session is stale (not an error condition).
// Stale sessions (ended longer than StaleSessionThreshold ago) are automatically deleted.
func (s *StateStore) Load(ctx context.Context, sessionID string) (*State, error) {
	// Validate session ID to prevent path traversal
	if err := validation.ValidateSessionID(sessionID); err != nil {
		return nil, fmt.Errorf("invalid session ID: %w", err)
	}

	root, err := os.OpenRoot(s.stateDir)
	if os.IsNotExist(err) {
		return nil, nil //nolint:nilnil // nil,nil indicates session not found (expected case)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to open session state directory: %w", err)
	}
	defer root.Close()

	fileName := sessionID + ".json"
	data, err := osroot.ReadFile(root, fileName)
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
	state.NormalizeAfterLoad(ctx)

	if state.IsStale() {
		logCtx := logging.WithComponent(ctx, "session")
		logging.Debug(logCtx, "deleting stale session state",
			slog.String("session_id", sessionID),
		)
		_ = s.Clear(ctx, sessionID) //nolint:errcheck // best-effort cleanup of stale session
		return nil, nil             //nolint:nilnil // stale session treated as not found
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

	// Use os.Root for traversal-resistant write of temp file.
	// Rename is not available on os.Root, so we keep using os.Rename.
	root, err := os.OpenRoot(s.stateDir)
	if err != nil {
		return fmt.Errorf("failed to open session state directory: %w", err)
	}
	defer root.Close()

	fileName := state.SessionID + ".json"
	tmpFileName := fileName + ".tmp"
	if err := osroot.WriteFile(root, tmpFileName, data, 0o600); err != nil {
		return fmt.Errorf("failed to write session state: %w", err)
	}

	// Atomic rename: not available on os.Root, use os.Rename with validated paths.
	stateFile := s.stateFilePath(state.SessionID)
	tmpFile := stateFile + ".tmp"
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

	// Remove all files for this session (state .json, .model hint, any future hint files).
	// filepath.Glob finds matches; os.Root ensures traversal-resistant removal.
	matches, _ := filepath.Glob(filepath.Join(s.stateDir, sessionID+".*")) //nolint:errcheck // pattern is always valid
	if len(matches) > 0 {
		root, rootErr := os.OpenRoot(s.stateDir)
		if rootErr != nil {
			return fmt.Errorf("failed to open session state directory for cleanup: %w", rootErr)
		}
		defer root.Close()
		for _, f := range matches {
			_ = osroot.Remove(root, filepath.Base(f)) //nolint:errcheck // best-effort cleanup
		}
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
			continue // Not found or stale (Load handles cleanup)
		}

		states = append(states, state)
	}
	return states, nil
}

// stateFilePath returns the path to a session state file.
func (s *StateStore) stateFilePath(sessionID string) string {
	return filepath.Join(s.stateDir, sessionID+".json")
}

// gitCommonDirCache caches the git common dir to avoid repeated subprocess calls.
// Keyed by working directory to handle directory changes (same pattern as paths.WorktreeRoot).
var (
	gitCommonDirMu       sync.RWMutex
	gitCommonDirCache    string
	gitCommonDirCacheDir string
)

// ClearGitCommonDirCache clears the cached git common dir.
// Useful for testing when changing directories.
func ClearGitCommonDirCache() {
	gitCommonDirMu.Lock()
	gitCommonDirCache = ""
	gitCommonDirCacheDir = ""
	gitCommonDirMu.Unlock()
}

// GetGitCommonDir returns the .git common directory for the current working
// directory. In a regular checkout this is .git/; in a worktree, it's the
// main repo's .git/ (not .git/worktrees/<name>/). Result is cached per
// working directory. This is a public wrapper around the package-internal
// helper for callers outside this package.
func GetGitCommonDir(ctx context.Context) (string, error) {
	return getGitCommonDir(ctx)
}

// getGitCommonDir returns the path to the shared git directory.
// In a regular checkout, this is .git/
// In a worktree, this is the main repo's .git/ (not .git/worktrees/<name>/)
// The result is cached per working directory.
func getGitCommonDir(ctx context.Context) (string, error) {
	cwd, err := os.Getwd() //nolint:forbidigo // used for cache key, not git-relative paths
	if err != nil {
		cwd = ""
	}

	// Check cache with read lock first
	gitCommonDirMu.RLock()
	if gitCommonDirCache != "" && gitCommonDirCacheDir == cwd {
		cached := gitCommonDirCache
		gitCommonDirMu.RUnlock()
		return cached, nil
	}
	gitCommonDirMu.RUnlock()

	// Cache miss — resolve via git subprocess
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
	commonDir = filepath.Clean(commonDir)

	gitCommonDirMu.Lock()
	gitCommonDirCache = commonDir
	gitCommonDirCacheDir = cwd
	gitCommonDirMu.Unlock()

	return commonDir, nil
}
