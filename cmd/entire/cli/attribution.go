package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/stringutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/spf13/cobra"
)

type attributionAuthorship string

const (
	attributionAI          attributionAuthorship = "ai"
	attributionHuman       attributionAuthorship = "human"
	attributionMixed       attributionAuthorship = "mixed"
	attributionUncommitted attributionAuthorship = "uncommitted"
)

type attributionLineRange struct {
	Start int
	End   int
}

type rawBlameLine struct {
	LineNumber int
	CommitSHA  string
	Author     string
	AuthorTime *time.Time
	Content    string
}

type attributionLine struct {
	LineNumber            int                   `json:"line_number"`
	Authorship            attributionAuthorship `json:"authorship"`
	Tag                   string                `json:"tag"`
	CommitSHA             string                `json:"commit_sha,omitempty"`
	ShortCommitSHA        string                `json:"short_commit_sha,omitempty"`
	Author                string                `json:"author,omitempty"`
	AuthorTime            *time.Time            `json:"author_time,omitempty"`
	CheckpointID          string                `json:"checkpoint_id,omitempty"`
	SessionID             string                `json:"session_id,omitempty"`
	Agent                 string                `json:"agent,omitempty"`
	Model                 string                `json:"model,omitempty"`
	Prompt                string                `json:"prompt,omitempty"`
	Intent                string                `json:"intent,omitempty"`
	MetadataMissing       bool                  `json:"metadata_missing,omitempty"`
	MetadataMissingReason string                `json:"metadata_missing_reason,omitempty"`
	SessionFallback       bool                  `json:"session_fallback,omitempty"`
	// PromptSessionLevel is set when Prompt is the session's overall/seed prompt
	// (e.g. an attach/trail ReviewPrompt) rather than a prompt recorded for this
	// specific checkpoint. `why` labels these differently and points at
	// `checkpoint explain`, since the prompt may not appear in this checkpoint's
	// own transcript slice.
	PromptSessionLevel bool                   `json:"prompt_session_level,omitempty"`
	Content            string                 `json:"content"`
	Candidates         []attributionCandidate `json:"candidates,omitempty"`
}

// attributionCheckpointContext is the resolved metadata for one checkpoint as
// it applies to a file: the agent/session that produced the file's lines plus
// the prompt and intent behind them. The same shape is used two ways — as a
// per-line candidate (one line may map to several checkpoints) and as the
// deduplicated per-file checkpoint map — so attributionCandidate aliases it
// rather than duplicating the fields.
type attributionCheckpointContext struct {
	CheckpointID          string   `json:"checkpoint_id"`
	SessionID             string   `json:"session_id,omitempty"`
	Agent                 string   `json:"agent,omitempty"`
	Model                 string   `json:"model,omitempty"`
	Prompt                string   `json:"prompt,omitempty"`
	Intent                string   `json:"intent,omitempty"`
	FilesTouched          []string `json:"files_touched,omitempty"`
	MetadataMissing       bool     `json:"metadata_missing,omitempty"`
	MetadataMissingReason string   `json:"metadata_missing_reason,omitempty"`
	Mixed                 bool     `json:"mixed,omitempty"`
	// SessionFallback is set when the file is not in any resolved session's
	// recorded paths (e.g. it was renamed after the checkpoint) and the
	// agent/prompt shown is a best-effort guess from the checkpoint's first
	// session rather than the session that actually touched this file.
	SessionFallback bool `json:"session_fallback,omitempty"`
	// PromptSessionLevel is set when Prompt is the session's overall/seed prompt
	// (ReviewPrompt) rather than a prompt recorded for this checkpoint.
	PromptSessionLevel bool `json:"prompt_session_level,omitempty"`
}

type attributionCandidate = attributionCheckpointContext

type fileAttributionResult struct {
	File        string                                  `json:"file"`
	Lines       []attributionLine                       `json:"lines"`
	Checkpoints map[string]attributionCheckpointContext `json:"checkpoints,omitempty"`
	Summary     attributionSummary                      `json:"summary"`
}

type attributionSummary struct {
	TotalLines       int `json:"total_lines"`
	AILines          int `json:"ai_lines"`
	HumanLines       int `json:"human_lines"`
	MixedLines       int `json:"mixed_lines"`
	UncommittedLines int `json:"uncommitted_lines"`
	AIPercentage     int `json:"ai_percentage"`
	HumanPercentage  int `json:"human_percentage"`
	MixedPercentage  int `json:"mixed_percentage"`
}

type attributionCheckpointReader interface {
	Read(ctx context.Context, checkpointID id.CheckpointID) (*checkpoint.CheckpointSummary, error)
	ReadSessionMetadataAndPrompts(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (*checkpoint.Metadata, string, error)
}

type attributionResolver struct {
	ctx         context.Context
	repo        *git.Repository
	store       attributionCheckpointReader
	fetchOnMiss bool

	commitCache     map[string]*object.Commit
	checkpointCache map[string]attributionCheckpointContext

	// Memoized remote metadata refresh. The refresh runs AT MOST ONCE per
	// resolver, no matter how many checkpoints miss: fetchAttributionMetadata
	// durably fetches entire/checkpoints/v1, so one refresh serves every
	// subsequent read, and a failure is not retried per checkpoint. This also
	// makes a single run internally consistent: every miss in the run sees the
	// same refresh outcome. Note the bound covers the REFRESH path only — the
	// underlying store is opened with an on-demand BlobFetcher (pre-existing
	// behavior, shared with blame), which may still attempt per-blob fetches
	// for individually missing objects during reads.
	refreshAttempted bool
	refreshErr       error
	freshRepo        *git.Repository // owned by the resolver; closed in Close
	storeGeneration  int             // bumped when a refresh swaps r.store

	// Memoized backend topology (see primaryBackendIsRefs).
	refsPrimaryKnown bool
	refsPrimary      bool
}

func newBlameCmd() *cobra.Command {
	var lineFlag string
	var jsonFlag bool
	var longFlag bool

	cmd := &cobra.Command{
		Use: "blame <file>[:line[-line]]",
		// Hidden from `entire help` while the feature is still maturing —
		// advertised under `entire labs`, and `entire blame` / `entire blame
		// --help` keep working normally.
		Hidden: true,
		Short:  "Show which lines came from Entire checkpoints",
		Long:   "Show git-blame-style line attribution enriched with Entire checkpoint metadata.\n\nLimit to a line or range with <file>:12, <file>:12-20, or the --line flag.",
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAttributionBlame(cmd.Context(), cmd.OutOrStdout(), args[0], attributionBlameOptions{
				LineFlag: lineFlag,
				JSON:     jsonFlag,
				Long:     longFlag,
			})
		},
	}

	cmd.Flags().StringVar(&lineFlag, "line", "", "Only show a line or range, for example 12 or 12-20")
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "Output attribution as JSON")
	cmd.Flags().BoolVar(&longFlag, "long", false, "Show the full attribution table with agent, model, author, and session columns")
	return cmd
}

func newWhyCmd() *cobra.Command {
	var jsonFlag bool
	var lineFlag string

	cmd := &cobra.Command{
		Use: "why <file>[:line]",
		// Hidden from `entire help` while the feature is still maturing —
		// advertised under `entire labs`, and `entire why` / `entire why
		// --help` keep working normally.
		Hidden: true,
		Short:  "Show why a line exists",
		Long:   "Explain the commit, checkpoint, prompt, and session behind a file or line.\n\nTarget a specific line with <file>:12 or the --line flag.",
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAttributionWhy(cmd.Context(), cmd.OutOrStdout(), args[0], attributionWhyOptions{
				LineFlag: lineFlag,
				JSON:     jsonFlag,
			})
		},
	}

	cmd.Flags().StringVar(&lineFlag, "line", "", "Explain a specific line, for example 12 (same as <file>:12)")
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "Output explanation as JSON")
	return cmd
}

type attributionBlameOptions struct {
	LineFlag string
	JSON     bool
	Long     bool
}

type attributionWhyOptions struct {
	LineFlag string
	JSON     bool
}

func runAttributionBlame(ctx context.Context, w io.Writer, file string, opts attributionBlameOptions) error {
	if f, spec := splitFileLineSpec(file); spec != "" {
		if opts.LineFlag != "" {
			return fmt.Errorf("specify the line with <file>:%s or --line %s, not both", spec, opts.LineFlag)
		}
		opts.LineFlag = spec
		file = f
	}

	var lineRange *attributionLineRange
	if opts.LineFlag != "" {
		parsed, err := parseAttributionLineRange(opts.LineFlag)
		if err != nil {
			return err
		}
		lineRange = parsed
	}

	result, err := resolveFileAttribution(ctx, file, false)
	if err != nil {
		return err
	}
	if lineRange != nil {
		result.Lines = filterAttributionLines(result.Lines, *lineRange)
		result.Summary = summarizeAttributionLines(result.Lines)
		result.Checkpoints = checkpointContextsForLines(result.Lines, result.Checkpoints)
	}

	if opts.JSON {
		return writeJSON(w, result)
	}
	renderAttributionBlame(w, result, opts.LineFlag, opts.Long)
	return nil
}

func runAttributionWhy(ctx context.Context, w io.Writer, target string, opts attributionWhyOptions) error {
	file, line, hasLine, err := parseAttributionWhyTarget(target)
	if err != nil {
		return err
	}
	if opts.LineFlag != "" {
		if hasLine {
			return errors.New("specify the line with <file>:line or --line, not both")
		}
		n, lineErr := parseSingleAttributionLine(opts.LineFlag)
		if lineErr != nil {
			return lineErr
		}
		line, hasLine = n, true
	}

	// entire why is explanation-focused: when local metadata is missing it
	// should attempt the same remote enrichment path as checkpoint explain.
	result, err := resolveFileAttribution(ctx, file, true)
	if err != nil {
		return err
	}

	if !hasLine {
		if opts.JSON {
			return writeJSON(w, result)
		}
		renderAttributionFileWhy(w, result)
		return nil
	}

	var selected *attributionLine
	for i := range result.Lines {
		if result.Lines[i].LineNumber == line {
			selected = &result.Lines[i]
			break
		}
	}
	if selected == nil {
		return fmt.Errorf("line %d is outside %s", line, result.File)
	}

	if opts.JSON {
		payload := struct {
			File        string                                  `json:"file"`
			Line        attributionLine                         `json:"line"`
			Checkpoints map[string]attributionCheckpointContext `json:"checkpoints,omitempty"`
		}{
			File:        result.File,
			Line:        *selected,
			Checkpoints: checkpointContextsForLines([]attributionLine{*selected}, result.Checkpoints),
		}
		return writeJSON(w, payload)
	}
	renderAttributionLineWhy(w, result.File, *selected)
	return nil
}

func resolveFileAttribution(ctx context.Context, file string, fetchOnMiss bool) (*fileAttributionResult, error) {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return nil, errors.New("not a git repository")
	}
	relFile, err := normalizeAttributionPath(repoRoot, file)
	if err != nil {
		return nil, err
	}

	rawLines, err := runGitBlame(ctx, repoRoot, relFile)
	if err != nil {
		return nil, err
	}

	resolver, err := newAttributionResolver(ctx, fetchOnMiss)
	if err != nil {
		return nil, err
	}
	defer resolver.Close()

	result := &fileAttributionResult{
		File:        relFile,
		Lines:       make([]attributionLine, 0, len(rawLines)),
		Checkpoints: make(map[string]attributionCheckpointContext),
	}
	for _, raw := range rawLines {
		line := resolver.resolveLine(raw, relFile)
		result.Lines = append(result.Lines, line)
		for _, candidate := range line.Candidates {
			if candidate.MetadataMissing {
				result.Checkpoints[candidate.CheckpointID] = attributionCheckpointContext{
					CheckpointID:          candidate.CheckpointID,
					MetadataMissing:       true,
					MetadataMissingReason: candidate.MetadataMissingReason,
				}
				continue
			}
			if checkpointCtx, ok := resolver.checkpointCache[candidate.CheckpointID]; ok {
				result.Checkpoints[candidate.CheckpointID] = checkpointCtx
			}
		}
	}
	result.Summary = summarizeAttributionLines(result.Lines)
	return result, nil
}

func newAttributionResolver(ctx context.Context, fetchOnMiss bool) (*attributionResolver, error) {
	repo, err := openRepository(ctx)
	if err != nil {
		return nil, fmt.Errorf("not a git repository: %w", err)
	}

	stores, err := checkpoint.Open(ctx, repo, checkpoint.OpenOptions{BlobFetcher: FetchBlobsByHash, RefFetcher: FetchCheckpointRef})
	if err != nil {
		return nil, fmt.Errorf("open checkpoint store: %w", err)
	}

	return &attributionResolver{
		ctx:             ctx,
		repo:            repo,
		store:           stores.Persistent,
		fetchOnMiss:     fetchOnMiss,
		commitCache:     make(map[string]*object.Commit),
		checkpointCache: make(map[string]attributionCheckpointContext),
	}, nil
}

func (r *attributionResolver) Close() {
	if r == nil {
		return
	}
	if r.repo != nil {
		_ = r.repo.Close()
	}
	if r.freshRepo != nil {
		_ = r.freshRepo.Close()
	}
}

func (r *attributionResolver) resolveLine(raw rawBlameLine, file string) attributionLine {
	line := attributionLine{
		LineNumber: raw.LineNumber,
		CommitSHA:  raw.CommitSHA,
		Author:     raw.Author,
		AuthorTime: raw.AuthorTime,
		Content:    raw.Content,
	}
	if raw.CommitSHA != "" && !isZeroCommit(raw.CommitSHA) {
		line.ShortCommitSHA = shortSHA(raw.CommitSHA)
	}

	if isZeroCommit(raw.CommitSHA) {
		line.Authorship = attributionUncommitted
		line.Tag = attributionTag(line.Authorship)
		return line
	}

	commit, err := r.commit(raw.CommitSHA)
	if err != nil {
		line.Authorship = attributionHuman
		line.Tag = attributionTag(line.Authorship)
		return line
	}

	cpIDs := trailers.ParseAllCheckpoints(commit.Message)
	if len(cpIDs) == 0 {
		line.Authorship = attributionHuman
		line.Tag = attributionTag(line.Authorship)
		return line
	}

	var candidates []attributionCandidate
	for _, cpID := range cpIDs {
		candidates = append(candidates, r.checkpointContext(cpID, file))
	}

	preferred := preferredAttributionCandidate(candidates, file)
	applyPreferredToLine(&line, preferred)
	line.Authorship = authorshipForPreferred(preferred)
	if len(candidates) > 0 {
		line.Candidates = candidates
	}

	line.Tag = attributionTag(line.Authorship)
	return line
}

func (r *attributionResolver) commit(sha string) (*object.Commit, error) {
	if commit, ok := r.commitCache[sha]; ok {
		return commit, nil
	}
	commit, err := r.repo.CommitObject(plumbing.NewHash(sha))
	if err != nil {
		return nil, err //nolint:wrapcheck // caller treats as missing attribution
	}
	r.commitCache[sha] = commit
	return commit, nil
}

func (r *attributionResolver) checkpointContext(cpID id.CheckpointID, file string) attributionCheckpointContext {
	key := cpID.String()
	if ctx, ok := r.checkpointCache[key]; ok {
		return ctx
	}

	ctx := r.readCheckpointContext(cpID, file)
	r.checkpointCache[key] = ctx
	return ctx
}

// localCheckpointRead is the outcome of one pass over the resolver's current
// store: the assembled context plus the diagnostics that decide whether a
// remote refresh could improve it and what reason to surface if not.
type localCheckpointRead struct {
	ctx           attributionCheckpointContext
	summaryErr    error
	sessionsTotal int
	sessionsRead  int
	sessionErr    error // first session read error, as the representative cause
}

// incomplete reports whether a remote refresh could plausibly improve this
// read: the checkpoint summary was unreadable, or the summary listed sessions
// whose per-session records (metadata/prompt blobs) were partially or wholly
// unreadable — the treeless-clone / gc-pruned case, where the blame tree is
// present but blobs are not.
func (l localCheckpointRead) incomplete() bool {
	return l.summaryErr != nil || (l.sessionsTotal > 0 && l.sessionsRead < l.sessionsTotal)
}

// worseThan reports whether this (post-refresh) read lost information relative
// to prev. A checkpoint that exists only locally (author hasn't pushed) can
// legitimately read worse from the refreshed store; in that case the local
// read wins.
func (l localCheckpointRead) worseThan(prev localCheckpointRead) bool {
	if l.summaryErr != nil {
		return prev.summaryErr == nil
	}
	if prev.summaryErr != nil {
		return false
	}
	return l.sessionsRead < prev.sessionsRead
}

// readCheckpointContext assembles the checkpoint context for a file, refreshing
// the metadata branch from the remote (once per resolver) when the local read
// is incomplete and fetchOnMiss is set. Every clone of the same remote thereby
// converges on the same answer, and when it cannot, MetadataMissingReason says
// deterministically why and how to recover.
func (r *attributionResolver) readCheckpointContext(cpID id.CheckpointID, file string) attributionCheckpointContext {
	logCtx := logging.WithComponent(r.ctx, "attribution")
	read := r.readCheckpointContextOnce(cpID, file)
	remoteLacksCheckpoint := false
	var retryReadErr error
	if read.incomplete() {
		logging.Debug(logCtx, "checkpoint metadata incomplete locally",
			slog.String("checkpoint_id", cpID.String()),
			slog.Bool("summary_missing", read.summaryErr != nil),
			slog.Int("sessions_total", read.sessionsTotal),
			slog.Int("sessions_read", read.sessionsRead),
			slog.Bool("fetch_on_miss", r.fetchOnMiss))
		// Retry only when the refresh actually changed the store during THIS
		// call: a later checkpoint whose first pass already ran against the
		// refreshed store gains nothing from an identical second pass.
		genBefore := r.storeGeneration
		if r.fetchOnMiss && r.refreshMetadataStore() == nil && r.storeGeneration != genBefore {
			retry := r.readCheckpointContextOnce(cpID, file)
			if retry.incomplete() {
				logging.Debug(logCtx, "checkpoint metadata still incomplete after refresh",
					slog.String("checkpoint_id", cpID.String()),
					slog.Bool("summary_missing", retry.summaryErr != nil),
					slog.Int("sessions_read", retry.sessionsRead),
					slog.Int("sessions_total", retry.sessionsTotal))
			}
			if retry.worseThan(read) {
				// Keep the local data, but don't discard what the retry
				// proved: a refreshed store that reports not-found means the
				// remote's metadata branch lacks this checkpoint entirely
				// (an unpushed local-only checkpoint) — the reason must say
				// that instead of speculating about missing session records.
				// A NON-absence retry failure proves nothing about the
				// remote; carry its error so the reason stays neutral.
				remoteLacksCheckpoint = errors.Is(retry.summaryErr, checkpoint.ErrCheckpointNotFound)
				if !remoteLacksCheckpoint {
					retryReadErr = retry.summaryErr
				}
			} else {
				read = retry
			}
		}
	}

	if read.summaryErr != nil {
		read.ctx.MetadataMissing = true
		read.ctx.MetadataMissingReason = r.missReason(cpID.String(),
			"checkpoint metadata was not found locally", read.summaryErr, true, false, nil)
		return read.ctx
	}
	if read.ctx.MetadataMissing {
		// Sessions were listed but none could be read (per-session blobs
		// absent — partial clone or pruned objects), so agent/model/prompt are
		// unavailable even though the checkpoint itself resolves.
		read.ctx.MetadataMissingReason = r.missReason(cpID.String(),
			fmt.Sprintf("checkpoint found, but none of its %d session record(s) could be read locally", read.sessionsTotal),
			read.sessionErr, false, remoteLacksCheckpoint, retryReadErr)
	}
	return read.ctx
}

// readCheckpointContextOnce is a single pass over the resolver's current
// store. It never triggers the metadata-branch refresh — that policy lives in
// readCheckpointContext — though the store itself may attempt on-demand
// per-blob fetches for individually missing objects (its BlobFetcher,
// pre-existing behavior shared with blame). MetadataMissingReason is left
// empty — the caller attaches it with refresh-outcome context.
func (r *attributionResolver) readCheckpointContextOnce(cpID id.CheckpointID, file string) localCheckpointRead {
	read := localCheckpointRead{ctx: attributionCheckpointContext{CheckpointID: cpID.String()}}
	summary, err := readAttributionCheckpointSummary(r.ctx, r.store, cpID)
	if err != nil {
		read.summaryErr = err
		return read
	}
	ctx := &read.ctx

	ctx.FilesTouched = normalizePathSlice(summary.FilesTouched)
	read.sessionsTotal = len(summary.Sessions)

	selected := checkpointSessionForFile{}
	var fallback checkpointSessionForFile
	matchedFile := false
	for i := range summary.Sessions {
		sessionCtx, readErr := r.readSessionForCheckpoint(cpID, i)
		if readErr != nil {
			if read.sessionErr == nil {
				read.sessionErr = readErr
			}
			continue
		}
		read.sessionsRead++
		if fallback.SessionID == "" {
			fallback = sessionCtx
		}
		if selected.SessionID == "" && pathsContainFile(sessionCtx.FilesTouched, file) {
			selected = sessionCtx
			matchedFile = true
		}
	}

	if selected.SessionID == "" {
		selected = fallback
	}

	// We resolved a session, but the file is in none of the resolved sessions'
	// recorded paths (e.g. it was renamed after the checkpoint) — so the agent
	// and prompt shown are a best-effort guess rather than the session that
	// actually produced this line. Flag the approximation in either case:
	//
	//   - sessionsRead > 1: a multi-session checkpoint where none matched, so the
	//     chosen fallback is one of several sessions with no path evidence — still
	//     a guess even when that session's own FilesTouched is empty.
	//   - len(selected.FilesTouched) > 0: the chosen session recorded paths that
	//     exclude this file (rename evidence), including the single-session case
	//     where there is no other session to compare against.
	//
	// Still suppress the single-session + empty-FilesTouched case: empty paths
	// mean "unknown", which is not evidence of a rename, so flagging it would
	// print a misleading caveat (common for older metadata and attach/trail
	// sessions that don't populate FilesTouched).
	if selected.SessionID != "" && !matchedFile && (read.sessionsRead > 1 || len(selected.FilesTouched) > 0) {
		ctx.SessionFallback = true
	}

	// Sessions existed but none could be read: the per-session detail (agent,
	// model, prompt) is unavailable even though the checkpoint commit exists.
	// Mark it missing so callers show the "trailer-level only" hint; the
	// caller decides whether a remote refresh should be attempted.
	if read.sessionsTotal > 0 && read.sessionsRead == 0 {
		ctx.MetadataMissing = true
	}

	// Mixed authorship is scoped to the session whose work actually touched
	// this file, not the checkpoint as a whole. A checkpoint that edited one
	// file with the agent and another by hand is "combined" overall, but a
	// line from the agent-only file is still purely [AI]. Fall back to the
	// checkpoint-wide attribution only when no session metadata resolved.
	switch {
	case selected.Attribution != nil:
		ctx.Mixed = attributionIsMixed(selected.Attribution)
	case selected.SessionID == "":
		ctx.Mixed = attributionIsMixed(summary.CombinedAttribution)
	}

	ctx.SessionID = selected.SessionID
	ctx.Agent = selected.Agent
	ctx.Model = selected.Model
	ctx.Prompt = selected.Prompt
	ctx.PromptSessionLevel = selected.PromptSessionLevel
	ctx.Intent = selected.Intent
	if len(selected.FilesTouched) > 0 {
		ctx.FilesTouched = selected.FilesTouched
	}
	if len(ctx.FilesTouched) == 0 {
		ctx.FilesTouched = normalizePathSlice(summary.FilesTouched)
	}
	return read
}

func readAttributionCheckpointSummary(ctx context.Context, reader attributionCheckpointReader, cpID id.CheckpointID) (*checkpoint.CheckpointSummary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err //nolint:wrapcheck // Propagating context cancellation
	}
	summary, err := reader.Read(ctx, cpID)
	if err != nil {
		return nil, fmt.Errorf("read committed checkpoint: %w", err)
	}
	if summary == nil {
		return nil, checkpoint.ErrCheckpointNotFound
	}
	return summary, nil
}

// fetchAttributionMetadata refreshes the local entire/checkpoints/v1 metadata
// from the remote. It returns nil ONLY when a remote fetch genuinely succeeded:
// the missing-reason wording relies on this to distinguish "the remote lacks
// the checkpoint" from "we never reached the remote", so it deliberately does
// NOT use getMetadataTree, whose local-branch fallback reports success when
// every actual fetch failed (offline, no remote, auth). The fetch is durable —
// it updates local refs/packfiles — so one call serves every read that follows.
// FetchMetadataBranch is the full-content variant: attribution needs the
// per-session blobs, so the tree-only probe used elsewhere is not enough.
// Injectable for tests.
var fetchAttributionMetadata = func(ctx context.Context) error {
	// A configured checkpoint remote is the source of truth for checkpoint
	// data: do not mask its failure with an origin fallback that may hold a
	// stale copy (or no metadata branch at all) — a later miss would then
	// claim "not pushed" about a remote we never reached.
	// Resolve the authority exactly once, then fetch the URL that was
	// verified. Independently re-resolving inside the fetch helper (which
	// re-reads settings and git remotes) opened a TOCTOU window where a
	// transient divergence could route the fetch to origin while attribution
	// recorded an authoritative checkpoint-remote refresh. A settings-load
	// failure is likewise a refresh failure — not "no checkpoint remote",
	// which would silently promote origin to the evidence source.
	s, err := settings.Load(ctx)
	if err != nil {
		return fmt.Errorf("checkpoint remote fetch failed: read settings: %w", err)
	}
	if s.GetCheckpointRemote() != nil {
		// remote.FetchURLWithSource reports whether the URL was genuinely
		// derived from the configured checkpoint_remote. A "successful" fetch
		// that actually hit origin would fake the evidence the not-pushed
		// wording relies on — require the explicit source signal, never a
		// URL-string comparison (token rewrites change origin's shape, and a
		// checkpoint_remote pointing at the origin repo derives an identical
		// URL legitimately).
		url, usedCheckpointRemote, err := remote.FetchURLWithSource(ctx)
		if err != nil {
			return fmt.Errorf("checkpoint remote fetch failed: %w", err)
		}
		if !usedCheckpointRemote {
			return errors.New("checkpoint remote fetch failed: checkpoint_remote is configured but its URL could not be derived (resolution fell back to origin)")
		}
		if err := strategy.FetchMetadataBranch(ctx, url); err != nil {
			return fmt.Errorf("checkpoint remote fetch failed: %w", err)
		}
		return nil
	}
	if err := FetchMetadataBranch(ctx); err != nil {
		return fmt.Errorf("origin fetch failed: %w", err)
	}
	return nil
}

// openAttributionStore opens a fresh checkpoint store over a freshly-opened
// repo. After a fetch, the resolver's original repo handle can't see the new
// packfiles (go-git's storer caches), so post-refresh reads need a new handle.
// The BlobFetcher AND RefFetcher are deliberately kept identical to the initial
// store (newAttributionResolver): after a genuine refresh they heal individual
// straggler blobs / per-checkpoint refs the branch fetch missed — dropping
// either would make the retry read strictly worse than the first pass on a
// git-refs-backend repo. Injectable for tests.
var openAttributionStore = func(ctx context.Context) (attributionCheckpointReader, *git.Repository, error) {
	repo, err := openRepository(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("reopen repository after metadata refresh: %w", err)
	}
	stores, err := checkpoint.Open(ctx, repo, checkpoint.OpenOptions{BlobFetcher: FetchBlobsByHash, RefFetcher: FetchCheckpointRef})
	if err != nil {
		_ = repo.Close()
		return nil, nil, fmt.Errorf("reopen checkpoint store after metadata refresh: %w", err)
	}
	return stores.Persistent, repo, nil
}

// primaryBackendIsRefs reports (memoized) whether the repo's primary checkpoint
// backend is the git-refs per-checkpoint store. The v1-branch refresh proves
// nothing about per-checkpoint refs, so the strong "the remote's
// entire/checkpoints/v1 does not contain it / not on the remote" evidence is
// only sound for the git-branch backend; refs-backend misses use neutral
// wording. A config load error counts as the default git-branch backend.
func (r *attributionResolver) primaryBackendIsRefs() bool {
	if r.refsPrimaryKnown {
		return r.refsPrimary
	}
	r.refsPrimaryKnown = true
	cfg, err := settings.LoadCheckpointsConfig(r.ctx)
	if err != nil {
		return false
	}
	r.refsPrimary = checkpoint.PrimaryIsRefs(cfg)
	return r.refsPrimary
}

// refreshMetadataStore fetches the metadata branch from the remote AT MOST ONCE
// per resolver and, on success, re-points the resolver's store at a fresh repo
// handle. Both the outcome and the fresh store are memoized, so a file whose
// lines miss on many checkpoints pays one refresh (or one failure), not one per
// checkpoint — and every miss in the run sees the same outcome.
func (r *attributionResolver) refreshMetadataStore() error {
	if r.refreshAttempted {
		return r.refreshErr
	}
	r.refreshAttempted = true
	logCtx := logging.WithComponent(r.ctx, "attribution")

	if err := fetchAttributionMetadata(r.ctx); err != nil {
		r.refreshErr = err
		logging.Debug(logCtx, "checkpoint metadata remote refresh failed",
			slog.String("error", err.Error()))
		return r.refreshErr
	}
	store, repo, err := openAttributionStore(r.ctx)
	if err != nil {
		r.refreshErr = err
		logging.Debug(logCtx, "checkpoint store reopen after refresh failed",
			slog.String("error", err.Error()))
		return r.refreshErr
	}
	r.freshRepo = repo
	r.store = store
	r.storeGeneration++
	logging.Debug(logCtx, "checkpoint metadata refreshed from remote")
	return nil
}

// missReason builds the deterministic, actionable MetadataMissingReason. The
// message distinguishes the recovery situations the issue reporters couldn't
// tell apart: no fetch was attempted (run it yourself), the fetch failed
// (here's the error), and the fetch succeeded but the data is still absent
// (fetching again won't help). For that last case the wording is only as
// strong as the evidence: "not pushed" requires the refreshed store to report
// not-found (a transient read error proves nothing about the remote), and
// remoteLacksCheckpoint carries the same proof for the session-records case
// (an unpushed local-only checkpoint). The explain hint is only useful for the
// whole-checkpoint miss; the reason is collapsed to one line because git fetch
// errors embed multi-line output. Renderers print it verbatim.
func (r *attributionResolver) missReason(checkpointID, base string, cause error, summaryMissing, remoteLacksCheckpoint bool, retryReadErr error) string {
	reason := base
	if cause != nil {
		reason = fmt.Sprintf("%s (%v)", base, cause)
	}

	switch {
	case r.refreshAttempted && r.refreshErr != nil:
		reason = fmt.Sprintf("%s (remote refresh failed: %v)", reason, r.refreshErr)
	case r.refreshAttempted && summaryMissing && errors.Is(cause, checkpoint.ErrCheckpointNotFound) && r.primaryBackendIsRefs():
		// git-refs primary: the v1-branch refresh proves nothing about
		// per-checkpoint refs, so make no claim about what the remote holds.
		return stringutil.CollapseWhitespace(reason + ". The metadata branch was refreshed, but this repo's primary checkpoint backend stores per-checkpoint refs, which that refresh may not cover — the checkpoint may not have been pushed yet, or its ref could not be fetched; attribution stays trailer-level.")
	case r.refreshAttempted && summaryMissing && errors.Is(cause, checkpoint.ErrCheckpointNotFound):
		// Refresh succeeded and the refreshed store reports not-found: the
		// remote's metadata branch genuinely doesn't have it.
		return stringutil.CollapseWhitespace(reason + ". The remote's entire/checkpoints/v1 does not contain it — the checkpoint may not have been pushed yet (its author can run: git push <remote> entire/checkpoints/v1), or it predates checkpointing.")
	case r.refreshAttempted && summaryMissing:
		// Refresh succeeded but the summary read failed for a reason other
		// than absence (storage error, cancellation): make no claim about
		// what the remote contains. The cause is already embedded above; no
		// log pointer — this path only emits debug-level entries.
		return stringutil.CollapseWhitespace(reason + ". The metadata branch was refreshed from the remote, but reading the checkpoint still failed.")
	case r.refreshAttempted && retryReadErr != nil:
		// Refresh succeeded but RE-reading this checkpoint from the refreshed
		// store failed for a non-absence reason: the refreshed store was never
		// successfully consulted, so make no remote claim — surface the retry
		// error instead.
		return stringutil.CollapseWhitespace(fmt.Sprintf("%s. The metadata branch was refreshed from the remote, but re-reading the checkpoint from the refreshed store failed (%v); attribution stays trailer-level.", reason, retryReadErr))
	case r.refreshAttempted && remoteLacksCheckpoint && !r.primaryBackendIsRefs():
		// The summary read locally, but a successful refresh proved the
		// remote lacks the checkpoint entirely: this is an unpushed
		// local-only checkpoint with damaged local session records.
		return stringutil.CollapseWhitespace(reason + ". This checkpoint is not on the remote's entire/checkpoints/v1 (it may not have been pushed yet), and the local session records are unreadable; attribution stays trailer-level.")
	case r.refreshAttempted && r.primaryBackendIsRefs():
		// git-refs primary: no speculation about the v1 branch's contents.
		return stringutil.CollapseWhitespace(reason + ". They are still unavailable after refreshing; attribution stays trailer-level.")
	case r.refreshAttempted:
		// The checkpoint resolved but its session records are still unreadable
		// after a successful refresh: the metadata branch was pushed without
		// them or the objects are unavailable — do not claim the checkpoint
		// itself is missing, and don't suggest the fetch that just ran.
		return stringutil.CollapseWhitespace(reason + ". They are still unavailable after refreshing from the remote — the metadata branch may have been pushed without them; attribution stays trailer-level.")
	}

	suggestion, isCommand := suggestCheckpointFetchCommand(r.ctx)
	if isCommand {
		suggestion = "Run: " + suggestion
	}
	if checkpointID == "" || !summaryMissing {
		return stringutil.CollapseWhitespace(fmt.Sprintf("%s. %s.", reason, suggestion))
	}
	return stringutil.CollapseWhitespace(fmt.Sprintf("%s. %s. Then re-run entire checkpoint explain %s.", reason, suggestion, checkpointID))
}

type checkpointSessionForFile struct {
	SessionID          string
	Agent              string
	Model              string
	Prompt             string
	PromptSessionLevel bool
	Intent             string
	FilesTouched       []string
	Attribution        *checkpoint.Attribution
}

func (r *attributionResolver) readSessionForCheckpoint(cpID id.CheckpointID, index int) (checkpointSessionForFile, error) {
	meta, prompts, err := r.store.ReadSessionMetadataAndPrompts(r.ctx, cpID, index)
	if err != nil {
		return checkpointSessionForFile{}, err //nolint:wrapcheck // caller skips partial metadata
	}
	intent := ""
	if meta.Summary != nil {
		intent = strings.TrimSpace(meta.Summary.Intent)
	}
	// prompt.txt holds session-wide prompts (extracted from the transcript at
	// offset 0; `checkpoint explain` re-derives a checkpoint-scoped prompt from
	// the transcript slice and only falls back to these). So the prompt shown
	// here is session-level — not necessarily this checkpoint's — whenever this
	// is a later checkpoint (transcript start > 0). Flag that so `why` labels it
	// "Session prompt:" and points at `checkpoint explain` instead of implying it
	// is scoped to this checkpoint. The first checkpoint (start 0) is exact.
	prompt := strings.TrimSpace(prompts)
	sessionLevel := false
	switch {
	case prompt != "":
		sessionLevel = meta.GetTranscriptStart() > 0
	case strings.TrimSpace(meta.ReviewPrompt) != "":
		// Empty prompt.txt (e.g. an attach/trail session) → the seed ReviewPrompt,
		// which is always a session-level value, not this checkpoint's prompt.
		prompt = strings.TrimSpace(meta.ReviewPrompt)
		sessionLevel = true
	default:
		prompt = intent
	}
	return checkpointSessionForFile{
		SessionID:          meta.SessionID,
		Agent:              string(meta.Agent),
		Model:              meta.Model,
		Prompt:             prompt,
		PromptSessionLevel: sessionLevel,
		Intent:             intent,
		FilesTouched:       normalizePathSlice(meta.FilesTouched),
		Attribution:        meta.Attribution,
	}, nil
}

func runGitBlame(ctx context.Context, repoRoot, file string) ([]rawBlameLine, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "blame", "--line-porcelain", "--", file)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return nil, fmt.Errorf("git blame --line-porcelain %s: %w (stderr: %s)", file, err, msg)
		}
		return nil, fmt.Errorf("git blame --line-porcelain %s: %w", file, err)
	}
	return parseBlamePorcelain(string(out))
}

var blameHeaderRe = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})\s+\d+\s+(\d+)(?:\s+\d+)?$`)

func parseBlamePorcelain(output string) ([]rawBlameLine, error) {
	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	var current *rawBlameLine
	var lines []rawBlameLine
	for scanner.Scan() {
		line := scanner.Text()
		if match := blameHeaderRe.FindStringSubmatch(line); match != nil {
			lineNumber, err := strconv.Atoi(match[2])
			if err != nil {
				return nil, fmt.Errorf("parse blame line number %q: %w", match[2], err)
			}
			current = &rawBlameLine{CommitSHA: match[1], LineNumber: lineNumber}
			continue
		}
		if current == nil {
			continue
		}
		switch {
		case strings.HasPrefix(line, "author "):
			current.Author = strings.TrimPrefix(line, "author ")
		case strings.HasPrefix(line, "author-time "):
			seconds, err := strconv.ParseInt(strings.TrimPrefix(line, "author-time "), 10, 64)
			if err == nil {
				t := time.Unix(seconds, 0).UTC()
				current.AuthorTime = &t
			}
		case strings.HasPrefix(line, "\t"):
			current.Content = strings.TrimPrefix(line, "\t")
			lines = append(lines, *current)
			current = nil
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan git blame output: %w", err)
	}
	return lines, nil
}

func parseAttributionLineRange(input string) (*attributionLineRange, error) {
	parts := strings.Split(input, "-")
	if len(parts) > 2 || parts[0] == "" {
		return nil, fmt.Errorf("invalid line range %q: use N or N-M", input)
	}
	start, err := strconv.Atoi(parts[0])
	if err != nil || start < 1 {
		return nil, fmt.Errorf("invalid line range %q: start must be a positive integer", input)
	}
	end := start
	if len(parts) == 2 {
		if parts[1] == "" {
			return nil, fmt.Errorf("invalid line range %q: end must be a positive integer", input)
		}
		end, err = strconv.Atoi(parts[1])
		if err != nil || end < 1 {
			return nil, fmt.Errorf("invalid line range %q: end must be a positive integer", input)
		}
	}
	if end < start {
		return nil, fmt.Errorf("invalid line range %q: end must be >= start", input)
	}
	return &attributionLineRange{Start: start, End: end}, nil
}

// splitFileLineSpec splits a positional argument of the form "<file>", "<file>:N"
// or "<file>:N-M" into the file path and the trailing line spec ("" when there is
// none). It only treats the suffix after the last colon as a line spec when it
// looks like a line or range (digits, optionally "-digits"), so file names that
// merely contain a colon are left intact. A Windows volume name (e.g. "C:") in
// the first path component is never treated as a line spec.
func splitFileLineSpec(arg string) (file string, spec string) {
	colon := strings.LastIndex(arg, ":")
	if colon == -1 || colon == len(arg)-1 {
		return arg, ""
	}
	if volume := filepath.VolumeName(arg); volume != "" && colon < len(volume) {
		return arg, ""
	}
	candidate := arg[colon+1:]
	if !attributionLineSpecRe.MatchString(candidate) {
		return arg, ""
	}
	return arg[:colon], candidate
}

// attributionLineSpecRe matches a single line ("12") or a range ("12-20").
var attributionLineSpecRe = regexp.MustCompile(`^\d+(-\d+)?$`)

// parseSingleAttributionLine parses a single positive line number for `why`,
// rejecting ranges (which only `blame` supports) with an actionable message.
func parseSingleAttributionLine(input string) (int, error) {
	input = strings.TrimSpace(input)
	if strings.Contains(input, "-") {
		return 0, fmt.Errorf("invalid line %q: why explains a single line; use entire blame for a range", input)
	}
	n, err := strconv.Atoi(input)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("invalid line %q: must be a positive integer", input)
	}
	return n, nil
}

// parseAttributionWhyTarget splits a `why` positional argument into a file and
// an optional single line. It shares splitFileLineSpec with `blame`, so a
// colon-then-non-numeric suffix is treated as part of the filename (not an
// error) and ranges get a friendly pointer at `blame`. When no line spec is
// present the caller may still supply one via --line.
func parseAttributionWhyTarget(input string) (file string, line int, hasLine bool, err error) {
	f, spec := splitFileLineSpec(input)
	if spec == "" {
		return input, 0, false, nil
	}
	n, parseErr := parseSingleAttributionLine(spec)
	if parseErr != nil {
		return "", 0, false, parseErr
	}
	return f, n, true, nil
}

func normalizeAttributionPath(repoRoot, file string) (string, error) {
	path := file
	if !filepath.IsAbs(path) {
		abs, err := filepath.Abs(path)
		if err != nil {
			return "", fmt.Errorf("resolve path %s: %w", file, err)
		}
		path = abs
	}
	canonicalRepoRoot := repoRoot
	if resolved, err := filepath.EvalSymlinks(repoRoot); err == nil {
		canonicalRepoRoot = resolved
	}
	canonicalPath := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		canonicalPath = resolved
	}
	rel, err := filepath.Rel(canonicalRepoRoot, canonicalPath)
	if err != nil {
		return "", fmt.Errorf("resolve path %s relative to repository: %w", file, err)
	}
	if rel == "." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || rel == ".." {
		return "", fmt.Errorf("%s is outside the repository", file)
	}
	return filepath.ToSlash(rel), nil
}

func filterAttributionLines(lines []attributionLine, lineRange attributionLineRange) []attributionLine {
	filtered := make([]attributionLine, 0, len(lines))
	for _, line := range lines {
		if line.LineNumber >= lineRange.Start && line.LineNumber <= lineRange.End {
			filtered = append(filtered, line)
		}
	}
	return filtered
}

func checkpointContextsForLines(lines []attributionLine, contexts map[string]attributionCheckpointContext) map[string]attributionCheckpointContext {
	if len(lines) == 0 || len(contexts) == 0 {
		return nil
	}
	pruned := make(map[string]attributionCheckpointContext)
	for _, line := range lines {
		for _, candidate := range line.Candidates {
			if ctx, ok := contexts[candidate.CheckpointID]; ok {
				pruned[candidate.CheckpointID] = ctx
			}
		}
		if line.CheckpointID != "" {
			if ctx, ok := contexts[line.CheckpointID]; ok {
				pruned[line.CheckpointID] = ctx
			}
		}
	}
	if len(pruned) == 0 {
		return nil
	}
	return pruned
}

func summarizeAttributionLines(lines []attributionLine) attributionSummary {
	var summary attributionSummary
	summary.TotalLines = len(lines)
	for _, line := range lines {
		switch line.Authorship {
		case attributionAI:
			summary.AILines++
		case attributionHuman:
			summary.HumanLines++
		case attributionMixed:
			summary.MixedLines++
		case attributionUncommitted:
			summary.UncommittedLines++
		}
	}
	// Apportion percentages with the largest-remainder method across all four
	// buckets so the displayed AI/Human/Mixed figures don't drift (e.g. three
	// equal thirds rendering as 33/33/33 = 99). Uncommitted shares the 100% but
	// is shown only as a count, so when it is present the three visible
	// percentages correctly total less than 100.
	pct := largestRemainderPercent(
		[]int{summary.AILines, summary.HumanLines, summary.MixedLines, summary.UncommittedLines},
		summary.TotalLines,
	)
	summary.AIPercentage = pct[0]
	summary.HumanPercentage = pct[1]
	summary.MixedPercentage = pct[2]
	return summary
}

// largestRemainderPercent apportions integer percentages that sum to 100 across
// counts whose own sum is total, using the largest-remainder (Hamilton) method.
// It avoids the truncation drift where independently floored shares total 99.
// Returns all-zero when total is non-positive.
func largestRemainderPercent(counts []int, total int) []int {
	pct := make([]int, len(counts))
	if total <= 0 {
		return pct
	}
	allocated := 0
	order := make([]int, len(counts))
	for i, c := range counts {
		pct[i] = c * 100 / total
		allocated += pct[i]
		order[i] = i
	}
	leftover := 100 - allocated
	if leftover <= 0 {
		return pct
	}
	// Hand the leftover points to the largest fractional remainders, breaking
	// ties by lower index for deterministic output.
	remainder := func(i int) int { return (counts[i] * 100) % total }
	sort.SliceStable(order, func(a, b int) bool {
		ra, rb := remainder(order[a]), remainder(order[b])
		if ra == rb {
			return order[a] < order[b]
		}
		return ra > rb
	})
	for i := 0; i < leftover && i < len(order); i++ {
		pct[order[i]]++
	}
	return pct
}

// attributionLineMarker returns a one-character flag for the blame tables:
// "~" when the agent/checkpoint shown is a best-effort guess (the file is not in
// the checkpoint session's recorded paths, or only trailer-level metadata was
// found), "?" when more than one checkpoint is a candidate for the line, and a
// space otherwise. `entire why` surfaces the same information in prose; this
// closes the gap where the blame table looked equally confident on every line.
func attributionLineMarker(line attributionLine) string {
	switch {
	case line.SessionFallback || line.MetadataMissing:
		return "~"
	case len(line.Candidates) > 1:
		return "?"
	default:
		return " "
	}
}

// renderAttributionMarkerLegend prints a one-line legend explaining the blame
// markers, but only for the markers actually present in the table.
func renderAttributionMarkerLegend(w io.Writer, sty statusStyles, lines []attributionLine) {
	approximate, ambiguous := false, false
	for _, line := range lines {
		switch attributionLineMarker(line) {
		case "~":
			approximate = true
		case "?":
			ambiguous = true
		}
	}
	if !approximate && !ambiguous {
		return
	}
	var parts []string
	if approximate {
		parts = append(parts, "~ best-effort attribution (file not in the checkpoint's recorded paths)")
	}
	if ambiguous {
		parts = append(parts, "? multiple candidate checkpoints — see entire why <file>:<line>")
	}
	fmt.Fprintf(w, "  %s\n", sty.render(sty.dim, strings.Join(parts, "   ")))
}

func renderAttributionBlame(w io.Writer, result *fileAttributionResult, lineFlag string, longOutput bool) {
	if longOutput {
		renderAttributionBlameLong(w, result, lineFlag)
		return
	}
	renderAttributionBlameCompact(w, result, lineFlag)
}

// renderAttributionBlameTable renders the scaffolding shared by every blame
// table — the file header, the empty-file short-circuit, and the trailing
// summary — and delegates the column layout to body.
func renderAttributionBlameTable(w io.Writer, result *fileAttributionResult, lineFlag string, body func(statusStyles)) {
	sty := newStatusStyles(w)
	fmt.Fprintf(w, "\n  %s\n\n", sty.render(sty.bold, result.File))

	if len(result.Lines) == 0 {
		fmt.Fprintln(w, sty.render(sty.dim, "  No lines to display."))
		return
	}

	body(sty)
	renderAttributionMarkerLegend(w, sty, result.Lines)
	renderAttributionSummary(w, sty, result.Summary, lineFlag)
}

func renderAttributionBlameCompact(w io.Writer, result *fileAttributionResult, lineFlag string) {
	renderAttributionBlameTable(w, result, lineFlag, func(sty statusStyles) {
		lineWidth := attributionLineColumnWidth(result.Lines)
		const agentWidth = 6
		const authorWidth = 6
		const checkpointWidth = 12
		const minContentWidth = 12
		// The trailing "+ 1 + 1" reserves a one-character marker column (plus its
		// separator) between Checkpoint and Content. Placing it after the last
		// fixed column means only Content shifts — the Tag/Agent/Author/Checkpoint
		// positions stay put.
		fixedWidth := 2 + lineWidth + 2 + len("[AI]") + 2 + agentWidth + 2 + authorWidth + 2 + checkpointWidth + 2 + 1 + 1
		contentWidth := sty.width - fixedWidth
		if contentWidth < minContentWidth {
			contentWidth = minContentWidth
		}
		tableWidth := fixedWidth + contentWidth - 2

		fmt.Fprintf(w, "  %*s  Tag   %-*s  %-*s  %-*s    Content\n", lineWidth, "Line", agentWidth, "Agent", authorWidth, "Author", checkpointWidth, "Checkpoint")
		fmt.Fprintf(w, "  %s\n", sty.render(sty.dim, strings.Repeat("─", tableWidth)))

		for _, line := range result.Lines {
			fmt.Fprintf(w, "  %s  %s  %-*s  %-*s  %-*s  %s %s\n",
				sty.render(sty.dim, fmt.Sprintf("%*d", lineWidth, line.LineNumber)),
				renderAttributionTag(sty, line.Authorship),
				agentWidth,
				stringutil.TruncateRunes(compactAttributionAgent(line), agentWidth, ""),
				authorWidth,
				stringutil.TruncateRunes(shortAuthorName(line.Author), authorWidth, ""),
				checkpointWidth,
				stringutil.TruncateRunes(compactAttributionCheckpoint(line), checkpointWidth, ""),
				sty.render(sty.dim, attributionLineMarker(line)),
				renderAttributionContentCompact(sty, line, contentWidth),
			)
		}
	})
}

func renderAttributionBlameLong(w io.Writer, result *fileAttributionResult, lineFlag string) {
	renderAttributionBlameTable(w, result, lineFlag, func(sty statusStyles) {
		lineWidth := attributionLineColumnWidth(result.Lines)
		const checkpointColumnWidth = 21
		fmt.Fprintf(w, "  %*s  Tag   %-12s  %-18s  %-16s  %-21s    Content\n",
			lineWidth, "Line", "Agent", "Model", "Author", "Checkpoint/Session")
		fmt.Fprintf(w, "  %s\n", sty.render(sty.dim, strings.Repeat("─", lineWidth+92)))

		for _, line := range result.Lines {
			fmt.Fprintf(w, "  %s  %s  %-12s  %-18s  %-16s  %-21s  %s %s\n",
				sty.render(sty.dim, fmt.Sprintf("%*d", lineWidth, line.LineNumber)),
				renderAttributionTag(sty, line.Authorship),
				stringutil.TruncateRunes(line.Agent, 12, ""),
				stringutil.TruncateRunes(line.Model, 18, ""),
				stringutil.TruncateRunes(shortAuthorName(line.Author), 16, ""),
				stringutil.TruncateRunes(shortCheckpointSession(line), checkpointColumnWidth, ""),
				sty.render(sty.dim, attributionLineMarker(line)),
				renderAttributionContent(sty, line),
			)
		}

		fmt.Fprintf(w, "  %s\n", sty.render(sty.dim, strings.Repeat("─", lineWidth+92)))
	})
}

func renderAttributionSummary(w io.Writer, sty statusStyles, summary attributionSummary, lineFlag string) {
	parts := []string{
		sty.render(sty.green, fmt.Sprintf("AI: %d (%d%%)", summary.AILines, summary.AIPercentage)),
		fmt.Sprintf("Human: %d (%d%%)", summary.HumanLines, summary.HumanPercentage),
		sty.render(sty.yellow, fmt.Sprintf("Mixed: %d (%d%%)", summary.MixedLines, summary.MixedPercentage)),
	}
	if summary.UncommittedLines > 0 {
		parts = append(parts, sty.render(sty.dim, fmt.Sprintf("Uncommitted: %d", summary.UncommittedLines)))
	}
	if lineFlag != "" {
		fmt.Fprintf(w, "  %s %s %s\n\n", sty.render(sty.bold, "Summary:"), strings.Join(parts, sty.render(sty.dim, " · ")), sty.render(sty.dim, "(filtered)"))
		return
	}
	fmt.Fprintf(w, "  %s %s\n\n", sty.render(sty.bold, "Summary:"), strings.Join(parts, sty.render(sty.dim, " · ")))
}

func compactAttributionAgent(line attributionLine) string {
	switch line.Authorship {
	case attributionAI, attributionMixed:
		return fallbackString(line.Agent, "AI")
	case attributionUncommitted:
		return "working"
	case attributionHuman:
		return ""
	default:
		return ""
	}
}

func compactAttributionCheckpoint(line attributionLine) string {
	if line.CheckpointID != "" {
		return line.CheckpointID
	}
	if line.Authorship == attributionUncommitted {
		return "uncommitted"
	}
	return ""
}

func renderAttributionContentCompact(sty statusStyles, line attributionLine, width int) string {
	return renderByAuthorship(sty, line.Authorship, stringutil.TruncateRunes(line.Content, width, "..."))
}

func renderAttributionLineWhy(w io.Writer, file string, line attributionLine) {
	sty := newStatusStyles(w)
	fmt.Fprintf(w, "\n  %s %d in %s\n", sty.render(sty.bold, "Line"), line.LineNumber, sty.render(sty.bold, file))
	if line.Content != "" {
		fmt.Fprintf(w, "  %s\n\n", sty.render(sty.dim, strings.TrimRight(line.Content, "\r")))
	}

	switch line.Authorship {
	case attributionUncommitted:
		fmt.Fprintf(w, "  %s\n\n", sty.render(sty.yellow, "This line is not committed yet, so Entire cannot attribute it."))
	case attributionHuman:
		fmt.Fprintf(w, "  Written by %s", sty.render(sty.cyan, fallbackString(shortAuthorName(line.Author), "unknown")))
		if line.ShortCommitSHA != "" {
			fmt.Fprintf(w, " %s commit %s", sty.render(sty.dim, "·"), sty.render(sty.dim, line.ShortCommitSHA))
		}
		if line.AuthorTime != nil {
			fmt.Fprintf(w, " %s %s", sty.render(sty.dim, "·"), line.AuthorTime.Format("2006-01-02"))
		}
		fmt.Fprintf(w, "\n  %s\n\n", sty.render(sty.dim, "No Entire checkpoint is linked to the commit that last touched this line."))
	case attributionAI, attributionMixed:
		fmt.Fprintf(w, "  %s by %s", renderAttributionTag(sty, line.Authorship), sty.render(sty.agent, fallbackString(line.Agent, "Entire-tracked agent")))
		if line.Model != "" {
			fmt.Fprintf(w, " %s %s", sty.render(sty.dim, "·"), sty.render(sty.dim, line.Model))
		}
		if line.CheckpointID != "" {
			fmt.Fprintf(w, " %s checkpoint %s", sty.render(sty.dim, "·"), sty.render(sty.cyan, line.CheckpointID))
		}
		if line.SessionID != "" {
			fmt.Fprintf(w, " %s session %s", sty.render(sty.dim, "·"), sty.render(sty.dim, shortSessionID(line.SessionID)))
		}
		if line.ShortCommitSHA != "" {
			fmt.Fprintf(w, " %s commit %s", sty.render(sty.dim, "·"), sty.render(sty.dim, line.ShortCommitSHA))
		}
		fmt.Fprintln(w)
		if line.Prompt != "" {
			promptLabel := "Prompt:"
			if line.PromptSessionLevel {
				promptLabel = "Session prompt:"
			}
			fmt.Fprintf(w, "  %s %q\n", sty.render(sty.bold, promptLabel), stringutil.TruncateRunes(stringutil.CollapseWhitespace(line.Prompt), 160, "..."))
			if line.PromptSessionLevel {
				fmt.Fprintf(w, "  %s\n", sty.render(sty.dim, "(session-level prompt — may not appear in this checkpoint's transcript; see the checkpoint explain command below for what drove this checkpoint)"))
			}
		}
		if line.Intent != "" && line.Intent != line.Prompt {
			fmt.Fprintf(w, "  %s %q\n", sty.render(sty.bold, "Intent:"), stringutil.TruncateRunes(stringutil.CollapseWhitespace(line.Intent), 160, "..."))
		}
		if line.MetadataMissing {
			message := "Checkpoint metadata was not found locally; showing trailer-level attribution only."
			if line.MetadataMissingReason != "" {
				message = line.MetadataMissingReason
			}
			fmt.Fprintf(w, "  %s\n", sty.render(sty.yellow, message))
		}
		if line.SessionFallback {
			fmt.Fprintf(w, "  %s\n", sty.render(sty.yellow, "This file is not in the checkpoint session's recorded paths (it may have been renamed); the agent and prompt shown are a best-effort guess, not necessarily the session that produced this line."))
		}
		if len(line.Candidates) > 1 {
			fmt.Fprintf(w, "\n  %s\n", sty.render(sty.bold, "Candidate checkpoints:"))
			for _, candidate := range line.Candidates {
				fmt.Fprintf(w, "  - %s", candidate.CheckpointID)
				if candidate.SessionID != "" {
					fmt.Fprintf(w, " session %s", shortSessionID(candidate.SessionID))
				}
				if candidate.Agent != "" {
					fmt.Fprintf(w, " · %s", candidate.Agent)
				}
				if candidate.Prompt != "" {
					fmt.Fprintf(w, " · %q", stringutil.TruncateRunes(stringutil.CollapseWhitespace(candidate.Prompt), 80, "..."))
				}
				fmt.Fprintln(w)
			}
		}
		// The "Full context" hint suggests `entire checkpoint explain <id>`. Only
		// show it when the metadata is actually present: when it is missing, the
		// remote refresh `why` already attempted did not produce it (it failed,
		// or the remote genuinely lacks the data), so explain would fail the
		// same way (the reported bug — a hint that resolves to a command that
		// immediately errors). In that case the MetadataMissingReason printed
		// above already carries the appropriate recovery guidance.
		if line.CheckpointID != "" && !line.MetadataMissing {
			fmt.Fprintf(w, "\n  %s %s\n\n", sty.render(sty.dim, "Full context:"), sty.render(sty.cyan, "entire checkpoint explain "+line.CheckpointID))
		} else {
			fmt.Fprintln(w)
		}
	}
}

func renderAttributionFileWhy(w io.Writer, result *fileAttributionResult) {
	sty := newStatusStyles(w)
	summary := result.Summary
	fmt.Fprintf(w, "\n  %s\n", sty.render(sty.bold, result.File))
	fmt.Fprintf(w, "  %d lines %s %s %s %s",
		summary.TotalLines,
		sty.render(sty.dim, "·"),
		sty.render(sty.green, fmt.Sprintf("%d%% AI (%d)", summary.AIPercentage, summary.AILines)),
		sty.render(sty.dim, "·"),
		fmt.Sprintf("%d%% human (%d)", summary.HumanPercentage, summary.HumanLines),
	)
	if summary.MixedLines > 0 {
		fmt.Fprintf(w, " %s %s", sty.render(sty.dim, "·"), sty.render(sty.yellow, fmt.Sprintf("%d%% mixed (%d)", summary.MixedPercentage, summary.MixedLines)))
	}
	fmt.Fprintln(w)

	counts := checkpointLineCounts(result.Lines)
	if len(counts) == 0 {
		fmt.Fprintf(w, "\n  %s\n\n", sty.render(sty.dim, "No Entire checkpoints are linked to this file's current lines."))
		return
	}

	fmt.Fprintf(w, "\n  %s\n", sty.render(sty.bold, "Top checkpoints:"))
	for _, count := range counts {
		ctx := result.Checkpoints[count.CheckpointID]
		fmt.Fprintf(w, "  - %s  %d lines", sty.render(sty.cyan, count.CheckpointID), count.Lines)
		if ctx.Agent != "" {
			fmt.Fprintf(w, " %s %s", sty.render(sty.dim, "·"), ctx.Agent)
		}
		if ctx.SessionID != "" {
			fmt.Fprintf(w, " %s session %s", sty.render(sty.dim, "·"), shortSessionID(ctx.SessionID))
		}
		if ctx.Prompt != "" {
			fmt.Fprintf(w, " %s %q", sty.render(sty.dim, "·"), stringutil.TruncateRunes(stringutil.CollapseWhitespace(ctx.Prompt), 90, "..."))
		}
		if ctx.MetadataMissing {
			message := "Checkpoint metadata was not found locally."
			if ctx.MetadataMissingReason != "" {
				message = ctx.MetadataMissingReason
			}
			fmt.Fprintf(w, "\n    %s %s", sty.render(sty.yellow, "metadata missing:"), message)
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "\n  %s\n\n", sty.render(sty.dim, "Tip: entire why "+result.File+":<line> shows the prompt behind a specific line."))
}

type checkpointLineCount struct {
	CheckpointID string
	Lines        int
}

func checkpointLineCounts(lines []attributionLine) []checkpointLineCount {
	counts := make(map[string]int)
	for _, line := range lines {
		if line.CheckpointID != "" {
			counts[line.CheckpointID]++
		}
	}
	out := make([]checkpointLineCount, 0, len(counts))
	for cpID, count := range counts {
		out = append(out, checkpointLineCount{CheckpointID: cpID, Lines: count})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Lines == out[j].Lines {
			return out[i].CheckpointID < out[j].CheckpointID
		}
		return out[i].Lines > out[j].Lines
	})
	if len(out) > 5 {
		out = out[:5]
	}
	return out
}

// renderByAuthorship applies the authorship colour to text. Human and any
// unknown authorship render plain.
func renderByAuthorship(sty statusStyles, authorship attributionAuthorship, text string) string {
	switch authorship {
	case attributionAI:
		return sty.render(sty.green, text)
	case attributionMixed:
		return sty.render(sty.yellow, text)
	case attributionUncommitted:
		return sty.render(sty.dim, text)
	case attributionHuman:
		return text
	default:
		return text
	}
}

func renderAttributionTag(sty statusStyles, authorship attributionAuthorship) string {
	return renderByAuthorship(sty, authorship, attributionTag(authorship))
}

func renderAttributionContent(sty statusStyles, line attributionLine) string {
	return renderByAuthorship(sty, line.Authorship, stringutil.TruncateRunes(line.Content, 120, "..."))
}

func maxAttributionLineNumber(lines []attributionLine) int {
	maxLine := 1
	for _, line := range lines {
		if line.LineNumber > maxLine {
			maxLine = line.LineNumber
		}
	}
	return maxLine
}

func attributionLineColumnWidth(lines []attributionLine) int {
	return max(len("Line"), len(strconv.Itoa(maxAttributionLineNumber(lines))))
}

func attributionTag(authorship attributionAuthorship) string {
	switch authorship {
	case attributionAI:
		return "[AI]"
	case attributionMixed:
		return "[MX]"
	case attributionUncommitted:
		return "[??]"
	case attributionHuman:
		return "[HU]"
	default:
		return "[HU]"
	}
}

// applyPreferredToLine copies the preferred candidate's metadata onto the line.
// It does not touch line.Authorship; callers decide how Mixed maps to authorship.
func applyPreferredToLine(line *attributionLine, preferred *attributionCandidate) {
	if preferred == nil {
		return
	}
	line.CheckpointID = preferred.CheckpointID
	line.SessionID = preferred.SessionID
	line.Agent = preferred.Agent
	line.Model = preferred.Model
	line.Prompt = preferred.Prompt
	line.Intent = preferred.Intent
	line.MetadataMissing = preferred.MetadataMissing
	line.MetadataMissingReason = preferred.MetadataMissingReason
	line.SessionFallback = preferred.SessionFallback
	line.PromptSessionLevel = preferred.PromptSessionLevel
}

// authorshipForPreferred maps the preferred candidate to a line's authorship.
// A committed line that carries a checkpoint trailer is [AI]; it is [MX] only
// when the candidate that actually produced it (the session whose work touched
// this file) reflects mixed AI+human work. Both the initial blame resolution
// and the why-time remote enrichment use this single rule, so a line never
// changes tag between `entire blame` and `entire why`.
func authorshipForPreferred(preferred *attributionCandidate) attributionAuthorship {
	if preferred != nil && preferred.Mixed {
		return attributionMixed
	}
	return attributionAI
}

func preferredAttributionCandidate(candidates []attributionCandidate, file string) *attributionCandidate {
	if len(candidates) == 0 {
		return nil
	}
	for i := range candidates {
		if pathsContainFile(candidates[i].FilesTouched, file) {
			return &candidates[i]
		}
	}
	return &candidates[0]
}

func pathsContainFile(paths []string, file string) bool {
	normalizedFile := normalizeGitPath(file)
	for _, p := range paths {
		if normalizeGitPath(p) == normalizedFile {
			return true
		}
	}
	return false
}

func normalizePathSlice(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if normalized := normalizeGitPath(p); normalized != "" {
			out = appendUniqueString(out, normalized)
		}
	}
	return out
}

func normalizeGitPath(path string) string {
	path = strings.TrimSpace(path)
	path = strings.TrimPrefix(path, "/")
	return filepath.ToSlash(path)
}

func attributionIsMixed(attr *checkpoint.Attribution) bool {
	if attr == nil {
		return false
	}
	agentChanged := attr.AgentLines+attr.AgentRemoved > 0
	humanChanged := attr.HumanAdded+attr.HumanModified+attr.HumanRemoved > 0
	return agentChanged && humanChanged
}

func shortCheckpointSession(line attributionLine) string {
	if line.CheckpointID == "" {
		return ""
	}
	if line.SessionID == "" {
		return line.CheckpointID
	}
	return line.CheckpointID + "/" + shortSessionID(line.SessionID)
}

func shortSessionID(sessionID string) string {
	if len(sessionID) <= 8 {
		return sessionID
	}
	return sessionID[:8]
}

func shortSHA(sha string) string {
	if len(sha) <= 8 {
		return sha
	}
	return sha[:8]
}

func shortAuthorName(author string) string {
	author = strings.TrimSpace(author)
	if before, _, ok := strings.Cut(author, "<"); ok {
		author = strings.TrimSpace(before)
	}
	return author
}

func fallbackString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func appendUniqueString(values []string, value string) []string {
	if value == "" {
		return values
	}
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func isZeroCommit(sha string) bool {
	return sha == "" || strings.Trim(sha, "0") == ""
}

func writeJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return fmt.Errorf("encode json: %w", err)
	}
	return nil
}
