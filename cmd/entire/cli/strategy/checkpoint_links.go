package strategy

import (
	"context"
	"slices"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
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
			CommittedAt:   link.CommittedAt,
		})
	}
}

// CheckpointLinksWithPending returns state's CheckpointLinks with the links
// other repositories journaled since the session last folded (a detached
// link-session child or a subagent-end sweep can finish after the home's
// last turn end), for display. It never changes state: only the session's
// own hooks write it.
func CheckpointLinksWithPending(ctx context.Context, state *SessionState) []session.CheckpointLink {
	view := &SessionState{SessionID: state.SessionID, CheckpointLinks: make([]session.CheckpointLink, len(state.CheckpointLinks))}
	for i, link := range state.CheckpointLinks {
		link.LinkedCommits = slices.Clone(link.LinkedCommits)
		view.CheckpointLinks[i] = link
	}
	foldCrossRepoLinks(ctx, view)
	return view.CheckpointLinks
}

// recordHomeCheckpointLink folds pending cross-repository links, then adds
// the home checkpoint just written, so the list stays in recorded order.
// commitSHA is empty when no commit carries the checkpoint.
func recordHomeCheckpointLink(ctx context.Context, repo *git.Repository, state *SessionState, cpID id.CheckpointID, commitSHA string) {
	foldCrossRepoLinks(ctx, state)
	now := time.Now().UTC()
	link := session.CheckpointLink{CheckpointID: cpID, TurnID: state.TurnID, Kind: session.CheckpointLinkHome, At: now, CommittedAt: now}
	if commitSHA != "" {
		link.LinkedCommits = []string{commitSHA}
		if commit, err := repo.CommitObject(plumbing.NewHash(commitSHA)); err == nil {
			link.CommittedAt = commit.Committer.When.UTC()
		}
	}
	if root, err := paths.WorktreeRoot(ctx); err == nil {
		link.RepoKey = publicRepoKey(ctx, root) // the same naming as the cross-repository entries
	}
	state.AddCheckpointLink(link)
}
