// Package checkpoint provides types and interfaces for checkpoint storage.
//
// A Checkpoint captures a point-in-time within a session, containing either
// full state (Temporary) or metadata with a commit reference (Committed).
//
// See docs/architecture/sessions-and-checkpoints.md for the full domain model.
package checkpoint

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/redact"

	"github.com/go-git/go-git/v6/plumbing"
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
	// Use ReadSessionContent to read actual transcript/prompts.
	// Returns nil, nil if the checkpoint does not exist.
	ReadCommitted(ctx context.Context, checkpointID id.CheckpointID) (*CheckpointSummary, error)

	// ReadSessionContent reads the actual content for a specific session within a checkpoint.
	// sessionIndex is 0-based (0 for first session, 1 for second, etc.).
	// Returns the session's metadata, transcript, and prompts.
	ReadSessionContent(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (*SessionContent, error)

	// ReadSessionContentByID reads a session's content by its session ID.
	// Useful when you have the session ID but don't know its index within the checkpoint.
	ReadSessionContentByID(ctx context.Context, checkpointID id.CheckpointID, sessionID string) (*SessionContent, error)

	// ListCommitted lists all committed checkpoints.
	ListCommitted(ctx context.Context) ([]CommittedInfo, error)

	// UpdateCommitted replaces the transcript and prompts for an existing
	// committed checkpoint. Used at stop time to finalize checkpoints with the full
	// session transcript (prompt to stop event).
	// Returns ErrCheckpointNotFound if the checkpoint doesn't exist.
	UpdateCommitted(ctx context.Context, opts UpdateCommittedOptions) error
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

	// Transcript is the session transcript content (full.jsonl).
	// Must be pre-redacted (via redact.JSONLBytes or redact.AlreadyRedacted for trusted sources).
	Transcript redact.RedactedBytes

	// Prompts contains user prompts from the session
	Prompts []string

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

	// Task checkpoint fields (for task/subagent checkpoints)
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
	Agent types.AgentType

	// Model is the LLM model used during the session (e.g., "claude-sonnet-4-20250514")
	Model string

	// TurnID correlates checkpoints from the same agent turn.
	TurnID string

	// Transcript position at checkpoint start - tracks what was added during this checkpoint
	TranscriptIdentifierAtStart string // Last identifier when checkpoint started (UUID for Claude, message ID for Gemini)
	CheckpointTranscriptStart   int    // Transcript line offset at start of this checkpoint's data

	// CheckpointTranscriptStart is written to both CommittedMetadata.CheckpointTranscriptStart
	// and the deprecated CommittedMetadata.TranscriptLinesAtStart for backward compatibility.

	// CompactTranscriptStart is the transcript.jsonl line offset at checkpoint start.
	// V2 /main writes this to checkpoint_transcript_start; v1 continues to use
	// CheckpointTranscriptStart (full.jsonl).
	CompactTranscriptStart int

	// TokenUsage contains the token usage for this checkpoint
	TokenUsage *agent.TokenUsage

	// SessionMetrics contains hook-provided session metrics (duration, turns, context usage)
	SessionMetrics *SessionMetrics

	// InitialAttribution is line-level attribution calculated at commit time
	// comparing checkpoint tree (agent work) to committed tree (may include human edits)
	InitialAttribution *InitialAttribution

	// PromptAttributionsJSON is the raw PromptAttributions data, JSON-encoded.
	// Persisted for diagnostic purposes — shows exactly which prompt recorded
	// which "user" lines, enabling root cause analysis of attribution bugs.
	// Uses json.RawMessage to avoid importing session package.
	PromptAttributionsJSON json.RawMessage

	// Summary is an optional AI-generated summary for this checkpoint.
	// This field may be nil when:
	//   - summarization is disabled in settings
	//   - summary generation failed (non-blocking, logged as warning)
	//   - the transcript was empty or too short to summarize
	//   - the checkpoint predates the summarization feature
	Summary *Summary

	// CompactTranscript is the Entire Transcript Format (transcript.jsonl) bytes.
	// Written to v2 /main ref alongside metadata. May be nil if compaction
	// was not performed (unknown agent, compaction error, empty transcript).
	CompactTranscript []byte
}

// UpdateCommittedOptions contains options for updating an existing committed checkpoint.
// Uses replace semantics: the transcript and prompts are fully replaced,
// not appended. At stop time we have the complete session transcript and want every
// checkpoint to contain it identically.
type UpdateCommittedOptions struct {
	// CheckpointID identifies the checkpoint to update
	CheckpointID id.CheckpointID

	// SessionID identifies which session slot to update within the checkpoint
	SessionID string

	// Transcript is the full session transcript (replaces existing).
	// Must be pre-redacted (via redact.JSONLBytes or redact.AlreadyRedacted for trusted sources).
	Transcript redact.RedactedBytes

	// Prompts contains all user prompts (replaces existing)
	Prompts []string

	// Agent identifies the agent type (needed for transcript chunking)
	Agent types.AgentType

	// CompactTranscript is the updated Entire Transcript Format bytes.
	// If non-nil, replaces the existing transcript.jsonl on v2 /main.
	CompactTranscript []byte

	// PrecomputedBlobs, if non-nil, provides chunk blob hashes and the
	// content-hash blob hash computed once for this transcript. When set,
	// UpdateCommitted skips the per-call ChunkTranscript + zlib work and
	// reuses these hashes. Used by finalizeAllTurnCheckpoints to avoid
	// re-compressing identical content N times.
	PrecomputedBlobs *PrecomputedTranscriptBlobs
}

// PrecomputedTranscriptBlobs holds blob hashes for a transcript that was
// chunked and written to the object store once, for reuse across multiple
// UpdateCommitted calls sharing the same transcript content.
//
// Blob hashes are content-addressed (SHA-1 of chunk bytes), so the same
// PrecomputedTranscriptBlobs works for both v1 (full.jsonl) and v2
// (raw_transcript) paths — only the tree-entry filename differs.
//
// Callers should avoid constructing this for empty transcripts; agent.ChunkTranscript
// would otherwise produce a single zero-length chunk and a hash for an empty
// blob, which downstream stores would never reference.
type PrecomputedTranscriptBlobs struct {
	// ChunkHashes are the blob hashes for each transcript chunk, in order.
	// Always non-empty when built via PrecomputeTranscriptBlobs (a non-empty
	// transcript chunks to at least one entry; callers should skip precompute
	// for empty transcripts).
	ChunkHashes []plumbing.Hash

	// ContentHashBlob is the blob hash of the "sha256:<hex>" content-hash
	// string for the transcript.
	ContentHashBlob plumbing.Hash

	// ContentHash is the "sha256:<hex>" string itself, so the short-circuit
	// path can compare without re-reading the blob.
	ContentHash string
}

// isUsable reports whether the precomputed blobs satisfy the invariants that
// consumers depend on: a non-zero content-hash blob and at least one chunk
// hash. Callers should fall back to the fresh-write path when this is false.
func (p *PrecomputedTranscriptBlobs) isUsable() bool {
	return p != nil && !p.ContentHashBlob.IsZero() && len(p.ChunkHashes) > 0
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
	Agent types.AgentType

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
}

// CommittedMetadata contains the metadata stored in metadata.json for each checkpoint.
type CommittedMetadata struct {
	CLIVersion       string          `json:"cli_version,omitempty"`
	CheckpointID     id.CheckpointID `json:"checkpoint_id"`
	SessionID        string          `json:"session_id"`
	Strategy         string          `json:"strategy"`
	CreatedAt        time.Time       `json:"created_at"`
	Branch           string          `json:"branch,omitempty"` // Branch where checkpoint was created (empty if detached HEAD)
	CheckpointsCount int             `json:"checkpoints_count"`
	FilesTouched     []string        `json:"files_touched"`

	// Agent identifies the agent that created this checkpoint (e.g., "Claude Code", "Cursor")
	Agent types.AgentType `json:"agent,omitempty"`

	// Model is the LLM model used during the session (e.g., "claude-sonnet-4-20250514").
	// Always written to metadata (empty string when unknown) so consumers can rely on the field's presence.
	Model string `json:"model"`

	// TurnID correlates checkpoints from the same agent turn.
	// When a turn's work spans multiple commits, each gets its own checkpoint
	// but they share the same TurnID for future aggregation/deduplication.
	TurnID string `json:"turn_id,omitempty"`

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

	// SessionMetrics contains hook-provided session metrics (duration, turns, context usage).
	// Populated for agents that provide these metrics via hooks (e.g., Cursor).
	SessionMetrics *SessionMetrics `json:"session_metrics,omitempty"`

	// AI-generated summary of the checkpoint
	Summary *Summary `json:"summary,omitempty"`

	// InitialAttribution is line-level attribution calculated at commit time
	InitialAttribution *InitialAttribution `json:"initial_attribution,omitempty"`

	// PromptAttributions is the raw per-prompt attribution data used to compute InitialAttribution.
	// Diagnostic field — shows which prompt recorded which "user" lines.
	PromptAttributions json.RawMessage `json:"prompt_attributions,omitempty"`
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
	Transcript  string `json:"transcript,omitempty"`
	ContentHash string `json:"content_hash,omitempty"`
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
//	│   └── content_hash.txt
//	├── 2/                    # Second session
//	└── 3/                    # Third session...
//
//nolint:revive // Named CheckpointSummary to avoid conflict with existing Summary struct
type CheckpointSummary struct {
	CLIVersion          string              `json:"cli_version,omitempty"`
	CheckpointID        id.CheckpointID     `json:"checkpoint_id"`
	Strategy            string              `json:"strategy"`
	Branch              string              `json:"branch,omitempty"`
	CheckpointsCount    int                 `json:"checkpoints_count"`
	FilesTouched        []string            `json:"files_touched"`
	Sessions            []SessionFilePaths  `json:"sessions"`
	TokenUsage          *agent.TokenUsage   `json:"token_usage,omitempty"`
	CombinedAttribution *InitialAttribution `json:"combined_attribution,omitempty"`
}

// SessionMetrics contains hook-provided session metrics from agents that report
// them via lifecycle hooks (e.g., Cursor). These supplement transcript-derived
// metrics for agents whose transcripts lack usage/timing data.
type SessionMetrics struct {
	DurationMs        int64 `json:"duration_ms,omitempty"`
	TurnCount         int   `json:"turn_count,omitempty"`
	ContextTokens     int   `json:"context_tokens,omitempty"`
	ContextWindowSize int   `json:"context_window_size,omitempty"`
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
//   - TotalCommitted keeps the historical "net additions" view for compatibility
//   - TotalLinesChanged measures total committed line changes (adds + modifies + removes)
//   - AgentPercentage represents "of the lines changed in this commit, what percentage came from the agent"
//   - AgentRemoved tracks committed deletions performed by the agent
type InitialAttribution struct {
	CalculatedAt      time.Time `json:"calculated_at"`
	AgentLines        int       `json:"agent_lines"`              // Lines added by agent that remain in the commit
	AgentRemoved      int       `json:"agent_removed"`            // Lines removed by agent that remain removed in the commit
	HumanAdded        int       `json:"human_added"`              // Lines added by human (excluding modifications)
	HumanModified     int       `json:"human_modified"`           // Lines modified by human (estimate: min(added, removed))
	HumanRemoved      int       `json:"human_removed"`            // Lines removed by human (excluding modifications)
	TotalCommitted    int       `json:"total_committed"`          // Net additions in commit (legacy additions-focused metric)
	TotalLinesChanged int       `json:"total_lines_changed"`      // Total committed line changes (adds + modifies + removes)
	AgentPercentage   float64   `json:"agent_percentage"`         // (agent_lines + agent_removed) / total_lines_changed * 100
	MetricVersion     int       `json:"metric_version,omitempty"` // 0/absent = legacy (additions-only %), 2 = changed-lines %
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
