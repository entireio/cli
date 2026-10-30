package strategy

import (
	"errors"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"
)

func TestCheckpointPushOutcomes(t *testing.T) {
	t.Parallel()
	const (
		pushedRef  = plumbing.ReferenceName("refs/entire/checkpoints/aa/aa0000000001")
		waitingRef = plumbing.ReferenceName("refs/entire/checkpoints/bb/bb0000000001")
		absentRef  = plumbing.ReferenceName("refs/entire/checkpoints/cc/cc0000000001")
	)
	tip := plumbing.NewHash("1111111111111111111111111111111111111111")
	drainErr := errors.New("drain push queue: disk full")
	waiting := map[plumbing.ReferenceName]struct{}{pushedRef: {}, waitingRef: {}}
	targets := []CheckpointPushRequest{{Ref: pushedRef}, {Ref: waitingRef}, {Ref: absentRef}}

	tests := []struct {
		name     string
		result   checkpointFlushResult
		flushErr error
		want     []CheckpointPushStatus
	}{
		{
			name:   "a waiting ref the flush did not report left the queue meanwhile",
			result: checkpointFlushResult{pushed: []checkpointRefPush{{name: pushedRef, hash: tip}}},
			want:   []CheckpointPushStatus{CheckpointPushUploaded, CheckpointPushNotWaiting, CheckpointPushNotWaiting},
		},
		{
			name:     "a flush-wide error fails every waiting ref it did not report",
			result:   checkpointFlushResult{pushed: []checkpointRefPush{{name: pushedRef, hash: tip}}},
			flushErr: drainErr,
			want:     []CheckpointPushStatus{CheckpointPushUploaded, CheckpointPushFailed, CheckpointPushNotWaiting},
		},
		{
			name:     "a flush that failed before reporting anything",
			flushErr: drainErr,
			want:     []CheckpointPushStatus{CheckpointPushFailed, CheckpointPushFailed, CheckpointPushNotWaiting},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			outcomes := checkpointPushOutcomes(targets, waiting, nil, tt.result, tt.flushErr)
			got := make([]CheckpointPushStatus, 0, len(outcomes))
			for _, outcome := range outcomes {
				got = append(got, outcome.Status)
				if outcome.Status == CheckpointPushFailed {
					require.ErrorIs(t, outcome.Err, drainErr)
				}
			}
			require.Equal(t, tt.want, got)
		})
	}
}

// The OPF decision is made from the refs waiting when the command looked. A
// named ref queued after that must not be sent, or it would skip OPF.
func TestWaitingTargetSelection_LeavesOutRefsNotWaiting(t *testing.T) {
	t.Parallel()
	const (
		waitingRef = plumbing.ReferenceName("refs/entire/checkpoints/aa/aa0000000001")
		laterRef   = plumbing.ReferenceName("refs/entire/checkpoints/bb/bb0000000001")
	)
	pin := plumbing.NewHash("1111111111111111111111111111111111111111")
	waiting := map[plumbing.ReferenceName]struct{}{waitingRef: {}}

	selection, err := waitingTargetSelection([]CheckpointPushRequest{{Ref: waitingRef, Commit: pin}, {Ref: laterRef}}, waiting)
	require.NoError(t, err)

	got, ok := selection.pin(waitingRef)
	require.True(t, ok)
	require.Equal(t, pin, got)
	_, ok = selection.pin(laterRef)
	require.False(t, ok, "a ref that was not waiting is not sent")

	none, err := waitingTargetSelection([]CheckpointPushRequest{{Ref: laterRef}}, waiting)
	require.NoError(t, err)
	_, ok = none.pin(waitingRef)
	require.False(t, ok, "selecting nothing must not fall back to selecting every ref")
}

func TestWaitingTargetSelection_RefusesTwoCommitsForOneRef(t *testing.T) {
	t.Parallel()
	const ref = plumbing.ReferenceName("refs/entire/checkpoints/aa/aa0000000001")
	_, err := waitingTargetSelection([]CheckpointPushRequest{
		{Ref: ref, Commit: plumbing.NewHash("1111111111111111111111111111111111111111")},
		{Ref: ref, Commit: plumbing.NewHash("2222222222222222222222222222222222222222")},
	}, map[plumbing.ReferenceName]struct{}{ref: {}})
	require.Error(t, err)
}
