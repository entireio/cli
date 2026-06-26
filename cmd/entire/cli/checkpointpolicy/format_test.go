package checkpointpolicy_test

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpointpolicy"
	"github.com/stretchr/testify/require"
)

func TestParseFormat(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		input      string
		wantFamily checkpointpolicy.CheckpointFamily
		wantString string
		wantErr    string
	}{
		{name: "branch v1", input: "branch-v1", wantFamily: checkpointpolicy.CheckpointFamilyBranch, wantString: "branch-v1.0.0"},
		{name: "branch v1 minor", input: "branch-v1.2", wantFamily: checkpointpolicy.CheckpointFamilyBranch, wantString: "branch-v1.2.0"},
		{name: "branch v1 patch", input: "branch-v1.2.3", wantFamily: checkpointpolicy.CheckpointFamilyBranch, wantString: "branch-v1.2.3"},
		{name: "refs prerelease", input: "refs-v2.0.0-rc.1", wantFamily: checkpointpolicy.CheckpointFamilyRefs, wantString: "refs-v2.0.0-rc.1"},
		{name: "unknown family parses", input: "unknown-v1", wantFamily: "unknown", wantString: "unknown-v1.0.0"},
		{name: "missing v", input: "branch-1", wantErr: "invalid checkpoint version"},
		{name: "zero major", input: "branch-v0", wantErr: "invalid checkpoint version"},
		{name: "non numeric major", input: "branch-vx", wantErr: "invalid checkpoint version"},
		{name: "build metadata", input: "branch-v1.2.3+build.1", wantErr: "must not include build metadata"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := checkpointpolicy.ParseFormat(tt.input)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.wantFamily, got.Family)
			require.Equal(t, tt.wantString, got.String())
		})
	}
}

func TestCompareFormats(t *testing.T) {
	t.Parallel()

	branchV1, err := checkpointpolicy.ParseFormat(checkpoint.CheckpointVersionBranchV1)
	require.NoError(t, err)
	branchV1Canonical, err := checkpointpolicy.ParseFormat("branch-v1.0.0")
	require.NoError(t, err)
	branchV1Minor, err := checkpointpolicy.ParseFormat("branch-v1.1.0")
	require.NoError(t, err)
	branchV1Patch, err := checkpointpolicy.ParseFormat("branch-v1.1.1")
	require.NoError(t, err)
	branchV1Prerelease, err := checkpointpolicy.ParseFormat("branch-v1.1.1-rc.1")
	require.NoError(t, err)
	refsV1, err := checkpointpolicy.ParseFormat("refs-v1")
	require.NoError(t, err)
	unknownV1, err := checkpointpolicy.ParseFormat("unknown-v1")
	require.NoError(t, err)

	require.Zero(t, checkpointpolicy.Compare(branchV1, branchV1Canonical))
	require.Negative(t, checkpointpolicy.Compare(branchV1, branchV1Minor))
	require.Negative(t, checkpointpolicy.Compare(branchV1Minor, branchV1Patch))
	require.Negative(t, checkpointpolicy.Compare(branchV1Prerelease, branchV1Patch))
	require.Negative(t, checkpointpolicy.Compare(branchV1Patch, refsV1))
	require.Negative(t, checkpointpolicy.Compare(refsV1, unknownV1))
}

func TestSupportedFormats(t *testing.T) {
	t.Parallel()

	branchV1, err := checkpointpolicy.ParseFormat(checkpoint.CheckpointVersionBranchV1)
	require.NoError(t, err)
	branchV1Canonical, err := checkpointpolicy.ParseFormat("branch-v1.0.0")
	require.NoError(t, err)
	branchPrerelease, err := checkpointpolicy.ParseFormat("branch-v1.0.0-rc.1")
	require.NoError(t, err)
	refsV1, err := checkpointpolicy.ParseFormat("refs-v1")
	require.NoError(t, err)
	unknownV1, err := checkpointpolicy.ParseFormat("unknown-v1")
	require.NoError(t, err)

	require.True(t, checkpointpolicy.CanRead(branchV1))
	require.True(t, checkpointpolicy.CanRead(branchV1Canonical))
	require.True(t, checkpointpolicy.CanWrite(branchV1))
	require.True(t, checkpointpolicy.CanWrite(branchV1Canonical))
	require.Equal(t, "branch-v1.0.0", branchV1.String())

	require.False(t, checkpointpolicy.CanRead(branchPrerelease))
	require.False(t, checkpointpolicy.CanWrite(branchPrerelease))

	require.False(t, checkpointpolicy.CanRead(refsV1))
	require.False(t, checkpointpolicy.CanWrite(refsV1))

	require.False(t, checkpointpolicy.CanRead(unknownV1))
	require.False(t, checkpointpolicy.CanWrite(unknownV1))
}
