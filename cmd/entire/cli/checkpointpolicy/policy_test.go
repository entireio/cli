package checkpointpolicy_test

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpointpolicy"
	"github.com/stretchr/testify/require"
)

func TestDefaultPolicy(t *testing.T) {
	t.Parallel()
	got := checkpointpolicy.DefaultPolicy()
	require.Equal(t, checkpointpolicy.DefaultCheckpointVersionSelector, got.CheckpointVersion)
	require.Equal(t, checkpointpolicy.DefaultCheckpointMinVersionRange, got.CheckpointMinVersion)
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
			in:   checkpointpolicy.Policy{CheckpointMinVersion: "^1.0.0"},
			want: checkpointpolicy.Policy{
				CheckpointVersion:    checkpointpolicy.DefaultCheckpointVersionSelector,
				CheckpointMinVersion: "^1.0.0",
			},
		},
		{
			name: "missing minimum",
			in:   checkpointpolicy.Policy{CheckpointVersion: "1.0"},
			want: checkpointpolicy.Policy{
				CheckpointVersion:    "1.0",
				CheckpointMinVersion: checkpointpolicy.DefaultCheckpointMinVersionRange,
			},
		},
		{
			name: "configured versions",
			in: checkpointpolicy.Policy{
				CheckpointVersion:    "1.0",
				CheckpointMinVersion: "^1.0.0",
			},
			want: checkpointpolicy.Policy{
				CheckpointVersion:    "1.0",
				CheckpointMinVersion: "^1.0.0",
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
		{name: "caret minimum", policy: checkpointpolicy.Policy{CheckpointVersion: "1", CheckpointMinVersion: "^1.0.0"}},
		{name: "exact version selector", policy: checkpointpolicy.Policy{CheckpointVersion: "1.0.0", CheckpointMinVersion: ">=1.0.0"}},
		{name: "legacy checkpoint version rejected", policy: checkpointpolicy.Policy{CheckpointVersion: "branch-v1", CheckpointMinVersion: ">=1.0.0"}, wantErr: `checkpoint_version "branch-v1" is not a valid SemVer selector`},
		{name: "version range rejected", policy: checkpointpolicy.Policy{CheckpointVersion: "^1.0.0", CheckpointMinVersion: ">=1.0.0"}, wantErr: `checkpoint_version "^1.0.0" is not a valid SemVer selector`},
		{name: "legacy minimum rejected", policy: checkpointpolicy.Policy{CheckpointVersion: "1", CheckpointMinVersion: "branch-v1"}, wantErr: `checkpoint_min_version "branch-v1" is not a valid SemVer constraint`},
		{name: "unreadable minimum", policy: checkpointpolicy.Policy{CheckpointVersion: "1", CheckpointMinVersion: ">=2.0.0"}, wantErr: `checkpoint_min_version ">=2.0.0" is not readable by this Entire CLI`},
		{name: "unsupported write selector", policy: checkpointpolicy.Policy{CheckpointVersion: "2", CheckpointMinVersion: ">=1.0.0"}, wantErr: `checkpoint_version "2" is not writable by this Entire CLI`},
		{name: "write version outside minimum", policy: checkpointpolicy.Policy{CheckpointVersion: "1", CheckpointMinVersion: "!=1.0.0"}, wantErr: `checkpoint_min_version "!=1.0.0" is not readable by this Entire CLI`},
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
