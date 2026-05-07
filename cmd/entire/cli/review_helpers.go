package cli

// review_helpers.go holds the review-related functions that must remain in
// the cli package due to import cycles. Functions in the review/ subpackage
// cannot import checkpoint or the per-agent reviewer packages without cycling
// back through review:
//
//   review → checkpoint → codex → review
//   review → claudecode/codex/geminicli → review
//
// headHasReviewCheckpoint requires checkpoint access and stays here.
// newReviewAttachCmd uses runAttachSurfaceReviewErrors (in attach.go)
// and also stays here.

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"

	git "github.com/go-git/go-git/v6"
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/external"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	cliReview "github.com/entireio/cli/cmd/entire/cli/review"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

// headHasReviewCheckpoint checks whether HEAD's checkpoint metadata includes
// a review session. Returns (true, infoString) if HasReview is set.
// Single lookup: read the Entire-Checkpoint trailer from HEAD, then resolve
// the CheckpointSummary via ResolveCommittedReaderForCheckpoint so v2-enabled
// repos also work (v1 alone would miss v2-written summaries).
func headHasReviewCheckpoint(ctx context.Context) (bool, string) {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		logging.Debug(ctx, "head review check: locate worktree root", slog.String("error", err.Error()))
		return false, ""
	}
	execCmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "log", "-1", "--format=%B")
	output, err := execCmd.Output()
	if err != nil {
		logging.Debug(ctx, "head review check: read HEAD commit message", slog.String("error", err.Error()))
		return false, ""
	}
	cpID, ok := trailers.ParseCheckpoint(string(output))
	if !ok {
		logging.Debug(ctx, "head review check: no Entire-Checkpoint trailer on HEAD")
		return false, ""
	}
	repo, err := git.PlainOpen(repoRoot)
	if err != nil {
		logging.Debug(ctx, "head review check: open repository", slog.String("error", err.Error()))
		return false, ""
	}
	v1Store := checkpoint.NewGitStore(repo)
	v2URL, urlErr := remote.FetchURL(ctx)
	if urlErr != nil {
		logging.Debug(ctx, "head review check: no configured v2 fetch remote", slog.String("error", urlErr.Error()))
		v2URL = ""
	}
	v2Store := checkpoint.NewV2GitStore(repo, v2URL)
	_, summary, err := checkpoint.ResolveCommittedReaderForCheckpoint(ctx, cpID, v1Store, v2Store, settings.IsCheckpointsV2Enabled(ctx))
	if err != nil || summary == nil {
		logging.Debug(ctx, "head review check: resolve checkpoint summary",
			slog.String("checkpoint_id", cpID.String()),
			slog.Any("error", err))
		return false, ""
	}
	if !summary.HasReview {
		logging.Debug(ctx, "head review check: summary HasReview is false", slog.String("checkpoint_id", cpID.String()))
		return false, ""
	}
	return true, fmt.Sprintf("checkpoint %s", cpID)
}

// newReviewAttachCmd is a thin wrapper around `entire attach --review`. It
// shares all wiring with runAttach; only the UX surface differs, letting
// users discover review-attach through `entire review` in help output.
//
// Migrated from the old review.go. Kept here (not in review/ subpackage)
// because it calls runAttachSurfaceReviewErrors which is in the cli package.
func newReviewAttachCmd() *cobra.Command {
	var (
		force      bool
		agentFlag  string
		skillsFlag []string
	)
	cmd := &cobra.Command{
		Use:   "attach <session-id>",
		Short: "Tag an existing agent session as a review",
		Long: `Tag an existing agent session as an agent_review and link it to
the current commit's checkpoint. Use this when you ran a review manually
(without 'entire review') and want the review metadata attached after
the fact.

The first user prompt in the transcript is recorded as the review
prompt. Pass --skills to declare which skills were actually run; omit
to attach a review without a declared skills list.

Equivalent to 'entire attach --review <session-id>' — provided here for
discoverability alongside the other review subcommands.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return cmd.Help()
			}
			if checkDisabledGuard(cmd.Context(), cmd.OutOrStdout()) {
				return nil
			}
			// Discover external agents so --agent <external-name> is
			// recognized and auto-detection covers them.
			external.DiscoverAndRegister(cmd.Context())

			marker, useMarker, markerErr := matchingPendingReviewMarker(cmd.Context(), agentFlag, cmd.Flags().Changed("agent"))
			if markerErr != nil {
				return markerErr
			}
			if useMarker && !cmd.Flags().Changed("agent") && marker.AgentName != "" {
				agentFlag = marker.AgentName
			}
			opts := attachOptions{
				Force:                force,
				Review:               true,
				ReviewSkillsOverride: skillsFlag,
			}
			if useMarker {
				if !cmd.Flags().Changed("skills") {
					opts.ReviewSkillsOverride = marker.Skills
				}
				opts.ReviewPromptOverride = marker.Prompt
			}
			err := runAttachSurfaceReviewErrors(cmd, args[0], types.AgentName(agentFlag), opts)
			if err == nil && useMarker {
				if clearErr := cliReview.ClearPendingReviewMarker(cmd.Context()); clearErr != nil {
					logging.Debug(cmd.Context(), "clear pending review marker after attach", slog.String("error", clearErr.Error()))
				}
			}
			return err
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "Skip confirmation and amend the last commit with the checkpoint trailer")
	cmd.Flags().StringVarP(&agentFlag, "agent", "a", string(agent.DefaultAgentName), "Agent that created the session")
	cmd.Flags().StringSliceVar(&skillsFlag, "skills", nil, "Optional: declare which review skills were run in this session")
	return cmd
}

func matchingPendingReviewMarker(ctx context.Context, selectedAgent string, agentChanged bool) (cliReview.PendingReviewMarker, bool, error) {
	marker, ok, err := cliReview.ReadPendingReviewMarker(ctx)
	if err != nil {
		return cliReview.PendingReviewMarker{}, false, fmt.Errorf("read pending review marker: %w", err)
	}
	if !ok {
		return cliReview.PendingReviewMarker{}, false, nil
	}
	if marker.WorktreePath != "" {
		worktreeRoot, rootErr := paths.WorktreeRoot(ctx)
		if rootErr != nil {
			return cliReview.PendingReviewMarker{}, false, fmt.Errorf("resolve worktree root for pending review marker: %w", rootErr)
		}
		if marker.WorktreePath != worktreeRoot {
			return cliReview.PendingReviewMarker{}, false, nil
		}
	}
	if agentChanged && marker.AgentName != "" && marker.AgentName != selectedAgent {
		return cliReview.PendingReviewMarker{}, false, nil
	}
	return marker, true, nil
}
