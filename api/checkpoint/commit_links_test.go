package checkpoint

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

func TestCommitLinks_Union(t *testing.T) {
	t.Parallel()
	sha := strings.Repeat("a", 40)
	a, b := id.MustCheckpointID("abc123def456"), id.MustCheckpointID("111111222222")
	infos := []CheckpointInfo{{CheckpointID: a, LinkedCommits: []LinkedCommit{{SHA: sha}}}, {CheckpointID: b, LinkedCommits: []LinkedCommit{{SHA: sha}, {SHA: sha}}}}
	require.Equal(t, []id.CheckpointID{a, b}, CheckpointsForCommit(infos, sha, []id.CheckpointID{a}))
	require.Equal(t, []id.CheckpointID{a, b}, CheckpointsForCommit(infos, sha, nil))
	require.Equal(t, []id.CheckpointID{b}, CheckpointsForCommit(infos, strings.Repeat("b", 40), []id.CheckpointID{b}))
}

func TestMergeCommitLinks_Validation(t *testing.T) {
	t.Parallel()
	for _, sha := range []string{"bad", strings.Repeat("0", 40), strings.Repeat("A", 40), strings.Repeat("x", 64)} {
		t.Run(sha, func(t *testing.T) {
			t.Parallel()
			_, err := MergeCommitLinks(nil, []LinkedCommit{{SHA: sha}})
			require.Error(t, err)
		})
	}
	links := []LinkedCommit{{SHA: strings.Repeat("a", 40)}, {SHA: strings.Repeat("b", 64), Repo: "gh/owner/repo"}}
	merged, err := MergeCommitLinks(links[:1], links)
	require.NoError(t, err)
	require.Equal(t, links, merged)
}
