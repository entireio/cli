package strategy

import (
	"context"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// foldCrossRepoLinks adds to state's CheckpointLinks the marker checkpoints
// other repositories recorded in this repository's link journal for the
// session. Other repositories only append to the journal; the session's own
// hooks (turn end, condensation) fold it, so only they write its state.
// AddCheckpointLink is keyed on the checkpoint, so folding again is a no-op.
func foldCrossRepoLinks(ctx context.Context, state *SessionState) {
	for _, link := range CrossRepoLinks(ctx, state.SessionID) {
		state.AddCheckpointLink(session.CheckpointLink{
			CheckpointID:  id.CheckpointID(link.CheckpointID),
			RepoKey:       link.RepoKey,
			LinkedCommits: []string{link.SHA},
			TurnID:        link.TurnID,
			Kind:          session.CheckpointLinkCrossRepo,
			At:            link.At,
		})
	}
}

// recordHomeCheckpointLink folds pending cross-repository links, then adds
// the home checkpoint just written, so the list stays in recorded order.
// commitSHA is empty when no commit carries the checkpoint.
func recordHomeCheckpointLink(ctx context.Context, state *SessionState, cpID id.CheckpointID, commitSHA string) {
	foldCrossRepoLinks(ctx, state)
	link := session.CheckpointLink{CheckpointID: cpID, TurnID: state.TurnID, Kind: session.CheckpointLinkHome, At: time.Now().UTC()}
	if commitSHA != "" {
		link.LinkedCommits = []string{commitSHA}
	}
	if root, err := paths.WorktreeRoot(ctx); err == nil {
		link.RepoKey, _ = settings.RepoKey(ctx, root) //nolint:errcheck // empty when unknown
	}
	state.AddCheckpointLink(link)
}
