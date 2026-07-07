package checkpointpolicy_test

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpointpolicy"
	"github.com/stretchr/testify/require"
)

func TestParseFormat(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		input   string
		want    checkpointpolicy.CheckpointFormat
		wantErr string
	}{
		{name: "branch v1", input: "branch-v1", want: checkpointpolicy.CheckpointFormat{Family: checkpointpolicy.CheckpointFamilyBranch, Major: 1}},
		{name: "refs v2", input: "refs-v2", want: checkpointpolicy.CheckpointFormat{Family: checkpointpolicy.CheckpointFamilyRefs, Major: 2}},
		{name: "unknown family parses", input: "unknown-v1", want: checkpointpolicy.CheckpointFormat{Family: "unknown", Major: 1}},
		{name: "missing v", input: "branch-1", wantErr: "invalid checkpoint format"},
		{name: "zero major", input: "branch-v0", wantErr: "invalid checkpoint major"},
		{name: "non numeric major", input: "branch-vx", wantErr: "invalid checkpoint major"},
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
			require.Equal(t, tt.want, got)
			require.Equal(t, tt.input, got.String())
		})
	}
}

func TestSupportedFormats(t *testing.T) {
	t.Parallel()

	branchV1, err := checkpointpolicy.ParseFormat(checkpointpolicy.CheckpointVersionBranchV1)
	require.NoError(t, err)
	refsV1, err := checkpointpolicy.ParseFormat("refs-v1")
	require.NoError(t, err)
	unknownV1, err := checkpointpolicy.ParseFormat("unknown-v1")
	require.NoError(t, err)

	require.True(t, checkpointpolicy.CanRead(branchV1))
	require.True(t, checkpointpolicy.CanWrite(branchV1))
	require.Equal(t, checkpointpolicy.CheckpointVersionBranchV1, branchV1.String())

	// refs-v1 is the git-refs store format: read- and write-supported.
	require.True(t, checkpointpolicy.CanRead(refsV1))
	require.True(t, checkpointpolicy.CanWrite(refsV1))
	require.Negative(t, checkpointpolicy.Compare(branchV1, refsV1))

	require.False(t, checkpointpolicy.CanRead(unknownV1))
	require.False(t, checkpointpolicy.CanWrite(unknownV1))
	require.Negative(t, checkpointpolicy.Compare(refsV1, unknownV1))
}
