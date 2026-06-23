package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	"github.com/entireio/cli/cmd/entire/cli/paths"
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
	LineNumber      int                    `json:"line_number"`
	Authorship      attributionAuthorship  `json:"authorship"`
	Tag             string                 `json:"tag"`
	CommitSHA       string                 `json:"commit_sha,omitempty"`
	ShortCommitSHA  string                 `json:"short_commit_sha,omitempty"`
	Author          string                 `json:"author,omitempty"`
	AuthorTime      *time.Time             `json:"author_time,omitempty"`
	CheckpointID    string                 `json:"checkpoint_id,omitempty"`
	SessionID       string                 `json:"session_id,omitempty"`
	Agent           string                 `json:"agent,omitempty"`
	Model           string                 `json:"model,omitempty"`
	Prompt          string                 `json:"prompt,omitempty"`
	Intent          string                 `json:"intent,omitempty"`
	MetadataMissing bool                   `json:"metadata_missing,omitempty"`
	SessionFallback bool                   `json:"session_fallback,omitempty"`
	Content         string                 `json:"content"`
	Candidates      []attributionCandidate `json:"candidates,omitempty"`
}

// attributionCheckpointContext is the resolved metadata for one checkpoint as
// it applies to a file: the agent/session that produced the file's lines plus
// the prompt and intent behind them. The same shape is used two ways — as a
// per-line candidate (one line may map to several checkpoints) and as the
// deduplicated per-file checkpoint map — so attributionCandidate aliases it
// rather than duplicating the fields.
type attributionCheckpointContext struct {
	CheckpointID    string   `json:"checkpoint_id"`
	SessionID       string   `json:"session_id,omitempty"`
	Agent           string   `json:"agent,omitempty"`
	Model           string   `json:"model,omitempty"`
	Prompt          string   `json:"prompt,omitempty"`
	Intent          string   `json:"intent,omitempty"`
	FilesTouched    []string `json:"files_touched,omitempty"`
	MetadataMissing bool     `json:"metadata_missing,omitempty"`
	Mixed           bool     `json:"mixed,omitempty"`
	// SessionFallback is set when the file is not in any resolved session's
	// recorded paths (e.g. it was renamed after the checkpoint) and the
	// agent/prompt shown is a best-effort guess from the checkpoint's first
	// session rather than the session that actually touched this file.
	SessionFallback bool `json:"session_fallback,omitempty"`
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
	ReadCommitted(ctx context.Context, checkpointID id.CheckpointID) (*checkpoint.CheckpointSummary, error)
	ReadSessionMetadataAndPrompts(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (*checkpoint.SessionContent, error)
}

type attributionResolver struct {
	ctx         context.Context
	repo        *git.Repository
	store       attributionCheckpointReader
	fetchOnMiss bool

	commitCache     map[string]*object.Commit
	checkpointCache map[string]attributionCheckpointContext
}

func newBlameCmd() *cobra.Command {
	var lineFlag string
	var jsonFlag bool
	var longFlag bool

	cmd := &cobra.Command{
		Use: "blame <file>",
		// Hidden from `entire help` while the feature is still maturing —
		// advertised under `entire labs`, and `entire blame` / `entire blame
		// --help` keep working normally.
		Hidden: true,
		Short:  "Show which lines came from Entire checkpoints",
		Long:   "Show git-blame-style line attribution enriched with Entire checkpoint metadata.",
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

	cmd := &cobra.Command{
		Use: "why <file[:line]>",
		// Hidden from `entire help` while the feature is still maturing —
		// advertised under `entire labs`, and `entire why` / `entire why
		// --help` keep working normally.
		Hidden: true,
		Short:  "Show why a line exists",
		Long:   "Explain the commit, checkpoint, prompt, and session behind a file or line.",
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAttributionWhy(cmd.Context(), cmd.OutOrStdout(), args[0], jsonFlag)
		},
	}

	cmd.Flags().BoolVar(&jsonFlag, "json", false, "Output explanation as JSON")
	return cmd
}

type attributionBlameOptions struct {
	LineFlag string
	JSON     bool
	Long     bool
}

func runAttributionBlame(ctx context.Context, w io.Writer, file string, opts attributionBlameOptions) error {
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

func runAttributionWhy(ctx context.Context, w io.Writer, target string, jsonOutput bool) error {
	file, line, hasLine, err := parseAttributionWhyTarget(target)
	if err != nil {
		return err
	}

	result, err := resolveFileAttribution(ctx, file, false)
	if err != nil {
		return err
	}

	if !hasLine {
		if jsonOutput {
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
	if selected.MetadataMissing && selected.CheckpointID != "" {
		if err := enrichAttributionLineWithFetch(ctx, result.File, selected, result.Checkpoints); err != nil {
			// Remote metadata enrichment is best-effort; the trailer-level
			// explanation is still useful and should remain available.
			selected.MetadataMissing = true
		}
	}

	if jsonOutput {
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
					CheckpointID:    candidate.CheckpointID,
					MetadataMissing: true,
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

	stores, err := checkpoint.Open(ctx, repo, checkpoint.OpenOptions{BlobFetcher: FetchBlobsByHash})
	if err != nil {
		return nil, fmt.Errorf("open checkpoint store: %w", err)
	}

	return &attributionResolver{
		ctx:             ctx,
		repo:            repo,
		store:           stores.Primary,
		fetchOnMiss:     fetchOnMiss,
		commitCache:     make(map[string]*object.Commit),
		checkpointCache: make(map[string]attributionCheckpointContext),
	}, nil
}

func (r *attributionResolver) Close() {
	if r != nil && r.repo != nil {
		_ = r.repo.Close()
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

func (r *attributionResolver) readCheckpointContext(cpID id.CheckpointID, file string) attributionCheckpointContext {
	ctx := attributionCheckpointContext{CheckpointID: cpID.String()}
	summary, err := readAttributionCheckpointSummary(r.ctx, r.store, cpID)
	if err != nil && r.fetchOnMiss {
		if fetched, fetchErr := r.fetchCheckpointContext(cpID, file); fetchErr == nil {
			return fetched
		}
	}
	if err != nil {
		ctx.MetadataMissing = true
		return ctx
	}

	ctx.FilesTouched = normalizePathSlice(summary.FilesTouched)

	selected := checkpointSessionForFile{}
	var fallback checkpointSessionForFile
	sessionsRead := 0
	matchedFile := false
	for i := range summary.Sessions {
		sessionCtx, readErr := r.readSessionForCheckpoint(cpID, i)
		if readErr != nil {
			continue
		}
		sessionsRead++
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

	// We resolved a session, but the file is in none of the sessions' recorded
	// paths, and there was more than one session to choose from — so the agent
	// and prompt shown are a guess (the checkpoint's first session) rather than
	// the session that actually produced this line. Flag the approximation.
	if selected.SessionID != "" && !matchedFile && sessionsRead > 1 {
		ctx.SessionFallback = true
	}

	// Sessions existed but none could be read: the per-session detail (agent,
	// model, prompt) is unavailable even though the checkpoint commit exists.
	// Mark it missing so callers show the "trailer-level only" hint and the
	// why path attempts a remote fetch.
	if len(summary.Sessions) > 0 && sessionsRead == 0 {
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
	ctx.Intent = selected.Intent
	if len(selected.FilesTouched) > 0 {
		ctx.FilesTouched = selected.FilesTouched
	}
	if len(ctx.FilesTouched) == 0 {
		ctx.FilesTouched = normalizePathSlice(summary.FilesTouched)
	}
	return ctx
}

func readAttributionCheckpointSummary(ctx context.Context, reader attributionCheckpointReader, cpID id.CheckpointID) (*checkpoint.CheckpointSummary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err //nolint:wrapcheck // Propagating context cancellation
	}
	summary, err := reader.ReadCommitted(ctx, cpID)
	if err != nil {
		return nil, fmt.Errorf("read committed checkpoint: %w", err)
	}
	if summary == nil {
		return nil, checkpoint.ErrCheckpointNotFound
	}
	return summary, nil
}

func enrichAttributionLineWithFetch(ctx context.Context, file string, line *attributionLine, checkpoints map[string]attributionCheckpointContext) error {
	if line == nil || len(line.Candidates) == 0 {
		return nil
	}
	resolver, err := newAttributionResolver(ctx, true)
	if err != nil {
		return err
	}
	defer resolver.Close()

	candidates := make([]attributionCandidate, 0, len(line.Candidates))
	for _, candidate := range line.Candidates {
		cpID, idErr := id.NewCheckpointID(candidate.CheckpointID)
		if idErr != nil {
			candidates = append(candidates, candidate)
			continue
		}
		cpCtx := resolver.checkpointContext(cpID, file)
		checkpoints[cpCtx.CheckpointID] = cpCtx
		candidates = append(candidates, cpCtx)
	}
	preferred := preferredAttributionCandidate(candidates, file)
	applyPreferredToLine(line, preferred)
	line.Candidates = candidates
	line.Authorship = authorshipForPreferred(preferred)
	line.Tag = attributionTag(line.Authorship)
	return nil
}

func (r *attributionResolver) fetchCheckpointContext(cpID id.CheckpointID, file string) (attributionCheckpointContext, error) {
	lookup, err := newExplainCheckpointLookup(r.ctx)
	if err != nil {
		return attributionCheckpointContext{}, err
	}
	defer lookup.Close()

	matches, fresh := matchCheckpointPrefixWithRemoteFallback(r.ctx, io.Discard, lookup, cpID.String())
	if fresh != lookup {
		defer fresh.Close()
	}
	if len(matches) != 1 {
		return attributionCheckpointContext{}, checkpoint.ErrCheckpointNotFound
	}

	oldStore := r.store
	oldFetchOnMiss := r.fetchOnMiss
	r.store = fresh.store
	r.fetchOnMiss = false
	ctx := r.readCheckpointContext(cpID, file)
	r.store = oldStore
	r.fetchOnMiss = oldFetchOnMiss
	return ctx, nil
}

type checkpointSessionForFile struct {
	SessionID    string
	Agent        string
	Model        string
	Prompt       string
	Intent       string
	FilesTouched []string
	Attribution  *checkpoint.InitialAttribution
}

func (r *attributionResolver) readSessionForCheckpoint(cpID id.CheckpointID, index int) (checkpointSessionForFile, error) {
	content, err := r.store.ReadSessionMetadataAndPrompts(r.ctx, cpID, index)
	if err != nil {
		return checkpointSessionForFile{}, err //nolint:wrapcheck // caller skips partial metadata
	}
	meta := content.Metadata
	intent := ""
	if meta.Summary != nil {
		intent = strings.TrimSpace(meta.Summary.Intent)
	}
	prompt := strings.TrimSpace(content.Prompts)
	if prompt == "" {
		prompt = strings.TrimSpace(meta.ReviewPrompt)
	}
	if prompt == "" {
		prompt = intent
	}
	return checkpointSessionForFile{
		SessionID:    meta.SessionID,
		Agent:        string(meta.Agent),
		Model:        meta.Model,
		Prompt:       prompt,
		Intent:       intent,
		FilesTouched: normalizePathSlice(meta.FilesTouched),
		Attribution:  meta.InitialAttribution,
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

func parseAttributionWhyTarget(input string) (file string, line int, hasLine bool, err error) {
	colon := strings.LastIndex(input, ":")
	if colon == -1 || colon == len(input)-1 {
		return input, 0, false, nil
	}
	if volume := filepath.VolumeName(input); volume != "" && colon < len(volume) {
		return input, 0, false, nil
	}
	linePart := input[colon+1:]
	parsed, parseErr := strconv.Atoi(linePart)
	if parseErr != nil || parsed < 1 {
		return "", 0, false, fmt.Errorf("invalid line target %q: use file:line", input)
	}
	return input[:colon], parsed, true, nil
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
	renderAttributionSummary(w, sty, result.Summary, lineFlag)
}

func renderAttributionBlameCompact(w io.Writer, result *fileAttributionResult, lineFlag string) {
	renderAttributionBlameTable(w, result, lineFlag, func(sty statusStyles) {
		lineWidth := attributionLineColumnWidth(result.Lines)
		const agentWidth = 6
		const authorWidth = 6
		const checkpointWidth = 12
		const minContentWidth = 12
		fixedWidth := 2 + lineWidth + 2 + len("[AI]") + 2 + agentWidth + 2 + authorWidth + 2 + checkpointWidth + 2
		contentWidth := sty.width - fixedWidth
		if contentWidth < minContentWidth {
			contentWidth = minContentWidth
		}
		tableWidth := fixedWidth + contentWidth - 2

		fmt.Fprintf(w, "  %*s  Tag   %-*s  %-*s  %-*s  Content\n", lineWidth, "Line", agentWidth, "Agent", authorWidth, "Author", checkpointWidth, "Checkpoint")
		fmt.Fprintf(w, "  %s\n", sty.render(sty.dim, strings.Repeat("─", tableWidth)))

		for _, line := range result.Lines {
			fmt.Fprintf(w, "  %s  %s  %-*s  %-*s  %-*s  %s\n",
				sty.render(sty.dim, fmt.Sprintf("%*d", lineWidth, line.LineNumber)),
				renderAttributionTag(sty, line.Authorship),
				agentWidth,
				stringutil.TruncateRunes(compactAttributionAgent(line), agentWidth, ""),
				authorWidth,
				stringutil.TruncateRunes(shortAuthorName(line.Author), authorWidth, ""),
				checkpointWidth,
				stringutil.TruncateRunes(compactAttributionCheckpoint(line), checkpointWidth, ""),
				renderAttributionContentCompact(sty, line, contentWidth),
			)
		}
	})
}

func renderAttributionBlameLong(w io.Writer, result *fileAttributionResult, lineFlag string) {
	renderAttributionBlameTable(w, result, lineFlag, func(sty statusStyles) {
		lineWidth := attributionLineColumnWidth(result.Lines)
		const checkpointColumnWidth = 21
		fmt.Fprintf(w, "  %*s  Tag   %-12s  %-18s  %-16s  %-21s  Content\n",
			lineWidth, "Line", "Agent", "Model", "Author", "Checkpoint/Session")
		fmt.Fprintf(w, "  %s\n", sty.render(sty.dim, strings.Repeat("─", lineWidth+92)))

		for _, line := range result.Lines {
			fmt.Fprintf(w, "  %s  %s  %-12s  %-18s  %-16s  %-21s  %s\n",
				sty.render(sty.dim, fmt.Sprintf("%*d", lineWidth, line.LineNumber)),
				renderAttributionTag(sty, line.Authorship),
				stringutil.TruncateRunes(line.Agent, 12, ""),
				stringutil.TruncateRunes(line.Model, 18, ""),
				stringutil.TruncateRunes(shortAuthorName(line.Author), 16, ""),
				stringutil.TruncateRunes(shortCheckpointSession(line), checkpointColumnWidth, ""),
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
			fmt.Fprintf(w, "  %s %q\n", sty.render(sty.bold, "Prompt:"), stringutil.TruncateRunes(stringutil.CollapseWhitespace(line.Prompt), 160, "..."))
		}
		if line.Intent != "" && line.Intent != line.Prompt {
			fmt.Fprintf(w, "  %s %q\n", sty.render(sty.bold, "Intent:"), stringutil.TruncateRunes(stringutil.CollapseWhitespace(line.Intent), 160, "..."))
		}
		if line.MetadataMissing {
			fmt.Fprintf(w, "  %s\n", sty.render(sty.yellow, "Checkpoint metadata was not found locally; showing trailer-level attribution only."))
		}
		if line.SessionFallback {
			fmt.Fprintf(w, "  %s\n", sty.render(sty.yellow, "This file is not in the checkpoint's recorded paths (it may have been renamed); the agent and prompt shown are from the checkpoint's first session."))
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
		if line.CheckpointID != "" {
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
	line.SessionFallback = preferred.SessionFallback
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

func attributionIsMixed(attr *checkpoint.InitialAttribution) bool {
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
