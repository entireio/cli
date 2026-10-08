package cli

import (
	"context"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

type commitLinksAttributionReader struct {
	attributionCheckpointReaderStub

	err   error
	infos []checkpoint.CheckpointInfo
	lists int
}

func (r *commitLinksAttributionReader) List(context.Context) ([]checkpoint.CheckpointInfo, error) {
	r.lists++
	return r.infos, r.err
}

func TestAttributionCommitLinks_UnionAndSingleListing(t *testing.T) {
	t.Parallel()
	sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	old, fresh := id.MustCheckpointID("abc123def456"), id.MustCheckpointID("111111222222")
	reader := &commitLinksAttributionReader{infos: []checkpoint.CheckpointInfo{{CheckpointID: old, LinkedCommits: []checkpoint.LinkedCommit{{SHA: sha}}}, {CheckpointID: fresh, LinkedCommits: []checkpoint.LinkedCommit{{SHA: sha}}}}}
	resolver := &attributionResolver{ctx: context.Background(), store: reader}
	commit := &object.Commit{Hash: plumbing.NewHash(sha), Message: "new work\n\nEntire-Checkpoint: " + fresh.String() + "\n"}
	for range 2 {
		require.Equal(t, []id.CheckpointID{fresh, old}, resolver.checkpointsForCommit(commit))
	}
	require.Equal(t, 1, reader.lists, "blame must not scan the checkpoint store once per line")
	commit.Message = "renamed\n"
	require.Equal(t, []id.CheckpointID{old, fresh}, resolver.checkpointsForCommit(commit))
}
