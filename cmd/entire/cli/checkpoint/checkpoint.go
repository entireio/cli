// Package checkpoint provides types and interfaces for checkpoint storage.
//
// A Checkpoint captures a point-in-time within a session, containing either
// full state (Temporary) or metadata with a commit reference (Committed).
//
// See docs/architecture/sessions-and-checkpoints.md for the full domain model.
package checkpoint

import (
	"context"
	"errors"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
)

// Errors returned by checkpoint operations.
var (
	// ErrCheckpointNotFound is returned when a checkpoint ID doesn't exist.
	ErrCheckpointNotFound = errors.New("checkpoint not found")

	// ErrNoTranscript is returned when a checkpoint exists but has no transcript.
	ErrNoTranscript = errors.New("no transcript found for checkpoint")
)

// Checkpoint represents a save point within a session.
type Checkpoint struct {
	// ID is the unique checkpoint identifier
	ID string

	// SessionID is the session this checkpoint belongs to
	SessionID string

	// Timestamp is when this checkpoint was created
	Timestamp time.Time

	// Type indicates temporary (full state) or committed (metadata only)
	Type Type

	// Message is a human-readable description of the checkpoint
	Message string
}

// Type indicates the storage location and lifecycle of a checkpoint.
type Type int

const (
	// Temporary checkpoints contain full state (code + metadata) and are stored
	// on shadow branches (entire/<commit-hash>). Used for intra-session rewind.
	Temporary Type = iota

	// Committed checkpoints contain metadata + commit reference and are stored
	// on the entire/sessions branch. They are the permanent record.
	Committed
)

// Store provides low-level primitives for reading and writing checkpoints.
// This is used by strategies to implement their storage approach.
//
// The interface matches the GitStore implementation signatures directly:
// - WriteTemporary takes WriteTemporaryOptions and returns a result with commit hash and skip status
// - ReadTemporary takes baseCommit (not sessionID) since shadow branches are keyed by commit
// - List methods return implementation-specific info types for richer data
type Store interface {
	// WriteTemporary writes a temporary checkpoint (full state) to a shadow branch.
	// Shadow branches are named entire/<base-commit-short-hash>.
	// Returns a result containing the commit hash and whether the checkpoint was skipped.
	// Checkpoints are skipped (deduplicated) when the tree hash matches the previous checkpoint.
	WriteTemporary(ctx context.Context, opts WriteTemporaryOptions) (WriteTemporaryResult, error)

	// ReadTemporary reads the latest checkpoint from a shadow branch.
	// baseCommit is the commit hash the session is based on.
	// Returns nil, nil if the shadow branch doesn't exist.
	ReadTemporary(ctx context.Context, baseCommit string) (*ReadTemporaryResult, error)

	// ListTemporary lists all shadow branches with their checkpoint info.
	ListTemporary(ctx context.Context) ([]TemporaryInfo, error)

	// WriteCommitted writes a committed checkpoint to the entire/sessions branch.
	// Checkpoints are stored at sharded paths: <id[:2]>/<id[2:]>/
	WriteCommitted(ctx context.Context, opts WriteCommittedOptions) error

	// ReadCommitted reads a committed checkpoint by ID.
	// Returns nil, nil if the checkpoint does not exist.
	ReadCommitted(ctx context.Context, checkpointID string) (*ReadCommittedResult, error)

	// ListCommitted lists all committed checkpoints.
	ListCommitted(ctx context.Context) ([]CommittedInfo, error)
}

// WriteTemporaryResult contains the result of writing a temporary checkpoint.
type WriteTemporaryResult struct {
	// CommitHash is the hash of the created or existing checkpoint commit
	CommitHash plumbing.Hash

	// Skipped is true if the checkpoint was skipped due to no changes
	// (tree hash matched the previous checkpoint)
	Skipped bool
}

// WriteTemporaryOptions contains options for writing a temporary checkpoint.
type WriteTemporaryOptions struct {
	// SessionID is the session identifier
	SessionID string

	// BaseCommit is the commit hash this session is based on
	BaseCommit string

	// ModifiedFiles are files that have been modified (relative paths)
	ModifiedFiles []string

	// NewFiles are files that have been created (relative paths)
	NewFiles []string

	// DeletedFiles are files that have been deleted (relative paths)
	DeletedFiles []string

	// MetadataDir is the relative path to the metadata directory
	MetadataDir string

	// MetadataDirAbs is the absolute path to the metadata directory
	MetadataDirAbs string

	// CommitMessage is the commit subject line
	CommitMessage string

	// AuthorName is the name to use for commits
	AuthorName string

	// AuthorEmail is the email to use for commits
	AuthorEmail string

	// IsFirstCheckpoint indicates if this is the first checkpoint of the session
	// When true, all working directory files are captured (not just modified)
	IsFirstCheckpoint bool
}

// ReadTemporaryResult contains the result of reading a temporary checkpoint.
type ReadTemporaryResult struct {
	// CommitHash is the hash of the checkpoint commit
	CommitHash plumbing.Hash

	// TreeHash is the hash of the tree containing the checkpoint state
	TreeHash plumbing.Hash

	// SessionID is the session identifier from the commit trailer
	SessionID string

	// MetadataDir is the metadata directory path from the commit trailer
	MetadataDir string

	// Timestamp is when the checkpoint was created
	Timestamp time.Time
}

// TemporaryInfo contains summary information about a shadow branch.
type TemporaryInfo struct {
	// BranchName is the full branch name (e.g., "entire/abc1234")
	BranchName string

	// BaseCommit is the short commit hash this branch is based on
	BaseCommit string

	// LatestCommit is the hash of the latest commit on the branch
	LatestCommit plumbing.Hash

	// SessionID is the session identifier from the latest commit
	SessionID string

	// Timestamp is when the latest checkpoint was created
	Timestamp time.Time
}

// WriteCommittedOptions contains options for writing a committed checkpoint.
type WriteCommittedOptions struct {
	// CheckpointID is the stable 12-hex-char identifier
	CheckpointID string

	// SessionID is the session identifier
	SessionID string

	// Strategy is the name of the strategy that created this checkpoint
	Strategy string

	// Branch is the branch name where the checkpoint was created (empty if detached HEAD)
	Branch string

	// Transcript is the session transcript content (full.jsonl)
	Transcript []byte

	// Prompts contains user prompts from the session
	Prompts []string

	// Context is the generated context.md content
	Context []byte

	// FilesTouched are files modified during the session
	FilesTouched []string

	// CheckpointsCount is the number of checkpoints in this session
	CheckpointsCount int

	// EphemeralBranch is the shadow branch name (for manual-commit strategy)
	EphemeralBranch string

	// AuthorName is the name to use for commits
	AuthorName string

	// AuthorEmail is the email to use for commits
	AuthorEmail string

	// MetadataDir is a directory containing additional metadata files to copy
	// If set, all files in this directory will be copied to the checkpoint path
	// This is useful for copying task metadata files, subagent transcripts, etc.
	MetadataDir string

	// Task checkpoint fields (for auto-commit strategy task checkpoints)
	IsTask    bool   // Whether this is a task checkpoint
	ToolUseID string // Tool use ID for task checkpoints

	// Additional task checkpoint fields for subagent checkpoints
	AgentID                string // Subagent identifier
	CheckpointUUID         string // UUID for transcript truncation when rewinding
	TranscriptPath         string // Path to session transcript file (alternative to in-memory Transcript)
	SubagentTranscriptPath string // Path to subagent's transcript file

	// Incremental checkpoint fields
	IsIncremental       bool   // Whether this is an incremental checkpoint
	IncrementalSequence int    // Checkpoint sequence number
	IncrementalType     string // Tool type that triggered this checkpoint
	IncrementalData     []byte // Tool input payload for this checkpoint

	// Commit message fields (used for task checkpoints)
	CommitSubject string // Subject line for the metadata commit (overrides default)

	// Agent identifies the agent that created this checkpoint (e.g., "Claude Code", "Cursor")
	Agent string

	// Transcript position at checkpoint start - tracks what was added during this checkpoint
	TranscriptUUIDAtStart  string // Last UUID when checkpoint started
	TranscriptLinesAtStart int    // Line count when checkpoint started

	// TokenUsage contains the token usage for this checkpoint
	TokenUsage *TokenUsage
}

// ReadCommittedResult contains the result of reading a committed checkpoint.
type ReadCommittedResult struct {
	// Metadata contains the checkpoint metadata
	Metadata CommittedMetadata

	// Transcript is the session transcript content (most recent session)
	Transcript []byte

	// Prompts contains user prompts (most recent session)
	Prompts string

	// Context is the context.md content
	Context string

	// ArchivedSessions contains transcripts from previous sessions when multiple
	// sessions were condensed to the same checkpoint. Ordered from oldest to newest
	// (1/, 2/, etc.). The root-level Transcript is the most recent session.
	ArchivedSessions []ArchivedSession
}

// ArchivedSession contains transcript data from a previous session
// that was archived when multiple sessions contributed to the same checkpoint.
type ArchivedSession struct {
	// SessionID is the session identifier for this archived session
	SessionID string

	// Transcript is the session transcript content
	Transcript []byte

	// Prompts contains user prompts from this session
	Prompts string

	// FolderIndex is the archive folder number (1, 2, etc.)
	FolderIndex int
}

// CommittedInfo contains summary information about a committed checkpoint.
type CommittedInfo struct {
	// CheckpointID is the stable 12-hex-char identifier
	CheckpointID string

	// SessionID is the session identifier (most recent session for multi-session checkpoints)
	SessionID string

	// CreatedAt is when the checkpoint was created
	CreatedAt time.Time

	// CheckpointsCount is the total number of checkpoints across all sessions
	CheckpointsCount int

	// FilesTouched are files modified during all sessions
	FilesTouched []string

	// Agent identifies the agent that created this checkpoint
	Agent string

	// IsTask indicates if this is a task checkpoint
	IsTask bool

	// ToolUseID is the tool use ID for task checkpoints
	ToolUseID string

	// Multi-session support
	SessionCount int      // Number of sessions (1 if single session)
	SessionIDs   []string // All session IDs that contributed
}

// CommittedMetadata contains the metadata stored in metadata.json for each checkpoint.
type CommittedMetadata struct {
	CheckpointID     string    `json:"checkpoint_id"`
	SessionID        string    `json:"session_id"`
	Strategy         string    `json:"strategy"`
	CreatedAt        time.Time `json:"created_at"`
	Branch           string    `json:"branch,omitempty"` // Branch where checkpoint was created (empty if detached HEAD)
	CheckpointsCount int       `json:"checkpoints_count"`
	FilesTouched     []string  `json:"files_touched"`

	// Agent identifies the agent that created this checkpoint (e.g., "Claude Code", "Cursor")
	Agent string `json:"agent,omitempty"`

	// Multi-session support: when multiple sessions contribute to the same checkpoint
	SessionCount int      `json:"session_count,omitempty"` // Number of sessions (1 if omitted for backwards compat)
	SessionIDs   []string `json:"session_ids,omitempty"`   // All session IDs that contributed

	// Task checkpoint fields (only populated for task checkpoints)
	IsTask    bool   `json:"is_task,omitempty"`
	ToolUseID string `json:"tool_use_id,omitempty"`

	// Transcript position at checkpoint start - tracks what was added during this checkpoint
	TranscriptUUIDAtStart  string `json:"transcript_uuid_at_start,omitempty"`  // Last UUID when checkpoint started
	TranscriptLinesAtStart int    `json:"transcript_lines_at_start,omitempty"` // Line count when checkpoint started

	// Token usage for this checkpoint
	TokenUsage *TokenUsage `json:"token_usage,omitempty"`
}

// TokenUsage represents aggregated token usage for a checkpoint
type TokenUsage struct {
	// Input tokens (fresh, not from cache)
	InputTokens int `json:"input_tokens"`
	// Tokens written to cache (billable at cache write rate)
	CacheCreationTokens int `json:"cache_creation_tokens"`
	// Tokens read from cache (discounted rate)
	CacheReadTokens int `json:"cache_read_tokens"`
	// Output tokens generated
	OutputTokens int `json:"output_tokens"`
	// Number of API calls made
	APICallCount int `json:"api_call_count"`
	// Subagent token usage (if any agents were spawned)
	SubagentTokens *TokenUsage `json:"subagent_tokens,omitempty"`
}

// Info provides summary information for listing checkpoints.
// This is the generic checkpoint info type.
type Info struct {
	// ID is the checkpoint identifier
	ID string

	// SessionID identifies the session
	SessionID string

	// Type indicates temporary or committed
	Type Type

	// CreatedAt is when the checkpoint was created
	CreatedAt time.Time

	// Message is a summary description
	Message string
}

// WriteTemporaryTaskOptions contains options for writing a task checkpoint.
// Task checkpoints are created when a subagent completes and contain both
// code changes and task-specific metadata.
type WriteTemporaryTaskOptions struct {
	// SessionID is the session identifier
	SessionID string

	// BaseCommit is the commit hash this session is based on
	BaseCommit string

	// ToolUseID is the unique identifier for this Task tool invocation
	ToolUseID string

	// AgentID is the subagent identifier
	AgentID string

	// ModifiedFiles are files that have been modified (relative paths)
	ModifiedFiles []string

	// NewFiles are files that have been created (relative paths)
	NewFiles []string

	// DeletedFiles are files that have been deleted (relative paths)
	DeletedFiles []string

	// TranscriptPath is the path to the main session transcript
	TranscriptPath string

	// SubagentTranscriptPath is the path to the subagent's transcript
	SubagentTranscriptPath string

	// CheckpointUUID is the UUID for transcript truncation when rewinding
	CheckpointUUID string

	// CommitMessage is the commit message (already formatted)
	CommitMessage string

	// AuthorName is the name to use for commits
	AuthorName string

	// AuthorEmail is the email to use for commits
	AuthorEmail string

	// IsIncremental indicates this is an incremental checkpoint
	IsIncremental bool

	// IncrementalSequence is the checkpoint sequence number
	IncrementalSequence int

	// IncrementalType is the tool that triggered this checkpoint
	IncrementalType string

	// IncrementalData is the tool_input payload for this checkpoint
	IncrementalData []byte
}

// TemporaryCheckpointInfo contains information about a single commit on a shadow branch.
// Used by ListTemporaryCheckpoints to provide rewind point data.
type TemporaryCheckpointInfo struct {
	// CommitHash is the hash of the checkpoint commit
	CommitHash plumbing.Hash

	// Message is the first line of the commit message
	Message string

	// SessionID is the session identifier from the Entire-Session trailer
	SessionID string

	// MetadataDir is the metadata directory path from trailers
	MetadataDir string

	// IsTaskCheckpoint indicates if this is a task checkpoint
	IsTaskCheckpoint bool

	// ToolUseID is the tool use ID for task checkpoints
	ToolUseID string

	// Timestamp is when the checkpoint was created
	Timestamp time.Time
}
