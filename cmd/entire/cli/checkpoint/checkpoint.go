// Package checkpoint provides types and interfaces for checkpoint storage.
//
// A Checkpoint captures a point-in-time within a session, containing either
// full state (Temporary) or metadata with a commit reference (Committed).
//
// See docs/architecture/sessions-and-checkpoints.md for the full domain model.
package checkpoint

import (
	"context"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"

	"github.com/go-git/go-git/v6/plumbing"
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
	// Ephemeral checkpoints contain full state (code + metadata) and are stored
	// on shadow branches (entire/<commit-hash>). Used for intra-session rewind.
	Ephemeral Type = iota

	// Persistent checkpoints contain metadata + commit reference and are stored
	// on the entire/checkpoints/v1 branch. They are the permanent record.
	Persistent
)

// EphemeralStore provides the production shadow-branch checkpoint surface.
type EphemeralStore interface {
	Write(ctx context.Context, req EphemeralWriteRequest) (WriteEphemeralResult, error)
	Read(ctx context.Context, baseCommit, worktreeID string) (*ReadEphemeralResult, error)
	List(ctx context.Context) ([]EphemeralInfo, error)
	ListCheckpoints(ctx context.Context, baseCommit, worktreeID, sessionID string, limit int) ([]EphemeralCheckpointInfo, error)
	ListCheckpointsForBranch(ctx context.Context, branchName, sessionID string, limit int) ([]EphemeralCheckpointInfo, error)
	ListAllCheckpoints(ctx context.Context, sessionID string, limit int) ([]EphemeralCheckpointInfo, error)
	GetTranscriptFromCommit(ctx context.Context, commitHash plumbing.Hash, metadataDir string, agentType types.AgentType) ([]byte, error)
	ShadowBranchExists(baseCommit, worktreeID string) bool
}

// WriteEphemeralResult contains the result of writing a temporary checkpoint.
type WriteEphemeralResult struct {
	// CommitHash is the hash of the created or existing checkpoint commit
	CommitHash plumbing.Hash

	// Skipped is true if the checkpoint was skipped due to no changes
	// (tree hash matched the previous checkpoint)
	Skipped bool
}

// WriteEphemeralOptions contains options for writing a temporary checkpoint.
type WriteEphemeralOptions struct {
	// SessionID is the session identifier
	SessionID string

	// BaseCommit is the commit hash this session is based on
	BaseCommit string

	// WorktreeID is the internal git worktree identifier (empty for main worktree)
	// Used to create worktree-specific shadow branch names
	WorktreeID string

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

// ReadEphemeralResult contains the result of reading a temporary checkpoint.
type ReadEphemeralResult struct {
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

// EphemeralInfo contains summary information about a shadow branch.
type EphemeralInfo struct {
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

// WriteEphemeralTaskOptions contains options for writing a task checkpoint.
// Task checkpoints are created when a subagent completes and contain both
// code changes and task-specific metadata.
type WriteEphemeralTaskOptions struct {
	// SessionID is the session identifier
	SessionID string

	// BaseCommit is the commit hash this session is based on
	BaseCommit string

	// WorktreeID is the internal git worktree identifier (empty for main worktree)
	// Used to create worktree-specific shadow branch names
	WorktreeID string

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

// EphemeralCheckpointInfo contains information about a single commit on a shadow branch.
// Used by ListCheckpoints to provide rewind point data.
type EphemeralCheckpointInfo struct {
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
