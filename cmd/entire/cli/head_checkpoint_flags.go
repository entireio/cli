package cli

// head_checkpoint_flags.go resolves the review/investigation umbrella flags
// for the checkpoint at HEAD. These functions live in the cli package (not the
// review/ subpackage) because they need checkpoint access, and review →
// checkpoint → codex → review would cycle. They are cross-feature: consumed by
// `entire status` and by both the review and investigate re-run guards.

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

// headCheckpointFlags returns the (HasReview, HasInvestigation, info) triple
// for HEAD's checkpoint. Returns (false, false, "") when there is no
// checkpoint at HEAD or when reading fails (logged via slog Debug).
//
// info is a human-readable string used by status / re-run guards (e.g.
// "checkpoint abc123def456"). It applies to whichever flag is true; callers
// display the appropriate flag's prose around it.
//
// Single lookup: read the Entire-Checkpoint trailer from HEAD, then resolve
// the CheckpointSummary through the configured committed checkpoint store
// (handles v1, v2, and dual reader selection internally).
func headCheckpointFlags(ctx context.Context) (hasReview, hasInvestigation bool, info string) {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		logging.Debug(ctx, "head checkpoint flags: locate worktree root", slog.String("error", err.Error()))
		return false, false, ""
	}
	execCmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "log", "-1", "--format=%B")
	output, err := execCmd.Output()
	if err != nil {
		logging.Debug(ctx, "head checkpoint flags: read HEAD commit message", slog.String("error", err.Error()))
		return false, false, ""
	}
	cpID, ok := trailers.ParseCheckpoint(string(output))
	if !ok {
		logging.Debug(ctx, "head checkpoint flags: no Entire-Checkpoint trailer on HEAD")
		return false, false, ""
	}
	repo, err := gitrepo.OpenPath(repoRoot)
	if err != nil {
		logging.Debug(ctx, "head checkpoint flags: open repository", slog.String("error", err.Error()))
		return false, false, ""
	}
	defer repo.Close()
	store, storeErr := checkpoint.NewCommittedReader(ctx, repo, checkpoint.CommittedReaderOptions{})
	if storeErr != nil {
		logging.Debug(ctx, "head checkpoint flags: checkpoint store unavailable", slog.String("error", storeErr.Error()))
		return false, false, ""
	}
	summary, err := checkpoint.ReadCommittedCheckpoint(ctx, store, cpID)
	if err != nil || summary == nil {
		logging.Debug(ctx, "head checkpoint flags: resolve checkpoint summary",
			slog.String("checkpoint_id", cpID.String()),
			slog.Any("error", err))
		return false, false, ""
	}
	return summary.HasReview, summary.HasInvestigation, fmt.Sprintf("checkpoint %s", cpID)
}

// headHasReviewCheckpoint checks whether HEAD's checkpoint metadata includes
// a review session. Returns (true, infoString) if HasReview is set.
// Thin compatibility wrapper around headCheckpointFlags so existing callers
// (status display, review re-run guard) keep their (bool, string) signature.
func headHasReviewCheckpoint(ctx context.Context) (bool, string) {
	hasReview, _, info := headCheckpointFlags(ctx)
	if !hasReview {
		return false, ""
	}
	return true, info
}

// headHasInvestigateCheckpoint reports whether HEAD's checkpoint has an
// investigation tagged on it. Mirrors headHasReviewCheckpoint for the
// investigation umbrella flag.
func headHasInvestigateCheckpoint(ctx context.Context) (bool, string) {
	_, hasInvestigation, info := headCheckpointFlags(ctx)
	if !hasInvestigation {
		return false, ""
	}
	return true, info
}
