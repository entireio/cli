package cli

// review_helpers.go holds the review-related functions that must remain in
// the cli package due to import cycles. Functions in the review/ subpackage
// cannot import checkpoint or the per-agent reviewer packages without cycling
// back through review:
//
//   review → checkpoint → codex → review
//   review → claudecode/codex/geminicli → review
//
// matchingPendingReviewMarker is consumed by `entire attach --review` (in
// attach.go) to adopt a pending-review marker. HEAD-checkpoint flag
// resolution lives in head_checkpoint_flags.go.

import (
	"context"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	cliReview "github.com/entireio/cli/cmd/entire/cli/review"
)

// matchingPendingReviewMarker returns the pending-review marker, if one exists
// and applies to the current worktree and selected agent. ok=false means there
// is no applicable marker (the attach should proceed without adopting one).
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
