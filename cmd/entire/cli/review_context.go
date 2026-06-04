package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	checkpointid "github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/stringutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

const (
	reviewContextMaxDetailRunes  = 320
	reviewContextMaxCheckpoints  = 20
	reviewContextMaxCommitScans  = 200
	reviewContextCommitSeparator = "\x1e"
)

type reviewContextSessionMetadataReader interface {
	ReadSessionMetadata(ctx context.Context, checkpointID checkpointid.CheckpointID, sessionIndex int) (*checkpoint.CommittedMetadata, error)
}

type reviewContextSessionMetadataPromptsReader interface {
	ReadSessionMetadataAndPrompts(ctx context.Context, checkpointID checkpointid.CheckpointID, sessionIndex int) (*checkpoint.SessionContent, error)
}

func reviewCheckpointContext(ctx context.Context, worktreeRoot string, scopeBaseRef string) string {
	committed := reviewCommittedCheckpointContext(ctx, worktreeRoot, scopeBaseRef)
	inProgress := reviewSessionContextForCurrentHead(ctx, worktreeRoot)
	return joinReviewContextSections(committed, inProgress)
}

// joinReviewContextSections concatenates non-empty review-context sections
// with a blank line between them so each lands as its own paragraph in the
// composed agent prompt. Either argument may be empty; when both are empty
// the result is empty (and ComposeReviewPrompt skips it cleanly).
func joinReviewContextSections(sections ...string) string {
	nonEmpty := sections[:0]
	for _, s := range sections {
		if s != "" {
			nonEmpty = append(nonEmpty, s)
		}
	}
	return strings.Join(nonEmpty, "\n\n")
}

// reviewSessionContextForCurrentHead resolves HEAD and delegates to
// reviewSessionContext. Kept separate from reviewCheckpointContext so that
// in-progress session context is surfaced even when there are no committed
// checkpoints in scope (the common case: branch with only uncommitted work).
func reviewSessionContextForCurrentHead(ctx context.Context, worktreeRoot string) string {
	repo, err := gitrepo.OpenPath(worktreeRoot)
	if err != nil {
		logging.Debug(ctx, "review session context: open repo", slog.String("error", err.Error()))
		return ""
	}
	defer repo.Close()
	head, err := repo.Head()
	if err != nil {
		logging.Debug(ctx, "review session context: resolve HEAD", slog.String("error", err.Error()))
		return ""
	}
	return reviewSessionContext(ctx, worktreeRoot, head.Hash().String())
}

// reviewCommittedCheckpointContext renders the "Checkpoint context from
// commits in scope:" section. Previously the body of reviewCheckpointContext;
// extracted so the parent can compose it with the in-progress session
// section.
func reviewCommittedCheckpointContext(ctx context.Context, worktreeRoot string, scopeBaseRef string) string {
	if scopeBaseRef == "" {
		return ""
	}

	messages, commitsTruncated, err := reviewContextCommitMessages(ctx, worktreeRoot, scopeBaseRef, reviewContextMaxCommitScans)
	if err != nil || len(messages) == 0 {
		if err != nil {
			logging.Debug(ctx, "review checkpoint context: list commit messages", slog.String("error", err.Error()))
		}
		return ""
	}

	repo, err := gitrepo.OpenPath(worktreeRoot)
	if err != nil {
		logging.Debug(ctx, "review checkpoint context: open repo", slog.String("error", err.Error()))
		return ""
	}
	defer repo.Close()
	store := checkpoint.NewCommittedReadStore(ctx, repo)

	var lines []string
	seen := map[checkpointid.CheckpointID]bool{}
	omittedCheckpoints := 0
	for _, message := range messages {
		for _, cpID := range trailers.ParseAllCheckpoints(message) {
			if seen[cpID] {
				continue
			}
			seen[cpID] = true

			if len(lines) >= reviewContextMaxCheckpoints {
				omittedCheckpoints++
				continue
			}

			summary, err := checkpoint.ReadCommittedCheckpoint(ctx, store, cpID)
			if err != nil {
				lines = append(lines, fmt.Sprintf("- %s: checkpoint metadata unavailable", cpID))
				continue
			}
			detail := reviewCheckpointDetail(ctx, store, cpID, summary)
			if detail == "" {
				detail = "no summary or prompt recorded"
			}
			lines = append(lines, fmt.Sprintf("- %s: %s", cpID, detail))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	if omittedCheckpoints > 0 {
		lines = append(lines, fmt.Sprintf("- ... %d more %s omitted", omittedCheckpoints, reviewContextCheckpointNoun(omittedCheckpoints)))
	}
	if commitsTruncated {
		lines = append(lines, fmt.Sprintf("- ... older commits omitted after scanning latest %d commits", reviewContextMaxCommitScans))
	}

	return "Checkpoint context from commits in scope:\n" +
		strings.Join(lines, "\n") +
		"\n\nUse `entire explain <id>` for full checkpoint context, or `entire explain <id> --raw-transcript` for raw transcripts."
}

// reviewSessionContext returns a "In-progress session context (uncommitted):"
// block summarising active agent sessions whose work is not yet committed.
//
// Inclusion criteria — a session qualifies if all of:
//   - state.WorktreePath == worktreeRoot (this checkout)
//   - state.BaseCommit == headSHA (work since the last commit on this branch)
//   - !state.FullyCondensed (still in flight)
//   - state.Kind != KindAgentReview (don't include the review agent itself)
//
// For each qualifying session, render one line:
//
//	<sessionID[:8]> <agent-name> [(touched: N file(s))] prompt: <latest prompt>
//
// where latest prompt is read from <worktree>/.entire/metadata/<sessionID>/prompt.txt
// (the on-filesystem path lifecycle.go appends to on every turn), passed through
// the existing reviewPromptText helper to match the committed-pipeline fallback
// format (loops backwards for the newest non-empty prompt, collapses whitespace,
// truncates).
//
// Best-effort: any error path returns "" so the run continues. Sessions whose
// prompt.txt is missing or empty are skipped silently.
//
// Why filesystem-not-shadow-branch: for active sessions prompts are written to
// disk at lifecycle.go:294-310 on every turn for mid-turn commit availability,
// before SaveStep copies them onto the shadow branch. Filesystem is canonical
// for in-progress reads; the shadow-branch copy is only canonical post-condensation.
func reviewSessionContext(ctx context.Context, worktreeRoot, headSHA string) string {
	if worktreeRoot == "" || headSHA == "" {
		return ""
	}
	store, err := session.NewStateStore(ctx)
	if err != nil {
		logging.Debug(ctx, "review session context: open state store", slog.String("error", err.Error()))
		return ""
	}
	states, err := store.List(ctx)
	if err != nil {
		logging.Debug(ctx, "review session context: list session states", slog.String("error", err.Error()))
		return ""
	}

	// Canonicalise the current worktree path once. State files written by
	// lifecycle hooks store whatever path the agent process was launched
	// from, which on macOS frequently differs from `paths.WorktreeRoot`
	// only by the /var → /private/var symlink. Comparing canonical forms
	// avoids missing matches that string equality would drop.
	worktreeCanon := canonicalisePath(worktreeRoot)

	var lines []string
	for _, st := range states {
		if st == nil {
			continue
		}
		if canonicalisePath(st.WorktreePath) != worktreeCanon {
			continue
		}
		if st.BaseCommit != headSHA {
			continue
		}
		if st.FullyCondensed {
			continue
		}
		if st.Kind == session.KindAgentReview {
			continue
		}
		line := formatReviewSessionLine(worktreeRoot, st)
		if line == "" {
			continue
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return ""
	}
	return "In-progress session context (uncommitted):\n" + strings.Join(lines, "\n")
}

// canonicalisePath returns the symlink-resolved absolute form of p. Falls
// back to p itself when EvalSymlinks fails (e.g., the path doesn't exist
// yet) so callers always get a usable comparable value.
func canonicalisePath(p string) string {
	if p == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return p
}

// formatReviewSessionLine renders one entry of the in-progress section.
// Returns "" when the session has no prompt content to report.
func formatReviewSessionLine(worktreeRoot string, st *session.State) string {
	promptPath := filepath.Join(worktreeRoot, paths.SessionMetadataDirFromSessionID(st.SessionID), paths.PromptFileName)
	raw, err := os.ReadFile(promptPath) //nolint:gosec // path constructed from validated session ID + fixed constants
	if err != nil {
		return ""
	}
	promptText := reviewPromptText(string(raw))
	if promptText == "" {
		return ""
	}

	short := st.SessionID
	if len(short) > 8 {
		short = short[:8]
	}
	agentName := string(st.AgentType)
	if agentName == "" {
		agentName = "agent"
	}

	parts := []string{"  " + short, agentName}
	if n := len(st.FilesTouched); n > 0 {
		fileWord := "files"
		if n == 1 {
			fileWord = "file"
		}
		parts = append(parts, fmt.Sprintf("(touched: %d %s)", n, fileWord))
	}
	parts = append(parts, "prompt: "+promptText)
	return strings.Join(parts, " ")
}

func reviewCheckpointDetail(
	ctx context.Context,
	reader checkpoint.CommittedReader,
	cpID checkpointid.CheckpointID,
	summary *checkpoint.CheckpointSummary,
) string {
	sessions := make([]reviewContextSessionDetail, 0, len(summary.Sessions))
	for i := len(summary.Sessions) - 1; i >= 0; i-- {
		meta, err := readReviewContextSessionMetadata(ctx, reader, cpID, i)
		if err != nil || meta == nil || session.Kind(meta.Kind).IsReview() {
			continue
		}
		sessions = append(sessions, reviewContextSessionDetail{
			index: i,
		})
		if text := reviewSummaryText(meta.Summary); text != "" {
			return "summary: " + text
		}
	}
	for _, sessionDetail := range sessions {
		prompts, err := readReviewContextSessionPrompts(ctx, reader, cpID, sessionDetail.index)
		if err == nil {
			if text := reviewPromptText(prompts); text != "" {
				return "prompt: " + text
			}
		}
	}
	return ""
}

type reviewContextSessionDetail struct {
	index int
}

func readReviewContextSessionMetadata(
	ctx context.Context,
	reader checkpoint.CommittedReader,
	cpID checkpointid.CheckpointID,
	sessionIndex int,
) (*checkpoint.CommittedMetadata, error) {
	if r, ok := reader.(reviewContextSessionMetadataReader); ok {
		return r.ReadSessionMetadata(ctx, cpID, sessionIndex) //nolint:wrapcheck // Best-effort prompt context.
	}
	content, err := reader.ReadSessionContent(ctx, cpID, sessionIndex)
	if err != nil {
		return nil, err //nolint:wrapcheck // Best-effort prompt context.
	}
	if content == nil {
		return nil, errors.New("session content is nil")
	}
	return &content.Metadata, nil
}

func readReviewContextSessionPrompts(
	ctx context.Context,
	reader checkpoint.CommittedReader,
	cpID checkpointid.CheckpointID,
	sessionIndex int,
) (string, error) {
	if r, ok := reader.(reviewContextSessionMetadataPromptsReader); ok {
		content, err := r.ReadSessionMetadataAndPrompts(ctx, cpID, sessionIndex)
		if err == nil {
			if content == nil {
				return "", errors.New("session content is nil")
			}
			return content.Prompts, nil
		}
		if !errors.Is(err, checkpoint.ErrCheckpointNotFound) {
			return "", err //nolint:wrapcheck // Best-effort prompt context.
		}
	}
	content, err := reader.ReadSessionContent(ctx, cpID, sessionIndex)
	if err != nil {
		return "", err //nolint:wrapcheck // Best-effort prompt context.
	}
	if content == nil {
		return "", errors.New("session content is nil")
	}
	return content.Prompts, nil
}

func reviewSummaryText(summary *checkpoint.Summary) string {
	if summary == nil {
		return ""
	}
	parts := []string{
		stringutil.CollapseWhitespace(summary.Intent),
		stringutil.CollapseWhitespace(summary.Outcome),
	}
	for _, item := range summary.OpenItems {
		if text := stringutil.CollapseWhitespace(item); text != "" {
			parts = append(parts, "open: "+text)
			break
		}
	}
	return truncateReviewContextText(strings.Join(nonEmptyReviewContextParts(parts), "; "))
}

func reviewPromptText(promptContent string) string {
	prompts := checkpoint.SplitPromptContent(promptContent)
	for i := len(prompts) - 1; i >= 0; i-- {
		if text := stringutil.CollapseWhitespace(prompts[i]); text != "" {
			return truncateReviewContextText(text)
		}
	}
	return ""
}

func nonEmptyReviewContextParts(parts []string) []string {
	result := parts[:0]
	for _, part := range parts {
		if part != "" {
			result = append(result, part)
		}
	}
	return result
}

func truncateReviewContextText(value string) string {
	runes := []rune(value)
	if len(runes) <= reviewContextMaxDetailRunes {
		return value
	}
	return strings.TrimSpace(string(runes[:reviewContextMaxDetailRunes-3])) + "..."
}

func reviewContextCheckpointNoun(count int) string {
	if count == 1 {
		return "checkpoint"
	}
	return "checkpoints"
}

func reviewContextCommitMessages(ctx context.Context, repoRoot string, scopeBaseRef string, maxCommits int) ([]string, bool, error) {
	if maxCommits <= 0 {
		return nil, false, nil
	}
	records, err := reviewContextGitRecords(
		ctx,
		repoRoot,
		"log",
		"--max-count="+strconv.Itoa(maxCommits+1),
		"--format="+reviewContextCommitSeparator+"%B",
		scopeBaseRef+"..HEAD",
	)
	if err != nil {
		return nil, false, err
	}
	truncated := len(records) > maxCommits
	if truncated {
		records = records[:maxCommits]
	}
	return records, truncated, nil
}

func reviewContextGitRecords(ctx context.Context, repoRoot string, args ...string) ([]string, error) {
	full := append([]string{"-C", repoRoot}, args...)
	output, err := exec.CommandContext(ctx, "git", full...).Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	parts := strings.Split(string(output), reviewContextCommitSeparator)
	records := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			records = append(records, trimmed)
		}
	}
	return records, nil
}
