package checkpoint

import (
	"fmt"
	"slices"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

// MergeCommitLinks validates and unions links without dropping older associations.
func MergeCommitLinks(existing, added []LinkedCommit) ([]LinkedCommit, error) {
	out := slices.Clone(existing)
	for _, link := range added {
		if (len(link.SHA) != 40 && len(link.SHA) != 64) || strings.Trim(link.SHA, "0123456789abcdef") != "" || strings.Trim(link.SHA, "0") == "" {
			return nil, fmt.Errorf("invalid linked commit SHA %q", link.SHA)
		}
		if !slices.Contains(out, link) {
			out = append(out, link)
		}
	}
	return out, nil
}

// CheckpointsForCommit unions trailer IDs with checkpoint-side links. Trailer
// order is preserved; a commit with fresh work can also link older checkpoints.
func CheckpointsForCommit(infos []CheckpointInfo, sha string, trailers []id.CheckpointID) []id.CheckpointID {
	return NewCommitLinkIndex(infos).Resolve(sha, trailers)
}

// CommitLinkIndex avoids scanning every checkpoint for each commit or blame line.
type CommitLinkIndex map[string][]id.CheckpointID

func NewCommitLinkIndex(infos []CheckpointInfo) CommitLinkIndex {
	index := make(CommitLinkIndex)
	for _, info := range infos {
		for _, link := range info.LinkedCommits {
			if !info.CheckpointID.IsEmpty() && !slices.Contains(index[link.SHA], info.CheckpointID) {
				index[link.SHA] = append(index[link.SHA], info.CheckpointID)
			}
		}
	}
	return index
}

func (index CommitLinkIndex) Resolve(sha string, trailers []id.CheckpointID) []id.CheckpointID {
	out := slices.Clone(trailers)
	for _, cid := range index[sha] {
		if !slices.Contains(out, cid) {
			out = append(out, cid)
		}
	}
	return out
}
