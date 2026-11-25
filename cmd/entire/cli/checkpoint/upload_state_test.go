package checkpoint

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUploadCoordinator_RequestMerge(t *testing.T) {
	t.Parallel()
	coord := NewUploadCoordinator(t.TempDir())

	require.NoError(t, coord.Request(UploadRequest{Remote: "origin", OPFDecision: UploadOPFRun, PendingCapture: "origin"}))
	require.NoError(t, coord.Request(UploadRequest{Remote: "origin", OPFDecision: UploadOPFUnset}))

	req, err := coord.TakeRequest()
	require.NoError(t, err)
	require.NotNil(t, req)
	assert.Equal(t, UploadOPFRun, req.OPFDecision, "a later push that resolved nothing must not drop an earlier run")
	assert.Equal(t, "origin", req.PendingCapture, "an unsatisfied capture for the same remote is kept")

	req, err = coord.TakeRequest()
	require.NoError(t, err)
	assert.Nil(t, req, "taking consumes the request")
}

func TestUploadCoordinator_RequestToOtherRemoteDropsCapture(t *testing.T) {
	t.Parallel()
	coord := NewUploadCoordinator(t.TempDir())
	require.NoError(t, coord.Request(UploadRequest{Remote: "origin", PendingCapture: "origin"}))
	require.NoError(t, coord.Request(UploadRequest{Remote: "fork"}))

	req, err := coord.TakeRequest()
	require.NoError(t, err)
	require.NotNil(t, req)
	assert.Equal(t, "fork", req.Remote)
	assert.Empty(t, req.PendingCapture)
}
