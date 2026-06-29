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
		CheckpointMinVersion: "refs-v1",
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
		CheckpointVersion:    "refs-v1",
		CheckpointMinVersion: checkpoint.CheckpointVersionBranchV1,
	}))
	require.True(t, checkpointpolicy.UnsupportedWrite(checkpointpolicy.Policy{
		CheckpointVersion:    "invalid",
		CheckpointMinVersion: checkpoint.CheckpointVersionBranchV1,
	}))
}

func TestUpgradeWarning(t *testing.T) {
	t.Parallel()

	got := checkpointpolicy.UpgradeWarning("brew upgrade entire")

	require.Contains(t, got, "[entire] This repository requires checkpoint support newer than this Entire CLI.")
	require.Contains(t, got, "[entire] Upgrade Entire, then rerun the command:")
	require.Contains(t, got, "[entire]   brew upgrade entire")
}
