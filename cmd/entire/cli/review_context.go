package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"

	git "github.com/go-git/go-git/v6"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	checkpointid "github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/settings"
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

	repo, err := git.PlainOpen(worktreeRoot)
	if err != nil {
		logging.Debug(ctx, "review checkpoint context: open repo", slog.String("error", err.Error()))
		return ""
	}
	v1 := checkpoint.NewGitStore(repo)
	v2URL, urlErr := remote.FetchURL(ctx)
	if urlErr != nil {
		logging.Debug(ctx, "review checkpoint context: no v2 fetch remote", slog.String("error", urlErr.Error()))
	}
	v2 := checkpoint.NewV2GitStore(repo, v2URL)
	preferCheckpointsV2 := settings.IsCheckpointsV2Enabled(ctx)

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

			reader, summary, err := checkpoint.ResolveCommittedReaderForCheckpoint(ctx, cpID, v1, v2, preferCheckpointsV2)
			if err != nil || summary == nil {
				lines = append(lines, fmt.Sprintf("- %s: checkpoint metadata unavailable", cpID))
				continue
			}
			detail := reviewCheckpointDetail(ctx, reader, cpID, summary)
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
		if err != nil {
			return "", err //nolint:wrapcheck // Best-effort prompt context.
		}
		if content == nil {
			return "", errors.New("session content is nil")
		}
		return content.Prompts, nil
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
