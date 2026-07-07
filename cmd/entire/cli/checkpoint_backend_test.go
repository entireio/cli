package cli

import (
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

func TestResolveCheckpointBackendType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "branch", want: checkpoint.BackendTypeGitBranch},
		{in: "refs", want: checkpoint.BackendTypeGitRefs},
		{in: "git-branch", want: checkpoint.BackendTypeGitBranch},
		{in: "git-refs", want: checkpoint.BackendTypeGitRefs},
		{in: "  REFS  ", want: checkpoint.BackendTypeGitRefs}, // trimmed + case-insensitive
		{in: "Branch", want: checkpoint.BackendTypeGitBranch},
		{in: "", wantErr: true},
		{in: "bogus", wantErr: true},
	}
	for _, tc := range tests {
		got, err := resolveCheckpointBackendType(tc.in)
		if tc.wantErr {
			require.Error(t, err, "input %q", tc.in)
			continue
		}
		require.NoError(t, err, "input %q", tc.in)
		assert.Equal(t, tc.want, got, "input %q", tc.in)
	}
}

func TestApplyCheckpointBackend_SetsPrimaryFromNil(t *testing.T) {
	t.Parallel()

	s := &EntireSettings{}
	applyCheckpointBackend(s, checkpoint.BackendTypeGitRefs)

	require.NotNil(t, s.Checkpoints)
	assert.Equal(t, checkpoint.BackendTypeGitRefs, s.Checkpoints.Primary.Type)
	assert.Empty(t, s.Checkpoints.Mirrors)
}

func TestApplyCheckpointBackend_PreservesUnrelatedMirror(t *testing.T) {
	t.Parallel()

	s := &EntireSettings{Checkpoints: &settings.CheckpointsConfig{
		Primary: settings.BackendConfig{Type: checkpoint.BackendTypeGitBranch},
		Mirrors: []settings.BackendConfig{{Type: "fs"}},
	}}
	applyCheckpointBackend(s, checkpoint.BackendTypeGitRefs)

	assert.Equal(t, checkpoint.BackendTypeGitRefs, s.Checkpoints.Primary.Type)
	require.Len(t, s.Checkpoints.Mirrors, 1)
	assert.Equal(t, "fs", s.Checkpoints.Mirrors[0].Type)
}

func TestApplyCheckpointBackend_DropsCollidingMirror(t *testing.T) {
	t.Parallel()

	// A git-refs mirror alongside a git-branch primary is valid; promoting the
	// primary to git-refs would collide (one-of-each-type), so the mirror is dropped.
	s := &EntireSettings{Checkpoints: &settings.CheckpointsConfig{
		Primary: settings.BackendConfig{Type: checkpoint.BackendTypeGitBranch},
		Mirrors: []settings.BackendConfig{{Type: checkpoint.BackendTypeGitRefs}},
	}}
	applyCheckpointBackend(s, checkpoint.BackendTypeGitRefs)

	assert.Equal(t, checkpoint.BackendTypeGitRefs, s.Checkpoints.Primary.Type)
	assert.Empty(t, s.Checkpoints.Mirrors, "mirror colliding with the new primary must be dropped")
}

func TestApplyCheckpointBackendFlag_EmptyIsNoOp(t *testing.T) {
	t.Parallel()

	s := &EntireSettings{}
	require.NoError(t, applyCheckpointBackendFlag(s, ""))
	assert.Nil(t, s.Checkpoints, "empty flag must not write a checkpoints block")
}

func TestApplyCheckpointBackendFlag_Invalid(t *testing.T) {
	t.Parallel()

	s := &EntireSettings{}
	err := applyCheckpointBackendFlag(s, "bogus")
	require.Error(t, err)
	assert.Contains(t, err.Error(), flagCheckpointBackend)
	assert.Nil(t, s.Checkpoints)
}

func TestUpdateCheckpointBackend_WritesAndReloads(t *testing.T) {
	// Uses t.Chdir (process-global cwd), so no t.Parallel.
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	ctx := context.Background()

	require.NoError(t, updateCheckpointBackend(ctx, io.Discard, EnableOptions{CheckpointBackend: "refs"}))

	cfg, err := settings.LoadCheckpointsConfig(ctx)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, checkpoint.BackendTypeGitRefs, cfg.Primary.Type)
	assert.True(t, checkpoint.PrimaryIsRefs(cfg))

	// Switching back to branch overrides the prior selection.
	require.NoError(t, updateCheckpointBackend(ctx, io.Discard, EnableOptions{CheckpointBackend: "branch"}))
	cfg, err = settings.LoadCheckpointsConfig(ctx)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, checkpoint.BackendTypeGitBranch, cfg.Primary.Type)
	assert.False(t, checkpoint.PrimaryIsRefs(cfg))
}

func TestUpdateCheckpointBackend_InvalidValue(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	err := updateCheckpointBackend(context.Background(), io.Discard, EnableOptions{CheckpointBackend: "bogus"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), flagCheckpointBackend)
}
