package checkpointpolicy_test

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpointpolicy"
	"github.com/stretchr/testify/require"
)

func TestRequiresUpgrade(t *testing.T) {
	t.Parallel()

	require.False(t, checkpointpolicy.RequiresUpgrade(checkpointpolicy.DefaultPolicy()))
	require.True(t, checkpointpolicy.RequiresUpgrade(checkpointpolicy.Policy{
		CheckpointVersion:    checkpoint.CheckpointVersionBranchV1,
		CheckpointMinVersion: "refs-v2",
	}))
	require.True(t, checkpointpolicy.RequiresUpgrade(checkpointpolicy.Policy{
		CheckpointVersion:    checkpoint.CheckpointVersionBranchV1,
		CheckpointMinVersion: "invalid",
	}))
}

func TestUnsupportedWrite(t *testing.T) {
	t.Parallel()

	require.False(t, checkpointpolicy.UnsupportedWrite(checkpointpolicy.DefaultPolicy()))
	require.True(t, checkpointpolicy.UnsupportedWrite(checkpointpolicy.Policy{
		CheckpointVersion:    "refs-v2",
		CheckpointMinVersion: checkpoint.CheckpointVersionBranchV1,
	}))
	require.True(t, checkpointpolicy.UnsupportedWrite(checkpointpolicy.Policy{
		CheckpointVersion:    "invalid",
		CheckpointMinVersion: checkpoint.CheckpointVersionBranchV1,
	}))
}

func TestCanSatisfyPolicy(t *testing.T) {
	t.Parallel()

	require.True(t, checkpointpolicy.CanSatisfyPolicy(checkpointpolicy.DefaultPolicy()))
	require.True(t, checkpointpolicy.CanSatisfyPolicy(checkpointpolicy.Policy{}))
	require.False(t, checkpointpolicy.CanSatisfyPolicy(checkpointpolicy.Policy{
		CheckpointVersion:    "refs-v2",
		CheckpointMinVersion: checkpoint.CheckpointVersionBranchV1,
	}))
	require.False(t, checkpointpolicy.CanSatisfyPolicy(checkpointpolicy.Policy{
		CheckpointVersion:    checkpoint.CheckpointVersionBranchV1,
		CheckpointMinVersion: "refs-v2",
	}))
}

func TestUnsupportedPolicyMessageIncludesSettingDetails(t *testing.T) {
	t.Parallel()

	got := checkpointpolicy.UnsupportedPolicyMessage(checkpointpolicy.Policy{
		CheckpointVersion:    "refs-v2",
		CheckpointMinVersion: "refs-v2",
	}, "brew upgrade entire")

	require.Contains(t, got, "[entire] This repository requires checkpoint support newer than this Entire CLI.")
	require.Contains(t, got, "[entire]   brew upgrade entire")
	require.Contains(t, got, `checkpoint_version "refs-v2" is not writable by this Entire CLI`)
	require.Contains(t, got, `checkpoint_min_version "refs-v2" is not readable by this Entire CLI`)
}

func TestUnsupportedPolicyMessageEmptyForSatisfiedPolicy(t *testing.T) {
	t.Parallel()

	require.Empty(t, checkpointpolicy.UnsupportedPolicyMessage(checkpointpolicy.DefaultPolicy(), "brew upgrade entire"))
}
