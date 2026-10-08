package checkpoint

import (
	apicheckpoint "github.com/entireio/cli/api/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

func commitLinksUpdate(links []LinkedCommit) func(*CheckpointSummary) error {
	return func(summary *CheckpointSummary) error {
		merged, err := apicheckpoint.MergeCommitLinks(summary.LinkedCommits, links)
		if err != nil {
			return err //nolint:wrapcheck // Shared contract validation.
		}
		summary.LinkedCommits = merged
		return nil
	}
}

// CommitLinkIndex is the reusable reverse lookup for checkpoint-side links.
type CommitLinkIndex = apicheckpoint.CommitLinkIndex

func NewCommitLinkIndex(infos []CheckpointInfo) CommitLinkIndex {
	return apicheckpoint.NewCommitLinkIndex(infos)
}

// CheckpointsForCommit resolves both trailer and checkpoint-side associations.
func CheckpointsForCommit(infos []CheckpointInfo, sha string, trailers []id.CheckpointID) []id.CheckpointID {
	return apicheckpoint.CheckpointsForCommit(infos, sha, trailers)
}
