package strategy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/external"
	"github.com/entireio/cli/cmd/entire/cli/agent/geminicli"
	"github.com/entireio/cli/cmd/entire/cli/agent/opencode"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	cpkg "github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/checkpointpolicy"
	"github.com/entireio/cli/cmd/entire/cli/entiredir"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/summarize"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
	"github.com/entireio/cli/cmd/entire/cli/transcript/imageextract"
	"github.com/entireio/cli/cmd/entire/cli/validation"
	"github.com/entireio/cli/perf"
	"github.com/entireio/cli/redact"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

var (
	discoverExternalSummaryProviders = external.DiscoverAndRegister
	isSummaryProviderCLIAvailable    = agent.IsSummaryCLIAvailable
)

// listCheckpoints returns all checkpoints from committed checkpoint storage.
func (s *ManualCommitStrategy) listCheckpoints(ctx context.Context) ([]CheckpointInfo, error) {
	repo, err := OpenRepository(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to open git repository: %w", err)
	}
	defer repo.Close()

	WarnIfMetadataDisconnected(ctx)
	store, err := s.getPersistentStore(ctx, repo)
	if err != nil {
		return nil, err
	}

	committed, err := store.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list committed checkpoints: %w", err)
	}

	return checkpointInfosFromCommitted(committed), nil
}

// getCheckpointLog returns the transcript for a specific checkpoint ID.
func (s *ManualCommitStrategy) getCheckpointLog(ctx context.Context, checkpointID id.CheckpointID) ([]byte, error) {
	repo, err := OpenRepository(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to open git repository: %w", err)
	}
	defer repo.Close()

	WarnIfMetadataDisconnected(ctx)
	store, err := s.getPersistentStore(ctx, repo)
	if err != nil {
		return nil, err
	}

	summary, err := cpkg.ReadCheckpoint(ctx, store, checkpointID)
	if err != nil {
		return nil, fmt.Errorf("failed to read checkpoint: %w", err)
	}
	content, err := cpkg.ReadLatestSessionContent(ctx, store, checkpointID, summary)
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

	// reconcileInterrupted allows this condensation to return a *different*
	// checkpoint ID than the caller passed, when it recognises the transcript
	// range in a checkpoint left behind by an interrupted write from a CLI that
	// predates reservations. Only a caller that chooses the ID itself may set it.
	//
	// PostCommit must NOT: its ID comes from the commit's Entire-Checkpoint
	// trailer, which is already written and cannot be revised. Redirecting the
	// write there leaves the commit naming a checkpoint that was never stored,
	// and updateCombinedAttributionForCheckpoint writing attribution under the
	// same non-existent ID.
	reconcileInterrupted bool

	// searchProbeAllowed gates the telemetry-only search-usage transcript scan
	// (detectSearchUsage). nil or false means don't scan: the probe then stays
	// its zero value, which the payload layer refuses to present as a
	// measurement (searchProbe.measured). Only PostCommit — the sole path that
	// emits the commit-condensed signal — passes a gate; the doctor and
	// session-end condensation paths never scan because nothing would read the
	// result. The gate is memoized per commit by commitCondensedEmitter, so the
	// settings load behind it runs at most once per PostCommit.
	searchProbeAllowed func() bool
}

// redactSessionJSONLBytes runs the regex-only redaction pipeline (the
// eight always-on/opt-in layers) over a session transcript at
// post-commit condensation. OPF is intentionally NOT included here —
// it runs exclusively in the pre-push rewrite path
// (strategy/manual_commit_opf_rewrite.go), which re-redacts the
// regex-only blobs and produces OPF-applied (9-layer) commits before
// the push.
//
// Exposed as a var so tests can inject deterministic success/error
// returns. The signature still takes a context so the var can be
// re-wired to JSONLBytesWithPrivacyFilter from tests that need OPF.
var redactSessionJSONLBytes = func(_ context.Context, b []byte) (redact.RedactedBytes, error) {
	return redact.JSONLBytes(b)
}

// extractSessionImages lifts inline base64 images out of a transcript into
// externalized assets via the per-agent image codec, returning the rewritten
// (placeholder-bearing) transcript. Agents with no codec, or transcripts with no
// externalizable images, pass through unchanged. Injectable for tests.
var extractSessionImages = func(agentType types.AgentType, transcript []byte) ([]byte, []cpkg.TranscriptAsset, error) {
	codec := imageextract.CodecFor(agentType)
	if codec == nil {
		return transcript, nil, nil
	}
	rewritten, assets, err := codec.ExtractImages(transcript)
	if err != nil {
		return transcript, nil, fmt.Errorf("extract images: %w", err)
	}
	if len(assets) == 0 {
		return transcript, nil, nil
	}
	out := make([]cpkg.TranscriptAsset, len(assets))
	for i, a := range assets {
		out[i] = cpkg.TranscriptAsset{Name: a.Name, MediaType: a.MediaType, Data: a.Data}
	}
	return rewritten, out, nil
}

// externalizeSessionImages runs the opt-in image-externalization step over a
// session transcript before redaction. When enabled it returns the rewritten
// (placeholder-bearing) transcript plus the extracted assets; when disabled,
// unsupported for the agent, or on error it returns the transcript unchanged with
// nil assets (the checkpoint then stores the inline transcript).
//
// It deliberately does NOT mutate the caller's transcript: the pre-externalization
// bytes are what CondenseResult.TranscriptSizeBaseline is measured on, and that
// baseline must stay in the shadow-branch blob's coordinate (sanitized but NOT
// image-externalized, matching what the Stop path writes) — feeding it the shrunken
// externalized size would report spurious growth on every subsequent commit.
func externalizeSessionImages(ctx, logCtx context.Context, state *SessionState, transcript []byte) ([]byte, []cpkg.TranscriptAsset) {
	if !settings.IsImageExternalizationEnabled(ctx) {
		return transcript, nil
	}
	rewritten, assets, err := extractSessionImages(state.AgentType, transcript)
	if err != nil {
		logging.Warn(logCtx, "image externalization failed; leaving transcript inline",
			slog.String("session_id", state.SessionID),
			slog.String("error", err.Error()))
		return transcript, nil
	}
	return rewritten, assets
}

// prepareTranscriptForStorage runs the first two steps of the stored-copy pipeline
// in order — sanitize (drop non-portable agent state), then externalize inline
// images. Redaction is the caller's next step, so the whole pipeline reads
// sanitize -> externalize -> redact. See prepareTaskTranscriptForStorage for
// the twin that runs the same pipeline over a subagent's own transcript.
//
// Each step has to precede the next:
//
//   - Sanitize first, so we neither externalize images out of items we are about to
//     discard — which would store an asset whose referencing transcript line is gone
//     moments later — nor redact megabytes of ciphertext only to throw it away.
//     Base64 is the pathological input for the entropy layer, so on a large Codex
//     rollout that scan alone costs tens of seconds.
//   - Externalize before redaction, because base64 is high-entropy and redaction
//     would otherwise flag and destroy it.
//
// It returns the sanitized size rather than the sanitized bytes: that size is the
// coordinate the CheckpointTranscriptSize growth baseline must use (see
// CondenseResult.TranscriptSizeBaseline), and returning the slice would keep a
// second multi-MB buffer reachable for the rest of the condensation just to read
// its length.
func prepareTranscriptForStorage(
	ctx, logCtx context.Context,
	ag agent.Agent,
	state *SessionState,
	raw []byte,
) (externalized []byte, assets []cpkg.TranscriptAsset, sanitizedSize int64) {
	sanitized := agent.SanitizeTranscriptForStorage(ag, raw)
	externalized, assets = externalizeSessionImages(ctx, logCtx, state, sanitized)
	return externalized, assets, int64(len(sanitized))
}

// prepareTaskTranscriptForStorage runs the sanitize -> externalize -> redact
// chain for a single subagent transcript, mirroring prepareTranscriptForStorage's
// first two steps and then completing with the same redaction the session
// transcript gets (see redactSessionTranscript). It is a SEPARATE entry point
// rather than a wrapper around prepareTranscriptForStorage for one reason:
// that function returns the session transcript's sanitized SIZE, which seeds
// CondenseResult.TranscriptSizeBaseline — the session's own growth-dedup
// baseline (SessionState.CheckpointTranscriptSize). A task transcript must
// NEVER contribute to that coordinate (it is not the session transcript), so
// this helper only returns the pipeline's terminal artifacts: redacted bytes
// and any externalized image assets. Callers append the assets to the
// checkpoint's own Assets list (TaskPayload has no Assets field of its own).
//
// It also carries checkpoint.prepareSubagentTranscript's size guard, for the
// same reason: agent-<agent-id>.jsonl is neither chunked nor capped, and
// redaction runs at roughly 220ms/MB. The cap is measured against the SANITIZED
// bytes, not the raw ones — sanitizing strips the bulk (Codex encrypted_content
// runs to ~20% of a rollout's bytes), so measuring raw would drop a transcript
// oversized only by payloads about to be discarded.
func prepareTaskTranscriptForStorage(
	ctx, logCtx context.Context,
	ag agent.Agent,
	state *SessionState,
	path string,
	raw []byte,
) (redacted redact.RedactedBytes, assets []cpkg.TranscriptAsset, tooLarge bool, err error) {
	sanitized := agent.SanitizeTranscriptForStorage(ag, raw)
	if len(sanitized) > agent.MaxChunkSize {
		logging.Warn(logCtx, "subagent transcript exceeds the blob size cap; storing task without it",
			slog.String("session_id", state.SessionID),
			slog.String("path", path),
			slog.Int("raw_bytes", len(raw)),
			slog.Int("sanitized_bytes", len(sanitized)),
			slog.Int("cap", agent.MaxChunkSize))
		return redact.RedactedBytes{}, nil, true, nil
	}
	externalized, assets := externalizeSessionImages(ctx, logCtx, state, sanitized)
	// nil repo declines prefix reuse: a subagent transcript is written once per
	// task, not appended across checkpoints, so there is nothing to reuse.
	redacted, _, err = redactSessionTranscript(logCtx, nil, "", externalized)
	if err != nil {
		return redact.RedactedBytes{}, nil, false, err
	}
	return redacted, assets, false, nil
}

// resolveTaskTranscriptPath falls back to the agent-layout convention when a
// task record has no declared transcript path (e.g. an agent that reports the
// path only on some events, or a legacy record captured before an agent
// started reporting one at all). Mirrors cli.ResolveAgentTranscriptPath,
// which this package cannot call directly — the cli package imports strategy,
// so the reverse import would cycle — and the logic itself is small enough
// that duplicating it here beats introducing a new shared package for this
// one call site (moving the transcript resolver into paths is deliberately
// out of scope for the durable-records plan this implements).
func resolveTaskTranscriptPath(state *SessionState, agentID string) string {
	if agentID == "" || state.TranscriptPath == "" {
		return ""
	}
	transcriptDir := filepath.Dir(state.TranscriptPath)
	name := paths.AgentTranscriptFileName(agentID)
	if nested := filepath.Join(paths.SubagentsDir(transcriptDir, state.SessionID), name); fileExists(nested) {
		return nested
	}
	if legacy := filepath.Join(transcriptDir, name); fileExists(legacy) {
		return legacy
	}
	return ""
}

// taskTranscriptReasonUnresolvable, taskTranscriptReasonUnreadable,
// taskTranscriptReasonEmpty, taskTranscriptReasonRedactionFailed, and
// taskTranscriptReasonTooLarge are the stable
// TaskPayload.TranscriptUnavailableReason categories. They deliberately
// carry no path or underlying-error detail: task.json is pushed to
// entire/checkpoints/v1, so a local filesystem path (which os.ReadFile's error
// text embeds) must never end up in it. The detailed error goes only to
// logging.Warn at the call site.
const (
	taskTranscriptReasonUnresolvable    = "transcript path unresolvable"
	taskTranscriptReasonUnreadable      = "transcript unreadable"
	taskTranscriptReasonEmpty           = "transcript empty"
	taskTranscriptReasonRedactionFailed = "transcript redaction failed"
	taskTranscriptReasonTooLarge        = "transcript too large"
)

// materializeTaskRecords resolves and redacts each of state.TaskRecords'
// subagent transcripts for storage under this checkpoint's
// tasks/<tool-use-id>/ subtree — the durable-records materializer for #2058:
// subagent transcripts used to die at condensation because no producer ever
// set the (now deleted) WriteOptions.IsTask route.
//
// Every record is materialized on every condensation, whether completed or
// still in flight: a completed record's transcript is stored once here and
// then removed from session state by the caller (see resetCheckpointWindow);
// an in-flight record's transcript-so-far is stored and the record stays so
// the next condensation re-materializes it. A record whose transcript cannot
// be resolved or read still produces a payload — with TranscriptUnavailableReason
// set instead of a Transcript — so the pointer is never silently dropped.
//
// A record with an unsafe or empty ToolUseID, or one whose AgentID is unsafe
// or empty at the point a transcript would be written, is skipped entirely —
// no payload at all, not even a reason-only one: an unsafe ToolUseID has no
// safe tasks/<id>/ directory to put a task.json under in the first place, and
// an empty/unsafe AgentID would corrupt the agent-<id>.jsonl filename (see
// writeTaskRecordEntries, which re-validates as a last resort). This must
// never wedge condensation — a poisoned record produces zero payloads, not an
// error, and the caller's normal completed-record removal
// (resetCheckpointWindow) still drops it once completed, since a record that
// can never materialize would otherwise retry forever.
//
// Task assets are appended to the incoming assets slice and returned (rather
// than living on TaskPayload, which has no Assets field of its own) so they
// join the checkpoint's own Assets list — see WriteOptions.Assets.
func (s *ManualCommitStrategy) materializeTaskRecords(
	ctx, logCtx context.Context,
	ag agent.Agent,
	state *SessionState,
	assets []cpkg.TranscriptAsset,
) ([]cpkg.TaskPayload, []cpkg.TranscriptAsset) {
	if len(state.TaskRecords) == 0 {
		return nil, assets
	}

	payloads := make([]cpkg.TaskPayload, 0, len(state.TaskRecords))
	for _, record := range state.TaskRecords {
		if record.ToolUseID == "" || validation.ValidateToolUseID(record.ToolUseID) != nil {
			logging.Warn(logCtx, "skipping task record: unsafe or empty tool_use_id",
				slog.String("session_id", state.SessionID),
				slog.String("tool_use_id", record.ToolUseID),
			)
			continue
		}

		payload := cpkg.TaskPayload{
			ToolUseID:       record.ToolUseID,
			AgentID:         record.AgentID,
			SubagentType:    record.SubagentType,
			TaskDescription: record.TaskDescription,
			Files:           record.Files,
			TokenUsage:      record.TokenUsage,
			StartedAt:       record.StartedAt,
			CompletedAt:     record.CompletedAt,
		}

		// Candidate transcript paths, tried in order: the agent-declared path
		// first, then the agent-layout fallback — declared paths are
		// unreliable (agents relocate/clean up transcripts), which is why the
		// fallback resolver exists at all, so a declared-but-unreadable path
		// must not short-circuit past it.
		var candidates []string
		if record.DeclaredTranscriptPath != "" {
			candidates = append(candidates, record.DeclaredTranscriptPath)
		}
		if fallback := resolveTaskTranscriptPath(state, record.AgentID); fallback != "" && fallback != record.DeclaredTranscriptPath {
			candidates = append(candidates, fallback)
		}
		if len(candidates) == 0 {
			payload.TranscriptUnavailableReason = taskTranscriptReasonUnresolvable
			payloads = append(payloads, payload)
			continue
		}

		// A transcript is about to be read and, if valid, stored as
		// agent-<agent-id>.jsonl — the agent ID becomes part of that path, so
		// it must be present and path-safe before going any further. Skip the
		// WHOLE record rather than merely omitting the transcript: this is
		// the same "poisoned identifier" shape as the ToolUseID check above.
		if record.AgentID == "" || validation.ValidateAgentID(record.AgentID) != nil {
			logging.Warn(logCtx, "skipping task record: unsafe or missing agent_id",
				slog.String("session_id", state.SessionID),
				slog.String("tool_use_id", record.ToolUseID),
			)
			continue
		}

		raw, transcriptPath, readErr := readFirstTranscript(candidates)
		if readErr != nil {
			logging.Warn(logCtx, "failed to read subagent transcript; storing task without it",
				slog.String("session_id", state.SessionID),
				slog.String("tool_use_id", record.ToolUseID),
				slog.String("error", readErr.Error()),
			)
			payload.TranscriptUnavailableReason = taskTranscriptReasonUnreadable
			payloads = append(payloads, payload)
			continue
		}
		if len(raw) == 0 {
			payload.TranscriptUnavailableReason = taskTranscriptReasonEmpty
			payloads = append(payloads, payload)
			continue
		}

		redacted, taskAssets, tooLarge, prepErr := prepareTaskTranscriptForStorage(ctx, logCtx, ag, state, transcriptPath, raw)
		if tooLarge {
			payload.TranscriptUnavailableReason = taskTranscriptReasonTooLarge
			payloads = append(payloads, payload)
			continue
		}
		if prepErr != nil {
			logging.Warn(logCtx, "failed to redact subagent transcript; storing task without it",
				slog.String("session_id", state.SessionID),
				slog.String("tool_use_id", record.ToolUseID),
				slog.String("error", prepErr.Error()),
			)
			payload.TranscriptUnavailableReason = taskTranscriptReasonRedactionFailed
			payloads = append(payloads, payload)
			continue
		}

		payload.Transcript = redacted
		assets = append(assets, taskAssets...)
		payloads = append(payloads, payload)
	}

	return payloads, assets
}

// readFirstTranscript tries each candidate path in order and returns the bytes
// of the first one that reads successfully, along with the path it came from
// (for logging — never for storage). Returns the last error when every
// candidate fails (callers only reach here with at least one candidate).
func readFirstTranscript(candidates []string) ([]byte, string, error) {
	var lastErr error
	for _, path := range candidates {
		data, err := os.ReadFile(path) //nolint:gosec // path is agent-declared or resolved from session state, not user input
		if err == nil {
			return data, path, nil
		}
		lastErr = err
	}
	return nil, "", lastErr
}

// sidecarSessionImages captures images an agent stores OUTSIDE the transcript
// (e.g. Cursor's per-session SQLite blob store) as checkpoint assets, so they are
// preserved with the session even though they never appear in full.jsonl. Unlike
// externalizeSessionImages there is no transcript placeholder and no round trip:
// these assets are preserve/view-only (the agent reads its own store on restore).
//
// Gated on the same opt-in flag. Best-effort: agents without the capability, or
// any capture error, yield no assets (the checkpoint is written without them).
func sidecarSessionImages(ctx, logCtx context.Context, ag agent.Agent, state *SessionState) []cpkg.TranscriptAsset {
	if !settings.IsImageExternalizationEnabled(ctx) {
		return nil
	}
	provider, ok := agent.AsSidecarImageProvider(ag)
	if !ok {
		return nil
	}
	assets, err := provider.SidecarImages(ctx, state.TranscriptPath)
	if err != nil {
		logging.Warn(logCtx, "sidecar image capture failed; checkpoint stored without them",
			slog.String("session_id", state.SessionID),
			slog.String("error", err.Error()))
		return nil
	}
	if len(assets) == 0 {
		return nil
	}
	out := make([]cpkg.TranscriptAsset, len(assets))
	for i, a := range assets {
		out[i] = cpkg.TranscriptAsset{Name: a.Name, MediaType: a.MediaType, Data: a.Data}
	}
	return out
}

// checkpointStepCount returns the number of user prompts attributed to the
// checkpoint being written: the turns counted since the current window's base.
// The base is re-anchored (deferred) the next time a turn is counted after a
// checkpoint write, so back-to-back checkpoints with no prompt between them share
// a count. Floored at 1 so we never record 0 (covers a fast-path checkpoint
// before any turn, and exec-mode gaps where turns weren't counted). Attach has
// its own count (see attachStepCount); it does not go through this path.
func checkpointStepCount(s *SessionState) int {
	if w := s.SessionTurnCount - s.PromptWindowBase; w >= 1 {
		return w
	}
	return 1
}

// CondenseSession condenses a session's shadow branch to permanent storage.
// checkpointID is the 12-hex-char value from the Entire-Checkpoint trailer.
// Metadata is stored at sharded path: <checkpoint_id[:2]>/<checkpoint_id[2:]>/
// Uses checkpoint.PersistentStore.Write with a checkpoint.Session request for persistent storage.
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
	policy, err := readLocalCheckpointPolicy(logCtx, repo)
	if err != nil {
		return nil, fmt.Errorf("checkpoint policy could not be read: %w", err)
	}
	if !checkpointpolicy.CanSatisfyPolicy(policy) {
		warnIfCheckpointPolicyNeedsUpgrade(logCtx, policy)
		return nil, errors.New("checkpoint policy cannot be satisfied by this Entire CLI")
	}

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
	// while keeping checkpoint metadata scoped to CheckpointTranscriptStart. The
	// recompute drops SubagentTokens (subagentsDir=""); the helper preserves the
	// cumulative subagent total across the backfill so resetCheckpointWindow's
	// baseline does not regress to nil (finding 019f5ebf-a57e).
	applyBackfilledSessionTokenUsage(ctx, ag, state, sessionData.Transcript, sessionData.TokenUsage)

	if !hasTokenUsageData(sessionData.TokenUsage) && hasTokenUsageData(state.CheckpointTokenUsage) {
		// Whole-value fallback: accumulateTokenUsage already carries SubagentTokens.
		sessionData.TokenUsage = accumulateTokenUsage(nil, state.CheckpointTokenUsage)
	} else {
		// Refill only the subagent total the recompute dropped. Runs after
		// applyBackfilledSessionTokenUsage, which needs the usage without it.
		sessionData.TokenUsage = withSubagentTokensFrom(sessionData.TokenUsage, state.CheckpointTokenUsage)
	}

	// Backfill the model from the transcript for agents that don't report it via
	// hooks (e.g., Pi records message.model but its hook events carry no model
	// field). Only fills when the model is otherwise unknown — hook-reported
	// models take precedence.
	if state.ModelName == "" {
		if model := sessionStateBackfillModel(ctx, ag, sessionData.Transcript); model != "" {
			state.ModelName = model
		}
	}

	if skipped := skipIfNothingToCondense(logCtx, sessionData, state, checkpointID, hasShadowBranch); skipped != nil {
		return skipped, nil
	}

	filterFilesTouched(sessionData, committedFiles, state)

	// sessionData.Transcript is left as the raw transcript; only the sanitized,
	// externalized, redacted copy is stored.
	externalizedTranscript, extractedAssets, transcriptSizeBaseline := prepareTranscriptForStorage(ctx, logCtx, ag, state, sessionData.Transcript)

	redactedTranscript, redactDuration := redactOrDrop(logCtx, repo, state.SessionID, externalizedTranscript, checkpointID)
	if skipped := skipIfPostRedactionEmpty(logCtx, redactedTranscript, sessionData, state, checkpointID); skipped != nil {
		return skipped, nil
	}

	// Telemetry-only search probe, computed exactly once for every result this
	// function can return (the recovery result below included) so no
	// construction site can ship the zero value by omission — the fabricated
	// negative searchProbe.measured exists to catch. Gated: the scan is a
	// full-transcript pass, and only the PostCommit path, with telemetry
	// enabled, has a reader for it. detectSearchUsage maps a nil agent or
	// empty transcript to unsupported, which is the honest answer for the
	// no-transcript extraction branch.
	if o.searchProbeAllowed != nil && o.searchProbeAllowed() {
		sessionData.SearchProbe = detectSearchUsage(ag, sessionData.Transcript)
	}

	// Capture agent sidecar images (e.g. Cursor's SQLite store) after the skip
	// check, so the sqlite3 shell-out is avoided when the checkpoint is discarded.
	extractedAssets = append(extractedAssets, sidecarSessionImages(ctx, logCtx, ag, state)...)

	// Materialize this session's subagent task records (#2058): every
	// completed-or-in-flight record's transcript is resolved, redacted, and
	// stored under this checkpoint's tasks/<tool-use-id>/ subtree.
	taskPayloads, extractedAssets := s.materializeTaskRecords(ctx, logCtx, ag, state, extractedAssets)

	store, err := s.getPersistentStore(ctx, repo)
	if err != nil {
		return nil, err
	}
	recovery := recoverInterruptedCondensation(
		ctx, logCtx,
		o.reconcileInterrupted && state.NeedsCondensationRecovery(),
		store, state, redactedTranscript, extractedAssets, sessionData, transcriptSizeBaseline,
	)
	if recovery.done {
		return recovery.result, recovery.err
	}

	writeOpts, attributionDuration, newSkillEvents := buildCondensationWriteOptions(
		ctx, repo, ref, state, sessionData, redactedTranscript, extractedAssets,
		taskPayloads, checkpointID, shadowBranchName, o,
	)

	writeV1Start := time.Now()
	writeCtx, writeCommittedSpan := perf.Start(ctx, "write_committed_v1")
	writeRequest := condensationSessionWriteRequest(writeOpts)
	if err := store.Write(writeCtx, writeRequest); err != nil {
		writeCommittedSpan.RecordError(err)
		writeCommittedSpan.End()
		return nil, fmt.Errorf("failed to write checkpoint metadata: %w", err)
	}
	writeCommittedSpan.End()
	writeV1Duration := time.Since(writeV1Start)

	// Deferred prompt-window reset: a checkpoint was written, so the window base
	// must be re-anchored — but not now. We defer until the next counted turn (in
	// persistEventMetadataToState) so two checkpoints with no prompt between them
	// report the same count instead of the second showing 0.
	state.PromptWindowResetPending = true

	logging.Debug(logCtx, "condense timings",
		slog.String("session_id", state.SessionID),
		slog.String("checkpoint_id", checkpointID.String()),
		slog.Int64("extract_session_data_ms", extractDuration.Milliseconds()),
		slog.Int64("calculate_session_attribution_ms", attributionDuration.Milliseconds()),
		slog.Int64("redact_transcript_ms", redactDuration.Milliseconds()),
		slog.Int64("write_committed_v1_ms", writeV1Duration.Milliseconds()),
		slog.Int64("total_ms", time.Since(condenseStart).Milliseconds()),
		slog.Int("transcript_bytes", len(sessionData.Transcript)),
		slog.Int("transcript_lines", sessionData.FullTranscriptLines),
	)

	return &CondenseResult{
		CheckpointID:           checkpointID,
		SessionID:              state.SessionID,
		CheckpointsCount:       checkpointStepCount(state),
		FilesTouched:           sessionData.FilesTouched,
		Prompts:                sessionData.Prompts,
		TotalTranscriptLines:   sessionData.FullTranscriptLines,
		TranscriptSizeBaseline: transcriptSizeBaseline,
		NewSkillEvents:         newSkillEvents,
		SearchProbe:            sessionData.SearchProbe,
	}, nil
}

// condensationSessionWriteRequest returns the write request for one
// condensation. Every condensation write is ReservedSession, unconditionally.
//
// The tempting refinement — send ReservedSession only when this session's own
// pending attempt matches the checkpoint ID, and plain Session otherwise —
// is wrong, and wrong in a worse direction than the problem it solves. A
// checkpoint can hold several sessions, and only the one that reserved the ID
// carries the reservation in its state. Routing per session would send the
// reserving session's write to the backend the ID belongs to and every sibling's
// write to the configured primary, splitting one checkpoint across both
// backends. Reads are not symmetrical about that: a ULID resolves from git-refs
// only, so the siblings written to the branch would simply be invisible. See
// TestCondensationSessionWrites_KeepSharedReservedCheckpointInOneBackend.
//
// The cost of routing everything by ID format is narrow. It only diverges from
// the configured primary when the ID's format disagrees with the primary, and
// the two fail-soft defaults agree: GenerateCheckpointID mints hex when the
// checkpoints config cannot be read, and resolvePrimaryType defaults to
// git-branch on the same unreadable config. Reaching a mismatch on a *fresh* ID
// therefore takes a config read that succeeds for one call and fails for the
// other — and even then the checkpoint stays readable, because a hex ID under a
// git-refs primary falls through to the branch on read.
//
// If the per-checkpoint routing decision ever does need to be conditional, it
// has to be made once for the whole checkpoint (from the commit trailer or a
// checkpoint-level flag), never per session.
func condensationSessionWriteRequest(opts cpkg.WriteOptions) cpkg.WriteRequest {
	return cpkg.ReservedSession(opts)
}

func buildCondensationWriteOptions(
	ctx context.Context,
	repo *git.Repository,
	shadowRef *plumbing.Reference,
	state *SessionState,
	sessionData *ExtractedSessionData,
	transcript redact.RedactedBytes,
	assets []cpkg.TranscriptAsset,
	tasks []cpkg.TaskPayload,
	checkpointID id.CheckpointID,
	shadowBranchName string,
	o condenseOpts,
) (cpkg.WriteOptions, time.Duration, []agent.SkillEvent) {
	authorName, authorEmail := GetGitAuthorFromRepo(repo)
	attrBase := state.AttributionBaseCommit
	if attrBase == "" {
		attrBase = state.BaseCommit
	}

	attributionStart := time.Now()
	attrCtx, attributionSpan := perf.Start(ctx, "calculate_session_attribution")
	attribution := calculateSessionAttributions(attrCtx, repo, shadowRef, sessionData, state, attributionOpts{
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

	var summary *cpkg.Summary
	if settings.IsSummarizeEnabled(ctx) && transcript.Len() > 0 {
		summary = generateSummary(ctx, transcript, sessionData.FilesTouched, state)
	}

	// Post-commit emits regex-only blobs. OPF runs later in the
	// pre-push rewrite path, never here.
	newSkillEvents, skillEvents := persistNewSkillEvents(state, sessionData.SkillEvents)
	return cpkg.WriteOptions{
		CheckpointID:                checkpointID,
		SessionID:                   state.SessionID,
		Strategy:                    StrategyNameManualCommit,
		Branch:                      GetCurrentBranchName(repo),
		Transcript:                  transcript,
		Assets:                      assets,
		Tasks:                       tasks,
		Prompts:                     sessionData.Prompts,
		FilesTouched:                sessionData.FilesTouched,
		CheckpointsCount:            checkpointStepCount(state),
		SaveStepCount:               state.StepCount,
		EphemeralBranch:             shadowBranchName,
		AuthorName:                  authorName,
		AuthorEmail:                 authorEmail,
		Agent:                       state.AgentType,
		Model:                       state.ModelName,
		TurnID:                      state.TurnID,
		TranscriptIdentifierAtStart: state.TranscriptIdentifierAtStart,
		CheckpointTranscriptStart:   state.CheckpointTranscriptStart,
		TokenUsage:                  sessionData.TokenUsage,
		SkillEvents:                 skillEvents,
		SessionMetrics:              buildSessionMetrics(state),
		Attribution:                 attribution,
		PromptAttributionsJSON:      marshalPromptAttributionsIncludingPending(state),
		Summary:                     summary,
		Kind:                        string(state.Kind),
		ReviewSkills:                state.ReviewSkills,
		ReviewPrompt:                state.ReviewPrompt,
		HasReview:                   state.Kind.IsReview(),
		HasInvestigation:            state.Kind.IsInvestigate(),
		InvestigateRunID:            state.InvestigateRunID,
		InvestigateTopic:            state.InvestigateTopic,
	}, attributionDuration, newSkillEvents
}

// redactOrDrop runs redactSessionTranscript and, on failure, logs a warning
// and returns empty bytes. Drop-on-failure is the long-standing contract here:
// hooks have no retry path, and a failed redaction must not block the commit.
func redactOrDrop(logCtx context.Context, repo *git.Repository, sessionID string, transcript []byte, checkpointID id.CheckpointID) (redact.RedactedBytes, time.Duration) {
	redactedTranscript, redactDuration, err := redactSessionTranscript(logCtx, repo, sessionID, transcript)
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

// skipIfNothingToCondense returns a Skipped result when there is no
// transcript, no files touched, AND no task records — nothing meaningful to
// condense, so return early instead of writing a metadata-only stub. A
// records-only session (read-only background subagent, empty parent
// transcript) must still write: its task payloads are the checkpoint's content.
//
// This check MUST run before filterFilesTouched. That function's fallback
// assigns all committed files to sessions with empty FilesTouched (designed
// for mid-turn commits where SaveStep hasn't run yet). Without this ordering,
// genuinely empty sessions (no transcript, no shadow branch, no tracked files)
// would acquire committed files from the fallback and bypass this gate.
func skipIfNothingToCondense(logCtx context.Context, sessionData *ExtractedSessionData, state *SessionState, checkpointID id.CheckpointID, hasShadowBranch bool) *CondenseResult {
	if len(sessionData.Transcript) > 0 || len(sessionData.FilesTouched) > 0 || state.HasTaskContent() {
		return nil
	}
	logging.Info(logCtx, "session skipped: no transcript or files to condense",
		slog.String("session_id", state.SessionID),
		slog.String("agent_type", string(state.AgentType)),
		slog.String("checkpoint_id", checkpointID.String()),
		slog.Bool("has_shadow_branch", hasShadowBranch),
		slog.String("transcript_path", state.TranscriptPath),
	)
	return newSkippedResult(checkpointID, state.SessionID)
}

// skipIfPostRedactionEmpty returns a Skipped result when redaction emptied the
// transcript AND the filtered FilesTouched is also empty. Without this, a
// session that passed the pre-redaction gate but got its transcript dropped by
// a malformed-JSONL redaction error would write a metadata-only stub. A
// session with task payloads to materialize must still write even when its
// parent transcript redacts to empty.
func skipIfPostRedactionEmpty(logCtx context.Context, redactedTranscript redact.RedactedBytes, sessionData *ExtractedSessionData, state *SessionState, checkpointID id.CheckpointID) *CondenseResult {
	if redactedTranscript.Len() > 0 || len(sessionData.FilesTouched) > 0 || state.HasTaskContent() {
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
// of the redaction operation for perf logging. Also the redaction step
// prepareTaskTranscriptForStorage reuses for a subagent's own transcript.
//
// A non-nil repo with a sessionID opts into prefix reuse (see
// checkpoint/redact_cache.go); a nil repo or empty sessionID redacts the whole
// content, which is what the per-subagent caller wants -- a task transcript is
// not the append-only stream the cache assumes.
func redactSessionTranscript(
	ctx context.Context,
	repo *git.Repository,
	sessionID string,
	transcript []byte,
) (redact.RedactedBytes, time.Duration, error) {
	start := time.Now()
	_, span := perf.Start(ctx, "redact_transcript")
	defer span.End()

	if len(transcript) == 0 {
		return redact.RedactedBytes{}, time.Since(start), nil
	}

	redacted, err := cpkg.RedactTranscriptCached(ctx, repo, sessionID, transcript, redactSessionJSONLBytes)
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
// Task records deliberately do NOT count here: a record-bearing session may
// have touched no files at all (read-only subagent), so letting records
// qualify the session for the committed-files fallback would be the exact
// mis-attribution this guard prevents.
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
	summary, err := summarize.GenerateFromTranscript(summarizeCtx, redact.AlreadyRedacted(scopedTranscript), filesTouched, state.AgentType, generator, nil) // no progress in the auto-summary hot path
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
func buildSummaryGenerator(ctx context.Context) summarize.Generator {
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
		discoverExternalSummaryProviders(ctx)
		ag, err = agent.Get(providerName)
		if err != nil {
			logging.Warn(ctx, "configured summary provider not available, using default",
				"provider", s.SummaryGeneration.Provider, "error", err.Error())
			return nil
		}
	}

	tg, ok := agent.AsTextGenerator(ag)
	if !ok {
		logging.Warn(ctx, "configured summary provider does not support text generation, using default",
			"provider", s.SummaryGeneration.Provider)
		return nil
	}

	// Check binary on PATH, not DetectPresence — a repo can use one agent
	// for development while a different agent generates summaries. Fall back
	// silently (Warn log) because this runs in the post-commit hook and a
	// hard error would block the commit.
	if !external.IsExternal(ag) && !isSummaryProviderCLIAvailable(providerName) {
		logging.Warn(ctx, "configured summary provider CLI binary not on PATH, using default",
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
// persisted diagnostics match the computed Attribution.
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

// withSubagentTokensFrom fills usage's SubagentTokens from src when usage has none
// of its own, returning a copy. It exists because the transcript recompute runs
// with subagentsDir="" and so always yields a nil SubagentTokens (see
// extractSessionData), which would otherwise replace a total already computed.
//
// The caller picks the source, and the two callers deliberately pick differently:
// condensation passes state.CheckpointTokenUsage (this window's total, already
// rescoped by SaveStep, so committed checkpoints stay summable rather than each
// re-reporting the session total), while applyBackfilledSessionTokenUsage passes
// state.TokenUsage (the session-wide cumulative, which is what
// resetCheckpointWindow must later snapshot as the next baseline).
//
// Copies rather than mutates: applyBackfilledSessionTokenUsage can adopt the
// checkpoint usage as state.TokenUsage (Copilot CLI), so mutating in place would
// overwrite the cumulative with a window delta.
//
// Known gap: a mid-turn commit that condenses before any SaveStep in the window has
// no CheckpointTokenUsage to draw on, so it records no subagent tokens. The live
// path could resolve a subagents dir from session state (as review/manifest.go
// does) and rescope against SubagentTokensBaseline; deferred, not blocked.
func withSubagentTokensFrom(usage, src *agent.TokenUsage) *agent.TokenUsage {
	if usage == nil || usage.SubagentTokens != nil || src == nil || src.SubagentTokens == nil {
		return usage
	}
	filled := *usage
	filled.SubagentTokens = src.SubagentTokens
	return &filled
}

// applyBackfilledSessionTokenUsage overwrites state.TokenUsage with the
// transcript-recomputed session total (see sessionStateBackfillTokenUsage) when
// one is available, preserving the cumulative subagent total across the backfill.
//
// resetCheckpointWindow captures the next window's baseline from
// state.TokenUsage.SubagentTokens after CondenseSession returns, so letting the
// backfill drop it would make the baseline nil and the next checkpoint re-report
// the full cumulative subagent total — hence the withSubagentTokensFrom fill,
// which copies so the cumulative is never mixed into checkpointUsage (the
// checkpoint-scoped value written to metadata).
func applyBackfilledSessionTokenUsage(ctx context.Context, ag agent.Agent, state *SessionState, transcript []byte, checkpointUsage *agent.TokenUsage) {
	backfillUsage := sessionStateBackfillTokenUsage(ctx, ag, state.AgentType, transcript, checkpointUsage)
	if backfillUsage == nil {
		return
	}
	state.TokenUsage = withSubagentTokensFrom(backfillUsage, state.TokenUsage)
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

// sessionStateBackfillModel extracts the LLM model from the transcript for
// agents that don't report it through hooks (e.g., Pi). Returns "" when the
// agent doesn't support model extraction, the transcript is empty, or no model
// can be determined. Errors are debug-logged because callers treat "" as "no
// model available".
func sessionStateBackfillModel(ctx context.Context, ag agent.Agent, transcript []byte) string {
	me, ok := agent.AsModelExtractor(ag)
	if !ok {
		return ""
	}
	model, err := me.ExtractModel(transcript)
	if err != nil {
		logging.Debug(ctx, "model backfill from transcript failed", slog.String("error", err.Error()))
		return ""
	}
	return model
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

func calculateSessionAttributions(ctx context.Context, repo *git.Repository, shadowRef *plumbing.Reference, sessionData *ExtractedSessionData, state *SessionState, opts ...attributionOpts) *cpkg.Attribution {
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

// committedFilesExcludingMetadata returns committed files with CLI- and
// agent-managed paths filtered out. Files under `.entire/`, `.git/`, agent
// config directories (e.g. `.cursor/`, `.claude/`), and registered protected
// files (e.g. `opencode.json`) are created by `entire enable` or the agent
// integration itself, not by user-prompted work, so they should not appear in
// files_touched when this fallback fires for sessions with no FilesTouched.
func committedFilesExcludingMetadata(committedFiles map[string]struct{}) []string {
	protectedFiles := agent.AllProtectedFiles()
	result := make([]string, 0, len(committedFiles))
	for f := range committedFiles {
		if isProtectedPath(f) {
			continue
		}
		if slices.Contains(protectedFiles, f) {
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

	// Calculate token usage from the checkpoint-scoped transcript portion.
	// Skill events annotate the stored raw transcript, which is full-session, so
	// extract them from offset 0; consumers can filter by checkpoint_transcript_start
	// if they only render the checkpoint-scoped slice.
	if len(data.Transcript) > 0 {
		// subagentsDir="" on purpose. Re-reading the subagent transcripts here would
		// re-parse the whole main transcript plus every subagent file from line 0 —
		// measured at ~29x the cost of this call, enough to triple post-commit
		// condensation for a subagent-heavy session — and would still yield a
		// cumulative snapshot needing the same rescoping SaveStep already did.
		// CondenseSession fills the already-rescoped window total in instead;
		// see withSubagentTokensFrom.
		data.TokenUsage = agent.CalculateTokenUsage(ctx, ag, data.Transcript, checkpointTranscriptStart, "")
		data.SkillEvents = agent.ExtractSkillEvents(ctx, ag, data.Transcript, 0)
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

	// Calculate token usage from the checkpoint-scoped transcript portion.
	// Skill events annotate the stored raw transcript, which is full-session, so
	// extract them from offset 0; consumers can filter by checkpoint_transcript_start
	// if they only render the checkpoint-scoped slice.
	if len(data.Transcript) > 0 {
		// subagentsDir="" for the cost reason in extractSessionData above — but NOT
		// for the cleanup reason: this is the live mid-turn path, where the subagent
		// transcripts are still on disk. It is the one place the gap noted on
		// withSubagentTokensFrom could be closed by reading them.
		data.TokenUsage = agent.CalculateTokenUsage(ctx, ag, data.Transcript, state.CheckpointTranscriptStart, "")
		data.SkillEvents = agent.ExtractSkillEvents(ctx, ag, data.Transcript, 0)
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
	root, err := entiredir.OpenForRead(ctx)
	if err != nil {
		return nil
	}
	data, err := osroot.ReadFile(root, sessionPromptName(sessionID))
	if err != nil || len(data) == 0 {
		return nil
	}
	return splitPromptContent(string(data))
}

// sessionPromptName is a session's prompt.txt relative to the .entire root.
func sessionPromptName(sessionID string) string {
	return entiredir.MustName(paths.SessionMetadataDirFromSessionID(sessionID)) + "/" + paths.PromptFileName
}

// clearFilesystemPrompt removes the filesystem prompt.txt for a session.
// Called after condensation so subsequent checkpoints start fresh.
func clearFilesystemPrompt(ctx context.Context, sessionID string) {
	root, err := entiredir.OpenForRead(ctx)
	if err != nil {
		return
	}
	_ = osroot.Remove(root, sessionPromptName(sessionID)) //nolint:errcheck // best-effort; a leftover prompt.txt is overwritten next turn
}

func ensureCondensationAttemptID(ctx context.Context, state *SessionState) (id.CheckpointID, bool, error) {
	if checkpointID := state.PendingCondensationID(); checkpointID != id.EmptyCheckpointID {
		return checkpointID, false, nil
	}
	checkpointID, err := cpkg.GenerateCheckpointID(ctx)
	if err != nil {
		return id.EmptyCheckpointID, false, fmt.Errorf("generate checkpoint ID: %w", err)
	}
	state.BeginCondensationAttempt(checkpointID)
	return checkpointID, true, nil
}

func hasEagerCondensationContent(state *SessionState) bool {
	return state.StepCount > 0 || state.HasTaskContent()
}

// PrepareSessionEndCondensation reserves an ID for content-bearing ENDED
// sessions or marks empty ENDED sessions fully condensed. File-bearing sessions
// remain eligible for PostCommit.
func PrepareSessionEndCondensation(ctx context.Context, state *SessionState) error {
	if state.Phase != session.PhaseEnded || len(state.FilesTouched) > 0 {
		return nil
	}
	if !hasEagerCondensationContent(state) {
		state.FullyCondensed = true
		state.ClearCondensationAttempt()
		return nil
	}
	_, _, err := ensureCondensationAttemptID(ctx, state)
	return err
}

func reserveDoctorCondensationAttempt(ctx context.Context, state *SessionState) (id.CheckpointID, error) {
	checkpointID, created, err := ensureCondensationAttemptID(ctx, state)
	if err != nil {
		return id.EmptyCheckpointID, err
	}
	if created && state.Phase == session.PhaseEnded && len(state.FilesTouched) == 0 {
		state.RequireCondensationRecovery()
	}
	return checkpointID, nil
}

// CondenseSessionByID condenses a session by its ID and cleans up.
// This is used by "entire doctor" to salvage stuck sessions.
func (s *ManualCommitStrategy) CondenseSessionByID(ctx context.Context, sessionID string) error {
	logCtx := logging.WithComponent(ctx, "condense-by-id")

	repo, err := OpenRepository(ctx)
	if err != nil {
		return fmt.Errorf("failed to open repository: %w", err)
	}
	defer repo.Close()

	var checkpointID id.CheckpointID
	reserveErr := MutateSessionState(ctx, sessionID, func(state *SessionState) error {
		var reserveErr error
		checkpointID, reserveErr = reserveDoctorCondensationAttempt(ctx, state)
		return reserveErr
	})
	if errors.Is(reserveErr, ErrStateNotFound) {
		return fmt.Errorf("session not found: %s", sessionID)
	}
	if reserveErr != nil {
		return fmt.Errorf("failed to reserve checkpoint ID: %w", reserveErr)
	}

	var shadowBranchName string
	var clearAfter bool
	var newSkillEvents []agent.SkillEvent
	mutErr := MutateSessionStateOnSaved(ctx, sessionID, func(state *SessionState) error {
		if state.PendingCondensationID() != checkpointID {
			return ErrMutationSkip
		}

		shadowBranchName = getShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)
		refName := plumbing.NewBranchReferenceName(shadowBranchName)
		_, refErr := repo.Reference(refName, true)
		hasShadowBranch := refErr == nil

		// Record-bearing sessions must materialize their records, not be cleared.
		if !hasShadowBranch && !state.HasTaskContent() {
			logging.Info(logCtx, "no shadow branch for session, clearing state only",
				slog.String("session_id", sessionID),
				slog.String("shadow_branch", shadowBranchName),
			)
			clearAfter = true
			return ErrMutationSkip
		}

		result, err := s.CondenseSession(ctx, repo, checkpointID, state, nil, condenseOpts{reconcileInterrupted: true})
		if err != nil {
			return fmt.Errorf("failed to condense session: %w", err)
		}
		newSkillEvents = result.NewSkillEvents

		if result.Skipped {
			logging.Info(logCtx, "session condensation skipped (no transcript or files), marking fully condensed",
				slog.String("session_id", sessionID),
			)
			state.FullyCondensed = true
			state.ClearCondensationAttempt()
			return nil
		}

		logging.Info(logCtx, "session condensed by ID",
			slog.String("session_id", sessionID),
			slog.String("checkpoint_id", result.CheckpointID.String()),
			slog.Int("checkpoints_condensed", result.CheckpointsCount),
		)

		resetCheckpointWindow(state)
		state.CheckpointTranscriptStart = result.TotalTranscriptLines
		state.CheckpointTranscriptSize = result.TranscriptSizeBaseline
		state.Phase = session.PhaseIdle
		state.LastCheckpointID = result.CheckpointID
		state.LastCheckpointCommitHash = state.BaseCommit
		state.RealignAttributionBase(state.BaseCommit)
		state.PromptAttributions = nil
		state.PendingPromptAttribution = nil
		return nil
	}, func() {
		// Skill telemetry only. commitCondensedEmitter.emit is deliberately NOT
		// called here: its payload is commit-scoped (files_committed counts a
		// commit's files, and prior_ai_history's git-log probe uses --skip=1 to
		// exclude the commit just made). This path condenses without a commit —
		// doctor repairing an uncondensed session — so those fields would be
		// meaningless and --skip=1 would exclude an unrelated HEAD. See
		// newCommitCondensedSignal.
		EmitSkillInvocationTelemetry(ctx, newSkillEvents)
	})
	if errors.Is(mutErr, ErrStateNotFound) {
		return fmt.Errorf("session not found: %s", sessionID)
	}
	if mutErr != nil {
		return mutErr
	}

	if clearAfter {
		if err := s.clearSessionState(ctx, sessionID); err != nil {
			return fmt.Errorf("failed to clear session state: %w", err)
		}
		return nil
	}

	if err := s.cleanupShadowBranchIfUnused(ctx, repo, shadowBranchName, sessionID); err != nil {
		logging.Warn(logCtx, "failed to clean up shadow branch",
			slog.String("shadow_branch", shadowBranchName),
			slog.String("error", err.Error()),
		)
	}
	return nil
}

func prepareEagerCondensation(
	logCtx context.Context,
	repo *git.Repository,
	state *SessionState,
) (shadowBranchName string, shouldCondense bool, err error) {
	// Files waiting for a user commit belong to PostCommit's carry-forward path.
	if len(state.FilesTouched) > 0 {
		return "", false, ErrMutationSkip
	}
	if !hasEagerCondensationContent(state) {
		state.FullyCondensed = true
		state.ClearCondensationAttempt()
		return "", false, nil
	}

	shadowBranchName = getShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)
	refName := plumbing.NewBranchReferenceName(shadowBranchName)
	if _, refErr := repo.Reference(refName, true); refErr != nil && !state.HasTaskContent() {
		logging.Info(logCtx, "eager condense: no shadow branch",
			slog.String("session_id", state.SessionID),
			slog.String("shadow_branch", shadowBranchName),
		)
		state.StepCount = 0
		state.FullyCondensed = true
		state.ClearCondensationAttempt()
		return shadowBranchName, false, nil
	}

	return shadowBranchName, true, nil
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
// Fail-open: if condensation fails, the session remains eligible for a later
// retry with the same reserved checkpoint ID.
func (s *ManualCommitStrategy) CondenseAndMarkFullyCondensed(ctx context.Context, sessionID string) error {
	logCtx := logging.WithComponent(ctx, "checkpoint")

	repo, err := OpenRepository(ctx)
	if err != nil {
		logging.Warn(logCtx, "eager condense: failed to open repository",
			slog.String("session_id", sessionID),
			slog.String("error", err.Error()),
		)
		return nil
	}
	defer repo.Close()

	var shadowBranchName string
	var checkpointID id.CheckpointID
	reservedState, loadErr := s.loadSessionState(ctx, sessionID)
	if loadErr != nil {
		logging.Warn(logCtx, "eager condense: failed to load reserved checkpoint ID",
			slog.String("session_id", sessionID),
			slog.String("error", loadErr.Error()),
		)
		return nil
	}
	if reservedState == nil {
		return nil
	}
	if len(reservedState.FilesTouched) > 0 ||
		(reservedState.FullyCondensed && !hasEagerCondensationContent(reservedState)) {
		return nil
	}
	checkpointID = reservedState.PendingCondensationID()
	shouldCondense := checkpointID != id.EmptyCheckpointID
	if !shouldCondense {
		reserveErr := MutateSessionState(ctx, sessionID, func(state *SessionState) error {
			var preflightErr error
			shadowBranchName, shouldCondense, preflightErr = prepareEagerCondensation(logCtx, repo, state)
			if preflightErr != nil || !shouldCondense {
				return preflightErr
			}
			checkpointID, _, preflightErr = ensureCondensationAttemptID(logCtx, state)
			return preflightErr
		})
		if errors.Is(reserveErr, ErrStateNotFound) || errors.Is(reserveErr, ErrMutationSkip) {
			return nil
		}
		if reserveErr != nil {
			logging.Warn(logCtx, "eager condense: failed to reserve checkpoint ID",
				slog.String("session_id", sessionID),
				slog.String("error", reserveErr.Error()),
			)
			return nil
		}
	}
	if !shouldCondense {
		return nil
	}

	var didCondense bool
	var newSkillEvents []agent.SkillEvent
	mutErr := MutateSessionStateOnSaved(ctx, sessionID, func(state *SessionState) error {
		var preflightErr error
		shadowBranchName, shouldCondense, preflightErr = prepareEagerCondensation(logCtx, repo, state)
		if preflightErr != nil || !shouldCondense {
			return preflightErr
		}

		checkpointID = state.PendingCondensationID()
		if checkpointID == id.EmptyCheckpointID {
			return ErrMutationSkip
		}

		result, condErr := s.CondenseSession(ctx, repo, checkpointID, state, nil, condenseOpts{reconcileInterrupted: true})
		if condErr != nil {
			logging.Warn(logCtx, "eager condense on session stop failed, doctor will retry",
				slog.String("session_id", sessionID),
				slog.String("error", condErr.Error()),
			)
			return ErrMutationSkip // fail-open
		}
		newSkillEvents = result.NewSkillEvents

		if result.Skipped {
			logging.Info(logCtx, "eager condense skipped (no transcript or files), marking fully condensed",
				slog.String("session_id", sessionID),
			)
			state.FullyCondensed = true
			state.ClearCondensationAttempt()
			return nil
		}

		resetCheckpointWindow(state)
		state.CheckpointTranscriptStart = result.TotalTranscriptLines
		state.LastCheckpointID = result.CheckpointID
		state.LastCheckpointCommitHash = state.BaseCommit
		state.RealignAttributionBase(state.BaseCommit)
		state.PromptAttributions = nil
		state.PendingPromptAttribution = nil
		state.FullyCondensed = true
		// Phase stays ENDED — do NOT set to IDLE

		logging.Info(logCtx, "eager condense on session stop succeeded",
			slog.String("session_id", sessionID),
			slog.String("checkpoint_id", result.CheckpointID.String()),
		)
		didCondense = true
		return nil
	}, func() {
		// Skill telemetry only — same reason as CondenseSessionByID: this
		// condenses the work left over after the last commit, so there is no
		// commit for the commit-condensed signal to describe.
		EmitSkillInvocationTelemetry(ctx, newSkillEvents)
	})
	if errors.Is(mutErr, ErrStateNotFound) {
		return nil
	}
	if mutErr != nil {
		return fmt.Errorf("failed to save session state: %w", mutErr)
	}

	if didCondense && shadowBranchName != "" {
		if err := s.cleanupShadowBranchIfUnused(ctx, repo, shadowBranchName, sessionID); err != nil {
			logging.Warn(logCtx, "eager condense: failed to clean up shadow branch",
				slog.String("shadow_branch", shadowBranchName),
				slog.String("error", err.Error()),
			)
		}
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
		// Only SaveStep checkpoints live on the shadow branch; task records do
		// not, so they no longer pin the branch alive.
		if otherShadow == shadowBranchName && state.StepCount > 0 {
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
