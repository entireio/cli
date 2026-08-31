package checkpoint

import (
	"encoding/json"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/redact"

	"github.com/go-git/go-git/v6/plumbing"
)

// TranscriptAsset is a binary blob (e.g. an image) lifted out of a transcript
// and stored raw in the checkpoint, referenced by a placeholder in the log.
type TranscriptAsset struct {
	Name      string // stable asset filename / id, also used in the placeholder
	MediaType string
	Data      []byte
}

// TaskPayload materializes one subagent task record's transcript and metadata
// into a session checkpoint's tasks/<tool-use-id>/ subtree. Produced by
// condensation from session.TaskRecords (see
// docs/superpowers/plans/2026-08-19-subagent-durable-records.md) and consumed
// by the write path via WriteOptions.Tasks — the replacement for the old
// unreachable IsTask/ToolUseID per-write route (#2058): no producer ever set
// that route's fields, so every subagent transcript died at condensation
// until this payload existed.
type TaskPayload struct {
	// ToolUseID is the Task tool invocation's tool_use_id — also the
	// directory name under tasks/.
	ToolUseID string

	// AgentID is the subagent identifier, used to name agent-<id>.jsonl.
	AgentID string

	// SubagentType and TaskDescription label the task in task.json.
	// TaskDescription is free text from the agent and is redacted by the
	// writer; callers pass it as recorded.
	SubagentType    string
	TaskDescription string

	// Transcript is the subagent's transcript content, already run through
	// the sanitize -> externalize -> redact pipeline (the same one the
	// session transcript gets — see redact.RedactedBytes for why callers
	// must not hand this field raw bytes). Empty (Len() == 0) means the
	// transcript was unavailable — see TranscriptUnavailableReason, which is
	// non-empty exactly when this is empty — and no agent-<id>.jsonl is
	// written. This mirrors how WriteOptions.Transcript itself expresses
	// "no content": a value type, not a pointer, with emptiness read via Len().
	Transcript redact.RedactedBytes

	// Files is the set of files touched by this subagent.
	Files []string

	// TokenUsage is this subagent's token usage, when known. nil when
	// unavailable.
	TokenUsage *types.TokenUsage

	// StartedAt is when the subagent launch was observed.
	StartedAt time.Time

	// CompletedAt is when the task record was completed. Zero means the task
	// was still in flight when this checkpoint was materialized — its
	// transcript (if any) is a transcript-so-far snapshot, not the final one.
	CompletedAt time.Time

	// TranscriptUnavailableReason explains why Transcript is empty. A stable
	// category string (e.g. "transcript unreadable", "transcript path
	// unresolvable", "transcript empty") — never the underlying error detail,
	// which may embed an absolute local path and must not enter a pushed
	// task.json; log that detail via logging.Warn instead. Empty exactly when
	// Transcript is non-empty.
	TranscriptUnavailableReason string
}

// WriteOptions contains options for writing a persistent checkpoint.
type WriteOptions struct {
	// CheckpointID is the stable 12-hex-char identifier
	CheckpointID id.CheckpointID

	// SessionID is the session identifier
	SessionID string

	// CreatedAt is when the checkpoint was originally created.
	// When zero, writers use the current time.
	CreatedAt time.Time

	// Strategy is the name of the strategy that created this checkpoint
	Strategy string

	// Branch is the branch name where the checkpoint was created (empty if detached HEAD)
	Branch string

	// CommitSHA links this checkpoint to an existing commit without a trailer.
	// It is an anchor — "imported at this point in time" — not attribution.
	// Currently set only by `entire import`: imported history has no
	// Entire-Checkpoint trailer (we never rewrite existing commits), so import
	// stamps the resolved anchor commit here (the default branch head when
	// resolvable; see resolveImportLinkCommitSHA for the fallback order).
	// Empty for all other writers. This comment is the canonical description;
	// Metadata.CommitSHA and CheckpointSummary.CommitSHA point back here.
	CommitSHA string

	// Transcript is the session transcript content (full.jsonl).
	// Must be pre-redacted (via redact.JSONLBytes or redact.AlreadyRedacted for trusted sources).
	Transcript redact.RedactedBytes

	// Assets are binary blobs (e.g. images) lifted out of Transcript and
	// referenced by path-bearing placeholders. Stored raw under the session's
	// assets/ folder. Empty for agents/transcripts with no externalized images.
	Assets []TranscriptAsset

	// Prompts contains the raw user prompts from the session. Run through
	// redactedJoinedPrompts before persisting — the writer does this
	// inside writeSessionToSubdirectory.
	Prompts []string

	// FilesTouched are files modified during the session
	FilesTouched []string

	// CheckpointsCount is the displayed "steps" count for this session: the number
	// of user prompts attributed to this checkpoint (floored at 1). Despite the
	// historical name/JSON tag, it is no longer a count of checkpoints.
	CheckpointsCount int

	// SaveStepCount is the number of SaveStep-recorded steps (shadow-branch
	// commits) for this session. Distinct from CheckpointsCount (the displayed
	// prompt count): this is the honest "did real checkpoint work happen" signal
	// used to gate combined attribution. 0 means a commit-only / fallback session.
	SaveStepCount int

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

	// TranscriptPath is a path to the session transcript file, used as a
	// fallback source when Transcript is empty (e.g. a caller that wants the
	// store to read and redact the file itself rather than doing so in memory).
	TranscriptPath string

	// Tasks materializes subagent task records dispatched by this session into
	// this checkpoint's tasks/<tool-use-id>/ subtree — see TaskPayload. Empty
	// for sessions with no subagent work (the vast majority) and for backends
	// or write paths that predate subagent-work durability (#2058); the writer
	// treats an empty slice as a no-op.
	Tasks []TaskPayload

	// Commit message fields
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
	// TokenTranscriptStart is the transcript offset where this checkpoint's
	// token_usage window began (SessionState.TokenTranscriptStart). It equals
	// CheckpointTranscriptStart except after a carry-forward, which resets only
	// the transcript offset; readers slicing the stored transcript for
	// per-checkpoint token attribution must use this one.
	TokenTranscriptStart int

	// CheckpointTranscriptStart is written to both Metadata.CheckpointTranscriptStart
	// and the deprecated Metadata.TranscriptLinesAtStart for backward compatibility.

	// TokenUsage contains the token usage for this checkpoint
	TokenUsage *types.TokenUsage

	// SkillEvents records explicit native skill signals observed in this session.
	SkillEvents []types.SkillEvent

	// SessionMetrics contains hook-provided session metrics (duration, turns, context usage)
	SessionMetrics *SessionMetrics

	// Attribution is line-level attribution calculated at commit time
	// comparing checkpoint tree (agent work) to committed tree (may include human edits)
	Attribution *Attribution

	// PromptAttributionsJSON is the raw PromptAttributions data, JSON-encoded.
	// Persisted for diagnostic purposes — shows exactly which prompt recorded
	// which "user" lines, enabling root cause analysis of attribution bugs.
	// Uses json.RawMessage to avoid importing session package.
	PromptAttributionsJSON json.RawMessage

	// CombinedAttribution is holistic attribution across all sessions.
	// Used during migration to preserve v1 root summary attribution.
	// During normal condensation this is nil (computed post-commit via a CheckpointAttribution write).
	CombinedAttribution *Attribution

	// Summary is an optional AI-generated summary for this checkpoint.
	// This field may be nil when:
	//   - summarization is disabled in settings
	//   - summary generation failed (non-blocking, logged as warning)
	//   - the transcript was empty or too short to summarize
	//   - the checkpoint predates the summarization feature
	Summary *Summary

	// Kind identifies the session purpose (e.g., "agent_review"). Empty for normal sessions.
	Kind string

	// ReviewSkills is the snapshot of skills used (only meaningful when Kind is a review kind).
	// May be empty when a review is attached post-hoc without declared skills.
	ReviewSkills []string

	// ReviewPrompt is the actual text of the review request (composed prompt
	// for spawn, first user prompt for attach). Only meaningful when Kind is
	// a review kind.
	ReviewPrompt string

	// HasReview is set by the caller when this session should mark its
	// checkpoint as reviewed. The caller computes this (e.g. via
	// session.Kind.IsReview) because checkpoint can't import session
	// — the session package imports checkpoint, creating a cycle.
	HasReview bool

	// InvestigateRunID is the 12-hex-char ID of the parent investigation
	// run (only meaningful when Kind is an investigate kind).
	InvestigateRunID string

	// InvestigateTopic is the human-readable topic the investigation was
	// asked to investigate (only meaningful when Kind is an investigate
	// kind).
	InvestigateTopic string

	// HasInvestigation is set by the caller when this session should mark
	// its checkpoint as part of an investigation. The caller computes this
	// (e.g. via session.Kind.IsInvestigate) because checkpoint can't import
	// session — the session package imports checkpoint, creating a cycle.
	HasInvestigation bool
}

// UpdateOptions contains options for updating an existing persistent checkpoint.
// Uses replace semantics: the transcript and prompts are fully replaced,
// not appended. At stop time we have the complete session transcript and want every
// checkpoint to contain it identically.
type UpdateOptions struct {
	// CheckpointID identifies the checkpoint to update
	CheckpointID id.CheckpointID

	// SessionID identifies which session slot to update within the checkpoint
	SessionID string

	// Transcript is the full session transcript (replaces existing).
	// Must be pre-redacted (via redact.JSONLBytes or redact.AlreadyRedacted for trusted sources).
	Transcript redact.RedactedBytes

	// Assets are the externalized image blobs matching Transcript's placeholders
	// (see WriteOptions.Assets). Set together with Transcript so the backfill keeps
	// the stored assets/ folder consistent with the transcript; empty clears any
	// previously-stored assets when Transcript is replaced.
	Assets []TranscriptAsset

	// PreserveAssetsWhenEmpty keeps already-stored assets instead of clearing them
	// when Assets is empty. Set on the finalize path for agents whose assets come
	// from a best-effort sidecar capture (e.g. Cursor's sqlite3 store read): a
	// transient capture miss at finalize must not wipe images a prior condensation
	// successfully stored. Left false for codec agents, where an empty set means
	// "the transcript has no images" and stale asset blobs should be cleared.
	PreserveAssetsWhenEmpty bool

	// Prompts contains the raw user prompts (replaces existing).
	// See WriteOptions.Prompts.
	Prompts []string

	// Agent identifies the agent type (needed for transcript chunking)
	Agent types.AgentType

	// SkillEvents replaces the session metadata skill_events when non-empty.
	SkillEvents []types.SkillEvent

	// PrecomputedBlobs, if non-nil, provides chunk blob hashes and the
	// content-hash blob hash computed once for this transcript. When set,
	// transcript backfill skips the per-call ChunkTranscript + zlib work and
	// reuses these hashes. Used by finalizeAllTurnCheckpoints to avoid
	// re-compressing identical content N times.
	PrecomputedBlobs *PrecomputedTranscriptBlobs
}

// PrecomputedTranscriptBlobs holds blob hashes for a transcript that was
// chunked and written to the object store once, for reuse across multiple
// transcript-backfill writes sharing the same transcript content.
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

// IsUsable reports whether the precomputed blobs satisfy the invariants that
// consumers depend on: a non-zero content-hash blob and at least one chunk
// hash. Callers should fall back to the fresh-write path when this is false.
func (p *PrecomputedTranscriptBlobs) IsUsable() bool {
	return p != nil && !p.ContentHashBlob.IsZero() && len(p.ChunkHashes) > 0
}

// CheckpointInfo contains summary information about a persisted checkpoint.
//
//nolint:revive // Named CheckpointInfo to avoid conflict with the generic Info type; the checkpoint.CheckpointInfo stutter is accepted (matches CheckpointSummary).
type CheckpointInfo struct {
	// CheckpointID is the stable 12-hex-char identifier
	CheckpointID id.CheckpointID

	// SessionID is the session identifier (most recent session for multi-session checkpoints)
	SessionID string

	// CreatedAt is when the checkpoint was created
	CreatedAt time.Time

	// CheckpointsCount is the aggregate displayed "steps" count across sessions:
	// the sum of per-session prompt-window counts. Despite the historical name,
	// it is not a count of checkpoint records.
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

	// Imported is true when this checkpoint was imported from pre-existing
	// agent history (Kind == "imported"): read-only and commit-less.
	Imported bool

	// ListedStub is true for names-only remote-discovery List entries that still
	// need hydration (or have not yet failed a hydration attempt). It is cleared
	// after a successful hydrate and also after a failed attempt (fail-once), so
	// callers do not re-fetch forever. A local ref whose root metadata was
	// unreadable has the same zero SessionID/SessionCount shape but ListedStub
	// false — do not treat field zero-ness alone as stub-ness.
	ListedStub bool `json:"-"`
}

// SessionContent contains the actual content for a session.
// This is used when reading full session data (transcript, prompts, context)
// as opposed to just the metadata/summary.
type SessionContent struct {
	// Metadata contains the session-specific metadata
	Metadata Metadata

	// Transcript is the session transcript content
	Transcript []byte

	// TranscriptBlobHashes are the stored raw transcript blob hashes in chunk
	// order. Callers that rewrite the same transcript under a different path can
	// reuse these content-addressed blobs instead of storing duplicate blobs.
	TranscriptBlobHashes []plumbing.Hash

	// Prompts contains user prompts from this session
	Prompts string
}

// Metadata contains the metadata stored in metadata.json for each checkpoint.
type Metadata struct {
	CLIVersion   string          `json:"cli_version,omitempty"`
	CheckpointID id.CheckpointID `json:"checkpoint_id"`
	SessionID    string          `json:"session_id"`
	Strategy     string          `json:"strategy"`
	CreatedAt    time.Time       `json:"created_at"`
	Branch       string          `json:"branch,omitempty"` // Branch where checkpoint was created (empty if detached HEAD)
	// CommitSHA anchors an imported checkpoint to an existing commit; empty for
	// non-imported checkpoints, which link via the Entire-Checkpoint trailer.
	// See WriteOptions.CommitSHA for the full semantics.
	CommitSHA        string `json:"commit_sha,omitempty"`
	CheckpointsCount int    `json:"checkpoints_count"`
	// SaveStepCount is the number of SaveStep-recorded steps for this session.
	// Honest "real checkpoint work happened" signal (0 = commit-only/fallback
	// session), kept separate from the displayed CheckpointsCount prompt count.
	// Added after CheckpointsCount stopped being a reliable did-SaveStep-run signal.
	SaveStepCount int      `json:"save_step_count,omitempty"`
	FilesTouched  []string `json:"files_touched"`

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
	CheckpointTranscriptStart   int    `json:"checkpoint_transcript_start,omitempty"`    // Raw transcript (full.jsonl) line offset at start of this checkpoint's data
	// TokenTranscriptStart is the raw transcript offset where this checkpoint's
	// token_usage window begins. Differs from CheckpointTranscriptStart after a
	// carry-forward (which resets the transcript offset to 0 so the stored
	// transcript is self-contained, but leaves the token window alone).
	//
	// Written only by CLIs that stamp token_usage_version >= 2, but its presence
	// is not a version marker: omitempty also drops it at offset 0, so a
	// session's first checkpoint omits it on v2 too. Absent decodes as 0, which
	// is the correct window start in exactly those cases.
	TokenTranscriptStart int `json:"token_transcript_start,omitempty"`

	// Deprecated: Use CheckpointTranscriptStart instead. Written for backward compatibility with older CLI versions.
	TranscriptLinesAtStart int `json:"transcript_lines_at_start,omitempty"`

	// CompactTranscriptStart is the line offset in the compact transcript.jsonl
	// at which this checkpoint's data begins. transcript.jsonl stores the full
	// compacted session (each checkpoint is self-contained), so readers segment
	// this checkpoint's slice as compactLines[CompactTranscriptStart:]. The slice
	// never drops this checkpoint's content, but its first line may repeat up to
	// one compact line that began in the previous checkpoint (when a streaming
	// message straddles the boundary and compaction merges it into one line), so
	// segmenters must tolerate a bounded head overlap.
	//
	// A nil pointer marks a legacy checkpoint whose transcript.jsonl holds only
	// this checkpoint's delta (CLI versions before the full-compact-transcript
	// change), which is read as-is from line 0. A pointer is used so that "absent"
	// (legacy delta file) is distinguishable from 0 (full file, first checkpoint).
	CompactTranscriptStart *int `json:"compact_transcript_start,omitempty"`

	// Token usage for this checkpoint
	TokenUsage *types.TokenUsage `json:"token_usage,omitempty"`

	// SkillEvents records explicit native skill signals observed in this session.
	// Consumers use these anchors to collapse skill-related raw transcript events.
	SkillEventsVersion int                `json:"skill_events_version,omitempty"`
	SkillEvents        []types.SkillEvent `json:"skill_events,omitempty"`

	// SessionMetrics contains hook-provided session metrics (duration, turns, context usage).
	// Populated for agents that provide these metrics via hooks (e.g., Cursor).
	SessionMetrics *SessionMetrics `json:"session_metrics,omitempty"`

	// AI-generated summary of the checkpoint
	Summary *Summary `json:"summary,omitempty"`

	// Attribution is line-level attribution calculated at commit time
	Attribution *Attribution `json:"initial_attribution,omitempty"`

	// PromptAttributions is the raw per-prompt attribution data used to compute Attribution.
	// Diagnostic field — shows which prompt recorded which "user" lines.
	PromptAttributions json.RawMessage `json:"prompt_attributions,omitempty"`

	// Kind identifies the session purpose (e.g., "agent_review"). Empty for normal sessions.
	Kind string `json:"kind,omitempty"`

	// ReviewSkills lists the review skills that were run (only set when Kind is a review kind).
	// May be empty when a review was attached post-hoc without declared skills.
	ReviewSkills []string `json:"review_skills,omitempty"`

	// ReviewPrompt is the actual text of the review request (composed prompt
	// for spawn, first user prompt for attach). Only set when Kind is a
	// review kind.
	ReviewPrompt string `json:"review_prompt,omitempty"`

	// InvestigateRunID is the 12-hex-char ID of the parent investigation
	// run. Only set when Kind is an investigate kind.
	InvestigateRunID string `json:"investigate_run_id,omitempty"`

	// InvestigateTopic is the human-readable topic the investigation was
	// asked to investigate. Only set when Kind is an investigate kind.
	InvestigateTopic string `json:"investigate_topic,omitempty"`
}

// GetTranscriptStart returns the transcript line offset at which this checkpoint's data begins.
// Returns 0 for new checkpoints (start from beginning). For data written by older CLI versions,
// falls back to the deprecated TranscriptLinesAtStart field.
func (m Metadata) GetTranscriptStart() int {
	if m.CheckpointTranscriptStart > 0 {
		return m.CheckpointTranscriptStart
	}
	return m.TranscriptLinesAtStart
}

// GetCompactTranscriptStart returns the line offset in transcript.jsonl at which
// this checkpoint's data begins, and whether the offset was recorded. ok=false
// means a legacy checkpoint whose transcript.jsonl holds only this checkpoint's
// delta (read it from line 0); ok=true with offset 0 means the full-compact file
// whose first checkpoint starts at the beginning.
func (m Metadata) GetCompactTranscriptStart() (offset int, ok bool) {
	if m.CompactTranscriptStart == nil {
		return 0, false
	}
	return *m.CompactTranscriptStart, true
}

// SessionFilePaths contains the absolute paths to session files from the git tree root.
// Paths include the full checkpoint path prefix (e.g., "/a1/b2c3d4e5f6/1/metadata.json").
// Used in CheckpointSummary.Sessions to map session IDs to their file locations.
type SessionFilePaths struct {
	Metadata string `json:"metadata"`
	// Transcript points at the raw full.jsonl, which CLI read paths
	// (rewind/resume/explain) resolve by filename.
	Transcript string `json:"transcript,omitempty"`
	// CompactTranscript points at the compact transcript.jsonl when one was
	// generated alongside full.jsonl. Omitted otherwise (non-compactable,
	// empty, or oversized transcripts, and older CLI versions). transcript.jsonl
	// holds the full compacted session; this checkpoint's slice begins at the
	// session metadata's compact_transcript_start (see Metadata.CompactTranscriptStart).
	CompactTranscript string `json:"compact_transcript,omitempty"`
	ContentHash       string `json:"content_hash,omitempty"`
	Prompt            string `json:"prompt"`
	// AssetsManifest points at assets/manifest.json when images were externalized
	// out of the transcript into the session's assets/ folder. Omitted otherwise.
	AssetsManifest string `json:"assets_manifest,omitempty"`
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
//	│   ├── metadata.json     # Session-specific Metadata
//	│   ├── full.jsonl        # Raw agent transcript
//	│   ├── transcript.jsonl  # Full compacted session (slice at compact_transcript_start)
//	│   ├── prompt.txt
//	│   └── content_hash.txt
//	├── 2/                    # Second session
//	└── 3/                    # Third session...
//
//nolint:revive // Named CheckpointSummary to avoid conflict with existing Summary struct
type CheckpointSummary struct {
	CLIVersion   string          `json:"cli_version,omitempty"`
	CheckpointID id.CheckpointID `json:"checkpoint_id"`
	Strategy     string          `json:"strategy"`
	Branch       string          `json:"branch,omitempty"`
	// CommitSHA: import-only anchor; see WriteOptions.CommitSHA.
	CommitSHA           string             `json:"commit_sha,omitempty"`
	CheckpointsCount    int                `json:"checkpoints_count"`
	FilesTouched        []string           `json:"files_touched"`
	Sessions            []SessionFilePaths `json:"sessions"`
	TokenUsage          *types.TokenUsage  `json:"token_usage,omitempty"`
	CombinedAttribution *Attribution       `json:"combined_attribution,omitempty"`

	// HasReview is the umbrella "any review happened" flag: true when at least
	// one session in this checkpoint has a review-kind Kind (currently
	// "agent_review"). When new review kinds are introduced they should also
	// cause this flag to be set so callers can keep asking "was this reviewed
	// in any way?" without caring about the variant.
	HasReview bool `json:"has_review,omitempty"`

	// HasInvestigation is the umbrella "any investigation happened" flag:
	// true when at least one session in this checkpoint has an
	// investigate-kind Kind (currently "agent_investigate"). When new
	// investigate kinds are introduced they should also cause this flag to
	// be set so callers can keep asking "was this investigated in any way?"
	// without caring about the variant.
	HasInvestigation bool `json:"has_investigation,omitempty"`

	// Imported is true when this checkpoint was imported from pre-existing
	// agent history (a session with Kind == "imported"): read-only and
	// commit-less.
	Imported bool `json:"imported,omitempty"`

	// TokenUsageVersion states how every session's token_usage in this
	// checkpoint was scoped and what it carries. TokenUsageVersionDelta means:
	// a per-checkpoint delta (tokens since the previous checkpoint of that
	// session, scoped by SessionState.TokenTranscriptStart — never the
	// session's running total), with the subset fields thinking_tokens and
	// cache_creation_1h_tokens populated where the agent records them.
	//
	// Absent (0) marks a legacy checkpoint: its token_usage may be either a
	// delta or, on a non-first checkpoint whose checkpoint_transcript_start is
	// 0/absent, the session's cumulative total. Readers summing across legacy
	// checkpoints must dedupe per session (keep the latest cumulative snapshot,
	// add only later deltas), must not read missing subset fields as zero, and
	// must not key on cli_version, which is "dev" on most rows.
	TokenUsageVersion int `json:"token_usage_version,omitempty"`
}

// TokenUsageVersionDelta is the TokenUsageVersion written since token scope was
// split from transcript scope and the subset fields were added (v0.11).
const TokenUsageVersionDelta = 2

// ResolveTokenUsageVersion decides the TokenUsageVersion to stamp when writing
// a checkpoint summary.
//
// The field sits on the summary but describes a per-session property: it tells
// readers that every session's token_usage in this checkpoint is a
// per-checkpoint delta. A single write only ever produces a delta-scoped row
// for the one session it writes, so a checkpoint that still carries rows from a
// legacy (pre-delta) CLI must keep its legacy value. Stamping the delta version
// over them — which a CLI upgrade between two writes to the same checkpoint
// would otherwise do — makes readers treat session-cumulative rows as deltas
// and skip the per-session dedupe those rows still need.
//
// existingVersion is the version already on the summary (0 when the checkpoint
// is new or predates the field). writesEverySession reports whether the write
// leaves the checkpoint holding only the session it just wrote, so that no
// older row survives it.
//
// When older rows do survive, the summary reports the weakest guarantee across
// them: the existing version, floored at what this writer itself produces. A
// legacy 0 therefore stays 0, and a hypothetical future version is reported as
// TokenUsageVersionDelta rather than claiming that scope for the row just
// written.
func ResolveTokenUsageVersion(existingVersion int, writesEverySession bool) int {
	if writesEverySession {
		return TokenUsageVersionDelta
	}
	return min(existingVersion, TokenUsageVersionDelta)
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

// Attribution captures line-level attribution metrics at commit time.
// This is a point-in-time snapshot comparing the checkpoint tree (agent work)
// against the committed tree (may include human edits).
//
// Attribution Metrics:
//   - TotalCommitted keeps the historical "net additions" view for compatibility
//   - TotalLinesChanged measures total committed line changes (adds + modifies + removes)
//   - AgentPercentage represents "of the lines changed in this commit, what percentage came from the agent"
//   - AgentRemoved tracks committed deletions performed by the agent
type Attribution struct {
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
