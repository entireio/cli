package checkpoint

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/stretchr/testify/assert"
)

type linkedSummaryReader struct {
	summaries map[id.CheckpointID]*CheckpointSummary
	reads     []id.CheckpointID
}

func (r *linkedSummaryReader) Read(_ context.Context, cid id.CheckpointID) (*CheckpointSummary, error) {
	r.reads = append(r.reads, cid)
	if s, ok := r.summaries[cid]; ok {
		return s, nil
	}
	return nil, errors.New("not found")
}

// Stubs minted around or after the commit are read for their links; older ones
// predate the commit and are never read.
func TestCheckpointsLinkedToWithStubs(t *testing.T) {
	t.Parallel()
	sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	committedAt := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	link := []LinkedCommit{{SHA: sha}}

	local := id.CheckpointID("111111111111")
	newStub := id.CheckpointID("222222222222")
	oldStub := id.CheckpointID("333333333333")
	unreadable := id.CheckpointID("444444444444")
	reader := &linkedSummaryReader{summaries: map[id.CheckpointID]*CheckpointSummary{
		newStub: {LinkedCommits: link},
		oldStub: {LinkedCommits: link},
	}}
	infos := []CheckpointInfo{
		{CheckpointID: local, LinkedCommits: link},
		{CheckpointID: newStub, ListedStub: true, CreatedAt: committedAt.Add(-time.Hour)},
		{CheckpointID: oldStub, ListedStub: true, CreatedAt: committedAt.Add(-48 * time.Hour)},
		{CheckpointID: unreadable, ListedStub: true, CreatedAt: committedAt.Add(time.Hour)},
	}

	got := CheckpointsLinkedToWithStubs(context.Background(), reader, infos, sha, committedAt)
	assert.Equal(t, []id.CheckpointID{local, newStub}, got)
	assert.Equal(t, []id.CheckpointID{newStub, unreadable}, reader.reads)
}
