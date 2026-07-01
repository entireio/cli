package checkpointpolicy_test

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpointpolicy"
	"github.com/stretchr/testify/require"
)

const unsupportedCheckpointVersionExpr = ">=2.0.0"

func TestDefaultPolicy(t *testing.T) {
	t.Parallel()
	got := checkpointpolicy.DefaultPolicy()
	require.Equal(t, checkpointpolicy.DefaultCheckpointVersionSelector, got.CheckpointVersion)
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
			in:   checkpointpolicy.Policy{},
			want: checkpointpolicy.Policy{
				CheckpointVersion: checkpointpolicy.DefaultCheckpointVersionSelector,
			},
		},
		{
			name: "configured version",
			in:   checkpointpolicy.Policy{CheckpointVersion: "1.0"},
			want: checkpointpolicy.Policy{
				CheckpointVersion: "1.0",
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
		{name: "caret version range", policy: checkpointpolicy.Policy{CheckpointVersion: "^1.0.0"}},
		{name: "comparator version range", policy: checkpointpolicy.Policy{CheckpointVersion: ">=1.0.0"}},
		{name: "legacy checkpoint version rejected", policy: checkpointpolicy.Policy{CheckpointVersion: "branch-v1"}, wantErr: `checkpoint_version "branch-v1" is not a valid SemVer constraint`},
		{name: "unsupported write selector", policy: checkpointpolicy.Policy{CheckpointVersion: unsupportedCheckpointVersionExpr}, wantErr: `checkpoint_version "` + unsupportedCheckpointVersionExpr + `" is not writable by this Entire CLI`},
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
