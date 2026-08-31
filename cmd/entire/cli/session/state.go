package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/proclive"
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
//
// Kind is a discriminator — it distinguishes review variants at a per-session
// granularity. The checkpoint-level HasReview flag remains an umbrella that
// any review-kind session should set (so future review kinds like manual
// review can be added without changing summary-shape).
type Kind string

const (
	// KindAgentReview tags a session created by `entire review` (agent-driven
	// review). Future review kinds (e.g., manual review) should be defined as
	// distinct Kind values AND added to Kind.IsReview so the checkpoint's
	// HasReview umbrella flag keeps covering them.
	KindAgentReview Kind = "agent_review"

	// KindAgentInvestigate tags a session created by `entire investigate`
	// (agent-driven investigation). A session is review OR investigate, not
	// both — Kind is single-valued. Future investigate kinds should be added
	// to Kind.IsInvestigate so the checkpoint's HasInvestigation umbrella
	// flag keeps covering them.
	KindAgentInvestigate Kind = "agent_investigate"

	// KindImported tags a checkpoint created by `entire import` from a
	// pre-existing agent transcript. Imported checkpoints are read-only and
	// commit-less; they live on the v1 metadata branch and push like any other
	// checkpoint.
	KindImported Kind = "imported"
)

// IsReview reports whether this Kind counts as "a review happened" for the
// purpose of CheckpointSummary.HasReview. Extend this when adding new
// review-kind Kind values (e.g. KindManualReview) so the umbrella flag stays
// accurate without string-literal coupling across packages.
func (k Kind) IsReview() bool {
	// Note: a switch is the natural shape here, but golangci's
	// singleCaseSwitch flags a one-case switch — so we keep it as a list of
	// equality checks. Add new review-kind values to the disjunction below.
	return k == KindAgentReview
}

// IsInvestigate reports whether this Kind counts as "an investigation
// happened" for the purpose of CheckpointSummary.HasInvestigation. Extend
// this when adding new investigate-kind Kind values so the umbrella flag
// stays accurate without string-literal coupling across packages.
func (k Kind) IsInvestigate() bool {
	// See IsReview for why this is an equality check rather than a switch.
	return k == KindAgentInvestigate
}

// IsImported reports whether this Kind is a read-only session reconstructed by
// `entire import` from a pre-existing transcript. Imported sessions are exempt
// from lifecycle management (staleness, orphan cleanup) and are not
// resumable/rewindable. Centralized here so those call sites don't couple to
// the string literal across packages.
func (k Kind) IsImported() bool {
	// See IsReview for why this is an equality check rather than a switch.
	return k == KindImported
}

// CondensationAttempt records the durable intent for an in-progress
// condensation. RecoveryPending is set only when doctor must first look for a
// checkpoint written before attempt IDs existed.
type CondensationAttempt struct {
	CheckpointID    id.CheckpointID `json:"checkpoint_id"`
	RecoveryPending bool            `json:"recovery_pending,omitempty"`
}

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

	// AdoptedIntoWorktreePath marks a source-side tombstone left behind after
	// `entire session adopt` moves this session into another repository/worktree.
	// Hook TurnStart must not reactivate tombstoned source records, otherwise the
	// same session ID can diverge in two session stores.
	AdoptedIntoWorktreePath string `json:"adopted_into_worktree_path,omitempty"`

	// AdoptedIntoWorktreeID is the target worktree ID paired with
	// AdoptedIntoWorktreePath when available.
	AdoptedIntoWorktreeID string `json:"adopted_into_worktree_id,omitempty"`

	// Branch is the git branch HEAD pointed at the last time this session took a
	// turn. Captured on each turn start so it tracks branches created or renamed
	// after the session began. Empty when HEAD was detached or for sessions
	// recorded before this field existed (callers derive it from commit trailers
	// as a fallback). Lets `entire resume` map a stopped session back to its
	// branch without the user remembering it.
	Branch string `json:"branch,omitempty"`

	// StartedAt is when the session was started
	StartedAt time.Time `json:"started_at"`

	// EndedAt is when the session was explicitly closed by the user.
	// nil means the session is still active or was not cleanly closed.
	EndedAt *time.Time `json:"ended_at,omitempty"`

	// Phase is the lifecycle stage of this session (see phase.go).
	// Empty means idle (backward compat with pre-state-machine files).
	Phase Phase `json:"phase,omitempty"`

	// Kind tags the session's purpose. Empty for normal agent sessions;
	// set to KindAgentReview when the session was started by `entire review`.
	Kind Kind `json:"kind,omitempty"`

	// ReviewSkills is the snapshot of configured review skills at session start.
	// Preserved so checkpoint metadata records which skills were run. May be
	// empty when a review was attached post-hoc and skills were not declared;
	// ReviewPrompt is the ground truth in that case.
	ReviewSkills []string `json:"review_skills,omitempty"`

	// ReviewPrompt is the actual text of the review request — the composed
	// prompt sent to the agent (spawn path) or the session's first user
	// prompt (attach path). Always populated when Kind is a review kind.
	ReviewPrompt string `json:"review_prompt,omitempty"`

	// InvestigateRunID is the 12-hex-char ID of the parent investigation
	// run when Kind is an investigate kind. Multiple sessions across rounds
	// share this ID so the loop driver can correlate them. Empty for
	// non-investigate sessions.
	InvestigateRunID string `json:"investigate_run_id,omitempty"`

	// InvestigateTopic is the human-readable topic the investigation was
	// asked to investigate. Snapshot at session start so checkpoint
	// metadata records what the agent was investigating. Only meaningful
	// when Kind is an investigate kind.
	InvestigateTopic string `json:"investigate_topic,omitempty"`

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

	// CaptureDegradedAt records when the session's most recent turn degraded
	// capture because a worktree status scan breached its budget (new-file
	// detection skipped, or the checkpoint itself skipped). Set at turn end,
	// cleared by the next turn whose scans stay within budget. Surfaced as a
	// warning by `entire status`.
	CaptureDegradedAt *time.Time `json:"capture_degraded_at,omitempty"`

	// StepCount is the number of checkpoints/steps created in this session.
	// JSON tag kept as "checkpoint_count" for backward compatibility with existing state files.
	StepCount int `json:"checkpoint_count"`

	// CheckpointTranscriptStart is the transcript line offset where the current
	// checkpoint cycle began. Set to 0 at session start, updated to current
	// transcript length after each condensation. Used to scope the transcript
	// for checkpoint condensation: "everything since last checkpoint".
	CheckpointTranscriptStart int `json:"checkpoint_transcript_start,omitempty"`

	// TokenTranscriptStart is the transcript offset where the current checkpoint's
	// token window began. It advances with CheckpointTranscriptStart after every
	// condensation, but unlike it is NOT reset by carryForwardToNewShadowBranch:
	// that reset exists so a post-partial-commit checkpoint's transcript is
	// self-contained, and computing token_usage from the same offset re-reported
	// the whole session's tokens on every such checkpoint (53% of non-first
	// checkpoints in this repo's history were cumulative). Transcript scope and
	// token scope are therefore tracked separately; checkpoint token_usage is
	// always "since the previous checkpoint".
	TokenTranscriptStart int `json:"token_transcript_start,omitempty"`
	// TokenWindowInitialized records that TokenTranscriptStart is maintained by
	// the CLI that wrote this file, so a zero value means "the window starts at
	// line 0" rather than "this file predates the field". omitempty keeps it out
	// of legacy files, which is exactly the signal NormalizeAfterLoad needs.
	TokenWindowInitialized bool `json:"token_window_initialized,omitempty"`

	// CheckpointTranscriptSize is the byte size of the transcript at last condensation.
	// Used for fast "has new content?" checks in PostCommit: compare the git blob size
	// against this value without reading the full transcript content.
	CheckpointTranscriptSize int64 `json:"checkpoint_transcript_size,omitempty"`

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

	// CondensationAttempt is saved before a persistent checkpoint write so a
	// retry after process death keeps both the intended ID and recovery mode.
	CondensationAttempt *CondensationAttempt `json:"condensation_attempt,omitempty"`

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

	// AttachedManually indicates this session was imported via
	// `entire session attach` rather than being captured by hooks during
	// normal agent execution.
	AttachedManually bool `json:"attached_manually,omitempty"`

	// ContextInjectionDecided records that the once-per-session model-context
	// injection (e.g. the `entire trail` pointer) has been handled for this
	// session, so the dispatcher does not re-inject on later turns. Set on the
	// first normal turn regardless of whether anything was injected: the prompt
	// path reads only clone-local cached trail enablement, and a missing/stale
	// false cache fails closed (miss the hint) rather than retrying/spamming.
	// Review/investigate sessions leave this false because they skip injection.
	ContextInjectionDecided bool `json:"context_injection_decided,omitempty"`

	// AgentType identifies the agent that created this session (e.g., "Claude Code", "Gemini CLI", "Cursor")
	AgentType types.AgentType `json:"agent_type,omitempty"`

	// ModelName is the LLM model used in this session (e.g., "claude-sonnet-4-20250514", "gpt-4o").
	// Set from hook data when the agent provides it.
	ModelName string `json:"model_name,omitempty"`

	// Token usage tracking (accumulated across all checkpoints in this session).
	//
	// DECISION: SubagentTokens is "latest snapshot wins", not summed. Subagent
	// usage arrives as a cumulative-since-session-start total (each subagent
	// transcript is re-read from line 0 every call), so accumulateTokenUsage
	// replaces rather than adds it (see cmd/entire/cli/strategy). Tradeoff: if
	// the main transcript resets or rotates mid-session (compaction writing a
	// fresh file, or a resume that truncates), a subsequent snapshot can be
	// SMALLER than a previous one, so this session-wide total regresses
	// (undercounts) for the rest of the session. This is accepted: undercounting
	// after a transcript reset is preferable to the multiplicative overcount the
	// summing approach produced, and the alternative (a session-wide high-water
	// mark) would mask genuine subagent-transcript cleanup. Checkpoint deltas do
	// not share this exposure — CheckpointTokenUsage.SubagentTokens is derived as
	// (this total - SubagentTokensBaseline) and floored at 0 by clampSubtract, so
	// a shrunk snapshot yields 0, never a negative or stale delta.
	TokenUsage *agent.TokenUsage `json:"token_usage,omitempty"`

	// CheckpointTokenUsage tracks hook-provided token usage since the last condensation.
	// This is checkpoint-scoped; TokenUsage remains the session-wide total.
	CheckpointTokenUsage *agent.TokenUsage `json:"checkpoint_token_usage,omitempty"`

	// SubagentTokensBaseline is a snapshot of TokenUsage.SubagentTokens captured
	// at the last condensation reset. Subagent token usage is always re-read
	// from the start of each subagent transcript (agent IDs are discovered from
	// the full main transcript so subagents spawned before the checkpoint
	// window are still found), so it arrives as a cumulative-since-session-start
	// total rather than a per-checkpoint delta. This baseline lets
	// CheckpointTokenUsage.SubagentTokens be rescoped to "since last
	// condensation" via SubtractTokenUsage instead of re-adding the same
	// cumulative total on every checkpoint.
	SubagentTokensBaseline *agent.TokenUsage `json:"subagent_tokens_baseline,omitempty"`

	// SkillEvents records explicit native skill signals observed during this session.
	// Stored as sidecar metadata so consumers can collapse skill-related transcript events
	// without mutating the raw agent transcript.
	//
	// This grows for the life of the session and is deliberately uncapped. It is
	// also the durable half of the exactly-once contract for skill telemetry:
	// extraction re-derives events from transcript offset 0 on every pass and
	// dedupes against this ledger (strategy.appendNewSkillEvents), so an event
	// whose entry never reached disk is announced twice. Trimming it therefore
	// re-enables double-reporting for exactly the long sessions a cap would
	// target.
	//
	// The cost is real but bounded, and it is paid on EVERY MutateSessionState —
	// i.e. every hook, including PostToolUse — because state is read and written
	// whole. Measured JSON round-trip: 0 events / 106 B / 1.6us; 10 / 6.0 KB /
	// 42us; 50 / 29.7 KB / 201us; 200 / 119 KB / 785us, i.e. ~594 B per event
	// and sub-millisecond at any realistic N. The steady-state dedupe rebuild is
	// cheap by comparison: 13us at 200 existing events.
	//
	// So a 100 KB session state is expected, not a leak. If the envelope ever
	// does need shrinking, the move is a narrower ledger — persist only the
	// dedupe keys (~40 B/event) and keep the full events transient — not a
	// truncation, which would break exactly-once.
	SkillEvents []agent.SkillEvent `json:"skill_events,omitempty"`

	// CommitCondensedSignalCheckpointID is the checkpoint ID of the last
	// cli_commit_condensed telemetry signal this session snapshotted, the
	// durable half of that signal's at-most-once contract. A `git commit
	// --amend` re-runs PostCommit with the SAME trailer checkpoint ID (an
	// ACTIVE session re-condenses unconditionally), so without this ledger one
	// logical commit is counted twice in both halves of the miss-rate ratio.
	// Persisted by the same state save that gates the emit, so a failed save
	// retries cleanly; a crash between save and emit loses the row, which is
	// the signal's accepted best-effort posture.
	CommitCondensedSignalCheckpointID string `json:"commit_condensed_signal_checkpoint_id,omitempty"`

	// Hook-provided session metrics (for agents like Cursor that report via hooks)
	SessionDurationMs int64 `json:"session_duration_ms,omitempty"`
	SessionTurnCount  int   `json:"session_turn_count,omitempty"`
	ContextTokens     int   `json:"context_tokens,omitempty"`
	ContextWindowSize int   `json:"context_window_size,omitempty"`

	// PromptWindowBase is the SessionTurnCount value at the start of the current
	// checkpoint window. The number of prompts attributed to the next checkpoint is
	// SessionTurnCount - PromptWindowBase (floored at 1 when written). It is only
	// advanced (deferred reset) the next time a turn is counted after a checkpoint
	// was written, so two checkpoints with no prompt between them report the same
	// count. Zero-value safe on old state files: base 0 ⇒ window = SessionTurnCount,
	// i.e. "all prompts so far" (correct first-checkpoint semantics).
	PromptWindowBase int `json:"prompt_window_base,omitempty"`

	// PromptWindowResetPending indicates a checkpoint was just written and the
	// window base must be re-anchored to the current SessionTurnCount the next time
	// a turn is counted. Deferred so back-to-back checkpoints share a count.
	PromptWindowResetPending bool `json:"prompt_window_reset_pending,omitempty"`

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

	// Owner fingerprints the process that owns this session's agent turn,
	// captured at each turn start via proclive.ResolveOwner. It lets liveness
	// checks detect an ACTIVE session whose agent has exited (clean /exit,
	// crash, kill, terminal close, reboot) without a SessionStop hook firing —
	// see OwnerExited. nil for legacy sessions or when the owner couldn't be
	// resolved, in which case liveness falls back to the StuckActiveThreshold
	// timeout. Only meaningful on Owner.Host.
	Owner *proclive.Identity `json:"owner,omitempty"`

	// TaskRecords tracks subagents dispatched by this session — the durable
	// pointer ledger for subagent work. See TaskRecord.
	TaskRecords []TaskRecord `json:"task_records,omitempty"`
}

// TaskRecord is the durable pointer ledger entry for a subagent dispatched by
// this session: a small session-state record (correlation ID, agent type,
// description, declared transcript path, files touched, tokens) rather than a
// shadow-tree write. Condensation is what materializes a record's transcript
// (sanitize → externalize → redact) into the parent session's checkpoint —
// see docs/superpowers/plans/2026-08-19-subagent-durable-records.md.
//
// CompletedAt zero means the record is still in flight (background launch
// observed, no completion signal yet); non-zero means the record was
// completed — via CompleteTaskRecord — by a foreground/background post-task
// capture, SubagentStop, or the SessionEnd sweep. Unlike the prior
// remove-on-claim model, a completed record is NOT deleted: it must persist
// so a later condensation can materialize its transcript. Consumed by the
// Final captures — the SubagentStop handler (handleSubagentStopFinal) and the
// SessionEnd sweep (completeLiveTaskRecords). A Final capture completes the
// record LAST, after successful extraction (strategy.CompleteTaskRecord's
// exactly-once mutation), so a failed capture leaves the record live for the
// SessionEnd sweep to retry.
type TaskRecord struct {
	// ToolUseID is the Task tool invocation's tool_use_id — the same ID used
	// to key TaskMetadataDir. Dedup key for AddTaskRecord.
	ToolUseID string `json:"tool_use_id"`

	// AgentID is the subagent identifier (tool_response.agentId at launch
	// time), used to resolve the subagent's own transcript file.
	AgentID string `json:"agent_id,omitempty"`

	// StartedAt is when the background launch was observed.
	StartedAt time.Time `json:"started_at"`

	// SubagentType and TaskDescription are captured at launch time because
	// SubagentStop payloads carry no tool_input — a Final-path capture has no
	// way to derive them from the event itself (ParseSubagentTypeAndDescription
	// yields empty strings on a nil ToolInput). The Final handler reads these
	// from the record to label the task step, falling back to the event's own
	// fields only when the record is absent.
	SubagentType    string `json:"subagent_type,omitempty"`
	TaskDescription string `json:"task_description,omitempty"`

	// DeclaredTranscriptPath is the subagent transcript path an agent's stop
	// hook declared (e.g. Claude Code's agent_transcript_path, or the
	// equivalent Codex/Cursor field) — the path #2058 says must not be lost
	// between mid-turn capture and condensation-time materialization. Empty
	// when no declared path was available; the materializer falls back to
	// ResolveAgentTranscriptPath in that case.
	DeclaredTranscriptPath string `json:"declared_transcript_path,omitempty"`

	// Files is the set of files touched by this subagent, merged into the
	// session's FilesTouched at completion time. Populated when the record
	// is completed; empty for a still in-flight record and for a completed
	// read-only subagent.
	Files []string `json:"files,omitempty"`

	// TokenUsage is this subagent's token usage, when the completing hook
	// payload provided one. nil when unavailable.
	TokenUsage *agent.TokenUsage `json:"token_usage,omitempty"`

	// CompletedAt is when this record was completed (CompleteTaskRecord).
	// Zero means the record is still in flight. See the type doc comment.
	CompletedAt time.Time `json:"completed_at,omitempty"`
}

// AddTaskRecord records a subagent launch, replacing any existing entry with
// the same ToolUseID. Dedup by ToolUseID: a retried or duplicate launch event
// must not create two records for the same task, which would make
// RemoveTaskRecord leave a stale entry behind after the first is cleared.
func (s *State) AddTaskRecord(task TaskRecord) {
	for i, existing := range s.TaskRecords {
		if existing.ToolUseID == task.ToolUseID {
			s.TaskRecords[i] = task
			return
		}
	}
	s.TaskRecords = append(s.TaskRecords, task)
}

// RemoveTaskRecord clears the record for toolUseID, if present. No-op when no
// record matches. Retained for tests and any caller that genuinely wants to
// discard a record outright — ordinary completion should use
// CompleteTaskRecord instead, which keeps the record for the materializer.
func (s *State) RemoveTaskRecord(toolUseID string) {
	for i, existing := range s.TaskRecords {
		if existing.ToolUseID == toolUseID {
			s.TaskRecords = append(s.TaskRecords[:i], s.TaskRecords[i+1:]...)
			return
		}
	}
}

// FindTaskRecord returns a pointer to the record for toolUseID, or nil if
// none exists. The pointer aliases the slice element, so callers must not
// retain it across a mutation that could reallocate TaskRecords (e.g.
// AddTaskRecord, RemoveTaskRecord).
func (s *State) FindTaskRecord(toolUseID string) *TaskRecord {
	for i := range s.TaskRecords {
		if s.TaskRecords[i].ToolUseID == toolUseID {
			return &s.TaskRecords[i]
		}
	}
	return nil
}

// CompleteTaskRecord marks the record for toolUseID as consumed exactly
// once: it sets CompletedAt and returns true, or returns false — a no-op —
// when no record exists for toolUseID or it was already completed
// (CompletedAt already non-zero). This is the exactly-once completion guard
// equivalent to the old claim-and-remove semantics: whichever caller
// observes true proceeds with the capture, a racing duplicate sees false and
// skips.
//
// The data fields (DeclaredTranscriptPath, Files, TokenUsage) are NOT set
// here. They are populated separately by the producer after successful
// extraction, via direct field mutation on the claimed record within the
// same MutateSessionState closure that called CompleteTaskRecord — see
// strategy.CompleteTaskRecord, the producer-facing wrapper that pairs the
// claim with the attach inside one lock.
func (s *State) CompleteTaskRecord(toolUseID string, completedAt time.Time) bool {
	for i := range s.TaskRecords {
		if s.TaskRecords[i].ToolUseID != toolUseID {
			continue
		}
		if !s.TaskRecords[i].CompletedAt.IsZero() {
			return false
		}
		s.TaskRecords[i].CompletedAt = completedAt
		return true
	}
	return false
}

// HasTaskContent reports whether this session carries pending subagent task
// content: any task record — live (transcript-so-far still needs capturing)
// or completed-unmaterialized (awaiting condensation) — counts. Condensation
// triggers and session-empty guards key on this, not on shadow-branch
// existence: task records never touch the shadow branch.
func (s *State) HasTaskContent() bool {
	return len(s.TaskRecords) > 0
}

// LiveTaskRecords returns the records not yet completed (CompletedAt zero) —
// i.e. still in flight. In-flight consumers (the SessionEnd sweep's "any
// in-flight work?" check) must use this rather than the raw TaskRecords
// slice: since CompleteTaskRecord no longer removes a record, TaskRecords
// mixes live records with already-completed ones awaiting materialization.
func (s *State) LiveTaskRecords() []TaskRecord {
	var live []TaskRecord
	for _, r := range s.TaskRecords {
		if r.CompletedAt.IsZero() {
			live = append(live, r)
		}
	}
	return live
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
	// TokenTranscriptStart was added after CheckpointTranscriptStart. A state file
	// written before it exists has token scope == transcript scope, except when a
	// carry-forward has just reset the transcript offset — which cannot be told
	// apart from "never condensed", so that one checkpoint stays cumulative.
	//
	// Gated on TokenWindowInitialized: a zero offset written deliberately by a
	// CLI that maintains the window must survive. The turn-end advance after a
	// mid-turn commit moves only CheckpointTranscriptStart, so a session whose
	// token window legitimately still starts at line 0 loads in exactly the
	// shape this migration keys on — re-coupling it there would drop the tail's
	// tokens from every later checkpoint.
	if !s.TokenWindowInitialized && s.TokenTranscriptStart == 0 && s.CheckpointTranscriptStart > 0 {
		s.TokenTranscriptStart = s.CheckpointTranscriptStart
	}
	// Either the value was already maintained or the migration above has just
	// resolved it; from here on it is known either way.
	s.TokenWindowInitialized = true
	// Clear deprecated fields so they aren't re-persisted.
	// Note: this is a one-way migration. If the state is re-saved, older CLI versions
	// will see 0 for these fields and fall back to scoping from the transcript start.
	// This is acceptable since CLI upgrades are monotonic and the worst case is
	// redundant transcript content in a condensation, not data loss.
	s.ClearLegacyTranscriptOffsets()

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

// ClearLegacyTranscriptOffsets clears deprecated transcript offset fields so
// callers that intentionally reset CheckpointTranscriptStart do not re-persist
// stale legacy state.
func (s *State) ClearLegacyTranscriptOffsets() {
	s.CondensedTranscriptLines = 0
	s.TranscriptLinesAtStart = 0
}

// PendingCondensationID returns the checkpoint ID reserved for an in-progress
// condensation, or the empty ID when no attempt is pending.
func (s *State) PendingCondensationID() id.CheckpointID {
	if s.CondensationAttempt == nil {
		return id.EmptyCheckpointID
	}
	return s.CondensationAttempt.CheckpointID
}

// BeginCondensationAttempt records a checkpoint ID before its persistent write.
func (s *State) BeginCondensationAttempt(checkpointID id.CheckpointID) {
	s.CondensationAttempt = &CondensationAttempt{CheckpointID: checkpointID}
}

// RequireCondensationRecovery keeps legacy orphan reconciliation enabled for
// the current attempt. It has no effect when no attempt is pending.
func (s *State) RequireCondensationRecovery() {
	if s.CondensationAttempt != nil {
		s.CondensationAttempt.RecoveryPending = true
	}
}

// NeedsCondensationRecovery reports whether doctor must reconcile a checkpoint
// written before attempt IDs existed.
func (s *State) NeedsCondensationRecovery() bool {
	return s.CondensationAttempt != nil && s.CondensationAttempt.RecoveryPending
}

// ClearCondensationAttempt completes or abandons the pending condensation.
func (s *State) ClearCondensationAttempt() {
	s.CondensationAttempt = nil
}

// RebaselineSubagentTokens snapshots the current cumulative subagent total
// (TokenUsage.SubagentTokens) into SubagentTokensBaseline so the next checkpoint
// window's CheckpointTokenUsage.SubagentTokens is rescoped to "since this
// re-baseline" rather than re-reporting the full cumulative subagent total.
//
// The invariant is: every site that starts a fresh checkpoint window by clearing
// CheckpointTokenUsage MUST also re-baseline. Callers: the condensation reset
// helper (resetCheckpointWindow) and cross-repo session adoption, which likewise
// opens a fresh target-local window. Sharing this here keeps the two in step.
func (s *State) RebaselineSubagentTokens() {
	if s.TokenUsage != nil {
		s.SubagentTokensBaseline = s.TokenUsage.SubagentTokens
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

// OwnerLiveness reports the liveness of this session's recorded owner process.
// It returns proclive.LivenessUnknown when no owner was recorded (legacy
// sessions, or sessions where the owner couldn't be resolved), so callers can
// fall back to the time-based IsStuckActive heuristic.
func (s *State) OwnerLiveness() proclive.Liveness {
	if s.Owner == nil {
		return proclive.LivenessUnknown
	}
	return proclive.Check(*s.Owner)
}

// OwnerExited reports true when this session's owning agent process is gone —
// exited cleanly, crashed, was killed, or the machine rebooted — without a
// SessionStop hook firing. Unlike IsStuckActive (a time-based heuristic), this
// is detected immediately, regardless of how recently the session interacted.
// It returns false when liveness is Unknown (no owner recorded, cross-host
// state, or an unsupported platform) so behavior degrades to the
// StuckActiveThreshold timeout.
//
// It deliberately covers IDLE as well as ACTIVE. An agent that finishes its
// last turn and then quits leaves the session IDLE, so gating on ACTIVE alone
// missed precisely the sessions left behind by agents with no session-end hook
// (Codex before 0.146) or killed before that hook could run: they lingered as
// "active" in `entire status` until StaleSessionThreshold deleted the state
// file outright, discarding pending checkpoint work instead of condensing it.
// Only already-finalized sessions are excluded — see IsEnded.
func (s *State) OwnerExited() bool {
	if s.IsEnded() {
		return false
	}
	return s.OwnerLiveness() == proclive.LivenessDead
}

// IsEnded reports whether this session has been finalized — the canonical
// "no longer a live session" predicate.
//
// Both halves matter and neither implies the other in practice: Phase is what
// the state machine sets, EndedAt is what the finalizing write stamps, and a
// state file can carry one without the other (a legacy record, or a partial
// write). Callers that filter for active sessions must agree on this rule, so
// it lives here rather than being re-spelled at each site.
func (s *State) IsEnded() bool {
	return s.Phase == PhaseEnded || s.EndedAt != nil
}

func (s *State) IsStale() bool {
	// Imported sessions are historical, read-only records reconstructed from
	// pre-existing transcripts; their timestamps are always old by nature.
	// Never auto-purge them or they'd vanish from `entire session list` on the
	// first read after import.
	if s.Kind.IsImported() {
		return false
	}
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
	if err := ensureTestIsolatedStateDir(commonDir); err != nil {
		return nil, err
	}
	return &StateStore{
		stateDir: filepath.Join(commonDir, SessionStateDirName),
	}, nil
}

// NewStateStoreForWorktree returns the state store for the repository at
// worktreeRoot, independent of the process's working directory. Callers that
// operate on a repo passed as an argument (e.g. agent import) must use this:
// the CWD-resolved NewStateStore writes session state into whatever repo the
// process happens to run in, which is how test fixtures once leaked into a
// developer's real .git/entire-sessions and hijacked commit linking.
func NewStateStoreForWorktree(ctx context.Context, worktreeRoot string) (*StateStore, error) {
	// An empty root would silently degrade to the process CWD (cmd.Dir = ""),
	// reproducing exactly the accidental-repo leak this constructor exists to
	// prevent.
	if worktreeRoot == "" {
		return nil, errors.New("worktree root required to scope the session state store")
	}
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--git-common-dir")
	cmd.Dir = worktreeRoot
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("resolve git common dir for %s: %w", worktreeRoot, err)
	}
	commonDir := strings.TrimSpace(string(output))
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(worktreeRoot, commonDir)
	}
	// Same go-test guard as NewStateStore: an explicit root computed from the
	// process CWD in a non-isolated test is just as accidental as the CWD
	// itself.
	if err := ensureTestIsolatedStateDir(commonDir); err != nil {
		return nil, err
	}
	return &StateStore{
		stateDir: filepath.Join(filepath.Clean(commonDir), SessionStateDirName),
	}, nil
}

// ensureTestIsolatedStateDir fails loud when `go test` code reaches a
// session state directory outside the temp root: the test is missing repo
// isolation (testutil.InitRepo + t.Chdir, or NewStateStoreForWorktree /
// NewStateStoreWithDir with a temp repo). Silence here is how fixture
// sessions once landed in a real repo's .git/entire-sessions and were then
// picked up by commit-to-session linking. Spawned binaries are unaffected:
// testing.Testing() is false in subprocesses, and integration/e2e harnesses
// isolate via environment instead.
func ensureTestIsolatedStateDir(commonDir string) error {
	if !testing.Testing() {
		return nil
	}
	// getGitCommonDir can return a cwd-relative ".git"; the temp-root
	// comparison needs the absolute location.
	if abs, err := filepath.Abs(commonDir); err == nil {
		commonDir = abs
	}
	if underTempRoot(commonDir) {
		return nil
	}
	return fmt.Errorf(
		"session state dir %q escapes test isolation; give the test an isolated repo (testutil.InitRepo + t.Chdir) or scope the store explicitly",
		filepath.Join(commonDir, SessionStateDirName))
}

// underTempRoot reports whether path is inside the OS temp root, comparing
// both the literal and symlink-resolved forms (macOS presents /var/folders
// and /private/var/folders for the same tree).
func underTempRoot(path string) bool {
	roots := []string{filepath.Clean(os.TempDir())}
	if resolved, err := filepath.EvalSymlinks(roots[0]); err == nil && resolved != roots[0] {
		roots = append(roots, resolved)
	}
	candidates := []string{filepath.Clean(path)}
	if resolved, err := filepath.EvalSymlinks(candidates[0]); err == nil && resolved != candidates[0] {
		candidates = append(candidates, resolved)
	}
	for _, root := range roots {
		for _, c := range candidates {
			if c == root || strings.HasPrefix(c, root+string(os.PathSeparator)) {
				return true
			}
		}
	}
	return false
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

	// Everything this CLI persists carries a meaningful token window, including
	// a deliberate 0 (see TokenWindowInitialized and NormalizeAfterLoad).
	state.TokenWindowInitialized = true

	// Validate session ID to prevent path traversal
	if err := validation.ValidateSessionID(state.SessionID); err != nil {
		return fmt.Errorf("invalid session ID: %w", err)
	}

	if err := os.MkdirAll(s.stateDir, 0o750); err != nil {
		return fmt.Errorf("failed to create session state directory: %w", err)
	}

	// Scope the final rename to an os.Root so the session-ID-derived destination
	// cannot escape the state directory even if validation were ever bypassed
	// (defense in depth; the ID is already validated above).
	root, err := os.OpenRoot(s.stateDir)
	if err != nil {
		return fmt.Errorf("failed to open session state directory: %w", err)
	}
	defer root.Close()

	data, err := jsonutil.MarshalIndentWithNewline(state, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal session state: %w", err)
	}

	fileName := state.SessionID + ".json"

	// Use a unique temp file per save. Concurrent hook processes can write the
	// same session ID, so a fixed "<session>.json.tmp" path can corrupt JSON.
	tmpFile, err := os.CreateTemp(s.stateDir, fileName+".*.tmp")
	if err != nil {
		return fmt.Errorf("failed to create temporary session state file: %w", err)
	}
	tmpFileName := tmpFile.Name()
	removeTmp := true
	defer func() {
		if removeTmp {
			_ = os.Remove(tmpFileName)
		}
	}()

	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("failed to write session state: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("failed to close session state file: %w", err)
	}

	// Atomic rename into the validated final path, via os.Root.
	if err := root.Rename(filepath.Base(tmpFileName), fileName); err != nil {
		return fmt.Errorf("failed to rename session state file: %w", err)
	}
	removeTmp = false
	return nil
}

// Clear removes the session state file for the given session ID.
func (s *StateStore) Clear(ctx context.Context, sessionID string) error {
	_ = ctx // Reserved for future use

	// Validate session ID to prevent path traversal
	if err := validation.ValidateSessionID(sessionID); err != nil {
		return fmt.Errorf("invalid session ID: %w", err)
	}

	// Remove all files for this session (state .json, .model hint, any future
	// hint files). Match by literal prefix rather than filepath.Glob: the
	// session ID is user-controlled, and a glob pattern would let metacharacters
	// match and delete other sessions' files. os.Root ensures traversal-resistant
	// removal.
	matches := matchSessionFiles(s.stateDir, sessionID)
	if len(matches) > 0 {
		root, rootErr := os.OpenRoot(s.stateDir)
		if rootErr != nil {
			return fmt.Errorf("failed to open session state directory for cleanup: %w", rootErr)
		}
		defer root.Close()
		for _, name := range matches {
			_ = osroot.Remove(root, name) //nolint:errcheck // best-effort cleanup
		}
	}

	return nil
}

// matchSessionFiles returns the names (not paths) of files in dir that belong to
// the given session ID — i.e. "<sessionID>.<ext>". It uses literal prefix
// matching, never glob patterns, so a session ID containing glob metacharacters
// cannot match unrelated files.
func matchSessionFiles(dir, sessionID string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil // missing/unreadable dir => nothing to clear
	}
	prefix := sessionID + "."
	var matched []string
	for _, e := range entries {
		if name := e.Name(); strings.HasPrefix(name, prefix) {
			matched = append(matched, name)
		}
	}
	return matched
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
