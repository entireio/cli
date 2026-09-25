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
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
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
// the CheckpointSummary from the v1 metadata branch.
func headCheckpointFlags(ctx context.Context) (hasReview, hasInvestigation bool, info string) {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		logging.Debug(ctx, "head checkpoint flags: locate worktree root", slog.String("error", err.Error()))
		return false, false, ""
	}
	// On the native path the repository is opened only once a trailer exists:
	// opening a reftable repository itself spawns Git.
	var repo *git.Repository
	if !gitrepo.ReadsNeedNativeGit(ctx) {
		if repo = openHeadRepository(ctx, repoRoot); repo == nil {
			return false, false, ""
		}
		defer repo.Close()
	}
	message, err := headCommitMessage(ctx, repo, repoRoot)
	if err != nil {
		logging.Debug(ctx, "head checkpoint flags: read HEAD commit message", slog.String("error", err.Error()))
		return false, false, ""
	}
	cpID, ok := trailers.ParseCheckpoint(message)
	if !ok {
		logging.Debug(ctx, "head checkpoint flags: no Entire-Checkpoint trailer on HEAD")
		return false, false, ""
	}
	if repo == nil {
		if repo = openHeadRepository(ctx, repoRoot); repo == nil {
			return false, false, ""
		}
		defer repo.Close()
	}
	stores, err := checkpoint.Open(ctx, repo, checkpoint.OpenOptions{ReadRemotes: strategy.CheckpointReadRemotes(ctx)})
	if err != nil {
		logging.Debug(ctx, "head checkpoint flags: open store", slog.String("error", err.Error()))
		return false, false, ""
	}
	summary, err := checkpoint.ReadCheckpoint(ctx, stores.Persistent, cpID)
	if err != nil || summary == nil {
		logging.Debug(ctx, "head checkpoint flags: resolve checkpoint summary",
			slog.String("checkpoint_id", cpID.String()),
			slog.Any("error", err))
		return false, false, ""
	}
	return summary.HasReview, summary.HasInvestigation, fmt.Sprintf("checkpoint %s", cpID)
}

// openHeadRepository opens repoRoot for headCheckpointFlags, logging and
// returning nil on failure.
func openHeadRepository(ctx context.Context, repoRoot string) *git.Repository {
	repo, err := gitrepo.OpenPath(repoRoot)
	if err != nil {
		logging.Debug(ctx, "head checkpoint flags: open repository", slog.String("error", err.Error()))
		return nil
	}
	return repo
}

// headCommitMessage returns HEAD's raw commit message. A nil repo means the
// caller's gitrepo.ReadsNeedNativeGit check chose native Git; with a repo it
// reads through go-git and keeps native Git as the fallback for replace refs
// and missing promisor objects that Git can fetch on demand.
//
// Both paths return the stored message bytes: git log's %B appends a format
// terminator, which is trimmed. The native path also transcodes a non-UTF-8
// commit encoding header, which go-git does not (see git-safety.md).
func headCommitMessage(ctx context.Context, repo *git.Repository, repoRoot string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("read HEAD message: %w", err)
	}
	if repo != nil {
		commit, err := gitrepo.CommitAtReference(ctx, repo, plumbing.HEAD)
		if err == nil {
			return commit.Message, nil
		}
		logging.Debug(ctx, "head commit message: go-git read failed, using native Git", slog.String("error", err.Error()))
	}
	out, err := exec.CommandContext(ctx, "git", "-C", repoRoot, "log", "-1", "--format=%B").Output()
	if err != nil {
		return "", fmt.Errorf("read HEAD message with Git: %w", err)
	}
	return strings.TrimSuffix(string(out), "\n"), nil
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
