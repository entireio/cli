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
	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

// explainExportOptions describes a request for one of the machine-readable
// output modes of `entire checkpoint explain`. Exactly one of the json or
// transcript flags is set when this struct is dispatched.
type explainExportOptions struct {
	sessionFilter  string
	commitRef      string
	checkpointFlag string
	target         string
	json           bool
	transcript     bool
	rawTranscript  bool
	sessionIndex   int
}

// runExplainExport handles --json, --transcript, and --raw-transcript with an
// explicit --session-index. JSON is metadata-only (no transcript bytes
// embedded); transcript bytes always stream to stdout from a flag, never from
// the JSON envelope.
func runExplainExport(ctx context.Context, w, errW io.Writer, opts explainExportOptions) error {
	hasTarget := opts.target != "" || opts.commitRef != "" || opts.checkpointFlag != ""

	if opts.transcript || opts.rawTranscript {
		if !hasTarget {
			return errors.New("--transcript requires a checkpoint ID or commit SHA (positional), --checkpoint/-c, or --commit flag")
		}
		return runExplainStreamTranscript(ctx, w, errW, opts)
	}

	// JSON mode.
	if !hasTarget {
		return runExplainListJSON(ctx, w, opts.sessionFilter)
	}
	return runExplainCheckpointJSON(ctx, w, errW, opts)
}

// resolveExplainCheckpointID resolves a target (positional, --checkpoint, or
// --commit) to a fully-qualified checkpoint ID using the existing lookup
// infrastructure (including remote metadata fetch on miss).
func resolveExplainCheckpointID(ctx context.Context, errW io.Writer, opts explainExportOptions) (id.CheckpointID, *explainCheckpointLookup, error) {
	if opts.commitRef != "" {
		repo, err := openRepository(ctx)
		if err != nil {
			return id.CheckpointID(""), nil, fmt.Errorf("not a git repository: %w", err)
		}
		hash, _, err := resolveCommitUnambiguous(repo, opts.commitRef)
		if err != nil {
			return id.CheckpointID(""), nil, fmt.Errorf("commit not found: %s: %w", opts.commitRef, err)
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
		return cpID, lookup, nil
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

	matches := matchCheckpointPrefix(lookup, prefix)
	if len(matches) == 0 {
		stop := startSpinner(errW, "Fetching checkpoint metadata from remote")
		_, _, v1Err := getMetadataTree(ctx)
		v2OK := false
		if lookup.preferCheckpointsV2 {
			if _, _, v2Err := getV2MetadataTree(ctx); v2Err == nil {
				v2OK = true
			}
		}
		stop(false)
		if v1Err == nil || v2OK {
			if fresh, freshErr := newExplainCheckpointLookup(ctx); freshErr == nil {
				lookup = fresh
				matches = matchCheckpointPrefix(lookup, prefix)
			}
		}
	}

	switch len(matches) {
	case 0:
		return id.CheckpointID(""), lookup, fmt.Errorf("%w: %s", checkpoint.ErrCheckpointNotFound, prefix)
	case 1:
		return matches[0], lookup, nil
	default:
		return id.CheckpointID(""), lookup, fmt.Errorf("%w: %s matches %d checkpoints", errAmbiguousCommitPrefix, prefix, len(matches))
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

// resolveSessionIndex maps the user's --session-index value (or the implicit
// default) onto a valid 0-based offset within summary.Sessions. Returns an
// error when the requested index exceeds the available range.
func resolveSessionIndex(summary *checkpoint.CheckpointSummary, requested int) (int, error) {
	if summary == nil || len(summary.Sessions) == 0 {
		return 0, checkpoint.ErrCheckpointNotFound
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
// session of the resolved checkpoint.
func runExplainStreamTranscript(ctx context.Context, w, errW io.Writer, opts explainExportOptions) error {
	cpID, lookup, err := resolveExplainCheckpointID(ctx, errW, opts)
	if err != nil {
		return err
	}

	reader, summary, err := checkpoint.ResolveCommittedReaderForCheckpoint(ctx, cpID, lookup.v1Store, lookup.v2Store, lookup.preferCheckpointsV2)
	if err != nil {
		return fmt.Errorf("failed to read checkpoint: %w", err)
	}

	idx, err := resolveSessionIndex(summary, opts.sessionIndex)
	if err != nil {
		return err
	}

	if opts.rawTranscript {
		content, readErr := reader.ReadSessionContent(ctx, cpID, idx)
		if readErr != nil {
			return fmt.Errorf("failed to read session content: %w", readErr)
		}
		if len(content.Transcript) == 0 {
			return fmt.Errorf("checkpoint %s session %d has no raw transcript", cpID, idx)
		}
		if _, err := w.Write(content.Transcript); err != nil {
			return fmt.Errorf("failed to write transcript: %w", err)
		}
		return nil
	}

	// Compact transcript path: only v2 stores have a normalized transcript.jsonl.
	v2Reader, ok := reader.(*checkpoint.V2GitStore)
	if !ok {
		return errors.New("compact transcript is only available for v2 checkpoints (try --raw-transcript)")
	}

	bytes, err := v2Reader.ReadSessionCompactTranscript(ctx, cpID, idx)
	if err != nil {
		return fmt.Errorf("failed to read compact transcript: %w", err)
	}
	if _, err := w.Write(bytes); err != nil {
		return fmt.Errorf("failed to write transcript: %w", err)
	}
	return nil
}

// checkpointExportJSON is the metadata-only envelope returned by
// `entire checkpoint explain --json`. It exposes only existing CheckpointSummary
// and CommittedMetadata fields — no schema invention, no transcript bytes.
type checkpointExportJSON struct {
	CheckpointID     string                  `json:"checkpoint_id"`
	Strategy         string                  `json:"strategy,omitempty"`
	Branch           string                  `json:"branch,omitempty"`
	CheckpointsCount int                     `json:"checkpoints_count"`
	FilesTouched     []string                `json:"files_touched,omitempty"`
	HasReview        bool                    `json:"has_review,omitempty"`
	SessionCount     int                     `json:"session_count"`
	Sessions         []checkpointSessionJSON `json:"sessions"`
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

	reader, summary, err := checkpoint.ResolveCommittedReaderForCheckpoint(ctx, cpID, lookup.v1Store, lookup.v2Store, lookup.preferCheckpointsV2)
	if err != nil {
		return fmt.Errorf("failed to read checkpoint: %w", err)
	}

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
	for idx := range summary.Sessions {
		meta, metaErr := readSessionMetadataForExport(ctx, reader, cpID, idx)
		if metaErr != nil {
			// Surface the per-session error as a stub entry rather than failing
			// the whole envelope — partial metadata is still useful to consumers.
			envelope.Sessions = append(envelope.Sessions, checkpointSessionJSON{Index: idx})
			continue
		}
		envelope.Sessions = append(envelope.Sessions, sessionMetadataToJSON(idx, meta))
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(envelope); err != nil {
		return fmt.Errorf("failed to encode checkpoint json: %w", err)
	}
	return nil
}

// readSessionMetadataForExport reads only metadata.json for a session — no
// transcript or prompt bytes. Prefers v2's cheap metadata-only reader when
// available, falls back to the v1 ReadSessionContent path otherwise.
func readSessionMetadataForExport(ctx context.Context, reader checkpoint.CommittedReader, cpID id.CheckpointID, idx int) (*checkpoint.CommittedMetadata, error) {
	if v2, ok := reader.(*checkpoint.V2GitStore); ok {
		meta, err := v2.ReadSessionMetadata(ctx, cpID, idx)
		if err != nil {
			return nil, fmt.Errorf("read v2 session metadata: %w", err)
		}
		return meta, nil
	}
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
// filtered by session ID prefix (mirrors the prose list view).
func runExplainListJSON(ctx context.Context, w io.Writer, sessionFilter string) error {
	repo, err := openRepository(ctx)
	if err != nil {
		return fmt.Errorf("not a git repository: %w", err)
	}

	points, err := getBranchCheckpoints(ctx, repo, branchCheckpointsLimit)
	if err != nil {
		if ctx.Err() != nil {
			return NewSilentError(ctx.Err())
		}
		points = nil
	}

	out := make([]branchCheckpointJSON, 0, len(points))
	for _, p := range points {
		if sessionFilter != "" && !strings.HasPrefix(p.SessionID, sessionFilter) {
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
	return nil
}
