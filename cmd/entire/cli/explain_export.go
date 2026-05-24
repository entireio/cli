package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

// checkpointMatchesSessionFilter returns true when any session that
// contributed to this checkpoint has an ID prefixed by sessionFilter.
// Multi-session checkpoints expose archived contributors via SessionIDs;
// matching only against SessionID (the latest contributor) silently drops
// any checkpoint where the requested session was archived.
func checkpointMatchesSessionFilter(p strategy.RewindPoint, sessionFilter string) bool {
	if strings.HasPrefix(p.SessionID, sessionFilter) {
		return true
	}
	for _, sid := range p.SessionIDs {
		if strings.HasPrefix(sid, sessionFilter) {
			return true
		}
	}
	return false
}

// explainExportOptions describes a request for one of the machine-readable
// output modes of `entire checkpoint explain`. Exactly one of json,
// transcript, or rawTranscript is set when this struct reaches
// runExplainExport. sessionIndex is meaningful only for transcript /
// rawTranscript requests; cobra-layer validation rejects it elsewhere.
type explainExportOptions struct {
	sessionFilter  string
	commitRef      string
	checkpointFlag string
	target         string
	json           bool
	transcript     bool
	rawTranscript  bool
	sessionIndex   int
	// listLimit caps the JSON list view at N entries. 0 means use the
	// default (branchCheckpointsLimit). Only consulted in list mode.
	listLimit int
}

type compactTranscriptReader interface {
	ReadSessionCompactTranscript(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) ([]byte, error)
}

// runExplainExport handles --json, --transcript, and --raw-transcript with an
// explicit --session-index. JSON is metadata-only (no transcript bytes
// embedded); transcript bytes always stream to stdout from a flag, never from
// the JSON envelope.
func runExplainExport(ctx context.Context, w, errW io.Writer, opts explainExportOptions) error {
	hasTarget := opts.target != "" || opts.commitRef != "" || opts.checkpointFlag != ""

	switch {
	case opts.transcript || opts.rawTranscript:
		if !hasTarget {
			flagName := "--transcript"
			if opts.rawTranscript {
				flagName = "--raw-transcript"
			}
			return fmt.Errorf("%s requires a checkpoint ID or commit SHA (positional), --checkpoint/-c, or --commit flag", flagName)
		}
		return runExplainStreamTranscript(ctx, w, errW, opts)
	case opts.json:
		if !hasTarget {
			return runExplainListJSON(ctx, w, errW, opts.sessionFilter, opts.listLimit)
		}
		return runExplainCheckpointJSON(ctx, w, errW, opts)
	default:
		// The cobra layer guarantees at least one mode flag is set before
		// dispatching here; this branch is a defensive guard against a
		// future caller invoking runExplainExport directly with no mode.
		return errors.New("internal: runExplainExport called without an output mode (json, transcript, or raw-transcript)")
	}
}

// resolveExplainCheckpointID resolves a target to a fully-qualified checkpoint
// ID. Resolution order matches the prose explain command:
//
//  1. --commit <ref>  → resolve as a git commit, read Entire-Checkpoint
//     trailer; remote metadata fetch-on-miss when the trailer points at
//     an unknown checkpoint.
//  2. --checkpoint <id> or positional checkpoint-id-prefix → match against
//     committed checkpoints, with remote metadata fetch-on-miss.
//  3. Positional that fails as a checkpoint prefix falls back to commit-ref
//     resolution (mirrors runExplainAuto).
func resolveExplainCheckpointID(ctx context.Context, errW io.Writer, opts explainExportOptions) (id.CheckpointID, *explainCheckpointLookup, error) {
	if opts.commitRef != "" {
		return resolveCheckpointFromCommitRef(ctx, errW, opts.commitRef)
	}

	prefix := opts.checkpointFlag
	if prefix == "" {
		prefix = opts.target
	}
	if prefix == "" {
		return id.CheckpointID(""), nil, errors.New("missing checkpoint target")
	}

	lookup, lookupErr := newExplainCheckpointLookup(ctx)
	if lookupErr != nil {
		return id.CheckpointID(""), nil, lookupErr
	}

	matches, lookup := matchCheckpointPrefixWithRemoteFallback(ctx, errW, lookup, prefix)
	switch len(matches) {
	case 1:
		return matches[0], lookup, nil
	case 0:
		// If the user passed a positional target (not --checkpoint), give it
		// one more shot as a commit ref before failing — mirrors the prose
		// runExplainAuto behavior so `--json <commit-sha>` works.
		if opts.target != "" && opts.checkpointFlag == "" {
			cpID, freshLookup, commitErr := resolveCheckpointFromCommitRef(ctx, errW, opts.target)
			if commitErr == nil {
				return cpID, freshLookup, nil
			}
		}
		return id.CheckpointID(""), lookup, fmt.Errorf("%w: %s", checkpoint.ErrCheckpointNotFound, prefix)
	default:
		return id.CheckpointID(""), lookup, fmt.Errorf("%w: %s matches %d checkpoints", errAmbiguousCommitPrefix, prefix, len(matches))
	}
}

// resolveCheckpointFromCommitRef opens the repo, resolves a git commit-ish,
// and extracts the Entire-Checkpoint trailer. If the resolved checkpoint
// isn't present in the local committed list, retries once after fetching
// metadata from the remote — symmetry with the prefix path so
// `--commit <sha>` and `--checkpoint <prefix>` share the same fetch
// behavior.
func resolveCheckpointFromCommitRef(ctx context.Context, errW io.Writer, commitRef string) (id.CheckpointID, *explainCheckpointLookup, error) {
	repo, err := openRepository(ctx)
	if err != nil {
		return id.CheckpointID(""), nil, fmt.Errorf("not a git repository: %w", err)
	}
	hash, _, err := resolveCommitUnambiguous(repo, commitRef)
	if err != nil {
		return id.CheckpointID(""), nil, fmt.Errorf("commit not found: %s: %w", commitRef, err)
	}
	commit, err := repo.CommitObject(hash)
	if err != nil {
		return id.CheckpointID(""), nil, fmt.Errorf("failed to read commit: %w", err)
	}
	cpID, found := trailers.ParseCheckpoint(commit.Message)
	if !found {
		return id.CheckpointID(""), nil, fmt.Errorf("commit %s has no Entire-Checkpoint trailer", commit.Hash)
	}
	lookup, lookupErr := newExplainCheckpointLookup(ctx)
	if lookupErr != nil {
		return id.CheckpointID(""), nil, lookupErr
	}

	// If the trailer points at a checkpoint we don't have locally, do the
	// same remote-fetch retry the prefix path uses; otherwise downstream
	// metadata reads would fail with an immediate "not found".
	if !lookupHasCheckpoint(lookup, cpID) {
		if matches, fresh := matchCheckpointPrefixWithRemoteFallback(ctx, errW, lookup, cpID.String()); len(matches) == 1 {
			lookup = fresh
		}
	}
	return cpID, lookup, nil
}

// lookupHasCheckpoint reports whether cpID is in the lookup's local committed
// list. Callers use this before triggering a remote-fetch retry.
func lookupHasCheckpoint(lookup *explainCheckpointLookup, cpID id.CheckpointID) bool {
	for _, info := range lookup.committed {
		if info.CheckpointID == cpID {
			return true
		}
	}
	return false
}

// matchCheckpointPrefixWithRemoteFallback returns all committed checkpoints
// whose ID starts with prefix. On a local miss, fetches metadata from the
// remote (treeless origin → full origin chain) and retries once with a fresh
// lookup. The returned lookup may differ from the input on retry.
func matchCheckpointPrefixWithRemoteFallback(ctx context.Context, errW io.Writer, lookup *explainCheckpointLookup, prefix string) ([]id.CheckpointID, *explainCheckpointLookup) {
	matches := matchCheckpointPrefix(lookup, prefix)
	if len(matches) > 0 {
		return matches, lookup
	}

	stop := startSpinner(errW, "Fetching checkpoint metadata from remote")
	_, _, v1Err := getMetadataTree(ctx)
	v2OK := false
	if shouldFetchV2Metadata(ctx, lookup) {
		if _, _, v2Err := getV2MetadataTree(ctx); v2Err == nil {
			v2OK = true
		}
	}
	stop(false)
	if v1Err != nil && !v2OK {
		return nil, lookup
	}
	fresh, freshErr := newExplainCheckpointLookup(ctx)
	if freshErr != nil {
		return nil, lookup
	}
	return matchCheckpointPrefix(fresh, prefix), fresh
}

func shouldFetchV2Metadata(ctx context.Context, lookup *explainCheckpointLookup) bool {
	if settings.IsCheckpointsV2Enabled(ctx) {
		return true
	}
	if lookup == nil {
		return false
	}
	switch lookup.store.(type) {
	case *checkpoint.V2GitStore, *checkpoint.DualCheckpointReader:
		return true
	default:
		return false
	}
}

func matchCheckpointPrefix(lookup *explainCheckpointLookup, prefix string) []id.CheckpointID {
	var matches []id.CheckpointID
	for _, info := range lookup.committed {
		if strings.HasPrefix(info.CheckpointID.String(), prefix) {
			matches = append(matches, info.CheckpointID)
		}
	}
	return matches
}

// errCheckpointHasNoSessions distinguishes "this checkpoint has zero sessions"
// from checkpoint.ErrCheckpointNotFound — callers using errors.Is on the
// not-found sentinel were getting wrong-fault UX (e.g. "did you mistype the
// ID?") for a legitimate empty-checkpoint edge case.
var errCheckpointHasNoSessions = errors.New("checkpoint has no sessions")

// resolveSessionIndex maps the user's --session-index value (or the implicit
// default) onto a valid 0-based offset within summary.Sessions.
func resolveSessionIndex(summary *checkpoint.CheckpointSummary, requested int) (int, error) {
	if summary == nil {
		return 0, checkpoint.ErrCheckpointNotFound
	}
	if len(summary.Sessions) == 0 {
		return 0, errCheckpointHasNoSessions
	}
	if requested < 0 {
		return len(summary.Sessions) - 1, nil
	}
	if requested >= len(summary.Sessions) {
		return 0, fmt.Errorf("session index %d out of range (checkpoint has %d sessions)", requested, len(summary.Sessions))
	}
	return requested, nil
}

// runExplainStreamTranscript streams either the compact transcript (default)
// or the raw transcript (when --raw-transcript is set) for the selected
// session of the resolved checkpoint. When --transcript is used on a
// v1-only checkpoint (compact transcripts are a v2-only artifact), falls
// through to the raw transcript with a one-line stderr note rather than
// erroring — the consumer's stated intent is "give me transcript bytes",
// and we have a way to satisfy it without making them re-run.
func runExplainStreamTranscript(ctx context.Context, w, errW io.Writer, opts explainExportOptions) error {
	cpID, lookup, err := resolveExplainCheckpointID(ctx, errW, opts)
	if err != nil {
		return err
	}

	store := lookup.store
	summary, err := checkpoint.ReadCommittedCheckpoint(ctx, store, cpID)
	if err != nil {
		return fmt.Errorf("failed to read checkpoint: %w", err)
	}

	compactReader, hasCompact := store.(compactTranscriptReader)
	wantCompact := !opts.rawTranscript

	idx, err := resolveSessionIndex(summary, opts.sessionIndex)
	if err != nil {
		return err
	}

	// Compact transcripts are only stored on v2; transparently fall through
	// to raw on v1 so consumers don't need to retry.
	if wantCompact && !hasCompact {
		fmt.Fprintln(errW, "note: compact transcript unavailable on v1 checkpoint, falling back to raw transcript")
		wantCompact = false
	}

	if !wantCompact {
		content, readErr := store.ReadSessionContent(ctx, cpID, idx)
		if readErr != nil {
			return fmt.Errorf("failed to read session content: %w", readErr)
		}
		if _, err := w.Write(content.Transcript); err != nil {
			return fmt.Errorf("failed to write transcript: %w", err)
		}
		return nil
	}

	compact, err := compactReader.ReadSessionCompactTranscript(ctx, cpID, idx)
	if err != nil {
		if errors.Is(err, checkpoint.ErrCheckpointNotFound) || errors.Is(err, checkpoint.ErrNoTranscript) {
			fmt.Fprintln(errW, "note: compact transcript unavailable, falling back to raw transcript")
			content, readErr := store.ReadSessionContent(ctx, cpID, idx)
			if readErr != nil {
				return fmt.Errorf("failed to read session content: %w", readErr)
			}
			if _, writeErr := w.Write(content.Transcript); writeErr != nil {
				return fmt.Errorf("failed to write transcript: %w", writeErr)
			}
			return nil
		}
		return fmt.Errorf("failed to read compact transcript: %w", err)
	}
	if _, err := w.Write(compact); err != nil {
		return fmt.Errorf("failed to write transcript: %w", err)
	}
	return nil
}

// checkpointExportJSON is the metadata-only envelope returned by
// `entire checkpoint explain --json`. It exposes only existing CheckpointSummary
// and CommittedMetadata fields — no schema invention, no transcript bytes.
//
// `partial` is true when any session metadata read failed; the offending
// entries surface their cause via Sessions[].error. Consumers that don't
// want to inspect every entry can branch on this single top-level flag.
// The command also exits non-zero in that case so automation doesn't
// mistake incomplete data for a clean export.
type checkpointExportJSON struct {
	CheckpointID     string                  `json:"checkpoint_id"`
	Strategy         string                  `json:"strategy,omitempty"`
	Branch           string                  `json:"branch,omitempty"`
	CheckpointsCount int                     `json:"checkpoints_count"`
	FilesTouched     []string                `json:"files_touched,omitempty"`
	HasReview        bool                    `json:"has_review,omitempty"`
	SessionCount     int                     `json:"session_count"`
	Sessions         []checkpointSessionJSON `json:"sessions"`
	Partial          bool                    `json:"partial,omitempty"`
}

type checkpointSessionJSON struct {
	Index        int                       `json:"index"`
	SessionID    string                    `json:"session_id,omitempty"`
	Agent        string                    `json:"agent,omitempty"`
	Model        string                    `json:"model,omitempty"`
	Kind         string                    `json:"kind,omitempty"`
	ReviewSkills []string                  `json:"review_skills,omitempty"`
	CreatedAt    *time.Time                `json:"created_at,omitempty"`
	TurnID       string                    `json:"turn_id,omitempty"`
	IsTask       bool                      `json:"is_task,omitempty"`
	ToolUseID    string                    `json:"tool_use_id,omitempty"`
	FilesTouched []string                  `json:"files_touched,omitempty"`
	TokenUsage   *checkpointSessionTokens  `json:"token_usage,omitempty"`
	Summary      *checkpointSessionSummary `json:"summary,omitempty"`

	// Error is set when this session's metadata could not be read. The Index
	// field remains valid; all other content fields are zero. Consumers can
	// detect this by checking for a non-empty Error.
	Error string `json:"error,omitempty"`
}

type checkpointSessionTokens struct {
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	CacheReadTokens     int `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens int `json:"cache_creation_tokens,omitempty"`
}

type checkpointSessionSummary struct {
	Intent  string `json:"intent,omitempty"`
	Outcome string `json:"outcome,omitempty"`
}

// runExplainCheckpointJSON resolves a single checkpoint and emits a metadata-only
// JSON envelope. Reads each session's metadata.json from /main; never reads any
// transcript file.
func runExplainCheckpointJSON(ctx context.Context, w, errW io.Writer, opts explainExportOptions) error {
	cpID, lookup, err := resolveExplainCheckpointID(ctx, errW, opts)
	if err != nil {
		return err
	}

	store := lookup.store
	summary, err := checkpoint.ReadCommittedCheckpoint(ctx, store, cpID)
	if err != nil {
		return fmt.Errorf("failed to read checkpoint: %w", err)
	}

	envelope, failedSessions := buildCheckpointJSONEnvelope(ctx, store, summary, cpID)

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(envelope); err != nil {
		return fmt.Errorf("failed to encode checkpoint json: %w", err)
	}

	// Fail hard so automation can't mistake incomplete metadata for a clean
	// export. The envelope (with its `partial` flag and per-session error
	// fields) has already been written to stdout; using SilentError keeps
	// the diagnostic on stderr from interleaving with that output.
	if envelope.Partial {
		fmt.Fprintf(errW, "checkpoint %s: failed to read metadata for %d session(s) (indexes %v)\n",
			cpID, len(failedSessions), failedSessions)
		return NewSilentError(fmt.Errorf("checkpoint %s export incomplete: %d session(s) unreadable", cpID, len(failedSessions)))
	}
	return nil
}

// buildCheckpointJSONEnvelope builds the JSON envelope for a single checkpoint,
// reading each session's metadata via the supplied reader. Returns the envelope
// plus the list of session indexes that failed to read; a non-empty failed
// list means envelope.Partial is true. Extracted from runExplainCheckpointJSON
// so the envelope-building behavior (per-session error fields, partial flag)
// can be tested independently of the v2 git tree, which the cli package
// can't easily corrupt.
func buildCheckpointJSONEnvelope(ctx context.Context, reader checkpoint.CommittedReader, summary *checkpoint.CheckpointSummary, cpID id.CheckpointID) (checkpointExportJSON, []int) {
	envelope := checkpointExportJSON{
		CheckpointID:     cpID.String(),
		Strategy:         summary.Strategy,
		Branch:           summary.Branch,
		CheckpointsCount: summary.CheckpointsCount,
		FilesTouched:     summary.FilesTouched,
		HasReview:        summary.HasReview,
		SessionCount:     len(summary.Sessions),
	}

	envelope.Sessions = make([]checkpointSessionJSON, 0, len(summary.Sessions))
	var failedSessions []int
	for idx := range summary.Sessions {
		meta, metaErr := readSessionMetadataForExport(ctx, reader, cpID, idx)
		if metaErr != nil {
			// Surface the per-session error as a stub entry with an explicit
			// error string rather than failing the whole envelope or silently
			// returning empty fields. Consumers branch on the `error` field
			// and the top-level `partial` flag.
			envelope.Sessions = append(envelope.Sessions, checkpointSessionJSON{
				Index: idx,
				Error: metaErr.Error(),
			})
			failedSessions = append(failedSessions, idx)
			continue
		}
		envelope.Sessions = append(envelope.Sessions, sessionMetadataToJSON(idx, meta))
	}
	envelope.Partial = len(failedSessions) > 0
	return envelope, failedSessions
}

// readSessionMetadataForExport reads only metadata.json for a session — no
// transcript or prompt bytes. Both v1 and v2 stores expose a metadata-only
// reader, so this never depends on transcript availability (which would
// cause an unrelated ErrNoTranscript on v1 checkpoints whose raw transcript
// has been pruned).
func readSessionMetadataForExport(ctx context.Context, reader checkpoint.CommittedReader, cpID id.CheckpointID, idx int) (*checkpoint.CommittedMetadata, error) {
	if r, ok := reader.(interface {
		ReadSessionMetadata(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (*checkpoint.CommittedMetadata, error)
	}); ok {
		meta, err := r.ReadSessionMetadata(ctx, cpID, idx)
		if err != nil {
			return nil, fmt.Errorf("read session metadata: %w", err)
		}
		return meta, nil
	}
	// CommittedReader doesn't promise a metadata-only method; fall back
	// to the heavier ReadSessionContent path. Reachable only if a third
	// store implementation is added without exposing metadata reads.
	content, err := reader.ReadSessionContent(ctx, cpID, idx)
	if err != nil {
		return nil, fmt.Errorf("read session content: %w", err)
	}
	meta := content.Metadata
	return &meta, nil
}

func sessionMetadataToJSON(idx int, meta *checkpoint.CommittedMetadata) checkpointSessionJSON {
	out := checkpointSessionJSON{
		Index:        idx,
		SessionID:    meta.SessionID,
		Agent:        string(meta.Agent),
		Model:        meta.Model,
		Kind:         meta.Kind,
		ReviewSkills: meta.ReviewSkills,
		TurnID:       meta.TurnID,
		IsTask:       meta.IsTask,
		ToolUseID:    meta.ToolUseID,
		FilesTouched: meta.FilesTouched,
	}
	if !meta.CreatedAt.IsZero() {
		ts := meta.CreatedAt
		out.CreatedAt = &ts
	}
	if meta.TokenUsage != nil {
		out.TokenUsage = &checkpointSessionTokens{
			InputTokens:         meta.TokenUsage.InputTokens,
			OutputTokens:        meta.TokenUsage.OutputTokens,
			CacheReadTokens:     meta.TokenUsage.CacheReadTokens,
			CacheCreationTokens: meta.TokenUsage.CacheCreationTokens,
		}
	}
	if meta.Summary != nil {
		out.Summary = &checkpointSessionSummary{
			Intent:  meta.Summary.Intent,
			Outcome: meta.Summary.Outcome,
		}
	}
	return out
}

// branchCheckpointJSON is one entry in the list emitted by
// `entire checkpoint explain --json` (no target).
type branchCheckpointJSON struct {
	CheckpointID     string    `json:"checkpoint_id"`
	SessionID        string    `json:"session_id,omitempty"`
	Agent            string    `json:"agent,omitempty"`
	Date             time.Time `json:"date"`
	Message          string    `json:"message,omitempty"`
	IsTaskCheckpoint bool      `json:"is_task_checkpoint,omitempty"`
	IsLogsOnly       bool      `json:"is_logs_only,omitempty"`
	SessionCount     int       `json:"session_count,omitempty"`
	SessionIDs       []string  `json:"session_ids,omitempty"`
}

// runExplainListJSON emits a JSON array of branch checkpoints, optionally
// filtered by session ID prefix (mirrors the prose list view). The cap
// defaults to branchCheckpointsLimit; pass listLimit > 0 to override.
//
// Truncation detection: we ask the underlying lister for one more than the
// effective cap. If we got that many back, we know there were at least
// `cap` checkpoints we didn't return — emit a stderr note so the consumer
// knows to set --limit higher. The JSON shape stays a flat array so jq
// pipelines don't have to unwrap.
func runExplainListJSON(ctx context.Context, w, errW io.Writer, sessionFilter string, listLimit int) error {
	repo, err := openRepository(ctx)
	if err != nil {
		return fmt.Errorf("not a git repository: %w", err)
	}

	limit := listLimit
	if limit <= 0 {
		limit = branchCheckpointsLimit
	}

	// Probe one extra so we can detect truncation.
	points, err := getBranchCheckpoints(ctx, repo, limit+1)
	if err != nil {
		if ctx.Err() != nil {
			return NewSilentError(ctx.Err())
		}
		// JSON consumers cannot distinguish "[]" from "fetch failed" if we
		// swallow the error. Surface it so scripts get a non-zero exit and
		// a real diagnostic instead of silently degraded output.
		return fmt.Errorf("failed to list checkpoints: %w", err)
	}
	truncated := len(points) > limit
	if truncated {
		points = points[:limit]
	}

	out := make([]branchCheckpointJSON, 0, len(points))
	for _, p := range points {
		if sessionFilter != "" && !checkpointMatchesSessionFilter(p, sessionFilter) {
			continue
		}
		entry := branchCheckpointJSON{
			SessionID:        p.SessionID,
			Agent:            string(p.Agent),
			Date:             p.Date,
			Message:          p.Message,
			IsTaskCheckpoint: p.IsTaskCheckpoint,
			IsLogsOnly:       p.IsLogsOnly,
			SessionCount:     p.SessionCount,
			SessionIDs:       p.SessionIDs,
		}
		if !p.CheckpointID.IsEmpty() {
			entry.CheckpointID = p.CheckpointID.String()
		} else {
			entry.CheckpointID = p.ID
		}
		out = append(out, entry)
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("failed to encode checkpoint list: %w", err)
	}

	if truncated {
		fmt.Fprintf(errW, "note: list capped at %d checkpoints; rerun with --limit <N> to see more\n", limit)
	}
	return nil
}
