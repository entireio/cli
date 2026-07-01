package checkpointpolicy_test

import (
	"testing"

	semver "github.com/Masterminds/semver/v3"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpointpolicy"
	"github.com/stretchr/testify/require"
)

func TestParseCheckpointVersionSelector(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		input     string
		matches   []string
		rejects   []string
		wantError string
	}{
		{name: "major", input: "1", matches: []string{"1.0.0", "1.4.2"}, rejects: []string{"2.0.0"}},
		{name: "major minor", input: "1.0", matches: []string{"1.0.0", "1.0.4"}, rejects: []string{"1.1.0"}},
		{name: "major minor patch", input: "1.0.0", matches: []string{"1.0.0"}, rejects: []string{"1.0.1"}},
		{name: "caret", input: "^1.0.0", matches: []string{"1.0.0", "1.4.2"}, rejects: []string{"2.0.0"}},
		{name: "comparator", input: ">=1.0.0", matches: []string{"1.0.0", "2.0.0"}, rejects: []string{"0.9.9"}},
		{name: "wildcard", input: "*", matches: []string{"1.0.0", "2.0.0"}},
		{name: "legacy metadata alias rejected", input: checkpoint.CheckpointVersionBranchV1, wantError: "not a valid SemVer constraint"},
		{name: "empty rejected", input: "", wantError: "not a valid SemVer constraint"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := checkpointpolicy.ParseCheckpointVersionSelector(tt.input)
			if tt.wantError != "" {
				require.ErrorContains(t, err, tt.wantError)
				return
			}
			require.NoError(t, err)
			for _, version := range tt.matches {
				require.True(t, got.Check(semver.MustParse(version)), "expected %q to match %q", tt.input, version)
			}
			for _, version := range tt.rejects {
				require.False(t, got.Check(semver.MustParse(version)), "expected %q to reject %q", tt.input, version)
			}
		})
	}
}

func TestSupportedLogicalVersions(t *testing.T) {
	t.Parallel()

	require.True(t, checkpointpolicy.IsSupportedCheckpointVersion(checkpointpolicy.LogicalCheckpointVersionV1))
	require.False(t, checkpointpolicy.IsSupportedCheckpointVersion("2.0.0"))
}

func TestValidateCheckpointMetadataVersion(t *testing.T) {
	t.Parallel()

	require.NoError(t, checkpointpolicy.ValidateCheckpointMetadataVersion("abc123", checkpoint.CheckpointVersionBranchV1))

	err := checkpointpolicy.ValidateCheckpointMetadataVersion("abc123", "2.0.0")
	require.ErrorContains(t, err, `checkpoint abc123 uses unsupported checkpoint_version "2.0.0"`)
	require.True(t, checkpointpolicy.IsUnsupportedCheckpointVersionError(err))

	err = checkpointpolicy.ValidateCheckpointMetadataVersion("abc123", "refs-v1")
	require.ErrorContains(t, err, `checkpoint abc123 has invalid checkpoint_version "refs-v1"`)
	require.False(t, checkpointpolicy.IsUnsupportedCheckpointVersionError(err))
}

func TestMetadataVersionMapping(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr string
	}{
		{name: "legacy empty", input: "", want: checkpointpolicy.LogicalCheckpointVersionV1},
		{name: "legacy branch v1", input: checkpoint.CheckpointVersionBranchV1, want: checkpointpolicy.LogicalCheckpointVersionV1},
		{name: "logical v1", input: checkpointpolicy.LogicalCheckpointVersionV1, want: checkpointpolicy.LogicalCheckpointVersionV1},
		{name: "future semver", input: "2.0.0", want: "2.0.0"},
		{name: "old refs family invalid", input: "refs-v1", wantErr: "invalid checkpoint_version"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := checkpointpolicy.LogicalVersionForCheckpointMetadata(tt.input)
			if tt.wantErr == "" {
				require.NoError(t, err)
				require.Equal(t, tt.want, got)
				return
			}
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestMetadataVersionForWriteVersion(t *testing.T) {
	t.Parallel()

	got, err := checkpointpolicy.MetadataVersionForWriteVersion(checkpointpolicy.LogicalCheckpointVersionV1)
	require.NoError(t, err)
	require.Equal(t, checkpoint.CheckpointVersionBranchV1, got)

	_, err = checkpointpolicy.MetadataVersionForWriteVersion("2.0.0")
	require.ErrorContains(t, err, `checkpoint_version "2.0.0" is not writable by this Entire CLI`)
}
