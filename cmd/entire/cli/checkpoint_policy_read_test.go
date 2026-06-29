package cli

import (
	"context"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/stretchr/testify/require"
)

func TestReadCheckpointInfoFromStoreRejectsUnsupportedCheckpointVersion(t *testing.T) {
	t.Parallel()

	cpID := id.MustCheckpointID("111111111111")
	_, err := readCheckpointInfoFromStore(context.Background(), checkpointInfoPolicyStub{
		summary: &checkpoint.CheckpointSummary{
			CheckpointID:      cpID,
			CheckpointVersion: "refs-v1",
		},
	}, cpID)

	require.EqualError(t, err, `checkpoint 111111111111 uses unsupported checkpoint_version "refs-v1": not read-supported by this Entire CLI`)
}

type checkpointInfoPolicyStub struct {
	summary *checkpoint.CheckpointSummary
}

func (s checkpointInfoPolicyStub) Read(context.Context, id.CheckpointID) (*checkpoint.CheckpointSummary, error) {
	return s.summary, nil
}

func (s checkpointInfoPolicyStub) List(context.Context) ([]checkpoint.CheckpointInfo, error) {
	return nil, nil
}

func (s checkpointInfoPolicyStub) ReadSessionContent(context.Context, id.CheckpointID, int) (*checkpoint.SessionContent, error) {
	return nil, checkpoint.ErrCheckpointNotFound
}

func (s checkpointInfoPolicyStub) ReadSessionMetadata(context.Context, id.CheckpointID, int) (*checkpoint.Metadata, error) {
	return nil, checkpoint.ErrCheckpointNotFound
}
