package checkpointpolicy_test

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpointpolicy"
	"github.com/stretchr/testify/require"
)

func TestUpgradeWarning(t *testing.T) {
	t.Parallel()

	got := checkpointpolicy.UpgradeWarning("brew upgrade entire")

	require.Contains(t, got, "[entire] This repository requires checkpoint support newer than this Entire CLI.")
	require.Contains(t, got, "[entire] Upgrade Entire, then rerun the command:")
	require.Contains(t, got, "[entire]   brew upgrade entire")
}

func TestEnsureCanReadVersion(t *testing.T) {
	t.Parallel()

	require.NoError(t, checkpointpolicy.EnsureCanReadVersion("abc123", checkpoint.CheckpointVersionBranchV1))
	require.NoError(t, checkpointpolicy.EnsureCanReadVersion("abc123", ""))

	err := checkpointpolicy.EnsureCanReadVersion("abc123", "refs-v1")
	require.ErrorContains(t, err, `checkpoint abc123 uses unsupported checkpoint_version "refs-v1"`)
	require.ErrorContains(t, err, "not read-supported")
	require.True(t, checkpointpolicy.IsUnsupportedVersion(err))
}

func TestEnsureCanReadVersionParseErrorIsNotUnsupportedVersion(t *testing.T) {
	t.Parallel()

	err := checkpointpolicy.EnsureCanReadVersion("abc123", "not-a-format")

	require.ErrorContains(t, err, `checkpoint abc123 has invalid checkpoint_version "not-a-format"`)
	require.False(t, checkpointpolicy.IsUnsupportedVersion(err))
}
