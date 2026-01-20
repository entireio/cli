// Package strategy provides an interface for different git strategies
// that can be used to save and manage Claude Code session changes.
package strategy

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"

	"entire.io/cli/cmd/entire/cli/checkpoint"
)

// ErrNoMetadata is returned when a commit does not have an Entire metadata trailer.
var ErrNoMetadata = errors.New("commit has no entire metadata")

// ErrNoSession is returned when no session info is available.
var ErrNoSession = errors.New("no session info available")

// ErrNotTaskCheckpoint is returned when a rewind point is not a task checkpoint.
var ErrNotTaskCheckpoint = errors.New("not a task checkpoint")

// ErrNotImplemented is returned when a feature is not yet implemented.
var ErrNotImplemented = errors.New("not implemented")

// IncrementalTypeTaskStart is the incremental type for the starting checkpoint
// created during PreToolUse[Task] hook before a subagent begins execution.
const IncrementalTypeTaskStart = "TaskStart"

// ShadowBranchConflictError is returned when a shadow branch already exists
// with activity from a different session or worktree.
type ShadowBranchConflictError struct {
	Branch           string    // The shadow branch name (e.g., "entire/abc1234")
	ExistingSession  string    // Session ID of the existing session
	ExistingWorktree string    // Worktree path of the existing session
	LastActivity     time.Time // When the existing session was last active
	CurrentSession   string    // Session ID of the current session attempting to start
	CurrentWorktree  string    // Worktree path of the current session
}

func (e *ShadowBranchConflictError) Error() string {
	return "shadow branch conflict: existing session from different worktree"
}

// SessionIDConflictError is returned when trying to start a new session
// but the shadow branch already has commits from a different session ID.
// This prevents orphaning existing session work.
type SessionIDConflictError struct {
	ExistingSession string // Session ID found in the shadow branch
	NewSession      string // Session ID being initialized
	ShadowBranch    string // The shadow branch name (e.g., "entire/abc1234")
}

func (e *SessionIDConflictError) Error() string {
	return "session ID conflict: shadow branch has commits from a different session"
}

// ExtractToolUseIDFromTaskMetadataDir extracts the ToolUseID from a task metadata directory path.
// Task metadata dirs have format: .entire/metadata/<session>/tasks/<toolUseID>
// Returns empty string if not a task metadata directory.
func ExtractToolUseIDFromTaskMetadataDir(metadataDir string) string {
	parts := strings.Split(metadataDir, "/")
	if len(parts) >= 2 && parts[len(parts)-2] == "tasks" {
		return parts[len(parts)-1]
	}
	return ""
}

// SessionInfo contains information about the current session state.
// This is used to generate trailers for linking commits to their AI session.
type SessionInfo struct {
	// SessionID is the session identifier extracted from the latest commit's metadata
	SessionID string

	// Reference is a strategy-specific reference string.
	// For manual-commit strategy: "entire/abc1234" (the shadow branch name)
	// Empty for commit strategy (metadata is in the same commit).
	Reference string

	// CommitHash is the full SHA of the commit containing the session metadata.
	// Empty for commit strategy.
	CommitHash string
}

// RewindPoint represents a point to which the user can rewind.
// This abstraction allows different strategies to use different
// identifiers (commit hashes, branch names, stash refs, etc.)
type RewindPoint struct {
	// ID is the unique identifier for this rewind point
	// (commit hash, branch name, stash ref, etc.)
	ID string

	// Message is the human-readable description/summary
	Message string

	// MetadataDir is the path to the metadata directory
	MetadataDir string

	// Date is when this rewind point was created
	Date time.Time

	// IsTaskCheckpoint indicates if this is a task checkpoint (vs a session checkpoint)
	IsTaskCheckpoint bool

	// ToolUseID is the tool use ID for task checkpoints (empty for session checkpoints)
	ToolUseID string

	// IsLogsOnly indicates this is a commit with session logs but no shadow branch state.
	// The logs can be restored from entire/sessions, but file state requires git checkout.
	IsLogsOnly bool

	// CheckpointID is the stable 12-hex-char identifier for logs-only points.
	// Used to retrieve logs from entire/sessions/<id[:2]>/<id[2:]>/full.jsonl
	CheckpointID string

	// Agent is the human-readable name of the agent that created this checkpoint
	// (e.g., "Claude Code", "Cursor")
	Agent string

	// SessionID is the session identifier for this checkpoint.
	// Used to distinguish checkpoints from different concurrent sessions.
	SessionID string

	// SessionPrompt is the initial prompt that started this session.
	// Used to help users identify which session a checkpoint belongs to.
	SessionPrompt string

	// SessionCount is the number of sessions in this checkpoint (1 for single-session).
	// Only populated for logs-only points with multi-session checkpoints.
	SessionCount int

	// SessionIDs contains all session IDs when this is a multi-session checkpoint.
	// The last entry is the most recent session (same as SessionID).
	// Only populated for logs-only points with multi-session checkpoints.
	SessionIDs []string

	// SessionPrompts contains the first prompt for each session (parallel to SessionIDs).
	// Used to display context when showing resume commands for multi-session checkpoints.
	SessionPrompts []string
}

// RewindPreview describes what will happen when rewinding to a checkpoint.
// Used to warn users about files that will be modified or deleted.
type RewindPreview struct {
	// FilesToRestore are files from the checkpoint that will be written/restored.
	FilesToRestore []string

	// FilesToDelete are untracked files that will be removed.
	// These are files created after the checkpoint that aren't in the checkpoint tree
	// and weren't present at session start.
	FilesToDelete []string

	// TrackedChanges are tracked files with uncommitted changes that will be reverted.
	// These come from the existing CanRewind() warning.
	TrackedChanges []string
}

// SaveContext contains all information needed for saving changes.
// All file paths should be pre-filtered and normalized by the CLI layer.
type SaveContext struct {
	// SessionID is the Claude Code session identifier
	SessionID string

	// ModifiedFiles is the list of files modified during the session
	// (extracted from the transcript, already filtered and relative)
	ModifiedFiles []string

	// NewFiles is the list of new files created during the session
	// (pre-computed by CLI from pre-prompt state comparison)
	NewFiles []string

	// DeletedFiles is the list of files deleted during the session
	// (tracked files that no longer exist)
	DeletedFiles []string

	// MetadataDir is the path to the session metadata directory
	MetadataDir string

	// MetadataDirAbs is the absolute path to the session metadata directory
	MetadataDirAbs string

	// CommitMessage is the generated commit message
	CommitMessage string

	// TranscriptPath is the path to the transcript file
	TranscriptPath string

	// AuthorName is the name to use for commits
	AuthorName string

	// AuthorEmail is the email to use for commits
	AuthorEmail string

	// AgentType is the human-readable agent name (e.g., "Claude Code", "Cursor")
	AgentType string

	// Transcript position at checkpoint start - tracks what was added during this checkpoint
	TranscriptUUIDAtStart  string // Last UUID when checkpoint started
	TranscriptLinesAtStart int    // Line count when checkpoint started

	// TokenUsage contains the token usage for this checkpoint
	TokenUsage *checkpoint.TokenUsage
}

// TaskCheckpointContext contains all information needed for saving a task checkpoint.
// This is called by the PostToolUse[Task] hook when a subagent completes.
// The strategy is responsible for creating metadata structures and storing them
// according to its storage approach.
type TaskCheckpointContext struct {
	// SessionID is the Claude Code session identifier
	SessionID string

	// ToolUseID is the unique identifier for this Task tool invocation
	ToolUseID string

	// AgentID is the subagent identifier (from tool_response.agentId)
	AgentID string

	// ModifiedFiles is the list of files modified by the subagent
	// (extracted from the subagent's transcript)
	ModifiedFiles []string

	// NewFiles is the list of new files created by the subagent
	// (computed from pre-task state comparison)
	NewFiles []string

	// DeletedFiles is the list of files deleted by the subagent
	DeletedFiles []string

	// TranscriptPath is the path to the main session transcript
	TranscriptPath string

	// SubagentTranscriptPath is the path to the subagent's transcript (if available)
	SubagentTranscriptPath string

	// CheckpointUUID is the UUID for transcript truncation when rewinding
	CheckpointUUID string

	// AuthorName is the name to use for commits
	AuthorName string

	// AuthorEmail is the email to use for commits
	AuthorEmail string

	// IsIncremental indicates this is an incremental checkpoint during task execution,
	// not a final task completion checkpoint. When true:
	// - Writes to checkpoints/NNN-{tool-use-id}.json instead of checkpoint.json
	// - Skips transcript handling
	// - Uses incremental commit message
	IsIncremental bool

	// IncrementalSequence is the checkpoint sequence number (1, 2, 3, ...)
	// Only used when IsIncremental is true
	IncrementalSequence int

	// IncrementalType is the tool that triggered this checkpoint ("TodoWrite", "Edit", etc.)
	// Only used when IsIncremental is true
	IncrementalType string

	// IncrementalData is the tool_input payload for this checkpoint
	// Only used when IsIncremental is true
	IncrementalData json.RawMessage

	// SubagentType is the type of subagent (e.g., "dev", "reviewer")
	// Extracted from tool_input.subagent_type in Task tool
	// Used for descriptive commit messages
	SubagentType string

	// TaskDescription is the task description provided to the subagent
	// Extracted from tool_input.description in Task tool
	// Used for descriptive commit messages
	TaskDescription string

	// TodoContent is the content of the in-progress todo item
	// Extracted from tool_input.todos where status == "in_progress"
	// Used for descriptive incremental checkpoint messages
	TodoContent string

	// AgentType is the human-readable agent name (e.g., "Claude Code", "Cursor")
	AgentType string
}

// TaskCheckpoint contains the checkpoint information written to checkpoint.json
type TaskCheckpoint struct {
	SessionID      string `json:"session_id"`
	ToolUseID      string `json:"tool_use_id"`
	CheckpointUUID string `json:"checkpoint_uuid"`
	AgentID        string `json:"agent_id,omitempty"`
}

// SubagentCheckpoint represents an intermediate checkpoint created during subagent execution.
// These are created by PostToolUse hooks for tools like TodoWrite, Edit, Write.
type SubagentCheckpoint struct {
	Type      string          `json:"type"`        // Tool name: "TodoWrite", "Edit", "Write"
	ToolUseID string          `json:"tool_use_id"` // The tool use ID that created this checkpoint
	Timestamp time.Time       `json:"timestamp"`   // When the checkpoint was created
	Data      json.RawMessage `json:"data"`        // Type-specific payload (tool_input)
}

// TaskMetadataDir returns the path to a task's metadata directory
// within the session metadata directory.
func TaskMetadataDir(sessionMetadataDir, toolUseID string) string {
	return sessionMetadataDir + "/tasks/" + toolUseID
}

// ReadTaskCheckpoint reads the checkpoint.json file from a task metadata directory.
// This is used during rewind to get the checkpoint UUID for transcript truncation.
func ReadTaskCheckpoint(taskMetadataDir string) (*TaskCheckpoint, error) {
	checkpointFile := taskMetadataDir + "/checkpoint.json"

	data, err := os.ReadFile(checkpointFile) //nolint:gosec // Reading from controlled git metadata path
	if err != nil {
		return nil, err
	}

	var checkpoint TaskCheckpoint
	if err := json.Unmarshal(data, &checkpoint); err != nil {
		return nil, err
	}

	return &checkpoint, nil
}

// Strategy defines the interface for git operation strategies.
// Different implementations can use commits, branches, stashes, etc.
//
// Note: State capture (tracking untracked files before a session) is handled
// by the CLI layer, not the strategy. The strategy receives pre-computed
// file lists in SaveContext.
type Strategy interface {
	// Name returns the strategy identifier (e.g., "commit", "branch", "stash")
	Name() string

	// Description returns a human-readable description for the setup wizard
	Description() string

	// AllowsMainBranch returns true if this strategy can safely operate on the
	// default branch (main/master). Strategies that don't modify the working
	// branch's commit history (like manual-commit strategies) can return true.
	// Strategies that create commits on the working branch should return false
	// to prevent polluting main branch history.
	AllowsMainBranch() bool

	// ValidateRepository checks if the repository is in a valid state
	// for this strategy to operate. Returns an error if validation fails.
	ValidateRepository() error

	// SaveChanges is called on Stop to save all session changes
	// using this strategy's approach (commit, branch, stash, etc.)
	SaveChanges(ctx SaveContext) error

	// SaveTaskCheckpoint is called by PostToolUse[Task] hook when a subagent completes.
	// Creates a checkpoint commit with task metadata for later rewind.
	// Different strategies may handle this differently:
	// - Commit strategy: commits to active branch
	// - Manual-commit strategy: commits to shadow branch
	// - Auto-commit strategy: commits logs to shadow only (code deferred to Stop)
	SaveTaskCheckpoint(ctx TaskCheckpointContext) error

	// GetRewindPoints returns available points to rewind to.
	// The limit parameter controls the maximum number of points to return.
	GetRewindPoints(limit int) ([]RewindPoint, error)

	// Rewind restores the repository to the given rewind point.
	// The metadataDir in the point is used to restore the session transcript.
	Rewind(point RewindPoint) error

	// CanRewind checks if rewinding is currently possible.
	// Returns (canRewind, reason if not, error)
	CanRewind() (bool, string, error)

	// PreviewRewind returns what will happen if rewinding to the given point.
	// This allows showing warnings about files that will be deleted before the rewind.
	// Returns nil if preview is not supported (e.g., auto-commit strategy).
	PreviewRewind(point RewindPoint) (*RewindPreview, error)

	// GetSessionLog returns the raw session log (full.log) and session ID for a given commit.
	// Returns ErrNoMetadata if the commit doesn't have an Entire metadata trailer.
	// The session ID is extracted from the commit trailers or metadata directory.
	GetSessionLog(checkpointID string) (logContent []byte, sessionID string, err error)

	// GetTaskCheckpoint returns the task checkpoint for a given rewind point.
	// For strategies that store checkpoints in git (auto-commit), this reads from the branch.
	// For strategies that store checkpoints on disk (commit, manual-commit), this reads from the filesystem.
	// Returns nil, nil if not a task checkpoint or checkpoint not found.
	GetTaskCheckpoint(point RewindPoint) (*TaskCheckpoint, error)

	// GetTaskCheckpointTranscript returns the session transcript for a task checkpoint.
	// For strategies that store transcripts in git (auto-commit), this reads from the branch.
	// For strategies that store transcripts on disk (commit, manual-commit), this reads from the filesystem.
	GetTaskCheckpointTranscript(point RewindPoint) ([]byte, error)

	// GetSessionInfo returns session information for linking commits.
	// This is used by the context command to generate trailers.
	// Returns ErrNoSession if no session info is available.
	GetSessionInfo() (*SessionInfo, error)

	// EnsureSetup ensures the strategy's required setup is in place,
	// installing any missing pieces (git hooks, gitignore entries, etc.).
	// Returns nil if setup is complete or was successfully installed.
	EnsureSetup() error

	// NOTE: ListSessions and GetSession are standalone functions in session.go.
	// They read from entire/sessions and merge with SessionSource if implemented.

	// GetMetadataRef returns a reference to the metadata commit for the given checkpoint.
	// Format: "<branch>@<commit-sha>" (e.g., "entire/sessions@abc123").
	// Returns empty string if not applicable (e.g., commit strategy with filesystem metadata).
	GetMetadataRef(checkpoint Checkpoint) string

	// GetSessionMetadataRef returns a reference to the most recent metadata commit for a session.
	// Format: "<branch>@<commit-sha>" (e.g., "entire/sessions@abc123").
	// Returns empty string if not applicable or session not found.
	GetSessionMetadataRef(sessionID string) string

	// GetSessionContext returns the context.md content for a session.
	// Returns empty string if not available.
	GetSessionContext(sessionID string) string

	// GetCheckpointLog returns the session transcript for a specific checkpoint.
	// For strategies that store transcripts in git branches (auto-commit, manual-commit),
	// this reads from the checkpoint's commit tree.
	// For strategies that store on disk (commit), reads from the filesystem.
	// Returns ErrNoMetadata if transcript is not available.
	GetCheckpointLog(checkpoint Checkpoint) ([]byte, error)
}

// SessionInitializer is an optional interface for strategies that need to
// initialize session state when a user prompt is submitted.
// Strategies like manual-commit use this to create session state files that
// the git prepare-commit-msg hook can detect.
type SessionInitializer interface {
	// InitializeSession creates session state for a new session.
	// Called during UserPromptSubmit hook before any checkpoints are created.
	// agentType is the human-readable name of the agent (e.g., "Claude Code").
	InitializeSession(sessionID string, agentType string) error
}

// PrepareCommitMsgHandler is an optional interface for strategies that need to
// handle the git prepare-commit-msg hook.
type PrepareCommitMsgHandler interface {
	// PrepareCommitMsg is called by the git prepare-commit-msg hook.
	// It can modify the commit message file to add trailers, etc.
	// The source parameter indicates how the commit was initiated:
	//   - "" or "template": normal editor flow
	//   - "message": using -m or -F flag
	//   - "merge": merge commit
	//   - "squash": squash merge
	//   - "commit": amend with -c/-C
	// Should return nil on errors to not block commits (log warnings to stderr).
	PrepareCommitMsg(commitMsgFile string, source string) error
}

// PostCommitHandler is an optional interface for strategies that need to
// handle the git post-commit hook.
type PostCommitHandler interface {
	// PostCommit is called by the git post-commit hook after a commit is created.
	// Used to perform actions like condensing session data after commits.
	// Should return nil on errors to not block subsequent operations (log warnings to stderr).
	PostCommit() error
}

// CommitMsgHandler is an optional interface for strategies that need to
// handle the git commit-msg hook.
type CommitMsgHandler interface {
	// CommitMsg is called by the git commit-msg hook after the user edits the message.
	// Used to validate or modify the final commit message before the commit is created.
	// If this returns an error, the commit is aborted.
	CommitMsg(commitMsgFile string) error
}

// PrePushHandler is an optional interface for strategies that need to
// handle the git pre-push hook.
type PrePushHandler interface {
	// PrePush is called by the git pre-push hook before pushing to a remote.
	// Used to push session branches (e.g., entire/sessions) alongside user pushes.
	// The remote parameter is the name of the remote being pushed to.
	// Should return nil on errors to not block pushes (log warnings to stderr).
	PrePush(remote string) error
}

// LogsOnlyRestorer is an optional interface for strategies that support
// restoring session logs without file state restoration.
// This is used for "logs-only" rewind points where only the session transcript
// can be restored (file state requires git checkout).
type LogsOnlyRestorer interface {
	// RestoreLogsOnly restores session logs from a logs-only rewind point.
	// Does not modify the working directory - only restores the transcript
	// to Claude's project directory.
	// If force is false, prompts for confirmation when local logs have newer timestamps.
	RestoreLogsOnly(point RewindPoint, force bool) error
}

// SessionResetter is an optional interface for strategies that support
// resetting session state and shadow branches.
// This is used by the "rewind reset" command to clean up shadow branches
// and session state when a user wants to start fresh.
type SessionResetter interface {
	// Reset deletes the shadow branch and session state for the current HEAD.
	// Returns nil if there's nothing to reset (no shadow branch).
	// The force parameter suppresses confirmation prompts.
	Reset(force bool) error
}

// ConcurrentSessionChecker is an optional interface for strategies that support
// detecting and warning about concurrent sessions with uncommitted changes.
// This is used by the UserPromptSubmit hook to block the first prompt when
// another session has uncommitted checkpoints on the same base commit.
type ConcurrentSessionChecker interface {
	// HasOtherActiveSessionsWithCheckpoints returns a session state if there's another
	// active session with uncommitted checkpoints on the same base commit.
	// Returns nil, nil if no such session exists.
	HasOtherActiveSessionsWithCheckpoints(currentSessionID string) (*SessionState, error)
}

// SessionSource is an optional interface for strategies that provide additional
// sessions beyond those stored on the entire/sessions branch.
// For example, manual-commit strategy provides active sessions from .git/entire-sessions/
// that haven't yet been condensed to entire/sessions.
//
// ListSessions() automatically discovers all registered strategies, checks if they
// implement SessionSource, and merges their additional sessions by ID.
type SessionSource interface {
	// GetAdditionalSessions returns sessions not yet on entire/sessions branch.
	GetAdditionalSessions() ([]*Session, error)
}

// OrphanedItemsLister is an optional interface for strategies that can identify
// orphaned items (shadow branches, session states, checkpoints) that should be
// cleaned up. This is used by the "entire session cleanup" command.
//
// ListAllCleanupItems() automatically discovers all registered strategies, checks
// if they implement OrphanedItemsLister, and combines their orphaned items.
type OrphanedItemsLister interface {
	// ListOrphanedItems returns items created by this strategy that are now orphaned.
	// Each strategy defines what "orphaned" means for its own data structures.
	ListOrphanedItems() ([]CleanupItem, error)
}
