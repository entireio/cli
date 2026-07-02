package checkpointpolicy_test

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpointpolicy"
	"github.com/stretchr/testify/require"
)

func TestDefaultPolicy(t *testing.T) {
	t.Parallel()
	got := checkpointpolicy.DefaultPolicy()
	require.Equal(t, checkpoint.CheckpointVersionBranchV1, got.CheckpointVersion)
	require.Equal(t, checkpoint.CheckpointVersionBranchV1, got.CheckpointMinVersion)
}

func TestNormalize(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   checkpointpolicy.Policy
		want checkpointpolicy.Policy
	}{
		{
			name: "default",
			in:   checkpointpolicy.DefaultPolicy(),
			want: checkpointpolicy.DefaultPolicy(),
		},
		{
			name: "missing version",
			in:   checkpointpolicy.Policy{CheckpointMinVersion: checkpoint.CheckpointVersionBranchV1},
			want: checkpointpolicy.DefaultPolicy(),
		},
		{
			name: "configured versions",
			in: checkpointpolicy.Policy{
				CheckpointVersion:    "refs-v1",
				CheckpointMinVersion: checkpoint.CheckpointVersionBranchV1,
			},
			want: checkpointpolicy.Policy{
				CheckpointVersion:    "refs-v1",
				CheckpointMinVersion: checkpoint.CheckpointVersionBranchV1,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := checkpointpolicy.Normalize(tt.in)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestValidatePolicy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		policy  checkpointpolicy.Policy
		wantErr string
	}{
		{name: "default", policy: checkpointpolicy.DefaultPolicy()},
		{name: "unknown current", policy: checkpointpolicy.Policy{CheckpointVersion: "future-v1", CheckpointMinVersion: "branch-v1"}, wantErr: `checkpoint_version "future-v1" is not supported by this Entire CLI`},
		{name: "unsupported current", policy: checkpointpolicy.Policy{CheckpointVersion: "branch-v2342", CheckpointMinVersion: "branch-v1"}, wantErr: `checkpoint_version "branch-v2342" is not supported by this Entire CLI`},
		{name: "unsupported minimum", policy: checkpointpolicy.Policy{CheckpointVersion: "branch-v1", CheckpointMinVersion: "refs-v2"}, wantErr: `checkpoint_min_version "refs-v2" is not supported by this Entire CLI`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := checkpointpolicy.ValidatePolicy(tt.policy)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}
