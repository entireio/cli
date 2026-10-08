package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

// checkpointMatchesSessionFilter returns true when any session that
// contributed to this checkpoint has an ID prefixed by sessionFilter.
// Multi-session checkpoints expose archived contributors via SessionIDs;
// matching only against SessionID (the latest contributor) silently drops
// any checkpoint where the requested session was archived.
func checkpointMatchesSessionFilter(p strategy.PendingCheckpoint, sessionFilter string) bool {
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
	// task selects a subagent task record (tool_use_id or agent_id) whose
	// transcript --transcript streams instead of a session's. Cobra-layer
	// validation guarantees it only arrives with transcript set.
	task string
	// listLimit caps the JSON list view at N entries. 0 means use the
	// default (branchCheckpointsLimit). Only consulted in list mode.
	listLimit int
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

	matches, resolvedLookup := matchCheckpointPrefixWithRemoteFallback(ctx, errW, lookup, prefix)
	if resolvedLookup != lookup {
		_ = lookup.Close()
		lookup = resolvedLookup
	}
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
				_ = lookup.Close()
				return cpID, freshLookup, nil
			}
			// Only "the target isn't a commit at all" is a genuine miss that
			// keeps the checkpoint-not-found report below. Everything else —
			// unreadable commit, missing trailer, ambiguous ref, repo or
			// lookup failure — reflects a real failure, not a miss; masking
			// those as not-found is this path's variant of the conflation
			// PR #1812 fixes for the prose path in runExplainAuto (this
			// path's fix: issue #1814).
			if !errors.Is(commitErr, errExportTargetNotCommit) {
				if freshLookup != nil {
					// Defensive: resolveCheckpointFromCommitRef documents a
					// nil lookup on error, but a leak here would be silent.
					_ = freshLookup.Close()
				}
				return id.CheckpointID(""), lookup, commitErr
			}
		}
		return id.CheckpointID(""), lookup, fmt.Errorf("%w: %s", checkpoint.ErrCheckpointNotFound, prefix)
	default:
		ids := make([]string, len(matches))
		for i, m := range matches {
			ids[i] = m.String()
		}
		return id.CheckpointID(""), lookup, fmt.Errorf("%w: %s matches %d checkpoints (%s)", errAmbiguousCommitPrefix, prefix, len(matches), strings.Join(ids, ", "))
	}
}

// errExportTargetNotCommit marks resolveCheckpointFromCommitRef's genuine
// "target does not resolve to any commit" outcome, so
// resolveExplainCheckpointID's positional commit fallback can fall through to
// its checkpoint-not-found report only for that case and surface every other
// failure verbatim.
var errExportTargetNotCommit = errors.New("commit not found")

// resolveCheckpointFromCommitRef opens the repo, resolves a git commit-ish,
// and extracts the Entire-Checkpoint trailer. If the resolved checkpoint
// isn't present in the local committed list, retries once after fetching
// metadata from the remote — symmetry with the prefix path so
// `--commit <sha>` and `--checkpoint <prefix>` share the same fetch
// behavior.
//
// Invariant: on every error return the lookup is nil (any lookup created
// along the way is closed internally), so callers may drop the lookup slot
// without closing it when err != nil.
func resolveCheckpointFromCommitRef(ctx context.Context, errW io.Writer, commitRef string) (id.CheckpointID, *explainCheckpointLookup, error) {
	repo, err := openRepository(ctx)
	if err != nil {
		return id.CheckpointID(""), nil, fmt.Errorf("not a git repository: %w", err)
	}
	defer repo.Close()
	hash, ambiguousMatches, err := resolveCommitUnambiguous(repo, commitRef)
	if err != nil {
		if errors.Is(err, errAmbiguousCommitPrefix) {
			// The target IS commit-like (several commits match); reporting it
			// as not-found would misdirect. Surface the ambiguity, naming the
			// candidates so the user can disambiguate without rerunning git log.
			candidates := make([]string, 0, len(ambiguousMatches))
			for _, m := range buildAmbiguousCommitMatches(repo, ambiguousMatches) {
				candidates = append(candidates, m.ShortID)
			}
			return id.CheckpointID(""), nil, fmt.Errorf("ambiguous commit ref %s (matches commits %s): %w", commitRef, strings.Join(candidates, ", "), err)
		}
		return id.CheckpointID(""), nil, fmt.Errorf("%w: %s: %w", errExportTargetNotCommit, commitRef, err)
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
		matches, fresh := matchCheckpointPrefixWithRemoteFallback(ctx, errW, lookup, cpID.String())
		if fresh != lookup {
			_ = lookup.Close()
			lookup = fresh
		}
		if len(matches) != 1 {
			// The commit resolved and its trailer parsed; the checkpoint is
			// simply not obtainable here. Failing now with the linkage beats
			// succeeding and letting a downstream read die with a bare
			// "checkpoint not found" that misdirects the user toward the
			// checkpoint ID when the problem is availability (offline,
			// unfetchable remote, or genuinely gone).
			_ = lookup.Close()
			return id.CheckpointID(""), nil, fmt.Errorf("commit %s references checkpoint %s, which is not available locally and could not be fetched from the remote", commitRef, cpID)
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

	// git-refs primary: refs-native checkpoints have no single metadata branch to
	// fetch — each is its own ref. When the prefix is a full checkpoint ID (the
	// Entire-Checkpoint commit trailer always is), fetch that one ref directly,
	// then re-list. A shorter prefix cannot be fetched per-ref, so a short-prefix
	// miss stays local-only for refs-native checkpoints.
	//
	// Legacy hex-ID checkpoints on the v1 branch are already covered before we
	// get here, at any prefix length: matchCheckpointPrefix reads lookup.committed,
	// which the store's List populates, and the git-branch read store recovers a
	// missing v1 branch from the checkpoint remote itself (MetadataBranchFetchFunc).
	if cpCfg, _ := settings.LoadCheckpointsConfig(ctx); checkpoint.PrimaryIsRefs(cpCfg) { //nolint:errcheck // fail-soft: bad config surfaces via Open elsewhere
		if cid, err := id.NewCheckpointID(prefix); err != nil {
			logging.Debug(ctx, "explain: prefix is not a full checkpoint ID; refs-primary store cannot fetch by prefix, treating as no match",
				slog.String("prefix", prefix))
		} else {
			// cid is already validated by NewCheckpointID above, so RefName can't
			// error here; the guard is defensive — treat it as a local-only miss
			// rather than fetch a malformed ref.
			refName, refErr := checkpoint.RefName(cid)
			if refErr != nil {
				return nil, lookup
			}
			stop := startSpinner(errW, "Fetching checkpoint from remote")
			fetchErr := FetchCheckpointRef(ctx, refName)
			stop(false)
			if fetchErr == nil {
				fresh, freshErr := newExplainCheckpointLookup(ctx)
				if freshErr == nil {
					if m := matchCheckpointPrefix(fresh, prefix); len(m) > 0 {
						return m, fresh
					}
					_ = fresh.Close()
				} else {
					// The collapse to "no match" below reads to the user as
					// "doesn't exist"; record what actually failed (issue #1815).
					logging.Debug(ctx, "explain: lookup rebuild after checkpoint ref fetch failed; treating as no match",
						slog.String("prefix", prefix),
						slog.String("error", freshErr.Error()))
				}
			} else {
				logging.Debug(ctx, "explain: on-demand checkpoint ref fetch failed; treating as no match",
					slog.String("ref", refName.String()),
					slog.String("error", fetchErr.Error()))
			}
		}
		return nil, lookup
	}

	stop := startSpinner(errW, "Fetching checkpoint metadata from remote")
	_, v1Repo, v1Err := getMetadataTree(ctx)
	if v1Repo != nil {
		_ = v1Repo.Close()
	}
	stop(false)
	if v1Err != nil {
		logging.Debug(ctx, "explain: metadata branch fetch failed; treating as no match",
			slog.String("prefix", prefix),
			slog.String("error", v1Err.Error()))
		return nil, lookup
	}
	fresh, freshErr := newExplainCheckpointLookup(ctx)
	if freshErr != nil {
		logging.Debug(ctx, "explain: lookup rebuild after metadata fetch failed; treating as no match",
			slog.String("prefix", prefix),
			slog.String("error", freshErr.Error()))
		return nil, lookup
	}
	return matchCheckpointPrefix(fresh, prefix), fresh
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

// runExplainStreamTranscript streams the stored transcript for the selected
// session of the resolved checkpoint.
func runExplainStreamTranscript(ctx context.Context, w, errW io.Writer, opts explainExportOptions) error {
	cpID, lookup, err := resolveExplainCheckpointID(ctx, errW, opts)
	if err != nil {
		if lookup != nil {
			_ = lookup.Close()
		}
		return err
	}
	defer lookup.Close()

	store := lookup.store
	if opts.task != "" {
		return streamTaskTranscript(ctx, w, store, cpID, opts.task)
	}
	summary, err := checkpoint.ReadCheckpoint(ctx, store, cpID)
	if err != nil {
		return fmt.Errorf("failed to read checkpoint: %w", err)
	}
	idx, err := resolveSessionIndex(summary, opts.sessionIndex)
	if err != nil {
		return err
	}

	content, readErr := store.ReadSessionContent(ctx, cpID, idx)
	if readErr != nil {
		return fmt.Errorf("failed to read session content: %w", readErr)
	}
	if _, err := w.Write(content.Transcript); err != nil {
		return fmt.Errorf("failed to write transcript: %w", err)
	}
	return nil
}

// streamTaskTranscript writes the stored transcript of the subagent task record
// that selector names (its tool_use_id or agent_id) to w.
//
// An exact tool_use_id is tried first: it names one record even when another
// record's agent_id happens to equal it, and it reads one task.json instead
// of every record's (each a network round trip after a filtered fetch). Only
// when no record has that tool_use_id are the records listed to match it as
// an agent_id.
func streamTaskTranscript(ctx context.Context, w io.Writer, reader checkpoint.TaskReader, cpID id.CheckpointID, selector string) error {
	transcript, err := reader.ReadTaskTranscript(ctx, cpID, selector)
	if errors.Is(err, checkpoint.ErrTaskNotFound) {
		entries, listErr := reader.ListTasks(ctx, cpID)
		if listErr != nil {
			return fmt.Errorf("failed to list subagent tasks for checkpoint %s: %w", cpID, listErr)
		}
		toolUseID, matchErr := matchTaskAgentID(entries, cpID, selector)
		if matchErr != nil {
			return matchErr
		}
		transcript, err = reader.ReadTaskTranscript(ctx, cpID, toolUseID)
		selector = toolUseID
	}
	if err != nil {
		return fmt.Errorf("failed to read subagent transcript for task %s in checkpoint %s: %w", selector, cpID, err)
	}
	if _, err := w.Write(transcript); err != nil {
		return fmt.Errorf("failed to write transcript: %w", err)
	}
	return nil
}

// matchTaskAgentID resolves a --task selector that is no record's tool_use_id
// to the one record whose agent_id it is. An agent_id can name several records
// (a resumed subagent keeps its ID across Task calls), and streaming an
// arbitrary one of them would be wrong, so that is an error.
func matchTaskAgentID(entries []checkpoint.TaskEntry, cpID id.CheckpointID, selector string) (string, error) {
	var matches []string
	for _, entry := range entries {
		if entry.Err == nil && entry.Record.AgentID == selector {
			matches = append(matches, entry.ToolUseID)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		if len(entries) == 0 {
			return "", fmt.Errorf("checkpoint %s has no subagent task records (--task %s)", cpID, selector)
		}
		available := make([]string, 0, len(entries))
		for _, entry := range entries {
			if entry.Err == nil && entry.Record.AgentID != "" {
				available = append(available, fmt.Sprintf("%s (agent %s)", entry.ToolUseID, entry.Record.AgentID))
				continue
			}
			available = append(available, entry.ToolUseID)
		}
		return "", fmt.Errorf("no subagent task %q in checkpoint %s; available: %s", selector, cpID, strings.Join(available, ", "))
	default:
		return "", fmt.Errorf("--task %q is ambiguous in checkpoint %s: it matches %d task records (%s); pass a tool_use_id", selector, cpID, len(matches), strings.Join(matches, ", "))
	}
}

// checkpointExportJSON is the metadata-only envelope returned by
// `entire checkpoint explain --json`. It exposes only existing CheckpointSummary
// and Metadata fields — no schema invention, no transcript bytes.
//
// `partial` is true when any session metadata read or subagent task record
// read failed; the offending entries surface their cause via
// Sessions[].error / Tasks[].error (or tasks_error when the task list itself
// was unreadable). Consumers that don't want to inspect every entry can
// branch on this single top-level flag. The command also exits non-zero in
// that case so automation doesn't mistake incomplete data for a clean export.
//
// `tasks` is always an array (empty when the checkpoint has no subagent task
// records) except when the checkpoint source cannot carry task records at all
// (--repo, read over the Entire API), where the key is omitted.
type checkpointExportJSON struct {
	CheckpointID     string                  `json:"checkpoint_id"`
	Strategy         string                  `json:"strategy,omitempty"`
	Branch           string                  `json:"branch,omitempty"`
	CheckpointsCount int                     `json:"checkpoints_count"`
	FilesTouched     []string                `json:"files_touched,omitempty"`
	HasReview        bool                    `json:"has_review,omitempty"`
	HasInvestigation bool                    `json:"has_investigation,omitempty"`
	SessionCount     int                     `json:"session_count"`
	Sessions         []checkpointSessionJSON `json:"sessions"`
	Tasks            []checkpointTaskJSON    `json:"tasks,omitzero"`
	TasksError       string                  `json:"tasks_error,omitempty"`
	Partial          bool                    `json:"partial,omitempty"`
}

// checkpointTaskJSON is one subagent task record (tasks/<tool_use_id>/) in
// the --json envelope. Metadata only: the transcript streams through
// `--transcript --task <tool_use_id>`, never through the envelope.
type checkpointTaskJSON struct {
	ToolUseID       string            `json:"tool_use_id"`
	AgentID         string            `json:"agent_id,omitempty"`
	SubagentType    string            `json:"subagent_type,omitempty"`
	TaskDescription string            `json:"task_description,omitempty"`
	StartedAt       *time.Time        `json:"started_at,omitempty"`
	CompletedAt     *time.Time        `json:"completed_at,omitempty"`
	Files           []string          `json:"files,omitempty"`
	TokenUsage      *types.TokenUsage `json:"token_usage,omitempty"`
	// TranscriptStored is a pointer so an unreadable record (Error set)
	// carries no field that looks like real data.
	TranscriptStored            *bool  `json:"transcript_stored,omitempty"`
	TranscriptUnavailableReason string `json:"transcript_unavailable_reason,omitempty"`

	// Error is set when this task record could not be read; only ToolUseID
	// is meaningful alongside it.
	Error string `json:"error,omitempty"`
}

// checkpointExportReader is what the single-checkpoint JSON envelope reads:
// per-session metadata plus the subagent task records.
type checkpointExportReader interface {
	checkpoint.SessionReader
	checkpoint.TaskReader
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
	TokenUsage   *types.TokenUsage         `json:"token_usage,omitempty"`
	Summary      *checkpointSessionSummary `json:"summary,omitempty"`

	// Investigation tagging — set only on sessions whose Kind is an
	// investigate kind.
	InvestigateRunID string `json:"investigate_run_id,omitempty"`
	InvestigateTopic string `json:"investigate_topic,omitempty"`

	// Error is set when this session's metadata could not be read. The Index
	// field remains valid; all other content fields are zero. Consumers can
	// detect this by checking for a non-empty Error.
	Error string `json:"error,omitempty"`
}

type checkpointSessionSummary struct {
	Intent    string                      `json:"intent,omitempty"`
	Outcome   string                      `json:"outcome,omitempty"`
	Learnings *checkpointSessionLearnings `json:"learnings,omitempty"`
	Friction  []string                    `json:"friction,omitempty"`
	OpenItems []string                    `json:"open_items,omitempty"`
}

// checkpointSessionLearnings mirrors apicheckpoint.LearningsSummary but marks
// every field omitempty so empty categories drop out of the export instead of
// serializing as empty arrays. CodeLearning is reused as-is — its wire tags
// already omit the zero line/end_line.
type checkpointSessionLearnings struct {
	Repo     []string                  `json:"repo,omitempty"`
	Code     []checkpoint.CodeLearning `json:"code,omitempty"`
	Workflow []string                  `json:"workflow,omitempty"`
}

// runExplainCheckpointJSON resolves a single checkpoint and emits a metadata-only
// JSON envelope. Reads each session's metadata through the committed checkpoint
// reader; never reads any transcript file.
func runExplainCheckpointJSON(ctx context.Context, w, errW io.Writer, opts explainExportOptions) error {
	cpID, lookup, err := resolveExplainCheckpointID(ctx, errW, opts)
	if err != nil {
		if lookup != nil {
			_ = lookup.Close()
		}
		return err
	}
	defer lookup.Close()

	store := lookup.store
	summary, err := checkpoint.ReadCheckpoint(ctx, store, cpID)
	if err != nil {
		return fmt.Errorf("failed to read checkpoint: %w", err)
	}
	envelope, failedSessions := buildCheckpointJSONEnvelope(ctx, store, summary, cpID)

	return writeCheckpointJSONEnvelope(w, errW, cpID, envelope, failedSessions)
}

// writeCheckpointJSONEnvelope encodes the envelope to w and, when it is
// partial, reports what was unreadable on errW and returns a SilentError.
// Shared by the local and --repo paths so both fail the same way.
func writeCheckpointJSONEnvelope(w, errW io.Writer, cpID id.CheckpointID, envelope checkpointExportJSON, failedSessions []int) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(envelope); err != nil {
		return fmt.Errorf("failed to encode checkpoint json: %w", err)
	}
	if !envelope.Partial {
		return nil
	}

	// Fail hard so automation can't mistake incomplete metadata for a clean
	// export. The envelope (with its `partial` flag and per-entry error
	// fields) has already been written to stdout; using SilentError keeps
	// the diagnostic on stderr from interleaving with that output.
	var problems []string
	if len(failedSessions) > 0 {
		fmt.Fprintf(errW, "checkpoint %s: failed to read metadata for %d session(s) (indexes %v)\n",
			cpID, len(failedSessions), failedSessions)
		problems = append(problems, fmt.Sprintf("%d session(s) unreadable", len(failedSessions)))
	}
	if envelope.TasksError != "" {
		fmt.Fprintf(errW, "checkpoint %s: failed to list subagent task records: %s\n", cpID, envelope.TasksError)
		problems = append(problems, "task records unreadable")
	}
	var failedTasks []string
	for _, task := range envelope.Tasks {
		if task.Error != "" {
			failedTasks = append(failedTasks, task.ToolUseID)
		}
	}
	if len(failedTasks) > 0 {
		fmt.Fprintf(errW, "checkpoint %s: failed to read %d subagent task record(s) (%s)\n",
			cpID, len(failedTasks), strings.Join(failedTasks, ", "))
		problems = append(problems, fmt.Sprintf("%d task record(s) unreadable", len(failedTasks)))
	}
	return NewSilentError(fmt.Errorf("checkpoint %s export incomplete: %s", cpID, strings.Join(problems, ", ")))
}

// buildCheckpointJSONEnvelope builds the JSON envelope for a single checkpoint,
// reading each session's metadata and the subagent task records via the
// supplied reader. Returns the envelope plus the list of session indexes that
// failed to read; envelope.Partial is true when that list is non-empty or any
// task record failed to read. Extracted from runExplainCheckpointJSON so the
// envelope-building behavior can be tested independently of git storage.
func buildCheckpointJSONEnvelope(ctx context.Context, reader checkpointExportReader, summary *checkpoint.CheckpointSummary, cpID id.CheckpointID) (checkpointExportJSON, []int) {
	envelope := checkpointExportJSON{
		CheckpointID:     cpID.String(),
		Strategy:         summary.Strategy,
		Branch:           summary.Branch,
		CheckpointsCount: summary.CheckpointsCount,
		FilesTouched:     summary.FilesTouched,
		HasReview:        summary.HasReview,
		HasInvestigation: summary.HasInvestigation,
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
	tasksPartial := addTasksToEnvelope(ctx, reader, cpID, &envelope)
	envelope.Partial = len(failedSessions) > 0 || tasksPartial
	return envelope, failedSessions
}

// addTasksToEnvelope fills envelope.Tasks from the checkpoint's subagent task
// records and reports whether any read failed. A source that cannot carry
// task records leaves Tasks nil, which omits the key rather than claiming
// the checkpoint had no subagents.
func addTasksToEnvelope(ctx context.Context, reader checkpoint.TaskReader, cpID id.CheckpointID, envelope *checkpointExportJSON) bool {
	entries, err := reader.ListTasks(ctx, cpID)
	if errors.Is(err, checkpoint.ErrTaskRecordsUnsupported) {
		return false
	}
	if err != nil {
		envelope.TasksError = err.Error()
		return true
	}
	envelope.Tasks = make([]checkpointTaskJSON, 0, len(entries))
	failed := false
	for _, entry := range entries {
		if entry.Err != nil {
			envelope.Tasks = append(envelope.Tasks, checkpointTaskJSON{ToolUseID: entry.ToolUseID, Error: entry.Err.Error()})
			failed = true
			continue
		}
		envelope.Tasks = append(envelope.Tasks, taskEntryToJSON(entry))
	}
	return failed
}

func taskEntryToJSON(entry checkpoint.TaskEntry) checkpointTaskJSON {
	rec := entry.Record
	stored := entry.TranscriptStored
	out := checkpointTaskJSON{
		ToolUseID:                   entry.ToolUseID,
		AgentID:                     rec.AgentID,
		SubagentType:                rec.SubagentType,
		TaskDescription:             rec.TaskDescription,
		Files:                       rec.Files,
		TokenUsage:                  boundedTokenUsage(rec.TokenUsage),
		TranscriptStored:            &stored,
		TranscriptUnavailableReason: rec.TranscriptUnavailableReason,
	}
	if !rec.StartedAt.IsZero() {
		ts := rec.StartedAt
		out.StartedAt = &ts
	}
	// A zero CompletedAt marks a task still in flight when the checkpoint was
	// written; omitting it keeps that distinguishable from a real time.
	if !rec.CompletedAt.IsZero() {
		ts := rec.CompletedAt
		out.CompletedAt = &ts
	}
	return out
}

// readSessionMetadataForExport reads only metadata.json for a session — no
// transcript or prompt bytes. GitStore exposes a metadata-only reader, so this
// never depends on transcript availability.
func readSessionMetadataForExport(ctx context.Context, reader checkpoint.SessionReader, cpID id.CheckpointID, idx int) (*checkpoint.Metadata, error) {
	meta, err := reader.ReadSessionMetadata(ctx, cpID, idx)
	if err != nil {
		return nil, fmt.Errorf("read session metadata: %w", err)
	}
	return meta, nil
}

func sessionMetadataToJSON(idx int, meta *checkpoint.Metadata) checkpointSessionJSON {
	out := checkpointSessionJSON{
		Index:            idx,
		SessionID:        meta.SessionID,
		Agent:            string(meta.Agent),
		Model:            meta.Model,
		Kind:             meta.Kind,
		ReviewSkills:     meta.ReviewSkills,
		TurnID:           meta.TurnID,
		IsTask:           meta.IsTask,
		ToolUseID:        meta.ToolUseID,
		FilesTouched:     meta.FilesTouched,
		InvestigateRunID: meta.InvestigateRunID,
		InvestigateTopic: meta.InvestigateTopic,
	}
	if !meta.CreatedAt.IsZero() {
		ts := meta.CreatedAt
		out.CreatedAt = &ts
	}
	// The persisted shape, subagent totals and completeness marker included,
	// with the subagent chain bounded (see boundedTokenUsage).
	out.TokenUsage = boundedTokenUsage(meta.TokenUsage)
	if meta.Summary != nil {
		out.Summary = summaryToExportJSON(meta.Summary)
	}
	return out
}

// boundedTokenUsage copies usage with its subagent_tokens chain truncated at
// types.MaxSubagentDepth. The usage comes from pushed metadata.json/task.json
// blobs, and indented JSON grows quadratically with chain depth, so a small
// hostile record could otherwise expand into hundreds of MB of output.
// AddTokenUsage with a nil operand is the existing depth-capped copy.
func boundedTokenUsage(usage *types.TokenUsage) *types.TokenUsage {
	return types.AddTokenUsage(usage, nil)
}

// summaryToExportJSON projects the full persisted summary onto the export
// struct. Friction/open_items/learnings were previously dropped, hiding data
// the prose view already renders. Redaction is applied upstream at persist
// time (RedactSummary), so no additional scrubbing is needed here.
func summaryToExportJSON(s *checkpoint.Summary) *checkpointSessionSummary {
	out := &checkpointSessionSummary{
		Intent:    s.Intent,
		Outcome:   s.Outcome,
		Friction:  s.Friction,
		OpenItems: s.OpenItems,
	}
	if hasAnyLearning(s.Learnings) {
		out.Learnings = &checkpointSessionLearnings{
			Repo:     s.Learnings.Repo,
			Code:     s.Learnings.Code,
			Workflow: s.Learnings.Workflow,
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
// Truncation detection: getBranchCheckpoints reports whether it hit its scan
// budget (the authoritative signal — it applies the cap internally). We also
// hard-cap the flat array at `limit` for the JSON contract, flagging
// truncation if that slice drops anything. The JSON shape stays a flat array
// so jq pipelines don't have to unwrap.
func runExplainListJSON(ctx context.Context, w, errW io.Writer, sessionFilter string, listLimit int) error {
	repo, err := openRepository(ctx)
	if err != nil {
		return fmt.Errorf("not a git repository: %w", err)
	}
	defer repo.Close()

	limit := listLimit
	if limit <= 0 {
		limit = branchCheckpointsLimit
	}

	points, truncated, err := getBranchCheckpoints(ctx, repo, limit)
	if err != nil {
		if ctx.Err() != nil {
			return NewSilentError(ctx.Err())
		}
		// JSON consumers cannot distinguish "[]" from "fetch failed" if we
		// swallow the error. Surface it so scripts get a non-zero exit and
		// a real diagnostic instead of silently degraded output.
		return fmt.Errorf("failed to list checkpoints: %w", err)
	}
	// getBranchCheckpoints budgets the live and imported lists independently,
	// so it can return up to 2*limit entries. Hard-cap the combined array to
	// the requested limit for the JSON contract.
	if len(points) > limit {
		points = points[:limit]
		truncated = true
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
