package strategy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/factoryaidroid"
	"github.com/entireio/cli/cmd/entire/cli/agent/geminicli"
	"github.com/entireio/cli/cmd/entire/cli/agent/opencode"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	cpkg "github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/summarize"
	"github.com/entireio/cli/cmd/entire/cli/textutil"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
	"github.com/entireio/cli/cmd/entire/cli/transcript/compact"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/entireio/cli/perf"
	"github.com/entireio/cli/redact"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// listCheckpoints returns all checkpoints from the metadata branch.
// Uses checkpoint.GitStore.ListCommitted() for reading from entire/checkpoints/v1.
func (s *ManualCommitStrategy) listCheckpoints(ctx context.Context) ([]CheckpointInfo, error) {
	store, err := s.getCheckpointStore()
	if err != nil {
		return nil, fmt.Errorf("failed to get checkpoint store: %w", err)
	}

	committed, err := store.ListCommitted(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list committed checkpoints: %w", err)
	}

	// Convert from checkpoint.CommittedInfo to strategy.CheckpointInfo
	result := make([]CheckpointInfo, 0, len(committed))
	for _, c := range committed {
		result = append(result, CheckpointInfo{
			CheckpointID:     c.CheckpointID,
			SessionID:        c.SessionID,
			CreatedAt:        c.CreatedAt,
			CheckpointsCount: c.CheckpointsCount,
			FilesTouched:     c.FilesTouched,
			Agent:            c.Agent,
			IsTask:           c.IsTask,
			ToolUseID:        c.ToolUseID,
			SessionCount:     c.SessionCount,
			SessionIDs:       c.SessionIDs,
		})
	}

	return result, nil
}

// getCheckpointLog returns the transcript for a specific checkpoint ID.
// Uses checkpoint.GitStore.ReadCommitted() for reading from entire/checkpoints/v1.
func (s *ManualCommitStrategy) getCheckpointLog(ctx context.Context, checkpointID id.CheckpointID) ([]byte, error) {
	store, err := s.getCheckpointStore()
	if err != nil {
		return nil, fmt.Errorf("failed to get checkpoint store: %w", err)
	}

	content, err := store.ReadLatestSessionContent(ctx, checkpointID)
	if err != nil {
		return nil, fmt.Errorf("failed to read checkpoint: %w", err)
	}
	if content == nil {
		return nil, fmt.Errorf("checkpoint not found: %s", checkpointID)
	}
	if len(content.Transcript) == 0 {
		return nil, fmt.Errorf("no transcript found for checkpoint: %s", checkpointID)
	}

	return content.Transcript, nil
}

// condenseOpts provides pre-resolved git objects to avoid redundant reads.
type condenseOpts struct {
	shadowRef        *plumbing.Reference // Pre-resolved shadow branch ref (nil = resolve from repo)
	headTree         *object.Tree        // Pre-resolved HEAD tree (passed through to calculateSessionAttributions)
	parentTree       *object.Tree        // Pre-resolved parent tree (nil for initial commits, for consistent non-agent line counting)
	repoDir          string              // Repository worktree path for git CLI commands
	parentCommitHash string              // HEAD's first parent hash for per-commit non-agent file detection
	headCommitHash   string              // HEAD commit hash (passed through for attribution)
	allAgentFiles    map[string]struct{} // Union of all sessions' FilesTouched for cross-session exclusion (nil = single-session)
}

var redactSessionJSONLBytes = redact.JSONLBytes

// CondenseSession condenses a session's shadow branch to permanent storage.
// checkpointID is the 12-hex-char value from the Entire-Checkpoint trailer.
// Metadata is stored at sharded path: <checkpoint_id[:2]>/<checkpoint_id[2:]>/
// Uses checkpoint.GitStore.WriteCommitted for the git operations.
//
// For mid-session commits (no Stop/SaveStep called yet), the shadow branch may not exist.
// In this case, data is extracted from the live transcript instead.
func (s *ManualCommitStrategy) CondenseSession(ctx context.Context, repo *git.Repository, checkpointID id.CheckpointID, state *SessionState, committedFiles map[string]struct{}, opts ...condenseOpts) (*CondenseResult, error) {
	ag, _ := agent.GetByAgentType(state.AgentType) //nolint:errcheck // ag may be nil for unknown agent types; callers use type assertions so nil is safe
	var o condenseOpts
	if len(opts) > 0 {
		o = opts[0]
	}
	logCtx := logging.WithComponent(ctx, "checkpoint")
	condenseStart := time.Now()

	shadowBranchName := getShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)
	ref, hasShadowBranch := resolveShadowRef(repo, shadowBranchName, o.shadowRef)

	// Re-resolve transcript path before any reads — handles agents that relocate
	// transcripts mid-session (e.g., Cursor CLI flat → nested layout change).
	// Errors are ignored; downstream readers handle missing transcripts gracefully.
	resolveTranscriptPath(state) //nolint:errcheck,gosec // best-effort; downstream readers handle missing files

	extractStart := time.Now()
	_, extractSessionDataSpan := perf.Start(ctx, "extract_session_data")
	var shadowHash plumbing.Hash
	if hasShadowBranch {
		shadowHash = ref.Hash()
	}
	sessionData, extractErr := s.extractOrCreateSessionData(ctx, repo, ag, shadowHash, hasShadowBranch, state)
	if extractErr != nil {
		extractSessionDataSpan.RecordError(extractErr)
		extractSessionDataSpan.End()
		return nil, extractErr
	}
	extractSessionDataSpan.End()
	extractDuration := time.Since(extractStart)

	// Backfill session state token usage from the freshly-extracted transcript.
	// Copilot CLI writes session.shutdown after the hooks return, so by condensation
	// time we can recover the authoritative full-session total from the transcript
	// while keeping checkpoint metadata scoped to CheckpointTranscriptStart.
	if backfillUsage := sessionStateBackfillTokenUsage(ctx, ag, state.AgentType, sessionData.Transcript, sessionData.TokenUsage); backfillUsage != nil {
		state.TokenUsage = backfillUsage
	}

	// Skip gate: if there is no transcript AND no files touched, there is nothing
	// meaningful to condense. Return early to avoid writing metadata-only stubs.
	//
	// This check MUST run before filterFilesTouched. That function's fallback
	// assigns all committed files to sessions with empty FilesTouched (designed
	// for mid-turn commits where SaveStep hasn't run yet). Without this ordering,
	// genuinely empty sessions (no transcript, no shadow branch, no tracked files)
	// would acquire committed files from the fallback and bypass this gate.
	if len(sessionData.Transcript) == 0 && len(sessionData.FilesTouched) == 0 {
		logging.Info(logCtx, "session skipped: no transcript or files to condense",
			slog.String("session_id", state.SessionID),
			slog.String("agent_type", string(state.AgentType)),
			slog.String("checkpoint_id", checkpointID.String()),
			slog.Bool("has_shadow_branch", hasShadowBranch),
			slog.String("transcript_path", state.TranscriptPath),
		)
		return newSkippedResult(checkpointID, state.SessionID), nil
	}

	filterFilesTouched(sessionData, committedFiles, state)

	redactedTranscript, redactDuration := redactOrDrop(logCtx, sessionData.Transcript, state.SessionID, checkpointID)
	if skipped := skipIfPostRedactionEmpty(logCtx, redactedTranscript, sessionData, state, checkpointID); skipped != nil {
		return skipped, nil
	}

	// Get checkpoint store
	store, err := s.getCheckpointStore()
	if err != nil {
		return nil, fmt.Errorf("failed to get checkpoint store: %w", err)
	}

	// Get author info
	authorName, authorEmail := GetGitAuthorFromRepo(repo)

	// Determine attribution base commit
	attrBase := state.AttributionBaseCommit
	if attrBase == "" {
		attrBase = state.BaseCommit
	}

	attributionStart := time.Now()
	attrCtx, attributionSpan := perf.Start(ctx, "calculate_session_attribution")
	attribution := calculateSessionAttributions(attrCtx, repo, ref, sessionData, state, attributionOpts{
		headTree:              o.headTree,
		parentTree:            o.parentTree,
		repoDir:               o.repoDir,
		attributionBaseCommit: attrBase,
		parentCommitHash:      o.parentCommitHash,
		headCommitHash:        o.headCommitHash,
		allAgentFiles:         o.allAgentFiles,
	})
	attributionSpan.End()
	attributionDuration := time.Since(attributionStart)

	// Get current branch name
	branchName := GetCurrentBranchName(repo)

	var summary *cpkg.Summary
	if settings.IsSummarizeEnabled(ctx) && redactedTranscript.Len() > 0 {
		summary = generateSummary(ctx, redactedTranscript, sessionData.FilesTouched, state)
	}

	// Build write options (shared by v1 and v2)
	writeOpts := cpkg.WriteCommittedOptions{
		CheckpointID:                checkpointID,
		SessionID:                   state.SessionID,
		Strategy:                    StrategyNameManualCommit,
		Branch:                      branchName,
		Transcript:                  redactedTranscript,
		Prompts:                     sessionData.Prompts,
		FilesTouched:                sessionData.FilesTouched,
		CheckpointsCount:            state.StepCount,
		EphemeralBranch:             shadowBranchName,
		AuthorName:                  authorName,
		AuthorEmail:                 authorEmail,
		Agent:                       state.AgentType,
		Model:                       state.ModelName,
		TurnID:                      state.TurnID,
		TranscriptIdentifierAtStart: state.TranscriptIdentifierAtStart,
		CheckpointTranscriptStart:   state.CheckpointTranscriptStart,
		TokenUsage:                  sessionData.TokenUsage,
		SessionMetrics:              buildSessionMetrics(state),
		InitialAttribution:          attribution,
		PromptAttributionsJSON:      marshalPromptAttributionsIncludingPending(state),
		Summary:                     summary,
	}

	compactResult := buildExternalCompactTranscript(ctx, ag, state)
	if compactResult == nil {
		internalResult := buildInternalCompactTranscript(ctx, ag, redactedTranscript, state)
		compactResult = &internalResult
	}
	writeOpts.CompactTranscript = compactResult.Transcript
	writeOpts.CompactTranscriptStart = compactResult.StartLine

	v2 := settings.CheckpointsVersion(ctx) == 2

	// Write checkpoint metadata to the primary store.
	writeV1Start := time.Now()
	writeCtx, writeCommittedSpan := perf.Start(ctx, "write_committed_v1")
	if !v2 {
		if err := store.WriteCommitted(writeCtx, writeOpts); err != nil {
			writeCommittedSpan.RecordError(err)
			writeCommittedSpan.End()
			return nil, fmt.Errorf("failed to write checkpoint metadata: %w", err)
		}
	}
	writeCommittedSpan.End()
	writeV1Duration := time.Since(writeV1Start)

	writeV2Start := time.Now()
	writeV2Ctx, writeCommittedV2Span := perf.Start(ctx, "write_committed_v2")
	if v2 {
		if err := writeCommittedV2(writeV2Ctx, repo, writeOpts); err != nil {
			writeCommittedV2Span.RecordError(err)
			writeCommittedV2Span.End()
			return nil, fmt.Errorf("failed to write checkpoint metadata to v2: %w", err)
		}
	} else {
		writeCommittedV2IfEnabled(writeV2Ctx, repo, writeOpts)
	}
	writeTaskMetadataV2IfEnabled(writeV2Ctx, repo, checkpointID, state.SessionID, ref)
	writeCommittedV2Span.End()
	writeV2Duration := time.Since(writeV2Start)

	logging.Debug(logCtx, "condense timings",
		slog.String("session_id", state.SessionID),
		slog.String("checkpoint_id", checkpointID.String()),
		slog.Int64("extract_session_data_ms", extractDuration.Milliseconds()),
		slog.Int64("calculate_session_attribution_ms", attributionDuration.Milliseconds()),
		slog.Int64("redact_transcript_ms", redactDuration.Milliseconds()),
		slog.Int64("compact_transcript_v2_ms", compactResult.Duration.Milliseconds()),
		slog.Int64("write_committed_v1_ms", writeV1Duration.Milliseconds()),
		slog.Int64("write_committed_v2_ms", writeV2Duration.Milliseconds()),
		slog.Int64("total_ms", time.Since(condenseStart).Milliseconds()),
		slog.Int("transcript_bytes", len(sessionData.Transcript)),
		slog.Int("transcript_lines", sessionData.FullTranscriptLines),
	)

	// Count scoped (new-only) compact lines, not full compact lines,
	// so state.CompactTranscriptStart accumulates correctly.
	compactLines := 0
	if compactResult.Transcript != nil {
		fullLines := countCompactLines(compactResult.Transcript)
		compactLines = fullLines - compactResult.StartLine
	}

	return &CondenseResult{
		CheckpointID:           checkpointID,
		SessionID:              state.SessionID,
		CheckpointsCount:       state.StepCount,
		FilesTouched:           sessionData.FilesTouched,
		Prompts:                sessionData.Prompts,
		TotalTranscriptLines:   sessionData.FullTranscriptLines,
		CompactTranscriptLines: compactLines,
		Transcript:             sessionData.Transcript,
	}, nil
}

// redactOrDrop runs redactSessionTranscript and, on failure, logs a warning
// and returns empty bytes. Drop-on-failure is the long-standing contract here:
// hooks have no retry path, and a failed redaction must not block the commit.
func redactOrDrop(logCtx context.Context, transcript []byte, sessionID string, checkpointID id.CheckpointID) (redact.RedactedBytes, time.Duration) {
	redactedTranscript, redactDuration, err := redactSessionTranscript(logCtx, transcript)
	if err != nil {
		logging.Warn(logCtx, "failed to redact transcript secrets, dropping transcript for checkpoint",
			slog.String("session_id", sessionID),
			slog.String("checkpoint_id", checkpointID.String()),
			slog.String("error", err.Error()),
		)
		return redact.RedactedBytes{}, redactDuration
	}
	return redactedTranscript, redactDuration
}

// skipIfPostRedactionEmpty returns a Skipped result when redaction emptied the
// transcript AND the filtered FilesTouched is also empty. Without this, a
// session that passed the pre-redaction gate but got its transcript dropped by
// a malformed-JSONL redaction error would write a metadata-only stub.
func skipIfPostRedactionEmpty(logCtx context.Context, redactedTranscript redact.RedactedBytes, sessionData *ExtractedSessionData, state *SessionState, checkpointID id.CheckpointID) *CondenseResult {
	if redactedTranscript.Len() > 0 || len(sessionData.FilesTouched) > 0 {
		return nil
	}
	logging.Info(logCtx, "session skipped: nothing to persist after redaction",
		slog.String("session_id", state.SessionID),
		slog.String("agent_type", string(state.AgentType)),
		slog.String("checkpoint_id", checkpointID.String()),
	)
	return newSkippedResult(checkpointID, state.SessionID)
}

func newSkippedResult(checkpointID id.CheckpointID, sessionID string) *CondenseResult {
	return &CondenseResult{
		CheckpointID: checkpointID,
		SessionID:    sessionID,
		Skipped:      true,
	}
}

// redactSessionTranscript redacts the transcript once for use by both the compact
// package and the checkpoint stores. Returns the redacted bytes and the duration
// of the redaction operation for perf logging.
func redactSessionTranscript(ctx context.Context, transcript []byte) (redact.RedactedBytes, time.Duration, error) {
	start := time.Now()
	_, span := perf.Start(ctx, "redact_transcript")
	defer span.End()

	if len(transcript) == 0 {
		return redact.RedactedBytes{}, time.Since(start), nil
	}

	redacted, err := redactSessionJSONLBytes(transcript)
	if err != nil {
		span.RecordError(err)
		return redact.RedactedBytes{}, time.Since(start), fmt.Errorf("failed to redact transcript secrets: %w", err)
	}
	return redacted, time.Since(start), nil
}

// resolveShadowRef returns the shadow branch reference, preferring a pre-resolved
// ref when available and falling back to a repo lookup.
func resolveShadowRef(repo *git.Repository, branchName string, preResolved *plumbing.Reference) (ref *plumbing.Reference, exists bool) {
	if preResolved != nil {
		return preResolved, true
	}
	refName := plumbing.NewBranchReferenceName(branchName)
	resolved, err := repo.Reference(refName, true)
	if err != nil {
		return nil, false
	}
	return resolved, true
}

// filterFilesTouched narrows sessionData.FilesTouched to files present in
// committedFiles. When no prior files were recorded, it falls back to the
// committed set (minus Entire metadata) — but only when sessionHasEvidenceOfWork
// is true. The fallback was originally unconditional, which let sessions that
// were registered at SessionStart but never produced anything (e.g. ephemeral
// Codex sessions whose hooks fired with a null transcript_path and never
// reached SaveStep) inherit another session's committed files.
func filterFilesTouched(sessionData *ExtractedSessionData, committedFiles map[string]struct{}, state *SessionState) {
	if len(committedFiles) == 0 {
		return
	}
	if len(sessionData.FilesTouched) > 0 {
		filtered := make([]string, 0, len(sessionData.FilesTouched))
		for _, f := range sessionData.FilesTouched {
			if _, ok := committedFiles[f]; ok {
				filtered = append(filtered, f)
			}
		}
		sessionData.FilesTouched = filtered
		return
	}
	if !sessionHasEvidenceOfWork(sessionData, state) {
		return
	}
	sessionData.FilesTouched = committedFilesExcludingMetadata(committedFiles)
}

// sessionHasEvidenceOfWork returns true when the session looks like a real
// participant — either it produced a readable transcript or a prior SaveStep
// recorded a checkpoint (StepCount > 0). False means the session was likely
// registered but never did anything; treating such a session as the author of
// the committed files would attribute another session's work to it.
func sessionHasEvidenceOfWork(sessionData *ExtractedSessionData, state *SessionState) bool {
	if len(sessionData.Transcript) > 0 {
		return true
	}
	return state != nil && state.StepCount > 0
}

// extractOrCreateSessionData tries to extract session data from the shadow branch,
// live transcript, or creates empty session data as a fallback. The empty case is
// handled by the skip gate in CondenseSession.
func (s *ManualCommitStrategy) extractOrCreateSessionData(ctx context.Context, repo *git.Repository, ag agent.Agent, shadowHash plumbing.Hash, hasShadowBranch bool, state *SessionState) (*ExtractedSessionData, error) {
	switch {
	case hasShadowBranch:
		// Shadow branch exists (from SaveStep commits) — extract transcript and
		// metadata from the branch tree, preferring the live transcript if fresher.
		data, err := s.extractSessionData(ctx, repo, shadowHash, state.SessionID, state.FilesTouched, state.AgentType, state.TranscriptPath, state.CheckpointTranscriptStart, state.Phase.IsActive())
		if err != nil {
			return nil, fmt.Errorf("failed to extract session data: %w", err)
		}
		return data, nil
	case state.TranscriptPath != "":
		// No shadow branch but a live transcript path is known — read directly
		// from disk. This handles mid-session commits before SaveStep runs.
		if state.Phase.IsActive() {
			prepareTranscriptIfNeeded(ctx, ag, state.TranscriptPath)
		}
		data, err := s.extractSessionDataFromLiveTranscript(ctx, state)
		if err != nil {
			return nil, fmt.Errorf("failed to extract session data from live transcript: %w", err)
		}
		return data, nil
	default:
		// No shadow branch and no transcript path — create empty session data.
		// This happens for sessions where the agent never set TranscriptPath
		// (e.g., Codex hooks may send null transcript_path). The skip gate in
		// CondenseSession will skip condensation if nothing is found.
		logging.Debug(logging.WithComponent(ctx, "checkpoint"),
			"no shadow branch and no transcript path, returning empty session data",
			slog.String("session_id", state.SessionID),
			slog.String("agent_type", string(state.AgentType)),
		)
		return &ExtractedSessionData{
			FilesTouched: state.FilesTouched,
		}, nil
	}
}

// compactTranscriptResult holds the output of compact transcript generation.
type compactTranscriptResult struct {
	Transcript []byte        // Entire Transcript Format (JSONL), redacted. Nil means "skip".
	StartLine  int           // Compact transcript line offset at checkpoint start.
	Duration   time.Duration // Time spent producing the compact transcript.
}

// compactAndRedactExternalTranscript calls the external agent's compact-transcript
// subcommand and redacts the result. Returns (nil, false) if the agent is not
// external. Returns (nil, true) if the agent is external but compaction failed.
func compactAndRedactExternalTranscript(ctx context.Context, ag agent.Agent, state *SessionState) (transcript []byte, isExternal bool) {
	compactor, ok := agent.AsTranscriptCompactor(ag)
	if !ok {
		if _, isCap := ag.(agent.CapabilityDeclarer); isCap {
			logging.Warn(ctx, "external transcript compaction unavailable, skipping transcript.jsonl",
				slog.String("session_id", state.SessionID),
				slog.String("agent", string(ag.Name())),
			)
			return nil, true
		}
		return nil, false
	}

	compacted := compactTranscriptForExternalAgent(ctx, compactor, state.SessionID, state.TranscriptPath)
	if compacted == nil {
		return nil, true
	}

	redacted, err := redactSessionJSONLBytes(compacted.Transcript)
	if err != nil {
		logging.Warn(ctx, "failed to redact external compact transcript, dropping",
			slog.String("session_id", state.SessionID),
			slog.String("agent", string(compactor.Name())),
			slog.String("error", err.Error()),
		)
		return nil, true
	}
	return redacted.Bytes(), true
}

// buildExternalCompactTranscript produces the compact transcript for external
// agents by calling the agent's compact-transcript subcommand and redacting
// the result. Returns nil if the agent is not external (caller should use
// buildInternalCompactTranscript instead).
func buildExternalCompactTranscript(ctx context.Context, ag agent.Agent, state *SessionState) *compactTranscriptResult {
	if !settings.IsCheckpointsV2Enabled(ctx) {
		return nil
	}

	compactStart := time.Now()
	compactCtx, compactSpan := perf.Start(ctx, "compact_transcript_v2")
	defer compactSpan.End()

	transcript, isExternal := compactAndRedactExternalTranscript(compactCtx, ag, state)
	if !isExternal {
		return nil
	}
	if transcript == nil {
		return &compactTranscriptResult{Duration: time.Since(compactStart)}
	}

	startLine := state.CompactTranscriptStart
	fullLines := countCompactLines(transcript)
	if fullLines < startLine {
		logging.Warn(compactCtx, "external compact transcript shorter than previous compact transcript start; resetting compact transcript start",
			slog.String("session_id", state.SessionID),
			slog.String("agent", string(ag.Name())),
			slog.Int("compact_transcript_lines", fullLines),
			slog.Int("previous_compact_transcript_start", startLine),
		)
		startLine = 0
	}

	return &compactTranscriptResult{
		Transcript: transcript,
		StartLine:  startLine,
		Duration:   time.Since(compactStart),
	}
}

// buildInternalCompactTranscript produces the compact transcript for built-in
// agents from already-redacted transcript bytes.
func buildInternalCompactTranscript(ctx context.Context, ag agent.Agent, redacted redact.RedactedBytes, state *SessionState) compactTranscriptResult {
	if !settings.IsCheckpointsV2Enabled(ctx) {
		return compactTranscriptResult{}
	}

	compactStart := time.Now()
	compactCtx, compactSpan := perf.Start(ctx, "compact_transcript_v2")
	defer compactSpan.End()

	// Generate scoped compact (only new content) for line counting and offset calculation.
	scopedCompact := compactTranscriptForV2(compactCtx, ag, redacted, state.CheckpointTranscriptStart)
	// Generate full compact (cumulative) for storage — v2 /main replaces
	// the session's transcript.jsonl on each write, so we must include all
	// prior content, not just the new portion.
	fullCompact := compactTranscriptForV2(compactCtx, ag, redacted, 0)
	startLine := computeCompactTranscriptStart(compactCtx, ag, state, redacted.Bytes(), scopedCompact)

	return compactTranscriptResult{
		Transcript: fullCompact,
		StartLine:  startLine,
		Duration:   time.Since(compactStart),
	}
}

func compactTranscriptForExternalAgent(
	ctx context.Context,
	compactor agent.TranscriptCompactor,
	sessionID string,
	transcriptPath string,
) *agent.CompactedTranscript {
	if transcriptPath == "" {
		logging.Warn(ctx, "external transcript compaction skipped: missing session transcript path",
			slog.String("session_id", sessionID),
			slog.String("agent", string(compactor.Name())),
		)
		return nil
	}

	compacted, err := compactor.CompactTranscript(ctx, transcriptPath)
	if err != nil {
		logging.Warn(ctx, "external transcript compaction failed, skipping transcript.jsonl on /main",
			slog.String("session_id", sessionID),
			slog.String("agent", string(compactor.Name())),
			slog.String("error", err.Error()),
		)
		return nil
	}
	if compacted == nil {
		logging.Warn(ctx, "external transcript compaction returned nil transcript",
			slog.String("session_id", sessionID),
			slog.String("agent", string(compactor.Name())),
		)
		return nil
	}
	if len(bytes.TrimSpace(compacted.Transcript)) == 0 {
		logging.Warn(ctx, "external transcript compaction returned empty transcript",
			slog.String("session_id", sessionID),
			slog.String("agent", string(compactor.Name())),
		)
		return nil
	}
	if !bytes.HasSuffix(compacted.Transcript, []byte{'\n'}) {
		compacted.Transcript = append(compacted.Transcript, '\n')
	}
	if len(compacted.Assets) > 0 {
		logging.Warn(ctx, "external transcript compaction returned assets that are not yet persisted",
			slog.String("session_id", sessionID),
			slog.String("agent", string(compactor.Name())),
			slog.Int("asset_count", len(compacted.Assets)),
		)
	}
	return compacted
}

// generateSummary produces an LLM-generated summary of the session transcript.
// The transcript must be pre-redacted to avoid sending secrets to the LLM.
// Returns nil if the scoped transcript is empty or generation fails.
func generateSummary(ctx context.Context, redactedTranscript redact.RedactedBytes, filesTouched []string, state *SessionState) *cpkg.Summary {
	summarizeCtx := logging.WithComponent(ctx, "summarize")
	transcriptBytes := redactedTranscript.Bytes()

	var scopedTranscript []byte
	switch state.AgentType {
	case agent.AgentTypeGemini:
		scoped, sliceErr := geminicli.SliceFromMessage(transcriptBytes, state.CheckpointTranscriptStart)
		if sliceErr != nil {
			logging.Warn(summarizeCtx, "failed to scope Gemini transcript for summary",
				slog.String("session_id", state.SessionID),
				slog.String("error", sliceErr.Error()))
		}
		scopedTranscript = scoped
	case agent.AgentTypeOpenCode:
		scoped, sliceErr := opencode.SliceFromMessage(transcriptBytes, state.CheckpointTranscriptStart)
		if sliceErr != nil {
			logging.Warn(summarizeCtx, "failed to scope OpenCode transcript for summary",
				slog.String("session_id", state.SessionID),
				slog.String("error", sliceErr.Error()))
		}
		scopedTranscript = scoped
	case agent.AgentTypeCodex, agent.AgentTypeClaudeCode, agent.AgentTypeCursor, agent.AgentTypeFactoryAIDroid, agent.AgentTypeUnknown:
		scopedTranscript = transcript.SliceFromLine(transcriptBytes, state.CheckpointTranscriptStart)
	}

	if len(scopedTranscript) == 0 {
		return nil
	}

	generator := buildSummaryGenerator(summarizeCtx)
	// scopedTranscript is sliced from redactedTranscript, which was redacted earlier in CondenseSession.
	summary, err := summarize.GenerateFromTranscript(summarizeCtx, redact.AlreadyRedacted(scopedTranscript), filesTouched, state.AgentType, generator)
	if err != nil {
		logging.Warn(summarizeCtx, "summary generation failed",
			slog.String("session_id", state.SessionID),
			slog.String("error", err.Error()))
		return nil
	}
	logging.Info(summarizeCtx, "summary generated",
		slog.String("session_id", state.SessionID))
	return summary
}

// buildSummaryGenerator returns a Generator based on the configured summary provider.
// Returns nil if no provider is configured (GenerateFromTranscript falls back to ClaudeGenerator).
//
// The return type is the summarize.Generator interface rather than the concrete
// adapter pointer so callers can't accidentally hold a non-nil interface that
// wraps a nil pointer (the classic Go nil-interface footgun).
func buildSummaryGenerator(ctx context.Context) summarize.Generator { //nolint:ireturn // interface return for nil-safety
	s, err := settings.Load(ctx)
	if err != nil {
		// Warn (not Debug): this is the auto-summarize hot path on every commit.
		// A settings-load failure silently downgrades the user's configured
		// provider to the default, and Debug would hide that from operators.
		logging.Warn(ctx, "could not load settings for summary provider, using default",
			"error", err.Error())
		return nil
	}
	if s.SummaryGeneration == nil || s.SummaryGeneration.Provider == "" {
		return nil
	}

	providerName := types.AgentName(s.SummaryGeneration.Provider)
	ag, err := agent.Get(providerName)
	if err != nil {
		logging.Warn(ctx, "configured summary provider not available, using default",
			"provider", s.SummaryGeneration.Provider, "error", err.Error())
		return nil
	}

	// Check binary on PATH, not DetectPresence — a repo can use one agent
	// for development while a different agent generates summaries. Fall back
	// silently (Warn log) because this runs in the post-commit hook and a
	// hard error would block the commit.
	if !agent.IsSummaryCLIAvailable(providerName) {
		logging.Warn(ctx, "configured summary provider CLI binary not on PATH, using default",
			"provider", s.SummaryGeneration.Provider)
		return nil
	}

	tg, ok := agent.AsTextGenerator(ag)
	if !ok {
		logging.Warn(ctx, "configured summary provider does not support text generation, using default",
			"provider", s.SummaryGeneration.Provider)
		return nil
	}

	return &summarize.TextGeneratorAdapter{
		TextGenerator: tg,
		Model:         summarize.ResolveModel(providerName, s.SummaryGeneration.Model),
	}
}

// marshalPromptAttributionsIncludingPending builds the complete prompt attribution slice
// (including PendingPromptAttribution for mid-turn commits) and encodes it to JSON.
// This must stay consistent with the slice used by calculateSessionAttributions so the
// persisted diagnostics match the computed InitialAttribution.
func marshalPromptAttributionsIncludingPending(state *SessionState) json.RawMessage {
	pas := make([]PromptAttribution, len(state.PromptAttributions), len(state.PromptAttributions)+1)
	copy(pas, state.PromptAttributions)
	if state.PendingPromptAttribution != nil {
		pas = append(pas, *state.PendingPromptAttribution)
	}
	if len(pas) == 0 {
		return nil
	}
	data, err := json.Marshal(pas)
	if err != nil {
		return nil
	}
	return data
}

// buildSessionMetrics creates a SessionMetrics from session state if any metrics are available.
// Returns nil if no hook-provided metrics exist (e.g., for agents that don't report them).
func buildSessionMetrics(state *SessionState) *cpkg.SessionMetrics {
	if state.SessionDurationMs == 0 && state.SessionTurnCount == 0 && state.ContextTokens == 0 && state.ContextWindowSize == 0 {
		return nil
	}
	return &cpkg.SessionMetrics{
		DurationMs:        state.SessionDurationMs,
		TurnCount:         state.SessionTurnCount,
		ContextTokens:     state.ContextTokens,
		ContextWindowSize: state.ContextWindowSize,
	}
}

func hasTokenUsageData(usage *agent.TokenUsage) bool {
	if usage == nil {
		return false
	}

	if usage.InputTokens > 0 || usage.CacheCreationTokens > 0 || usage.CacheReadTokens > 0 || usage.OutputTokens > 0 || usage.APICallCount > 0 {
		return true
	}

	return hasTokenUsageData(usage.SubagentTokens)
}

// sessionStateBackfillTokenUsage returns the best session-level token usage to
// persist in session state after condensation.
func sessionStateBackfillTokenUsage(ctx context.Context, ag agent.Agent, agentType types.AgentType, transcript []byte, checkpointUsage *agent.TokenUsage) *agent.TokenUsage {
	if agentType == agent.AgentTypeCopilotCLI && len(transcript) > 0 {
		fullSessionUsage := agent.CalculateTokenUsage(ctx, ag, transcript, 0, "")
		if hasTokenUsageData(fullSessionUsage) {
			return fullSessionUsage
		}
		logging.Debug(ctx, "copilot-cli: full-session token read produced no data, falling back to checkpoint usage")
	}

	if agentType == agent.AgentTypeCopilotCLI && hasTokenUsageData(checkpointUsage) {
		return checkpointUsage
	}

	if checkpointUsage != nil && checkpointUsage.InputTokens > 0 {
		return checkpointUsage
	}

	return nil
}

// attributionOpts provides pre-resolved git objects to avoid redundant reads.
type attributionOpts struct {
	headTree              *object.Tree        // HEAD commit tree (already resolved by PostCommit)
	shadowTree            *object.Tree        // Shadow branch tree (already resolved by PostCommit)
	parentTree            *object.Tree        // Parent commit tree (nil for initial commits, for consistent non-agent line counting)
	repoDir               string              // Repository worktree path for git CLI commands
	parentCommitHash      string              // HEAD's first parent hash (preferred diff base for non-agent files)
	attributionBaseCommit string              // Base commit hash for non-agent file detection (empty = fall back to go-git tree walk)
	headCommitHash        string              // HEAD commit hash for non-agent file detection (empty = fall back to go-git tree walk)
	allAgentFiles         map[string]struct{} // Union of all sessions' FilesTouched (nil = single-session)
}

func calculateSessionAttributions(ctx context.Context, repo *git.Repository, shadowRef *plumbing.Reference, sessionData *ExtractedSessionData, state *SessionState, opts ...attributionOpts) *cpkg.InitialAttribution {
	// Calculate initial attribution using accumulated prompt attribution data.
	// This uses user edits captured at each prompt start (before agent works),
	// plus any user edits after the final checkpoint (shadow → head).
	//
	// When shadowRef is nil (agent committed mid-turn before SaveStep),
	// HEAD is used as the shadow tree. This is correct because the agent's
	// commit IS HEAD — there are no user edits between agent work and commit.
	logCtx := logging.WithComponent(ctx, "attribution")

	var o attributionOpts
	if len(opts) > 0 {
		o = opts[0]
	}

	headTree := o.headTree
	if headTree == nil {
		headRef, headErr := repo.Head()
		if headErr != nil {
			logging.Debug(logCtx, "attribution skipped: failed to get HEAD",
				slog.String("error", headErr.Error()))
			return nil
		}

		headCommit, commitErr := repo.CommitObject(headRef.Hash())
		if commitErr != nil {
			logging.Debug(logCtx, "attribution skipped: failed to get HEAD commit",
				slog.String("error", commitErr.Error()))
			return nil
		}

		var treeErr error
		headTree, treeErr = headCommit.Tree()
		if treeErr != nil {
			logging.Debug(logCtx, "attribution skipped: failed to get HEAD tree",
				slog.String("error", treeErr.Error()))
			return nil
		}
	}

	// Get shadow tree: from pre-resolved cache, shadow branch, or HEAD (agent committed directly).
	shadowTree := o.shadowTree
	if shadowTree == nil {
		if shadowRef != nil {
			shadowCommit, shadowErr := repo.CommitObject(shadowRef.Hash())
			if shadowErr != nil {
				logging.Debug(logCtx, "attribution skipped: failed to get shadow commit",
					slog.String("error", shadowErr.Error()),
					slog.String("shadow_ref", shadowRef.Hash().String()))
				return nil
			}
			var shadowTreeErr error
			shadowTree, shadowTreeErr = shadowCommit.Tree()
			if shadowTreeErr != nil {
				logging.Debug(logCtx, "attribution skipped: failed to get shadow tree",
					slog.String("error", shadowTreeErr.Error()))
				return nil
			}
		} else {
			// No shadow branch: agent committed mid-turn. Use HEAD as shadow
			// because the agent's work is the commit itself.
			logging.Debug(logCtx, "attribution: using HEAD as shadow (no shadow branch)")
			shadowTree = headTree
		}
	}

	// Get base tree (state before session started)
	var baseTree *object.Tree
	attrBase := state.AttributionBaseCommit
	if attrBase == "" {
		attrBase = state.BaseCommit // backward compat
	}
	if baseCommit, baseErr := repo.CommitObject(plumbing.NewHash(attrBase)); baseErr == nil {
		if tree, baseTErr := baseCommit.Tree(); baseTErr == nil {
			baseTree = tree
		} else {
			logging.Debug(logCtx, "attribution: base tree unavailable",
				slog.String("error", baseTErr.Error()))
		}
	} else {
		logging.Debug(logCtx, "attribution: base commit unavailable",
			slog.String("error", baseErr.Error()),
			slog.String("attribution_base", attrBase))
	}

	// Include PendingPromptAttribution if it was never moved to PromptAttributions.
	// This happens when an agent commits mid-turn without calling SaveStep (e.g., Codex).
	// PendingPromptAttribution is set during UserPromptSubmit but only moved to
	// PromptAttributions during SaveStep. Without this, mid-turn commits have no PA
	// data and pre-session worktree dirt cannot be identified for baseline exclusion.
	promptAttrs := state.PromptAttributions
	if state.PendingPromptAttribution != nil {
		promptAttrs = append(promptAttrs, *state.PendingPromptAttribution)
	}

	// Log accumulated prompt attributions for debugging
	var totalUserAdded, totalUserRemoved int
	for i, pa := range promptAttrs {
		totalUserAdded += pa.UserLinesAdded
		totalUserRemoved += pa.UserLinesRemoved
		logging.Debug(logCtx, "prompt attribution data",
			slog.Int("checkpoint", pa.CheckpointNumber),
			slog.Int("user_added", pa.UserLinesAdded),
			slog.Int("user_removed", pa.UserLinesRemoved),
			slog.Int("agent_added", pa.AgentLinesAdded),
			slog.Int("agent_removed", pa.AgentLinesRemoved),
			slog.Int("index", i))
	}

	attribution := CalculateAttributionWithAccumulated(ctx, AttributionParams{
		BaseTree:              baseTree,
		ShadowTree:            shadowTree,
		HeadTree:              headTree,
		ParentTree:            o.parentTree,
		FilesTouched:          sessionData.FilesTouched,
		PromptAttributions:    promptAttrs,
		RepoDir:               o.repoDir,
		ParentCommitHash:      o.parentCommitHash,
		AttributionBaseCommit: attrBase,
		HeadCommitHash:        o.headCommitHash,
		AllAgentFiles:         o.allAgentFiles,
	})

	if attribution != nil {
		logging.Info(logCtx, "attribution calculated",
			slog.Int("agent_lines", attribution.AgentLines),
			slog.Int("human_added", attribution.HumanAdded),
			slog.Int("human_modified", attribution.HumanModified),
			slog.Int("human_removed", attribution.HumanRemoved),
			slog.Int("total_committed", attribution.TotalCommitted),
			slog.Float64("agent_percentage", attribution.AgentPercentage),
			slog.Int("accumulated_user_added", totalUserAdded),
			slog.Int("accumulated_user_removed", totalUserRemoved),
			slog.Int("files_touched", len(sessionData.FilesTouched)))
	}

	return attribution
}

// committedFilesExcludingMetadata returns committed files with CLI metadata paths filtered out.
// `.entire/` files are created by `entire enable`, not by the agent, and should not be
// attributed as agent work when used as a fallback for sessions with no FilesTouched.
func committedFilesExcludingMetadata(committedFiles map[string]struct{}) []string {
	result := make([]string, 0, len(committedFiles))
	for f := range committedFiles {
		if strings.HasPrefix(f, ".entire/") || strings.HasPrefix(f, paths.EntireMetadataDir+"/") {
			continue
		}
		result = append(result, f)
	}
	slices.Sort(result)
	return result
}

// extractSessionData extracts session data from the shadow branch.
// filesTouched is the list of files tracked during the session (from SessionState.FilesTouched).
// agentType identifies the agent (e.g., "Gemini CLI", "Claude Code") to determine transcript format.
// liveTranscriptPath, when non-empty and readable, is preferred over the shadow branch copy.
// This handles the case where SaveStep was skipped (no code changes) but the transcript
// continued growing — the shadow branch copy would be stale.
// checkpointTranscriptStart is the line offset (Claude) or message index (Gemini) where the current checkpoint began.
func (s *ManualCommitStrategy) extractSessionData(ctx context.Context, repo *git.Repository, shadowRef plumbing.Hash, sessionID string, filesTouched []string, agentType types.AgentType, liveTranscriptPath string, checkpointTranscriptStart int, isActive bool) (*ExtractedSessionData, error) {
	ag, _ := agent.GetByAgentType(agentType) //nolint:errcheck // ag may be nil for unknown agent types; callers use type assertions so nil is safe
	commit, err := repo.CommitObject(shadowRef)
	if err != nil {
		return nil, fmt.Errorf("failed to get commit object: %w", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("failed to get commit tree: %w", err)
	}

	data := &ExtractedSessionData{}
	// sessionID is already an "entire session ID" (with date prefix)
	metadataDir := paths.SessionMetadataDirFromSessionID(sessionID)

	// Extract transcript — prefer the live file when available, fall back to shadow branch.
	// The shadow branch copy may be stale if the last turn ended without code changes
	// (SaveStep is only called when there are file modifications).
	var fullTranscript string
	if liveTranscriptPath != "" {
		// Ensure transcript file exists (OpenCode creates it lazily via `opencode export`).
		// Only wait for flush when the session is active — for idle/ended sessions the
		// transcript is already fully flushed (the Stop hook completed the flush).
		if isActive {
			prepareTranscriptIfNeeded(ctx, ag, liveTranscriptPath)
		}
		if liveData, readErr := os.ReadFile(liveTranscriptPath); readErr == nil && len(liveData) > 0 { //nolint:gosec // path from session state
			fullTranscript = string(liveData)
		}
	}
	if fullTranscript == "" {
		// Fall back to shadow branch copy
		if file, fileErr := tree.File(metadataDir + "/" + paths.TranscriptFileName); fileErr == nil {
			if content, contentErr := file.Contents(); contentErr == nil {
				fullTranscript = content
			}
		} else if file, fileErr := tree.File(metadataDir + "/" + paths.TranscriptFileNameLegacy); fileErr == nil {
			if content, contentErr := file.Contents(); contentErr == nil {
				fullTranscript = content
			}
		}
	}

	// Process transcript based on agent type
	if fullTranscript != "" {
		data.Transcript = []byte(fullTranscript)
		data.FullTranscriptLines = countTranscriptItems(agentType, fullTranscript)
		// Read prompts from shadow branch tree (source of truth after SaveStep)
		if file, fileErr := tree.File(metadataDir + "/" + paths.PromptFileName); fileErr == nil {
			if content, contentErr := file.Contents(); contentErr == nil && content != "" {
				data.Prompts = splitPromptContent(content)
			}
		}
		// Filesystem fallback (written at turn start, covers mid-turn commits)
		if len(data.Prompts) == 0 {
			data.Prompts = readPromptsFromFilesystem(ctx, sessionID)
		}
	}

	// Use tracked files from session state (not all files in tree)
	data.FilesTouched = filesTouched

	// Calculate token usage from the extracted transcript portion
	if len(data.Transcript) > 0 {
		data.TokenUsage = agent.CalculateTokenUsage(ctx, ag, data.Transcript, checkpointTranscriptStart, "") //TODO: why do we not use here subagents dir?
	}

	return data, nil
}

// extractSessionDataFromLiveTranscript extracts session data directly from the live transcript file.
// This is used for mid-session commits where no shadow branch exists yet.
func (s *ManualCommitStrategy) extractSessionDataFromLiveTranscript(ctx context.Context, state *SessionState) (*ExtractedSessionData, error) {
	data := &ExtractedSessionData{}

	ag, _ := agent.GetByAgentType(state.AgentType) //nolint:errcheck // ag may be nil for unknown agent types; callers use type assertions so nil is safe

	// Resolve the transcript path (handles agents that relocate mid-session).
	transcriptPath, resolveErr := resolveTranscriptPath(state)
	if resolveErr != nil {
		return nil, resolveErr
	}

	liveData, err := os.ReadFile(transcriptPath) //nolint:gosec // path validated by resolveTranscriptPath
	if err != nil {
		return nil, fmt.Errorf("failed to read live transcript: %w", err)
	}

	if len(liveData) == 0 {
		return nil, errors.New("live transcript is empty")
	}

	fullTranscript := string(liveData)
	data.Transcript = liveData
	data.FullTranscriptLines = countTranscriptItems(state.AgentType, fullTranscript)
	data.Prompts = readPromptsFromFilesystem(ctx, state.SessionID)

	// Resolve files touched: prefers hook-populated state, falls back to transcript extraction
	data.FilesTouched = s.resolveFilesTouched(ctx, state)

	// Calculate token usage from the extracted transcript portion
	if len(data.Transcript) > 0 {
		data.TokenUsage = agent.CalculateTokenUsage(ctx, ag, data.Transcript, state.CheckpointTranscriptStart, "") //TODO: why do we not use here subagents dir?
	}

	return data, nil
}

// countTranscriptItems counts lines (JSONL) or messages (JSON) in a transcript.
// For Claude Code and JSONL-based agents, this counts lines.
// For Gemini CLI, OpenCode, and JSON-based agents, this counts messages.
// Returns 0 if the content is empty or malformed.
func countTranscriptItems(agentType types.AgentType, content string) int {
	if content == "" {
		return 0
	}

	// OpenCode uses export JSON format with {"info": {...}, "messages": [...]}
	if agentType == agent.AgentTypeOpenCode {
		session, err := opencode.ParseExportSession([]byte(content))
		if err == nil && session != nil {
			return len(session.Messages)
		}
		return 0
	}

	// Try Gemini format first if agentType is Gemini, or as fallback if Unknown
	if agentType == agent.AgentTypeGemini || agentType == agent.AgentTypeUnknown {
		transcript, err := geminicli.ParseTranscript([]byte(content))
		if err == nil && transcript != nil && len(transcript.Messages) > 0 {
			return len(transcript.Messages)
		}
		// If agentType is explicitly Gemini but parsing failed, return 0
		if agentType == agent.AgentTypeGemini {
			return 0
		}
		// Otherwise fall through to JSONL parsing for Unknown type
	}

	// Claude Code and other JSONL-based agents
	allLines := strings.Split(content, "\n")
	// Trim trailing empty lines (from final \n in JSONL)
	for len(allLines) > 0 && strings.TrimSpace(allLines[len(allLines)-1]) == "" {
		allLines = allLines[:len(allLines)-1]
	}
	return len(allLines)
}

// extractUserPrompts extracts all user prompts from transcript content.
// Returns prompts with IDE context tags stripped (e.g., <ide_opened_file>).
func extractUserPrompts(agentType types.AgentType, content string) []string {
	if content == "" {
		return nil
	}

	// Droid has its own envelope format — use its parser to normalize first
	if agentType == agent.AgentTypeFactoryAIDroid {
		lines, _, err := factoryaidroid.ParseDroidTranscriptFromBytes([]byte(content), 0)
		if err != nil {
			return nil
		}
		var prompts []string
		for _, line := range lines {
			if line.Type != transcript.TypeUser {
				continue
			}
			if text := transcript.ExtractUserContent(line.Message); text != "" {
				if stripped := textutil.StripIDEContextTags(text); stripped != "" {
					prompts = append(prompts, stripped)
				}
			}
		}
		return prompts
	}

	// OpenCode uses JSONL with a different per-line schema than Claude Code
	if agentType == agent.AgentTypeOpenCode {
		prompts, err := opencode.ExtractAllUserPrompts([]byte(content))
		if err == nil && len(prompts) > 0 {
			cleaned := make([]string, 0, len(prompts))
			for _, prompt := range prompts {
				if stripped := textutil.StripIDEContextTags(prompt); stripped != "" {
					cleaned = append(cleaned, stripped)
				}
			}
			return cleaned
		}
		return nil
	}

	// Try Gemini format first if agentType is Gemini, or as fallback if Unknown
	if agentType == agent.AgentTypeGemini || agentType == agent.AgentTypeUnknown {
		prompts, err := geminicli.ExtractAllUserPrompts([]byte(content))
		if err == nil && len(prompts) > 0 {
			// Strip IDE context tags for consistency with Claude Code handling
			cleaned := make([]string, 0, len(prompts))
			for _, prompt := range prompts {
				if stripped := textutil.StripIDEContextTags(prompt); stripped != "" {
					cleaned = append(cleaned, stripped)
				}
			}
			return cleaned
		}
		// If agentType is explicitly Gemini but parsing failed, return nil
		if agentType == agent.AgentTypeGemini {
			return nil
		}
		// Otherwise fall through to JSONL parsing for Unknown type
	}

	// Claude Code and other JSONL-based agents
	return extractUserPromptsFromLines(strings.Split(content, "\n"))
}

// extractUserPromptsFromLines extracts user prompts from JSONL transcript lines.
// IDE-injected context tags (like <ide_opened_file>) are stripped from the results.
func extractUserPromptsFromLines(lines []string) []string {
	var prompts []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		var entry map[string]interface{}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}

		// Check for user message:
		// - Claude Code uses "type": "human" or "type": "user"
		// - Cursor uses "role": "user"
		msgType, _ := entry["type"].(string) //nolint:errcheck // type assertion on interface{} from JSON
		msgRole, _ := entry["role"].(string) //nolint:errcheck // type assertion on interface{} from JSON
		isUser := msgType == "human" || msgType == "user" || msgRole == "user"
		if !isUser {
			continue
		}

		// Extract message content
		message, ok := entry["message"].(map[string]interface{})
		if !ok {
			continue
		}

		// Handle string content
		if content, ok := message["content"].(string); ok && content != "" {
			cleaned := textutil.StripIDEContextTags(content)
			if cleaned != "" {
				prompts = append(prompts, cleaned)
			}
			continue
		}

		// Handle array content (e.g., multiple text blocks from VSCode)
		if arr, ok := message["content"].([]interface{}); ok {
			var texts []string
			for _, item := range arr {
				if m, ok := item.(map[string]interface{}); ok {
					if m["type"] == "text" {
						if text, ok := m["text"].(string); ok {
							texts = append(texts, text)
						}
					}
				}
			}
			if len(texts) > 0 {
				cleaned := textutil.StripIDEContextTags(strings.Join(texts, "\n\n"))
				if cleaned != "" {
					prompts = append(prompts, cleaned)
				}
			}
		}
	}
	return prompts
}

// splitPromptContent splits prompt.txt content on the "\n\n---\n\n" separator.
// Returns nil if content is empty.
func splitPromptContent(content string) []string {
	if content == "" {
		return nil
	}
	parts := strings.Split(content, "\n\n---\n\n")
	var result []string
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

// readPromptsFromFilesystem reads prompt.txt from the filesystem session metadata directory.
// This file is written at turn start and updated at each SaveStep, providing prompt data
// even for mid-turn commits where the shadow branch may not have been updated.
func readPromptsFromFilesystem(ctx context.Context, sessionID string) []string {
	sessionDir := paths.SessionMetadataDirFromSessionID(sessionID)
	sessionDirAbs, err := paths.AbsPath(ctx, sessionDir)
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(sessionDirAbs, paths.PromptFileName)) //nolint:gosec // path from session ID
	if err != nil || len(data) == 0 {
		return nil
	}
	return splitPromptContent(string(data))
}

// clearFilesystemPrompt removes the filesystem prompt.txt for a session.
// Called after condensation so subsequent checkpoints start fresh.
func clearFilesystemPrompt(ctx context.Context, sessionID string) {
	sessionDir := paths.SessionMetadataDirFromSessionID(sessionID)
	sessionDirAbs, err := paths.AbsPath(ctx, sessionDir)
	if err != nil {
		return
	}
	promptPath := filepath.Join(sessionDirAbs, paths.PromptFileName)
	_ = os.Remove(promptPath)
}

// CondenseSessionByID condenses a session by its ID and cleans up.
// This is used by "entire doctor" to salvage stuck sessions.
func (s *ManualCommitStrategy) CondenseSessionByID(ctx context.Context, sessionID string) error {
	logCtx := logging.WithComponent(ctx, "condense-by-id")

	// Load session state
	state, err := s.loadSessionState(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("failed to load session state: %w", err)
	}
	if state == nil {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	// Open repository
	repo, err := OpenRepository(ctx)
	if err != nil {
		return fmt.Errorf("failed to open repository: %w", err)
	}

	// Generate a checkpoint ID
	checkpointID, err := id.Generate()
	if err != nil {
		return fmt.Errorf("failed to generate checkpoint ID: %w", err)
	}

	// Check if shadow branch exists (required for condensation)
	shadowBranchName := getShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)
	refName := plumbing.NewBranchReferenceName(shadowBranchName)
	_, refErr := repo.Reference(refName, true)
	hasShadowBranch := refErr == nil

	if !hasShadowBranch {
		// No shadow branch means no checkpoint data to condense.
		// Just clean up the state file.
		logging.Info(logCtx, "no shadow branch for session, clearing state only",
			slog.String("session_id", sessionID),
			slog.String("shadow_branch", shadowBranchName),
		)
		if err := s.clearSessionState(ctx, sessionID); err != nil {
			return fmt.Errorf("failed to clear session state: %w", err)
		}
		return nil
	}

	// Condense the session
	result, err := s.CondenseSession(ctx, repo, checkpointID, state, nil)
	if err != nil {
		return fmt.Errorf("failed to condense session: %w", err)
	}

	if result.Skipped {
		// Nothing to condense. Mark fully condensed so entire doctor doesn't
		// keep retrying this empty session on every invocation.
		logging.Info(logCtx, "session condensation skipped (no transcript or files), marking fully condensed",
			slog.String("session_id", sessionID),
		)
		state.FullyCondensed = true
		return s.saveSessionState(ctx, state)
	}

	logging.Info(logCtx, "session condensed by ID",
		slog.String("session_id", sessionID),
		slog.String("checkpoint_id", result.CheckpointID.String()),
		slog.Int("checkpoints_condensed", result.CheckpointsCount),
	)

	// Update session state: reset step count and transition to idle
	state.StepCount = 0
	state.CheckpointTranscriptStart = result.TotalTranscriptLines
	state.CompactTranscriptStart += result.CompactTranscriptLines
	state.CheckpointTranscriptSize = int64(len(result.Transcript))
	state.Phase = session.PhaseIdle
	state.LastCheckpointID = checkpointID
	state.LastCheckpointCommitHash = state.BaseCommit
	state.RealignAttributionBase(state.BaseCommit)
	state.PromptAttributions = nil
	state.PendingPromptAttribution = nil

	if err := s.saveSessionState(ctx, state); err != nil {
		return fmt.Errorf("failed to save session state: %w", err)
	}

	// Clean up shadow branch if no other sessions need it
	if err := s.cleanupShadowBranchIfUnused(ctx, repo, shadowBranchName, sessionID); err != nil {
		logging.Warn(logCtx, "failed to clean up shadow branch",
			slog.String("shadow_branch", shadowBranchName),
			slog.String("error", err.Error()),
		)
		// Non-fatal: condensation succeeded, shadow branch cleanup is best-effort
	}

	return nil
}

// CondenseAndMarkFullyCondensed condenses an ENDED session and marks it
// FullyCondensed in one operation. Used by the session stop hook to eagerly
// clean up sessions so PostCommit doesn't have to process them.
//
// This does NOT call CondenseSessionByID because that method has two behaviors
// we don't want: (1) it calls clearSessionState when no shadow branch exists
// (deletes the state file entirely), and (2) it sets Phase = IDLE. Instead,
// we inline the condensation logic with ENDED-appropriate behavior.
//
// Fail-open: if condensation fails, the session is left in its current state
// and PostCommit will still process it on the next commit.
func (s *ManualCommitStrategy) CondenseAndMarkFullyCondensed(ctx context.Context, sessionID string) error {
	logCtx := logging.WithComponent(ctx, "checkpoint")

	state, err := s.loadSessionState(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("failed to load session state: %w", err)
	}
	if state == nil {
		return nil // No state file
	}

	// Sessions with FilesTouched must be processed by PostCommit for carry-forward
	// tracking — each user commit that overlaps with tracked files gets its own
	// checkpoint. Eagerly condensing here would prevent that 1:1 linkage.
	if len(state.FilesTouched) > 0 {
		return nil
	}

	// Only condense if there's uncondensed data
	if state.StepCount <= 0 {
		// No data and no files — mark FullyCondensed
		state.FullyCondensed = true
		return s.saveSessionState(ctx, state)
	}

	// Check if shadow branch exists — required for condensation
	repo, err := OpenRepository(ctx)
	if err != nil {
		logging.Warn(logCtx, "eager condense: failed to open repository",
			slog.String("session_id", sessionID),
			slog.String("error", err.Error()),
		)
		return nil // fail-open
	}

	shadowBranchName := getShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)
	refName := plumbing.NewBranchReferenceName(shadowBranchName)
	_, refErr := repo.Reference(refName, true)
	hasShadowBranch := refErr == nil

	if !hasShadowBranch {
		// No shadow branch = no checkpoint data to condense.
		// Unlike CondenseSessionByID, we do NOT delete the state file.
		logging.Info(logCtx, "eager condense: no shadow branch",
			slog.String("session_id", sessionID),
			slog.String("shadow_branch", shadowBranchName),
		)
		state.StepCount = 0
		state.FullyCondensed = true // FilesTouched is already empty (checked above)
		return s.saveSessionState(ctx, state)
	}

	// Generate checkpoint ID and condense
	checkpointID, err := id.Generate()
	if err != nil {
		logging.Warn(logCtx, "eager condense: failed to generate checkpoint ID",
			slog.String("error", err.Error()),
		)
		return nil // fail-open
	}

	// Condense with nil committedFiles (include all FilesTouched)
	result, err := s.CondenseSession(ctx, repo, checkpointID, state, nil)
	if err != nil {
		logging.Warn(logCtx, "eager condense on session stop failed, PostCommit will retry",
			slog.String("session_id", sessionID),
			slog.String("error", err.Error()),
		)
		return nil // fail-open
	}

	if result.Skipped {
		// No transcript or files — nothing to condense. Mark fully condensed
		// so PostCommit doesn't keep retrying this empty session.
		logging.Info(logCtx, "eager condense skipped (no transcript or files), marking fully condensed",
			slog.String("session_id", sessionID),
		)
		state.FullyCondensed = true
		return s.saveSessionState(ctx, state)
	}

	// Update state — keep Phase = ENDED (unlike CondenseSessionByID which sets IDLE)
	state.StepCount = 0
	state.CheckpointTranscriptStart = result.TotalTranscriptLines
	state.CompactTranscriptStart += result.CompactTranscriptLines
	state.LastCheckpointID = checkpointID
	state.LastCheckpointCommitHash = state.BaseCommit
	state.RealignAttributionBase(state.BaseCommit)
	state.PromptAttributions = nil
	state.PendingPromptAttribution = nil
	state.FullyCondensed = true // FilesTouched is already empty (checked above)
	// Phase stays ENDED — do NOT set to IDLE

	logging.Info(logCtx, "eager condense on session stop succeeded",
		slog.String("session_id", sessionID),
		slog.String("checkpoint_id", result.CheckpointID.String()),
	)

	if err := s.saveSessionState(ctx, state); err != nil {
		return fmt.Errorf("failed to save session state: %w", err)
	}

	// Clean up shadow branch
	if err := s.cleanupShadowBranchIfUnused(ctx, repo, shadowBranchName, sessionID); err != nil {
		logging.Warn(logCtx, "eager condense: failed to clean up shadow branch",
			slog.String("shadow_branch", shadowBranchName),
			slog.String("error", err.Error()),
		)
	}

	return nil
}

// cleanupShadowBranchIfUnused deletes a shadow branch if no other active sessions reference it.
func (s *ManualCommitStrategy) cleanupShadowBranchIfUnused(ctx context.Context, _ *git.Repository, shadowBranchName, excludeSessionID string) error {
	// List all session states to check if any other session uses this shadow branch
	allStates, err := s.listAllSessionStates(ctx)
	if err != nil {
		return fmt.Errorf("failed to list session states: %w", err)
	}

	for _, state := range allStates {
		if state.SessionID == excludeSessionID {
			continue
		}
		otherShadow := getShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)
		if otherShadow == shadowBranchName && state.StepCount > 0 {
			// Another session still needs this shadow branch
			return nil
		}
	}

	// No other sessions need it, delete the shadow branch via CLI
	// (go-git v5's RemoveReference doesn't persist with packed refs/worktrees)
	if err := DeleteBranchCLI(ctx, shadowBranchName); err != nil {
		// Branch already gone is not an error
		if errors.Is(err, ErrBranchNotFound) {
			return nil
		}
		return fmt.Errorf("failed to remove shadow branch: %w", err)
	}
	return nil
}

// compactTranscriptForV2 produces the Entire Transcript Format (transcript.jsonl)
// from a redacted agent transcript. Returns nil if compaction cannot be performed
// (nil agent, empty transcript, or compaction error) —
// callers treat nil as "skip writing transcript.jsonl to /main".
func compactTranscriptForV2(ctx context.Context, ag agent.Agent, transcript redact.RedactedBytes, checkpointTranscriptStart int) []byte {
	if ag == nil || transcript.Len() == 0 {
		return nil
	}

	compacted, err := compact.Compact(transcript, compact.MetadataFields{
		Agent:      string(ag.Name()),
		CLIVersion: versioninfo.Version,
		StartLine:  checkpointTranscriptStart,
	})
	if err != nil {
		logging.Warn(ctx, "compact transcript generation failed, skipping transcript.jsonl on /main",
			slog.String("agent", string(ag.Name())),
			slog.String("error", err.Error()),
		)
		return nil
	}
	return compacted
}

// countCompactLines returns line count for compact transcript JSONL.
func countCompactLines(compactTranscript []byte) int {
	return bytes.Count(compactTranscript, []byte{'\n'})
}

// computeCompactTranscriptStart chooses the compact transcript start line offset
// for v2 /main metadata.
//
// Preferred source is session state CompactTranscriptStart. For legacy sessions
// that have only full-transcript offsets persisted, this recalculates the compact
// offset from transcript bytes when possible. On any failure, returns 0 (fail-open).
func computeCompactTranscriptStart(ctx context.Context, ag agent.Agent, state *SessionState, transcript []byte, scopedCompact []byte) int {
	if state.CompactTranscriptStart > 0 {
		return state.CompactTranscriptStart
	}
	if state.CheckpointTranscriptStart == 0 || ag == nil || len(transcript) == 0 || len(scopedCompact) == 0 {
		return 0
	}

	// transcript is already redacted (passed as .Bytes() from RedactedBytes).
	fullCompacted, err := compact.Compact(redact.AlreadyRedacted(transcript), compact.MetadataFields{
		Agent:      string(ag.Name()),
		CLIVersion: versioninfo.Version,
		StartLine:  0,
	})
	if err != nil || len(fullCompacted) == 0 {
		logging.Warn(ctx, "failed to recalculate compact transcript start, using 0",
			slog.String("session_id", state.SessionID),
		)
		return 0
	}

	fullLines := countCompactLines(fullCompacted)
	scopedLines := countCompactLines(scopedCompact)
	offset := fullLines - scopedLines
	if offset < 0 {
		return 0
	}
	return offset
}

// writeCommittedV2 writes checkpoint data to v2 refs unconditionally.
// Callers decide whether to propagate or swallow the error (v2-only vs dual-write).
func writeCommittedV2(ctx context.Context, repo *git.Repository, opts cpkg.WriteCommittedOptions) error {
	v2URL, err := remote.FetchURL(ctx)
	if err != nil {
		logging.Debug(ctx, "manual-commit condensation: using origin for v2 write fetch remote",
			slog.String("error", err.Error()),
		)
		v2URL = originRemote
	}
	v2Store := cpkg.NewV2GitStore(repo, v2URL)
	if err := v2Store.WriteCommitted(ctx, opts); err != nil {
		return fmt.Errorf("v2 write committed: %w", err)
	}
	return nil
}

// writeCommittedV2IfEnabled writes checkpoint data to v2 refs when checkpoints_v2
// is enabled. Failures are logged as warnings — in dual-write mode v2 writes are
// best-effort and must not block the v1 path.
func writeCommittedV2IfEnabled(ctx context.Context, repo *git.Repository, opts cpkg.WriteCommittedOptions) {
	if !settings.IsCheckpointsV2Enabled(ctx) {
		return
	}
	if err := writeCommittedV2(ctx, repo, opts); err != nil {
		logging.Warn(ctx, "v2 dual-write failed",
			slog.String("checkpoint_id", opts.CheckpointID.String()),
			slog.String("error", err.Error()),
		)
	}
}

// writeTaskMetadataV2IfEnabled copies task metadata trees from the shadow branch
// to v2 /full/current when dual-write is enabled.
//
// This mirrors migrate's task backfill behavior for newly created checkpoints so
// task rewind artifacts (tasks/<tool-use-id>/...) are available in v2 immediately,
// not only after running `entire migrate --checkpoints v2`.
func writeTaskMetadataV2IfEnabled(
	ctx context.Context,
	repo *git.Repository,
	checkpointID id.CheckpointID,
	sessionID string,
	shadowRef *plumbing.Reference,
) {
	if !settings.IsCheckpointsV2Enabled(ctx) || shadowRef == nil {
		return
	}

	shadowCommit, err := repo.CommitObject(shadowRef.Hash())
	if err != nil {
		logging.Warn(ctx, "v2 dual-write task metadata copy skipped: failed to read shadow commit",
			slog.String("checkpoint_id", checkpointID.String()),
			slog.String("session_id", sessionID),
			slog.String("error", err.Error()),
		)
		return
	}

	shadowTree, err := shadowCommit.Tree()
	if err != nil {
		logging.Warn(ctx, "v2 dual-write task metadata copy skipped: failed to read shadow tree",
			slog.String("checkpoint_id", checkpointID.String()),
			slog.String("session_id", sessionID),
			slog.String("error", err.Error()),
		)
		return
	}

	tasksPath := paths.SessionMetadataDirFromSessionID(sessionID) + "/tasks"
	tasksTree, err := shadowTree.Tree(tasksPath)
	if err != nil {
		return
	}

	v2URL, err := remote.FetchURL(ctx)
	if err != nil {
		logging.Debug(ctx, "manual-commit condensation: using origin for v2 task metadata fetch remote",
			slog.String("error", err.Error()),
		)
		v2URL = originRemote
	}
	v2Store := cpkg.NewV2GitStore(repo, v2URL)
	sessionIndex, err := resolveV2SessionIndexForCheckpoint(repo, checkpointID, sessionID)
	if err != nil {
		logging.Warn(ctx, "v2 dual-write task metadata copy skipped: failed to resolve session index",
			slog.String("checkpoint_id", checkpointID.String()),
			slog.String("session_id", sessionID),
			slog.String("error", err.Error()),
		)
		return
	}

	if err := spliceTaskTreeToV2FullCurrent(ctx, repo, v2Store, checkpointID, sessionIndex, tasksTree.Hash); err != nil {
		logging.Warn(ctx, "v2 dual-write task metadata copy failed",
			slog.String("checkpoint_id", checkpointID.String()),
			slog.String("session_id", sessionID),
			slog.String("error", err.Error()),
		)
	}
}

func resolveV2SessionIndexForCheckpoint(repo *git.Repository, checkpointID id.CheckpointID, sessionID string) (int, error) {
	v2MainRef, err := repo.Reference(plumbing.ReferenceName(paths.V2MainRefName), true)
	if err != nil {
		return 0, fmt.Errorf("read v2 /main ref: %w", err)
	}
	v2MainCommit, err := repo.CommitObject(v2MainRef.Hash())
	if err != nil {
		return 0, fmt.Errorf("read v2 /main commit: %w", err)
	}
	v2MainTree, err := v2MainCommit.Tree()
	if err != nil {
		return 0, fmt.Errorf("read v2 /main tree: %w", err)
	}

	checkpointTree, err := v2MainTree.Tree(checkpointID.Path())
	if err != nil {
		return 0, fmt.Errorf("read checkpoint subtree on v2 /main: %w", err)
	}

	metadataFile, err := checkpointTree.File(paths.MetadataFileName)
	if err != nil {
		return 0, fmt.Errorf("read checkpoint summary metadata: %w", err)
	}
	metadataContent, err := metadataFile.Contents()
	if err != nil {
		return 0, fmt.Errorf("read checkpoint summary contents: %w", err)
	}

	var summary cpkg.CheckpointSummary
	if err := json.Unmarshal([]byte(metadataContent), &summary); err != nil {
		return 0, fmt.Errorf("parse checkpoint summary metadata: %w", err)
	}

	for i := range len(summary.Sessions) {
		sessionTree, err := checkpointTree.Tree(strconv.Itoa(i))
		if err != nil {
			continue
		}
		sessionMetadataFile, err := sessionTree.File(paths.MetadataFileName)
		if err != nil {
			continue
		}
		sessionMetadataContent, err := sessionMetadataFile.Contents()
		if err != nil {
			continue
		}

		var sessionMeta cpkg.CommittedMetadata
		if err := json.Unmarshal([]byte(sessionMetadataContent), &sessionMeta); err != nil {
			continue
		}
		if sessionMeta.SessionID == sessionID {
			return i, nil
		}
	}

	return 0, fmt.Errorf("session %q not found in v2 checkpoint %s", sessionID, checkpointID)
}

func spliceTaskTreeToV2FullCurrent(
	ctx context.Context,
	repo *git.Repository,
	v2Store *cpkg.V2GitStore,
	checkpointID id.CheckpointID,
	sessionIndex int,
	tasksTreeHash plumbing.Hash,
) error {
	refName := plumbing.ReferenceName(paths.V2FullCurrentRefName)
	parentHash, rootTreeHash, err := v2Store.GetRefState(refName)
	if err != nil {
		return fmt.Errorf("get v2 /full/current ref state: %w", err)
	}
	incomingTasksTree, err := repo.TreeObject(tasksTreeHash)
	if err != nil {
		return fmt.Errorf("read task tree: %w", err)
	}

	shardPrefix := string(checkpointID[:2])
	shardSuffix := string(checkpointID[2:])
	sessionDir := strconv.Itoa(sessionIndex)

	newRootHash, err := cpkg.UpdateSubtree(repo, rootTreeHash,
		[]string{shardPrefix, shardSuffix, sessionDir, "tasks"},
		incomingTasksTree.Entries,
		cpkg.UpdateSubtreeOptions{MergeMode: cpkg.MergeKeepExisting},
	)
	if err != nil {
		return fmt.Errorf("splice task tree into v2 /full/current: %w", err)
	}

	authorName, authorEmail := cpkg.GetGitAuthorFromRepo(repo)
	commitHash, err := cpkg.CreateCommit(ctx, repo, newRootHash, parentHash,
		fmt.Sprintf("Checkpoint: %s (task metadata)\n", checkpointID),
		authorName, authorEmail)
	if err != nil {
		return fmt.Errorf("create v2 task metadata commit: %w", err)
	}

	if err := repo.Storer.SetReference(plumbing.NewHashReference(refName, commitHash)); err != nil {
		return fmt.Errorf("update v2 /full/current ref: %w", err)
	}

	return nil
}
