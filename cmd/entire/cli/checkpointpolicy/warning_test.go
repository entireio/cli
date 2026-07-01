package checkpointpolicy_test

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpointpolicy"
	"github.com/stretchr/testify/require"
)

func TestHasUnsupportedCheckpointVersion(t *testing.T) {
	t.Parallel()

	require.False(t, checkpointpolicy.HasUnsupportedCheckpointVersion(checkpointpolicy.DefaultPolicy()))
	require.True(t, checkpointpolicy.HasUnsupportedCheckpointVersion(checkpointpolicy.Policy{
		CheckpointVersion: unsupportedCheckpointVersionExpr,
	}))
	require.True(t, checkpointpolicy.HasUnsupportedCheckpointVersion(checkpointpolicy.Policy{
		CheckpointVersion: "branch-v1",
	}))
}

func TestCanSatisfyPolicy(t *testing.T) {
	t.Parallel()

	require.True(t, checkpointpolicy.CanSatisfyPolicy(checkpointpolicy.DefaultPolicy()))
	require.True(t, checkpointpolicy.CanSatisfyPolicy(checkpointpolicy.Policy{}))
	require.False(t, checkpointpolicy.CanSatisfyPolicy(checkpointpolicy.Policy{
		CheckpointVersion: unsupportedCheckpointVersionExpr,
	}))
}

func TestUnsupportedPolicyMessageIncludesSettingDetails(t *testing.T) {
	t.Parallel()

	got := checkpointpolicy.UnsupportedPolicyMessage(checkpointpolicy.Policy{
		CheckpointVersion: unsupportedCheckpointVersionExpr,
	}, "brew upgrade entire")

	require.Contains(t, got, "[entire] This repository requires checkpoint support newer than this Entire CLI.")
	require.Contains(t, got, "[entire]   brew upgrade entire")
	require.Contains(t, got, `checkpoint_version "`+unsupportedCheckpointVersionExpr+`" is not supported by this Entire CLI`)
}

func TestUnsupportedPolicyMessageEmptyForSatisfiedPolicy(t *testing.T) {
	t.Parallel()

	require.Empty(t, checkpointpolicy.UnsupportedPolicyMessage(checkpointpolicy.DefaultPolicy(), "brew upgrade entire"))
}
