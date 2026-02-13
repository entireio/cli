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

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"

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
	// on the entire/checkpoints/v1 branch. They are the permanent record.
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
	// worktreeID is the internal git worktree identifier (empty for main worktree).
	// Returns nil, nil if the shadow branch doesn't exist.
	ReadTemporary(ctx context.Context, baseCommit, worktreeID string) (*ReadTemporaryResult, error)

	// ListTemporary lists all shadow branches with their checkpoint info.
	ListTemporary(ctx context.Context) ([]TemporaryInfo, error)

	// WriteCommitted writes a committed checkpoint to the entire/checkpoints/v1 branch.
	// Checkpoints are stored at sharded paths: <id[:2]>/<id[2:]>/
	WriteCommitted(ctx context.Context, opts WriteCommittedOptions) error

	// ReadCommitted reads a committed checkpoint's summary by ID.
	// Returns only the CheckpointSummary (paths + aggregated stats), not actual content.
	// Use ReadSessionContent to read actual transcript/prompts/context.
	// Returns nil, nil if the checkpoint does not exist.
	ReadCommitted(ctx context.Context, checkpointID id.CheckpointID) (*CheckpointSummary, error)

	// ReadSessionContent reads the actual content for a specific session within a checkpoint.
	// sessionIndex is 0-based (0 for first session, 1 for second, etc.).
	// Returns the session's metadata, transcript, prompts, and context.
	ReadSessionContent(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (*SessionContent, error)

	// ReadSessionContentByID reads a session's content by its session ID.
	// Useful when you have the session ID but don't know its index within the checkpoint.
	ReadSessionContentByID(ctx context.Context, checkpointID id.CheckpointID, sessionID string) (*SessionContent, error)

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
	CheckpointID id.CheckpointID

	// SessionID is the session identifier
	SessionID string

	// Strategy is the name of the strategy that created this checkpoint
	Strategy string

	// Branch is the branch name where the checkpoint was created (empty if detached HEAD)
	Branch string

	// CommitTreeHash is the git tree hash of HEAD at condensation time.
	// Used as a content-addressable anchor for re-linking after history rewrites (rebase, squash).
	CommitTreeHash string

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
	Agent agent.AgentType

	// Transcript position at checkpoint start - tracks what was added during this checkpoint
	TranscriptIdentifierAtStart string // Last identifier when checkpoint started (UUID for Claude, message ID for Gemini)
	CheckpointTranscriptStart   int    // Transcript line offset at start of this checkpoint's data

	// CheckpointTranscriptStart is written to both CommittedMetadata.CheckpointTranscriptStart
	// and the deprecated CommittedMetadata.TranscriptLinesAtStart for backward compatibility.

	// TokenUsage contains the token usage for this checkpoint
	TokenUsage *agent.TokenUsage

	// InitialAttribution is line-level attribution calculated at commit time
	// comparing checkpoint tree (agent work) to committed tree (may include human edits)
	InitialAttribution *InitialAttribution

	// Summary is an optional AI-generated summary for this checkpoint.
	// This field may be nil when:
	//   - summarization is disabled in settings
	//   - summary generation failed (non-blocking, logged as warning)
	//   - the transcript was empty or too short to summarize
	//   - the checkpoint predates the summarization feature
	Summary *Summary

	// SessionTranscriptPath is the home-relative path to the session transcript file.
	// Persisted in CommittedMetadata so restore can write the transcript back to
	// the correct location without reconstructing agent-specific paths.
	SessionTranscriptPath string
}

// CommittedInfo contains summary information about a committed checkpoint.
type CommittedInfo struct {
	// CheckpointID is the stable 12-hex-char identifier
	CheckpointID id.CheckpointID

	// SessionID is the session identifier (most recent session for multi-session checkpoints)
	SessionID string

	// CreatedAt is when the checkpoint was created
	CreatedAt time.Time

	// CheckpointsCount is the total number of checkpoints across all sessions
	CheckpointsCount int

	// FilesTouched are files modified during all sessions
	FilesTouched []string

	// Agent identifies the agent that created this checkpoint
	Agent agent.AgentType

	// IsTask indicates if this is a task checkpoint
	IsTask bool

	// ToolUseID is the tool use ID for task checkpoints
	ToolUseID string

	// Multi-session support
	SessionCount int      // Number of sessions (1 if single session)
	SessionIDs   []string // All session IDs that contributed
}

// SessionContent contains the actual content for a session.
// This is used when reading full session data (transcript, prompts, context)
// as opposed to just the metadata/summary.
type SessionContent struct {
	// Metadata contains the session-specific metadata
	Metadata CommittedMetadata

	// Transcript is the session transcript content
	Transcript []byte

	// Prompts contains user prompts from this session
	Prompts string

	// Context is the context.md content
	Context string
}

// CommittedMetadata contains the metadata stored in metadata.json for each checkpoint.
type CommittedMetadata struct {
	CLIVersion       string          `json:"cli_version,omitempty"`
	CheckpointID     id.CheckpointID `json:"checkpoint_id"`
	SessionID        string          `json:"session_id"`
	Strategy         string          `json:"strategy"`
	CreatedAt        time.Time       `json:"created_at"`
	Branch           string          `json:"branch,omitempty"`           // Branch where checkpoint was created (empty if detached HEAD)
	CommitTreeHash   string          `json:"commit_tree_hash,omitempty"` // Git tree hash of HEAD at condensation time
	CheckpointsCount int             `json:"checkpoints_count"`
	FilesTouched     []string        `json:"files_touched"`

	// Agent identifies the agent that created this checkpoint (e.g., "Claude Code", "Cursor")
	Agent agent.AgentType `json:"agent,omitempty"`

	// Task checkpoint fields (only populated for task checkpoints)
	IsTask    bool   `json:"is_task,omitempty"`
	ToolUseID string `json:"tool_use_id,omitempty"`

	// Transcript position at checkpoint start - tracks what was added during this checkpoint
	TranscriptIdentifierAtStart string `json:"transcript_identifier_at_start,omitempty"` // Last identifier when checkpoint started (UUID for Claude, message ID for Gemini)
	CheckpointTranscriptStart   int    `json:"checkpoint_transcript_start,omitempty"`    // Transcript line offset at start of this checkpoint's data

	// Deprecated: Use CheckpointTranscriptStart instead. Written for backward compatibility with older CLI versions.
	TranscriptLinesAtStart int `json:"transcript_lines_at_start,omitempty"`

	// Token usage for this checkpoint
	TokenUsage *agent.TokenUsage `json:"token_usage,omitempty"`

	// AI-generated summary of the checkpoint
	Summary *Summary `json:"summary,omitempty"`

	// InitialAttribution is line-level attribution calculated at commit time
	InitialAttribution *InitialAttribution `json:"initial_attribution,omitempty"`

	// TranscriptPath is the home-relative path to the session transcript file.
	// Persisted so restore can write the transcript back to the correct location
	// without needing to reconstruct agent-specific paths (e.g. SHA-256 hashed dirs for Gemini).
	TranscriptPath string `json:"transcript_path,omitempty"`
}

// GetTranscriptStart returns the transcript line offset at which this checkpoint's data begins.
// Returns 0 for new checkpoints (start from beginning). For data written by older CLI versions,
// falls back to the deprecated TranscriptLinesAtStart field.
func (m CommittedMetadata) GetTranscriptStart() int {
	if m.CheckpointTranscriptStart > 0 {
		return m.CheckpointTranscriptStart
	}
	return m.TranscriptLinesAtStart
}

// SessionFilePaths contains the absolute paths to session files from the git tree root.
// Paths include the full checkpoint path prefix (e.g., "/a1/b2c3d4e5f6/1/metadata.json").
// Used in CheckpointSummary.Sessions to map session IDs to their file locations.
type SessionFilePaths struct {
	Metadata    string `json:"metadata"`
	Transcript  string `json:"transcript"`
	Context     string `json:"context"`
	ContentHash string `json:"content_hash"`
	Prompt      string `json:"prompt"`
}

// CheckpointSummary is the root-level metadata.json for a checkpoint.
// It contains aggregated statistics from all sessions and a map of session IDs
// to their file paths. Session-specific data (including initial_attribution)
// is stored in the session's subdirectory metadata.json.
//
// Structure on entire/checkpoints/v1 branch:
//
//	<checkpoint-id[:2]>/<checkpoint-id[2:]>/
//	├── metadata.json         # This CheckpointSummary
//	├── 1/                    # First session
//	│   ├── metadata.json     # Session-specific CommittedMetadata
//	│   ├── full.jsonl
//	│   ├── prompt.txt
//	│   ├── context.md
//	│   └── content_hash.txt
//	├── 2/                    # Second session
//	└── 3/                    # Third session...
//
//nolint:revive // Named CheckpointSummary to avoid conflict with existing Summary struct
type CheckpointSummary struct {
	CLIVersion       string             `json:"cli_version,omitempty"`
	CheckpointID     id.CheckpointID    `json:"checkpoint_id"`
	Strategy         string             `json:"strategy"`
	Branch           string             `json:"branch,omitempty"`
	CommitTreeHash   string             `json:"commit_tree_hash,omitempty"`
	CheckpointsCount int                `json:"checkpoints_count"`
	FilesTouched     []string           `json:"files_touched"`
	Sessions         []SessionFilePaths `json:"sessions"`
	TokenUsage       *agent.TokenUsage  `json:"token_usage,omitempty"`
}

// Summary contains AI-generated summary of a checkpoint.
type Summary struct {
	Intent    string           `json:"intent"`     // What user wanted to accomplish
	Outcome   string           `json:"outcome"`    // What was achieved
	Learnings LearningsSummary `json:"learnings"`  // Categorized learnings
	Friction  []string         `json:"friction"`   // Problems/annoyances encountered
	OpenItems []string         `json:"open_items"` // Tech debt, unfinished work
}

// LearningsSummary contains learnings grouped by scope.
type LearningsSummary struct {
	Repo     []string       `json:"repo"`     // Codebase-specific patterns/conventions
	Code     []CodeLearning `json:"code"`     // File/module specific findings
	Workflow []string       `json:"workflow"` // General dev practices
}

// CodeLearning captures a learning tied to a specific code location.
type CodeLearning struct {
	Path    string `json:"path"`               // File path
	Line    int    `json:"line,omitempty"`     // Start line number
	EndLine int    `json:"end_line,omitempty"` // End line for ranges (optional)
	Finding string `json:"finding"`            // What was learned
}

// InitialAttribution captures line-level attribution metrics at commit time.
// This is a point-in-time snapshot comparing the checkpoint tree (agent work)
// against the committed tree (may include human edits).
//
// Attribution Metrics:
//   - TotalCommitted measures "net additions" (lines added that remain in the commit)
//   - AgentPercentage represents "of the new code added, what percentage came from the agent"
//   - Deletion work is tracked separately in HumanRemoved (not included in percentage)
//
// Deletion-Only Commits:
// For commits with only deletions (no additions), TotalCommitted will be 0 and
// AgentPercentage will be 0. This is by design - the percentage metric is only
// meaningful for commits that add code. Deletion contributions are captured in
// the HumanRemoved field but don't affect the attribution percentage.
type InitialAttribution struct {
	CalculatedAt    time.Time `json:"calculated_at"`
	AgentLines      int       `json:"agent_lines"`      // Lines added by agent (base → shadow diff)
	HumanAdded      int       `json:"human_added"`      // Lines added by human (excluding modifications)
	HumanModified   int       `json:"human_modified"`   // Lines modified by human (estimate: min(added, removed))
	HumanRemoved    int       `json:"human_removed"`    // Lines removed by human (excluding modifications)
	TotalCommitted  int       `json:"total_committed"`  // Net additions in commit (agent + human new lines, not total file size)
	AgentPercentage float64   `json:"agent_percentage"` // agent_lines / total_committed * 100 (0 for deletion-only commits)
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
